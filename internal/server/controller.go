package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
	"github.com/juliosaraiva/patrol-cd/internal/controller/clusterinfo"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/operation"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/hydrator"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// Defaults of the application controller (OPS-009).
const (
	DefaultControllerMetricsPort = 8082
	// DefaultTreeAPIAddress serves the controller tree API to patrol-server.
	DefaultTreeAPIAddress = ":8090"
	// shardWriterResync re-writes Cluster.status.assignedShard periodically.
	shardWriterResync = 10 * time.Minute
)

// ControllerConfig is the configuration of the application controller.
type ControllerConfig struct {
	// Namespace is the control-plane namespace. Required.
	Namespace string
	// RestConfig reaches the control-plane cluster. Required.
	RestConfig *krest.Config
	// Params are the resolved controller parameters (nil: defaults).
	Params *settings.Params
	// InternalToken authenticates repo-server, commit-server and tree API calls. Required.
	InternalToken string
	// RepoServer configures the repo-server client.
	RepoServer RepoServerClientOptions
	// CommitServer configures the commit-server client of the source hydrator (Address is
	// commit.server).
	CommitServer CommitServerClientOptions
	// HydratorEnabled runs the source hydrator (hydrator.enabled).
	HydratorEnabled bool

	// MetricsAddress serves /metrics, the probes and the profiler ("-" disables);
	// HealthzAddress optionally serves the probes on their own listener.
	MetricsAddress, HealthzAddress string
	EnablePprof                    bool
	PprofFile                      string
	// TreeAPIAddress serves the controller tree API ("-" disables); TreeAPITLS enables TLS
	// (and mutual TLS) on it.
	TreeAPIAddress string
	TreeAPITLS     TLSServerOptions

	// Sharding configures shard ownership (sharding.ConfigFromEnv); Identity is the pod
	// name (default: the host name).
	Sharding sharding.Config
	Identity string
	// ClusterTuning configures destination cluster clients (nil: defaults).
	ClusterTuning *clusters.Tuning
	// ClusterInfoInterval is how often Cluster status is written (0: the default; the command
	// reads PATROL_UPDATE_CLUSTER_INFO_TIMEOUT).
	ClusterInfoInterval time.Duration

	ShutdownTimeout time.Duration
	Registry        *prometheus.Registry
	Logger          *slog.Logger
	// OnListening, when set, is called with the bound metrics and tree API addresses.
	OnListening func(metrics, tree net.Addr)
}

// Validate checks the configuration.
func (c *ControllerConfig) Validate() error {
	var errs []error
	if c.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	}
	if c.RestConfig == nil {
		errs = append(errs, errors.New("a Kubernetes client configuration is required"))
	}
	if c.InternalToken == "" {
		errs = append(errs, errors.New("the internal token is required"))
	}
	if c.RepoServer.Address == "" {
		errs = append(errs, errors.New("repo-server address is required"))
	}
	if c.HydratorEnabled && c.CommitServer.Address == "" {
		errs = append(errs, errors.New("the commit-server address is required with the hydrator"))
	}
	if (c.TreeAPITLS.CertFile == "") != (c.TreeAPITLS.KeyFile == "") {
		errs = append(errs, errors.New("--tls-cert and --tls-key must be set together"))
	}
	if c.ClusterInfoInterval < 0 {
		errs = append(errs, errors.New("the cluster info interval cannot be negative"))
	}
	for name, addr := range map[string]string{"metrics": c.MetricsAddress, "tree API": c.TreeAPIAddress} {
		if listenerEnabled(addr) {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				errs = append(errs, fmt.Errorf("%s address %q: %w", name, addr, err))
			}
		}
	}
	return errors.Join(errs...)
}

// controllerParams are the parameter-derived settings of the controller.
type controllerParams struct {
	core                  core.Params
	persistResourceHealth bool
	serverSideDiff        bool
	repoErrorGrace        time.Duration
	jqTimeout             time.Duration
	engine                syncpkg.Config
	cacheBase             cache.Settings
	hydrationProcessors   int
	algorithm             string
}

func controllerParamsFrom(p *settings.Params) controllerParams {
	out := controllerParams{core: core.ParamsFrom(p), engine: syncpkg.DefaultConfig(), repoErrorGrace: 180 * time.Second, hydrationProcessors: hydrator.DefaultProcessors}
	out.cacheBase.ClusterResyncPeriod = 12 * time.Hour
	if p == nil {
		return out
	}
	out.persistResourceHealth = p.Bool("controller.resource.health.persist")
	out.serverSideDiff = p.Bool("controller.diff.server.side")
	out.repoErrorGrace = p.Duration("controller.repo.error.grace.period.seconds")
	out.jqTimeout = p.Duration("controller.ignore.normalizer.jq.timeout")
	if _, ok := p.Lookup("controller.sync.wave.delay.seconds"); ok {
		out.engine.WaveDelay = time.Duration(p.Int("controller.sync.wave.delay.seconds")) * time.Second
	}
	if n := p.Int("controller.kubectl.parallelism.limit"); n > 0 {
		out.engine.Parallelism = int(n)
	}
	if p.Bool("controller.cluster.cache.batch.events.processing") {
		out.cacheBase.EventProcessingInterval = p.Duration("controller.cluster.cache.events.processing.interval")
	}
	if n := p.Int("controller.hydration.processors"); n > 0 {
		out.hydrationProcessors = int(n)
	}
	out.algorithm = p.String("controller.sharding.algorithm")
	return out
}

// RunController assembles the application controller (settings, cluster registry,
// repository resolver, live-state caches, comparison, sync engines, Application and
// Operation reconcilers, Lease-based sharding, the tree API and, when enabled, the source
// hydrator) and runs it until ctx ends.
func RunController(ctx context.Context, cfg ControllerConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid controller configuration: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reg := cfg.Registry
	if reg == nil {
		reg = newRegistry()
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	identity := cfg.Identity
	if identity == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determine the pod identity: %w", err)
		}
		identity = h
	}
	cp := controllerParamsFrom(cfg.Params)
	shardCfg := cfg.Sharding
	if cp.algorithm != "" {
		shardCfg.Algorithm = sharding.ParseAlgorithm(cp.algorithm, log)
	}
	if shardCfg.Hostname == "" {
		shardCfg.Hostname = identity
	}

	scheme, err := newScheme()
	if err != nil {
		return err
	}
	matcher := core.NewNamespaceMatcher(cfg.Namespace, cp.core.ApplicationNamespaces)
	appNamespaces := map[string]crcache.Config{cfg.Namespace: {}}
	if matcher.ClusterWide() {
		appNamespaces = map[string]crcache.Config{crcache.AllNamespaces: {}}
	}
	control := map[string]crcache.Config{cfg.Namespace: {}}
	mgr, err := ctrl.NewManager(cfg.RestConfig, manager.Options{
		Scheme:                 scheme,
		Logger:                 logr.FromSlogHandler(log.Handler()),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Shard Leases decide which replica processes what (ADR-0008); there is no
		// manager-wide leader election.
		LeaderElection:          false,
		GracefulShutdownTimeout: ptr.To(cfg.ShutdownTimeout),
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
	kube, err := kubernetes.NewForConfig(cfg.RestConfig)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg.RestConfig)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	recorder := newEventRecorder(kube, scheme, "patrol-controller")
	defer recorder.Shutdown()

	settingsMetrics, err := settings.NewPrometheusMetrics(reg)
	if err != nil {
		return err
	}
	settingsMgr, err := settings.NewManager(kube, settings.ManagerOptions{Namespace: cfg.Namespace, Recorder: recorder, Metrics: settingsMetrics, Logger: log})
	if err != nil {
		return err
	}
	if err := settingsMgr.Start(ctx); err != nil {
		return fmt.Errorf("start settings manager: %w", err)
	}
	inCluster := settingsMgr.Current().InClusterEnabled()
	registry, err := clusters.NewRegistry(dyn, kube, clusters.RegistryOptions{
		Namespace: cfg.Namespace, InClusterEnabled: &inCluster, Logger: log, LocalServerVersion: localServerVersion(kube),
	})
	if err != nil {
		return err
	}
	if err := registry.Start(ctx); err != nil {
		return fmt.Errorf("start cluster registry: %w", err)
	}
	lister, err := repository.NewLister(dyn, kube, repository.ListerOptions{Namespace: cfg.Namespace, Logger: log})
	if err != nil {
		return err
	}
	if err := lister.Start(ctx); err != nil {
		return fmt.Errorf("start repository lister: %w", err)
	}
	repoClient, err := newRepoServerClient(cfg.RepoServer, cfg.InternalToken)
	if err != nil {
		return err
	}
	resolver, err := repository.NewResolver(lister, repository.Options{Logger: log, Registerer: reg, RepoServer: repoClient})
	if err != nil {
		return err
	}

	metrics, err := core.NewMetrics(reg)
	if err != nil {
		return err
	}
	tuning := clusters.DefaultTuning()
	if cfg.ClusterTuning != nil {
		tuning = *cfg.ClusterTuning
	}
	clusterOpts := clusters.Options{Tuning: &tuning, FakeInCluster: clusters.FakeInClusterFromEnv()}
	clusterConfig := func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error) {
		return registry.RestConfig(ctx, c, clusterOpts)
	}
	rawConfig := func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error) {
		return registry.RawRestConfig(ctx, c, clusterOpts)
	}

	// Sharding (ADR-0008).
	assigner, err := sharding.NewAssigner(sharding.AssignerOptions{Algorithm: shardCfg.Algorithm, Replicas: max(shardCfg.Replicas, 1), Logger: log, Registerer: reg})
	if err != nil {
		return err
	}
	all, err := registry.All()
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	assigner.Init(all, nil)
	var replicas sharding.ReplicaSource = sharding.StaticReplicas(shardCfg.Replicas)
	if shardCfg.DynamicDistribution {
		replicas = sharding.WorkloadReplicas{Client: kube, Namespace: cfg.Namespace, Name: shardCfg.ControllerName}
	}

	var (
		appRec  *app.Reconciler
		requeue = make(chan event.TypedGenericEvent[*v1alpha1.Operation], 1024)
	)
	cacheMgr := cache.NewManager(cache.ManagerOptions{Logger: log, OnCacheCreated: func(key string, cc cache.ClusterCache) {
		if appRec != nil {
			appRec.OnClusterCacheCreated(key, cc)
		}
	}})
	defer cacheMgr.Close()
	base := cp.cacheBase
	if cp.serverSideDiff {
		base.RetainManagedFields = true
	}
	live, err := compare.NewLiveStateCache(compare.LiveStateCacheOptions{
		Manager: cacheMgr, Config: func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error) {
			rc, err := clusterConfig(ctx, c)
			if err != nil {
				return nil, err
			}
			return metrics.WrapConfig(rc, c.Spec.Server), nil
		},
		Settings: settingsMgr, ControlPlaneNamespace: cfg.Namespace, Base: base, Logger: log,
	})
	if err != nil {
		return err
	}
	ssd, err := compare.NewKubeServerSideDiff(rawConfig, live, 0)
	if err != nil {
		return err
	}
	cmpMgr, err := compare.New(compare.Deps{
		RepoServer: repoClient, Repositories: resolver, Settings: settingsMgr, Clusters: registry,
		LiveState: live, ServerSideDiff: ssd, ControlPlaneNamespace: cfg.Namespace,
		ApplicationExists:     app.ApplicationExists(mgr.GetClient(), matcher),
		ServerSideDiffEnabled: cp.serverSideDiff, PersistResourceHealth: cp.persistResourceHealth,
		RepoErrorGracePeriod: cp.repoErrorGrace, GPGEnabled: !gpgDisabled(), JQTimeout: cp.jqTimeout,
		Registerer: reg, Logger: log,
	})
	if err != nil {
		return err
	}

	// forgetUnowned drops live-state caches and sync engines of clusters this replica no
	// longer owns (they are rebuilt on demand after a takeover).
	var own *sharding.Ownership
	var engines *operation.KubeEngines
	forgetUnowned := func() {
		if own == nil || engines == nil {
			return
		}
		cs, err := registry.All()
		if err != nil {
			return
		}
		for _, c := range cs {
			if !own.OwnsClusterObject(c) {
				engines.Forget(c.Spec.Server)
				cacheMgr.Invalidate(clusters.NormalizeServer(c.Spec.Server))
				live.Forget(c)
			}
		}
	}
	// requeueAll asks both reconcilers to look at everything again after an ownership change
	// (work skipped while the shard was not owned is never requeued otherwise).
	requeueAll := func(ctx context.Context) {
		// Reads go through the manager cache: wait until it is started and synced.
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return
		}
		if appRec != nil {
			if err := appRec.RefreshAll(ctx); err != nil {
				log.Warn("cannot requeue applications after a shard change", "error", err)
			}
		}
		var ops v1alpha1.OperationList
		if err := mgr.GetClient().List(ctx, &ops); err != nil {
			log.Warn("cannot requeue operations after a shard change", "error", err)
			return
		}
		for i := range ops.Items {
			op := &ops.Items[i]
			if !core.IsNonTerminal(op) {
				continue
			}
			select {
			case requeue <- event.TypedGenericEvent[*v1alpha1.Operation]{Object: op}:
			default:
				log.Warn("operation requeue channel full; the operation waits for its next event", "operation", op.Namespace+"/"+op.Name)
			}
		}
	}
	shardWriterTrigger := func() {}
	lm, err := sharding.NewLeaseManager(sharding.LeaseManagerOptions{
		Client: sharding.NewLeaseClient(kube, cfg.Namespace), Namespace: cfg.Namespace, Identity: identity,
		Config: shardCfg, Replicas: replicas,
		OnAcquired: func(ctx context.Context, shard int) {
			log.Info("shard acquired", "shard", shard)
			shardWriterTrigger()
			go requeueAll(ctx)
		},
		OnLost: func(shard int) {
			log.Info("shard lost", "shard", shard)
			go forgetUnowned()
		},
		OnReplicasChanged: func(n int) {
			if assigner.SetReplicas(n) {
				log.Info("controller replica count changed", "replicas", n)
			}
		},
		Logger: log, Registerer: reg,
	})
	if err != nil {
		return err
	}
	own, err = sharding.NewOwnership(assigner, lm, registry, log)
	if err != nil {
		return err
	}
	statusWriter := clusters.NewStatusWriter(dyn)
	writer, err := sharding.NewAssignedShardWriter(assigner, lm, statusWriter, log)
	if err != nil {
		return err
	}
	shardWriterTrigger = writer.Trigger
	clusterInfo, err := clusterinfo.New(clusterinfo.Options{
		Assigner: assigner, Owner: lm, Reader: mgr.GetClient(), Namespace: cfg.Namespace, Counter: registry, Recorder: statusWriter,
		CacheState: func(server string) *clusters.CacheState {
			cc, ok := cacheMgr.Lookup(server)
			if !ok {
				return nil
			}
			return compare.ClusterCacheState(cc)
		},
		Interval: cfg.ClusterInfoInterval, Logger: log,
	})
	if err != nil {
		return err
	}

	deps := core.Deps{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorder("patrol-controller"),
		Comparer: cmpMgr, Settings: settingsMgr, LiveState: live, ClusterConfig: clusterConfig, Clusters: registry,
		Namespace: cfg.Namespace, Params: cp.core, Metrics: metrics, OwnsCluster: own.OwnsClusterObject, Logger: log,
	}
	appRec, err = app.New(app.Config{Deps: deps})
	if err != nil {
		return err
	}
	engineCfg := cp.engine
	engineCfg.Logger = log
	engines, err = operation.NewKubeEngines(operation.KubeEnginesOptions{Config: clusterConfig, Engine: &engineCfg, Metrics: metrics, Logger: log})
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
	if err := setupOperationController(mgr, opRec, matcher, cp.core.OperationProcessors, requeue); err != nil {
		return err
	}

	// Feed the assigner (consistent hashing counts Applications per cluster) and follow
	// cluster, settings and assignment changes.
	appInf, err := mgr.GetCache().GetInformer(ctx, &v1alpha1.Application{})
	if err != nil {
		return err
	}
	if _, err := appInf.AddEventHandler(assignerAppHandler{assigner: assigner}); err != nil {
		return fmt.Errorf("watch applications for sharding: %w", err)
	}
	cancelClusters := registry.Subscribe(func(ev clusters.Event) {
		assigner.HandleClusterEvent(ev)
		if ev.Type == clusters.EventAdded {
			return
		}
		if ev.Type == clusters.EventDeleted || ev.Changes.Credentials || ev.Changes.Spec || ev.Changes.Identity {
			engines.Forget(ev.Server)
			cacheMgr.Invalidate(clusters.NormalizeServer(ev.Server))
			if ev.Old != nil {
				live.Forget(ev.Old)
			}
			return
		}
		if clusterCacheRefreshRequested(ev) {
			// POST /api/v1/clusters/{server}/invalidate-cache (upstream refreshRequestedAt):
			// drop the live-state cache, rebuild it at once (upstream warms it up) and let the
			// Applications compare against the new one.
			log.Info("cluster cache invalidation requested", "server", ev.Server)
			cacheMgr.Invalidate(clusters.NormalizeServer(ev.Server))
			live.Forget(ev.New)
			go func(c *v1alpha1.Cluster) {
				if _, err := live.ClusterCache(ctx, c); err != nil && ctx.Err() == nil {
					log.Warn("cannot rebuild the cluster cache after an invalidation request", "server", c.Spec.Server, "error", err)
				}
				if err := appRec.RefreshAll(ctx); err != nil && ctx.Err() == nil {
					log.Warn("cannot refresh applications after a cluster cache invalidation", "error", err)
				}
			}(ev.New)
		}
	})
	defer cancelClusters()
	cancelAssign := assigner.Subscribe(func([]sharding.Change) {
		writer.Trigger()
		go func() {
			forgetUnowned()
			requeueAll(ctx)
		}()
	})
	defer cancelAssign()
	unsubscribeSettings := settingsMgr.Subscribe(func(_, cur *settings.Settings) {
		registry.SetInClusterEnabled(cur.InClusterEnabled())
		if err := appRec.RefreshAll(ctx); err != nil {
			log.Warn("cannot refresh applications after a settings change", "error", err)
		}
	})
	defer unsubscribeSettings()

	// Source hydrator (hydrator.enabled).
	if cfg.HydratorEnabled {
		if err := setupHydrator(ctx, mgr, cfg, cp, repoClient, resolver, settingsMgr, matcher, own, reg, log); err != nil {
			return err
		}
	}

	// Tree API for patrol-server.
	var treeHandler http.Handler
	if listenerEnabled(cfg.TreeAPIAddress) {
		h, err := treeapi.NewHandler(&treeState{
			reader: mgr.GetClient(), app: appRec, own: own, matcher: matcher, settings: settingsMgr, registry: registry,
			live: live, ssd: ssd, clusterConfig: clusterConfig, namespace: cfg.Namespace, jqTimeout: cp.jqTimeout, log: log,
		}, treeapi.HandlerOptions{Token: cfg.InternalToken, Logger: log, Registerer: reg})
		if err != nil {
			return err
		}
		treeHandler = tracingHandler("controller-tree-api", h)
	}

	var synced atomic.Bool
	ready := func(context.Context) error {
		if !synced.Load() {
			return errors.New("informer caches are not synced")
		}
		if err := lm.Healthz(); err != nil {
			return err
		}
		if settingsMgr.Current() == nil {
			return errors.New("settings are not loaded")
		}
		return nil
	}
	var (
		metricsAddr, treeAddr net.Addr
		bound                 atomic.Int32
	)
	notify := func() {
		if bound.Add(1) == 2 && cfg.OnListening != nil {
			cfg.OnListening(metricsAddr, treeAddr)
		}
	}
	log.Info("starting application controller", "namespace", cfg.Namespace, "applicationNamespaces", cp.core.ApplicationNamespaces,
		"statusProcessors", cp.core.StatusProcessors, "operationProcessors", cp.core.OperationProcessors,
		"shardingAlgorithm", shardCfg.Algorithm, "replicas", shardCfg.Replicas, "dynamicDistribution", shardCfg.DynamicDistribution,
		"identity", identity, "hydrator", cfg.HydratorEnabled)
	return runGroup(ctx, cfg.ShutdownTimeout, log,
		task{name: "manager", run: func(ctx context.Context) error {
			if err := mgr.Start(ctx); err != nil {
				return fmt.Errorf("run controller manager: %w", err)
			}
			return nil
		}},
		task{name: "cache-sync", run: func(ctx context.Context) error {
			if mgr.GetCache().WaitForCacheSync(ctx) {
				synced.Store(true)
			}
			return nil
		}},
		task{name: "shard-leases", run: lm.Run},
		task{name: "shard-status", run: func(ctx context.Context) error { writer.Run(ctx, shardWriterResync); return nil }},
		task{name: "cluster-info", run: func(ctx context.Context) error {
			// Reads go through the manager cache: wait until it is started and synced.
			if mgr.GetCache().WaitForCacheSync(ctx) {
				clusterInfo.Run(ctx)
			}
			return nil
		}},
		task{name: "tree-api", run: func(ctx context.Context) error {
			return serveInternalHTTP(ctx, internalHTTP{
				name: "controller tree API", addr: cfg.TreeAPIAddress, handler: treeHandler, tls: cfg.TreeAPITLS,
				shutdown: cfg.ShutdownTimeout, log: log,
			}, func(a net.Addr) { treeAddr = a; notify() })
		}},
		task{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{
				MetricsAddr: cfg.MetricsAddress, HealthzAddr: cfg.HealthzAddress, Gatherer: reg, Ready: ready,
				PprofEnabled: cfg.EnablePprof, PprofFile: cfg.PprofFile, Logger: log, ShutdownTimeout: cfg.ShutdownTimeout,
			}, func(m, _ net.Addr) { metricsAddr = m; notify() })
		}},
	)
}

// clusterCacheRefreshRequested reports whether a cluster event carries a new cache
// invalidation request: the cd.patrol.example/refresh annotation of the Cluster (written by
// POST /api/v1/clusters/{server}/invalidate-cache, upstream refreshRequestedAt) was added or
// changed. The current state delivered on subscription (EventAdded) never invalidates, so a
// request already served before a restart is not repeated (upstream compares it with the
// last cache sync time).
func clusterCacheRefreshRequested(ev clusters.Event) bool {
	if ev.Type != clusters.EventModified || ev.New == nil {
		return false
	}
	requested, ok, _ := clusters.RefreshRequestedAt(ev.New)
	if !ok {
		return false
	}
	previous, had, _ := clusters.RefreshRequestedAt(ev.Old)
	return !had || !previous.Equal(requested)
}

// setupOperationController registers the Operation reconciler like
// (*operation.Reconciler).SetupWithManager, plus a channel source through which the wiring
// requeues non-terminal Operations after a shard ownership change (an Operation skipped
// while its shard was not owned has no other event to bring it back).
func setupOperationController(mgr manager.Manager, r *operation.Reconciler, ns *core.NamespaceMatcher, workers int, requeue <-chan event.TypedGenericEvent[*v1alpha1.Operation]) error {
	allowed := predicate.NewPredicateFuncs(func(o client.Object) bool { return ns.Allowed(o.GetNamespace()) })
	appChanged := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{},
		predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			return (e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil)
		}})
	c := mgr.GetClient()
	opsOfApp := func(ctx context.Context, o client.Object) []reconcile.Request {
		ops, err := core.ListOperations(ctx, c, o.GetNamespace(), o.GetName())
		if err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range ops {
			if core.IsNonTerminal(&ops[i]) {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ops[i])})
			}
		}
		return out
	}
	if workers <= 0 {
		workers = 1
	}
	err := ctrl.NewControllerManagedBy(mgr).
		Named(operation.ControllerName).
		For(&v1alpha1.Operation{}, builder.WithPredicates(allowed)).
		Watches(&v1alpha1.Application{}, handler.EnqueueRequestsFromMapFunc(opsOfApp), builder.WithPredicates(allowed, appChanged)).
		WatchesRawSource(source.Channel(requeue, &handler.TypedEnqueueRequestForObject[*v1alpha1.Operation]{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers, SkipNameValidation: ptr.To(true)}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("create operation controller: %w", err)
	}
	return nil
}

// assignerAppHandler feeds Application destinations to the shard assigner.
type assignerAppHandler struct{ assigner *sharding.Assigner }

func (h assignerAppHandler) OnAdd(obj any, _ bool) {
	if a, ok := obj.(*v1alpha1.Application); ok {
		h.assigner.SetApplication(a)
	}
}

func (h assignerAppHandler) OnUpdate(_, obj any) { h.OnAdd(obj, false) }

func (h assignerAppHandler) OnDelete(obj any) {
	if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	if a, ok := obj.(*v1alpha1.Application); ok {
		h.assigner.DeleteApplication(a.Namespace, a.Name)
	}
}

// setupHydrator registers the source hydrator with the manager, restricted to Applications
// of allowed namespaces whose destination shard this replica owns.
func setupHydrator(ctx context.Context, mgr manager.Manager, cfg ControllerConfig, cp controllerParams, repo *apiclient.HTTPClient,
	resolver *repository.Resolver, st *settings.Manager, ns *core.NamespaceMatcher, own *sharding.Ownership,
	reg prometheus.Registerer, log *slog.Logger) error {
	commit, err := newCommitServerClient(cfg.CommitServer, cfg.InternalToken)
	if err != nil {
		return fmt.Errorf("commit-server client: %w", err)
	}
	repos := hydrator.Repositories{Resolver: resolver}
	_, err = hydrator.Setup(ctx, mgr, hydrator.Deps{
		Enabled: true, ControlPlaneNamespace: cfg.Namespace,
		RepoServer: repo, CommitServer: commit,
		Repositories: repos, WriteCredentials: repos, HelmRepositories: repos,
		Settings: st, Processors: cp.hydrationProcessors, DisableGPG: gpgDisabled(),
		AppFilter: func(a *v1alpha1.Application) bool { return ns.Allowed(a.Namespace) && own.OwnsApplication(a) },
		Registry:  reg, Logger: log,
	})
	if err != nil {
		return fmt.Errorf("set up the source hydrator: %w", err)
	}
	return nil
}

// internalHTTP is an internal listener (tree API, commit-server style).
type internalHTTP struct {
	name     string
	addr     string
	handler  http.Handler
	tls      TLSServerOptions
	shutdown time.Duration
	log      *slog.Logger
}

// serveInternalHTTP serves h until ctx ends (TLS when a certificate is configured). A
// disabled address waits for ctx.
func serveInternalHTTP(ctx context.Context, o internalHTTP, started func(net.Addr)) error {
	if !listenerEnabled(o.addr) || o.handler == nil {
		if started != nil {
			started(nil)
		}
		<-ctx.Done()
		return nil
	}
	l, err := net.Listen("tcp", o.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.addr, err)
	}
	if o.tls.Enabled() {
		tc, err := serverTLSConfig(o.tls)
		if err != nil {
			_ = l.Close()
			return err
		}
		l = tls.NewListener(l, tc)
	}
	if started != nil {
		started(l.Addr())
	}
	srv := &http.Server{
		Handler: o.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog:    slog.NewLogLogger(o.log.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	o.log.Info("serving "+o.name, "address", l.Addr().String(), "tls", o.tls.Enabled(), "mtls", o.tls.ClientCAFile != "")
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", o.name, err)
		}
		return nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.shutdown)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown %s: %w", o.name, err)
	}
	return nil
}
