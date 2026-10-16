// Package core implements `patrol --core` (API-055, docs/architecture/components/cli-core-mode.md):
// the transport.Provider that runs the patrol-server API inside the CLI process
// (server.NewCoreHandler) and connects the API client to it through an in-memory
// http.RoundTripper. No Patrol CD server is contacted and no port is bound. Authentication
// and Patrol RBAC are disabled; every Kubernetes call uses the caller's kubeconfig
// credentials, so their Kubernetes RBAC is the authorization. The repo-server and the
// controller tree API are reached through Kubernetes port-forward (transport.PodDialer)
// with the same credentials, or through an Endpoints override in tests.
//
// Register (a cli.Registrar) installs the provider on the CLI factory and refuses the
// commands that need Patrol CD sessions.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/server"
)

// Default client tuning of the caller's configuration (client-go defaults to 5 QPS).
const (
	defaultQPS   = 50
	defaultBurst = 100
)

// Options configure the provider.
type Options struct {
	// Endpoints replaces port-forwarding to the repo-server and the controller (tests).
	Endpoints *Endpoints
	// LookupEnv reads EnvEndpoints and EnvAllowEndpoints (nil: the override is only taken
	// from Endpoints).
	LookupEnv func(string) (string, bool)
	// DialPod overrides the port-forward dialer (tests). Nil uses transport.PodDialer.
	DialPod DialPodFunc
	// Version is the CLI version (User-Agent).
	Version string
	// ConfigureServer adjusts the server configuration before it is built (tests).
	ConfigureServer func(*server.CoreConfig)
}

// Provider is the core-mode transport.Provider.
type Provider struct {
	opts Options
}

// NewProvider returns a provider.
func NewProvider(o Options) *Provider { return &Provider{opts: o} }

// Connect implements transport.Provider: it loads the kubeconfig, assembles the
// in-process API server and returns a connection whose HTTP client serves every request
// in-process. Closing the connection stops the server, the port-forward streams and the
// informers it started.
func (p *Provider) Connect(ctx context.Context, req transport.Request) (*transport.Connection, error) {
	log := req.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cc := req.Kube.ClientConfig()
	restCfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("core mode: load kubeconfig: %w", err)
	}
	restCfg = rest.CopyConfig(restCfg)
	if restCfg.QPS == 0 {
		restCfg.QPS = defaultQPS
	}
	if restCfg.Burst == 0 {
		restCfg.Burst = defaultBurst
	}
	restCfg.UserAgent = strings.TrimSpace("patrol-cli/" + p.opts.Version + " (core mode)")
	ns := resolveNamespace(cc, req.Kube.Overrides)

	endpoints := p.opts.Endpoints
	if endpoints == nil {
		if endpoints, err = endpointsFromEnv(p.opts.LookupEnv); err != nil {
			return nil, err
		}
	}
	var closers []func() error
	closeAll := func() error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			if err := closers[i](); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	scfg := server.CoreConfig{
		Namespace:         ns,
		RestConfig:        restCfg,
		DestinationConfig: newDestinationConfigs(cc, restCfg).RESTConfig,
		TreeAPI: server.TreeAPIClientOptions{
			StatefulSet: treeapi.DefaultStatefulSet, Service: treeapi.DefaultService,
			TLS: server.TLSClientOptions{Plaintext: true},
		},
		OnWarning: func(msg string) { log.Warn(msg) },
		Logger:    serverLogger(ctx, log),
	}
	if endpoints != nil {
		if err := endpoints.Validate(); err != nil {
			return nil, fmt.Errorf("core mode endpoints: %w", err)
		}
		scfg.RepoServerURL, scfg.TreeAPI.URL = endpoints.RepoServer, endpoints.Controller
		if tok := endpoints.Token; tok != "" {
			scfg.InternalToken = func(context.Context) (string, error) { return tok, nil }
		}
		log.Debug("core mode: using explicit repo-server and controller endpoints (test/debug override)")
	} else {
		dial := p.opts.DialPod
		if dial == nil {
			pd, err := transport.NewPodDialer(restCfg, log)
			if err != nil {
				return nil, err
			}
			closers = append(closers, pd.Close)
			dial = pd.DialPod
		}
		hc, err := rest.HTTPClientFor(restCfg)
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("create Kubernetes HTTP client: %w", err)
		}
		closers = append(closers, func() error { utilnet.CloseIdleConnectionsFor(hc.Transport); return nil })
		kube, err := kubernetes.NewForConfigAndClient(restCfg, hc)
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("create Kubernetes client: %w", err)
		}
		router := newPodRouter(kube, ns, nameOr(req.Kube.RepoServerName, cmdutil.DefaultRepoServerName),
			nameOr(req.Kube.ControllerName, cmdutil.DefaultControllerName), treeapi.DefaultStatefulSet, dial)
		scfg.Dial = router.Dial
	}
	if p.opts.ConfigureServer != nil {
		p.opts.ConfigureServer(&scfg)
	}
	h, stop, err := server.NewCoreHandler(ctx, scfg)
	if err != nil {
		_ = closeAll()
		return nil, fmt.Errorf("core mode: %w", err)
	}
	closers = append(closers, func() error { stop(); return nil })
	ht := NewHandlerTransport(h)
	closers = append(closers, ht.Close)
	log.Debug("core mode: in-process API server ready", slog.String("namespace", ns))
	return transport.NewConnection(transport.Connection{
		ServerAddr:   cmdutil.CoreServer,
		PlainText:    true,
		HTTPClient:   &http.Client{Transport: ht},
		AuthDisabled: true,
	}, closeAll), nil
}

func nameOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// serverLogger passes the in-process server logs to the CLI logger only at debug level:
// the CLI reports failures through the API errors.
func serverLogger(ctx context.Context, log *slog.Logger) *slog.Logger {
	if log.Enabled(ctx, slog.LevelDebug) {
		return log.With(slog.String("component", "core-server"))
	}
	return slog.New(slog.DiscardHandler)
}

// IsInProcess reports whether c serves requests in-process (core mode), i.e. through a
// HandlerTransport and not over a network connection.
func IsInProcess(c *http.Client) bool {
	if c == nil {
		return false
	}
	_, ok := c.Transport.(*HandlerTransport)
	return ok
}
