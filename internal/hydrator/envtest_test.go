package hydrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
)

// Integration tests: the hydrator runs in a controller-runtime manager against a real
// kube-apiserver (envtest) with the generated CRDs, renders through a fake repo-server, and
// commits through the real commit-server (HTTP API, typed client) into a local bare
// repository with the git CLI.

const envToken = "hydrator-envtest-token"

type integration struct {
	t      *testing.T
	ctx    context.Context
	ns     string
	c      client.Client
	repo   *fakeRepoServer
	remote *bareRemote
	h      *Hydrator
	mgr    manager.Manager
}

func startIntegration(t *testing.T) *integration {
	t.Helper()
	if envCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest integration test skipped")
	}
	ctx := testCtx(t)
	ns := "hyd-" + rand.String(6)
	c, err := client.New(envCfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	proj := defaultProject("default")
	proj.Namespace = ns
	require.NoError(t, c.Create(ctx, proj))

	in := &integration{t: t, ctx: ctx, ns: ns, c: c, repo: newFakeRepoServer(shaA), remote: newBareRemote(t)}

	workDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	svc := commitserver.NewService(commitserver.ServiceOptions{WorkDir: workDir, AllowLocalRepositories: true, Logger: discard})
	handler, err := commitserver.NewHandler(svc, commitserver.HandlerOptions{Token: envToken, Logger: discard})
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	commitClient, err := commitserver.NewClient(ts.URL, envToken)
	require.NoError(t, err)

	mgr, err := manager.New(envCfg, manager.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	repos := &fakeRepos{}
	h, err := Setup(ctx, mgr, Deps{
		Enabled:               true,
		ControlPlaneNamespace: ns,
		RepoServer:            in.repo,
		CommitServer:          commitClient,
		Repositories:          repos,
		WriteCredentials:      repos,
		Settings:              StaticSettings{Settings: defaultSettings()},
		Processors:            2,
		PollInterval:          300 * time.Millisecond,
		Registry:              prometheus.NewRegistry(),
		Logger:                testLogger(t),
	})
	require.NoError(t, err)
	require.NotNil(t, h)
	in.h = h
	in.mgr = mgr
	return in
}

// start runs the manager until the test ends. Objects created before start are in the
// cache's initial listing, so a group is complete when its key is first processed.
func (in *integration) start() {
	mctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := in.mgr.Start(mctx); err != nil {
			in.t.Errorf("manager: %v", err)
		}
	}()
	in.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func (in *integration) app(name, path string) *v1alpha1.Application {
	app := hydratorApp(name, path)
	app.Namespace = in.ns
	app.Spec.SourceHydrator.SyncSource.RepoURL = in.remote.url()
	return app
}

func (in *integration) create(objs ...client.Object) {
	in.t.Helper()
	for _, o := range objs {
		require.NoError(in.t, in.c.Create(in.ctx, o))
	}
}

func (in *integration) get(name string) *v1alpha1.Application {
	in.t.Helper()
	app := &v1alpha1.Application{}
	require.NoError(in.t, in.c.Get(in.ctx, client.ObjectKey{Namespace: in.ns, Name: name}, app))
	return app
}

// waitFor polls until cond holds for every named app.
func (in *integration) waitFor(desc string, cond func(app *v1alpha1.Application) bool, names ...string) {
	in.t.Helper()
	require.Eventually(in.t, func() bool {
		for _, n := range names {
			app := &v1alpha1.Application{}
			if err := in.c.Get(in.ctx, client.ObjectKey{Namespace: in.ns, Name: n}, app); err != nil || !cond(app) {
				return false
			}
		}
		return true
	}, 90*time.Second, 100*time.Millisecond, "waiting for %s of %v", desc, names)
}

func phaseIs(phase v1alpha1.HydrateOperationPhase, drySHA string) func(*v1alpha1.Application) bool {
	return func(app *v1alpha1.Application) bool {
		cur := app.Status.SourceHydrator.CurrentOperation
		return cur != nil && cur.Phase == phase && (drySHA == "" || cur.DrySHA == drySHA)
	}
}

// APP-113, APP-114, APP-115, APP-116, APP-118..APP-122, APP-132, APP-137: the hydration
// lifecycle of two grouped Applications: Hydrating → Hydrated in one commit, a new dry
// commit produces a new hydrated commit, a dry commit with unchanged output and a hard
// hydrate produce no commit.
func TestEnvHydrationLifecycle(t *testing.T) {
	in := startIntegration(t)
	block := make(chan struct{})
	in.repo.mu.Lock()
	in.repo.block = block
	in.repo.mu.Unlock()

	a, b := in.app("a", "out/a"), in.app("b", "out/b")
	in.create(a, b)
	in.start()

	// Hydrating: start time and configuration snapshot recorded before rendering.
	in.waitFor("Hydrating", phaseIs(v1alpha1.HydrateOperationPhaseHydrating, ""), "a", "b")
	for _, n := range []string{"a", "b"} {
		cur := in.get(n).Status.SourceHydrator.CurrentOperation
		require.NotNil(t, cur.StartedAt)
		assert.Nil(t, cur.FinishedAt)
		assert.Equal(t, in.get(n).Spec.SourceHydrator, cur.SourceHydrator)
	}
	in.repo.mu.Lock()
	in.repo.block = nil
	in.repo.mu.Unlock()
	close(block)

	in.waitFor("Hydrated", phaseIs(v1alpha1.HydrateOperationPhaseHydrated, shaA), "a", "b")
	head1 := in.remote.head("env/prod")
	assert.Equal(t, 2, in.remote.count("env/prod"), "orphan initial commit plus one hydrated commit for both apps")
	assert.Equal(t, "Initial commit for env/prod", in.remote.git("log", "--format=%s", "-1", "env/prod~1"))
	for _, n := range []string{"a", "b"} {
		app := in.get(n)
		s := app.Status.SourceHydrator
		assert.Equal(t, head1, s.CurrentOperation.HydratedSHA)
		require.NotNil(t, s.CurrentOperation.FinishedAt)
		assert.Equal(t, &v1alpha1.SuccessfulHydrateOperation{DrySHA: shaA, HydratedSHA: head1, SourceHydrator: app.Spec.SourceHydrator}, s.LastSuccessfulOperation)
		assert.Equal(t, shaA, s.LastComparedDryRevision)
		assert.Equal(t, "normal", app.Annotations[v1alpha1.AnnotationKeyRefresh])
	}
	for _, f := range []string{"out/a/manifest.yaml", "out/a/README.md", "out/a/hydrator.metadata", "out/b/manifest.yaml", "hydrator.metadata", ".gitattributes"} {
		assert.True(t, in.remote.has("env/prod", f), f)
	}
	manifest := in.remote.show("env/prod", "out/a/manifest.yaml")
	assert.Contains(t, manifest, "content: v1")
	assert.NotContains(t, manifest, v1alpha1.AnnotationKeyTrackingID)
	assert.Contains(t, in.remote.show("env/prod", "out/a/README.md"), "git checkout "+shaA)
	var md map[string]any
	require.NoError(t, json.Unmarshal([]byte(in.remote.show("env/prod", "hydrator.metadata")), &md))
	assert.Equal(t, shaA, md["drySha"])
	assert.Equal(t, dryRepo, md["repoURL"])
	assert.Equal(t, "Update aaaaaaa", md["subject"])
	msg := in.remote.git("log", "--format=%B", "-1", "env/prod")
	assert.True(t, strings.HasPrefix(msg, "aaaaaaa: Update aaaaaaa"), msg)
	assert.Equal(t, "Patrol CD <patrol-cd@example.com>", in.remote.git("log", "--format=%an <%ae>", "-1", "env/prod"))
	assert.Equal(t, shaA, in.remote.note(head1))

	// A new dry commit is detected by polling and hydrated into a new commit.
	in.repo.set(shaB, "v2")
	in.waitFor("hydration of the new dry commit", phaseIs(v1alpha1.HydrateOperationPhaseHydrated, shaB), "a", "b")
	head2 := in.remote.head("env/prod")
	assert.NotEqual(t, head1, head2)
	assert.Equal(t, 3, in.remote.count("env/prod"))
	assert.Contains(t, in.remote.show("env/prod", "out/b/manifest.yaml"), "content: v2")
	assert.Equal(t, head2, in.get("a").Status.SourceHydrator.CurrentOperation.HydratedSHA)

	// APP-122: a dry commit that does not change the output creates no commit; the note
	// records the new dry SHA on the unchanged head.
	const shaD = "dddddddddddddddddddddddddddddddddddddddd"
	in.repo.set(shaD, "v2")
	in.waitFor("hydration without changes", phaseIs(v1alpha1.HydrateOperationPhaseHydrated, shaD), "a", "b")
	assert.Equal(t, head2, in.remote.head("env/prod"))
	assert.Equal(t, 3, in.remote.count("env/prod"))
	assert.Equal(t, head2, in.get("b").Status.SourceHydrator.CurrentOperation.HydratedSHA)
	assert.Equal(t, shaD, in.remote.note(head2))

	// APP-115: a hard hydrate re-runs the hydration (and the commit-server) without a new
	// commit, and the annotation is consumed.
	manifestsBefore, _ := in.repo.counts()
	app := in.get("a")
	app.Annotations[v1alpha1.AnnotationKeyHydrate] = "hard"
	require.NoError(t, in.c.Update(in.ctx, app))
	// Rendering starts after the group is marked Hydrating, so once both apps were rendered
	// again, Hydrated means the hard hydration finished.
	require.Eventually(t, func() bool {
		n, _ := in.repo.counts()
		return n >= manifestsBefore+2
	}, 90*time.Second, 50*time.Millisecond, "hard hydration renders the group")
	in.waitFor("hard hydration", func(app *v1alpha1.Application) bool {
		_, pending := app.Annotations[v1alpha1.AnnotationKeyHydrate]
		return !pending && phaseIs(v1alpha1.HydrateOperationPhaseHydrated, shaD)(app)
	}, "a", "b")
	assert.Equal(t, head2, in.remote.head("env/prod"))
	assert.Equal(t, 3, in.remote.count("env/prod"))
}

// APP-116, APP-117: one invalid Application fails the whole group and nothing is pushed.
func TestEnvHydrationFailureFailsGroup(t *testing.T) {
	in := startIntegration(t)
	restricted := defaultProject("restricted", in.remote.url())
	restricted.Namespace = in.ns
	a, b := in.app("a", "out/a"), in.app("b", "out/b")
	b.Spec.Project = "restricted"
	in.create(restricted, a, b)
	in.start()

	in.waitFor("Failed", phaseIs(v1alpha1.HydrateOperationPhaseFailed, ""), "a", "b")
	assert.Equal(t, fmt.Sprintf("Failed to hydrate: cannot hydrate because application %s/b has an error", in.ns),
		in.get("a").Status.SourceHydrator.CurrentOperation.Message)
	assert.Equal(t, "Failed to hydrate: application repo "+dryRepo+" is not permitted in project 'restricted'",
		in.get("b").Status.SourceHydrator.CurrentOperation.Message)
	_, err := in.remote.tryGit("rev-parse", "--verify", "refs/heads/env/prod")
	assert.Error(t, err, "nothing pushed")
}

// APP-120, APP-123: with hydrateTo the commit lands on the staging branch, created from the
// bootstrapped sync branch.
func TestEnvHydrateToStagingBranch(t *testing.T) {
	in := startIntegration(t)
	a := in.app("a", "out/a")
	a.Spec.SourceHydrator.HydrateTo = &v1alpha1.HydrateTo{TargetBranch: "env/prod-next"}
	in.create(a)
	in.start()

	in.waitFor("Hydrated", phaseIs(v1alpha1.HydrateOperationPhaseHydrated, shaA), "a")
	assert.Equal(t, 1, in.remote.count("env/prod"), "sync branch bootstrapped as an orphan")
	assert.False(t, in.remote.has("env/prod", "out/a/manifest.yaml"))
	assert.Equal(t, 2, in.remote.count("env/prod-next"))
	assert.True(t, in.remote.has("env/prod-next", "out/a/manifest.yaml"))
	assert.Equal(t, in.remote.head("env/prod-next"), in.get("a").Status.SourceHydrator.CurrentOperation.HydratedSHA)
}

// testLogger logs to the test log with HYDRATOR_TEST_LOG=1, and discards otherwise.
func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("HYDRATOR_TEST_LOG") != "1" {
		return discard
	}
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tWriter struct{ t *testing.T }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// --- bare repository fixture ---

type bareRemote struct {
	t    *testing.T
	bare string
	env  []string
}

func newBareRemote(t *testing.T) *bareRemote {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	r := &bareRemote{t: t, bare: filepath.Join(dir, "hydrated.git"), env: []string{
		"HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
	}}
	cmd := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	cmd.Env = append(cmd.Environ(), r.env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return r
}

func (r *bareRemote) url() string { return "file://" + r.bare }

func (r *bareRemote) tryGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.bare
	cmd.Env = append(cmd.Environ(), r.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.Join(err, errors.New(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *bareRemote) git(args ...string) string {
	r.t.Helper()
	out, err := r.tryGit(args...)
	require.NoError(r.t, err, "git %v", args)
	return out
}

func (r *bareRemote) head(branch string) string { return r.git("rev-parse", "refs/heads/"+branch) }

func (r *bareRemote) count(branch string) int {
	var n int
	_, err := fmt.Sscanf(r.git("rev-list", "--count", "refs/heads/"+branch), "%d", &n)
	require.NoError(r.t, err)
	return n
}

func (r *bareRemote) show(branch, path string) string {
	return r.git("show", "refs/heads/"+branch+":"+path)
}

func (r *bareRemote) has(branch, path string) bool {
	_, err := r.tryGit("cat-file", "-e", "refs/heads/"+branch+":"+path)
	return err == nil
}

func (r *bareRemote) note(sha string) string {
	var n struct {
		DrySHA string `json:"drySha"`
	}
	require.NoError(r.t, json.Unmarshal([]byte(r.git("notes", "--ref="+commitserver.NoteNamespace, "show", sha)), &n))
	return n.DrySHA
}
