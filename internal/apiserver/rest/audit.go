package rest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// AuditResult is the outcome recorded in an audit entry.
type AuditResult string

// Audit results.
const (
	AuditSucceeded AuditResult = "succeeded"
	AuditFailed    AuditResult = "failed"
	AuditDenied    AuditResult = "denied"
)

// Kubernetes Event reasons of audit events.
const (
	ReasonResourceCreated = "ResourceCreated"
	ReasonResourceUpdated = "ResourceUpdated"
	ReasonResourceDeleted = "ResourceDeleted"
	ReasonResourceAction  = "ResourceActionRan"
	ReasonAccessDenied    = "AccessDenied"
)

// ObjectRef identifies the Kubernetes object an audited request acted on.
type ObjectRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        types.UID
}

// AuditEntry is one audit record: who did what to which object, and the outcome. It
// never contains request bodies, tokens or secrets.
type AuditEntry struct {
	Time      time.Time
	RequestID string
	// Actor is the verified principal subject ("anonymous" without one). For a login,
	// the account that logged in on success.
	Actor     string
	ActorKind string
	// Operation is the route's operation ID.
	Operation string
	Method    string
	// Route is the route pattern (raw paths are not recorded; they may embed URLs).
	Route string
	// Resource, Action and Object are the RBAC triple of the request, when known.
	Resource, Action, Object string
	Result                   AuditResult
	Status                   int
	// Reason explains a denial or failure (RBAC decision reason or error code).
	Reason string
	// Target is the object acted on; Application and AppProject targets also get a
	// Kubernetes Event.
	Target *ObjectRef
	// Message optionally overrides the Event message (no secrets).
	Message string
	// EventReason optionally overrides the Event reason.
	EventReason string
}

// Auditor records audit entries.
type Auditor interface {
	Audit(ctx context.Context, e AuditEntry)
}

// AuditorFunc adapts a function to Auditor.
type AuditorFunc func(ctx context.Context, e AuditEntry)

// Audit calls f.
func (f AuditorFunc) Audit(ctx context.Context, e AuditEntry) { f(ctx, e) }

// EventRecorder records Kubernetes Events; client-go's record.EventRecorder implements
// it.
type EventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any)
}

// AuditRecord is the mutable audit state of a request. Handlers annotate it (target
// object, RBAC triple, message) with AuditFrom.
type AuditRecord struct {
	mu        sync.Mutex
	actor     string
	resource  string
	action    string
	object    string
	target    *ObjectRef
	message   string
	reason    string
	evReason  string
	emitted   bool
	suppress  bool
	forceEmit bool
}

// AuditFrom returns the request's audit record, or nil (all methods are nil-safe).
func AuditFrom(ctx context.Context) *AuditRecord {
	if st := stateFrom(ctx); st != nil {
		return st.audit
	}
	return nil
}

// SetActor overrides the actor (e.g. the account that just logged in).
func (a *AuditRecord) SetActor(actor string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.actor = actor
	a.mu.Unlock()
}

// SetCheck records the RBAC triple of the request.
func (a *AuditRecord) SetCheck(resource, action, object string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.resource, a.action, a.object = resource, action, object
	a.mu.Unlock()
}

// SetTarget records the object acted on. Application and AppProject targets also get a
// Kubernetes Event.
func (a *AuditRecord) SetTarget(ref ObjectRef) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.target = &ref
	a.mu.Unlock()
}

// SetMessage sets the human-readable audit message (no secrets).
func (a *AuditRecord) SetMessage(format string, args ...any) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.message = fmt.Sprintf(format, args...)
	a.mu.Unlock()
}

// SetReason records why a request failed (e.g. "login failed").
func (a *AuditRecord) SetReason(reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.reason = reason
	a.mu.Unlock()
}

// SetEventReason overrides the Kubernetes Event reason.
func (a *AuditRecord) SetEventReason(reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.evReason = reason
	a.mu.Unlock()
}

// Record forces an audit entry for this request even if the route's policy would not
// emit one (e.g. a GET with side effects such as a refresh).
func (a *AuditRecord) Record() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.forceEmit = true
	a.mu.Unlock()
}

// LogAuditor writes audit entries as structured log lines (message "audit") and, when a
// recorder is set, Kubernetes Events for Application and AppProject targets.
type LogAuditor struct {
	Logger   *slog.Logger
	Recorder EventRecorder
	Metrics  *Metrics
}

// Audit implements Auditor.
func (l *LogAuditor) Audit(ctx context.Context, e AuditEntry) {
	lg := l.Logger
	if lg == nil {
		lg = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("requestId", e.RequestID),
		slog.String("actor", e.Actor),
		slog.String("actorKind", e.ActorKind),
		slog.String("operation", e.Operation),
		slog.String("method", e.Method),
		slog.String("route", e.Route),
		slog.String("result", string(e.Result)),
		slog.Int("status", e.Status),
	}
	if e.Resource != "" {
		attrs = append(attrs, slog.String("resource", e.Resource), slog.String("action", e.Action), slog.String("object", e.Object))
	}
	if e.Reason != "" {
		attrs = append(attrs, slog.String("reason", e.Reason))
	}
	if e.Target != nil {
		attrs = append(attrs, slog.String("targetKind", e.Target.Kind), slog.String("targetNamespace", e.Target.Namespace), slog.String("targetName", e.Target.Name))
	}
	level := slog.LevelInfo
	if e.Result == AuditDenied {
		level = slog.LevelWarn
	}
	lg.LogAttrs(ctx, level, "audit", attrs...)
	l.Metrics.audited(e.Result)
	if l.Recorder != nil && e.Target != nil && isEventTarget(e.Target.Kind) && e.Result != AuditFailed {
		l.event(e)
	}
}

func isEventTarget(kind string) bool { return kind == "Application" || kind == "AppProject" }

func (l *LogAuditor) event(e AuditEntry) {
	ref := &corev1.ObjectReference{
		APIVersion: e.Target.APIVersion,
		Kind:       e.Target.Kind,
		Namespace:  e.Target.Namespace,
		Name:       e.Target.Name,
		UID:        e.Target.UID,
	}
	actor := e.Actor
	if actor == "" {
		actor = "anonymous"
	}
	eventType, reason := corev1.EventTypeNormal, e.EventReason
	if e.Result == AuditDenied {
		eventType = corev1.EventTypeWarning
		if reason == "" {
			reason = ReasonAccessDenied
		}
	}
	if reason == "" {
		switch e.Method {
		case http.MethodPost:
			reason = ReasonResourceCreated
		case http.MethodDelete:
			reason = ReasonResourceDeleted
		default:
			reason = ReasonResourceUpdated
		}
	}
	msg := e.Message
	if msg == "" {
		verb := map[string]string{ReasonResourceCreated: "created", ReasonResourceDeleted: "deleted", ReasonAccessDenied: "was denied"}[reason]
		if verb == "" {
			verb = "updated"
		}
		msg = fmt.Sprintf("%s %s %s %s", actor, verb, strings.ToLower(e.Target.Kind), e.Target.Name)
		if e.Result == AuditDenied && e.Action != "" {
			msg = fmt.Sprintf("%s was denied %s on %s %s", actor, e.Action, strings.ToLower(e.Target.Kind), e.Target.Name)
		}
	}
	l.Recorder.Eventf(ref, eventType, reason, "%s", msg)
}

// emitAudit builds the entry of the current request and sends it to the auditor once.
func emitAudit(ctx context.Context, st *requestState, r *http.Request, result AuditResult, status int, reason string) {
	if st == nil || st.env == nil || st.env.Auditor == nil || st.audit == nil {
		return
	}
	a := st.audit
	a.mu.Lock()
	if a.emitted || a.suppress {
		a.mu.Unlock()
		return
	}
	a.emitted = true
	e := AuditEntry{
		Time:        time.Now().UTC(),
		RequestID:   RequestID(ctx),
		Actor:       st.identity.Actor(),
		Operation:   st.route.OperationID,
		Method:      r.Method,
		Route:       st.route.Pattern(),
		Resource:    a.resource,
		Action:      a.action,
		Object:      a.object,
		Result:      result,
		Status:      status,
		Reason:      reason,
		Target:      a.target,
		Message:     a.message,
		EventReason: a.evReason,
	}
	if a.actor != "" {
		e.Actor = a.actor
	}
	if e.Reason == "" {
		e.Reason = a.reason
	}
	if st.identity != nil && st.identity.Principal != nil {
		e.ActorKind = string(st.identity.Principal.Kind)
	}
	a.mu.Unlock()
	st.env.Auditor.Audit(ctx, e)
}
