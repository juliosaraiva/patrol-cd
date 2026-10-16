package appset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// SetupWithManager registers the OwnerIndex field index and the controller: ApplicationSets
// (filtered by ShouldRequeueForApplicationSet), owned Applications (ShouldRequeueForApplication)
// and Clusters of the control-plane namespace (ApplicationSets with a Clusters generator), all
// restricted to allowed namespaces.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.Application{}, OwnerIndex, OwnerIndexFunc); err != nil {
		return fmt.Errorf("indexing applications by owner: %w", err)
	}
	ps := r.cfg.EnableProgressiveSyncs
	clusters := &clusterEventHandler{client: mgr.GetClient(), namespace: r.cfg.Namespace, namespaces: r.cfg.namespaces, log: r.cfg.log}
	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&v1alpha1.ApplicationSet{}, builder.WithPredicates(applicationSetPredicate(ps), namespacePredicate(r.cfg.namespaces))).
		Owns(&v1alpha1.Application{}, builder.WithPredicates(applicationPredicate(ps), namespacePredicate(r.cfg.namespaces))).
		Watches(&v1alpha1.Cluster{}, clusters).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.cfg.ConcurrentReconciliations,
			// Run may be called again in the same process (restart after losing leadership).
			SkipNameValidation: ptr.To(true),
		}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("creating applicationset controller: %w", err)
	}
	return nil
}

// RunConfig configures Run.
type RunConfig struct {
	Config
	// RestConfig is the Kubernetes client configuration (required).
	RestConfig *rest.Config
	// LeaderElection runs the controller only on the elected replica.
	LeaderElection bool
	// LeaderElectionID names the Lease (default DefaultLeaderElectionID).
	LeaderElectionID string
	// LeaderElectionNamespace defaults to the control-plane namespace.
	LeaderElectionNamespace string
	// CacheSyncPeriod is the informer resync period (0: controller-runtime default).
	CacheSyncPeriod time.Duration
}

// Run runs the ApplicationSet controller until ctx is cancelled. Metrics go to
// Config.Registry; the caller serves them and the probes.
func Run(ctx context.Context, rc RunConfig) error {
	if rc.RestConfig == nil {
		return errors.New("appset: a REST config is required")
	}
	if rc.Namespace == "" {
		return errors.New("appset: the control-plane namespace is required")
	}
	matcher, err := NewNamespaceMatcher(rc.Namespace, rc.ApplicationSetNamespaces)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("building scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("building scheme: %w", err)
	}
	watched := map[string]cache.Config{cache.AllNamespaces: {}}
	if lits, ok := matcher.LiteralNamespaces(); ok {
		watched = map[string]cache.Config{}
		for _, ns := range lits {
			watched[ns] = cache.Config{}
		}
	}
	control := map[string]cache.Config{rc.Namespace: {}}
	leaderID := rc.LeaderElectionID
	if leaderID == "" {
		leaderID = DefaultLeaderElectionID
	}
	leaderNS := rc.LeaderElectionNamespace
	if leaderNS == "" {
		leaderNS = rc.Namespace
	}
	logger := rc.Logger
	if logger == nil {
		logger = rc.Generators.Logger
	}
	opts := manager.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                rc.LeaderElection,
		LeaderElectionID:              leaderID,
		LeaderElectionNamespace:       leaderNS,
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				&v1alpha1.ApplicationSet{}: {Namespaces: watched},
				&v1alpha1.Application{}:    {Namespaces: watched},
				&v1alpha1.Cluster{}:        {Namespaces: control},
				&v1alpha1.AppProject{}:     {Namespaces: control},
				&v1alpha1.Repository{}:     {Namespaces: control},
				&v1alpha1.SCMProvider{}:    {Namespaces: watched},
			},
		},
	}
	if rc.CacheSyncPeriod > 0 {
		opts.Cache.SyncPeriod = ptr.To(rc.CacheSyncPeriod)
	}
	if logger != nil {
		opts.Logger = logr.FromSlogHandler(logger.Handler())
	}
	mgr, err := ctrl.NewManager(rc.RestConfig, opts)
	if err != nil {
		return fmt.Errorf("creating controller manager: %w", err)
	}
	// Only the types configured above are cached (ReaderFailOnMissingInformer); create their
	// informers so reads work before the first watch event.
	for _, obj := range []client.Object{&v1alpha1.ApplicationSet{}, &v1alpha1.Application{}, &v1alpha1.Cluster{},
		&v1alpha1.AppProject{}, &v1alpha1.Repository{}, &v1alpha1.SCMProvider{}} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("creating %T informer: %w", obj, err)
		}
	}
	cfg := rc.Config
	if cfg.Generators.Client == nil {
		cfg.Generators.Client = &splitReader{cached: mgr.GetClient(), live: mgr.GetAPIReader()}
	}
	if cfg.Generators.Dynamic == nil {
		dyn, err := dynamic.NewForConfig(rc.RestConfig)
		if err != nil {
			return fmt.Errorf("creating dynamic client: %w", err)
		}
		cfg.Generators.Dynamic = dyn
	}
	r, err := NewReconciler(cfg, mgr.GetClient(), mgr.GetAPIReader(), mgr.GetEventRecorder(ControllerName))
	if err != nil {
		return err
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return err
	}
	r.cfg.log.Info("starting applicationset controller",
		"namespace", rc.Namespace, "applicationSetNamespaces", rc.ApplicationSetNamespaces,
		"policy", r.cfg.Policy, "policyOverride", r.cfg.EnablePolicyOverride,
		"progressiveSyncs", r.cfg.EnableProgressiveSyncs, "dryRun", r.cfg.DryRun)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running applicationset controller: %w", err)
	}
	return nil
}

// splitReader serves generator reads: Secrets and ConfigMaps (credentials, plugin and cluster
// decision resource configuration, possibly in tenant namespaces) are read from the API server
// so the controller does not cache every Secret of the cluster; everything else is read from
// the manager cache.
type splitReader struct {
	cached client.Reader
	live   client.Reader
}

func (s *splitReader) pick(obj runtime.Object) client.Reader {
	switch obj.(type) {
	case *corev1.Secret, *corev1.SecretList, *corev1.ConfigMap, *corev1.ConfigMapList:
		return s.live
	}
	return s.cached
}

// Get implements client.Reader.
func (s *splitReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return s.pick(obj).Get(ctx, key, obj, opts...)
}

// List implements client.Reader.
func (s *splitReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return s.pick(list).List(ctx, list, opts...)
}
