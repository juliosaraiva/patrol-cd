package integration

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

// waitHookJob waits for a Job whose name starts with prefix in namespace of d and returns its
// name.
func waitHookJob(t *testing.T, d *stack.Cluster, namespace, prefix string) string {
	t.Helper()
	var name string
	stack.Eventually(t, 2*time.Minute, "hook Job "+prefix+"*", func() (bool, string) {
		jobs := d.List(t, "batch/v1", "Job", namespace, metav1.ListOptions{})
		var names []string
		for _, j := range jobs {
			names = append(names, j.GetName())
			if strings.HasPrefix(j.GetName(), prefix) && j.GetDeletionTimestamp() == nil {
				name = j.GetName()
				return true, ""
			}
		}
		return false, "jobs: " + strings.Join(names, ",")
	})
	return name
}

func jobsWithPrefix(t *testing.T, d *stack.Cluster, namespace, prefix string) []unstructured.Unstructured {
	t.Helper()
	var out []unstructured.Unstructured
	for _, j := range d.List(t, "batch/v1", "Job", namespace, metav1.ListOptions{}) {
		if strings.HasPrefix(j.GetName(), prefix) {
			out = append(out, j)
		}
	}
	return out
}

func liveData(t *testing.T, d *stack.Cluster, namespace, name, key string) string {
	t.Helper()
	o := d.Get(t, "v1", "ConfigMap", namespace, name)
	v, _, _ := unstructured.NestedString(o.Object, "data", key)
	return v
}

func resourceStatus(app *v1alpha1.Application, kind, name string) *v1alpha1.ResourceStatus {
	for i := range app.Status.Resources {
		r := &app.Status.Resources[i]
		if r.Kind == kind && r.Name == name {
			return r
		}
	}
	return nil
}

// TestGuestbookLifecycle: create through the API → OutOfSync/Missing → manual sync with a
// PreSync hook (the sync waits for the hook, then for the Deployment's health) →
// Synced/Healthy with a history entry → resource tree through the API → drift is detected →
// self-heal restores it → a new git revision is applied by automated sync.
func TestGuestbookLifecycle(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "guestbook")
	repo := s.Git().NewRepo(t, "guestbook")
	rev1 := repo.CommitFiles(t, "guestbook v1", guestbookFiles("hello"))

	created := s.CreateApp(t, s.NewApp("guestbook", repo, "guestbook", d, ns))
	assert.Equal(t, "guestbook", created.Name)

	app := s.WaitApp(t, "guestbook", "OutOfSync and Missing at the first revision", func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync && a.Status.Health.Status == v1alpha1.HealthStatusMissing &&
			a.Status.Sync.Revision == rev1 && len(a.Status.Resources) == 3
	})
	for _, kn := range [][2]string{{"ConfigMap", "guestbook-config"}, {"Service", "guestbook-ui"}, {"Deployment", "guestbook-ui"}} {
		r := resourceStatus(app, kn[0], kn[1])
		require.NotNil(t, r, "%s %s is a managed resource", kn[0], kn[1])
		assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, r.Status)
	}

	// Manual sync: an Operation initiated by the admin.
	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "guestbook"})
	assert.Equal(t, "admin", op.Spec.InitiatedBy.Username)
	assert.False(t, op.Spec.InitiatedBy.Automated)
	hook := waitHookJob(t, d, ns, "db-migrate-")
	s.WaitOperation(t, op.Name, "waiting for the PreSync hook", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "waiting for completion of hook")
	})
	assert.False(t, d.Exists(t, "apps/v1", "Deployment", ns, "guestbook-ui"), "nothing of the Sync phase is applied before the PreSync hook completed")
	stack.CompleteJob(t, d, ns, hook)
	stack.Eventually(t, time.Minute, "Deployment applied after the hook", func() (bool, string) {
		return d.Exists(t, "apps/v1", "Deployment", ns, "guestbook-ui"), ""
	})
	stack.RolloutDeployment(t, d, ns, "guestbook-ui")
	op = s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError)
	require.Equal(t, v1alpha1.OperationSucceeded, op.Status.Phase, stack.DescribeOperation(op))
	require.NotNil(t, op.Status.SyncResult)
	assert.Equal(t, rev1, op.Status.SyncResult.Revision)

	app = s.WaitSyncedHealthy(t, "guestbook", rev1)
	require.Len(t, app.Status.History, 1)
	assert.Equal(t, rev1, app.Status.History[0].Revision)
	assert.Equal(t, op.Name, app.Status.History[0].OperationName)
	assert.Equal(t, "hello", liveData(t, d, ns, "guestbook-config", "GREETING"))
	assert.Len(t, jobsWithPrefix(t, d, ns, "db-migrate-"), 1, "one hook Job per sync")

	// Resource tree through the API (controller tree API behind patrol-server).
	tree := s.Tree(t, "guestbook")
	for _, n := range []struct{ group, kind, name string }{{"", "ConfigMap", "guestbook-config"}, {"", "Service", "guestbook-ui"}, {"apps", "Deployment", "guestbook-ui"}} {
		node := tree.FindNode(n.group, n.kind, ns, n.name)
		if assert.NotNil(t, node, "tree node %s %s", n.kind, n.name) && node.Health != nil {
			assert.Equal(t, v1alpha1.HealthStatusHealthy, node.Health.Status, "%s health in the tree", n.kind)
		}
	}

	// Drift: a live change makes the Application OutOfSync; without automated sync it stays.
	d.MergePatch(t, "v1", "ConfigMap", ns, "guestbook-config", `{"data":{"GREETING":"drifted"}}`)
	s.WaitApp(t, "guestbook", "OutOfSync after a live change", func(a *v1alpha1.Application) bool {
		r := resourceStatus(a, "ConfigMap", "guestbook-config")
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync && r != nil && r.Status == v1alpha1.SyncStatusCodeOutOfSync
	})
	stack.Consistently(t, 3*time.Second, "drift without automated sync", func() (bool, string) {
		v := liveData(t, d, ns, "guestbook-config", "GREETING")
		return v == "drifted", "GREETING=" + v
	})

	// Self-heal restores the live state with a partial, automated Operation (no hooks, no
	// history entry).
	s.UpdateApp(t, "guestbook", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{SelfHeal: stack.Ptr(true), Prune: stack.Ptr(true)}}
	})
	stack.Eventually(t, 2*time.Minute, "self-heal restores the ConfigMap", func() (bool, string) {
		v := liveData(t, d, ns, "guestbook-config", "GREETING")
		return v == "hello", "GREETING=" + v
	})
	app = s.WaitSyncedHealthy(t, "guestbook", rev1)
	assert.Len(t, app.Status.History, 1, "self-heal records no history")
	ops := s.Operations(t, "guestbook")
	require.GreaterOrEqual(t, len(ops), 2)
	heal := ops[len(ops)-1]
	assert.True(t, heal.Spec.InitiatedBy.Automated, "self-heal is automated")
	assert.Len(t, jobsWithPrefix(t, d, ns, "db-migrate-"), 1, "self-heal runs no hooks")

	// A new git revision, announced by a signed GitHub push webhook to patrol-server, is
	// applied by automated sync (hooks run again: the workload simulation completes the new
	// hook Job).
	s.SimulateWorkloads(d, ns)
	rev2 := repo.CommitFiles(t, "greeting v2", map[string]string{"guestbook/configmap.yaml": guestbookFiles("hola")["guestbook/configmap.yaml"]})
	require.Equal(t, http.StatusOK, s.GitHubPush(t, repo, "main", rev2, "guestbook/configmap.yaml"), "webhook accepted")
	app = s.WaitSyncedHealthy(t, "guestbook", rev2)
	assert.Equal(t, "hola", liveData(t, d, ns, "guestbook-config", "GREETING"))
	require.Len(t, app.Status.History, 2)
	assert.Equal(t, rev2, app.Status.History[1].Revision)
	assert.True(t, app.Status.History[1].InitiatedBy.Automated)
}

// syncAndWait requests a sync through the API and waits for a terminal phase.
func syncAndWait(t *testing.T, s *stack.Stack, req *apiclient.ApplicationSyncRequest) *v1alpha1.Operation {
	t.Helper()
	op := s.Sync(t, req)
	return s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError,
		v1alpha1.OperationCancelled, v1alpha1.OperationRejected)
}

// requireSucceeded fails unless op Succeeded.
func requireSucceeded(t *testing.T, op *v1alpha1.Operation) {
	t.Helper()
	require.Equal(t, v1alpha1.OperationSucceeded, op.Status.Phase, stack.DescribeOperation(op))
}

func result(op *v1alpha1.Operation, kind, name string) *v1alpha1.ResourceResult {
	if op.Status.SyncResult == nil {
		return nil
	}
	for i := range op.Status.SyncResult.Resources {
		r := &op.Status.SyncResult.Resources[i]
		if r.Kind == kind && r.Name == name {
			return r
		}
	}
	return nil
}

func resultIndex(op *v1alpha1.Operation, kind, name string) int {
	if op.Status.SyncResult == nil {
		return -1
	}
	for i, r := range op.Status.SyncResult.Resources {
		if r.Kind == kind && r.Name == name {
			return i
		}
	}
	return -1
}

// TestPruneOnlyTrackedResources: a manifest removed from git is pruned only by a sync with
// prune, never an unmanaged live object with the same labels, and never an object tracked by
// another Application (which reports SharedResourceWarning while both claim it).
func TestPruneOnlyTrackedResources(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "prune")
	repo := s.Git().NewRepo(t, "prune")
	repo.CommitFiles(t, "two apps", map[string]string{
		"app-a/keep.yaml":      configMap("keep", map[string]string{"k": "v"}, nil),
		"app-a/remove-me.yaml": configMap("remove-me", map[string]string{"k": "v"}, nil),
		"app-a/shared.yaml":    configMap("shared", map[string]string{"owner": "a"}, nil),
		"app-b/shared.yaml":    configMap("shared", map[string]string{"owner": "b"}, nil),
	})
	// An unmanaged object with the same labels as the managed ones.
	d.Apply(t, ns, configMap("unmanaged", map[string]string{"x": "y"}, nil))

	s.CreateApp(t, s.NewApp("app-a", repo, "app-a", d, ns))
	s.WaitSyncStatus(t, "app-a", v1alpha1.SyncStatusCodeOutOfSync, "")
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "app-a"}))
	s.WaitSyncedHealthy(t, "app-a", "")

	// app-b claims a resource app-a tracks: SharedResourceWarning.
	s.CreateApp(t, s.NewApp("app-b", repo, "app-b", d, ns))
	b := s.WaitApp(t, "app-b", "SharedResourceWarning", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionSharedResourceWarning, "shared")
	})
	assert.Contains(t, stack.Condition(b, v1alpha1.ApplicationConditionSharedResourceWarning).Message, "app-a")
	// Syncing app-b takes the resource over; now app-a reports the warning.
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "app-b"}))
	assert.Equal(t, "b", liveData(t, d, ns, "shared", "owner"))
	s.Refresh(t, "app-a", false)
	s.WaitApp(t, "app-a", "SharedResourceWarning after app-b took the resource over", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionSharedResourceWarning, "shared")
	})

	// Remove two manifests from app-a: remove-me (tracked by app-a) and shared (tracked by
	// app-b now).
	repo.Remove(t, "app-a/remove-me.yaml", "app-a/shared.yaml")
	rev2 := repo.Commit(t, "drop two resources from app-a")
	s.Refresh(t, "app-a", false)
	a := s.WaitApp(t, "app-a", "remove-me requires pruning", func(a *v1alpha1.Application) bool {
		r := resourceStatus(a, "ConfigMap", "remove-me")
		return a.Status.Sync.Revision == rev2 && a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync && r != nil && r.RequiresPruning
	})
	assert.Nil(t, resourceStatus(a, "ConfigMap", "shared"), "an object tracked by another Application is not a resource of app-a")
	assert.Nil(t, resourceStatus(a, "ConfigMap", "unmanaged"), "an untracked object is not a resource of app-a")
	assert.False(t, stack.HasCondition(a, v1alpha1.ApplicationConditionSharedResourceWarning, ""), "app-a no longer claims the shared resource")

	// Automated sync without prune leaves the object (pruning is opt-in).
	s.UpdateApp(t, "app-a", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}
	})
	stack.Consistently(t, 4*time.Second, "automated sync without prune does not delete", func() (bool, string) {
		return d.Exists(t, "v1", "ConfigMap", ns, "remove-me"), "remove-me deleted"
	})
	s.UpdateApp(t, "app-a", func(a *v1alpha1.Application) { a.Spec.SyncPolicy = nil })

	// A manual sync without prune leaves it too.
	op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "app-a"})
	requireSucceeded(t, op)
	if r := result(op, "ConfigMap", "remove-me"); assert.NotNil(t, r, stack.DescribeOperation(op)) {
		assert.Equal(t, v1alpha1.ResultCodePruneSkipped, r.Status)
	}
	assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "remove-me"))

	// A sync with prune deletes exactly the tracked object.
	op = syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "app-a", Prune: true})
	requireSucceeded(t, op)
	if r := result(op, "ConfigMap", "remove-me"); assert.NotNil(t, r, stack.DescribeOperation(op)) {
		assert.Equal(t, v1alpha1.ResultCodePruned, r.Status)
	}
	stack.Eventually(t, time.Minute, "remove-me pruned", func() (bool, string) {
		return !d.Exists(t, "v1", "ConfigMap", ns, "remove-me"), ""
	})
	assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "keep"))
	assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "unmanaged"), "untracked objects are never pruned")
	assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "shared"), "objects tracked by another Application are never pruned")
	assert.Equal(t, "b", liveData(t, d, ns, "shared", "owner"))
	s.WaitSyncedHealthy(t, "app-a", rev2)
	s.WaitSyncedHealthy(t, "app-b", "")
}

// TestSyncOptions exercises the sync options end to end: CreateNamespace with
// managedNamespaceMetadata, ServerSideApply, Replace, Force, PruneLast, ApplyOutOfSyncOnly and
// Validate=false.
func TestSyncOptions(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	repo := s.Git().NewRepo(t, "options")

	t.Run("CreateNamespace with managed namespace metadata", func(t *testing.T) {
		ns := "created-" + s.ID
		s.WatchNamespace(d, ns)
		repo.CommitFiles(t, "create-namespace", map[string]string{"create-ns/cm.yaml": configMap("in-new-ns", map[string]string{"a": "1"}, nil)})
		app := s.NewApp("create-ns", repo, "create-ns", d, ns)
		app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
			SyncOptions:              v1alpha1.SyncOptions{"CreateNamespace=true"},
			ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "blue"}, Annotations: map[string]string{"owner": "patrol"}},
		}
		s.CreateApp(t, app)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "create-ns"}))
		nsObj := d.Get(t, "v1", "Namespace", "", ns)
		assert.Equal(t, "blue", nsObj.GetLabels()["team"])
		assert.Equal(t, "patrol", nsObj.GetAnnotations()["owner"])
		assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "in-new-ns"))
		s.WaitSyncedHealthy(t, "create-ns", "")
	})

	ns := s.NewNamespace(t, d, "options")

	t.Run("ServerSideApply", func(t *testing.T) {
		repo.CommitFiles(t, "ssa", map[string]string{"ssa/cm.yaml": configMap("ssa", map[string]string{"a": "1"}, nil)})
		app := s.NewApp("ssa", repo, "ssa", d, ns)
		app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"ServerSideApply=true"}}
		s.CreateApp(t, app)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "ssa"}))
		live := d.Get(t, "v1", "ConfigMap", ns, "ssa")
		var applied bool
		for _, mf := range live.GetManagedFields() {
			if mf.Manager == "patrol-controller" && mf.Operation == metav1.ManagedFieldsOperationApply {
				applied = true
			}
		}
		assert.True(t, applied, "the controller owns the fields through server-side apply: %+v", live.GetManagedFields())
		assert.NotContains(t, live.GetAnnotations(), "kubectl.kubernetes.io/last-applied-configuration")
		s.WaitSyncedHealthy(t, "ssa", "")
	})

	t.Run("Replace", func(t *testing.T) {
		repo.CommitFiles(t, "replace v1", map[string]string{"replace/cm.yaml": configMap("replaced", map[string]string{"a": "1"}, nil)})
		s.CreateApp(t, s.NewApp("replace", repo, "replace", d, ns))
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "replace"}))
		// Another writer adds a label; a client-side apply would keep it, a replace drops it.
		d.MergePatch(t, "v1", "ConfigMap", ns, "replaced", `{"metadata":{"labels":{"added-by":"someone-else"}}}`)
		repo.CommitFiles(t, "replace v2", map[string]string{"replace/cm.yaml": configMap("replaced", map[string]string{"a": "2"}, nil)})
		s.Refresh(t, "replace", false)
		op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "replace", SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"Replace=true"}}})
		requireSucceeded(t, op)
		live := d.Get(t, "v1", "ConfigMap", ns, "replaced")
		assert.Equal(t, "2", liveData(t, d, ns, "replaced", "a"))
		assert.NotContains(t, live.GetLabels(), "added-by", "replace writes the whole object")
	})

	t.Run("Force", func(t *testing.T) {
		immutable := func(v string, annotations map[string]string) string {
			return configMap("frozen", map[string]string{"a": v}, annotations) + "immutable: true\n"
		}
		repo.CommitFiles(t, "force v1", map[string]string{"force/cm.yaml": immutable("1", nil)})
		s.CreateApp(t, s.NewApp("force", repo, "force", d, ns))
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "force"}))
		uid := d.Get(t, "v1", "ConfigMap", ns, "frozen").GetUID()
		repo.CommitFiles(t, "force v2", map[string]string{"force/cm.yaml": immutable("2", nil)})
		s.Refresh(t, "force", false)
		// Without force the immutable field cannot change.
		op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "force"})
		require.Equal(t, v1alpha1.OperationFailed, op.Status.Phase, stack.DescribeOperation(op))
		assert.Contains(t, stack.DescribeOperation(op), "immutable")
		// Forced sync (strategy force, `patrol app sync --force`) with Replace=true deletes and
		// re-creates the object.
		op = syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "force",
			Strategy:    &v1alpha1.SyncStrategy{Hook: &v1alpha1.SyncStrategyHook{SyncStrategyApply: v1alpha1.SyncStrategyApply{Force: true}}},
			SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"Replace=true"}}})
		requireSucceeded(t, op)
		uid2 := d.Get(t, "v1", "ConfigMap", ns, "frozen").GetUID()
		assert.NotEqual(t, uid, uid2, "re-created")
		assert.Equal(t, "2", liveData(t, d, ns, "frozen", "a"))
		// The resource annotation Force=true,Replace=true does the same on a plain sync.
		repo.CommitFiles(t, "force v3", map[string]string{"force/cm.yaml": immutable("3", map[string]string{v1alpha1.AnnotationKeySyncOptions: "Force=true,Replace=true"})})
		s.Refresh(t, "force", false)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "force"}))
		assert.NotEqual(t, uid2, d.Get(t, "v1", "ConfigMap", ns, "frozen").GetUID(), "re-created")
		assert.Equal(t, "3", liveData(t, d, ns, "frozen", "a"))
	})

	t.Run("PruneLast", func(t *testing.T) {
		wave := func(w string) map[string]string { return map[string]string{v1alpha1.AnnotationKeySyncWave: w} }
		repo.CommitFiles(t, "prune-last v1", map[string]string{
			"prune-last/old.yaml":   configMap("pl-old", nil, wave("0")),
			"prune-last/first.yaml": configMap("pl-first", nil, wave("0")),
		})
		app := s.NewApp("prune-last", repo, "prune-last", d, ns)
		app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"PruneLast=true"}}
		s.CreateApp(t, app)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "prune-last"}))
		repo.Remove(t, "prune-last/old.yaml")
		repo.CommitFiles(t, "prune-last v2", map[string]string{"prune-last/late.yaml": configMap("pl-late", nil, wave("3"))})
		s.Refresh(t, "prune-last", false)
		op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "prune-last", Prune: true})
		requireSucceeded(t, op)
		pruned, late := resultIndex(op, "ConfigMap", "pl-old"), resultIndex(op, "ConfigMap", "pl-late")
		require.GreaterOrEqual(t, pruned, 0, stack.DescribeOperation(op))
		require.GreaterOrEqual(t, late, 0, stack.DescribeOperation(op))
		assert.Equal(t, v1alpha1.ResultCodePruned, op.Status.SyncResult.Resources[pruned].Status)
		assert.Greater(t, pruned, late, "the prune runs after the last wave: %s", stack.DescribeOperation(op))
		stack.Eventually(t, time.Minute, "pl-old deleted", func() (bool, string) {
			return !d.Exists(t, "v1", "ConfigMap", ns, "pl-old"), ""
		})
	})

	t.Run("ApplyOutOfSyncOnly", func(t *testing.T) {
		repo.CommitFiles(t, "oos v1", map[string]string{
			"oos/x.yaml": configMap("oos-x", map[string]string{"v": "1"}, nil),
			"oos/y.yaml": configMap("oos-y", map[string]string{"v": "1"}, nil),
		})
		app := s.NewApp("oos", repo, "oos", d, ns)
		app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"ApplyOutOfSyncOnly=true"}}
		s.CreateApp(t, app)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "oos"}))
		s.WaitSyncedHealthy(t, "oos", "")
		repo.CommitFiles(t, "oos v2", map[string]string{"oos/x.yaml": configMap("oos-x", map[string]string{"v": "2"}, nil)})
		s.Refresh(t, "oos", false)
		s.WaitApp(t, "oos", "only oos-x OutOfSync", func(a *v1alpha1.Application) bool {
			x, y := resourceStatus(a, "ConfigMap", "oos-x"), resourceStatus(a, "ConfigMap", "oos-y")
			return x != nil && y != nil && x.Status == v1alpha1.SyncStatusCodeOutOfSync && y.Status == v1alpha1.SyncStatusCodeSynced
		})
		op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "oos"})
		requireSucceeded(t, op)
		assert.NotNil(t, result(op, "ConfigMap", "oos-x"), "the out-of-sync resource is applied")
		assert.Nil(t, result(op, "ConfigMap", "oos-y"), "the synced resource is skipped: %s", stack.DescribeOperation(op))
		assert.Equal(t, "2", liveData(t, d, ns, "oos-x", "v"))
	})

	t.Run("Validate=false", func(t *testing.T) {
		// "spec" is not a ConfigMap field: strict field validation rejects it.
		repo.CommitFiles(t, "validate", map[string]string{"validate/cm.yaml": configMap("lenient", map[string]string{"a": "1"}, nil) + "spec:\n  unknown: true\n"})
		s.CreateApp(t, s.NewApp("validate", repo, "validate", d, ns))
		op := syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "validate"})
		require.Equal(t, v1alpha1.OperationFailed, op.Status.Phase, stack.DescribeOperation(op))
		assert.Contains(t, stack.DescribeOperation(op), "spec")
		assert.False(t, d.Exists(t, "v1", "ConfigMap", ns, "lenient"), "the dry run stops the sync before any change")
		op = syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "validate", SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"Validate=false"}}})
		requireSucceeded(t, op)
		assert.Equal(t, "1", liveData(t, d, ns, "lenient", "a"))
	})
}

func waves(n string) map[string]string { return map[string]string{v1alpha1.AnnotationKeySyncWave: n} }

// gateJob is a Job in a sync wave whose health the test controls (HoldAnnotation).
func gateJob(name, wave string) string {
	return `apiVersion: batch/v1
kind: Job
metadata:
  name: ` + name + `
  annotations:
    ` + v1alpha1.AnnotationKeySyncWave + `: "` + wave + `"
    ` + stack.HoldAnnotation + `: "true"
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: gate
          image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
          command: ["true"]
`
}

func createdAt(t *testing.T, d *stack.Cluster, apiVersion, kind, ns, name string) time.Time {
	t.Helper()
	return d.Get(t, apiVersion, kind, ns, name).GetCreationTimestamp().Time
}

// TestSyncWavesOrdering: waves are applied in order, a wave waits for the health of the
// previous one, and creation timestamps follow the waves (2s wave delay).
func TestSyncWavesOrdering(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "waves")
	repo := s.Git().NewRepo(t, "waves")
	repo.CommitFiles(t, "waves", map[string]string{
		"waves/a.yaml": configMap("wave-minus-1", nil, waves("-1")),
		"waves/b.yaml": configMap("wave-0", nil, nil),
		"waves/c.yaml": gateJob("wave-1-gate", "1"),
		"waves/d.yaml": configMap("wave-2", nil, waves("2")),
	})
	s.CreateApp(t, s.NewApp("waves", repo, "waves", d, ns))
	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "waves"})
	stack.Eventually(t, time.Minute, "wave 1 applied", func() (bool, string) {
		return d.Exists(t, "batch/v1", "Job", ns, "wave-1-gate"), ""
	})
	s.WaitOperation(t, op.Name, "waiting for the health of wave 1", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "wave-1-gate")
	})
	stack.Consistently(t, 3*time.Second, "wave 2 waits for wave 1", func() (bool, string) {
		return !d.Exists(t, "v1", "ConfigMap", ns, "wave-2"), "wave-2 exists"
	})
	stack.CompleteJob(t, d, ns, "wave-1-gate")
	requireSucceeded(t, s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError))

	tm1 := createdAt(t, d, "v1", "ConfigMap", ns, "wave-minus-1")
	t0 := createdAt(t, d, "v1", "ConfigMap", ns, "wave-0")
	t1 := createdAt(t, d, "batch/v1", "Job", ns, "wave-1-gate")
	t2 := createdAt(t, d, "v1", "ConfigMap", ns, "wave-2")
	assert.True(t, tm1.Before(t0), "wave -1 (%s) before wave 0 (%s)", tm1, t0)
	assert.True(t, t0.Before(t1), "wave 0 (%s) before wave 1 (%s)", t0, t1)
	assert.True(t, t1.Before(t2), "wave 1 (%s) before wave 2 (%s)", t1, t2)
	s.WaitSyncedHealthy(t, "waves", "")
}

// TestFailedSyncRetryAndRecovery: a manifest the API server rejects fails the automated
// Operation after its retries (with backoff), the Application reports SyncError and the same
// revision is not retried; fixing git recovers automatically.
func TestFailedSyncRetryAndRecovery(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "retry")
	repo := s.Git().NewRepo(t, "retry")
	svc := func(port int) string {
		return "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\nspec:\n  ports:\n    - port: " + itoa(port) + "\n  selector:\n    app: web\n"
	}
	rev1 := repo.CommitFiles(t, "good", map[string]string{"retry/svc.yaml": svc(80), "retry/cm.yaml": configMap("settings", map[string]string{"v": "1"}, nil)})
	app := s.NewApp("retry", repo, "retry", d, ns)
	app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
		Automated: &v1alpha1.SyncPolicyAutomated{Prune: stack.Ptr(true)},
		Retry:     &v1alpha1.RetryStrategy{Limit: 2, Backoff: &v1alpha1.Backoff{Duration: "1s", Factor: stack.Ptr(int64(2)), MaxDuration: "5s"}},
	}
	s.CreateApp(t, app)
	s.WaitSyncedHealthy(t, "retry", rev1)

	rev2 := repo.CommitFiles(t, "invalid port", map[string]string{"retry/svc.yaml": svc(70000)})
	s.Refresh(t, "retry", false)
	var failed *v1alpha1.Operation
	sawRetry := false
	stack.Eventually(t, 2*time.Minute, "automated operation for the invalid revision fails after its retries", func() (bool, string) {
		ops := s.Operations(t, "retry")
		if len(ops) == 0 {
			return false, "no operations"
		}
		last := ops[len(ops)-1]
		if last.Spec.Sync == nil || last.Spec.Sync.Revision != rev2 {
			return false, stack.DescribeOperation(&last)
		}
		if strings.Contains(last.Status.Message, "Retrying attempt #") {
			sawRetry = true
		}
		failed = &last
		return last.Status.Phase == v1alpha1.OperationFailed, stack.DescribeOperation(&last)
	})
	assert.True(t, failed.Spec.InitiatedBy.Automated)
	assert.Equal(t, int64(2), failed.Status.RetryCount, stack.DescribeOperation(failed))
	assert.Contains(t, failed.Status.Message, "(retried 2 times)")
	assert.Contains(t, failed.Status.Message, "one or more objects failed to apply")
	assert.Contains(t, stack.DescribeOperation(failed), "70000", "the API server's reason is in the resource result")
	assert.True(t, sawRetry, "the retry state was visible while backing off")
	require.NotNil(t, failed.Status.StartedAt)
	require.NotNil(t, failed.Status.FinishedAt)
	assert.GreaterOrEqual(t, failed.Status.FinishedAt.Sub(failed.Status.StartedAt.Time), 3*time.Second, "two retries with backoff 1s×2^n")
	a := s.WaitApp(t, "retry", "SyncError for the failed revision", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionSyncError, "Failed last sync attempt")
	})
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, a.Status.Sync.Status)
	n := len(s.Operations(t, "retry"))
	stack.Consistently(t, 5*time.Second, "the failed revision is not attempted again", func() (bool, string) {
		got := len(s.Operations(t, "retry"))
		return got == n, itoa(got) + " operations"
	})

	// Fix git: automated sync recovers and clears SyncError.
	rev3 := repo.CommitFiles(t, "valid port", map[string]string{"retry/svc.yaml": svc(8080)})
	s.Refresh(t, "retry", false)
	a = s.WaitSyncedHealthy(t, "retry", rev3)
	assert.Nil(t, stack.Condition(a, v1alpha1.ApplicationConditionSyncError), "SyncError cleared: %s", stack.DescribeApp(a))
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestTerminateOperation: a running Operation (held by its PreSync hook) blocks other syncs,
// terminates through the API (Cancelled, running hook cleaned up, nothing applied), and the
// next sync is admitted.
func TestTerminateOperation(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "terminate")
	repo := s.Git().NewRepo(t, "terminate")
	hook := `apiVersion: batch/v1
kind: Job
metadata:
  generateName: slow-hook-
  annotations:
    ` + v1alpha1.AnnotationKeyHook + `: PreSync
    ` + stack.HoldAnnotation + `: "true"
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: slow
          image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
          command: ["sleep", "3600"]
`
	repo.CommitFiles(t, "hooked app", map[string]string{"terminate/hook.yaml": hook, "terminate/cm.yaml": configMap("after-hook", map[string]string{"a": "1"}, nil)})
	s.CreateApp(t, s.NewApp("terminate", repo, "terminate", d, ns))
	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "terminate"})
	job := waitHookJob(t, d, ns, "slow-hook-")
	s.WaitOperation(t, op.Name, "running, waiting for the hook", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "waiting for completion of hook")
	})

	// One operation at a time.
	_, err := s.API(t).SyncApplication(t.Context(), &apiclient.ApplicationSyncRequest{Name: "terminate"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another operation is already in progress")

	resp, err := s.API(t).TerminateApplicationOperation(t.Context(), &apiclient.ApplicationRef{Name: "terminate"})
	require.NoError(t, err)
	if resp.Operation != nil {
		assert.Equal(t, op.Name, resp.Operation.Name)
	}
	cancelled := s.WaitOperationPhase(t, op.Name, v1alpha1.OperationCancelled, v1alpha1.OperationError, v1alpha1.OperationFailed, v1alpha1.OperationSucceeded)
	require.Equal(t, v1alpha1.OperationCancelled, cancelled.Status.Phase, stack.DescribeOperation(cancelled))
	assert.Contains(t, cancelled.Status.Message, "Operation terminated")
	stack.Eventually(t, time.Minute, "running hook cleaned up", func() (bool, string) {
		o, err := d.TryGet(t.Context(), "batch/v1", "Job", ns, job)
		if err != nil {
			return false, err.Error()
		}
		return o == nil || o.GetDeletionTimestamp() != nil, "hook still present"
	})
	assert.False(t, d.Exists(t, "v1", "ConfigMap", ns, "after-hook"), "nothing after the hook was applied")
	s.WaitApp(t, "terminate", "operation state Cancelled", func(a *v1alpha1.Application) bool {
		return a.Status.OperationState != nil && a.Status.OperationState.OperationName == op.Name && a.Status.OperationState.Phase == v1alpha1.OperationCancelled
	})

	// The slot is free again.
	op2 := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "terminate"})
	var job2 string
	stack.Eventually(t, time.Minute, "a new hook for the new operation", func() (bool, string) {
		for _, j := range jobsWithPrefix(t, d, ns, "slow-hook-") {
			if j.GetName() != job && j.GetDeletionTimestamp() == nil {
				job2 = j.GetName()
				return true, ""
			}
		}
		return false, ""
	})
	stack.CompleteJob(t, d, ns, job2)
	requireSucceeded(t, s.WaitOperationPhase(t, op2.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError))
	assert.True(t, d.Exists(t, "v1", "ConfigMap", ns, "after-hook"))
}

// TestRollbackAndHistoryLimit: revisionHistoryLimit bounds history and retained Operations;
// rollback re-applies a history entry (recorded as a new entry), is refused for unknown ids
// and while automated sync is enabled.
func TestRollbackAndHistoryLimit(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "rollback")
	repo := s.Git().NewRepo(t, "rollback")
	revs := make([]string, 0, 3)
	app := s.NewApp("rollback", repo, "rollback", d, ns)
	app.Spec.RevisionHistoryLimit = stack.Ptr(int64(2))
	for i, v := range []string{"one", "two", "three"} {
		revs = append(revs, repo.CommitFiles(t, "version "+v, map[string]string{"rollback/cm.yaml": configMap("versioned", map[string]string{"version": v}, nil)}))
		if i == 0 {
			s.CreateApp(t, app)
		}
		s.Refresh(t, "rollback", false)
		s.WaitSyncStatus(t, "rollback", v1alpha1.SyncStatusCodeOutOfSync, revs[i])
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "rollback"}))
		s.WaitSyncedHealthy(t, "rollback", revs[i])
	}
	a := s.GetApp(t, "rollback")
	require.Len(t, a.Status.History, 2, "revisionHistoryLimit=2")
	// Ids start at 0 (upstream); the oldest entry (id 0) was truncated.
	assert.Equal(t, []int64{1, 2}, []int64{a.Status.History[0].ID, a.Status.History[1].ID})
	assert.Equal(t, revs[1], a.Status.History[0].Revision)
	assert.Equal(t, revs[2], a.Status.History[1].Revision)
	stack.Eventually(t, time.Minute, "terminal Operations retained up to the history limit", func() (bool, string) {
		n := len(s.Operations(t, "rollback"))
		return n <= 2, itoa(n) + " operations"
	})

	// Unknown (truncated) id.
	_, err := s.API(t).RollbackApplication(t.Context(), &apiclient.ApplicationRollbackRequest{Name: "rollback", ID: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not have deployment with id 0")

	// Roll back to "two".
	resp, err := s.API(t).RollbackApplication(t.Context(), &apiclient.ApplicationRollbackRequest{Name: "rollback", ID: 1})
	require.NoError(t, err)
	op := s.WaitOperationPhase(t, resp.Operation.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError)
	requireSucceeded(t, op)
	assert.Equal(t, "two", liveData(t, d, ns, "versioned", "version"))
	a = s.WaitApp(t, "rollback", "history entry of the rollback, OutOfSync against main", func(a *v1alpha1.Application) bool {
		n := len(a.Status.History)
		return n == 2 && a.Status.History[n-1].ID == 3 && a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync
	})
	assert.Equal(t, revs[1], a.Status.History[1].Revision)

	// Not while automated sync is enabled.
	s.UpdateApp(t, "rollback", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}
	})
	_, err = s.API(t).RollbackApplication(t.Context(), &apiclient.ApplicationRollbackRequest{Name: "rollback", ID: 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback cannot be initiated when auto-sync is enabled")
	s.WaitSyncedHealthy(t, "rollback", revs[2])
	assert.Equal(t, "three", liveData(t, d, ns, "versioned", "version"), "automated sync returned to main")
}

// TestDeletion: a cascading delete through the API removes the managed resources before the
// Application (resources finalizer); a non-cascading delete leaves them.
func TestDeletion(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	repo := s.Git().NewRepo(t, "deletion")
	repo.CommitFiles(t, "apps", map[string]string{
		"cascade/a.yaml": configMap("cascade-a", map[string]string{"k": "v"}, waves("0")),
		"cascade/b.yaml": configMap("cascade-b", map[string]string{"k": "v"}, waves("1")),
		"keep/a.yaml":    configMap("keep-a", map[string]string{"k": "v"}, nil),
	})
	nsC := s.NewNamespace(t, d, "cascade")
	nsK := s.NewNamespace(t, d, "keep")
	for _, a := range []struct{ name, ns string }{{"cascade", nsC}, {"keep", nsK}} {
		s.CreateApp(t, s.NewApp(a.name, repo, a.name, d, a.ns))
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: a.name}))
		s.WaitSyncedHealthy(t, a.name, "")
	}

	require.NoError(t, s.API(t).DeleteApplication(t.Context(), &apiclient.ApplicationDeleteRequest{Name: "cascade", Cascade: stack.Ptr(true)}))
	stack.Eventually(t, 2*time.Minute, "cascading deletion completes", func() (bool, string) {
		if a, ok := s.LookupApp(t, "cascade"); ok { // one read: it may go between two
			return false, "finalizers=" + strings.Join(a.Finalizers, ",") + " " + stack.DescribeApp(a)
		}
		return true, ""
	})
	assert.False(t, d.Exists(t, "v1", "ConfigMap", nsC, "cascade-a"))
	assert.False(t, d.Exists(t, "v1", "ConfigMap", nsC, "cascade-b"))

	require.NoError(t, s.API(t).DeleteApplication(t.Context(), &apiclient.ApplicationDeleteRequest{Name: "keep", Cascade: stack.Ptr(false)}))
	stack.Eventually(t, time.Minute, "non-cascading deletion completes", func() (bool, string) {
		return s.AppGone(t, "keep"), ""
	})
	stack.Consistently(t, 3*time.Second, "resources of a non-cascading deletion stay", func() (bool, string) {
		return d.Exists(t, "v1", "ConfigMap", nsK, "keep-a"), "keep-a deleted"
	})
}

// TestObjectOfMissingApplicationIsOrphan: the real controller counts an object tracked by an
// Application that does not exist as an orphan (DIFF-078), and not the object of one that
// exists. Its event is attributed to the missing Application, so it takes a refresh to count.
func TestObjectOfMissingApplicationIsOrphan(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	repo := s.Git().NewRepo(t, "orphans")
	repo.CommitFiles(t, "apps", map[string]string{
		"monitor/a.yaml": configMap("monitor-a", map[string]string{"k": "v"}, nil),
		"peer/a.yaml":    configMap("peer-a", map[string]string{"k": "v"}, nil),
	})
	ns := s.NewNamespace(t, d, "orphans")
	warn := true
	_, err := s.API(t).CreateProject(t.Context(), &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "orphans"},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:       []string{"*"},
			Destinations:      []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Warn: &warn},
		},
	}, false)
	require.NoError(t, err)
	monitor := s.NewApp("monitor", repo, "monitor", d, ns)
	monitor.Spec.Project = "orphans"
	for _, a := range []*v1alpha1.Application{monitor, s.NewApp("peer", repo, "peer", d, ns)} {
		s.CreateApp(t, a)
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: a.Name}))
		s.WaitSyncedHealthy(t, a.Name, "")
	}
	got := s.RefreshUntil(t, "monitor", "the object of peer is no orphan", func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced
	})
	require.Nil(t, stack.Condition(got, v1alpha1.ApplicationConditionOrphanedResourceWarning), stack.DescribeApp(got))

	d.Apply(t, ns, configMap("ghost", nil, map[string]string{
		v1alpha1.AnnotationKeyInstallationID: s.InstallationID,
		v1alpha1.AnnotationKeyTrackingID:     "gone-app:/ConfigMap:" + ns + "/ghost",
	}))
	s.RefreshUntil(t, "monitor", "the object of the missing Application counted", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionOrphanedResourceWarning, "Application has 1 orphaned resources")
	})
}

// TestOnboardingThroughAPI registers a destination cluster (bearer token) and a private git
// repository (basic auth) through the API and creates a validated Application for them right
// away, as a script running `patrol cluster add`, `patrol repo add` and `patrol app create`
// would.
func TestOnboardingThroughAPI(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1, UnregisteredDedicated: true})
	d := s.Destination(2)
	creds := d.Credentials()
	api := s.API(t)

	_, err := api.CreateCluster(t.Context(), &apiclient.ClusterInput{
		Server: d.Server, Name: "onboarded",
		Config: apiclient.ClusterConfigInput{
			BearerToken:     string(creds[v1alpha1.ClusterSecretKeyBearerToken]),
			TLSClientConfig: apiclient.ClusterTLSClientConfigInput{CAData: creds[v1alpha1.ClusterSecretKeyCAData]},
		},
	}, false)
	require.NoError(t, err, "register the cluster")

	gs := s.NewGitServer(t)
	repo := gs.NewRepo(t, "private")
	rev := repo.CommitFiles(t, "app", map[string]string{"app/cm.yaml": configMap("onboarded", map[string]string{"k": "v"}, nil)})
	_, err = api.CreateRepository(t.Context(), &apiclient.RepositoryCreateRequest{Body: apiclient.RepositoryWriteRequest{
		Spec:        v1alpha1.RepositorySpec{URL: repo.URL, Type: v1alpha1.RepositoryTypeGit},
		Credentials: &apiclient.RepositoryCredentials{Username: gs.Username, Password: gs.Password},
	}})
	require.NoError(t, err, "register the repository")

	ns := s.NewNamespace(t, d, "onboard")
	app := s.NewApp("onboard", repo, "app", d, ns)
	app.Spec.Destination = v1alpha1.ApplicationDestination{Name: "onboarded", Namespace: ns}
	s.CreateApp(t, app)
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "onboard"}))
	s.WaitSyncedHealthy(t, "onboard", rev)
	assert.Equal(t, "v", liveData(t, d, ns, "onboarded", "k"))
}
