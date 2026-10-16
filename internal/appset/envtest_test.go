package appset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
)

func newSchemeForTests() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// envFixture is a running controller (Run) watching a fresh control-plane namespace and a
// fresh tenant namespace.
type envFixture struct {
	t       *testing.T
	ctx     context.Context
	control string
	tenant  string
	c       client.Client
}

func suffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func startEnv(t *testing.T, mutate func(*RunConfig)) *envFixture {
	t.Helper()
	ctx := requireEnv(t)
	f := &envFixture{t: t, ctx: ctx, control: "patrol-" + suffix(), tenant: "team-" + suffix(), c: envClient}
	for _, ns := range []string{f.control, f.tenant} {
		require.NoError(t, envClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	require.NoError(t, envClient.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: f.control},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:      []string{"*"},
			Destinations:     []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			SourceNamespaces: []string{f.tenant},
		},
	}))
	rc := RunConfig{
		Config: Config{
			Namespace:                f.control,
			ApplicationSetNamespaces: []string{f.tenant},
			Logger:                   testLogger(),
			EnablePolicyOverride:     true,
			Generators:               generators.Config{},
		},
		RestConfig: testCfg,
	}
	if mutate != nil {
		mutate(&rc)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, rc) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Error("controller did not stop")
		}
	})
	return f
}

func (f *envFixture) appSet(name string, elems ...map[string]any) *v1alpha1.ApplicationSet {
	as := listAppSet(f.t, f.tenant, name, elems...)
	return as
}

func (f *envFixture) create(obj client.Object) {
	f.t.Helper()
	require.NoError(f.t, f.c.Create(f.ctx, obj))
}

// update re-reads obj, applies mutate and updates it (conflict retries).
func (f *envFixture) updateAppSet(name string, mutate func(*v1alpha1.ApplicationSet)) {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		as := &v1alpha1.ApplicationSet{}
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: name}, as); err != nil {
			return false
		}
		mutate(as)
		return f.c.Update(f.ctx, as) == nil
	}, 10*time.Second, 100*time.Millisecond)
}

func (f *envFixture) updateApp(name string, mutate func(*v1alpha1.Application), status bool) {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		app := &v1alpha1.Application{}
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: name}, app); err != nil {
			return false
		}
		mutate(app)
		if status {
			return f.c.Status().Update(f.ctx, app) == nil
		}
		return f.c.Update(f.ctx, app) == nil
	}, 10*time.Second, 100*time.Millisecond)
}

// apps lists the tenant Applications (empty on error: it runs inside Eventually conditions,
// which may outlive the test).
func (f *envFixture) apps() map[string]v1alpha1.Application {
	var list v1alpha1.ApplicationList
	_ = f.c.List(f.ctx, &list, client.InNamespace(f.tenant))
	out := map[string]v1alpha1.Application{}
	for _, a := range list.Items {
		out[a.Name] = a
	}
	return out
}

// getAppSet returns the ApplicationSet (empty on error, see apps).
func (f *envFixture) getAppSet(name string) *v1alpha1.ApplicationSet {
	as := &v1alpha1.ApplicationSet{}
	_ = f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: name}, as)
	return as
}

// waitObserved waits until the controller reconciled the current generation of an
// ApplicationSet (status.observedGeneration), then returns it.
func (f *envFixture) waitObserved(name string) *v1alpha1.ApplicationSet {
	f.t.Helper()
	var as *v1alpha1.ApplicationSet
	require.Eventually(f.t, func() bool {
		as = &v1alpha1.ApplicationSet{}
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: name}, as); err != nil {
			return false
		}
		up := findCondition(as, v1alpha1.ApplicationSetConditionParametersGenerated)
		return as.Status.ObservedGeneration == as.Generation && up != nil && up.ObservedGeneration == as.Generation
	}, 30*time.Second, 100*time.Millisecond)
	return as
}

func (f *envFixture) eventually(cond func() bool, msg string) {
	f.t.Helper()
	require.Eventually(f.t, cond, 30*time.Second, 100*time.Millisecond, msg)
}

func TestEnvtestLifecycle(t *testing.T) {
	t.Parallel()
	f := startEnv(t, nil)
	as := f.appSet("lifecycle", elem("a", "dev"), elem("b", "prod"))
	as.Spec.Template.Namespace = f.control
	as.Spec.PreservedFields = &v1alpha1.ApplicationPreservedFields{Annotations: []string{"keep"}}
	as.Spec.IgnoreApplicationDifferences = v1alpha1.ApplicationSetIgnoreDifferences{{JSONPointers: []string{"/spec/syncPolicy"}}}
	as.Spec.Template.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}
	f.create(as)

	f.eventually(func() bool { return len(f.apps()) == 2 }, "Applications are created in the ApplicationSet namespace")
	got := f.waitObserved("lifecycle")
	f.eventually(func() bool {
		got = f.getAppSet("lifecycle")
		c := findCondition(got, v1alpha1.ApplicationSetConditionResourcesUpToDate)
		return c != nil && c.Status == metav1.ConditionTrue && got.Status.ResourcesCount == 2
	}, "ResourcesUpToDate and resources status")
	assert.Equal(t, v1alpha1.HealthStatusHealthy, got.Status.Health.Status)
	app := f.apps()["a"]
	owner := metav1.GetControllerOf(&app)
	require.NotNil(t, owner)
	assert.Equal(t, got.UID, owner.UID)
	assert.Equal(t, []string{v1alpha1.FinalizerResources}, app.Finalizers)

	// A manual spec edit is reverted; ignored fields and preserved keys keep their live values.
	f.updateApp("a", func(a *v1alpha1.Application) {
		a.Spec.Source.Path = "tampered"
		a.Spec.SyncPolicy = nil
		a.Annotations = map[string]string{"keep": "user", "drop": "user"}
	}, false)
	f.eventually(func() bool {
		a := f.apps()["a"]
		return a.Spec.Source.Path == "apps/a" && a.Annotations["keep"] == "user" && a.Annotations["drop"] == "" && a.Spec.SyncPolicy == nil
	}, "spec reverted, ignored syncPolicy and preserved annotation kept, other annotation dropped")

	// Status-only changes of an Application are not reverted or rewritten.
	f.updateApp("a", func(a *v1alpha1.Application) { a.Status.Health.Status = v1alpha1.HealthStatusHealthy }, true)
	rv := f.apps()["a"].ResourceVersion
	time.Sleep(time.Second)
	assert.Equal(t, rv, f.apps()["a"].ResourceVersion, "no write after a status-only change")

	// Template change and element removal (sync policy).
	f.updateAppSet("lifecycle", func(as *v1alpha1.ApplicationSet) {
		as.Spec.Template.Spec.Source.TargetRevision = "v2"
		as.Spec.Generators[0].List.Elements = jsonList(t, elem("a", "dev"))
	})
	f.eventually(func() bool {
		apps := f.apps()
		b, bExists := apps["b"]
		return apps["a"].Spec.Source.TargetRevision == "v2" && (!bExists || b.DeletionTimestamp != nil)
	}, "a updated and b deleted")

	// The refresh annotation is removed after a successful pass.
	f.updateAppSet("lifecycle", func(as *v1alpha1.ApplicationSet) {
		if as.Annotations == nil {
			as.Annotations = map[string]string{}
		}
		as.Annotations[v1alpha1.AnnotationKeyApplicationSetRefresh] = "true"
	})
	f.eventually(func() bool {
		_, ok := f.getAppSet("lifecycle").Annotations[v1alpha1.AnnotationKeyApplicationSetRefresh]
		return !ok
	}, "refresh annotation removed")
}

func TestEnvtestPolicies(t *testing.T) {
	t.Parallel()
	f := startEnv(t, nil)
	tests := []struct {
		policy          v1alpha1.ApplicationsSyncPolicy
		updated, pruned bool
	}{
		{v1alpha1.ApplicationsSyncPolicyCreateOnly, false, false},
		{v1alpha1.ApplicationsSyncPolicyCreateUpdate, true, false},
		{v1alpha1.ApplicationsSyncPolicyCreateDelete, false, true},
		{v1alpha1.ApplicationsSyncPolicySync, true, true},
	}
	for _, tt := range tests {
		name := string(tt.policy)
		as := f.appSet(name, elem(name+"-keep", "dev"), elem(name+"-drop", "dev"))
		as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(tt.policy), PreserveResourcesOnDeletion: true}
		f.create(as)
	}
	for _, tt := range tests {
		name := string(tt.policy)
		f.eventually(func() bool {
			apps := f.apps()
			_, a := apps[name+"-keep"]
			_, b := apps[name+"-drop"]
			return a && b
		}, name+": both Applications created")
		f.updateAppSet(name, func(as *v1alpha1.ApplicationSet) {
			as.Spec.Template.Spec.Source.TargetRevision = "v2"
			as.Spec.Generators[0].List.Elements = jsonList(t, elem(name+"-keep", "dev"))
		})
	}
	for _, tt := range tests {
		name := string(tt.policy)
		f.waitObserved(name)
		f.eventually(func() bool {
			apps := f.apps()
			_, dropped := apps[name+"-drop"]
			rev := apps[name+"-keep"].Spec.Source.TargetRevision
			wantRev := "HEAD"
			if tt.updated {
				wantRev = "v2"
			}
			return rev == wantRev && dropped != tt.pruned
		}, name+": update/delete per policy")
	}
}

func TestEnvtestDeletion(t *testing.T) {
	t.Parallel()
	f := startEnv(t, nil)

	// preserveResourcesOnDeletion: the controller adds its finalizer and, on deletion, removes
	// the resources finalizers of the Applications before releasing the ApplicationSet.
	preserve := f.appSet("preserve", elem("p1", "dev"))
	preserve.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}
	preserve.Spec.Template.Finalizers = []string{v1alpha1.FinalizerResources, v1alpha1.FinalizerPostDelete}
	f.create(preserve)
	f.eventually(func() bool {
		return slices.Contains(f.getAppSet("preserve").Finalizers, v1alpha1.FinalizerApplicationSet) && len(f.apps()["p1"].Finalizers) == 2
	}, "finalizer added and Application created")
	require.NoError(t, f.c.Delete(f.ctx, f.getAppSet("preserve")))
	f.eventually(func() bool {
		err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: "preserve"}, &v1alpha1.ApplicationSet{})
		return apierrors.IsNotFound(err)
	}, "ApplicationSet released")
	assert.Equal(t, []string{v1alpha1.FinalizerPostDelete}, f.apps()["p1"].Finalizers)

	// create-only with the finalizer: Applications survive without owner references.
	keep := f.appSet("keep", elem("k1", "dev"))
	keep.Finalizers = []string{v1alpha1.FinalizerApplicationSet}
	keep.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(v1alpha1.ApplicationsSyncPolicyCreateOnly)}
	f.create(keep)
	f.eventually(func() bool { _, ok := f.apps()["k1"]; return ok }, "created")
	require.NoError(t, f.c.Delete(f.ctx, f.getAppSet("keep")))
	f.eventually(func() bool {
		err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: "keep"}, &v1alpha1.ApplicationSet{})
		return apierrors.IsNotFound(err) && len(f.apps()["k1"].OwnerReferences) == 0
	}, "Application released from ownership")
}

func TestEnvtestTemplatedProjectEscalation(t *testing.T) {
	t.Parallel()
	f := startEnv(t, nil)
	require.NoError(t, f.c.Create(f.ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: f.control},
		Spec:       v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}}},
	}))
	as := f.appSet("escalate", elem("ok", "default"), elem("evil", "admin"))
	as.Spec.Template.Spec.Project = "{{env}}"
	f.create(as)
	f.eventually(func() bool {
		got := f.getAppSet("escalate")
		c := findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred)
		_, ok := f.apps()["ok"]
		return ok && c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ApplicationSetReasonApplicationValidationError &&
			c.Message == "application 'evil' in namespace '"+f.tenant+"' is not permitted to use project 'admin'"
	}, "validation error reported, valid Application created")
	assert.NotContains(t, f.apps(), "evil", "a tenant ApplicationSet cannot use a project that does not permit its namespace")
	assert.Equal(t, v1alpha1.HealthStatusDegraded, f.getAppSet("escalate").Status.Health.Status)
}

// envRepos is a concurrency-safe in-memory generators.RepoService that records requests.
type envRepos struct {
	mu       sync.Mutex
	dirs     []string
	requests []generators.RepoRequest
}

func (r *envRepos) GetFiles(_ context.Context, req generators.RepoRequest, _ string) (map[string][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return map[string][]byte{}, nil
}

func (r *envRepos) GetDirectories(_ context.Context, req generators.RepoRequest) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return slices.Clone(r.dirs), nil
}

func (r *envRepos) projects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.requests))
	for _, req := range r.requests {
		out = append(out, req.Project)
	}
	return out
}

// TestEnvtestGitCredentialScope: a tenant ApplicationSet cannot read a repository with the
// credentials of a project that does not trust its namespace, whatever project its generated
// Applications use.
func TestEnvtestGitCredentialScope(t *testing.T) {
	t.Parallel()
	repos := &envRepos{dirs: []string{"secret-dir-of-team-b"}}
	f := startEnv(t, func(rc *RunConfig) { rc.Generators.Repos = repos })
	require.NoError(t, f.c.Create(f.ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b", Namespace: f.control},
		Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			SourceNamespaces: []string{"team-b"}},
	}))
	gitAppSet := func(name, top, generatorProject string) *v1alpha1.ApplicationSet {
		as := f.appSet(name)
		as.Spec.Generators = []v1alpha1.ApplicationSetGenerator{{Git: &v1alpha1.GitGenerator{
			RepoURL: "https://git.example.com/team-b/private.git", Revision: "HEAD",
			Directories: []v1alpha1.GitDirectoryGeneratorItem{{Path: "*"}},
		}}}
		as.Spec.Generators[0].Git.Template.Spec.Project = generatorProject
		as.Spec.Template.Name = name + "-{{path.basename}}"
		as.Spec.Template.Labels = nil
		as.Spec.Template.Spec.Source.Path = "{{path}}"
		as.Spec.Template.Spec.Destination.Namespace = "{{path.basename}}"
		as.Spec.Template.Spec.Project = top
		return as
	}

	// Top-level project of the victim, generator-level project of the tenant.
	f.create(gitAppSet("steal", "team-b", "default"))
	f.eventually(func() bool { _, ok := f.apps()["steal-secret-dir-of-team-b"]; return ok }, "Application generated")
	assert.Equal(t, "default", f.apps()["steal-secret-dir-of-team-b"].Spec.Project)
	assert.NotContains(t, repos.projects(), "team-b", "the repository was never read with team-b's credential scope")

	// Victim project as the effective project: the generator fails before reading anything.
	f.create(gitAppSet("direct", "team-b", ""))
	f.eventually(func() bool {
		c := findCondition(f.getAppSet("direct"), v1alpha1.ApplicationSetConditionErrorOccurred)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ApplicationSetReasonApplicationParamsGenerationError &&
			strings.Contains(c.Message, "ApplicationSet namespace '"+f.tenant+"' is not permitted to use project 'team-b'")
	}, "generation error reported")
	assert.NotContains(t, repos.projects(), "team-b")
	for name := range f.apps() {
		assert.False(t, strings.HasPrefix(name, "direct-"), "no Application generated from team-b's repository")
	}
}

func TestEnvtestClusterWatch(t *testing.T) {
	t.Parallel()
	f := startEnv(t, nil)
	as := &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "clusters", Namespace: f.tenant},
		Spec: v1alpha1.ApplicationSetSpec{
			Generators: []v1alpha1.ApplicationSetGenerator{{Clusters: &v1alpha1.ClusterGenerator{
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			}}},
			Template: v1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "guestbook-{{name}}"},
				Spec: v1alpha1.ApplicationSpec{
					Project:     "default",
					Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example/repo.git", Path: "guestbook"},
					Destination: v1alpha1.ApplicationDestination{Server: "{{server}}", Namespace: "guestbook"},
				},
			},
		},
	}
	f.create(as)
	f.waitObserved("clusters")
	assert.Empty(t, f.apps())
	require.NoError(t, f.c.Create(f.ctx, &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "east", Namespace: f.control, Labels: map[string]string{"env": "prod"}},
		Spec:       v1alpha1.ClusterSpec{Name: "east", Server: "https://east.example"},
	}))
	f.eventually(func() bool { _, ok := f.apps()["guestbook-east"]; return ok }, "a new Cluster triggers generation without a timer")
}

func TestEnvtestRollingSync(t *testing.T) {
	t.Parallel()
	f := startEnv(t, func(rc *RunConfig) { rc.EnableProgressiveSyncs = true })
	as := f.appSet("rollout", elem("dev1", "dev"), elem("prod1", "prod"))
	as.Spec.Template.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}
	as.Spec.Strategy = rollingSync(step(nil, envIn("dev")), step(nil, envIn("prod")))
	as.Spec.Strategy.DeletionOrder = v1alpha1.ApplicationSetDeletionOrderReverse
	f.create(as)

	status := func(name string) v1alpha1.ProgressiveSyncStatusCode {
		for _, s := range f.getAppSet("rollout").Status.ApplicationStatus {
			if s.Application == name {
				return s.Status
			}
		}
		return ""
	}
	ops := func() map[string]v1alpha1.Operation {
		var list v1alpha1.OperationList
		require.NoError(t, f.c.List(f.ctx, &list, client.InNamespace(f.tenant)))
		out := map[string]v1alpha1.Operation{}
		for _, op := range list.Items {
			out[op.Spec.Application] = op
		}
		return out
	}
	// simulate plays a fast application controller: the Operation ran and the Application
	// became Synced with the given health, all within the second of the Pending transition (the
	// API server stores seconds, so only the Operation's identity orders it after the transition).
	simulate := func(name string, health v1alpha1.HealthStatusCode) {
		var at metav1.Time
		for _, s := range f.getAppSet("rollout").Status.ApplicationStatus {
			if s.Application == name && s.LastTransitionTime != nil {
				at = *s.LastTransitionTime
			}
		}
		require.False(t, at.IsZero())
		f.updateApp(name, func(a *v1alpha1.Application) {
			a.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced
			a.Status.Health.Status = health
			a.Status.ReconciledAt = &at
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: ops()[name].Name, Phase: v1alpha1.OperationSucceeded, StartedAt: &at, FinishedAt: &at}
		}, true)
	}

	f.eventually(func() bool { return status("dev1") == v1alpha1.ProgressiveSyncPending }, "dev1 Pending")
	f.eventually(func() bool { _, ok := ops()["dev1"]; return ok }, "dev1 Operation created")
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, status("prod1"))
	assert.NotContains(t, ops(), "prod1")
	app := f.apps()["dev1"]
	assert.False(t, *app.Spec.SyncPolicy.Automated.Enabled)
	f.eventually(func() bool {
		c := findCondition(f.getAppSet("rollout"), v1alpha1.ApplicationSetConditionRolloutProgressing)
		return c != nil && c.Status == metav1.ConditionTrue && c.Message == "ApplicationSet is performing rollout of step 1"
	}, "rollout step 1")

	simulate("dev1", v1alpha1.HealthStatusProgressing)
	f.eventually(func() bool { return status("dev1") == v1alpha1.ProgressiveSyncProgressing }, "dev1 Progressing")
	simulate("dev1", v1alpha1.HealthStatusHealthy)
	f.eventually(func() bool { return status("dev1") == v1alpha1.ProgressiveSyncHealthy }, "dev1 Healthy")
	f.eventually(func() bool { return status("prod1") == v1alpha1.ProgressiveSyncPending }, "prod1 Pending after step 1")
	f.eventually(func() bool { _, ok := ops()["prod1"]; return ok }, "prod1 Operation created")

	simulate("prod1", v1alpha1.HealthStatusHealthy)
	f.eventually(func() bool {
		return status("prod1") == v1alpha1.ProgressiveSyncProgressing || status("prod1") == v1alpha1.ProgressiveSyncHealthy
	}, "prod1 progressing")
	simulate("prod1", v1alpha1.HealthStatusHealthy)
	f.eventually(func() bool {
		c := findCondition(f.getAppSet("rollout"), v1alpha1.ApplicationSetConditionRolloutProgressing)
		return status("prod1") == v1alpha1.ProgressiveSyncHealthy && c != nil && c.Status == metav1.ConditionFalse
	}, "rollout complete")

	// Reverse deletion: prod1 (step 2) is deleted before dev1 (step 1).
	require.True(t, slices.Contains(f.getAppSet("rollout").Finalizers, v1alpha1.FinalizerApplicationSet))
	require.NoError(t, f.c.Delete(f.ctx, f.getAppSet("rollout")))
	f.eventually(func() bool { return f.apps()["prod1"].DeletionTimestamp != nil }, "prod1 deleted first")
	assert.Nil(t, f.apps()["dev1"].DeletionTimestamp, "dev1 waits for prod1")
	f.updateApp("prod1", func(a *v1alpha1.Application) { a.Finalizers = nil }, false) // the application controller finishes
	f.eventually(func() bool { return f.apps()["dev1"].DeletionTimestamp != nil }, "then dev1")
	f.updateApp("dev1", func(a *v1alpha1.Application) { a.Finalizers = nil }, false)
	f.eventually(func() bool {
		err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.tenant, Name: "rollout"}, &v1alpha1.ApplicationSet{})
		return apierrors.IsNotFound(err)
	}, "ApplicationSet released once every Application is gone")
}

func testLogger() *slog.Logger {
	if os.Getenv("APPSET_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}
