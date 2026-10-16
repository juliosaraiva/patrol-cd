package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// AccessMode is the authentication requirement of a route.
type AccessMode int

// Access modes.
const (
	// AccessPublic routes need no credentials. A presented token is still verified (best
	// effort) so handlers can tailor the response to logged-in callers.
	AccessPublic AccessMode = iota + 1
	// AccessAuthenticated routes need a valid token (or the anonymous identity when
	// users.anonymous.enabled is true).
	AccessAuthenticated
	// AccessRBAC routes are authenticated and authorized with an RBAC check, enforced by
	// the middleware (Access.Resolve) or by the handler (Access.InHandler).
	AccessRBAC
)

func (m AccessMode) String() string {
	switch m {
	case AccessPublic:
		return "public"
	case AccessAuthenticated:
		return "authenticated"
	case AccessRBAC:
		return "rbac"
	}
	return "unset"
}

// Check is one RBAC question: may the caller perform Action on Resource for Object.
type Check struct {
	Resource, Action, Object string
}

// Access is a route's authentication and authorization rule.
type Access struct {
	Mode AccessMode
	// Resource, Action and Object document the RBAC check (Object in the contract's
	// notation, e.g. "<project>/<name>" or "{name}"). They appear in the OpenAPI document
	// as x-patrol-rbac.
	Resource, Action, Object string
	// Resolve computes the check enforced by the middleware from the request (path
	// values, query). Required for AccessRBAC routes unless InHandler is set.
	Resolve func(r *http.Request) (Check, error)
	// InHandler marks checks the handler performs with Authorize because they need state
	// the middleware does not have (the project of an Application, a self exemption).
	// The middleware then only requires authentication.
	InHandler bool
	// Note is free text for the documentation (e.g. "self allowed").
	Note string
}

// Public returns the AccessPublic rule.
func Public() Access { return Access{Mode: AccessPublic} }

// Authenticated returns the AccessAuthenticated rule.
func Authenticated() Access { return Access{Mode: AccessAuthenticated} }

// RBAC returns a middleware-enforced check whose object is computed by object.
// objectDoc documents the object in the contract's notation.
func RBAC(resource, action, objectDoc string, object func(r *http.Request) (string, error)) Access {
	return Access{
		Mode: AccessRBAC, Resource: resource, Action: action, Object: objectDoc,
		Resolve: func(r *http.Request) (Check, error) {
			obj := ""
			if object != nil {
				var err error
				if obj, err = object(r); err != nil {
					return Check{}, err
				}
			}
			return Check{Resource: resource, Action: action, Object: obj}, nil
		},
	}
}

// RBACPathValue returns a middleware-enforced check whose object is the path value
// param (documented as "{param}").
func RBACPathValue(resource, action, param string) Access {
	return RBAC(resource, action, "{"+param+"}", func(r *http.Request) (string, error) {
		return r.PathValue(param), nil
	})
}

// RBACInHandler documents a check the handler performs with Authorize.
func RBACInHandler(resource, action, objectDoc string) Access {
	return Access{Mode: AccessRBAC, Resource: resource, Action: action, Object: objectDoc, InHandler: true}
}

// AuditPolicy selects which requests of a route are audited.
type AuditPolicy int

// Audit policies.
const (
	// AuditMutations audits non-GET/HEAD/OPTIONS requests (the default).
	AuditMutations AuditPolicy = iota
	// AuditAlways audits every request.
	AuditAlways
	// AuditNever disables request auditing (authorization denials are still audited).
	AuditNever
)

// ResponseDoc documents an additional response of a route.
type ResponseDoc struct {
	Status      int
	Description string
	// ContentType defaults to application/json when Type is set.
	ContentType string
	Type        reflect.Type
	// Headers names response headers (e.g. "Location").
	Headers []string
}

// Route is one API operation.
type Route struct {
	// Method is an HTTP method, or "" for any method (raw routes only).
	Method string
	// Path is a Go 1.22 ServeMux path pattern, e.g. "/api/v1/account/{name}"; a final
	// "{name...}" wildcard matches the rest of the path.
	Path        string
	OperationID string
	Summary     string
	Description string
	Tags        []string
	Access      Access

	// Request and Response are the Go types of the request (path, query and body
	// bindings) and of the JSON response; set by Register. Raw routes may set them for
	// the documentation.
	Request  reflect.Type
	Response reflect.Type
	// Stream marks Server-Sent Event routes; Response is then the event data type.
	Stream bool
	// SuccessStatus is the status of successful responses (default 200).
	SuccessStatus int
	// MaxBodyBytes caps the request body (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// AllowUnknownFields accepts unknown JSON body fields. Bodies that are Kubernetes
	// objects always accept them (fields of newer API versions); every other body
	// rejects them.
	AllowUnknownFields bool
	// RequestContentType documents a raw request body media type (default
	// application/json for typed routes). A route whose Request type has no body fields
	// and that reads the body itself (an upload, a proxy) sets it to document that body;
	// a media range ("*/*") documents an opaque body of any type.
	RequestContentType string
	// RequestDescription describes the raw request body documented by
	// RequestContentType.
	RequestDescription string
	// RequestBodyOptional documents the raw request body as optional (e.g. a proxy that
	// forwards whatever body it receives).
	RequestBodyOptional bool
	// ResponseContentType documents a raw response media type.
	ResponseContentType string
	// RateLimited applies the login rate limiter (per client IP).
	RateLimited bool
	Deprecated  bool
	// Hidden omits the route from the OpenAPI document (aliases, proxies).
	Hidden bool
	// DocumentationOnly marks a route that is served outside the route table (e.g. the
	// SCM webhook receiver mounted by the server wiring) but belongs in the OpenAPI
	// document. Mount never registers it, so it cannot change routing; its Handler may be
	// nil.
	DocumentationOnly bool
	// Extensions are additional OpenAPI operation fields; every key must start with
	// "x-" and values must encode as JSON (maps are written with sorted keys).
	Extensions map[string]any
	// SkipAuth makes the chain ignore credentials entirely (no token verification, no
	// CSRF check, no cookie renewal) for public routes that proxy to services which do not
	// act on the caller's Patrol identity (the bundled Dex). Requires AccessPublic.
	SkipAuth bool
	// NoRenew disables login session renewal on this route. Routes that end or
	// invalidate the caller's session (logout, password change) set it so their response
	// never carries a replacement token (RenewTokenHeader or a refreshed cookie) that
	// would outlive the action.
	NoRenew   bool
	Audit     AuditPolicy
	Responses []ResponseDoc

	// Handler serves the route; set by Register, or by the caller for raw routes.
	Handler http.Handler
}

// Pattern returns the ServeMux pattern of r.
func (r *Route) Pattern() string {
	if r.Method == "" {
		return r.Path
	}
	return r.Method + " " + r.Path
}

// PathParams returns the wildcard names of r.Path in order.
func (r *Route) PathParams() []string {
	var out []string
	for _, m := range wildcardRE.FindAllStringSubmatch(r.Path, -1) {
		out = append(out, m[1])
	}
	return out
}

func (r *Route) successStatus() int {
	if r.SuccessStatus == 0 {
		return http.StatusOK
	}
	return r.SuccessStatus
}

func (r *Route) maxBody() int64 {
	if r.MaxBodyBytes <= 0 {
		return DefaultMaxBodyBytes
	}
	return r.MaxBodyBytes
}

// DefaultMaxBodyBytes is the default request body cap of a route (4 MiB).
const DefaultMaxBodyBytes int64 = 4 << 20

var (
	wildcardRE    = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(?:\.\.\.)?\}`)
	operationIDRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	validMethods  = []string{"", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
)

// Table is a set of routes. It is safe for concurrent registration.
type Table struct {
	mu     sync.Mutex
	routes []*Route
	errs   []error
}

// NewTable returns an empty table.
func NewTable() *Table { return &Table{} }

// Err returns the registration errors (nil when every route is valid).
func (t *Table) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return errors.Join(t.errs...)
}

// Routes returns copies of the registered routes sorted by path, then method.
func (t *Table) Routes() []Route {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Route, len(t.routes))
	for i, r := range t.routes {
		out[i] = *r
	}
	slices.SortFunc(out, func(a, b Route) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Method, b.Method)
	})
	return out
}

func (t *Table) fail(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.errs = append(t.errs, err)
}

// Add registers a raw route whose Handler serves the request directly.
func (t *Table) Add(r Route) {
	if r.Handler == nil && !r.DocumentationOnly {
		t.fail(fmt.Errorf("route %s %s: no handler", r.Method, r.Path))
		return
	}
	t.add(r)
}

func (t *Table) add(r Route) {
	if err := validateRoute(&r); err != nil {
		t.fail(err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, o := range t.routes {
		if o.OperationID == r.OperationID {
			t.errs = append(t.errs, fmt.Errorf("route %s: duplicate operation ID %q", r.Pattern(), r.OperationID))
			return
		}
		if o.Pattern() == r.Pattern() {
			t.errs = append(t.errs, fmt.Errorf("route %s: registered twice", r.Pattern()))
			return
		}
	}
	rr := r
	t.routes = append(t.routes, &rr)
}

func validateRoute(r *Route) error {
	where := fmt.Sprintf("route %s %s", r.Method, r.Path)
	if !slices.Contains(validMethods, r.Method) {
		return fmt.Errorf("%s: unsupported method", where)
	}
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("%s: path must start with /", where)
	}
	if !operationIDRE.MatchString(r.OperationID) {
		return fmt.Errorf("%s: operation ID %q must be alphanumeric camelCase", where, r.OperationID)
	}
	switch r.Access.Mode {
	case AccessPublic, AccessAuthenticated:
	case AccessRBAC:
		if r.Access.Resource == "" || r.Access.Action == "" {
			return fmt.Errorf("%s: RBAC access needs a resource and an action", where)
		}
		if r.Access.Resolve == nil && !r.Access.InHandler {
			return fmt.Errorf("%s: RBAC access needs Resolve or InHandler", where)
		}
	default:
		return fmt.Errorf("%s: access mode not set", where)
	}
	if r.SkipAuth && r.Access.Mode != AccessPublic {
		return fmt.Errorf("%s: SkipAuth requires public access", where)
	}
	if r.DocumentationOnly && r.Hidden {
		return fmt.Errorf("%s: a documentation-only route cannot be hidden", where)
	}
	for k := range r.Extensions {
		if !strings.HasPrefix(k, "x-") {
			return fmt.Errorf("%s: OpenAPI extension %q must start with x-", where, k)
		}
	}
	if err := checkPattern(r.Pattern()); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if r.Request != nil {
		meta, err := metaFor(r.Request)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		params := r.PathParams()
		for _, p := range params {
			if !slices.ContainsFunc(meta.path, func(f paramField) bool { return f.name == p }) {
				return fmt.Errorf("%s: path parameter %q has no `path` field in %s", where, p, r.Request)
			}
		}
		for _, f := range meta.path {
			if !slices.Contains(params, f.name) {
				return fmt.Errorf("%s: field %s binds path parameter %q, which the pattern lacks", where, f.goName, f.name)
			}
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodDelete) && meta.hasBody {
			return fmt.Errorf("%s: %s requests cannot carry a JSON body (%s has body fields)", where, r.Method, r.Request)
		}
	}
	return nil
}

// checkPattern validates a ServeMux pattern by registering it on a scratch mux;
// ServeMux reports invalid patterns by panicking, which is converted to an error.
func checkPattern(pattern string) (err error) {
	return safeHandle(http.NewServeMux(), pattern, http.NotFoundHandler())
}

func safeHandle(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("invalid pattern %q: %v", pattern, v)
		}
	}()
	mux.Handle(pattern, h)
	return nil
}

// HandlerFunc is a typed JSON handler.
type HandlerFunc[Req, Resp any] func(ctx context.Context, req *Req) (*Resp, error)

// Register adds a typed JSON route: the request is decoded into Req (path, query and
// JSON body bindings, see package apiclient), h is called, and its result is encoded as
// JSON with r.SuccessStatus, or its error as a problem document.
func Register[Req, Resp any](t *Table, r Route, h HandlerFunc[Req, Resp]) {
	r.Request = reflect.TypeFor[Req]()
	r.Response = reflect.TypeFor[Resp]()
	meta, err := metaFor(r.Request)
	if err != nil {
		t.fail(fmt.Errorf("route %s %s: %w", r.Method, r.Path, err))
		return
	}
	if r.Method == "" {
		t.fail(fmt.Errorf("route %s: typed routes need a method", r.Path))
		return
	}
	opts := decodeOptions{
		maxBody:      r.maxBody(),
		allowUnknown: r.AllowUnknownFields || meta.k8sObject,
	}
	status := r.successStatus()
	r.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		in := new(Req)
		if err := decodeRequest(w, req, meta, reflect.ValueOf(in).Elem(), opts); err != nil {
			WriteError(w, req, err)
			return
		}
		ctx := withHTTP(req.Context(), w, req)
		out, err := h(ctx, in)
		if err != nil {
			WriteError(w, req, err)
			return
		}
		WriteJSON(w, status, out)
	})
	t.add(r)
}

// StreamHandlerFunc is a typed Server-Sent Events producer; it sends events to sink
// until ctx is done or it returns.
type StreamHandlerFunc[Req, Event any] func(ctx context.Context, req *Req, sink *Sink[Event]) error

// RegisterStream adds a typed Server-Sent Events route. The request is decoded into Req,
// then ServeSSE runs h with opts.
func RegisterStream[Req, Event any](t *Table, r Route, opts StreamOptions[Event], h StreamHandlerFunc[Req, Event]) {
	r.Request = reflect.TypeFor[Req]()
	r.Response = reflect.TypeFor[Event]()
	r.Stream = true
	meta, err := metaFor(r.Request)
	if err != nil {
		t.fail(fmt.Errorf("route %s %s: %w", r.Method, r.Path, err))
		return
	}
	if r.Method != http.MethodGet {
		t.fail(fmt.Errorf("route %s %s: streams must use GET", r.Method, r.Path))
		return
	}
	dopts := decodeOptions{maxBody: r.maxBody()}
	r.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		in := new(Req)
		if err := decodeRequest(w, req, meta, reflect.ValueOf(in).Elem(), dopts); err != nil {
			WriteError(w, req, err)
			return
		}
		ServeSSE(w, req, opts, func(ctx context.Context, sink *Sink[Event]) error {
			return h(ctx, in, sink)
		})
	})
	t.add(r)
}
