package rest

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// TokenSource tells where the request's token came from.
type TokenSource string

// Token sources.
const (
	TokenSourceNone   TokenSource = ""
	TokenSourceHeader TokenSource = "header"
	TokenSourceCookie TokenSource = "cookie"
)

// Identity is the authenticated caller of a request.
type Identity struct {
	// Principal is the verified token principal; nil for anonymous and unauthenticated
	// callers.
	Principal *auth.Principal
	// Anonymous is true when the request proceeds with the anonymous identity
	// (users.anonymous.enabled) because no valid token was presented.
	Anonymous bool
	// AuthDisabled is true when the server runs with authentication disabled.
	AuthDisabled bool
	// Source is where the token was found.
	Source TokenSource
	// AuthError is the verification error of a presented token on public or anonymous
	// routes (nil otherwise). It is never returned to the client verbatim.
	AuthError error
	// Subjects is the RBAC identity.
	Subjects rbac.Subjects

	token string // raw token; never logged
}

// LoggedIn reports whether the caller presented a valid token (upstream LoggedIn).
func (id *Identity) LoggedIn() bool { return id != nil && id.Principal != nil }

// Token returns the raw token presented by the caller ("" when none). Never log it.
func (id *Identity) Token() string {
	if id == nil {
		return ""
	}
	return id.token
}

// Actor is the audit name of the caller: the principal subject, "anonymous" or "".
func (id *Identity) Actor() string {
	switch {
	case id == nil:
		return ""
	case id.Principal != nil:
		return id.Principal.Subject
	case id.Anonymous || id.AuthDisabled:
		return "anonymous"
	}
	return ""
}

// IsLocal reports whether the caller authenticated as the local account name with a
// Patrol-issued session or API token (upstream: issuer argocd and same identifier).
func (id *Identity) IsLocal(name string) bool {
	if id == nil || id.Principal == nil {
		return false
	}
	k := id.Principal.Kind
	return (k == auth.KindLogin || k == auth.KindAPIKey) && id.Principal.Subject == name
}

// IssuedAt returns the token issue time (zero when unauthenticated).
func (id *Identity) IssuedAt() time.Time {
	if id == nil || id.Principal == nil {
		return time.Time{}
	}
	return id.Principal.IssuedAt
}

// Groups returns the RBAC group subjects of the caller.
func (id *Identity) Groups() []string {
	if id == nil {
		return nil
	}
	return slices.Clone(id.Subjects.Groups)
}

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyState
	keyHTTP
)

// requestState is the per-request state of a mounted route.
type requestState struct {
	route    *Route
	env      *Env
	identity *Identity
	audit    *AuditRecord
	// reverify re-checks the caller's credentials (streams).
	reverify func(context.Context) error
}

func withState(ctx context.Context, st *requestState) context.Context {
	return context.WithValue(ctx, keyState, st)
}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(keyState).(*requestState)
	return st
}

// IdentityFrom returns the caller's identity, or nil outside a mounted route.
func IdentityFrom(ctx context.Context) *Identity {
	if st := stateFrom(ctx); st != nil {
		return st.identity
	}
	return nil
}

// RouteFrom returns the matched route, or nil.
func RouteFrom(ctx context.Context) *Route {
	if st := stateFrom(ctx); st != nil {
		return st.route
	}
	return nil
}

// RequestID returns the request ID assigned by the RequestID middleware ("" when absent).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// WithRequestID returns ctx carrying id (for tests and internal callers).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

type httpPair struct {
	w http.ResponseWriter
	r *http.Request
}

func withHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	return context.WithValue(ctx, keyHTTP, &httpPair{w: w, r: r})
}

// HTTPRequest returns the *http.Request of a typed handler's context, for handlers that
// read cookies or headers. It returns nil outside a request.
func HTTPRequest(ctx context.Context) *http.Request {
	if p, _ := ctx.Value(keyHTTP).(*httpPair); p != nil {
		return p.r
	}
	return nil
}

// ResponseWriter returns the http.ResponseWriter of a typed handler's context, for
// handlers that set cookies or headers before the response body is written. Handlers
// must not write the body themselves.
func ResponseWriter(ctx context.Context) http.ResponseWriter {
	if p, _ := ctx.Value(keyHTTP).(*httpPair); p != nil {
		return p.w
	}
	return nil
}

// Reverify re-checks the caller's credentials (the token is still valid and not revoked,
// anonymous access is still enabled). Long-running streams call it periodically.
func Reverify(ctx context.Context) error {
	st := stateFrom(ctx)
	if st == nil || st.reverify == nil {
		return nil
	}
	return st.reverify(ctx)
}
