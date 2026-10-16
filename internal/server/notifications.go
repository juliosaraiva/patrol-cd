package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	notifcontroller "github.com/juliosaraiva/patrol-cd/internal/notifications/controller"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
)

// Defaults of the notifications controller (upstream ports and timeouts).
const (
	DefaultNotificationsMetricsAddr = ":9001"
	// notificationsRepoTimeout is the repo-server call timeout of the repo.* template
	// helpers (fixed upstream).
	notificationsRepoTimeout = 5 * time.Second
	// DefaultRepoServerAddress is the default repo-server address of components without the
	// repo.server parameter.
	DefaultRepoServerAddress = "patrol-repo-server:8081"
)

// NotificationsConfig is the configuration of the notifications controller.
type NotificationsConfig struct {
	// Controller is the controller configuration (RestConfig and Namespace are required).
	Controller notifcontroller.Config
	// InternalToken and RepoServer configure the repo-server client of the repo.* helpers;
	// an empty token disables them (they fail with "repo service unavailable").
	InternalToken string
	RepoServer    RepoServerClientOptions
	// MetricsAddress serves /metrics and the probes; HealthzAddress optionally the probes.
	MetricsAddress, HealthzAddress string
	EnablePprof                    bool
	PprofFile                      string
	ShutdownTimeout                time.Duration
	Registry                       *prometheus.Registry
	Logger                         *slog.Logger
	// OnListening, when set, receives the bound metrics address.
	OnListening func(metrics net.Addr)
}

// Validate checks the configuration.
func (c *NotificationsConfig) Validate() error {
	var errs []error
	if c.Controller.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	}
	if c.Controller.RestConfig == nil {
		errs = append(errs, errors.New("a Kubernetes client configuration is required"))
	}
	return errors.Join(errs...)
}

// repoResolverFunc adapts the repository resolver to the notifications controller.
type repoResolverFunc func(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)

func (f repoResolverFunc) ResolveRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error) {
	return f(ctx, repoURL, project)
}

// RunNotificationsController runs the notifications controller with the repo-server client
// of the repo.* helpers (credentials resolved from Repository objects) and the metrics and
// probe listeners until ctx ends.
func RunNotificationsController(ctx context.Context, cfg NotificationsConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid notifications controller configuration: %w", err)
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
	nc := cfg.Controller
	nc.Registry, nc.Logger = reg, log
	var ready func(context.Context) error
	if cfg.InternalToken != "" && cfg.RepoServer.Address != "" {
		ro := cfg.RepoServer
		if ro.Timeout <= 0 {
			ro.Timeout = notificationsRepoTimeout
		}
		repoClient, err := newRepoServerClient(ro, cfg.InternalToken)
		if err != nil {
			return err
		}
		kube, err := kubernetes.NewForConfig(nc.RestConfig)
		if err != nil {
			return fmt.Errorf("create Kubernetes client: %w", err)
		}
		dyn, err := dynamic.NewForConfig(nc.RestConfig)
		if err != nil {
			return fmt.Errorf("create dynamic client: %w", err)
		}
		lister, err := repository.NewLister(dyn, kube, repository.ListerOptions{Namespace: nc.Namespace, Logger: log})
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
		nc.RepoClient = repoClient
		nc.RepoResolver = repoResolverFunc(resolver.GetRepository)
		ready = func(context.Context) error {
			if !lister.HasSynced() {
				return errors.New("repository lister is not synced")
			}
			return nil
		}
	} else {
		log.Warn("no internal token or repo-server address: the repo.* template helpers are unavailable")
	}
	return runGroup(ctx, cfg.ShutdownTimeout, log,
		task{name: "notifications-controller", run: func(ctx context.Context) error { return notifcontroller.Run(ctx, nc) }},
		task{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{
				MetricsAddr: cfg.MetricsAddress, HealthzAddr: cfg.HealthzAddress, Gatherer: reg, Ready: ready,
				PprofEnabled: cfg.EnablePprof, PprofFile: cfg.PprofFile, Logger: log, ShutdownTimeout: cfg.ShutdownTimeout,
			}, func(m, _ net.Addr) {
				if cfg.OnListening != nil {
					cfg.OnListening(m)
				}
			})
		}},
	)
}
