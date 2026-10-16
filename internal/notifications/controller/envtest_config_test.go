package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

// updateConfigMap applies mutate to a ConfigMap's data with conflict retries.
func updateConfigMap(ctx context.Context, t *testing.T, key types.NamespacedName, mutate func(map[string]string)) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if err := envClient.Get(ctx, key, cm); err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		mutate(cm.Data)
		return envClient.Update(ctx, cm)
	}))
}

// NOTIF-006/014/055: custom ConfigMap/Secret names are honoured (objects with other names are
// ignored), edits apply without restart, an unparsable ConfigMap is reported with an Event and
// sends nothing until fixed, and an undefined trigger is reported on the Application.
func TestCustomConfigNamesHotReloadAndInvalidConfiguration(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-reload"
	const cmName, secretName = "custom-notifications-cm", "custom-notifications-secret"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	cmKey := types.NamespacedName{Namespace: ns, Name: cmName}
	require.NoError(t, envClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}, Data: map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, "headers:\n- name: X-Token\n  value: $token\n"),
		"template.a":           webhookTemplate("hook", "a"),
		"trigger.on-a":         "- when: 'true'\n  send: [a]",
	}}))
	require.NoError(t, envClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns}, StringData: map[string]string{"token": "custom-secret-token"}}))
	// The default-named ConfigMap is not the configuration here.
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		"template.b":           webhookTemplate("hook", "ignored"),
		"trigger.on-b":         "- when: 'true'\n  send: [b]",
	}, nil)
	mkProject(ctx, t, ns, "default", "", nil)
	key := mkApp(ctx, t, ns, "reload", "default", nil, map[string]string{
		subscribePrefix + "on-a.hook": "",
		subscribePrefix + "on-b.hook": "",
	})

	cfg := baseConfig(ns, prometheus.NewRegistry())
	cfg.ConfigMapName, cfg.SecretName = cmName, secretName
	startController(t, cfg)

	require.Eventually(t, func() bool { return hook.count("event", "a") == 1 }, 20*time.Second, 50*time.Millisecond)
	a, _ := hook.find("event", "a")
	assert.Equal(t, "custom-secret-token", a.Header.Get("X-Token"), "$references resolve from the custom Secret")
	require.Eventually(t, func() bool {
		for _, e := range failureEvents(ctx, t, key) {
			if strings.Contains(e.Note, "trigger with name 'on-b' is not defined") {
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "events: %s", dumpEvents(ctx, t, ns))
	assert.Zero(t, hook.count("event", "ignored"), "a ConfigMap with another name is not read")

	// Hot reload: defining the trigger delivers it without restart.
	updateConfigMap(ctx, t, cmKey, func(d map[string]string) {
		d["template.b"] = webhookTemplate("hook", "b")
		d["trigger.on-b"] = "- when: 'true'\n  send: [b]"
	})
	require.Eventually(t, func() bool { return hook.count("event", "b") == 1 }, 20*time.Second, 50*time.Millisecond)

	// An unparsable ConfigMap: Warning Event on it, nothing sent from it.
	updateConfigMap(ctx, t, cmKey, func(d map[string]string) { d["trigger.on-c"] = "- when: [unclosed" })
	require.Eventually(t, func() bool {
		list := &eventsv1.EventList{}
		require.NoError(t, envClient.List(ctx, list, client.InNamespace(ns)))
		for _, e := range list.Items {
			if e.Regarding.Kind == "ConfigMap" && e.Regarding.Name == cmName && e.Reason == ReasonInvalidNotificationsConfiguration && e.Type == corev1.EventTypeWarning {
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "events: %s", dumpEvents(ctx, t, ns))
	late := mkApp(ctx, t, ns, "late", "default", nil, map[string]string{subscribePrefix + "on-a.hook": ""})
	time.Sleep(3 * time.Second)
	assert.Zero(t, hook.count("app", "late"), "nothing is sent from an invalid configuration")

	// Fixed: deliveries resume.
	updateConfigMap(ctx, t, cmKey, func(d map[string]string) { delete(d, "trigger.on-c") })
	require.Eventually(t, func() bool { return hook.count("app", "late") == 1 }, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, hook.count("event", "b"))
	assert.Empty(t, failureEvents(ctx, t, late))
}

// NOTIF-052 recovery: deleting the delivery-state annotation re-sends currently true
// conditions exactly once.
func TestDeletingDeliveryStateResendsOnce(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-state-reset"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		"template.any":         webhookTemplate("hook", "any"),
		"trigger.always":       "- when: 'true'\n  send: [any]",
	}, nil)
	mkProject(ctx, t, ns, "default", "", nil)
	key := mkApp(ctx, t, ns, "reset", "default", nil, map[string]string{subscribePrefix + "always.hook": ""})
	startController(t, baseConfig(ns, prometheus.NewRegistry()))

	require.Eventually(t, func() bool { return hook.count("app", "reset") == 1 && len(deliveryState(ctx, t, key)) == 1 }, 20*time.Second, 50*time.Millisecond)
	updateApp(ctx, t, key, func(a *v1alpha1.Application) {
		delete(a.Annotations, notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix))
	})
	require.Eventually(t, func() bool { return hook.count("app", "reset") == 2 }, 20*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)
	assert.Equal(t, 2, hook.count("app", "reset"))
	assert.Len(t, deliveryState(ctx, t, key), 1)
}

// NOTIF-032/093..100: the upstream catalog trigger on-sync-failed (oncePer revision) fires for
// a cancelled Patrol Operation, which Argo CD reports as Failed "Operation terminated"; a
// later failure of the same revision is not notified again, a new revision is.
func TestCatalogTriggerOnCancelledOperation(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-catalog"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		// Overrides the catalog template of the same name (ConfigMap keys win).
		"template.app-sync-failed": `webhook:
  hook:
    method: POST
    body: |
      {"app": {{.app.metadata.name | toJson}},
       "phase": {{.app.status.operationState.phase | toJson}},
       "message": {{.app.status.operationState.message | toJson}},
       "revision": {{.app.status.operationState.syncResult.revision | toJson}},
       "resources": {{with .app.status.operationState.syncResult.resources}}{{len .}}{{else}}0{{end}}}
`,
	}, nil)
	mkProject(ctx, t, ns, "default", "", nil)
	key := mkApp(ctx, t, ns, "cancelled", "default", nil, map[string]string{subscribePrefix + "on-sync-failed.hook": ""})
	mkOperation(ctx, t, ns, "cancelled-1", "cancelled",
		v1alpha1.OperationSpec{Sync: &v1alpha1.SyncOperation{Revision: "rev1"}},
		v1alpha1.OperationStatus{
			Phase: v1alpha1.OperationCancelled, StartedAt: timePtr("2026-01-01T00:00:00Z"), FinishedAt: timePtr("2026-01-01T00:01:00Z"),
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "rev1", Resources: []v1alpha1.ResourceResult{{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "cm", Status: v1alpha1.ResultCodeSynced}}},
		})
	reg := prometheus.NewRegistry()
	startController(t, baseConfig(ns, reg))

	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) {
		s.Sync.Status = v1alpha1.SyncStatusCodeOutOfSync
		s.Health.Status = v1alpha1.HealthStatusHealthy
		s.ReconciledAt = timePtr("2026-01-01T00:02:00Z")
		s.OperationState = &v1alpha1.ApplicationOperationState{
			OperationName: "cancelled-1", Phase: v1alpha1.OperationCancelled,
			StartedAt: timePtr("2026-01-01T00:00:00Z"), FinishedAt: timePtr("2026-01-01T00:01:00Z"),
			SyncResult: &v1alpha1.SyncResultSummary{Revision: "rev1"},
		}
	})
	require.Eventually(t, func() bool { return hook.count("app", "cancelled") == 1 }, 20*time.Second, 50*time.Millisecond)
	got, _ := hook.find("app", "cancelled")
	assert.Equal(t, "Failed", got.Body["phase"])
	assert.Equal(t, "Operation terminated", got.Body["message"])
	assert.Equal(t, "rev1", got.Body["revision"])
	assert.InDelta(t, 1.0, got.Body["resources"], 0)

	// Same revision fails again: oncePer suppresses it.
	mkOperation(ctx, t, ns, "cancelled-2", "cancelled",
		v1alpha1.OperationSpec{Sync: &v1alpha1.SyncOperation{Revision: "rev1"}},
		v1alpha1.OperationStatus{Phase: v1alpha1.OperationFailed, Message: "one or more objects failed", StartedAt: timePtr("2026-01-01T00:03:00Z"), FinishedAt: timePtr("2026-01-01T00:04:00Z"),
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "rev1"}})
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) {
		s.ReconciledAt = timePtr("2026-01-01T00:05:00Z")
		s.OperationState = &v1alpha1.ApplicationOperationState{
			OperationName: "cancelled-2", Phase: v1alpha1.OperationFailed, Message: "one or more objects failed",
			StartedAt: timePtr("2026-01-01T00:03:00Z"), FinishedAt: timePtr("2026-01-01T00:04:00Z"),
			SyncResult: &v1alpha1.SyncResultSummary{Revision: "rev1"},
		}
	})
	require.Eventually(t, func() bool {
		return counter(t, reg, "patrol_notifications_trigger_eval_total", map[string]string{"name": "on-sync-failed", "triggered": "true"}) >= 3
	}, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, hook.count("app", "cancelled"))

	// A new revision fails: notified.
	mkOperation(ctx, t, ns, "cancelled-3", "cancelled",
		v1alpha1.OperationSpec{Sync: &v1alpha1.SyncOperation{Revision: "rev2"}},
		v1alpha1.OperationStatus{Phase: v1alpha1.OperationError, Message: "infrastructure error", StartedAt: timePtr("2026-01-01T00:06:00Z"), FinishedAt: timePtr("2026-01-01T00:07:00Z"),
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "rev2"}})
	setAppStatus(ctx, t, key, func(s *v1alpha1.ApplicationStatus) {
		s.ReconciledAt = timePtr("2026-01-01T00:08:00Z")
		s.OperationState = &v1alpha1.ApplicationOperationState{
			OperationName: "cancelled-3", Phase: v1alpha1.OperationError, Message: "infrastructure error",
			StartedAt: timePtr("2026-01-01T00:06:00Z"), FinishedAt: timePtr("2026-01-01T00:07:00Z"),
			SyncResult: &v1alpha1.SyncResultSummary{Revision: "rev2"},
		}
	})
	require.Eventually(t, func() bool { return hook.count("app", "cancelled") == 2 }, 20*time.Second, 50*time.Millisecond)
	second := hook.requests()[1]
	assert.Equal(t, "Error", second.Body["phase"])
	assert.Equal(t, "rev2", second.Body["revision"])
}

// NOTIF-001/004: with several processors and leader election, each application is notified
// exactly once and the Lease is held by the controller.
func TestConcurrentProcessorsWithLeaderElection(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "notif-concurrency"
	mkNamespace(ctx, t, ns)
	hook := newReceiver(t)
	mkConfig(ctx, t, ns, map[string]string{
		"service.webhook.hook": webhookService(hook.srv.URL, ""),
		"template.any":         webhookTemplate("hook", "any"),
		"trigger.always":       "- when: 'true'\n  send: [any]",
		"subscriptions":        "- recipients: [hook]\n  triggers: [always]\n  selector: fleet=true",
	}, nil)
	mkProject(ctx, t, ns, "default", "", nil)
	const apps = 12
	for i := range apps {
		mkApp(ctx, t, ns, fmt.Sprintf("app-%02d", i), "default", map[string]string{"fleet": "true"}, nil)
	}
	mkApp(ctx, t, ns, "not-in-fleet", "default", nil, nil)

	cfg := baseConfig(ns, prometheus.NewRegistry())
	cfg.Processors = 4
	cfg.LeaderElection = true
	cfg.LeaderElectionID = "notif-concurrency-test"
	startController(t, cfg)

	require.Eventually(t, func() bool { return len(hook.requests()) >= apps }, 30*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)
	for i := range apps {
		assert.Equal(t, 1, hook.count("app", fmt.Sprintf("app-%02d", i)))
	}
	assert.Zero(t, hook.count("app", "not-in-fleet"), "default subscriptions honour their selector")
	lease := &coordinationv1.Lease{}
	require.NoError(t, envClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "notif-concurrency-test"}, lease))
	require.NotNil(t, lease.Spec.HolderIdentity)
	assert.NotEmpty(t, *lease.Spec.HolderIdentity)
}

// NOTIF-001: invalid configuration fails before anything starts.
func TestRunRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	err := Run(context.Background(), Config{Namespace: "patrol"})
	require.ErrorContains(t, err, "rest config is required")
}
