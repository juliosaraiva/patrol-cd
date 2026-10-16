package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/ui"
)

// Defaults of patrol-server (upstream ports, OPS-009).
const (
	DefaultServerPort        = 8080
	DefaultServerMetricsPort = 8083
	// DefaultExtensionsDir holds UI extension bundles (upstream /tmp/extensions).
	DefaultExtensionsDir = "/tmp/extensions"
	// DefaultDownloadDir holds CLI binaries served under /download (patrol-<os>-<arch>).
	DefaultDownloadDir = "/shared/download"
	// DefaultDexServer is the bundled Dex address (server.dex.server).
	DefaultDexServer = "patrol-dex-server:5556"
	// signingKeyWait bounds the wait for the session signing key after bootstrap.
	signingKeyWait = time.Minute
)

// Config is the configuration of patrol-server.
type Config struct {
	// Namespace is the control-plane namespace. Required.
	Namespace string
	// RestConfig reaches the control-plane cluster. Required.
	RestConfig *krest.Config

	// ListenAddress serves the API, UI and webhooks (host:port). Required.
	ListenAddress string
	// MetricsAddress serves /metrics, the probes and the profiler ("-" disables).
	MetricsAddress string
	// HealthzAddress optionally serves the probes on their own listener.
	HealthzAddress string
	// EnablePprof serves the profiler without the per-request file check; PprofFile is the
	// file whose content "true" enables it per request.
	EnablePprof bool
	PprofFile   string

	// Insecure serves plain HTTP (TLS terminated in front of the server).
	Insecure bool
	// TLS: version range, TLS 1.2 ciphers and an optional certificate pair read from files
	// (reloaded on change). Without files the patrol-secret tls.crt/tls.key pair is used, else
	// a generated self-signed certificate.
	TLSMinVersion, TLSMaxVersion, TLSCiphers string
	TLSCertFile, TLSKeyFile                  string

	// RootPath mounts the server under a path prefix. BaseHRef is the UI base path (<base
	// href>, cookie path) without a root path; with one, the root path is the base (upstream
	// forces it, warning when BaseHRef differs).
	RootPath, BaseHRef string
	// XFrameOptions and ContentSecurityPolicy are header values (nil: defaults; "": none).
	XFrameOptions, ContentSecurityPolicy *string
	// DisableAuth disables authentication and authorization.
	DisableAuth bool
	// TrustedProxies are reverse proxies whose X-Forwarded-For is believed.
	TrustedProxies []netip.Prefix

	// UI is the web UI (nil: the embedded build, or the "UI not built" page). StaticAssetsDir
	// adds the files of a directory (--staticassets): looked up after the UI, confined to the
	// directory with os.OpenRoot; a missing directory is skipped with a warning.
	UI              fs.FS
	StaticAssetsDir string
	// ExtensionsDir holds UI extensions (/extensions.js, /extensions/<path>).
	ExtensionsDir string
	// DownloadDir holds CLI binaries (/download/patrol-<os>-<arch>).
	DownloadDir string

	// InternalToken authenticates calls to the repo-server and the controller tree API.
	// Required.
	InternalToken string
	// RepoServer configures the repo-server client.
	RepoServer RepoServerClientOptions
	// TreeAPI configures the controller tree API client.
	TreeAPI TreeAPIClientOptions
	// DexServer is the bundled Dex address; DexPlaintext and DexStrictTLS select the
	// transport (OPS-102). Strict TLS verifies Dex against the CA bundle DexCAFile and pins
	// its certificate to DexCertFile (defaults: ca.crt and tls.crt in DefaultDexTLSDir).
	DexServer                  string
	DexPlaintext, DexStrictTLS bool
	DexCAFile, DexCertFile     string

	// ApplicationNamespaces and ApplicationSetNamespaces are the additional namespaces
	// (globs or "/regexp/") of Applications and ApplicationSets.
	ApplicationNamespaces    []string
	ApplicationSetNamespaces []string
	// HydratorEnabled enables the write-repository routes.
	HydratorEnabled bool
	// ConnectionStateTTL is the age after which repository connection states are checked
	// again (server.connection.status.cache.expiration; 0: the service default).
	ConnectionStateTTL time.Duration
	// SyncWithReplaceAllowed permits Replace=true syncs through the API.
	SyncWithReplaceAllowed bool
	// EnableProxyExtension enables proxy extensions.
	EnableProxyExtension bool
	// WebhookParallelism and WebhookRefreshWorkers size the webhook workers.
	WebhookParallelism, WebhookRefreshWorkers int
	// AppSet configures ApplicationSet generation through the API.
	AppSet AppSetAPIOptions
	// AllowClusterExec lets cluster connection checks run exec plugins and use the server's
	// cloud identity (off by default, see clusters.ConnectorOptions).
	AllowClusterExec bool
	// ClusterTuning configures clients of destination clusters (nil: defaults).
	ClusterTuning *clusters.Tuning

	// ShutdownTimeout bounds the graceful drain.
	ShutdownTimeout time.Duration
	// Registry receives the metrics (nil: a new registry with Go and process collectors).
	Registry *prometheus.Registry
	// Logger is the server logger (nil: slog.Default()).
	Logger *slog.Logger
	// OnListening, when set, is called with the bound API and metrics addresses (tests).
	OnListening func(api, metrics net.Addr)
}

// AppSetAPIOptions configure ApplicationSet generation in the API server (upstream
// --appset-* flags).
type AppSetAPIOptions struct {
	EnableSCMProviders       bool
	AllowedSCMProviders      []string
	SCMRootCAPath            string
	EnableNewGitFileGlobbing bool
	EnableGitHubAPIMetrics   bool
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	}
	if c.RestConfig == nil {
		errs = append(errs, errors.New("a Kubernetes client configuration is required"))
	}
	if c.ListenAddress == "" {
		errs = append(errs, errors.New("listen address is required"))
	} else if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		errs = append(errs, fmt.Errorf("listen address %q: %w", c.ListenAddress, err))
	}
	if listenerEnabled(c.MetricsAddress) {
		if _, _, err := net.SplitHostPort(c.MetricsAddress); err != nil {
			errs = append(errs, fmt.Errorf("metrics address %q: %w", c.MetricsAddress, err))
		}
	}
	if c.InternalToken == "" {
		errs = append(errs, errors.New("the internal token is required"))
	}
	if c.RepoServer.Address == "" {
		errs = append(errs, errors.New("repo-server address is required"))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("--tls-cert and --tls-key must be set together"))
	}
	if c.RootPath != "" && !strings.HasPrefix(c.RootPath, "/") {
		errs = append(errs, fmt.Errorf("root path %q must start with /", c.RootPath))
	}
	if _, err := apiserver.ParseTLSVersion(orDefault(c.TLSMinVersion, "1.2")); err != nil {
		errs = append(errs, fmt.Errorf("TLS min version: %w", err))
	}
	if _, err := apiserver.ParseTLSVersion(orDefault(c.TLSMaxVersion, "1.3")); err != nil {
		errs = append(errs, fmt.Errorf("TLS max version: %w", err))
	}
	if _, err := apiserver.ParseCipherSuites(splitCiphers(c.TLSCiphers)); err != nil {
		errs = append(errs, fmt.Errorf("TLS ciphers: %w", err))
	}
	if c.WebhookParallelism < 0 || c.WebhookRefreshWorkers < 0 {
		errs = append(errs, errors.New("webhook worker counts must not be negative"))
	}
	return errors.Join(errs...)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Run assembles patrol-server and serves until ctx ends, then drains for at most
// ShutdownTimeout. It is the entry point of `patrolcd server` and of the integration tests.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid server configuration: %w", err)
	}
	s, err := newServerAssembly(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.close()
	return s.run(ctx)
}

// serverAssembly is the dependency graph of patrol-server.
type serverAssembly struct {
	cfg      Config
	log      *slog.Logger
	registry *prometheus.Registry

	kube      kubernetes.Interface
	dyn       dynamic.Interface
	direct    client.WithWatch
	cache     crcache.Cache
	reader    client.Reader
	recorder  *eventRecorder
	settings  *settings.Manager
	enforcer  *rbac.Enforcer
	lister    *repository.Lister
	registryC *clusters.Registry
	api       *apiserver.Server
	webhook   webhookRunner
	// staticRoot is the opened static assets directory (closed with the assembly).
	staticRoot *os.Root

	cacheSynced atomic.Bool
}

// webhookRunner is the part of *webhook.Handler the server runs.
type webhookRunner interface {
	Start(ctx context.Context) error
}

func newServerAssembly(ctx context.Context, cfg Config) (*serverAssembly, error) {
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
	s := &serverAssembly{cfg: cfg, log: log, registry: reg}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	if s.kube, err = kubernetes.NewForConfig(cfg.RestConfig); err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	if s.dyn, err = dynamic.NewForConfig(cfg.RestConfig); err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	if s.direct, err = client.NewWithWatch(cfg.RestConfig, client.Options{Scheme: scheme}); err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	control := map[string]crcache.Config{cfg.Namespace: {}}
	if s.cache, err = crcache.New(cfg.RestConfig, crcache.Options{
		Scheme:                      scheme,
		ReaderFailOnMissingInformer: true,
		ByObject: map[client.Object]crcache.ByObject{
			&v1alpha1.Application{}:    {Namespaces: cacheNamespaces(cfg.Namespace, cfg.ApplicationNamespaces)},
			&v1alpha1.ApplicationSet{}: {Namespaces: cacheNamespaces(cfg.Namespace, cfg.ApplicationSetNamespaces)},
			&v1alpha1.AppProject{}:     {Namespaces: control},
			&v1alpha1.Cluster{}:        {Namespaces: control},
			&v1alpha1.Repository{}:     {Namespaces: control},
			&v1alpha1.SCMProvider{}:    {Namespaces: cacheNamespaces(cfg.Namespace, cfg.ApplicationSetNamespaces)},
		},
	}); err != nil {
		return nil, fmt.Errorf("create informer cache: %w", err)
	}
	for _, obj := range []client.Object{&v1alpha1.Application{}, &v1alpha1.ApplicationSet{}, &v1alpha1.AppProject{},
		&v1alpha1.Cluster{}, &v1alpha1.Repository{}, &v1alpha1.SCMProvider{}} {
		if _, err := s.cache.GetInformer(ctx, obj); err != nil {
			return nil, fmt.Errorf("create %T informer: %w", obj, err)
		}
	}
	s.reader = &splitReader{cached: s.cache, live: s.direct, isCached: isPatrolCachedType}
	s.recorder = newEventRecorder(s.kube, scheme, "patrol-server")

	// Settings: bootstrap the session signing key (upstream generates server.secretkey), then
	// follow every settings object.
	if generated, err := settings.EnsureServerSecretKey(ctx, s.kube, cfg.Namespace, settings.WithBootstrapLogger(log)); err != nil {
		s.close()
		return nil, fmt.Errorf("bootstrap %s: %w", settings.DefaultSecretName, err)
	} else if generated {
		log.Info("generated the session signing key", "secret", settings.DefaultSecretName)
	}
	settingsMetrics, err := settings.NewPrometheusMetrics(reg)
	if err != nil {
		s.close()
		return nil, err
	}
	if s.settings, err = settings.NewManager(s.kube, settings.ManagerOptions{
		Namespace: cfg.Namespace, Recorder: s.recorder, Metrics: settingsMetrics, Logger: log,
	}); err != nil {
		s.close()
		return nil, err
	}
	if err := s.settings.Start(ctx); err != nil {
		s.close()
		return nil, fmt.Errorf("start settings manager: %w", err)
	}
	keys, err := s.waitSigningKeys(ctx)
	if err != nil {
		s.close()
		return nil, err
	}

	// Repositories and clusters.
	if s.lister, err = repository.NewLister(s.dyn, s.kube, repository.ListerOptions{Namespace: cfg.Namespace, Logger: log}); err != nil {
		s.close()
		return nil, err
	}
	inCluster := s.settings.Current().InClusterEnabled()
	if s.registryC, err = clusters.NewRegistry(s.dyn, s.kube, clusters.RegistryOptions{
		Namespace: cfg.Namespace, InClusterEnabled: &inCluster, Logger: log,
		LocalServerVersion: localServerVersion(s.kube),
	}); err != nil {
		s.close()
		return nil, err
	}

	if err := s.buildAPI(ctx, keys); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// waitSigningKeys waits until the settings snapshot carries a usable signing key (the
// Secret written by the bootstrap reaches the informer shortly after).
func (s *serverAssembly) waitSigningKeys(ctx context.Context) (auth.KeySet, error) {
	deadline := time.Now().Add(signingKeyWait)
	for {
		var lastErr error
		if st := s.settings.Current(); st != nil {
			ks, err := st.SigningKeys()
			if err == nil {
				return ks, nil
			}
			lastErr = err
		}
		if time.Now().After(deadline) {
			return auth.KeySet{}, fmt.Errorf("no session signing key in %s: %w", settings.DefaultSecretName, lastErr)
		}
		select {
		case <-ctx.Done():
			return auth.KeySet{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// localServerVersion probes the control-plane cluster version (status of the synthetic
// in-cluster cluster).
func localServerVersion(kube kubernetes.Interface) func(ctx context.Context) (string, error) {
	return func(_ context.Context) (string, error) {
		v, err := kube.Discovery().ServerVersion()
		if err != nil {
			return "", err
		}
		return v.GitVersion, nil
	}
}

// uiFS returns the UI file system: the configured one or the embedded build, combined with
// the static assets directory (OPS-111).
func (s *serverAssembly) uiFS() (fs.FS, error) {
	base := s.cfg.UI
	if base == nil {
		var built bool
		base, built = ui.FS()
		if !built {
			s.log.Warn("the web UI is not built into this binary; serving a placeholder page (run `make ui-build`)")
		}
	}
	fsys, root, err := withStaticAssets(base, s.cfg.StaticAssetsDir, s.log)
	if err != nil {
		return nil, err
	}
	s.staticRoot = root
	return fsys, nil
}

// withStaticAssets combines the UI file system with the static assets directory (upstream
// --staticassets): files are looked up in the UI first, then in the directory, as upstream
// does. The directory is opened with os.OpenRoot, so symbolic links that stay inside it work
// (ConfigMap volumes link every key through ..data) while a link leading out of it (to
// /etc/passwd, or a mounted token) fails instead of being served. A missing directory only
// logs upstream's warning; any other open error is fatal. The returned root (nil when the
// directory is not used) must be closed after serving.
func withStaticAssets(base fs.FS, dir string, log *slog.Logger) (fs.FS, *os.Root, error) {
	if dir == "" {
		return base, nil, nil
	}
	root, err := os.OpenRoot(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Warn(fmt.Sprintf("Static assets directory %q does not exist, using only embedded assets", dir))
		return base, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("static assets directory: %w", err)
	}
	return layeredFS{base, root.FS()}, root, nil
}

// layeredFS opens a name in each file system in order and returns the first success, else
// the last error (upstream composableFS). An error other than fs.ErrNotExist from the
// static assets root (a link escaping the directory) therefore reaches the UI handler,
// which never serves the file.
type layeredFS []fs.FS

// Open implements fs.FS.
func (l layeredFS) Open(name string) (fs.File, error) {
	err := error(&fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist})
	for _, f := range l {
		var file fs.File
		if file, err = f.Open(name); err == nil {
			return file, nil
		}
	}
	return nil, err
}

// apiConfig maps Config to the apiserver configuration.
func (s *serverAssembly) apiConfig() (apiserver.Config, error) {
	c := s.cfg
	uiFS, err := s.uiFS()
	if err != nil {
		return apiserver.Config{}, err
	}
	ac := apiserver.Config{
		Namespace:             c.Namespace,
		ListenAddress:         c.ListenAddress,
		MetricsAddress:        "-", // served by the observability listener (with the profiler)
		Insecure:              c.Insecure,
		RootPath:              c.RootPath,
		BaseHRef:              effectiveBaseHRef(c.RootPath, c.BaseHRef, s.log),
		XFrameOptions:         c.XFrameOptions,
		ContentSecurityPolicy: c.ContentSecurityPolicy,
		UI:                    uiFS,
		DisableAuth:           c.DisableAuth,
		ShutdownTimeout:       c.ShutdownTimeout,
		TrustedProxies:        c.TrustedProxies,
		TLS:                   apiserver.TLSOptions{MinVersion: c.TLSMinVersion, MaxVersion: c.TLSMaxVersion},
		Core: core.Config{
			AppsInAnyNamespaceEnabled: len(c.ApplicationNamespaces) > 0,
			HydratorEnabled:           c.HydratorEnabled,
			SyncWithReplaceAllowed:    c.SyncWithReplaceAllowed,
		},
	}
	if !c.Insecure {
		ciphers, ignored, added, err := apiCipherSuites(c.TLSMinVersion, c.TLSCiphers)
		if err != nil {
			return apiserver.Config{}, fmt.Errorf("TLS ciphers: %w", err)
		}
		ac.TLS.Ciphers = ciphers
		if ignored {
			s.log.Warn("TLSv1.3 cipher suites are not configurable, ignoring value of --tlsciphers")
		}
		if len(added) > 0 {
			level := slog.LevelWarn
			if c.TLSCiphers == DefaultTLSCiphers {
				level = slog.LevelInfo
			}
			s.log.Log(context.Background(), level, "TLS cipher suites lack the suites HTTP/2 requires; added them",
				"configured", c.TLSCiphers, "added", strings.Join(added, ":"))
		}
	}
	if c.TLSCertFile != "" && !c.Insecure {
		// Certificate files (a mounted TLS Secret) win over the patrol-secret pair, which
		// wins over the generated self-signed certificate (upstream order, OPS-096).
		fc, err := newFileCertificate(c.TLSCertFile, c.TLSKeyFile)
		if err != nil {
			return apiserver.Config{}, err
		}
		src := s.settings
		ac.TLS.Certificate = func() *tls.Certificate {
			if cert := fc.get(); cert != nil {
				return cert
			}
			if st := src.Current(); st != nil {
				return st.ServerCertificate()
			}
			return nil
		}
	}
	return ac, nil
}

// effectiveBaseHRef returns the UI base path (OPS-105): with a root path, the root path
// itself, as upstream argocd-server forces it, logging upstream's warning when the base href
// was set to something else (the default "/" included); otherwise baseHRef.
func effectiveBaseHRef(rootPath, baseHRef string, log *slog.Logger) string {
	if strings.Trim(rootPath, "/") == "" {
		return baseHRef
	}
	if baseHRef != "" && strings.Trim(baseHRef, "/") != strings.Trim(rootPath, "/") {
		log.Warn(fmt.Sprintf("--basehref and --rootpath had conflict: basehref: %s rootpath: %s", baseHRef, rootPath))
	}
	return rootPath
}

// ready reports readiness: settings loaded, caches and listers synced.
func (s *serverAssembly) ready(context.Context) error {
	switch {
	case s.settings.Current() == nil:
		return errors.New("settings are not loaded")
	case !s.cacheSynced.Load():
		return errors.New("informer caches are not synced")
	case !s.lister.HasSynced():
		return errors.New("repository lister is not synced")
	case !s.registryC.HasSynced():
		return errors.New("cluster registry is not synced")
	}
	return nil
}

func (s *serverAssembly) run(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenAddress, err)
	}
	var metricsAddr net.Addr
	metricsBound := make(chan struct{})
	tasks := []task{
		{name: "informer-cache", run: func(ctx context.Context) error { return s.cache.Start(ctx) }},
		{name: "cache-sync", run: func(ctx context.Context) error {
			if !s.cache.WaitForCacheSync(ctx) {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("informer caches did not sync")
			}
			s.cacheSynced.Store(true)
			s.bootstrapObjects(ctx)
			return nil
		}},
		{name: "repository-lister", run: func(ctx context.Context) error {
			if err := s.lister.Start(ctx); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		}},
		{name: "cluster-registry", run: func(ctx context.Context) error {
			if err := s.registryC.Start(ctx); err != nil {
				return err
			}
			unsubscribe := s.settings.Subscribe(func(_, cur *settings.Settings) {
				s.registryC.SetInClusterEnabled(cur.InClusterEnabled())
			})
			defer unsubscribe()
			<-ctx.Done()
			return nil
		}},
		{name: "webhook", run: s.webhook.Start},
		{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{
				MetricsAddr: s.cfg.MetricsAddress, HealthzAddr: s.cfg.HealthzAddress, Gatherer: s.registry,
				Ready: s.ready, PprofEnabled: s.cfg.EnablePprof, PprofFile: s.cfg.PprofFile, Logger: s.log,
				ShutdownTimeout: s.cfg.ShutdownTimeout,
			}, func(m, _ net.Addr) { metricsAddr = m; close(metricsBound) })
		}},
		{name: "api", run: func(ctx context.Context) error {
			select {
			case <-metricsBound:
			case <-ctx.Done():
				_ = l.Close()
				return nil
			}
			if s.cfg.OnListening != nil {
				s.cfg.OnListening(l.Addr(), metricsAddr)
			}
			return s.api.Serve(ctx, l, nil)
		}},
	}
	return runGroup(ctx, s.cfg.ShutdownTimeout, s.log, tasks...)
}

// close releases resources that outlive run (event broadcaster).
func (s *serverAssembly) close() {
	if s.recorder != nil {
		s.recorder.Shutdown()
	}
	if s.staticRoot != nil {
		_ = s.staticRoot.Close()
	}
}

// projectGetter reads AppProjects of the control-plane namespace through the cache.
func (s *serverAssembly) projectGetter() apiserver.ProjectGetter {
	return func(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
		var p v1alpha1.AppProject
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
}

// auditRecorder adapts the event recorder for audit Events.
func (s *serverAssembly) auditRecorder() rest.EventRecorder { return s.recorder }
