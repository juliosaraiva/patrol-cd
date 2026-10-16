package hydrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

const (
	controlNS = "patrol"
	dryRepo   = "https://git.example.com/org/dry.git"
	shaA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaH      = "cccccccccccccccccccccccccccccccccccccccc"
)

var (
	testScheme = runtime.NewScheme()
	discard    = slog.New(slog.NewTextHandler(io.Discard, nil))

	// envCfg is the envtest API server, set only when KUBEBUILDER_ASSETS is set.
	envCfg *rest.Config
)

func TestMain(m *testing.M) {
	// The test binary doubles as the GIT_ASKPASS helper and the sandbox rlimit helper (the
	// commit-server runs in-process in the integration tests).
	git.MaybeAskpassMain()
	sandbox.MaybeHelperMain()
	ctrllog.SetLogger(logr.Discard())
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	if err := v1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	envCfg = cfg
	return m.Run()
}

func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// --- fixtures ---

func defaultProject(name string, sourceRepos ...string) *v1alpha1.AppProject {
	if len(sourceRepos) == 0 {
		sourceRepos = []string{"*"}
	}
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  sourceRepos,
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		},
	}
}

func hydratorApp(name, path string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "apps"},
			SourceHydrator: &v1alpha1.SourceHydrator{
				DrySource:  v1alpha1.DrySource{RepoURL: dryRepo, TargetRevision: "main", Path: "dry/" + name},
				SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: path},
			},
		},
	}
}

func defaultSettings() *settings.Settings {
	return settings.Parse(settings.Input{Namespace: controlNS})
}

// --- fakes ---

// fakeRepoServer is an in-memory repo-server: the dry branch resolves to rev; each app's
// dry path renders one ConfigMap carrying tracking metadata (which the hydrator strips).
type fakeRepoServer struct {
	mu         sync.Mutex
	rev        string
	content    string
	failPaths  map[string]error
	resolveErr error
	pathsResp  *apiclient.UpdateRevisionForPathsResponse
	integrity  *apiclient.SourceIntegrityResult
	// block, when set, makes GenerateManifest wait until it is closed.
	block chan struct{}
	// onGenerate, when set, runs at the start of every GenerateManifest call (outside the
	// lock), e.g. to change an Application while its group is being hydrated.
	onGenerate func(*apiclient.ManifestRequest)

	manifestReqs []*apiclient.ManifestRequest
	resolveReqs  []*apiclient.ResolveRevisionRequest
	pathsReqs    []*apiclient.UpdateRevisionForPathsRequest
}

var _ apiclient.Client = (*fakeRepoServer)(nil)

func newFakeRepoServer(rev string) *fakeRepoServer {
	return &fakeRepoServer{rev: rev, content: "v1", failPaths: map[string]error{}}
}

func (f *fakeRepoServer) set(rev, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev, f.content = rev, content
}

func (f *fakeRepoServer) GenerateManifest(ctx context.Context, req *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	f.mu.Lock()
	block, hook := f.block, f.onGenerate
	f.manifestReqs = append(f.manifestReqs, req)
	f.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failPaths[req.Source.Path]; err != nil {
		return nil, err
	}
	rev := req.Revision
	if !git.IsCommitSHA(rev) {
		rev = f.rev
	}
	name := strings.ReplaceAll(strings.TrimPrefix(req.Source.Path, "dry/"), "/", "-")
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": req.Namespace,
			"labels":    map[string]any{"app": name},
			"annotations": map[string]any{
				v1alpha1.AnnotationKeyTrackingID:     req.AppName + ":/ConfigMap:" + req.Namespace + "/" + name,
				v1alpha1.AnnotationKeyInstallationID: "install-1",
				"example.com/keep":                   "yes",
			},
		},
		"data": map[string]any{"content": f.content},
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return &apiclient.ManifestResponse{
		Manifests:             []string{string(data)},
		Revision:              rev,
		SourceType:            "Directory",
		Commands:              []string{"render " + req.Source.Path},
		SourceIntegrityResult: f.integrity,
	}, nil
}

func (f *fakeRepoServer) ResolveRevision(_ context.Context, req *apiclient.ResolveRevisionRequest) (*apiclient.ResolveRevisionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveReqs = append(f.resolveReqs, req)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return &apiclient.ResolveRevisionResponse{Revision: f.rev, AmbiguousRevision: req.Revision + " (" + f.rev + ")"}, nil
}

func (f *fakeRepoServer) UpdateRevisionForPaths(_ context.Context, req *apiclient.UpdateRevisionForPathsRequest) (*apiclient.UpdateRevisionForPathsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathsReqs = append(f.pathsReqs, req)
	if f.pathsResp == nil {
		return nil, errors.New("unexpected UpdateRevisionForPaths")
	}
	r := *f.pathsResp
	return &r, nil
}

func (f *fakeRepoServer) GetRevisionMetadata(_ context.Context, req *apiclient.RevisionMetadataRequest) (*apiclient.RevisionMetadata, error) {
	return &apiclient.RevisionMetadata{
		Author:  "Dry Author <dry@example.com>",
		Date:    time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Message: "Update " + req.Revision[:7] + "\n\nBody text.\n\nArgocd-reference-commit-sha: 0123456789abcdef0123456789abcdef01234567\nArgocd-reference-commit-author: Code Author <code@example.com>",
		References: []apiclient.RevisionReference{{
			SHA:    "0123456789abcdef0123456789abcdef01234567",
			Author: "Code Author <code@example.com>",
		}},
	}, nil
}

func (f *fakeRepoServer) counts() (manifests, resolves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.manifestReqs), len(f.resolveReqs)
}

var errNotImplemented = errors.New("not implemented by the fake repo-server")

func (f *fakeRepoServer) GenerateManifestWithFiles(context.Context, *apiclient.ManifestRequest, io.Reader) (*apiclient.ManifestResponse, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) TestRepository(context.Context, *apiclient.TestRepositoryRequest) (*apiclient.TestRepositoryResponse, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) ListRefs(context.Context, *apiclient.ListRefsRequest) (*apiclient.Refs, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) ListOCITags(context.Context, *apiclient.ListRefsRequest) (*apiclient.Refs, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) ListApps(context.Context, *apiclient.ListAppsRequest) (*apiclient.AppList, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) ListPlugins(context.Context) (*apiclient.PluginList, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetAppDetails(context.Context, *apiclient.AppDetailsQuery) (*apiclient.AppDetails, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetRevisionChartDetails(context.Context, *apiclient.ChartDetailsRequest) (*apiclient.ChartDetails, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetOCIMetadata(context.Context, *apiclient.ChartDetailsRequest) (*apiclient.OCIMetadata, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetHelmCharts(context.Context, *apiclient.HelmChartsRequest) (*apiclient.HelmChartsResponse, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetGitFiles(context.Context, *apiclient.GitFilesRequest) (*apiclient.GitFilesResponse, error) {
	return nil, errNotImplemented
}
func (f *fakeRepoServer) GetGitDirectories(context.Context, *apiclient.GitDirectoriesRequest) (*apiclient.GitDirectoriesResponse, error) {
	return nil, errNotImplemented
}

// fakeCommitServer records requests and returns a fixed SHA or error.
type fakeCommitServer struct {
	mu   sync.Mutex
	sha  string
	err  error
	reqs []*commitserver.CommitHydratedManifestsRequest
}

func (f *fakeCommitServer) CommitHydratedManifests(_ context.Context, req *commitserver.CommitHydratedManifestsRequest) (*commitserver.CommitHydratedManifestsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return &commitserver.CommitHydratedManifestsResponse{HydratedSHA: f.sha}, nil
}

func (f *fakeCommitServer) requests() []*commitserver.CommitHydratedManifestsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// fakeRepos resolves anonymous access and records the projects asked for.
type fakeRepos struct {
	mu            sync.Mutex
	writeProjects []string
	writeErr      error
	writeCreds    *apiclient.Repository
}

func (f *fakeRepos) GetRepository(_ context.Context, repoURL, _ string) (*apiclient.Repository, error) {
	return &apiclient.Repository{URL: repoURL}, nil
}

func (f *fakeRepos) GetWriteRepository(_ context.Context, repoURL, project string) (*apiclient.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeProjects = append(f.writeProjects, project)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	if f.writeCreds != nil {
		r := *f.writeCreds
		r.URL = repoURL
		return &r, nil
	}
	return &apiclient.Repository{URL: repoURL}, nil
}

// testEnv is a hydrator over a controller-runtime fake client.
type testEnv struct {
	h      *Hydrator
	c      client.Client
	repo   *fakeRepoServer
	commit *fakeCommitServer
	repos  *fakeRepos
	now    time.Time
}

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&v1alpha1.Application{}).
		WithIndex(&v1alpha1.Application{}, keyIndexField, indexByKey).
		WithObjects(objs...).
		Build()
	e := &testEnv{
		c:      c,
		repo:   newFakeRepoServer(shaA),
		commit: &fakeCommitServer{sha: shaH},
		repos:  &fakeRepos{},
		now:    time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
	}
	h, err := New(Deps{
		Enabled:               true,
		ControlPlaneNamespace: controlNS,
		Client:                c,
		RepoServer:            e.repo,
		CommitServer:          e.commit,
		Repositories:          e.repos,
		WriteCredentials:      e.repos,
		Settings:              StaticSettings{Settings: defaultSettings()},
		Logger:                discard,
		Now:                   func() time.Time { return e.now },
	})
	require.NoError(t, err)
	e.h = h
	return e
}

func (e *testEnv) get(t *testing.T, name string) *v1alpha1.Application {
	t.Helper()
	app := &v1alpha1.Application{}
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKey{Namespace: controlNS, Name: name}, app))
	return app
}

func keyOf(t *testing.T, app *v1alpha1.Application) Key {
	t.Helper()
	k, ok := KeyOf(app)
	require.True(t, ok)
	return k
}

// settingsCM is a patrol-cm ConfigMap with data.
func settingsCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: controlNS},
		Data:       data,
	}
}

// process requests k (as an evaluation would) and processes it.
func (e *testEnv) process(ctx context.Context, k Key) (reconcile.Result, error) {
	e.h.Enqueue(k)
	return e.h.ReconcileKey(ctx, k)
}
