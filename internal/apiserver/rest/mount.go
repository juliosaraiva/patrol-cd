package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Env holds the collaborators of the per-route chain.
type Env struct {
	Authenticator *Authenticator
	Enforcer      Enforcer
	Auditor       Auditor
	// LoginLimiter rate-limits routes marked RateLimited; nil disables limiting.
	LoginLimiter *RateLimiter
	CSRF         CSRFConfig
	Metrics      *Metrics
	Logger       *slog.Logger
	// DisableAuth skips authentication and authorization (upstream --disable-auth).
	DisableAuth bool
}

// Mount registers every route of t on mux, each wrapped in the route chain:
// authentication → CSRF → rate limiting → RBAC → audit → handler. Documentation-only
// routes (Route.DocumentationOnly) are skipped.
func (t *Table) Mount(mux *http.ServeMux, env *Env) error {
	if err := t.Err(); err != nil {
		return err
	}
	if env == nil {
		return errors.New("mount: nil env")
	}
	var errs []error
	for _, r := range t.Routes() {
		rt := r
		if rt.DocumentationOnly {
			continue
		}
		if err := safeHandle(mux, rt.Pattern(), env.wrap(&rt)); err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", rt.Pattern(), err))
		}
	}
	return errors.Join(errs...)
}

// Handler returns a handler serving t: matched routes run their chain; unmatched
// requests get 404, or 405 with an Allow header when another method matches, as problem
// documents.
func (t *Table) Handler(env *Env) (http.Handler, error) {
	mux := http.NewServeMux()
	if err := t.Mount(mux, env); err != nil {
		return nil, err
	}
	return &APIHandler{Mux: mux}, nil
}

// APIHandler serves a ServeMux and renders its 404/405 responses as problems.
type APIHandler struct {
	Mux *http.ServeMux
}

// Matches reports whether a route of the mux matches r (any method).
func (h *APIHandler) Matches(r *http.Request) bool {
	_, pattern := h.Mux.Handler(r)
	return pattern != ""
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := h.Mux.Handler(r); pattern != "" {
		h.Mux.ServeHTTP(w, r)
		return
	}
	NotMatched(h.Mux, w, r)
}

// NotMatched writes the problem document for a request no route of mux matched: 405 with
// the allowed methods when the path matches another method, else 404.
func NotMatched(mux *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if m == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, p := mux.Handler(probe); p != "" && strings.HasPrefix(p, m+" ") {
			allow = append(allow, m)
		}
	}
	if len(allow) > 0 {
		if slices.Contains(allow, http.MethodGet) {
			allow = append(allow, http.MethodHead)
		}
		WriteError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: "invalid_argument",
			Detail: "method " + r.Method + " is not allowed on this path", Allow: allow})
		return
	}
	WriteError(w, r, NotFound("no route for %s %s", r.Method, truncatePath(r.URL.Path)))
}

// wrap builds the chain of one route.
func (env *Env) wrap(rt *Route) http.Handler {
	pattern := rt.Pattern()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		env.Metrics.inflightAdd(1)
		sw := wrapWriter(w)
		defer func() {
			env.Metrics.inflightAdd(-1)
			env.Metrics.observe(rt.OperationID, r.Method, sw.code(), time.Since(start))
		}()
		if ai := accessInfoFrom(r.Context()); ai != nil {
			ai.route.Store(&pattern)
		}

		st := &requestState{route: rt, env: env}
		r = r.WithContext(withState(r.Context(), st))
		ctx := r.Context()

		// Authentication.
		authn := env.Authenticator
		if authn == nil {
			authn = &Authenticator{DisableAuth: env.DisableAuth}
		}
		mode := rt.Access.Mode
		var (
			id       *Identity
			reverify func(context.Context) error
			err      error
		)
		if rt.SkipAuth {
			id = &Identity{}
		} else if id, reverify, err = authn.authenticate(sw, r, mode, !rt.NoRenew); err != nil {
			WriteError(sw, r, err)
			return
		}
		st.identity, st.reverify = id, reverify
		if ai := accessInfoFrom(ctx); ai != nil {
			actor := id.Actor()
			ai.actor.Store(&actor)
		}

		// CSRF: cookie-authenticated state-changing requests.
		if id.Source == TokenSourceCookie && !isSafeMethod(r.Method) && !env.DisableAuth {
			if err := env.CSRF.check(r); err != nil {
				WriteError(sw, r, err)
				return
			}
		}

		// Login rate limiting.
		if rt.RateLimited && env.LoginLimiter != nil {
			if ok, wait := env.LoginLimiter.Allow(r); !ok {
				env.Metrics.limited(rt.OperationID)
				WriteError(sw, r, &Error{Status: http.StatusTooManyRequests, Code: "resource_exhausted",
					Detail: "too many requests; retry later", RetryAfter: wait})
				return
			}
		}

		if rt.Audit == AuditAlways || (rt.Audit == AuditMutations && !isSafeMethod(r.Method)) {
			st.audit = &AuditRecord{}
		} else {
			st.audit = &AuditRecord{suppress: true}
		}

		// RBAC enforced by the middleware.
		if mode == AccessRBAC && !rt.Access.InHandler {
			c, err := rt.Access.Resolve(r)
			if err != nil {
				WriteError(sw, r, err)
				return
			}
			st.audit.SetCheck(c.Resource, c.Action, c.Object)
			if d := env.decide(id, c.Resource, c.Action, c.Object); !d.Allowed {
				st.audit.suppress = false
				WriteError(sw, r, env.deny(ctx, st, r, c, d))
				return
			}
		}

		rt.Handler.ServeHTTP(sw, r)

		if st.audit.forceEmit {
			st.audit.suppress = false
		}
		status := sw.code()
		result := AuditSucceeded
		reason := ""
		if status >= 400 {
			result = AuditFailed
			reason = http.StatusText(status)
		}
		emitAudit(ctx, st, r, result, status, reason)
	})
}
