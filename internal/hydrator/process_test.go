package hydrator

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// APP-116, APP-118, APP-132, APP-137: two Applications sharing the dry source and the
// destination branch are hydrated in one commit-server request; both end Hydrated.
func TestReconcileKeyHydratesGroupInOneCommit(t *testing.T) {
	t.Parallel()
	a, b := hydratorApp("a", "out/a"), hydratorApp("b", "out/b")
	e := newTestEnv(t, defaultProject("default"), a, b)
	ctx := testCtx(t)

	_, err := e.process(ctx, keyOf(t, a))
	require.NoError(t, err)

	reqs := e.commit.requests()
	require.Len(t, reqs, 1)
	req := reqs[0]
	assert.Equal(t, shaA, req.DrySHA)
	assert.Equal(t, "env/prod", req.SyncBranch)
	assert.Equal(t, "env/prod", req.TargetBranch)
	assert.Equal(t, dryRepo, req.Repo.URL)
	assert.Equal(t, dryRepo, req.DryRepoURL)
	assert.Equal(t, settings.DefaultReadmeMessageTemplate, req.ReadmeTemplate)
	assert.Empty(t, req.Author.Name, "unset author is defaulted by the commit-server")
	assert.Contains(t, req.CommitMessage, "aaaaaaa: Update aaaaaaa")
	assert.Contains(t, req.CommitMessage, "Co-authored-by: Code Author <code@example.com>")
	require.Len(t, req.Paths, 2)
	assert.Equal(t, "out/a", req.Paths[0].Path)
	assert.Equal(t, "out/b", req.Paths[1].Path)
	assert.Equal(t, []string{"render dry/a"}, req.Paths[0].Commands)
	require.NotNil(t, req.DryCommitMetadata)

	// Tracking metadata is stripped (APP-118); other annotations are kept.
	var obj map[string]any
	require.NoError(t, json.Unmarshal(req.Paths[0].Manifests[0], &obj))
	ann := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.NotContains(t, ann, v1alpha1.AnnotationKeyTrackingID)
	assert.NotContains(t, ann, v1alpha1.AnnotationKeyInstallationID)
	assert.Equal(t, "yes", ann["example.com/keep"])

	// The first app renders at the target revision, the others at the resolved SHA, both
	// without caches.
	require.Len(t, e.repo.manifestReqs, 2)
	revs := map[string]string{}
	for _, r := range e.repo.manifestReqs {
		revs[r.Source.Path] = r.Revision
		assert.True(t, r.NoCache)
		assert.True(t, r.NoRevisionCache)
		assert.Equal(t, "apps", r.Namespace)
		assert.Equal(t, "default", r.ProjectName)
	}
	assert.Equal(t, map[string]string{"dry/a": "main", "dry/b": shaA}, revs)

	for _, name := range []string{"a", "b"} {
		app := e.get(t, name)
		s := app.Status.SourceHydrator
		require.NotNil(t, s.CurrentOperation, name)
		assert.Equal(t, v1alpha1.HydrateOperationPhaseHydrated, s.CurrentOperation.Phase)
		assert.Equal(t, shaA, s.CurrentOperation.DrySHA)
		assert.Equal(t, shaH, s.CurrentOperation.HydratedSHA)
		assert.Empty(t, s.CurrentOperation.Message)
		require.NotNil(t, s.CurrentOperation.StartedAt)
		require.NotNil(t, s.CurrentOperation.FinishedAt)
		assert.Equal(t, app.Spec.SourceHydrator, s.CurrentOperation.SourceHydrator)
		require.NotNil(t, s.LastSuccessfulOperation)
		assert.Equal(t, v1alpha1.SuccessfulHydrateOperation{DrySHA: shaA, HydratedSHA: shaH, SourceHydrator: app.Spec.SourceHydrator}, *s.LastSuccessfulOperation)
		assert.Equal(t, shaA, s.LastComparedDryRevision)
		assert.Equal(t, string(v1alpha1.RefreshTypeNormal), app.Annotations[v1alpha1.AnnotationKeyRefresh], "post-hydration refresh")
	}
	assert.Equal(t, []string{"default"}, e.repos.writeProjects)
}

// APP-116: an already hydrated dry SHA is not committed again; a forced (hard) hydration
// calls the commit-server, and a changed configuration of any app does too.
func TestReconcileKeySkipsAlreadyHydratedDrySHA(t *testing.T) {
	t.Parallel()
	a, b := hydratorApp("a", "out/a"), hydratorApp("b", "out/b")
	e := newTestEnv(t, defaultProject("default"), a, b)
	ctx := testCtx(t)
	k := keyOf(t, a)

	_, err := e.process(ctx, k)
	require.NoError(t, err)
	require.Len(t, e.commit.requests(), 1)

	_, err = e.process(ctx, k)
	require.NoError(t, err)
	assert.Len(t, e.commit.requests(), 1, "same dry SHA and configuration: no commit request")
	assert.Equal(t, v1alpha1.HydrateOperationPhaseHydrated, e.get(t, "b").Status.SourceHydrator.CurrentOperation.Phase)

	e.h.forced.Store(k, struct{}{})
	_, err = e.process(ctx, k)
	require.NoError(t, err)
	assert.Len(t, e.commit.requests(), 2, "hard hydrate calls the commit-server")

	// A changed sync path (same group) is written without a new dry commit.
	app := e.get(t, "b")
	app.Spec.SourceHydrator.SyncSource.Path = "out/b2"
	require.NoError(t, e.c.Update(ctx, app))
	_, err = e.process(ctx, k)
	require.NoError(t, err)
	reqs := e.commit.requests()
	require.Len(t, reqs, 3)
	assert.Equal(t, "out/b2", reqs[2].Paths[1].Path)
}

// APP-117: validation errors are reported on the failing app and a generic error on the
// others; nothing is rendered or committed.
func TestReconcileKeyValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(a, b *v1alpha1.Application)
		objs   []client.Object
		want   map[string]string
	}{
		{
			name:   "project not found",
			mutate: func(_, b *v1alpha1.Application) { b.Spec.Project = "missing" },
			want: map[string]string{
				"a": "Failed to hydrate: cannot hydrate because application patrol/b has an error",
				"b": `Failed to hydrate: failed to get project "missing": `,
			},
		},
		{
			name:   "dry repo not permitted",
			mutate: func(_, b *v1alpha1.Application) { b.Spec.Project = "restricted" },
			objs:   []client.Object{defaultProject("restricted", "https://git.example.com/other.git")},
			want: map[string]string{
				"a": "Failed to hydrate: cannot hydrate because application patrol/b has an error",
				"b": "Failed to hydrate: application repo " + dryRepo + " is not permitted in project 'restricted'",
			},
		},
		{
			name: "destination repo not permitted",
			mutate: func(a, b *v1alpha1.Application) {
				for _, x := range []*v1alpha1.Application{a, b} {
					x.Spec.SourceHydrator.SyncSource.RepoURL = "https://git.example.com/org/hydrated.git"
				}
				b.Spec.Project = "restricted"
			},
			objs: []client.Object{defaultProject("restricted", dryRepo)},
			want: map[string]string{
				"a": "Failed to hydrate: cannot hydrate because application patrol/b has an error",
				"b": "Failed to hydrate: destination repo https://git.example.com/org/hydrated.git is not permitted in project 'restricted'",
			},
		},
		{
			name:   "outside the repository",
			mutate: func(a, _ *v1alpha1.Application) { a.Spec.SourceHydrator.SyncSource.Path = "../x" },
			want: map[string]string{
				"a": `Failed to hydrate: app is configured to hydrate outside the repository (branch "env/prod", path "../x") which is not allowed`,
				"b": "Failed to hydrate: cannot hydrate because application patrol/a has an error",
			},
		},
		{
			name:   "same destination",
			mutate: func(_, b *v1alpha1.Application) { b.Spec.SourceHydrator.SyncSource.Path = "out/a" },
			want: map[string]string{
				"a": "Failed to hydrate: app patrol/b hydrator uses the same destination: repo=" + dryRepo + ", path=out/a",
				"b": "Failed to hydrate: app patrol/a hydrator uses the same destination: repo=" + dryRepo + ", path=out/a",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := hydratorApp("a", "out/a"), hydratorApp("b", "out/b")
			tc.mutate(a, b)
			objs := append([]client.Object{defaultProject("default"), a, b}, tc.objs...)
			e := newTestEnv(t, objs...)
			_, err := e.process(testCtx(t), keyOf(t, a))
			require.NoError(t, err)
			assert.Empty(t, e.commit.requests())
			assert.Empty(t, e.repo.manifestReqs)
			for name, want := range tc.want {
				cur := e.get(t, name).Status.SourceHydrator.CurrentOperation
				require.NotNil(t, cur, name)
				assert.Equal(t, v1alpha1.HydrateOperationPhaseFailed, cur.Phase, name)
				assert.Contains(t, cur.Message, want, name)
				assert.NotNil(t, cur.FinishedAt)
				assert.Empty(t, cur.DrySHA)
			}
		})
	}
}

// APP-116, APP-130: a render failure of one app fails the whole group; the resolved dry
// SHA is recorded on every app.
func TestReconcileKeyRenderFailureFailsGroup(t *testing.T) {
	t.Parallel()
	a, b, c := hydratorApp("a", "out/a"), hydratorApp("b", "out/b"), hydratorApp("c", "out/c")
	e := newTestEnv(t, defaultProject("default"), a, b, c)
	e.repo.failPaths["dry/b"] = errors.New("kustomize build failed: boom")
	e.repo.failPaths["dry/c"] = errors.New("helm template failed")

	_, err := e.process(testCtx(t), keyOf(t, a))
	require.NoError(t, err)
	assert.Empty(t, e.commit.requests())

	want := map[string]string{
		"a": "Failed to hydrate: cannot hydrate because application patrol/b and 1 more have errors",
		"b": `Failed to hydrate: failed to get manifests: failed to get repo objects for app "patrol/b": kustomize build failed: boom`,
		"c": `Failed to hydrate: failed to get manifests: failed to get repo objects for app "patrol/c": helm template failed`,
	}
	for name, msg := range want {
		s := e.get(t, name).Status.SourceHydrator
		assert.Equal(t, v1alpha1.HydrateOperationPhaseFailed, s.CurrentOperation.Phase)
		assert.Equal(t, msg, s.CurrentOperation.Message)
		assert.Equal(t, shaA, s.CurrentOperation.DrySHA)
		assert.Equal(t, shaA, s.LastComparedDryRevision)
		assert.Nil(t, s.LastSuccessfulOperation)
	}
}

// APP-126, APP-130: group-wide errors (template, credentials, commit) fail every app with
// the specific error.
func TestReconcileKeyGroupErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(e *testEnv)
		want  string
	}{
		{
			name:  "commit",
			setup: func(e *testEnv) { e.commit.err = errors.New("push rejected") },
			want:  "Failed to hydrate: failed to commit hydrated manifests: push rejected",
		},
		{
			name:  "credentials",
			setup: func(e *testEnv) { e.repos.writeErr = errors.New("secret not found") },
			want:  "Failed to hydrate: failed to get hydrator credentials: secret not found",
		},
		{
			name: "template",
			setup: func(e *testEnv) {
				e.h.d.Settings = StaticSettings{Settings: settings.Parse(settings.Input{Namespace: controlNS, IgnorePartOfLabel: true, ConfigMaps: []*corev1.ConfigMap{settingsCM(map[string]string{settings.KeyCommitMessageTemplate: `{{ fail "template exploded" }}`})}})}
			},
			want: "Failed to hydrate: failed to get hydrator commit templated message: failed to execute commit message template",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := hydratorApp("a", "out/a"), hydratorApp("b", "out/b")
			e := newTestEnv(t, defaultProject("default"), a, b)
			tc.setup(e)
			_, err := e.process(testCtx(t), keyOf(t, a))
			require.NoError(t, err)
			for _, name := range []string{"a", "b"} {
				cur := e.get(t, name).Status.SourceHydrator.CurrentOperation
				assert.Equal(t, v1alpha1.HydrateOperationPhaseFailed, cur.Phase)
				assert.Contains(t, cur.Message, tc.want)
				assert.Equal(t, shaA, cur.DrySHA)
			}
		})
	}
}

// APP-125: project-scoped write credentials only when every app belongs to one project.
func TestReconcileKeyWriteCredentialProject(t *testing.T) {
	t.Parallel()
	a, b := hydratorApp("a", "out/a"), hydratorApp("b", "out/b")
	b.Spec.Project = "team"
	e := newTestEnv(t, defaultProject("default"), defaultProject("team"), a, b)
	e.repos.writeCreds = &apiclient.Repository{Username: "bot", Password: "s3cret"}
	_, err := e.process(testCtx(t), keyOf(t, a))
	require.NoError(t, err)
	assert.Equal(t, []string{""}, e.repos.writeProjects)
	reqs := e.commit.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "bot", reqs[0].Repo.Username)
	assert.Equal(t, dryRepo, reqs[0].Repo.URL)
}

// APP-134: a dry source under a GPG policy must have been verified by the repo-server, and a
// failing verification result always fails the hydration. GPG enforcement is on with the
// zero Deps (regression: the zero value used to ignore failing results).
func TestReconcileKeySourceIntegrity(t *testing.T) {
	t.Parallel()
	policy := &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: []string{"ABCDEF0123456789"}},
	}}}}
	failing := &apiclient.SourceIntegrityResult{Checks: []apiclient.SourceIntegrityCheck{{Name: "gpg", Problems: []string{"commit is not signed"}}}}
	passing := &apiclient.SourceIntegrityResult{Checks: []apiclient.SourceIntegrityCheck{{Name: "gpg"}}}
	const (
		notPerformed = `Failed to hydrate: failed to get manifests: source integrity verification required but not performed for app "patrol/a" dry revision "` + shaA + `"`
		failed       = `Failed to hydrate: failed to get manifests: source integrity verification failed for app "patrol/a" dry revision "` + shaA + `": gpg: commit is not signed`
	)
	for _, tc := range []struct {
		name       string
		policy     *v1alpha1.SourceIntegrity
		disableGPG bool
		result     *apiclient.SourceIntegrityResult
		want       string // "" means Hydrated
	}{
		{name: "not performed", policy: policy, want: notPerformed},
		{name: "failed", policy: policy, result: failing, want: failed},
		{name: "passed", policy: policy, result: passing},
		{name: "failed with GPG disabled", policy: policy, disableGPG: true, result: failing, want: failed},
		{name: "failed without criteria", result: failing, want: failed},
		{name: "not performed with GPG disabled", policy: policy, disableGPG: true},
		{name: "no criteria", result: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proj := defaultProject("default")
			proj.Spec.SourceIntegrity = tc.policy.DeepCopy()
			a := hydratorApp("a", "out/a")
			e := newTestEnv(t, proj, a)
			e.h.d.DisableGPG = tc.disableGPG
			e.repo.integrity = tc.result
			_, err := e.process(testCtx(t), keyOf(t, a))
			require.NoError(t, err)
			cur := e.get(t, "a").Status.SourceHydrator.CurrentOperation
			if tc.want == "" {
				assert.Equal(t, v1alpha1.HydrateOperationPhaseHydrated, cur.Phase, cur.Message)
				assert.Len(t, e.commit.requests(), 1)
				return
			}
			assert.Equal(t, v1alpha1.HydrateOperationPhaseFailed, cur.Phase)
			assert.Equal(t, tc.want, cur.Message)
			assert.Empty(t, e.commit.requests())
		})
	}
}

// APP-131: an app already Hydrating keeps its start time when the key is re-processed; its
// snapshot is refreshed to the spec rendered now (it was recorded for an older spec), so the
// status describes what was hydrated.
func TestReconcileKeyKeepsStartTimeOfHydratingApps(t *testing.T) {
	t.Parallel()
	a := hydratorApp("a", "out/a")
	started := metav1.NewTime(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	old := a.Spec.SourceHydrator.DeepCopy()
	old.SyncSource.Path = "out/old"
	a.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{
		StartedAt: &started, Phase: v1alpha1.HydrateOperationPhaseHydrating, SourceHydrator: old,
	}
	e := newTestEnv(t, defaultProject("default"), a)
	_, err := e.process(testCtx(t), keyOf(t, a))
	require.NoError(t, err)
	app := e.get(t, "a")
	cur := app.Status.SourceHydrator.CurrentOperation
	assert.Equal(t, v1alpha1.HydrateOperationPhaseHydrated, cur.Phase)
	assert.True(t, started.Equal(cur.StartedAt), "start time kept: %v", cur.StartedAt)
	assert.Equal(t, app.Spec.SourceHydrator, cur.SourceHydrator)
	assert.Equal(t, app.Spec.SourceHydrator, app.Status.SourceHydrator.LastSuccessfulOperation.SourceHydrator)
	reqs := e.commit.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "out/a", reqs[0].Paths[0].Path)
}

// Apps of other groups, without a source hydrator, or filtered out are not hydrated.
func TestReconcileKeySelectsGroup(t *testing.T) {
	t.Parallel()
	a := hydratorApp("a", "out/a")
	other := hydratorApp("other", "out/other")
	other.Spec.SourceHydrator.SyncSource.TargetBranch = "env/staging"
	filtered := hydratorApp("filtered", "out/filtered")
	// Same group as a, spelled differently (normalized URL).
	spelled := hydratorApp("spelled", "out/spelled")
	spelled.Spec.SourceHydrator.DrySource.RepoURL = "https://GIT.example.com/org/dry"
	e := newTestEnv(t, defaultProject("default"), a, other, filtered, spelled)
	e.h.d.AppFilter = func(app *v1alpha1.Application) bool { return app.Name != "filtered" }

	_, err := e.process(testCtx(t), keyOf(t, a))
	require.NoError(t, err)
	reqs := e.commit.requests()
	require.Len(t, reqs, 1)
	var paths []string
	for _, p := range reqs[0].Paths {
		paths = append(paths, p.Path)
	}
	assert.Equal(t, []string{"out/a", "out/spelled"}, paths)
	assert.Nil(t, e.get(t, "other").Status.SourceHydrator.CurrentOperation)
	assert.Nil(t, e.get(t, "filtered").Status.SourceHydrator.CurrentOperation)
}

func TestReconcileKeyWithoutApps(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	res, err := e.h.ReconcileKey(context.Background(), Key{SourceRepoURL: "x"})
	require.NoError(t, err)
	assert.Zero(t, res)
}

func TestGenericHydrationError(t *testing.T) {
	t.Parallel()
	assert.NoError(t, genericHydrationError(nil))
	assert.EqualError(t, genericHydrationError(map[string]error{"ns/b": errors.New("x")}), "cannot hydrate because application ns/b has an error")
	assert.EqualError(t, genericHydrationError(map[string]error{"ns/c": errors.New("x"), "ns/b": errors.New("y"), "ns/d": errors.New("z")}),
		"cannot hydrate because application ns/b and 2 more have errors")
}

// A request made from a version the latest processing read or wrote is covered by it; a
// request from another version, an external request, or a forced one is processed.
func TestReconcileKeyCoverage(t *testing.T) {
	t.Parallel()
	a := hydratorApp("a", "out/a")
	e := newTestEnv(t, defaultProject("default"), a)
	ctx := testCtx(t)
	k := keyOf(t, a)
	nn := types.NamespacedName{Namespace: controlNS, Name: "a"}
	manifests := func() int { n, _ := e.repo.counts(); return n }

	app := e.get(t, "a")
	before, spec := app.ResourceVersion, specDigest(app)
	e.h.request(k, nn, keyRequest{rv: before, spec: spec, coverable: true})
	_, err := e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	require.Equal(t, 1, manifests())
	after := e.get(t, "a").ResourceVersion
	require.NotEqual(t, before, after)

	for _, rv := range []string{before, after} {
		e.h.request(k, nn, keyRequest{rv: rv, spec: spec, drySHA: shaA, coverable: true})
		_, err = e.h.ReconcileKey(ctx, k)
		require.NoError(t, err)
		assert.Equal(t, 1, manifests(), "request from version %s is covered", rv)
	}

	e.h.forced.Store(k, struct{}{})
	e.h.request(k, nn, keyRequest{rv: after, spec: spec, coverable: true})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 2, manifests(), "a forced request is always processed")

	e.h.request(k, nn, keyRequest{rv: "some-other-version", spec: spec, coverable: true})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 3, manifests(), "a request from an unseen version is processed")

	after = e.get(t, "a").ResourceVersion
	e.h.request(k, nn, keyRequest{rv: after, spec: spec, drySHA: shaB, coverable: true})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 4, manifests(), "a request for another dry revision is processed")

	after = e.get(t, "a").ResourceVersion
	e.h.request(k, nn, keyRequest{rv: after, spec: "another-spec", coverable: true})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 5, manifests(), "a request with a spec the processing did not hydrate is processed")

	after = e.get(t, "a").ResourceVersion
	e.h.request(k, nn, keyRequest{rv: after, coverable: true})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 6, manifests(), "a request without a spec digest is processed")

	after = e.get(t, "a").ResourceVersion
	e.h.request(k, nn, keyRequest{rv: after, spec: spec})
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 7, manifests(), "a time-based request is processed")

	_, err = e.process(ctx, k)
	require.NoError(t, err)
	assert.Equal(t, 8, manifests(), "an external request is processed")
}

// APP-114, APP-116: a spec change made while the group is being hydrated is hydrated next.
// The version written by the processing carries the new spec, but the processing rendered
// the previous one, so the "spec differs" request made from that version is not covered
// (regression: it was dropped as already handled, at every poll, until another writer
// changed the Application). The pending refresh annotation keeps the post-hydration refresh
// from writing a newer version.
func TestReconcileKeySpecChangeDuringHydrationIsNotLost(t *testing.T) {
	t.Parallel()
	a := hydratorApp("a", "out/a")
	a.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeNormal)}
	e := newTestEnv(t, defaultProject("default"), a)
	ctx := testCtx(t)
	k := keyOf(t, a)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: controlNS, Name: "a"}}

	var once sync.Once
	e.repo.onGenerate = func(*apiclient.ManifestRequest) {
		once.Do(func() {
			app := e.get(t, "a")
			app.Spec.SourceHydrator.SyncSource.Path = "out/a2"
			require.NoError(t, e.c.Update(ctx, app))
		})
	}

	_, err := e.h.ReconcileApp(ctx, req)
	require.NoError(t, err)
	_, err = e.h.ReconcileKey(ctx, k)
	require.NoError(t, err)
	require.Len(t, e.commit.requests(), 1)
	s := e.get(t, "a").Status.SourceHydrator
	require.Equal(t, "out/a", s.CurrentOperation.SourceHydrator.SyncSource.Path, "the first hydration rendered the previous spec")

	// The event of the status write, then two polls.
	for range 3 {
		_, err = e.h.ReconcileApp(ctx, req)
		require.NoError(t, err)
		_, err = e.h.ReconcileKey(ctx, k)
		require.NoError(t, err)
	}
	reqs := e.commit.requests()
	require.Len(t, reqs, 2, "the changed spec is hydrated once")
	require.Len(t, reqs[1].Paths, 1)
	assert.Equal(t, "out/a2", reqs[1].Paths[0].Path)
	app := e.get(t, "a")
	s = app.Status.SourceHydrator
	assert.Equal(t, v1alpha1.HydrateOperationPhaseHydrated, s.CurrentOperation.Phase)
	assert.Equal(t, app.Spec.SourceHydrator, s.CurrentOperation.SourceHydrator)
	require.NotNil(t, s.LastSuccessfulOperation)
	assert.Equal(t, app.Spec.SourceHydrator, s.LastSuccessfulOperation.SourceHydrator)
}
