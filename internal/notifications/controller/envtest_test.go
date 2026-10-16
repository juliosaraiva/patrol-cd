package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

const subscribePrefix = "notifications.cd.patrol.example/subscribe."

// webhookTemplate renders a JSON body describing the application for the webhook service
// named hook. Nested fields use dig so absent sections render null instead of failing.
func webhookTemplate(hook, event string) string {
	return fmt.Sprintf(`webhook:
  %s:
    method: POST
    path: /%s
    body: |
      {"event": %q,
       "app": {{.app.metadata.name | toJson}},
       "namespace": {{.app.metadata.namespace | toJson}},
       "phase": {{dig "status" "operationState" "phase" nil .app | toJson}},
       "revision": {{dig "status" "operationState" "syncResult" "revision" nil .app | toJson}},
       "resources": {{dig "status" "operationState" "syncResult" "resources" nil .app | toJson}},
       "initiatedBy": {{dig "status" "operationState" "operation" "initiatedBy" "username" nil .app | toJson}},
       "health": {{dig "status" "health" "status" nil .app | toJson}},
       "sync": {{dig "status" "sync" "status" nil .app | toJson}},
       "conditions": {{dig "status" "conditions" nil .app | toJson}},
       "project": {{dig "metadata" "name" nil .appProject | toJson}},
       "projectDescription": {{dig "spec" "description" nil .appProject | toJson}},
       "secrets": "{{if .secrets}}exposed{{else}}hidden{{end}}"}
`, hook, event, event)
}

func webhookService(url string, extra string) string {
	return "url: " + url + "\nretryMax: 0\n" + extra
}

// NOTIF-002/010/022/025/026/032/047/052/057/058: an Application moving through sync/health/
// operation states gets one delivery per transition, with the Operation's per-resource results
// merged into .app.status.operationState; the state annotation prevents duplicates across a
// controller restart.
func TestControllerDeliversOnTransitionsAndDeduplicatesAcrossRestart(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-transitions"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, "headers:\n- name: X-Token\n  value: $token\n"),
		"template.running":     webhookTemplate("hook", "running"),
		"template.synced":      webhookTemplate("hook", "synced"),
		"template.degraded":    webhookTemplate("hook", "degraded"),
		"trigger.on-running": `- when: app.status.operationState != nil and app.status.operationState.phase in ['Running']
  send: [running]`,
		"trigger.on-synced": `- when: app.status.operationState != nil and app.status.operationState.phase in ['Succeeded']
  oncePer: app.status.operationState?.syncResult?.revision
  send: [synced]`,
		"trigger.on-degraded": `- when: app.status.health.status == 'Degraded'
  send: [degraded]`,
	}, map[string]string{"token": "s3cr3t-token-value"})
	mkProject(ctx, t, ns, "default", "Default project", nil)
	key := mkApp(ctx, t, ns, "guestbook", "default", nil, map[string]string{
		subscribePrefix + "on-running.hook":  "",
		subscribePrefix + "on-synced.hook":   "",
		subscribePrefix + "on-degraded.hook": "",
	})
	mkOperation(ctx, t, ns, "guestbook-sync-1", "guestbook",
		v1alpha1.OperationSpec{Sync: &v1alpha1.SyncOperation{Revision: "abc123"}, InitiatedBy: v1alpha1.OperationInitiator{Username: "alice"}},
		v1alpha1.OperationStatus{Phase: v1alpha1.OperationRunning, StartedAt: timePtr("2026-01-01T00:00:00Z")})

	reg := prometheus.NewRegistry()
	stop := startController(t, baseConfig(ns, reg))

	// Running: delivered once, with the requested operation from the Operation object.
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) {
		s.Sync.Status = v1alpha1.SyncStatusCodeOutOfSync
		s.Health.Status = v1alpha1.HealthStatusProgressing
		s.ReconciledAt = timePtr("2026-01-01T00:00:00Z")
		s.OperationState = &v1alpha1.ApplicationOperationState{OperationName: "guestbook-sync-1", Phase: v1alpha1.OperationRunning, StartedAt: timePtr("2026-01-01T00:00:00Z")}
		s.Conditions = []metav1.Condition{
			{Type: v1alpha1.ApplicationConditionSynced, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonOutOfSync, Message: "out of sync", LastTransitionTime: mustTime("2026-01-01T00:00:00Z")},
			{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonSharedResource, Message: "shared", LastTransitionTime: mustTime("2026-01-01T00:00:00Z")},
		}
	})
	require.Eventually(t, func() bool { return hook.count("event", "running") == 1 }, 20*time.Second, 50*time.Millisecond)
	running, _ := hook.find("event", "running")
	assert.Equal(t, http.MethodPost, running.Method)
	assert.Equal(t, "/running", running.Path)
	assert.Equal(t, "s3cr3t-token-value", running.Header.Get("X-Token"), "$token resolved from the control-plane Secret")
	assert.Equal(t, "Running", running.Body["phase"])
	assert.Equal(t, "alice", running.Body["initiatedBy"])
	assert.Equal(t, "default", running.Body["project"])
	assert.Equal(t, "Default project", running.Body["projectDescription"])
	assert.Equal(t, "exposed", running.Body["secrets"], ".secrets is available without self-service")
	assert.Equal(t, []any{map[string]any{"type": "SharedResourceWarning", "message": "shared", "lastTransitionTime": "2026-01-01T00:00:00Z"}}, running.Body["conditions"],
		"only active Argo CD condition types are shown")

	// Operation finished but the app's sync status is older than finishedAt: skipped (NOTIF-010).
	setOperationStatus(ctx, t, types.NamespacedName{Namespace: ns, Name: "guestbook-sync-1"}, v1alpha1.OperationStatus{
		Phase:      v1alpha1.OperationSucceeded,
		StartedAt:  timePtr("2026-01-01T00:00:00Z"),
		FinishedAt: timePtr("2026-01-01T00:05:00Z"),
		SyncResult: &v1alpha1.SyncOperationResult{
			Revision: "abc123",
			Resources: []v1alpha1.ResourceResult{{
				Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "guestbook", Name: "guestbook-ui",
				Status: v1alpha1.ResultCodeSynced, Message: "deployment.apps/guestbook-ui created", SyncPhase: v1alpha1.SyncPhaseSync,
			}},
		},
	})
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) {
		s.Sync.Status = v1alpha1.SyncStatusCodeSynced
		s.Health.Status = v1alpha1.HealthStatusHealthy
		s.OperationState.Phase = v1alpha1.OperationSucceeded
		s.OperationState.FinishedAt = timePtr("2026-01-01T00:05:00Z")
		s.OperationState.SyncResult = &v1alpha1.SyncResultSummary{Revision: "abc123"}
	})
	time.Sleep(3 * time.Second)
	assert.Zero(t, hook.count("event", "synced"), "stale sync status must not be evaluated")

	// Refreshed after the operation: delivered once, with the Operation's resources.
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) { s.ReconciledAt = timePtr("2026-01-01T00:06:00Z") })
	require.Eventually(t, func() bool { return hook.count("event", "synced") == 1 }, 20*time.Second, 50*time.Millisecond)
	synced, _ := hook.find("event", "synced")
	assert.Equal(t, "Succeeded", synced.Body["phase"])
	assert.Equal(t, "abc123", synced.Body["revision"])
	assert.Equal(t, "Healthy", synced.Body["health"])
	assert.Equal(t, "Synced", synced.Body["sync"])
	resources, ok := synced.Body["resources"].([]any)
	require.True(t, ok, "resources from the Operation: %v", synced.Raw)
	require.Len(t, resources, 1)
	assert.Equal(t, "guestbook-ui", resources[0].(map[string]any)["name"])
	assert.Equal(t, "deployment.apps/guestbook-ui created", resources[0].(map[string]any)["message"])

	// The delivery state is persisted; the cleared on-running condition is gone.
	require.Eventually(t, func() bool {
		st := deliveryState(ctx, t, key)
		hasSynced, hasRunning := false, false
		for k := range st {
			hasSynced = hasSynced || strings.Contains(k, ":on-synced:")
			hasRunning = hasRunning || strings.Contains(k, ":on-running:") || strings.HasPrefix(k, "on-running:")
		}
		return hasSynced && !hasRunning
	}, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "on-synced", "service": "hook", "succeeded": "true"}))
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "on-running", "service": "hook", "succeeded": "true"}))
	assert.Positive(t, counter(t, reg, "patrol_notifications_trigger_eval_total", map[string]string{"name": "on-degraded", "triggered": "false"}))

	// Restart: the new controller evaluates the application again but sends nothing new.
	time.Sleep(2 * time.Second)
	stop()
	reg2 := prometheus.NewRegistry()
	startController(t, baseConfig(ns, reg2))
	updateApp(ctx, t, key, func(a *v1alpha1.Application) { a.Labels = map[string]string{"touched": "true"} })
	require.Eventually(t, func() bool {
		return counter(t, reg2, "patrol_notifications_trigger_eval_total", map[string]string{"name": "on-synced", "triggered": "true"}) >= 3
	}, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, hook.count("event", "synced"), "no duplicate after restart")
	assert.Equal(t, 1, hook.count("event", "running"))
	assert.Zero(t, counterSum(t, reg2, "patrol_notifications_deliveries_total"))

	// Health degrades: one delivery, not repeated while it stays degraded.
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) { s.Health.Status = v1alpha1.HealthStatusDegraded })
	require.Eventually(t, func() bool { return hook.count("event", "degraded") == 1 }, 20*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)
	assert.Equal(t, 1, hook.count("event", "degraded"))
	assert.Equal(t, 1, hook.count("event", "synced"))
	assert.Empty(t, failureEvents(ctx, t, key))
}

// NOTIF-054/018: self-service configurations. A namespace configuration notifies only about
// Applications of its namespace, cannot use control-plane Secret values ($references or
// .secrets), and failures about services only the other configuration defines are not reported.
func TestSelfServiceNamespaceConfigurationIsolation(t *testing.T) {
	ctx := requireEnv(t)
	const cp, nsA, nsB = "notif-ss-cp", "notif-ss-a", "notif-ss-b"
	mkNamespace(ctx, t, cp, nsA, nsB)
	cpHook, tenantHook := newReceiver(t), newReceiver(t)
	mkConfig(ctx, t, cp, map[string]string{
		"service.webhook.cp-hook": webhookService(cpHook.srv.URL, ""),
		"template.cp-tmpl":        webhookTemplate("cp-hook", "cp"),
		"trigger.always":          "- when: 'true'\n  send: [cp-tmpl]",
	}, map[string]string{"cp-token": "control-plane-super-secret"})
	mkConfig(ctx, t, nsA, map[string]string{
		"service.webhook.tenant-hook": webhookService(tenantHook.srv.URL,
			"headers:\n- name: X-Stolen\n  value: $cp-token\n- name: X-Tenant\n  value: $tenant-token\n"),
		"template.tenant-tmpl": webhookTemplate("tenant-hook", "tenant"),
		"trigger.always":       "- when: 'true'\n  send: [tenant-tmpl]",
		// A default subscription without selector: applies to every app this config may see.
		"subscriptions": "- recipients: [tenant-hook]\n  triggers: [always]",
	}, map[string]string{"tenant-token": "tenant-a-token"})
	mkProject(ctx, t, cp, "default", "", nil)
	a1 := mkApp(ctx, t, nsA, "app-a1", "default", nil, map[string]string{subscribePrefix + "always.cp-hook": ""})
	b1 := mkApp(ctx, t, nsB, "app-b1", "default", nil, map[string]string{subscribePrefix + "always.cp-hook": ""})

	cfg := baseConfig(cp, prometheus.NewRegistry())
	cfg.ApplicationNamespaces = []string{"notif-ss-*"}
	cfg.SelfServiceEnabled = true
	// httptest listens on loopback; AllowPrivateEgress (baseConfig) only covers the control plane.
	cfg.TenantEgressAllowedPrefixes = loopbackPrefixes
	stop := startController(t, cfg)

	require.Eventually(t, func() bool {
		return cpHook.count("app", "app-a1") == 1 && cpHook.count("app", "app-b1") == 1 && tenantHook.count("app", "app-a1") == 1
	}, 30*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)

	assert.Zero(t, tenantHook.count("app", "app-b1"), "namespace A configuration must not see namespace B applications")
	assert.Equal(t, 1, tenantHook.count("app", "app-a1"))
	tenantReq, _ := tenantHook.find("app", "app-a1")
	assert.Equal(t, "$cp-token", tenantReq.Header.Get("X-Stolen"), "control-plane Secret keys are not resolvable from a namespace configuration")
	assert.Equal(t, "tenant-a-token", tenantReq.Header.Get("X-Tenant"))
	assert.Equal(t, "hidden", tenantReq.Body["secrets"])
	for _, rec := range append(tenantHook.requests(), cpHook.requests()...) {
		assert.NotContains(t, rec.Raw, "control-plane-super-secret")
		for _, vs := range rec.Header {
			for _, v := range vs {
				assert.NotContains(t, v, "control-plane-super-secret")
			}
		}
	}
	cpReq, _ := cpHook.find("app", "app-a1")
	assert.Equal(t, "hidden", cpReq.Body["secrets"], "self-service hides .secrets for the control-plane configuration too")
	// cp-hook is unknown to namespace A's configuration and tenant-hook to the control-plane one:
	// neither is a reportable failure.
	assert.Empty(t, failureEvents(ctx, t, a1))
	assert.Empty(t, failureEvents(ctx, t, b1))

	// Self-service disabled: namespace configurations are ignored.
	stop()
	a2 := mkApp(ctx, t, nsA, "app-a2", "default", nil, nil)
	cfg2 := baseConfig(cp, prometheus.NewRegistry())
	cfg2.ApplicationNamespaces = []string{"notif-ss-*"}
	reg2 := cfg2.Registry.(*prometheus.Registry)
	startController(t, cfg2)
	updateApp(ctx, t, a1, func(a *v1alpha1.Application) { a.Labels = map[string]string{"touched": "true"} })
	require.Eventually(t, func() bool {
		return counter(t, reg2, "patrol_notifications_trigger_eval_total", map[string]string{"name": "always", "triggered": "true"}) >= 4
	}, 20*time.Second, 50*time.Millisecond)
	assert.Zero(t, tenantHook.count("app", "app-a2"))
	assert.Equal(t, 1, tenantHook.count("app", "app-a1"))
	assert.Empty(t, failureEvents(ctx, t, a2))
}

var loopbackPrefixes = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// T22/T8 (NOTIF-053/054): a failure of the control-plane configuration about an Application
// of another namespace is reported there without the endpoint URL (whose path may hold a
// token) or the response body; the namespace's own configuration cannot reach a range only
// the control plane may reach; in the control-plane namespace the full error is kept.
func TestTenantNamespaceEventsAndEgressIsolation(t *testing.T) {
	ctx := requireEnv(t)
	const cp, appsNS = "notif-t22", "notif-t22-apps"
	const secretPath = "/services/T000/B000/XXXXSECRETPATH"
	const body = "internal-detail: db=10.0.0.5 user=svc-notify"
	mkNamespace(ctx, t, cp, appsNS)
	var cpCalls atomic.Int32
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cpCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(cpSrv.Close)
	tenantHook := newReceiver(t)
	mkConfig(ctx, t, cp, map[string]string{
		"service.webhook.cp-hook": webhookService(cpSrv.URL+secretPath, ""),
		"template.cp-tmpl":        webhookTemplate("cp-hook", "cp"),
		"trigger.always":          "- when: 'true'\n  send: [cp-tmpl]",
	}, nil)
	mkConfig(ctx, t, appsNS, map[string]string{
		"service.webhook.tenant-hook": webhookService(tenantHook.srv.URL, ""),
		"template.tenant-tmpl":        webhookTemplate("tenant-hook", "tenant"),
		"trigger.tenant-always":       "- when: 'true'\n  send: [tenant-tmpl]",
		"subscriptions":               "- recipients: [tenant-hook]\n  triggers: [tenant-always]",
	}, nil)
	mkProject(ctx, t, cp, "team", "", map[string]string{subscribePrefix + "always.cp-hook": ""})
	remote := mkApp(ctx, t, appsNS, "remote", "team", nil, nil)
	local := mkApp(ctx, t, cp, "local", "team", nil, nil)

	reg := prometheus.NewRegistry()
	cfg := baseConfig(cp, reg) // AllowPrivateEgress: the control plane may reach loopback
	cfg.ApplicationNamespaces = []string{appsNS}
	cfg.SelfServiceEnabled = true
	startController(t, cfg)

	noteOf := func(key types.NamespacedName, action string) (string, bool) {
		for _, e := range failureEvents(ctx, t, key) {
			if e.Action == action {
				return e.Note, true
			}
		}
		return "", false
	}
	require.Eventually(t, func() bool {
		_, cpRemote := noteOf(remote, "Notify:always:cp-hook")
		_, tenantRemote := noteOf(remote, "Notify:tenant-always:tenant-hook:"+appsNS)
		_, cpLocal := noteOf(local, "Notify:always:cp-hook")
		return cpRemote && tenantRemote && cpLocal
	}, 30*time.Second, 100*time.Millisecond, "events: %s\n%s", dumpEvents(ctx, t, appsNS), dumpEvents(ctx, t, cp))

	// Control-plane failure about the tenant's Application: class only.
	note, _ := noteOf(remote, "Notify:always:cp-hook")
	assert.Contains(t, note, "always")
	assert.Contains(t, note, "cp-hook")
	assert.Contains(t, note, "HTTP 500")
	// Nothing of the control-plane endpoint or response reaches any Event of the namespace.
	list := &eventsv1.EventList{}
	require.NoError(t, envClient.List(ctx, list, client.InNamespace(appsNS)))
	require.NotEmpty(t, list.Items)
	for _, e := range list.Items {
		for _, leak := range []string{"XXXXSECRETPATH", "/services/", cpSrv.Listener.Addr().String(), "internal-detail", "10.0.0.5", "svc-notify"} {
			assert.NotContains(t, e.Note, leak, "event %s/%s", e.Reason, e.Action)
		}
	}

	// The namespace's own webhook to loopback is refused: the control-plane egress policy
	// (AllowPrivateEgress) does not extend to namespace configurations.
	note, _ = noteOf(remote, "Notify:tenant-always:tenant-hook:"+appsNS)
	assert.Contains(t, note, "not allowed", "own configuration: full error")
	assert.Contains(t, note, "(configuration in namespace "+appsNS+")")
	assert.Empty(t, tenantHook.requests(), "the tenant webhook never reached the loopback receiver")
	assert.Positive(t, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "tenant-always", "service": "tenant-hook", "succeeded": "false"}))

	// In the control-plane namespace the full (redacted) error stays available to operators.
	note, _ = noteOf(local, "Notify:always:cp-hook")
	assert.Contains(t, note, "XXXXSECRETPATH")
	assert.Contains(t, note, "internal-detail")
	assert.Positive(t, cpCalls.Load())
	assert.Positive(t, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "always", "service": "cp-hook", "succeeded": "false"}))
}

// NOTIF-050/033: AppProject subscription annotations apply to every Application of the
// project (looked up in the control-plane namespace, also for Applications elsewhere); an
// empty project means "default"; annotation changes are picked up without waiting for resync.
// The Applications exist before the controller starts and are notified on its first
// evaluation: like upstream there is no start-up suppression, only the delivery state
// prevents re-sends.
func TestProjectLevelSubscriptions(t *testing.T) {
	ctx := requireEnv(t)
	const cp, appsNS = "notif-proj", "notif-proj-apps"
	mkNamespace(ctx, t, cp, appsNS)
	hook := newReceiver(t)
	mkConfig(ctx, t, cp, map[string]string{
		"service.webhook.proj-hook": webhookService(hook.srv.URL, ""),
		"template.any":              webhookTemplate("proj-hook", "any"),
		"trigger.on-any":            "- when: 'true'\n  send: [any]",
	}, nil)
	sub := map[string]string{subscribePrefix + "on-any.proj-hook": ""}
	mkProject(ctx, t, cp, "team", "Team project", sub)
	mkProject(ctx, t, cp, "other", "Other project", nil)
	mkProject(ctx, t, cp, "default", "Default project", sub)
	mkApp(ctx, t, cp, "team-cp", "team", nil, nil)
	mkApp(ctx, t, appsNS, "team-remote", "team", nil, nil)
	mkApp(ctx, t, appsNS, "other-app", "other", nil, nil)
	mkApp(ctx, t, appsNS, "no-project", "", nil, nil)

	cfg := baseConfig(cp, prometheus.NewRegistry())
	cfg.ApplicationNamespaces = []string{appsNS}
	cfg.ResyncPeriod = time.Hour // project changes must not depend on the resync
	startController(t, cfg)

	require.Eventually(t, func() bool {
		return hook.count("app", "team-cp") == 1 && hook.count("app", "team-remote") == 1 && hook.count("app", "no-project") == 1
	}, 30*time.Second, 50*time.Millisecond)
	remote, _ := hook.find("app", "team-remote")
	assert.Equal(t, appsNS, remote.Body["namespace"])
	assert.Equal(t, "team", remote.Body["project"])
	assert.Equal(t, "Team project", remote.Body["projectDescription"])
	np, _ := hook.find("app", "no-project")
	assert.Equal(t, "default", np.Body["project"])
	time.Sleep(2 * time.Second)
	assert.Zero(t, hook.count("app", "other-app"), "an application of an unsubscribed project")

	// Subscribing the other project notifies its application promptly.
	proj := &v1alpha1.AppProject{}
	require.NoError(t, envClient.Get(ctx, types.NamespacedName{Namespace: cp, Name: "other"}, proj))
	proj.Annotations = sub
	require.NoError(t, envClient.Update(ctx, proj))
	require.Eventually(t, func() bool { return hook.count("app", "other-app") == 1 }, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, hook.count("app", "team-cp"))
}

// NOTIF-053/055/057: a failing delivery emits a Warning Event and a succeeded="false" metric,
// is not recorded, and is retried until it succeeds; an unknown service is reported too, and
// does not block the other destination.
func TestDeliveryFailureEmitsEventAndMetricThenRetries(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-failure"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	hook.status.Store(http.StatusInternalServerError)
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		"template.any":         webhookTemplate("hook", "any"),
		"trigger.always":       "- when: 'true'\n  send: [any]",
	}, nil)
	mkProject(ctx, t, ns, "default", "", nil)
	key := mkApp(ctx, t, ns, "flaky", "default", nil, map[string]string{
		subscribePrefix + "always.hook": "",
		subscribePrefix + "always.nope": "",
	})
	reg := prometheus.NewRegistry()
	startController(t, baseConfig(ns, reg))

	require.Eventually(t, func() bool {
		return counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "always", "service": "hook", "succeeded": "false"}) >= 1 &&
			counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "always", "service": "nope", "succeeded": "false"}) >= 1
	}, 20*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		var hookEvent, nopeEvent bool
		for _, e := range failureEvents(ctx, t, key) {
			assert.Equal(t, corev1.EventTypeWarning, e.Type)
			hookEvent = hookEvent || strings.Contains(e.Note, "always to hook:") && strings.Contains(e.Note, "500")
			nopeEvent = nopeEvent || strings.Contains(e.Note, "notification service 'nope' is not supported")
		}
		return hookEvent && nopeEvent
	}, 20*time.Second, 100*time.Millisecond, "events: %v", dumpEvents(ctx, t, ns))
	for k := range deliveryState(ctx, t, key) {
		assert.NotContains(t, k, ":hook:", "a failed delivery is not recorded")
	}

	// The endpoint recovers: the next evaluation delivers and records it.
	hook.status.Store(http.StatusOK)
	require.Eventually(t, func() bool { return hook.count("app", "flaky") == 1 }, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		for k := range deliveryState(ctx, t, key) {
			if strings.Contains(k, ":hook:") {
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1.0, counter(t, reg, "patrol_notifications_deliveries_total", map[string]string{"trigger": "always", "service": "hook", "succeeded": "true"}))
	time.Sleep(2 * time.Second)
	assert.Equal(t, 1, hook.count("app", "flaky"))
}

// NOTIF-002/003: only Applications of the control-plane namespace and the configured
// application namespaces (regular expression here) that match the label selector are processed.
func TestApplicationScopeAndLabelSelector(t *testing.T) {
	ctx := requireEnv(t)
	const cp, in, out = "notif-scope", "notif-scope-in", "notif-scope-out"
	mkNamespace(ctx, t, cp, in, out)
	hook := newReceiver(t)
	mkConfig(ctx, t, cp, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		"template.any":         webhookTemplate("hook", "any"),
		"trigger.always":       "- when: 'true'\n  send: [any]",
	}, nil)
	mkProject(ctx, t, cp, "default", "", nil)
	sub := map[string]string{subscribePrefix + "always.hook": ""}
	notify := map[string]string{"notify": "true"}
	mkApp(ctx, t, cp, "cp-labelled", "default", notify, sub)
	mkApp(ctx, t, cp, "cp-unlabelled", "default", nil, sub)
	mkApp(ctx, t, in, "in-labelled", "default", notify, sub)
	mkApp(ctx, t, out, "out-labelled", "default", notify, sub)

	cfg := baseConfig(cp, prometheus.NewRegistry())
	cfg.ApplicationNamespaces = []string{"/^notif-scope-in$/"}
	cfg.AppLabelSelector = "notify=true"
	startController(t, cfg)

	require.Eventually(t, func() bool {
		return hook.count("app", "cp-labelled") == 1 && hook.count("app", "in-labelled") == 1
	}, 30*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)
	assert.Zero(t, hook.count("app", "cp-unlabelled"))
	assert.Zero(t, hook.count("app", "out-labelled"))
}

// NOTIF-052: the state patch uses optimistic concurrency; on conflict the delta is re-applied
// to the current annotation so concurrent entries are kept.
func TestPersistMergesConcurrentStateChanges(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-persist"
	mkNamespace(ctx, t, ns)
	key := mkApp(ctx, t, ns, "app", "default", nil, map[string]string{
		notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix): `{"old":1,"stale":2}`,
	})
	c, err := Config{RestConfig: testCfg, Namespace: ns, AllowPrivateEgress: true}.complete()
	require.NoError(t, err)
	r, err := newReconciler(c, envClient, envClient, nil)
	require.NoError(t, err)

	stale := newApplication()
	require.NoError(t, envClient.Get(ctx, key, stale))
	// A concurrent writer adds an entry after we read the object.
	updateApp(ctx, t, key, func(a *v1alpha1.Application) {
		a.Annotations[notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix)] = `{"old":1,"stale":2,"foreign":3}`
	})
	delta := diffState(notifications.DeliveryState{"old": 1, "stale": 2}, notifications.DeliveryState{"old": 1, "mine": 4})
	require.NoError(t, r.persist(ctx, stale, delta))

	assert.Equal(t, notifications.DeliveryState{"old": 1, "foreign": 3, "mine": 4}, deliveryState(ctx, t, key))
	latest := &v1alpha1.Application{}
	require.NoError(t, envClient.Get(ctx, key, latest))
	assert.Equal(t, latest.ResourceVersion, r.memo.pendingWrite(key), "our write is remembered until the cache shows it")

	// Applying an empty result removes the annotation.
	require.NoError(t, r.persist(ctx, newestApp(ctx, t, key), diffState(notifications.DeliveryState{"old": 1, "foreign": 3, "mine": 4}, notifications.DeliveryState{})))
	require.NoError(t, envClient.Get(ctx, key, latest))
	_, present := latest.Annotations[notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix)]
	assert.False(t, present)
}

func newestApp(ctx context.Context, t *testing.T, key types.NamespacedName) *unstructured.Unstructured {
	t.Helper()
	u := newApplication()
	require.NoError(t, envClient.Get(ctx, key, u))
	return u
}

var _ client.Object = newApplication()

func dumpEvents(ctx context.Context, t *testing.T, ns string) string {
	list := &eventsv1.EventList{}
	require.NoError(t, envClient.List(ctx, list, client.InNamespace(ns)))
	var b strings.Builder
	for _, e := range list.Items {
		fmt.Fprintf(&b, "%s %s/%s %s %s: %s\n", e.Type, e.Regarding.Kind, e.Regarding.Name, e.Reason, e.Action, e.Note)
	}
	cl := &corev1.EventList{}
	require.NoError(t, envClient.List(ctx, cl, client.InNamespace(ns)))
	for _, e := range cl.Items {
		fmt.Fprintf(&b, "core: %s %s/%s %s: %s\n", e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
	}
	return b.String()
}
