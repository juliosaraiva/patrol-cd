package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/operation"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// DefaultLeaderElectionID names the controller's leader election Lease when sharding is
// not used.
const DefaultLeaderElectionID = "patrol-controller"

// RunConfig configures Run.
type RunConfig struct {
	// RestConfig reaches the control-plane cluster. Required.
	RestConfig *rest.Config
	// Namespace is the control-plane namespace. Required.
	Namespace string
	// Params are the controller parameters (ParamsFrom(settings.ResolveParams(...))).
	Params core.Params
	// RepoServer is the repo-server client. Required.
	RepoServer apiclient.Client
	// Registerer receives the controller, comparison and settings metrics (nil: none). The
	// caller serves them and the probes.
	Registerer prometheus.Registerer
	// Logger is the structured logger (nil: slog.Default()).
	Logger *slog.Logger

	// LeaderElection runs the reconcilers only on the elected replica.
	LeaderElection bool
	// LeaderElectionID names the Lease (default DefaultLeaderElectionID).
	LeaderElectionID string

	// Engine is the sync engine configuration (nil: syncpkg.DefaultConfig()).
	Engine *syncpkg.Config
	// CacheBase are the live-state cache tuning knobs (ClusterResyncPeriod defaults to 12h,
	// the upstream controller default).
	CacheBase cache.Settings
	// PersistResourceHealth stores per-resource health in status.resources.
	PersistResourceHealth bool
	// ServerSideDiff enables server-side diff by default.
	ServerSideDiff bool
	// RepoErrorGracePeriod defers repo-server errors (default 180s).
	RepoErrorGracePeriod time.Duration
	// JQTimeout bounds jq ignore rules (zero: diff default).
	JQTimeout time.Duration
	// ClusterOptions are passed to the cluster registry's REST config builder.
	ClusterOptions clusters.Options
}

// Run builds the controller's dependencies (settings manager, cluster registry, repository
// resolver, live-state caches, comparison manager, sync engines) and runs the Application
// and Operation reconcilers until ctx ends.
func Run(ctx context.Context, rc RunConfig) error {
	if rc.RestConfig == nil || rc.Namespace == "" || rc.RepoServer == nil {
		return errors.New("controller: RestConfig, Namespace and RepoServer are required")
	}
	log := rc.Logger
	if log == nil {
		log = slog.Default()
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}
	matcher := core.NewNamespaceMatcher(rc.Namespace, rc.Params.ApplicationNamespaces)
	appNamespaces := map[string]crcache.Config{rc.Namespace: {}}
	if matcher.ClusterWide() {
		appNamespaces = map[string]crcache.Config{crcache.AllNamespaces: {}}
	}
	control := map[string]crcache.Config{rc.Namespace: {}}
	leaderID := rc.LeaderElectionID
	if leaderID == "" {
		leaderID = DefaultLeaderElectionID
	}
	mgr, err := ctrl.NewManager(rc.RestConfig, manager.Options{
		Scheme:                        scheme,
		Logger:                        logr.FromSlogHandler(log.Handler()),
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                rc.LeaderElection,
		LeaderElectionID:              leaderID,
		LeaderElectionNamespace:       rc.Namespace,
		LeaderElectionReleaseOnCancel: true,
		Cache: crcache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]crcache.ByObject{
				&v1alpha1.Application{}: {Namespaces: appNamespaces},
				&v1alpha1.Operation{}:   {Namespaces: appNamespaces},
				&v1alpha1.AppProject{}:  {Namespaces: control},
				&v1alpha1.Cluster{}:     {Namespaces: control},
				&v1alpha1.Repository{}:  {Namespaces: control},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	for _, obj := range []client.Object{&v1alpha1.Application{}, &v1alpha1.Operation{}, &v1alpha1.AppProject{}, &v1alpha1.Cluster{}, &v1alpha1.Repository{}} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("create %T informer: %w", obj, err)
		}
	}

	kube, err := kubernetes.NewForConfig(rc.RestConfig)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rc.RestConfig)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	var settingsMetrics settings.Metrics
	if rc.Registerer != nil {
		if settingsMetrics, err = settings.NewPrometheusMetrics(rc.Registerer); err != nil {
			return err
		}
	}
	settingsMgr, err := settings.NewManager(kube, settings.ManagerOptions{Namespace: rc.Namespace, Logger: log, Metrics: settingsMetrics})
	if err != nil {
		return err
	}
	if err := settingsMgr.Start(ctx); err != nil {
		return fmt.Errorf("start settings manager: %w", err)
	}
	inCluster := settingsMgr.Current().InClusterEnabled()
	registry, err := clusters.NewRegistry(dyn, kube, clusters.RegistryOptions{Namespace: rc.Namespace, InClusterEnabled: &inCluster, Logger: log})
	if err != nil {
		return err
	}
	if err := registry.Start(ctx); err != nil {
		return err
	}
	lister, err := repository.NewLister(dyn, kube, repository.ListerOptions{Namespace: rc.Namespace, Logger: log})
	if err != nil {
		return err
	}
	if err := lister.Start(ctx); err != nil {
		return err
	}
	resolver, err := repository.NewResolver(lister, repository.Options{Logger: log, Registerer: rc.Registerer, RepoServer: rc.RepoServer})
	if err != nil {
		return err
	}

	metrics, err := core.NewMetrics(rc.Registerer)
	if err != nil {
		return err
	}
	clusterConfig := func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
		return registry.RestConfig(ctx, c, rc.ClusterOptions)
	}
	rawConfig := func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
		return registry.RawRestConfig(ctx, c, rc.ClusterOptions)
	}

	// The cache manager is created before the Application reconciler that handles its
	// events; the handler is bound once the reconciler exists.
	var appRec *app.Reconciler
	cacheMgr := cache.NewManager(cache.ManagerOptions{Logger: log, OnCacheCreated: func(key string, cc cache.ClusterCache) {
		if appRec != nil {
			appRec.OnClusterCacheCreated(key, cc)
		}
	}})
	defer cacheMgr.Close()
	base := rc.CacheBase
	if base.ClusterResyncPeriod == 0 {
		base.ClusterResyncPeriod = 12 * time.Hour
	}
	if rc.ServerSideDiff {
		base.RetainManagedFields = true
	}
	live, err := compare.NewLiveStateCache(compare.LiveStateCacheOptions{
		Manager: cacheMgr, Config: func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
			cfg, err := clusterConfig(ctx, c)
			if err != nil {
				return nil, err
			}
			return metrics.WrapConfig(cfg, c.Spec.Server), nil
		},
		Settings: settingsMgr, ControlPlaneNamespace: rc.Namespace, Base: base, Logger: log,
	})
	if err != nil {
		return err
	}
	ssd, err := compare.NewKubeServerSideDiff(rawConfig, live, 0)
	if err != nil {
		return err
	}
	grace := rc.RepoErrorGracePeriod
	if grace == 0 {
		grace = 180 * time.Second
	}
	cmp, err := compare.New(compare.Deps{
		RepoServer: rc.RepoServer, Repositories: resolver, Settings: settingsMgr, Clusters: registry,
		LiveState: live, ServerSideDiff: ssd, ControlPlaneNamespace: rc.Namespace,
		ApplicationExists:     app.ApplicationExists(mgr.GetClient(), matcher),
		ServerSideDiffEnabled: rc.ServerSideDiff, PersistResourceHealth: rc.PersistResourceHealth,
		RepoErrorGracePeriod: grace, JQTimeout: rc.JQTimeout, Registerer: rc.Registerer, Logger: log,
	})
	if err != nil {
		return err
	}
	deps := core.Deps{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorder("patrol-controller"),
		Comparer: cmp, Settings: settingsMgr, LiveState: live, ClusterConfig: clusterConfig, Clusters: registry,
		Namespace: rc.Namespace, Params: rc.Params, Metrics: metrics, Logger: log,
	}
	appRec, err = app.New(app.Config{Deps: deps})
	if err != nil {
		return err
	}
	engines, err := operation.NewKubeEngines(operation.KubeEnginesOptions{Config: clusterConfig, Engine: rc.Engine, Metrics: metrics, Logger: log})
	if err != nil {
		return err
	}
	opDeps := deps
	opDeps.Refresher = appRec.Refresher()
	opRec, err := operation.New(operation.Config{Deps: opDeps, Engines: engines})
	if err != nil {
		return err
	}
	if err := core.IndexOperations(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := app.IndexFields(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := appRec.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := opRec.SetupWithManager(mgr); err != nil {
		return err
	}

	unsubscribeSettings := settingsMgr.Subscribe(func(old, cur *settings.Settings) {
		registry.SetInClusterEnabled(cur.InClusterEnabled())
		if err := appRec.RefreshAll(ctx); err != nil {
			log.Warn("cannot refresh applications after a settings change", "error", err)
		}
	})
	defer unsubscribeSettings()
	cancelClusters := registry.Subscribe(func(ev clusters.Event) {
		if ev.Type == clusters.EventAdded {
			return
		}
		if ev.Type == clusters.EventDeleted || ev.Changes.Credentials || ev.Changes.Spec || ev.Changes.Identity {
			engines.Forget(ev.Server)
			cacheMgr.Invalidate(clusters.NormalizeServer(ev.Server))
			if ev.Old != nil {
				live.Forget(ev.Old)
			}
		}
	})
	defer cancelClusters()

	log.Info("starting application controller", "namespace", rc.Namespace, "applicationNamespaces", rc.Params.ApplicationNamespaces,
		"statusProcessors", rc.Params.StatusProcessors, "operationProcessors", rc.Params.OperationProcessors, "syncTimeout", rc.Params.SyncTimeout)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}
	return nil
}
