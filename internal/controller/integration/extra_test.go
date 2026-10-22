package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestDeletionConfirmation: a resource with Delete=confirm blocks cascade deletion until the
// deletion-approved annotation is at least as recent as the deletion (APP-062, SYNC-033).
func TestDeletionConfirmation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "confirm", "main",
		withAnnotation(configMap("confirm-cm", nil, nil), v1alpha1.AnnotationKeySyncOptions, "Delete=confirm"))
	a := e.newApp("confirm", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	got := e.waitApp(a, "synced", synced(rev))
	require.Len(t, got.Status.Resources, 1)
	assert.True(t, got.Status.Resources[0].RequiresDeletionConfirmation)

	// An approval older than the deletion does not count.
	e.updateApp(a, func(app *v1alpha1.Application) {
		app.Annotations = map[string]string{v1alpha1.AnnotationKeyDeletionApproved: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	})
	e.deleteApp(a)
	time.Sleep(2 * time.Second)
	cm := mustLive(t, e, "confirm-cm")
	assert.Nil(t, cm.GetDeletionTimestamp(), "deletion waits for confirmation")

	e.updateApp(a, func(app *v1alpha1.Application) {
		app.Annotations[v1alpha1.AnnotationKeyDeletionApproved] = time.Now().Add(time.Second).UTC().Format(time.RFC3339)
	})
	e.waitLive(gvrConfigMap, "confirm-cm", "deleted after approval", gone)
	e.waitAppGone(a)
}

// TestRetryRefreshPicksUpNewRevision: with retry.refresh a failed attempt is retried at the
// latest revision of the branch (SYNC-028).
func TestRetryRefreshPicksUpNewRevision(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "rr-svc"},
		"spec": map[string]any{"type": "NotAType", "ports": []any{map[string]any{"port": int64(80)}}},
	}}
	e.repo.commit(e.repoURL, "rr", "main", bad)
	a := e.newApp("rr")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	op := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Retry = &v1alpha1.RetryStrategy{Limit: 3, Refresh: true, Backoff: &v1alpha1.Backoff{Duration: "3s", Factor: ptr.To[int64](1)}}
	})
	e.waitOp(op, "first attempt failed", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && op.Status.FinishedAt != nil
	})
	fixed := e.repo.commit(e.repoURL, "rr", "main", configMap("rr-cm", nil, nil))
	done := e.waitOp(op, "succeeded at the fixed revision", phaseIs(v1alpha1.OperationSucceeded))
	assert.Equal(t, int64(1), done.Status.RetryCount)
	assert.Equal(t, fixed, done.Status.SyncResult.Revision)
	assert.Equal(t, []string{fixed}, done.Status.Plan.ResolvedRevisions)
	e.waitLive(gvrConfigMap, "rr-cm", "applied", exists)
}

// TestImpersonation: with application.sync.impersonation.enabled the sync runs as the
// project's destination service account: without RBAC it is forbidden, with a Role it
// succeeds; enforced impersonation without a matching account fails (SYNC-093/094).
func TestImpersonation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController(withSettings(map[string]string{"application.sync.impersonation.enabled": "true"}))
	proj := permissiveProject(e.ns, "imp")
	proj.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "*", Namespace: e.dest, DefaultServiceAccount: "deployer"}}
	e.create(proj)
	e.create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest}})
	rev := e.repo.commit(e.repoURL, "imp", "main", configMap("imp-cm", nil, nil))
	a := e.newApp("imp", func(a *v1alpha1.Application) { a.Spec.Project = "imp" })
	e.create(a)
	e.waitApp(a, "compared", outOfSync)

	denied := e.waitOp(e.sync(a), "forbidden", func(op *v1alpha1.Operation) bool { return op.Status.Phase.IsTerminal() })
	assert.NotEqual(t, v1alpha1.OperationSucceeded, denied.Status.Phase, describeOp(denied))
	assert.Contains(t, strings.ToLower(describeOp(denied)), "forbidden")
	_, ok := e.live(gvrConfigMap, "imp-cm")
	assert.False(t, ok)

	e.create(&rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"*"}}},
	})
	e.create(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "deployer"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "deployer", Namespace: e.dest}},
	})
	e.waitOp(e.sync(a), "allowed", phaseIs(v1alpha1.OperationSucceeded))
	cm := mustLive(t, e, "imp-cm")
	assert.NotEmpty(t, cm.GetManagedFields())
	assert.Equal(t, rev, e.waitApp(a, "synced", synced(rev)).Status.Sync.Revision)

	other := e.newApp("imp-none", func(a *v1alpha1.Application) {
		a.Spec.Project = "imp"
		a.Spec.Source.Path = "imp"
		a.Spec.Destination.Namespace = e.ns
	})
	e.create(other)
	e.waitApp(other, "compared", outOfSync)
	failed := e.waitOp(e.sync(other), "no matching service account", func(op *v1alpha1.Operation) bool { return op.Status.Phase.IsTerminal() })
	assert.Equal(t, v1alpha1.OperationError, failed.Status.Phase)
	assert.Contains(t, failed.Status.Message, "failed to find a matching service account to impersonate")
}

// TestTerminateWithoutMatchingServiceAccount: with enforced impersonation, an Operation
// terminated after the project lost its destination service account ends Error without
// cleaning up hooks: termination never falls back to the controller's identity (upstream
// SyncAppState returns before Terminate).
func TestTerminateWithoutMatchingServiceAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	settingsData := map[string]string{"application.sync.impersonation.enabled": "true"}
	ctl := e.startController(withSettings(settingsData))
	proj := permissiveProject(e.ns, "impterm")
	proj.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "*", Namespace: e.dest, DefaultServiceAccount: "deployer"}}
	e.create(proj)
	e.create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest}})
	e.create(&rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps", "pods"}, Verbs: []string{"*"}}},
	})
	e.create(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "deployer", Namespace: e.dest},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "deployer"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "deployer", Namespace: e.dest}},
	})
	e.repo.commit(e.repoURL, "impterm", "main", hookPod("impterm-pre-", v1alpha1.HookTypePreSync), configMap("impterm-cm", nil, nil))
	a := e.newApp("impterm", func(a *v1alpha1.Application) { a.Spec.Project = "impterm" })
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	op := e.sync(a)
	e.waitOp(op, "waiting for the hook", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && strings.Contains(op.Status.Message, "waiting for completion of hook")
	})
	require.Len(t, e.podsWithPrefix("impterm-pre-"), 1)

	// While no controller runs, the project loses its service account and the operation is
	// cancelled, so the next controller's first step is the termination.
	ctl.stop()
	cur := &v1alpha1.AppProject{}
	require.NoError(t, e.c.Get(e.ctx, client.ObjectKeyFromObject(proj), cur))
	cur.Spec.DestinationServiceAccounts = nil
	require.NoError(t, e.c.Update(e.ctx, cur))
	cancelOp(e, op)
	e.startController(withSettings(settingsData))

	done := e.waitOp(op, "terminated with an error", func(op *v1alpha1.Operation) bool { return op.Status.Phase.IsTerminal() })
	assert.Equal(t, v1alpha1.OperationError, done.Status.Phase, describeOp(done))
	assert.True(t, strings.HasPrefix(done.Status.Message, "Operation termination had errors: failed to find a matching service account to impersonate"), done.Status.Message)
	time.Sleep(time.Second)
	assert.Len(t, e.podsWithPrefix("impterm-pre-"), 1, "the hook is not deleted with the controller's identity")
}

// TestOperationSourceOverrideNotPermitted: an Operation created directly (kubectl) with a
// source override is checked against the project's sourceRepos like the Application's own
// source: a repository outside the project fails the Operation with InvalidSpecError and
// nothing is applied; a permitted override syncs.
func TestOperationSourceOverrideNotPermitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	proj := permissiveProject(e.ns, "restricted")
	proj.Spec.SourceRepos = []string{e.repoURL}
	e.create(proj)
	otherRepo := "https://git.example.com/other-" + e.sfx + ".git"
	e.repo.commit(otherRepo, "evil", "main", configMap("evil-cm", nil, nil))
	e.repo.commit(e.repoURL, "ovr", "main", configMap("ovr-cm", nil, nil))
	e.repo.commit(e.repoURL, "ovr-alt", "main", configMap("ovr-alt-cm", nil, nil))
	a := e.newApp("ovr", func(a *v1alpha1.Application) { a.Spec.Project = "restricted" })
	e.create(a)
	e.waitApp(a, "compared", outOfSync)

	denied := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Sync.Source = &v1alpha1.ApplicationSource{RepoURL: otherRepo, Path: "evil", TargetRevision: "main"}
	})
	failed := e.waitOp(denied, "failed", func(op *v1alpha1.Operation) bool { return op.Status.Phase.IsTerminal() })
	assert.Equal(t, v1alpha1.OperationError, failed.Status.Phase)
	assert.Equal(t, "InvalidSpecError: application repo "+otherRepo+" is not permitted in project 'restricted'", failed.Status.Message)
	assert.True(t, metaCondition(failed.Status.Conditions, v1alpha1.OperationConditionCompleted, metav1.ConditionTrue))
	_, ok := e.live(gvrConfigMap, "evil-cm")
	assert.False(t, ok, "nothing from the forbidden repository is applied")

	allowed := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Sync.Source = &v1alpha1.ApplicationSource{RepoURL: e.repoURL, Path: "ovr-alt", TargetRevision: "main"}
	})
	done := e.waitOp(allowed, "permitted override synced", phaseIs(v1alpha1.OperationSucceeded))
	require.NotNil(t, done.Status.SyncResult.Source)
	assert.Equal(t, "ovr-alt", done.Status.SyncResult.Source.Path, "syncResult records the override")
	e.waitLive(gvrConfigMap, "ovr-alt-cm", "applied", exists)
}

// TestSyncMetrics: completed operations are counted by phase and destination requests are
// counted per verb.
func TestSyncMetrics(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev := e.repo.commit(e.repoURL, "met", "main", configMap("met-cm", nil, nil))
	a := e.newApp("met")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))
	assert.Eventually(t, func() bool {
		n, err := testutil.GatherAndCount(ctl.registry, "patrol_app_sync_total")
		return err == nil && n == 1
	}, waitTimeout, pollEvery)
	mfs, err := ctl.registry.Gather()
	require.NoError(t, err)
	found := map[string]bool{}
	for _, mf := range mfs {
		found[mf.GetName()] = true
		if mf.GetName() == "patrol_app_sync_total" {
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "phase" {
						assert.Equal(t, "Succeeded", l.GetValue())
					}
				}
			}
		}
	}
	for _, name := range []string{"patrol_app_reconcile_duration_seconds", "patrol_app_k8s_request_total", "patrol_app_operation_step_duration_seconds"} {
		assert.True(t, found[name], name)
	}
}
