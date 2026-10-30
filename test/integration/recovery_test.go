package integration

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

// heldHook is a PreSync hook Job (generateName) whose completion the test controls.
func heldHook(prefix string) string {
	return `apiVersion: batch/v1
kind: Job
metadata:
  generateName: ` + prefix + `
  annotations:
    ` + v1alpha1.AnnotationKeyHook + `: PreSync
    ` + stack.HoldAnnotation + `: "true"
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: hook
          image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
          command: ["true"]
`
}

// multiWaveFiles: a held PreSync hook, wave 0 and 2 ConfigMaps and a held Job in wave 1.
func multiWaveFiles(dir, value string) map[string]string {
	return map[string]string{
		dir + "/hook.yaml":   heldHook("pre-"),
		dir + "/wave0.yaml":  configMap("wave-0", map[string]string{"value": value}, waves("0")),
		dir + "/gate.yaml":   gateJob("wave-1-gate", "1"),
		dir + "/wave2.yaml":  configMap("wave-2", map[string]string{"value": value}, waves("2")),
		dir + "/static.yaml": configMap("static", map[string]string{"k": "v"}, waves("0")),
	}
}

// TestControllerRestartMidSync: the controller is restarted twice during a multi-wave sync
// (while the PreSync hook runs, then while wave 1 waits for health) and the branch moves in
// between. The same Operation completes with the revision it planned, the hook is adopted
// (never duplicated) and the held wave's Job is not re-created.
func TestControllerRestartMidSync(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "restart")
	repo := s.Git().NewRepo(t, "restart")
	rev1 := repo.CommitFiles(t, "v1", multiWaveFiles("app", "one"))
	s.CreateApp(t, s.NewApp("restart", repo, "app", d, ns))
	s.WaitSyncStatus(t, "restart", v1alpha1.SyncStatusCodeOutOfSync, rev1)

	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "restart"})
	hook := waitHookJob(t, d, ns, "pre-")
	running := s.WaitOperation(t, op.Name, "planned and waiting for the hook", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && o.Status.Plan != nil && strings.Contains(o.Status.Message, "waiting for completion of hook")
	})
	require.Equal(t, []string{rev1}, running.Status.Plan.ResolvedRevisions)

	// The branch moves while the sync is running: the plan must not follow it.
	rev2 := repo.CommitFiles(t, "v2", multiWaveFiles("app", "two"))
	require.NotEqual(t, rev1, rev2)

	// Restart 1, during the PreSync phase.
	s.RestartController(t)
	stack.Consistently(t, 3*time.Second, "the running hook is adopted, not duplicated", func() (bool, string) {
		n := len(jobsWithPrefix(t, d, ns, "pre-"))
		return n == 1, itoa(n) + " hook jobs"
	})
	stack.CompleteJob(t, d, ns, hook)
	stack.Eventually(t, time.Minute, "wave 1 applied", func() (bool, string) {
		return d.Exists(t, "batch/v1", "Job", ns, "wave-1-gate"), ""
	})
	s.WaitOperation(t, op.Name, "waiting for the health of wave 1", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "wave-1-gate")
	})
	gateUID := d.Get(t, "batch/v1", "Job", ns, "wave-1-gate").GetUID()

	// Restart 2, during wave 1; the wave completes while the controller is down.
	s.Stop(t, stack.Controller)
	stack.CompleteJob(t, d, ns, "wave-1-gate")
	s.StartComponent(t, stack.Controller)

	done := s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError, v1alpha1.OperationCancelled)
	requireSucceeded(t, done)
	assert.Equal(t, []string{rev1}, done.Status.Plan.ResolvedRevisions, "plan unchanged")
	require.NotNil(t, done.Status.SyncResult)
	assert.Equal(t, rev1, done.Status.SyncResult.Revision)
	assert.Equal(t, int64(0), done.Status.RetryCount, "restarts consume no retries")
	assert.Equal(t, "one", liveData(t, d, ns, "wave-0", "value"))
	assert.Equal(t, "one", liveData(t, d, ns, "wave-2", "value"), "the moved branch was not applied")
	assert.Len(t, jobsWithPrefix(t, d, ns, "pre-"), 1, "exactly one PreSync hook")
	assert.Equal(t, gateUID, d.Get(t, "batch/v1", "Job", ns, "wave-1-gate").GetUID(), "the wave-1 Job was not re-created")
	ops := s.Operations(t, "restart")
	assert.Len(t, ops, 1, "no other Operation was created")

	a := s.WaitApp(t, "restart", "history of the completed sync; OutOfSync against the moved branch", func(a *v1alpha1.Application) bool {
		return len(a.Status.History) == 1 && a.Status.Sync.Revision == rev2 && a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync
	})
	assert.Equal(t, rev1, a.Status.History[0].Revision)
}

// TestCacheLossMidOperation: the repo-server's caches and working copies and the
// controller's live-state cache are dropped while an Operation waits on a held wave; the
// Operation completes and the Application status is rebuilt from scratch.
func TestCacheLossMidOperation(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "cacheloss")
	repo := s.Git().NewRepo(t, "cacheloss")
	files := multiWaveFiles("app", "one")
	delete(files, "app/hook.yaml")
	rev1 := repo.CommitFiles(t, "v1", files)
	s.CreateApp(t, s.NewApp("cacheloss", repo, "app", d, ns))
	s.WaitSyncStatus(t, "cacheloss", v1alpha1.SyncStatusCodeOutOfSync, rev1)
	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "cacheloss"})
	s.WaitOperation(t, op.Name, "waiting for the health of wave 1", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "wave-1-gate")
	})

	// Drop the repo-server caches (new process, empty root directory).
	gitBefore := s.Git().Requests()
	s.DropRepoServerCache(t)
	// Drop the controller's live-state cache of the destination through the API
	// (upstream: refreshRequestedAt), then measure the relist.
	before := stack.MetricSum(s.Metrics(t, stack.Controller), "patrol_app_k8s_request_total", map[string]string{"server": d.Server, "verb": "get"})
	_, err := s.API(t).InvalidateClusterCache(t.Context(), apiclient.ClusterByServer(d.Server))
	require.NoError(t, err)
	stack.Eventually(t, time.Minute, "the controller relists the destination after the cache invalidation", func() (bool, string) {
		after := stack.MetricSum(s.Metrics(t, stack.Controller), "patrol_app_k8s_request_total", map[string]string{"server": d.Server, "verb": "get"})
		// A rebuild lists every API of the cluster again (dozens of GETs).
		return after-before >= 30, "GET requests since the invalidation: " + itoa(int(after-before))
	})

	stack.CompleteJob(t, d, ns, "wave-1-gate")
	requireSucceeded(t, s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError))
	a := s.WaitSyncedHealthy(t, "cacheloss", rev1)
	assert.Len(t, a.Status.Resources, 4, stack.DescribeApp(a))
	assert.Greater(t, s.Git().Requests(), gitBefore, "the repo-server fetched the repository again")
	tree := s.Tree(t, "cacheloss")
	for _, name := range []string{"wave-0", "wave-2", "static"} {
		assert.NotNil(t, tree.FindNode("", "ConfigMap", ns, name), "tree node %s", name)
	}
	assert.NotNil(t, tree.FindNode("batch", "Job", ns, "wave-1-gate"))

	// A hard refresh after a second drop rebuilds the status again.
	s.DropRepoServerCache(t)
	s.RestartController(t)
	s.Refresh(t, "cacheloss", true)
	s.WaitSyncedHealthy(t, "cacheloss", rev1)
}

// TestDestinationUnreachable: the destination's kube-apiserver stops (a) while an Operation
// waits on a held wave and (b) under a restarted controller. Operations stay Running without
// consuming retries, the Application and the Cluster report the failure, nothing is lost, and
// everything recovers when the API server is back.
func TestDestinationUnreachable(t *testing.T) {
	s := stack.Start(t, stack.Options{
		DedicatedDestinations: 1,
		// The controller and server log failed requests to the stopped destination.
		AllowedErrors: []string{`connection refused`, `dest-2`, `Failed to load live state`, `cluster unreachable`},
		Controller:    fastClusterInfo,
	})
	d := s.Destination(2)
	require.Equal(t, stack.AuthBearerToken, d.Auth)
	ns := s.NewNamespace(t, d, "unreachable")
	repo := s.Git().NewRepo(t, "unreachable")
	files := multiWaveFiles("app", "one")
	delete(files, "app/hook.yaml")
	rev1 := repo.CommitFiles(t, "v1", files)
	s.CreateApp(t, s.NewApp("unreachable", repo, "app", d, ns))
	s.WaitSyncStatus(t, "unreachable", v1alpha1.SyncStatusCodeOutOfSync, rev1)

	// (a) Stop the destination while the Operation waits on wave 1.
	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "unreachable"})
	s.WaitOperation(t, op.Name, "waiting for the health of wave 1", func(o *v1alpha1.Operation) bool {
		return o.Status.Phase == v1alpha1.OperationRunning && strings.Contains(o.Status.Message, "wave-1-gate")
	})
	d.Stop(t)
	stack.Consistently(t, 10*time.Second, "the Operation stays Running without consuming retries", func() (bool, string) {
		o := s.GetOperation(t, op.Name)
		return o.Status.Phase == v1alpha1.OperationRunning && o.Status.RetryCount == 0, stack.DescribeOperation(o)
	})
	d.Start(t)
	stack.CompleteJob(t, d, ns, "wave-1-gate")
	requireSucceeded(t, s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError))
	s.WaitSyncedHealthy(t, "unreachable", rev1)

	// (b) Destination down under a fresh controller: ComparisonError, bounded retries.
	s.UpdateApp(t, "unreachable", func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: stack.Ptr(true)}}
	})
	d.Stop(t)
	s.RestartController(t)
	s.RequestRefresh(t, "unreachable", false)
	a := s.WaitApp(t, "unreachable", "ComparisonError while the destination is down", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionComparisonError, "")
	})
	t.Logf("condition while unreachable: %s", stack.Condition(a, v1alpha1.ApplicationConditionComparisonError).Message)
	// The Cluster reports the outage (O-1).
	down := waitCluster(t, s, d, "the Cluster reports the outage", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusFailed)
	})
	assert.NotEmpty(t, down.Status.ConnectionState.Message)
	assert.Nil(t, down.Status.Info.CacheInfo.LastCacheSyncTime, "no statistics from a cache that cannot sync")
	// Backoff: the controller does not hammer the unreachable cluster.
	errorsBefore := stack.MetricSum(s.Metrics(t, stack.Controller), "patrol_app_k8s_request_total", map[string]string{"server": d.Server, "response_code": "error"})
	window := 10 * time.Second
	stack.Consistently(t, window, "ComparisonError persists while the destination is down", func() (bool, string) {
		a := s.GetApp(t, "unreachable")
		return stack.HasCondition(a, v1alpha1.ApplicationConditionComparisonError, ""), stack.DescribeApp(a)
	})
	errorsAfter := stack.MetricSum(s.Metrics(t, stack.Controller), "patrol_app_k8s_request_total", map[string]string{"server": d.Server, "response_code": "error"})
	t.Logf("failed requests to the stopped destination in %s: %.0f", window, errorsAfter-errorsBefore)
	assert.Less(t, errorsAfter-errorsBefore, 200.0, "connection attempts are backed off")

	// No data loss: spec, history and Operations are intact; a new commit waits.
	rev2 := repo.CommitFiles(t, "v2", map[string]string{"app/wave0.yaml": configMap("wave-0", map[string]string{"value": "two"}, waves("0"))})
	s.RequestRefresh(t, "unreachable", false)
	a = s.GetApp(t, "unreachable")
	require.Len(t, a.Status.History, 1)
	assert.Equal(t, rev1, a.Status.History[0].Revision)
	assert.Len(t, s.Operations(t, "unreachable"), 1)

	d.Start(t)
	s.SimulateWorkloads(d, ns)
	// The live-state cache retries a failed sync after its retry timeout, so refresh until
	// the controller compares against the cluster again.
	a = s.RefreshUntil(t, "unreachable", "Synced and Healthy at the new revision", func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced && a.Status.Health.Status == v1alpha1.HealthStatusHealthy && a.Status.Sync.Revision == rev2
	})
	assert.Nil(t, stack.Condition(a, v1alpha1.ApplicationConditionComparisonError), "ComparisonError cleared")
	assert.Equal(t, "two", liveData(t, d, ns, "wave-0", "value"))
	up := waitCluster(t, s, d, "the Cluster reports the recovery", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusSuccessful)
	})
	assert.Empty(t, up.Status.ConnectionState.Message)
	assert.NotEmpty(t, up.Status.ServerVersion)
	assert.NotNil(t, up.Status.Info.CacheInfo.LastCacheSyncTime)
}

// TestRepoServerDown: with the repo-server stopped, a periodic-style (latest) comparison
// within the repo error grace period keeps the last status; a refresh (force-resolve, which
// bypasses the grace period as upstream) reports ComparisonError; the API refuses syncs that
// need revision resolution; everything recovers once the repo-server is back.
func TestRepoServerDown(t *testing.T) {
	s := stack.Start(t, stack.Options{
		ControllerParams: map[string]string{"controller.repo.error.grace.period.seconds": "60"},
		AllowedErrors:    []string{`repo-server`, `connection refused`, `failed to generate manifest`},
	})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "reposerver")
	repo := s.Git().NewRepo(t, "reposerver")
	rev1 := repo.CommitFiles(t, "v1", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "1"}, nil)})
	s.CreateApp(t, s.NewApp("reposerver", repo, "app", d, ns))
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "reposerver"}))
	s.WaitSyncedHealthy(t, "reposerver", rev1)

	s.StopRepoServer(t)
	// A spec change (generation not observed: a latest-level comparison) within the grace
	// period keeps the last status.
	s.UpdateApp(t, "reposerver", func(a *v1alpha1.Application) {
		a.Spec.Info = []v1alpha1.Info{{Name: "probe", Value: "1"}}
	})
	stack.Consistently(t, 3*time.Second, "status kept within the grace period", func() (bool, string) {
		a := s.GetApp(t, "reposerver")
		return !stack.HasCondition(a, v1alpha1.ApplicationConditionComparisonError, "") && a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced, stack.DescribeApp(a)
	})
	// A refresh resolves revisions again (no revision cache, no grace period): the outage is
	// reported.
	s.RequestRefresh(t, "reposerver", false)
	a := s.WaitApp(t, "reposerver", "ComparisonError with the repo-server down", func(a *v1alpha1.Application) bool {
		return stack.HasCondition(a, v1alpha1.ApplicationConditionComparisonError, "failed to generate manifest")
	})
	t.Logf("condition: %s", stack.Condition(a, v1alpha1.ApplicationConditionComparisonError).Message)
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, a.Status.Sync.Status)
	assert.Len(t, a.Status.History, 1, "no data loss")

	// The API cannot resolve a revision without the repo-server.
	_, err := s.API(t).SyncApplication(t.Context(), &apiclient.ApplicationSyncRequest{Name: "reposerver", Revision: "main"})
	require.Error(t, err)
	t.Logf("sync with the repo-server down: %v", err)
	assert.True(t, apiclient.IsUnavailable(err) || strings.Contains(err.Error(), "repo-server"), "%v", err)

	s.StartRepoServer(t)
	rev2 := repo.CommitFiles(t, "v2", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "2"}, nil)})
	s.Refresh(t, "reposerver", false)
	a = s.WaitSyncStatus(t, "reposerver", v1alpha1.SyncStatusCodeOutOfSync, rev2)
	assert.Nil(t, stack.Condition(a, v1alpha1.ApplicationConditionComparisonError), "ComparisonError cleared")
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "reposerver"}))
	s.WaitSyncedHealthy(t, "reposerver", rev2)
	assert.Equal(t, "2", liveData(t, d, ns, "cm", "v"))
}

// TestAPIConflictInjection: every other status write of the controller to Operations and
// Applications fails with 409 Conflict while a hooked, multi-wave sync runs, and the test
// writes the Application concurrently. The sync completes once, with one hook, one history
// entry and a consistent status.
func TestAPIConflictInjection(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(1)
	ns := s.NewNamespace(t, d, "conflicts")
	repo := s.Git().NewRepo(t, "conflicts")
	files := multiWaveFiles("app", "one")
	rev1 := repo.CommitFiles(t, "v1", files)
	s.CreateApp(t, s.NewApp("conflicts", repo, "app", d, ns))
	s.WaitSyncStatus(t, "conflicts", v1alpha1.SyncStatusCodeOutOfSync, rev1)

	// Every other write fails until 16 conflicts were injected (a bounded budget: alternating
	// failures shared by concurrent writers could otherwise starve one of them forever).
	var mu sync.Mutex
	var writes, injected int
	s.SetControllerFault(func(r *http.Request) (int, bool) {
		if !stack.IsWrite(r, "operations", false) && !stack.IsWrite(r, "applications", true) {
			return 0, false
		}
		mu.Lock()
		defer mu.Unlock()
		writes++
		if injected < 16 && writes%2 == 1 {
			injected++
			return http.StatusConflict, true
		}
		return 0, false
	})
	t.Cleanup(func() { s.SetControllerFault(nil) })

	// Concurrent writers of the Application's metadata (real 409s for the controller).
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			patchLabel(t, s, "conflicts", itoa(i))
		}
	}()
	defer close(stop)

	op := s.Sync(t, &apiclient.ApplicationSyncRequest{Name: "conflicts"})
	hook := waitHookJob(t, d, ns, "pre-")
	stack.CompleteJob(t, d, ns, hook)
	stack.Eventually(t, time.Minute, "wave 1 applied", func() (bool, string) {
		return d.Exists(t, "batch/v1", "Job", ns, "wave-1-gate"), ""
	})
	stack.CompleteJob(t, d, ns, "wave-1-gate")
	done := s.WaitOperationPhase(t, op.Name, v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError)
	requireSucceeded(t, done)
	assert.Greater(t, s.InjectedFaults()[http.StatusConflict], 3, "conflicts were injected")
	assert.Len(t, jobsWithPrefix(t, d, ns, "pre-"), 1, "the hook ran once")
	s.WaitSyncedHealthy(t, "conflicts", rev1)
	// The completion's Application write (summary and history) may have been among the
	// rejected writes; the controller retries it.
	a := s.WaitApp(t, "conflicts", "history entry and summary of the completed Operation", func(a *v1alpha1.Application) bool {
		return len(a.Status.History) > 0 && a.Status.OperationState != nil && a.Status.OperationState.Phase == v1alpha1.OperationSucceeded
	})
	require.Len(t, a.Status.History, 1, "one history entry despite conflicting writes")
	assert.Equal(t, op.Name, a.Status.History[0].OperationName)
	assert.Equal(t, op.Name, a.Status.OperationState.OperationName)
	assert.Len(t, s.Operations(t, "conflicts"), 1)
	stack.Consistently(t, 3*time.Second, "no duplicate history entry", func() (bool, string) {
		n := len(s.GetApp(t, "conflicts").Status.History)
		return n == 1, itoa(n) + " entries"
	})
}

// patchLabel writes a label on the Application (a concurrent writer). It runs in a goroutine:
// errors are ignored and it never calls t.Fatal.
func patchLabel(t *testing.T, s *stack.Stack, name, value string) {
	t.Helper()
	o, err := s.Control().TryGet(t.Context(), v1alpha1.GroupVersion.String(), "Application", s.Namespace, name)
	if err != nil || o == nil {
		return
	}
	lbls := o.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls["test.patrol.example/writer"] = value
	o.SetLabels(lbls)
	ri := s.Control().Dynamic.Resource(v1alpha1.GroupVersion.WithResource("applications")).Namespace(s.Namespace)
	_, _ = ri.Update(t.Context(), o, metav1.UpdateOptions{}) // conflicts with the controller are expected
}
