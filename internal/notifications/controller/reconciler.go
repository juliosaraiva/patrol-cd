package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

const (
	// retryBase is the first retry delay after failed deliveries; it doubles per consecutive
	// failing evaluation up to the resync period.
	retryBase = 5 * time.Second
	// persistTimeout bounds recording the delivery state, also during shutdown.
	persistTimeout = 30 * time.Second
	// maxPersistAttempts bounds optimistic-concurrency retries of the state patch.
	maxPersistAttempts = 8
)

// evalMetrics forwards trigger evaluations to Prometheus. Deliveries are counted by the
// controller from the results, after dropping failures another configuration handles.
type evalMetrics struct {
	m *notifications.PrometheusMetrics
}

func (e evalMetrics) IncDelivery(string, string, bool) {}
func (e evalMetrics) IncTriggerEvaluation(trigger string, triggered bool) {
	e.m.IncTriggerEvaluation(trigger, triggered)
}

type reconciler struct {
	cfg      *completed
	client   client.Client // cached reads, writes
	live     client.Reader // uncached reads
	recorder events.EventRecorder
	engines  *engineFactory
	ops      OperationGetter
	metrics  *notifications.PrometheusMetrics
	memo     *memo
	log      *slog.Logger
	prefix   string
}

// taggedResult is a delivery result with the configuration that produced it.
type taggedResult struct {
	notifications.DeliveryResult
	engine *engineEntry
}

// Reconcile evaluates one Application.
func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	key := req.NamespacedName
	log := r.log.With("application", key.Name, "namespace", key.Namespace)
	if !r.cfg.scope.matches(key.Namespace) {
		log.Debug("skipping application: not in one of the application namespaces, nor the controller namespace")
		return reconcile.Result{}, nil
	}
	app := newApplication()
	if err := r.client.Get(ctx, key, app); err != nil {
		if apierrors.IsNotFound(err) {
			r.memo.forget(key)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting application: %w", err)
	}
	app, err := r.current(ctx, key, app)
	if err != nil {
		return reconcile.Result{}, err
	}
	if app == nil {
		r.memo.forget(key)
		return reconcile.Result{}, nil
	}
	failed, err := r.process(ctx, log, app)
	if err != nil {
		return reconcile.Result{}, err
	}
	streak := r.memo.recordOutcome(key, failed)
	return reconcile.Result{RequeueAfter: r.requeueAfter(streak)}, nil
}

// current returns the Application to evaluate: the cached copy, or a live read while the cache
// may not show our last state write yet.
func (r *reconciler) current(ctx context.Context, key types.NamespacedName, cached *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	rv := r.memo.pendingWrite(key)
	if rv == "" {
		return cached, nil
	}
	if cached.GetResourceVersion() == rv {
		r.memo.confirmWrite(key)
		return cached, nil
	}
	live := newApplication()
	if err := r.live.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading application: %w", err)
	}
	if live.GetResourceVersion() == cached.GetResourceVersion() {
		r.memo.confirmWrite(key)
	}
	return live, nil
}

func (r *reconciler) requeueAfter(streak int) time.Duration {
	resync := r.cfg.ResyncPeriod
	if streak > 0 {
		d := retryBase
		for i := 1; i < streak && d < resync; i++ {
			d *= 2
		}
		return min(d, resync)
	}
	// Spread periodic evaluations so all applications do not fire together.
	return resync + rand.N(resync/10+1) //nolint:gosec // jitter, not security sensitive
}

// process evaluates the Application against every applicable configuration, reports results
// and records the delivery state. failed reports deliveries worth retrying sooner.
func (r *reconciler) process(ctx context.Context, log *slog.Logger, app *unstructured.Unstructured) (failed bool, err error) {
	key := client.ObjectKeyFromObject(app)
	engines, err := r.engines.forApp(ctx, app.GetNamespace())
	if err != nil {
		return false, err
	}
	subscribed := false
	view := subscriptionView(app)
	for _, e := range engines {
		dests, err := e.engine.Destinations(ctx, view)
		if err != nil {
			return false, fmt.Errorf("resolving notification subscriptions: %w", err)
		}
		if len(dests) > 0 {
			subscribed = true
			break
		}
	}
	if !subscribed {
		return false, nil
	}
	tmplApp, err := TemplateApplication(ctx, app.Object, r.ops)
	if err != nil {
		return false, err
	}
	// The freshness gate (NOTIF-010) looks at the Argo CD view, as upstream sees its object.
	if ok, reason := notifications.SyncStatusRefreshed(tmplApp); !ok {
		log.Debug("skipping application", "reason", reason)
		return false, nil
	}

	stored := notifications.DeliveryStateFromAnnotations(app.GetAnnotations(), r.prefix)
	state := stored
	if d, ok := r.memo.unpersistedDelta(key); ok {
		state = d.applyTo(stored)
	}
	var results []taggedResult
	var procErr error
	for _, e := range engines {
		next, res, err := e.engine.Process(ctx, tmplApp, state)
		state = next
		for _, dr := range res {
			results = append(results, taggedResult{DeliveryResult: dr, engine: e})
		}
		if err != nil {
			procErr = fmt.Errorf("processing notifications: %w", err)
			break
		}
	}
	failed = r.report(ctx, log, app, engines, results)

	delta := diffState(stored, state)
	if delta.empty() {
		r.memo.clearUnpersisted(key)
		return failed, procErr
	}
	// Notifications were sent: record them even when ctx ended (shutdown), so a restart does not
	// send them again.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := r.persist(pctx, app, delta); err != nil {
		r.memo.failedWrite(key, delta)
		return failed, errors.Join(procErr, fmt.Errorf("recording delivery state: %w", err))
	}
	return failed, procErr
}

// subscriptionView is the part of an Application subscriptions depend on (labels, annotations,
// project), so checking for subscribers does not deep-copy a large status.
func subscriptionView(app *unstructured.Unstructured) map[string]any {
	meta := map[string]any{"name": app.GetName(), "namespace": app.GetNamespace()}
	if l := app.GetLabels(); l != nil {
		labels := make(map[string]any, len(l))
		for k, v := range l {
			labels[k] = v
		}
		meta["labels"] = labels
	}
	if a := app.GetAnnotations(); a != nil {
		annotations := make(map[string]any, len(a))
		for k, v := range a {
			annotations[k] = v
		}
		meta["annotations"] = annotations
	}
	spec := map[string]any{}
	if project, ok := nestedString(app.Object, "spec", "project"); ok {
		spec["project"] = project
	}
	return map[string]any{"metadata": meta, "spec": spec}
}

// report counts, logs and records Events for the results. Failures of a configuration about a
// service or trigger it does not define are dropped when another configuration of the
// Application defines it (self-service: each configuration only handles its own names).
func (r *reconciler) report(ctx context.Context, log *slog.Logger, app *unstructured.Unstructured, engines []*engineEntry, results []taggedResult) (failed bool) {
	for _, tr := range results {
		attrs := []any{"trigger", tr.Trigger, "service", tr.Destination.Service, "recipient", tr.Destination.Recipient}
		if tr.ConditionKey != "" {
			attrs = append(attrs, "condition", tr.ConditionKey)
		}
		if tr.engine.tenant {
			attrs = append(attrs, "configNamespace", tr.engine.namespace)
		}
		switch tr.Status {
		case notifications.StatusSent:
			r.metrics.IncDelivery(tr.Trigger, tr.Destination.Service, true)
			log.Info("notification sent", attrs...)
		case notifications.StatusSkippedDuplicate:
			log.Debug("notification already sent", attrs...)
		case notifications.StatusFailed:
			if ctx.Err() != nil && errors.Is(tr.Err, ctx.Err()) {
				log.Debug("notification interrupted by shutdown", attrs...)
				continue
			}
			if suppressFailure(tr, engines) {
				log.Debug("ignoring failure handled by another notifications configuration", append(attrs, "error", tr.Err)...)
				continue
			}
			configErr := isConfigurationError(tr.Err)
			if !errors.Is(tr.Err, notifications.ErrTriggerNotFound) {
				r.metrics.IncDelivery(tr.Trigger, tr.Destination.Service, false)
			}
			if !configErr {
				failed = true
			}
			log.Warn("failed to deliver notification", append(attrs, "error", tr.Err)...)
			if r.recorder != nil {
				r.recorder.Eventf(app, nil, corev1.EventTypeWarning, ReasonNotificationDeliveryFailed, notifyAction(tr), "%s", truncateUTF8(failureNote(app.GetNamespace(), tr), maxEventNoteBytes))
			}
		}
	}
	return failed
}

// failureNote is the note of the delivery-failure Event, which is created in the
// Application's namespace (appNamespace). The full error (endpoint URL with its path, recipient,
// up to 512 bytes of the response body) is only written when the failing configuration lives
// in that namespace: the namespace's own configuration, or the control-plane configuration
// for an Application of the control-plane namespace. Otherwise (the control-plane
// configuration notifying about an Application elsewhere) readers of the namespace's Events
// must not learn control-plane endpoints, which may carry tokens in their path (Slack/Teams
// incoming webhooks), nor internal response content (threat model T22): the note names only
// the trigger, the service and the failure class; the full redacted error is in the logs.
func failureNote(appNamespace string, tr taggedResult) string {
	if tr.engine == nil || tr.engine.namespace != appNamespace {
		return fmt.Sprintf("Failed to deliver notification %s to service %s: %s (details in the notifications controller logs)",
			tr.Trigger, tr.Destination.Service, failureClass(tr.Err))
	}
	note := fmt.Sprintf("Failed to deliver notification %s to %s: %v", tr.Trigger, tr.Destination, tr.Err)
	if tr.engine.tenant {
		note += fmt.Sprintf(" (configuration in namespace %s)", tr.engine.namespace)
	}
	return note
}

// failureClass summarises a delivery error without any of its text: no URL, recipient,
// response body or template output.
func failureClass(err error) string {
	var se *services.StatusError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("the service answered HTTP %d", se.StatusCode)
	case errors.Is(err, notifications.ErrTriggerNotFound):
		return "the trigger is not defined"
	case errors.Is(err, notifications.ErrTemplateNotFound):
		return "a template is not defined"
	case errors.Is(err, notifications.ErrServiceNotConfigured):
		return "the service is not configured"
	case errors.Is(err, services.ErrRecipientNotConfigured):
		return "the recipient is not configured"
	case errors.Is(err, services.ErrURLNotAllowed):
		return "the endpoint is not allowed"
	case errors.Is(err, services.ErrInvalidOptions):
		return "the service is misconfigured"
	case errors.Is(err, services.ErrInvalidNotification):
		return "the rendered notification is invalid"
	case errors.Is(err, notifications.ErrTemplateOutputTooLarge), errors.Is(err, notifications.ErrTemplateLimitExceeded):
		return "a template limit was exceeded"
	case errors.Is(err, notifications.ErrRepoServiceUnavailable):
		return "the repository service is unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "the delivery timed out"
	case errors.Is(err, services.ErrDeliveryFailed):
		return "the service rejected the notification"
	default:
		return "rendering or delivery failed"
	}
}

// notifyAction is the Event action of a delivery failure: "Notify:<trigger>:<service>". The
// events.k8s.io recorder aggregates Events with the same regarding object, reason and action
// into one series (keeping the first note), so the action names the failing trigger and
// service; each failing destination then gets its own Event, and a destination failing at
// every evaluation updates its series count instead of creating new Events.
func notifyAction(tr taggedResult) string {
	a := actionNotify + ":" + tr.Trigger + ":" + tr.Destination.Service
	if tr.engine != nil && tr.engine.tenant {
		a += ":" + tr.engine.namespace
	}
	return truncateUTF8(a, maxEventActionBytes)
}

// isConfigurationError reports failures that retrying cannot fix.
func isConfigurationError(err error) bool {
	return errors.Is(err, notifications.ErrTriggerNotFound) ||
		errors.Is(err, notifications.ErrTemplateNotFound) ||
		errors.Is(err, notifications.ErrServiceNotConfigured)
}

// suppressFailure reports whether a "not defined" failure belongs to another configuration:
// a namespace configuration never reports unknown services/triggers (the control-plane
// configuration does, once), and the control-plane configuration does not report names a
// namespace configuration of the Application defines.
func suppressFailure(tr taggedResult, engines []*engineEntry) bool {
	if len(engines) < 2 {
		return false
	}
	var defined func(*engineEntry) bool
	switch {
	case errors.Is(tr.Err, notifications.ErrServiceNotConfigured):
		defined = func(e *engineEntry) bool { return e.hasService(tr.Destination.Service) }
	case errors.Is(tr.Err, notifications.ErrTriggerNotFound):
		defined = func(e *engineEntry) bool { return e.hasTrigger(tr.Trigger) }
	default:
		return false
	}
	if tr.engine.tenant {
		return true
	}
	for _, e := range engines {
		if e != tr.engine && defined(e) {
			return true
		}
	}
	return false
}

// persist applies delta to the delivery-state annotation with a merge patch carrying the
// resourceVersion (optimistic concurrency). On conflict the Application is read again and the
// delta re-applied to its current annotation, preserving concurrent changes.
func (r *reconciler) persist(ctx context.Context, app *unstructured.Unstructured, delta stateDelta) error {
	key := client.ObjectKeyFromObject(app)
	annKey := notifications.DeliveryStateAnnotation(r.prefix)
	backoff := wait.Backoff{Duration: 20 * time.Millisecond, Factor: 2, Jitter: 0.2, Steps: maxPersistAttempts, Cap: 2 * time.Second}
	obj := app
	var lastErr error
	for attempt := range maxPersistAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(lastErr, ctx.Err())
			case <-time.After(backoff.Step()):
			}
			fresh := newApplication()
			if err := r.live.Get(ctx, key, fresh); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("reading application: %w", err)
			}
			obj = fresh
		}
		current := obj.GetAnnotations()[annKey]
		enc, err := delta.applyTo(notifications.ParseDeliveryState(current)).Encode()
		if err != nil {
			return err
		}
		if enc == current {
			r.memo.written(key, obj.GetResourceVersion())
			return nil
		}
		patch, err := statePatch(obj.GetResourceVersion(), annKey, enc)
		if err != nil {
			return err
		}
		err = r.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
		switch {
		case err == nil:
			r.memo.written(key, obj.GetResourceVersion())
			return nil
		case apierrors.IsNotFound(err):
			return nil
		case apierrors.IsConflict(err):
			lastErr = err
		default:
			return fmt.Errorf("patching application: %w", err)
		}
	}
	return fmt.Errorf("still conflicting after %d attempts: %w", maxPersistAttempts, lastErr)
}

// statePatch is a JSON merge patch setting (or removing, for "") one annotation, with the
// resourceVersion precondition.
func statePatch(resourceVersion, key, value string) ([]byte, error) {
	var v any
	if value != "" {
		v = value
	}
	meta := map[string]any{"annotations": map[string]any{key: v}}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	b, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return nil, fmt.Errorf("encoding state patch: %w", err)
	}
	return b, nil
}
