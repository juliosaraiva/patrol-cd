package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

// projectIndex indexes cached Applications by project ("" counts as "default").
const projectIndex = "spec.project"

// Run runs the notifications controller until ctx is cancelled, then returns nil (a clean stop
// also records the state of deliveries made so far). It returns an error when the configuration
// is invalid, the caches cannot sync (e.g. missing RBAC) or the controller fails.
//
// The controller serves no HTTP endpoint: metrics go to Config.Registry and the caller serves
// them with its own probes. With Config.LeaderElection only the elected replica evaluates.
func Run(ctx context.Context, cfg Config) error {
	c, err := cfg.complete()
	if err != nil {
		return err
	}
	mgr, err := newManager(c)
	if err != nil {
		return err
	}
	if err := setup(ctx, mgr, c); err != nil {
		return err
	}
	c.Logger.Info("starting notifications controller",
		"namespace", c.Namespace,
		"applicationNamespaces", c.ApplicationNamespaces,
		"selfService", c.SelfServiceEnabled,
		"processors", c.Processors,
		"configMap", c.ConfigMapName,
		"secret", c.SecretName)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running notifications controller: %w", err)
	}
	return nil
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	return scheme, nil
}

// cacheNamespaces maps the namespaces to cache: all when patterns are not literal.
func cacheNamespaces(c *completed, multi bool) map[string]cache.Config {
	if !multi {
		return map[string]cache.Config{c.Namespace: {}}
	}
	if lits, ok := c.scope.literalNamespaces(); ok {
		out := make(map[string]cache.Config, len(lits))
		for _, ns := range lits {
			out[ns] = cache.Config{}
		}
		return out
	}
	return map[string]cache.Config{cache.AllNamespaces: {}}
}

func newManager(c *completed) (manager.Manager, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	appNamespaces := cacheNamespaces(c, c.scope.multiNamespace())
	configNamespaces := cacheNamespaces(c, c.SelfServiceEnabled && c.scope.multiNamespace())
	appCache := cache.ByObject{Namespaces: appNamespaces}
	if !c.selector.Empty() {
		appCache.Label = c.selector
	}
	opts := manager.Options{
		Scheme:                        scheme,
		Logger:                        logr.FromSlogHandler(c.Logger.Handler()),
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                c.LeaderElection,
		LeaderElectionID:              c.LeaderElectionID,
		LeaderElectionNamespace:       c.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			// Only the types below are ever cached: a Get of anything else (e.g. arbitrary
			// Secrets) fails instead of starting a cluster-wide informer.
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				newApplication(): appCache,
				newAppProject():  {Namespaces: map[string]cache.Config{c.Namespace: {}}},
				&corev1.ConfigMap{}: {
					Namespaces: configNamespaces,
					Field:      fields.OneTermEqualSelector("metadata.name", c.ConfigMapName),
				},
				&corev1.Secret{}: {
					Namespaces: configNamespaces,
					Field:      fields.OneTermEqualSelector("metadata.name", c.SecretName),
				},
			},
		},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	}
	mgr, err := ctrl.NewManager(c.RestConfig, opts)
	if err != nil {
		return nil, fmt.Errorf("creating controller manager: %w", err)
	}
	return mgr, nil
}

// setup registers the reconciler with mgr.
func setup(ctx context.Context, mgr manager.Manager, c *completed) error {
	for _, obj := range []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("creating %T informer: %w", obj, err)
		}
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, newApplication(), projectIndex, appProjectName); err != nil {
		return fmt.Errorf("indexing applications by project: %w", err)
	}
	r, err := newReconciler(c, mgr.GetClient(), mgr.GetAPIReader(), mgr.GetEventRecorder(ControllerName))
	if err != nil {
		return err
	}
	err = ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(newApplication()).
		Watches(newAppProject(), handler.EnqueueRequestsFromMapFunc(r.applicationsOfProject),
			builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: c.Processors,
			// Run may be called again in the same process (restart after losing leadership).
			SkipNameValidation: ptr.To(true),
		}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("creating notifications controller: %w", err)
	}
	return nil
}

func newReconciler(c *completed, cl client.Client, live client.Reader, recorder events.EventRecorder) (*reconciler, error) {
	promMetrics, err := notifications.NewPrometheusMetrics(c.Registry)
	if err != nil {
		return nil, fmt.Errorf("registering notification metrics: %w", err)
	}
	engineOpts := []notifications.Option{
		notifications.WithProjectGetter(NewProjectGetter(cl, c.Namespace)),
		notifications.WithMetrics(evalMetrics{m: promMetrics}),
		notifications.WithLogger(newEngineLogger(c.Logger)),
		// The network is per configuration (engineFactory.build): control plane or tenant.
		notifications.WithClock(c.Now),
		notifications.WithTemplateLimits(c.TemplateLimits),
	}
	if c.SendTimeout > 0 {
		engineOpts = append(engineOpts, notifications.WithSendTimeout(c.SendTimeout))
	}
	if repo := NewRepoService(c.RepoClient, c.RepoResolver); repo != nil {
		engineOpts = append(engineOpts, notifications.WithRepoService(repo))
	}
	return &reconciler{
		cfg:      c,
		client:   cl,
		live:     live,
		recorder: recorder,
		engines:  newEngineFactory(c, cl, recorder, engineOpts),
		ops:      NewOperationGetter(live, DefaultOperationCacheBytes),
		metrics:  promMetrics,
		memo:     newMemo(c.Now),
		log:      c.Logger,
		prefix:   notifications.DefaultAnnotationPrefix,
	}, nil
}

// appProjectName is the projectIndex function.
func appProjectName(obj client.Object) []string {
	u, ok := obj.(interface{ UnstructuredContent() map[string]any })
	if !ok {
		return nil
	}
	name, _ := nestedString(u.UnstructuredContent(), "spec", "project")
	if name == "" {
		name = "default"
	}
	return []string{name}
}

// applicationsOfProject enqueues the Applications of an AppProject whose annotations changed,
// so project-level subscriptions apply without waiting for the next resync.
func (r *reconciler) applicationsOfProject(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.cfg.Namespace {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("ApplicationList"))
	if err := r.client.List(ctx, list, client.MatchingFields{projectIndex: obj.GetName()}); err != nil {
		r.log.Warn("listing applications of project", "project", obj.GetName(), "error", err)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].GetNamespace(), Name: list.Items[i].GetName()}})
	}
	return reqs
}
