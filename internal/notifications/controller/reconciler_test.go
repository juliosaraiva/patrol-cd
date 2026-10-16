package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

func TestRequeueAfter(t *testing.T) {
	t.Parallel()
	r := &reconciler{cfg: &completed{Config: Config{ResyncPeriod: time.Minute}}}
	for range 50 {
		d := r.requeueAfter(0)
		assert.GreaterOrEqual(t, d, time.Minute)
		assert.LessOrEqual(t, d, time.Minute+6*time.Second, "jitter is at most 10%")
	}
	assert.Equal(t, retryBase, r.requeueAfter(1))
	assert.Equal(t, 2*retryBase, r.requeueAfter(2))
	assert.Equal(t, 4*retryBase, r.requeueAfter(3))
	assert.Equal(t, time.Minute, r.requeueAfter(10), "capped at the resync period")
	assert.Equal(t, time.Minute, r.requeueAfter(1000))
}

func TestStatePatch(t *testing.T) {
	t.Parallel()
	b, err := statePatch("12", "notified.notifications.cd.patrol.example", `{"k":1}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"12","annotations":{"notified.notifications.cd.patrol.example":"{\"k\":1}"}}}`, string(b))
	b, err = statePatch("", "a", "")
	require.NoError(t, err)
	assert.JSONEq(t, `{"metadata":{"annotations":{"a":null}}}`, string(b), "an empty state removes the annotation")
}

func TestNotifyAction(t *testing.T) {
	t.Parallel()
	cp := &engineEntry{namespace: "patrol"}
	tenant := &engineEntry{namespace: "team-a", tenant: true}
	tr := func(e *engineEntry, trigger, service string) taggedResult {
		return taggedResult{DeliveryResult: notifications.DeliveryResult{Trigger: trigger, Destination: services.Destination{Service: service, Recipient: "r"}}, engine: e}
	}
	assert.Equal(t, "Notify:on-sync-failed:slack", notifyAction(tr(cp, "on-sync-failed", "slack")))
	assert.Equal(t, "Notify:on-sync-failed:slack:team-a", notifyAction(tr(tenant, "on-sync-failed", "slack")))
	long := notifyAction(tr(cp, strings.Repeat("t", 200), "slack"))
	assert.Len(t, long, maxEventActionBytes)
}

// T22: a failure Event in a namespace other than the failing configuration's never carries
// the endpoint URL, recipient or response body of the control-plane configuration.
func TestFailureNote(t *testing.T) {
	t.Parallel()
	cp := &engineEntry{namespace: "patrol"}
	tenant := &engineEntry{namespace: "team-a", tenant: true}
	const secretURL = "https://hooks.example.com/services/T000/B000/XXXXSECRETPATH"
	statusErr := fmt.Errorf("sending: %w", &services.StatusError{Service: "webhook", Method: "POST", URL: secretURL, StatusCode: 500, Body: "internal-detail: db=10.0.0.5 user=svc-notify"})
	guardErr := fmt.Errorf("%w: address 10.0.0.5 is not publicly routable", services.ErrURLNotAllowed)
	res := func(e *engineEntry, err error) taggedResult {
		return taggedResult{DeliveryResult: notifications.DeliveryResult{Trigger: "on-sync-failed", Destination: services.Destination{Service: "hook", Recipient: "#private-ops"}, Status: notifications.StatusFailed, Err: err}, engine: e}
	}
	tests := []struct {
		name        string
		appNS       string
		tr          taggedResult
		want        []string
		mustNotHave []string
	}{
		{
			name: "control plane about an app elsewhere: class only", appNS: "team-a", tr: res(cp, statusErr),
			want:        []string{"on-sync-failed", "service hook", "HTTP 500", "controller logs"},
			mustNotHave: []string{"XXXXSECRETPATH", "hooks.example.com", "internal-detail", "10.0.0.5", "#private-ops"},
		},
		{
			name: "control plane, guard refusal elsewhere", appNS: "team-a", tr: res(cp, guardErr),
			want: []string{"the endpoint is not allowed"}, mustNotHave: []string{"10.0.0.5"},
		},
		{
			name: "control plane, unknown error elsewhere", appNS: "team-a", tr: res(cp, errors.New("dial tcp "+secretURL+": refused")),
			want: []string{"rendering or delivery failed"}, mustNotHave: []string{"XXXXSECRETPATH"},
		},
		{
			name: "missing engine is treated as foreign", appNS: "patrol", tr: res(nil, statusErr),
			want: []string{"HTTP 500"}, mustNotHave: []string{"XXXXSECRETPATH"},
		},
		{
			name: "control plane about an app of the control-plane namespace: full error", appNS: "patrol", tr: res(cp, statusErr),
			want: []string{"Failed to deliver notification on-sync-failed to hook:#private-ops: ", "XXXXSECRETPATH", "internal-detail"},
		},
		{
			name: "namespace configuration in its own namespace: full error", appNS: "team-a", tr: res(tenant, statusErr),
			want: []string{"XXXXSECRETPATH", "(configuration in namespace team-a)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			note := failureNote(tc.appNS, tc.tr)
			for _, w := range tc.want {
				assert.Contains(t, note, w)
			}
			for _, n := range tc.mustNotHave {
				assert.NotContains(t, note, n)
			}
		})
	}
}

func TestFailureClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", &services.StatusError{StatusCode: 403, URL: "https://h/p"}), "the service answered HTTP 403"},
		{fmt.Errorf("x: %w", notifications.ErrTriggerNotFound), "the trigger is not defined"},
		{fmt.Errorf("x: %w", notifications.ErrTemplateNotFound), "a template is not defined"},
		{fmt.Errorf("x: %w", notifications.ErrServiceNotConfigured), "the service is not configured"},
		{fmt.Errorf("x: %w", services.ErrRecipientNotConfigured), "the recipient is not configured"},
		{fmt.Errorf("x: %w", services.ErrURLNotAllowed), "the endpoint is not allowed"},
		{fmt.Errorf("x: %w", services.ErrInvalidOptions), "the service is misconfigured"},
		{fmt.Errorf("x: %w", services.ErrInvalidNotification), "the rendered notification is invalid"},
		{fmt.Errorf("x: %w", notifications.ErrTemplateOutputTooLarge), "a template limit was exceeded"},
		{fmt.Errorf("x: %w", notifications.ErrTemplateLimitExceeded), "a template limit was exceeded"},
		{fmt.Errorf("x: %w", notifications.ErrRepoServiceUnavailable), "the repository service is unavailable"},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "the delivery timed out"},
		{fmt.Errorf("x: %w", services.ErrDeliveryFailed), "the service rejected the notification"},
		{errors.New("anything"), "rendering or delivery failed"},
		{nil, "rendering or delivery failed"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, failureClass(tc.err), "%v", tc.err)
	}
}

func TestSuppressFailure(t *testing.T) {
	t.Parallel()
	cp := &engineEntry{namespace: "patrol", services: map[string]bool{"cp-hook": true}, triggers: map[string]bool{"cp-trigger": true}}
	tenant := &engineEntry{namespace: "team-a", tenant: true, services: map[string]bool{"t-hook": true}, triggers: map[string]bool{"t-trigger": true}}
	both := []*engineEntry{cp, tenant}
	svcErr := fmt.Errorf("%w: notification service 'x' is not supported", notifications.ErrServiceNotConfigured)
	trgErr := fmt.Errorf("%w: trigger with name 'x' is not defined", notifications.ErrTriggerNotFound)
	res := func(e *engineEntry, trigger, service string, err error) taggedResult {
		return taggedResult{DeliveryResult: notifications.DeliveryResult{Trigger: trigger, Destination: services.Destination{Service: service}, Status: notifications.StatusFailed, Err: err}, engine: e}
	}
	tests := []struct {
		name    string
		tr      taggedResult
		engines []*engineEntry
		want    bool
	}{
		{name: "single configuration reports", tr: res(cp, "cp-trigger", "nope", svcErr), engines: []*engineEntry{cp}},
		{name: "tenant never reports unknown services", tr: res(tenant, "cp-trigger", "cp-hook", svcErr), engines: both, want: true},
		{name: "tenant never reports unknown triggers", tr: res(tenant, "cp-trigger", "cp-hook", trgErr), engines: both, want: true},
		{name: "control plane defers to the tenant's service", tr: res(cp, "t-trigger", "t-hook", svcErr), engines: both, want: true},
		{name: "control plane defers to the tenant's trigger", tr: res(cp, "t-trigger", "cp-hook", trgErr), engines: both, want: true},
		{name: "unknown everywhere is reported once", tr: res(cp, "cp-trigger", "nope", svcErr), engines: both},
		{name: "delivery errors are always reported", tr: res(tenant, "t-trigger", "t-hook", errors.New("HTTP 500")), engines: both},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, suppressFailure(tc.tr, tc.engines))
		})
	}
}

// NOTIF-053/055/057: results become metrics, logs and Warning Events; configuration errors
// are reported but not retried sooner; interrupted deliveries are not reported.
func TestReport(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := notifications.NewPrometheusMetrics(reg)
	require.NoError(t, err)
	rec := &recordingRecorder{}
	r := &reconciler{metrics: m, recorder: rec, log: slog.New(slog.DiscardHandler)}
	cp := &engineEntry{namespace: "patrol", services: map[string]bool{"hook": true}, triggers: map[string]bool{"t": true}}
	app := newApplication()
	app.SetName("guestbook")
	app.SetNamespace("patrol")
	res := func(status notifications.DeliveryStatus, trigger, service string, err error) taggedResult {
		return taggedResult{DeliveryResult: notifications.DeliveryResult{Trigger: trigger, Destination: services.Destination{Service: service, Recipient: "r"}, Status: status, Err: err}, engine: cp}
	}

	failed := r.report(context.Background(), slog.New(slog.DiscardHandler), app, []*engineEntry{cp}, []taggedResult{
		res(notifications.StatusSent, "t", "hook", nil),
		res(notifications.StatusSkippedDuplicate, "t", "hook", nil),
		res(notifications.StatusFailed, "t", "nope", fmt.Errorf("%w: notification service 'nope' is not supported", notifications.ErrServiceNotConfigured)),
		res(notifications.StatusFailed, "missing", "hook", fmt.Errorf("%w: trigger with name 'missing' is not defined", notifications.ErrTriggerNotFound)),
	})
	assert.False(t, failed, "configuration errors do not shorten the requeue")
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "t", "service": "hook", "succeeded": "true"}))
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "t", "service": "nope", "succeeded": "false"}))
	assert.Zero(t, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "missing", "service": "hook", "succeeded": "false"}),
		"an undefined trigger is not a delivery attempt")
	events := rec.all()
	require.Len(t, events, 2)
	for _, e := range events {
		assert.Equal(t, corev1.EventTypeWarning, e.typ)
		assert.Equal(t, ReasonNotificationDeliveryFailed, e.reason)
		assert.Same(t, app, e.obj)
	}
	assert.Equal(t, "Notify:t:nope", events[0].action)
	assert.Contains(t, events[0].note, "Failed to deliver notification t to nope:r: ")
	assert.Contains(t, events[1].note, "trigger with name 'missing' is not defined")

	// A delivery error asks for a sooner retry.
	failed = r.report(context.Background(), slog.New(slog.DiscardHandler), app, []*engineEntry{cp}, []taggedResult{
		res(notifications.StatusFailed, "t", "hook", errors.New("webhook: HTTP 500")),
	})
	assert.True(t, failed)
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "t", "service": "hook", "succeeded": "false"}))

	// Deliveries interrupted by shutdown are neither counted nor reported.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed = r.report(ctx, slog.New(slog.DiscardHandler), app, []*engineEntry{cp}, []taggedResult{
		res(notifications.StatusFailed, "t", "hook", fmt.Errorf("sending: %w", context.Canceled)),
	})
	assert.False(t, failed)
	assert.Len(t, rec.all(), 3)
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "t", "service": "hook", "succeeded": "false"}))
}

// NOTIF-002: applications outside the watched namespaces are never read nor evaluated, and a
// deleted application is forgotten.
func TestReconcileScope(t *testing.T) {
	t.Parallel()
	scheme, err := newScheme()
	require.NoError(t, err)
	var gets atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	cfg, err := Config{RestConfig: &rest.Config{}, Namespace: "patrol", ApplicationNamespaces: []string{"team-*"}, AllowPrivateEgress: true}.complete()
	require.NoError(t, err)
	r, err := newReconciler(cfg, c, c, nil)
	require.NoError(t, err)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "other", Name: "app"}})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, res)
	assert.Zero(t, gets.Load(), "out-of-scope applications are not read")

	key := types.NamespacedName{Namespace: "team-a", Name: "gone"}
	r.memo.failedWrite(key, diffState(nil, notifications.DeliveryState{"k": 1}))
	res, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, res)
	_, ok := r.memo.unpersistedDelta(key)
	assert.False(t, ok, "a deleted application is forgotten")
}

func TestEvalMetricsForwardsOnlyEvaluations(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := notifications.NewPrometheusMetrics(reg)
	require.NoError(t, err)
	e := evalMetrics{m: m}
	e.IncDelivery("t", "s", true)
	e.IncTriggerEvaluation("t", true)
	assert.Zero(t, counterSum(t, reg, "patrol_notifications_deliveries_total"))
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_trigger_eval_total", map[string]string{"name": "t", "triggered": "true"}))
}

func TestIsConfigurationError(t *testing.T) {
	t.Parallel()
	for _, err := range []error{notifications.ErrTriggerNotFound, notifications.ErrTemplateNotFound, notifications.ErrServiceNotConfigured} {
		assert.True(t, isConfigurationError(fmt.Errorf("x: %w", err)))
	}
	assert.False(t, isConfigurationError(errors.New("HTTP 500")))
}

// The subscription check sees exactly what the engine needs and the same destinations as the
// full object.
func TestSubscriptionView(t *testing.T) {
	t.Parallel()
	app := newApplication()
	app.SetName("guestbook")
	app.SetNamespace("apps")
	app.SetLabels(map[string]string{"team": "a"})
	app.SetAnnotations(map[string]string{subscribePrefix + "on-x.hook": "r1;r2"})
	app.Object["spec"] = map[string]any{"project": "team", "source": map[string]any{"repoURL": "x"}}
	app.Object["status"] = map[string]any{"resources": []any{map[string]any{"kind": "ConfigMap"}}}
	view := subscriptionView(app)
	assert.Equal(t, map[string]any{
		"metadata": map[string]any{
			"name": "guestbook", "namespace": "apps",
			"labels":      map[string]any{"team": "a"},
			"annotations": map[string]any{subscribePrefix + "on-x.hook": "r1;r2"},
		},
		"spec": map[string]any{"project": "team"},
	}, view)

	cfg, err := notifications.ParseConfig(map[string]string{
		"trigger.on-x":  "- when: 'true'\n  send: [t]",
		"template.t":    "message: hi",
		"subscriptions": "- recipients: [hook:r3]\n  triggers: [on-x]\n  selector: team=a",
	}, nil)
	require.NoError(t, err)
	eng, err := notifications.NewEngine(cfg)
	require.NoError(t, err)
	full, err := eng.Destinations(context.Background(), app.Object)
	require.NoError(t, err)
	fromView, err := eng.Destinations(context.Background(), view)
	require.NoError(t, err)
	assert.Equal(t, full, fromView)
	assert.Len(t, fromView["on-x"], 3)
	assert.Equal(t, map[string]any{"spec": map[string]any{}, "metadata": map[string]any{"name": "", "namespace": ""}}, subscriptionView(newApplication()))
}
