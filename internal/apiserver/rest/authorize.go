package rest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// Enforcer evaluates RBAC requests; *rbac.Enforcer implements it.
type Enforcer interface {
	Enforce(s rbac.Subjects, resource, action, object string) rbac.Decision
}

// Can reports whether the caller may perform action on resource/object, without
// auditing. List handlers use it to filter items.
func Can(ctx context.Context, resource, action, object string) bool {
	st := stateFrom(ctx)
	if st == nil || st.env == nil {
		return false
	}
	return st.env.decide(st.identity, resource, action, object).Allowed
}

// Authorize enforces an RBAC check in a handler. On denial it records an audit entry
// and returns a 403 *Error with the upstream message shape
// "permission denied: <resource>, <action>, <object>[, sub: <subject>][, iat: <time>]".
func Authorize(ctx context.Context, resource, action, object string) error {
	st := stateFrom(ctx)
	if st == nil || st.env == nil {
		return PermissionDenied("permission denied")
	}
	d := st.env.decide(st.identity, resource, action, object)
	if d.Allowed {
		return nil
	}
	return st.env.deny(ctx, st, HTTPRequest(ctx), Check{resource, action, object}, d)
}

func (env *Env) decide(id *Identity, resource, action, object string) rbac.Decision {
	if id == nil {
		return rbac.Decision{Reason: "no identity"}
	}
	if env.DisableAuth || id.AuthDisabled {
		return rbac.Decision{Allowed: true, Reason: "authentication disabled"}
	}
	if env.Enforcer == nil {
		return rbac.Decision{Reason: "no RBAC enforcer configured"}
	}
	if id.Principal == nil && !id.Anonymous {
		return rbac.Decision{Reason: "not authenticated"}
	}
	return env.Enforcer.Enforce(id.Subjects, resource, action, object)
}

// deny audits a denial and returns the 403 error.
func (env *Env) deny(ctx context.Context, st *requestState, r *http.Request, c Check, d rbac.Decision) error {
	msg := fmt.Sprintf("permission denied: %s, %s, %s", c.Resource, c.Action, c.Object)
	if id := st.identity; id != nil && id.Principal != nil {
		msg += ", sub: " + id.Principal.Subject
		if !id.Principal.IssuedAt.IsZero() {
			msg += ", iat: " + id.Principal.IssuedAt.UTC().Format(time.RFC3339)
		}
	}
	if st.audit == nil {
		st.audit = &AuditRecord{}
	}
	st.audit.mu.Lock()
	st.audit.suppress = false // denials are always audited
	st.audit.mu.Unlock()
	st.audit.SetCheck(c.Resource, c.Action, c.Object)
	if r == nil {
		r = &http.Request{Method: st.route.Method}
	}
	emitAudit(ctx, st, r, AuditDenied, http.StatusForbidden, d.Reason)
	env.Metrics.denied(st.route.OperationID)
	return PermissionDenied("%s", msg)
}
