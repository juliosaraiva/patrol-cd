// Package transport selects how the patrol CLI reaches patrol-server.
//
// A Provider turns the resolved server options into a Connection: the address the API
// client uses, its TLS settings and optionally an *http.Client that replaces the one built
// from them. Three providers exist:
//
//   - Direct (default): the server address as given (HTTPS unless --plaintext);
//   - PortForward (--port-forward / --port-forward-namespace): a Kubernetes port-forward
//     to a ready patrol-server pod, exposed on a random 127.0.0.1 port (API-054);
//   - core mode (--core, API-055): an in-process API server talking to Kubernetes with the
//     kubeconfig credentials. It is provided by internal/cli/core (which reaches the
//     repo-server and the controller with PodDialer, the listener-free variant of the
//     port-forward machinery); builds without it use Unavailable, which fails with
//     ErrCoreModeUnavailable.
//
// Connections are closed by the CLI when the command finishes.
package transport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"k8s.io/client-go/tools/clientcmd"
)

// ErrCoreModeUnavailable is returned by the default core-mode provider.
var ErrCoreModeUnavailable = errors.New("core mode not available in this build")

// Request describes the server the CLI wants to reach.
type Request struct {
	// ServerAddr is the configured address ("host[:port]" or URL); empty for port-forward
	// and core mode.
	ServerAddr string
	PlainText  bool
	Insecure   bool
	// Kube configures Kubernetes access for port-forward and core mode.
	Kube KubeOptions
	// Logger receives diagnostics (never credentials).
	Logger *slog.Logger
}

// KubeOptions locate the Kubernetes cluster that runs Patrol.
type KubeOptions struct {
	// KubeconfigPaths are the kubeconfig files to merge (KUBECONFIG); empty uses
	// ~/.kube/config.
	KubeconfigPaths []string
	// Overrides are the kubectl override flags (--kube-context sets CurrentContext).
	Overrides clientcmd.ConfigOverrides
	// Namespace is the Patrol namespace (--port-forward-namespace); empty uses the
	// kubeconfig context namespace.
	Namespace string
	// ServerName, RepoServerName and ControllerName are the app.kubernetes.io/name label
	// values of the components.
	ServerName     string
	RepoServerName string
	ControllerName string
}

// ClientConfig returns the kubeconfig loader for k.
func (k KubeOptions) ClientConfig() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(k.KubeconfigPaths) > 0 {
		rules.Precedence = k.KubeconfigPaths
	}
	overrides := k.Overrides
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &overrides)
}

// Connection is an established way to reach the server.
type Connection struct {
	// ServerAddr is the address the API client must use.
	ServerAddr string
	PlainText  bool
	Insecure   bool
	// HTTPClient, when set, replaces the client the API client builds from the TLS
	// options (in-process transports).
	HTTPClient *http.Client
	// AuthDisabled reports a server that needs no token (core mode).
	AuthDisabled bool
	closeFn      func() error
}

// NewConnection returns a connection that calls closeFn (may be nil) on Close.
func NewConnection(c Connection, closeFn func() error) *Connection {
	c.closeFn = closeFn
	return &c
}

// Close releases the connection's resources (port-forward listener, in-process server).
func (c *Connection) Close() error {
	if c == nil || c.closeFn == nil {
		return nil
	}
	fn := c.closeFn
	c.closeFn = nil
	return fn()
}

// Provider establishes connections.
type Provider interface {
	Connect(ctx context.Context, req Request) (*Connection, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, req Request) (*Connection, error)

// Connect calls f.
func (f ProviderFunc) Connect(ctx context.Context, req Request) (*Connection, error) {
	return f(ctx, req)
}

// Direct connects to the configured address.
var Direct Provider = ProviderFunc(func(_ context.Context, req Request) (*Connection, error) {
	if req.ServerAddr == "" {
		return nil, errors.New("Patrol server address unspecified") //nolint:staticcheck // upstream wording
	}
	return NewConnection(Connection{ServerAddr: req.ServerAddr, PlainText: req.PlainText, Insecure: req.Insecure}, nil), nil
})

// Unavailable is a provider that always fails with err.
func Unavailable(err error) Provider {
	return ProviderFunc(func(context.Context, Request) (*Connection, error) { return nil, err })
}
