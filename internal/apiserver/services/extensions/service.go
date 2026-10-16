// Package extensions implements patrol-server's proxy extensions (parity API-046..049,
// Beta in the baseline): requests to /extensions/<name>/<path> are authorized for the
// Application named in the Patrol-Application-Name and Patrol-Project-Name headers and
// forwarded to the backend service that patrol-cm extension.config (or
// extension.config.<name>) configures for the Application's destination cluster.
//
// Security model (threat model T1, T8, T16, T22):
//   - disabled unless Config.Enabled (server.enable.proxy.extension); then 404;
//   - authentication by the route chain (cookie-authenticated mutations need the CSRF
//     header); authorization requires "extensions, invoke, <name>" and "applications, get"
//     on the Application, which must exist, belong to the named project, live in an enabled
//     namespace allowed by the project and target a destination the project permits. Every
//     authorization failure gets the same 403, before the extension is looked up, so callers
//     learn nothing about Applications or extensions they may not use;
//   - backends come from the admin configuration only (http/https URLs, no credentials in the
//     URL); the request path never changes the backend host; redirects are not followed;
//   - only allow-listed request headers are forwarded (never Authorization, Cookie or
//     client-supplied Patrol-* identity headers); the server adds the caller and destination
//     headers, then the configured headers (Secret values resolved in settings, never logged);
//     hop-by-hop headers are dropped in both directions and Set-Cookie is removed from
//     responses, so a backend cannot plant cookies on the Patrol origin;
//   - request and response bodies are capped, the backend connection, response headers and
//     the whole exchange have timeouts, WebSocket upgrades are refused unless enabled;
//   - every call is audited (extensions, invoke, <name>) and counted per extension.
package extensions

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Defaults of Config.
const (
	DefaultMaxRequestBytes       int64 = 16 << 20
	DefaultMaxResponseBytes      int64 = 64 << 20
	DefaultRequestTimeout              = 2 * time.Minute
	DefaultResponseHeaderTimeout       = 30 * time.Second
)

// Config is the static configuration of the proxy.
type Config struct {
	// Enabled is server.enable.proxy.extension; when false every request is 404.
	Enabled bool
	// Namespace is the control-plane namespace (AppProjects and Clusters live there).
	Namespace string
	// ApplicationNamespaces are the other namespaces Applications may live in
	// (application.namespaces: names, globs or "/regexp/").
	ApplicationNamespaces []string
	// AllowedRequestHeaders extends the forwarded request headers (DefaultAllowedHeaders).
	// Credential and identity headers are never forwarded whatever this says.
	AllowedRequestHeaders []string
	// EnableWebSockets forwards WebSocket upgrades (off by default).
	EnableWebSockets bool
	// MaxRequestBytes and MaxResponseBytes cap the bodies (defaults 16 MiB, 64 MiB).
	MaxRequestBytes  int64
	MaxResponseBytes int64
	// RequestTimeout bounds a whole exchange (not WebSocket sessions); default 2 minutes.
	RequestTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for the backend's response headers (30 s).
	ResponseHeaderTimeout time.Duration
}

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// Deps are the collaborators of the proxy.
type Deps struct {
	Config   Config
	Settings SettingsSource
	// Reader reads Applications, AppProjects and Clusters (may be a cache).
	Reader client.Reader
	// Registry receives the per-extension metrics (nil: not registered).
	Registry prometheus.Registerer
	Logger   *slog.Logger
}

// Service serves the proxy routes; it implements apiserver.Service.
type Service struct {
	d       Deps
	allowed map[string]bool
	metrics *metrics

	mu  sync.Mutex
	reg *registry
}

// New returns the service.
func New(d Deps) *Service {
	c := &d.Config
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d, allowed: allowedHeaders(c.AllowedRequestHeaders), metrics: newMetrics(d.Registry, d.Logger)}
}

const tag = "extensions"

// Register adds the proxy routes (one per method) to t.
func (s *Service) Register(t *rest.Table) {
	acc := rest.RBACInHandler(rbac.ResourceExtensions, rbac.ActionInvoke, "{extension}")
	acc.Note = "plus applications, get on the Application named by Patrol-Application-Name and Patrol-Project-Name; every denial is the same 403"
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		// The body is forwarded as is (any media type), so it is documented as an
		// optional opaque body; GET requests are documented without one.
		bodyType, bodyDoc := "", ""
		if m != http.MethodGet {
			bodyType = "*/*"
			bodyDoc = "Opaque body forwarded unchanged to the backend (any media type; Content-Type is forwarded). Bodies over the proxy's request limit are refused with 413."
		}
		t.Add(rest.Route{
			Method: m, Path: apiclient.PathExtension,
			OperationID: "invokeExtension" + m[:1] + strings.ToLower(m[1:]),
			Summary:     "Proxy extension call (" + m + ")",
			Tags:        []string{tag},
			Access:      acc,
			Description: "Forwards the request (path after the extension name, query, allow-listed headers, body) to the extension backend selected by the Application's destination cluster. " +
				"Requires the headers `Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>`. Disabled unless server.enable.proxy.extension is true.",
			Request:             reflect.TypeFor[apiclient.ExtensionRequest](),
			RequestContentType:  bodyType,
			RequestDescription:  bodyDoc,
			RequestBodyOptional: true,
			ResponseContentType: "*/*",
			Audit:               rest.AuditAlways,
			Handler:             http.HandlerFunc(s.serve),
			Responses: []rest.ResponseDoc{
				{Status: http.StatusOK, Description: "The backend's response (any status and media type).", ContentType: "*/*"},
				{Status: http.StatusBadGateway, Description: "The backend could not be reached or answered invalidly."},
				{Status: http.StatusGatewayTimeout, Description: "The backend did not answer in time."},
			},
		})
	}
}

var errNotConfigured = errors.New("not configured")

// extensionNameRE is the name rule of extension.config (upstream).
var extensionNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// target is the Application a request acts for.
type target struct {
	namespace, name, project string
}

func badHeaders(format string, args ...any) error {
	return rest.InvalidArgument("Invalid headers: "+format, args...)
}

// parseTarget validates the Patrol-Application-Name and Patrol-Project-Name headers.
func parseTargetHeaders(r *http.Request) (*target, error) {
	appHeader := r.Header.Get(apiclient.HeaderExtensionApplication)
	if appHeader == "" {
		return nil, badHeaders("header %q must be provided", apiclient.HeaderExtensionApplication)
	}
	parts := strings.Split(appHeader, ":")
	if len(parts) != 2 {
		return nil, badHeaders("invalid value for %q header: expected format: <namespace>:<app-name>", apiclient.HeaderExtensionApplication)
	}
	t := &target{namespace: parts[0], name: parts[1], project: r.Header.Get(apiclient.HeaderExtensionProject)}
	if len(validation.IsDNS1123Label(t.namespace)) > 0 {
		return nil, badHeaders("invalid value for namespace")
	}
	if len(validation.IsDNS1123Subdomain(t.name)) > 0 {
		return nil, badHeaders("invalid value for application name")
	}
	if t.project == "" {
		return nil, badHeaders("header %q must be provided", apiclient.HeaderExtensionProject)
	}
	if len(validation.IsDNS1123Subdomain(t.project)) > 0 {
		return nil, badHeaders("invalid value for project name")
	}
	return t, nil
}

// unauthorized is the single answer to every authorization failure.
func unauthorized() error { return rest.PermissionDenied("Unauthorized extension request") }

func (s *Service) namespaceEnabled(ns string) bool {
	if ns == s.d.Config.Namespace {
		return true
	}
	return project.IsAppNamespacePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: s.d.Config.ApplicationNamespaces}}, ns, s.d.Config.Namespace)
}

// authorize enforces the RBAC checks and the Application/project/destination rules,
// returning the Application and its resolved destination cluster. reason (for the audit
// entry) explains a failure; the client only ever sees unauthorized().
func (s *Service) authorize(ctx context.Context, st *settings.Settings, t *target, ext string) (*v1alpha1.Application, *v1alpha1.Cluster, string, error) {
	appObj := rbac.AppObject(s.d.Config.Namespace, t.project, t.namespace, t.name)
	if err := rest.Authorize(ctx, rbac.ResourceApplications, rbac.ActionGet, appObj); err != nil {
		return nil, nil, "application authorization denied", unauthorized()
	}
	if err := rest.Authorize(ctx, rbac.ResourceExtensions, rbac.ActionInvoke, ext); err != nil {
		return nil, nil, "extension invocation denied", unauthorized()
	}
	if !s.namespaceEnabled(t.namespace) {
		return nil, nil, "application namespace not enabled", unauthorized()
	}
	if s.d.Reader == nil {
		return nil, nil, "", rest.Unavailable("applications are not available")
	}
	app := &v1alpha1.Application{}
	if err := s.d.Reader.Get(ctx, client.ObjectKey{Namespace: t.namespace, Name: t.name}, app); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, "application not found", unauthorized()
		}
		return nil, nil, "", err
	}
	if app.Spec.GetProject() != t.project {
		return nil, nil, "project mismatch", unauthorized()
	}
	proj := &v1alpha1.AppProject{}
	if err := s.d.Reader.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: t.project}, proj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, "project not found", unauthorized()
		}
		return nil, nil, "", err
	}
	if !project.IsAppNamespacePermitted(proj, app.Namespace, s.d.Config.Namespace) {
		return nil, nil, "application namespace not permitted by the project", unauthorized()
	}
	lookup := project.NewKubeLookup(s.d.Reader, s.d.Config.Namespace, st.InClusterEnabled())
	ok, cluster, err := project.IsAppDestinationPermitted(ctx, proj, app.Spec.Destination, lookup)
	if err != nil {
		return nil, nil, "destination cannot be resolved", unauthorized()
	}
	if !ok {
		return nil, nil, "destination not permitted by the project", unauthorized()
	}
	return app, cluster, "", nil
}

// registry returns the extensions of the current configuration, rebuilding them when it
// changed (no restart needed); idle connections of the previous registry are closed.
func (s *Service) registry(st *settings.Settings) *registry {
	exts := st.Extensions()
	fp := fingerprintOf(exts)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil && s.reg.fingerprint == fp {
		return s.reg
	}
	old := s.reg
	s.reg = buildRegistry(exts, s.d.Config.ResponseHeaderTimeout, s.d.Logger)
	old.close()
	return s.reg
}

// serve handles one proxy request.
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	audit := rest.AuditFrom(ctx)
	ext := strings.NewReplacer("\r", "", "\n", "").Replace(r.PathValue("extension"))
	audit.SetCheck(rbac.ResourceExtensions, rbac.ActionInvoke, ext)
	fail := func(reason string, err error) {
		if reason != "" {
			audit.SetReason(reason)
		}
		rest.WriteError(w, r, err)
	}
	if !s.d.Config.Enabled {
		fail("proxy extensions disabled", rest.NotFound("proxy extensions are not enabled"))
		return
	}
	if ext == "" {
		fail("", rest.InvalidArgument("Invalid URL: extension name must be provided"))
		return
	}
	if !extensionNameRE.MatchString(ext) {
		fail("", rest.InvalidArgument("Invalid URL: invalid extension name"))
		return
	}
	t, err := parseTargetHeaders(r)
	if err != nil {
		fail("", err)
		return
	}
	upgrade := isUpgrade(r)
	if upgrade && !s.d.Config.EnableWebSockets {
		fail("websocket refused", rest.InvalidArgument("WebSocket requests are not enabled for proxy extensions"))
		return
	}
	if s.d.Settings == nil || s.d.Settings.Current() == nil {
		fail("", rest.Unavailable("settings are not available").WithCause(errNotConfigured))
		return
	}
	st := s.d.Settings.Current()
	app, cluster, reason, err := s.authorize(ctx, st, t, ext)
	if err != nil {
		fail(reason, err)
		return
	}
	e, ok := s.registry(st).extensions[ext]
	if !ok {
		fail("extension not found", rest.NotFound("Extension not found"))
		return
	}
	be := e.find(app.Spec.Destination, cluster)
	if be == nil {
		fail("no backend for the destination", rest.InvalidArgument("invalid extension"))
		return
	}
	audit.SetMessage("%s %s via extension %s for application %s/%s", r.Method, apiclient.PathExtension, ext, app.Namespace, app.Name)
	s.forward(w, r, e, be, app, upgrade)
}

// isUpgrade reports whether r asks for a protocol upgrade (WebSocket, h2c).
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}
