package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/webhook"
)

// Defaults of the ApplicationSet controller (OPS-009, upstream ports).
const (
	DefaultAppSetMetricsAddr = ":8080"
	DefaultAppSetProbeAddr   = ":8081"
)

// AppSetConfig is the configuration of the ApplicationSet controller.
type AppSetConfig struct {
	// Run is the controller configuration (Namespace and RestConfig are required).
	Run appset.RunConfig
	// InternalToken authenticates repo-server calls (Git generators). Required.
	InternalToken string
	// RepoServer configures the repo-server client.
	RepoServer RepoServerClientOptions
	// SubmodulesEnabled and NewGitFileGlobbing configure the Git generator.
	SubmodulesEnabled, NewGitFileGlobbing bool
	// MetricsAddress serves /metrics and the probes; HealthzAddress serves the probes
	// (upstream --probe-addr); WebhookAddress, when set, receives SCM webhooks (upstream
	// :7000; patrol-server's /api/webhook refreshes ApplicationSets as well).
	MetricsAddress, HealthzAddress, WebhookAddress string
	EnablePprof                                    bool
	PprofFile                                      string
	// WebhookParallelism sizes the webhook workers.
	WebhookParallelism int
	ShutdownTimeout    time.Duration
	Registry           *prometheus.Registry
	Logger             *slog.Logger
	// OnListening, when set, receives the bound metrics address.
	OnListening func(metrics net.Addr)
}

// Validate checks the configuration.
func (c *AppSetConfig) Validate() error {
	var errs []error
	if c.Run.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	}
	if c.Run.RestConfig == nil {
		errs = append(errs, errors.New("a Kubernetes client configuration is required"))
	}
	if c.InternalToken == "" {
		errs = append(errs, errors.New("the internal token is required"))
	}
	if c.RepoServer.Address == "" {
		errs = append(errs, errors.New("repo-server address is required"))
	}
	return errors.Join(errs...)
}

// RunAppSetController runs the ApplicationSet controller with its repo-server client,
// repository credential resolution, metrics and probe listeners, and the optional webhook
// listener, until ctx ends.
func RunAppSetController(ctx context.Context, cfg AppSetConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid applicationset controller configuration: %w", err)
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
	rc := cfg.Run
	rc.Logger, rc.Registry = log, reg
	rc.Generators.Logger = log
	kube, err := kubernetes.NewForConfig(rc.RestConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rc.RestConfig)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	repoClient, err := newRepoServerClient(cfg.RepoServer, cfg.InternalToken)
	if err != nil {
		return err
	}
	lister, err := repository.NewLister(dyn, kube, repository.ListerOptions{Namespace: rc.Namespace, Logger: log})
	if err != nil {
		return err
	}
	if err := lister.Start(ctx); err != nil {
		return fmt.Errorf("start repository lister: %w", err)
	}
	resolver, err := repository.NewResolver(lister, repository.Options{Logger: log, Registerer: reg, RepoServer: repoClient})
	if err != nil {
		return err
	}
	rc.Generators.Repos = &generators.RepoServerService{
		Client: repoClient, Repositories: repositoryResolverAdapter(resolver),
		SubmodulesEnabled: cfg.SubmodulesEnabled, NewGitFileGlobbing: cfg.NewGitFileGlobbing,
	}
	rc.Generators.Dynamic = dyn

	tasks := []task{
		{name: "applicationset-controller", run: func(ctx context.Context) error { return appset.Run(ctx, rc) }},
		{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{
				MetricsAddr: cfg.MetricsAddress, HealthzAddr: cfg.HealthzAddress, Gatherer: reg,
				PprofEnabled: cfg.EnablePprof, PprofFile: cfg.PprofFile, Logger: log, ShutdownTimeout: cfg.ShutdownTimeout,
				Ready: func(context.Context) error {
					if !lister.HasSynced() {
						return errors.New("repository lister is not synced")
					}
					return nil
				},
			}, func(m, _ net.Addr) {
				if cfg.OnListening != nil {
					cfg.OnListening(m)
				}
			})
		}},
	}
	if listenerEnabled(cfg.WebhookAddress) {
		wt, err := appSetWebhook(ctx, cfg, rc, kube, reg, log)
		if err != nil {
			return err
		}
		tasks = append(tasks, wt...)
	}
	return runGroup(ctx, cfg.ShutdownTimeout, log, tasks...)
}

// appSetWebhook builds the optional webhook listener of the ApplicationSet controller: the
// same handler as patrol-server's /api/webhook (signatures verified with patrol-secret and
// SCMProvider secrets), reading through the API server.
func appSetWebhook(ctx context.Context, cfg AppSetConfig, rc appset.RunConfig, kube kubernetes.Interface, reg prometheus.Registerer, log *slog.Logger) ([]task, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(rc.RestConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	st, err := settings.NewManager(kube, settings.ManagerOptions{Namespace: rc.Namespace, Logger: log})
	if err != nil {
		return nil, err
	}
	if err := st.Start(ctx); err != nil {
		return nil, fmt.Errorf("start settings manager: %w", err)
	}
	gen := rc.Generators
	gen.Client, gen.Namespace = c, rc.Namespace
	wh, err := webhook.NewHandler(webhook.Config{
		Namespace: rc.Namespace, ApplicationSetNamespaces: rc.ApplicationSetNamespaces,
		Client: c, Reader: c, APIReader: c, Settings: webhook.CurrentSettings(st),
		Workers: cfg.WebhookParallelism, GeneratorEvaluator: appset.NewEvaluator(gen),
		ShutdownTimeout: cfg.ShutdownTimeout, Registerer: reg, Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("webhook handler: %w", err)
	}
	mux := http.NewServeMux()
	wh.Register(mux)
	return []task{
		{name: "webhook-workers", run: wh.Start},
		{name: "webhook-listener", run: func(ctx context.Context) error {
			return serveInternalHTTP(ctx, internalHTTP{name: "webhook listener", addr: cfg.WebhookAddress, handler: mux, shutdown: cfg.ShutdownTimeout, log: log}, nil)
		}},
	}, nil
}

// appSetRunConfigFromParams maps the applicationsetcontroller.* parameters.
func appSetRunConfigFromParams(p *settings.Params, namespace string, rest *krest.Config) (appset.RunConfig, error) {
	rc := appset.RunConfig{RestConfig: rest}
	rc.Namespace = namespace
	if p == nil {
		rc.EnablePolicyOverride = true
		rc.MaxResourcesStatusCount = appset.DefaultMaxResourcesStatusCount
		return rc, nil
	}
	rc.LeaderElection = p.Bool("applicationsetcontroller.enable.leader.election")
	policy := p.String("applicationsetcontroller.policy")
	pol, err := appset.ParsePolicy(policy)
	if err != nil {
		return rc, err
	}
	rc.Policy = pol
	if v, ok := p.Lookup("applicationsetcontroller.enable.policy.override"); ok && v.Source != settings.ParamFromDefault {
		rc.EnablePolicyOverride = p.Bool("applicationsetcontroller.enable.policy.override")
	} else {
		rc.EnablePolicyOverride = appset.DefaultEnablePolicyOverride(policy)
	}
	rc.ConcurrentReconciliations = int(p.Int("applicationsetcontroller.concurrent.reconciliations.max"))
	rc.DryRun = p.Bool("applicationsetcontroller.dryrun")
	rc.EnableProgressiveSyncs = p.Bool("applicationsetcontroller.enable.progressive.syncs")
	rc.ApplicationSetNamespaces = p.Strings("applicationsetcontroller.namespaces")
	rc.GlobalPreservedAnnotations = p.Strings("applicationsetcontroller.global.preserved.annotations")
	rc.GlobalPreservedLabels = p.Strings("applicationsetcontroller.global.preserved.labels")
	rc.MaxResourcesStatusCount = int(p.Int("applicationsetcontroller.status.max.resources.count"))
	rc.Generators.DefaultRequeueAfter = p.Duration("applicationsetcontroller.requeue.after")
	rc.Generators.SCM = generators.SCMConfig{
		Enabled:            p.Bool("applicationsetcontroller.enable.scm.providers"),
		AllowedProviders:   p.Strings("applicationsetcontroller.allowed.scm.providers"),
		RootCAPath:         p.String("applicationsetcontroller.scm.root.ca.path"),
		TokenRefStrictMode: p.Bool("applicationsetcontroller.enable.tokenref.strict.mode"),
	}
	return rc, nil
}
