package app_test

// End-to-end harness of the app and appset command groups: the real patrol API server of
// internal/cli/clitest (envtest, auth, RBAC, settings) with the applications and
// applicationsets services added, the real controller tree API (handler and client) over a
// state provider computed from envtest, an in-process repo-server fake and a controller
// stand-in that reconciles Applications and executes Operations against envtest.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // fake commit SHAs, not a security use
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/app"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
)

const (
	inCluster     = "https://kubernetes.default.svc"
	repoURL       = "https://github.com/org/repo"
	internalToken = "clitest-internal-token-0123456789"
	// holdAnnotation makes the controller stand-in keep new Operations of the Application
	// Running until they are canceled (terminate-op).
	holdAnnotation = "clitest.patrol/hold-operations"
)

// e2ePolicy: alice is read-only; bob manages applications and ApplicationSets of project
// team-a; nobody has no grant; admin is role:admin (built in).
const e2ePolicy = `g, alice, role:readonly
p, bob, applications, *, team-a/*, allow
p, bob, applicationsets, *, team-a/*, allow
p, bob, projects, get, team-a, allow
`

// e2eEnv is a running API server with the application routes and a controller stand-in.
type e2eEnv struct {
	*clitest.Env
	repo   *repoFake
	mapper meta.RESTMapper
	// dest is the destination namespace of the test Applications.
	dest string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	lastErr error
}

func newE2E(t *testing.T) *e2eEnv {
	t.Helper()
	x := &e2eEnv{repo: newRepoFake()}
	env := clitest.New(t, clitest.Options{
		Registrars: []cli.Registrar{app.Register},
		RBAC:       map[string]string{"policy.csv": e2ePolicy},
		RepoServer: x.repo,
		Services:   x.services,
	})
	x.Env = env
	x.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(env.Clientset.Discovery()))
	ns, err := env.Clientset.CoreV1().Namespaces().Create(env.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "patrol-dest-"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	x.dest = ns.Name
	for _, p := range []string{"default", "team-a"} {
		env.CreateProject(&v1alpha1.AppProject{
			ObjectMeta: metav1.ObjectMeta{Name: p},
			Spec: v1alpha1.AppProjectSpec{
				SourceRepos:  []string{"*"},
				Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			},
		})
	}
	x.ctx, x.cancel = context.WithCancel(env.Context())
	x.done = make(chan struct{})
	go x.runController()
	t.Cleanup(func() {
		x.cancel()
		<-x.done
		if t.Failed() {
			if err := x.controllerErr(); err != nil {
				t.Logf("controller stand-in: last error: %v", err)
			}
		}
	})
	return x
}

// services adds the applications and applicationsets services to the server.
func (x *e2eEnv) services(e *clitest.Env) []apiserver.Service {
	t := e.T
	ctx := e.Context()
	logger := slog.New(slog.DiscardHandler)

	cache, err := crcache.New(e.RestConfig, crcache.Options{Scheme: clitest.Scheme, DefaultNamespaces: map[string]crcache.Config{e.Namespace: {}}})
	require.NoError(t, err)
	inf, err := cache.GetInformer(ctx, &v1alpha1.Application{})
	require.NoError(t, err)
	broadcaster := applications.NewBroadcaster(0)
	_, err = inf.AddEventHandler(broadcaster)
	require.NoError(t, err)
	go func() { _ = cache.Start(ctx) }()
	require.True(t, cache.WaitForCacheSync(ctx))

	lister, err := repository.NewLister(e.Dynamic, e.Clientset, repository.ListerOptions{Namespace: e.Namespace, ResyncPeriod: time.Minute})
	require.NoError(t, err)
	require.NoError(t, lister.Start(ctx))
	resolver, err := repository.NewResolver(lister, repository.Options{RepoServer: x.repo, Logger: logger})
	require.NoError(t, err)

	th, err := treeapi.NewHandler(&treeState{x: x}, treeapi.HandlerOptions{Token: internalToken})
	require.NoError(t, err)
	tsrv := httptest.NewServer(th)
	t.Cleanup(tsrv.Close)
	tc, err := treeapi.NewClient(treeapi.StaticRouter{URL: tsrv.URL}, internalToken)
	require.NoError(t, err)

	apps := applications.New(applications.Deps{
		Config:       applications.Config{Namespace: e.Namespace, RefreshTimeout: 15 * time.Second, RefreshPollInterval: 50 * time.Millisecond},
		Client:       e.Kube,
		Settings:     e.Settings,
		Events:       broadcaster,
		Repositories: resolver,
		RepoServer:   x.repo,
		Resources: applications.ResourceDeps{
			Tree: tc,
			Clusters: applications.ClusterConfigsFunc(func(context.Context, *v1alpha1.Cluster) (*rest.Config, error) {
				return rest.CopyConfig(e.RestConfig), nil
			}),
			TreePollInterval: 100 * time.Millisecond,
		},
		Logger: logger,
	})
	appsets := applicationsets.New(applicationsets.Deps{
		Config: applicationsets.Config{Namespace: e.Namespace},
		Reader: e.Kube, APIReader: e.Kube, Writer: e.Kube, Watcher: e.Kube,
		Generator: applicationsets.NewGenerator(generators.Config{Client: e.Kube, Namespace: e.Namespace}),
		Logger:    logger,
	})
	return []apiserver.Service{apps, appsets}
}

func (x *e2eEnv) setErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.lastErr = err
}

func (x *e2eEnv) controllerErr() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.lastErr
}

// token returns a login session token of a local account.
func (x *e2eEnv) token(user string) string { return x.LoginToken(user) }

// app reads an Application from Kubernetes (nil when missing).
func (x *e2eEnv) app(name string) *v1alpha1.Application {
	a := &v1alpha1.Application{}
	if err := x.Kube.Get(x.Context(), client.ObjectKey{Namespace: x.Namespace, Name: name}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		x.T.Fatal(err)
	}
	return a
}

// waitReconciled waits until the controller stand-in compared the current generation of
// Application name.
func (x *e2eEnv) waitReconciled(name string) {
	x.T.Helper()
	x.WaitFor("application "+name+" reconciled", func() bool {
		a := x.app(name)
		if err := x.controllerErr(); err != nil {
			x.T.Logf("controller stand-in: %v", err)
		}
		return a != nil && a.Status.ReconciledAt != nil && a.Status.ObservedGeneration == a.Generation
	})
}

// configMap reads a ConfigMap of the destination namespace (nil when missing).
func (x *e2eEnv) configMap(name string) *corev1.ConfigMap {
	cm, err := x.Clientset.CoreV1().ConfigMaps(x.dest).Get(x.Context(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		x.T.Fatal(err)
	}
	return cm
}

// createArgs are the flags creating Application name from path of the test repository.
func (x *e2eEnv) createArgs(name, project, pth string, extra ...string) []string {
	return append([]string{"app", "create", name, "--repo", repoURL, "--path", pth, "--dest-server", inCluster,
		"--dest-namespace", x.dest, "--project", project}, extra...)
}

// ---------------------------------------------------------------------------------------
// Repo-server fake

// fakeSHA is the commit a revision resolves to (a 40-hex revision is a commit already).
func fakeSHA(rev string) string {
	if rev == "" {
		rev = "HEAD"
	}
	if commitRE.MatchString(rev) {
		return strings.ToLower(rev)
	}
	sum := sha1.Sum([]byte(rev)) //nolint:gosec // fake commit SHA
	return hex.EncodeToString(sum[:])
}

var commitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// repoFake serves manifests of the test repository by path (and optionally by revision)
// and renders uploaded local files (YAML/JSON documents of the archive).
type repoFake struct {
	*clitest.FakeRepoServer
	mu        sync.Mutex
	manifests map[string][]string // path → JSON manifests
	byRev     map[string][]string // path@sha → JSON manifests
	uploads   [][]string          // file names of each GenerateManifestWithFiles archive
}

func newRepoFake() *repoFake {
	return &repoFake{FakeRepoServer: &clitest.FakeRepoServer{}, manifests: map[string][]string{}, byRev: map[string][]string{}}
}

// set sets the manifests (YAML or JSON objects) of a path, for every revision or for one.
func (r *repoFake) set(t *testing.T, pth, rev string, objs ...string) {
	t.Helper()
	var out []string
	for _, o := range objs {
		j, err := yaml.YAMLToJSON([]byte(o))
		require.NoError(t, err)
		out = append(out, string(j))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rev == "" {
		r.manifests[pth] = out
		return
	}
	r.byRev[pth+"@"+fakeSHA(rev)] = out
}

func (r *repoFake) manifestsFor(pth, rev string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.byRev[pth+"@"+fakeSHA(rev)]; ok {
		return append([]string(nil), m...)
	}
	return append([]string(nil), r.manifests[pth]...)
}

func (r *repoFake) lastUpload() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.uploads) == 0 {
		return nil
	}
	return r.uploads[len(r.uploads)-1]
}

// GenerateManifest implements the repo-server client.
func (r *repoFake) GenerateManifest(_ context.Context, req *reposerver.ManifestRequest) (*reposerver.ManifestResponse, error) {
	return &reposerver.ManifestResponse{Manifests: r.manifestsFor(req.Source.Path, req.Revision), Revision: fakeSHA(req.Revision), SourceType: "Directory"}, nil
}

// GenerateManifestWithFiles renders the YAML/JSON files of the uploaded archive.
func (r *repoFake) GenerateManifestWithFiles(_ context.Context, _ *reposerver.ManifestRequest, files io.Reader) (*reposerver.ManifestResponse, error) {
	if c, ok := files.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	gz, err := gzip.NewReader(io.LimitReader(files, 64<<20))
	if err != nil {
		return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: err.Error()}
	}
	tr := tar.NewReader(gz)
	var names, manifests []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: err.Error()}
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		names = append(names, h.Name)
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, err
		}
		switch path.Ext(h.Name) {
		case ".yaml", ".yml", ".json":
		default:
			continue
		}
		for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(b), -1) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			j, err := yaml.YAMLToJSON([]byte(doc))
			if err != nil {
				return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: err.Error()}
			}
			manifests = append(manifests, string(j))
		}
	}
	sort.Strings(names)
	r.mu.Lock()
	r.uploads = append(r.uploads, names)
	r.mu.Unlock()
	return &reposerver.ManifestResponse{Manifests: manifests, SourceType: "Directory"}, nil
}

// ResolveRevision implements the repo-server client.
func (r *repoFake) ResolveRevision(_ context.Context, req *reposerver.ResolveRevisionRequest) (*reposerver.ResolveRevisionResponse, error) {
	return &reposerver.ResolveRevisionResponse{Revision: fakeSHA(req.Revision), AmbiguousRevision: req.Revision}, nil
}

// GetAppDetails implements the repo-server client.
func (r *repoFake) GetAppDetails(context.Context, *reposerver.AppDetailsQuery) (*reposerver.AppDetails, error) {
	return &reposerver.AppDetails{Type: "Directory", Directory: &reposerver.DirectoryAppSpec{}}, nil
}

// GetRevisionMetadata implements the repo-server client.
func (r *repoFake) GetRevisionMetadata(_ context.Context, req *reposerver.RevisionMetadataRequest) (*reposerver.RevisionMetadata, error) {
	return &reposerver.RevisionMetadata{Author: "Dev <dev@example.com>", Message: "commit " + req.Revision, Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

// ---------------------------------------------------------------------------------------
// Desired and live state

// managed returns the managed resources of app: the target manifests of its source (the
// destination namespace defaulted) paired with their live objects in envtest.
func (x *e2eEnv) managed(ctx context.Context, a *v1alpha1.Application, rev string) ([]compare.ManagedResource, error) {
	src := sourceOf(a)
	targets, err := x.objects(src.Path, rev, nil, a.Spec.Destination.Namespace)
	if err != nil {
		return nil, err
	}
	out := make([]compare.ManagedResource, 0, len(targets))
	for _, tgt := range targets {
		live, gvk, err := x.live(ctx, tgt)
		if err != nil {
			return nil, err
		}
		mr := compare.ManagedResource{
			Key:     compare.ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: tgt.GetNamespace(), Name: tgt.GetName()},
			Version: gvk.Version, Target: tgt, Live: live,
		}
		mr.Diff = diff.Result{Modified: live == nil || !subset(withoutMeta(tgt.Object), live.Object)}
		if live != nil {
			mr.Diff.NormalizedLive, _ = json.Marshal(live.Object)
		}
		mr.Diff.PredictedLive, _ = json.Marshal(tgt.Object)
		out = append(out, mr)
	}
	return out, nil
}

// objects decodes the manifests of pth at rev (or the given local manifests).
func (x *e2eEnv) objects(pth, rev string, local []string, ns string) ([]*unstructured.Unstructured, error) {
	raw := local
	if raw == nil {
		raw = x.repo.manifestsFor(pth, rev)
	}
	out := make([]*unstructured.Unstructured, 0, len(raw))
	for _, m := range raw {
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON([]byte(m)); err != nil {
			return nil, err
		}
		if u.GetNamespace() == "" {
			u.SetNamespace(ns)
		}
		out = append(out, u)
	}
	return out, nil
}

func (x *e2eEnv) resourceFor(u *unstructured.Unstructured) (schema.GroupVersionResource, schema.GroupVersionKind, error) {
	gvk := u.GroupVersionKind()
	m, err := x.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, gvk, err
	}
	return m.Resource, gvk, nil
}

func (x *e2eEnv) live(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, schema.GroupVersionKind, error) {
	gvr, gvk, err := x.resourceFor(u)
	if err != nil {
		return nil, gvk, err
	}
	live, err := x.Dynamic.Resource(gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, gvk, nil
	}
	return live, gvk, err
}

func withoutMeta(obj map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		if k != "metadata" {
			out[k] = v
		}
	}
	return out
}

// subset reports whether every field of want is in have with the same value.
func subset(want, have any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subset(v, h[k]) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !subset(w[i], h[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(want) == fmt.Sprint(have)
	}
}

// treeState is the controller state behind the real tree API handler.
type treeState struct{ x *e2eEnv }

func (s *treeState) getApp(ctx context.Context, k treeapi.AppKey) (*v1alpha1.Application, error) {
	a := &v1alpha1.Application{}
	if err := s.x.Kube.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: k.Name}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: application %s", treeapi.ErrNotFound, k)
		}
		return nil, err
	}
	return a, nil
}

func (s *treeState) Tree(ctx context.Context, k treeapi.AppKey) (*compare.ApplicationTree, error) {
	a, err := s.getApp(ctx, k)
	if err != nil {
		return nil, err
	}
	managed, err := s.x.managed(ctx, a, sourceOf(a).TargetRevision)
	if err != nil {
		return nil, err
	}
	tree := &compare.ApplicationTree{}
	for _, m := range managed {
		if m.Live == nil {
			continue
		}
		created := m.Live.GetCreationTimestamp()
		tree.Nodes = append(tree.Nodes, compare.ResourceNode{
			ResourceRef: compare.ResourceRef{Group: m.Key.Group, Version: m.Version, Kind: m.Key.Kind, Namespace: m.Key.Namespace, Name: m.Key.Name, UID: string(m.Live.GetUID())},
			Health:      &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy},
			CreatedAt:   &created, ResourceVersion: m.Live.GetResourceVersion(),
		})
	}
	return tree, nil
}

func (s *treeState) ManagedResources(ctx context.Context, k treeapi.AppKey) ([]compare.ManagedResource, error) {
	a, err := s.getApp(ctx, k)
	if err != nil {
		return nil, err
	}
	return s.x.managed(ctx, a, sourceOf(a).TargetRevision)
}

func (s *treeState) ServerSideDiff(_ context.Context, _ treeapi.AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error) {
	out := make([]diff.Result, len(live))
	for i := range live {
		enc := func(u *unstructured.Unstructured) []byte {
			if u == nil {
				return []byte("null")
			}
			b, _ := json.Marshal(u.Object)
			return b
		}
		modified := live[i] == nil || target[i] == nil || !subset(withoutMeta(target[i].Object), live[i].Object)
		predicted := enc(target[i])
		if !modified {
			predicted = enc(live[i])
		}
		out[i] = diff.Result{NormalizedLive: enc(live[i]), PredictedLive: predicted, Modified: modified}
	}
	return out, nil
}

func (s *treeState) LiveResource(ctx context.Context, k treeapi.AppKey, ref compare.ResourceRef) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind})
	u.SetNamespace(ref.Namespace)
	u.SetName(ref.Name)
	live, _, err := s.x.live(ctx, u)
	if err != nil {
		return nil, err
	}
	if live == nil {
		return nil, treeapi.ErrNotFound
	}
	return live, nil
}

func (s *treeState) SensitiveAnnotations() []string { return nil }

// ---------------------------------------------------------------------------------------
// Controller stand-in

func (x *e2eEnv) runController() {
	defer close(x.done)
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-x.ctx.Done():
			return
		case <-tick.C:
		}
		x.setErr(x.reconcileAll(x.ctx))
	}
}

func (x *e2eEnv) reconcileAll(ctx context.Context) error {
	var ops v1alpha1.OperationList
	if err := x.Kube.List(ctx, &ops, client.InNamespace(x.Namespace)); err != nil {
		return err
	}
	for i := range ops.Items {
		if err := x.runOperation(ctx, &ops.Items[i]); err != nil && !apierrors.IsConflict(err) {
			x.setErr(err)
		}
	}
	var apps v1alpha1.ApplicationList
	if err := x.Kube.List(ctx, &apps, client.InNamespace(x.Namespace)); err != nil {
		return err
	}
	for i := range apps.Items {
		if err := x.reconcileApp(ctx, &apps.Items[i], false); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			x.setErr(err)
		}
	}
	var sets v1alpha1.ApplicationSetList
	if err := x.Kube.List(ctx, &sets, client.InNamespace(x.Namespace)); err != nil {
		return err
	}
	for i := range sets.Items {
		if err := x.reconcileAppSet(ctx, &sets.Items[i]); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			x.setErr(err)
		}
	}
	return nil
}

// reconcileApp compares an Application (new, changed, refresh requested or forced) and
// writes its status, then clears the refresh annotation; a deleting Application loses its
// finalizers (cascade deletion of the stand-in).
func (x *e2eEnv) reconcileApp(ctx context.Context, a *v1alpha1.Application, force bool) error {
	if a.DeletionTimestamp != nil {
		if len(a.Finalizers) == 0 {
			return nil
		}
		return x.Kube.Patch(ctx, a, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
	}
	_, refresh := a.Annotations[v1alpha1.AnnotationKeyRefresh]
	if !force && !refresh && a.Status.ReconciledAt != nil && a.Status.ObservedGeneration == a.Generation {
		return nil
	}
	src := sourceOf(a)
	managed, err := x.managed(ctx, a, src.TargetRevision)
	if err != nil {
		return err
	}
	now := metav1.Now()
	st := &a.Status
	st.Resources = nil
	synced, healthy := true, true
	for _, m := range managed {
		rs := v1alpha1.ResourceStatus{Group: m.Key.Group, Version: m.Version, Kind: m.Key.Kind, Namespace: m.Key.Namespace, Name: m.Key.Name,
			Status: v1alpha1.SyncStatusCodeSynced, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}}
		if m.Diff.Modified {
			rs.Status, synced = v1alpha1.SyncStatusCodeOutOfSync, false
		}
		if m.Live == nil {
			rs.Health, healthy = &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusMissing}, false
		}
		st.Resources = append(st.Resources, rs)
	}
	st.Sync.Status = v1alpha1.SyncStatusCodeSynced
	if !synced {
		st.Sync.Status = v1alpha1.SyncStatusCodeOutOfSync
	}
	st.Sync.ComparedTo = v1alpha1.ComparedTo{Source: a.Spec.Source.DeepCopy(), Sources: a.Spec.Sources.DeepCopy(), Destination: a.Spec.Destination}
	health := v1alpha1.HealthStatusHealthy
	if !healthy {
		health = v1alpha1.HealthStatusMissing
	}
	if st.Health.Status != health {
		st.Health = v1alpha1.AppHealthStatus{Status: health, LastTransitionTime: &now}
	}
	st.SourceType = v1alpha1.ApplicationSourceTypeDirectory
	st.ReconciledAt = &now
	st.ObservedGeneration = a.Generation
	if err := x.Kube.Status().Update(ctx, a); err != nil {
		return err
	}
	if refresh {
		return x.Kube.Patch(ctx, a, client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"`+v1alpha1.AnnotationKeyRefresh+`":null,"`+v1alpha1.AnnotationKeyHydrate+`":null}}}`)))
	}
	return nil
}

// runOperation executes a non-terminal Operation: canceled ones end Cancelled, held ones
// stay Running, others apply the target manifests (of the requested revision, the rollback
// history entry or the local manifests) to envtest and succeed.
func (x *e2eEnv) runOperation(ctx context.Context, op *v1alpha1.Operation) error {
	if op.IsTerminal() {
		return nil
	}
	a := &v1alpha1.Application{}
	if err := x.Kube.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Spec.Application}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return x.finish(ctx, op, nil, v1alpha1.OperationFailed, "application not found", nil)
		}
		return err
	}
	if op.Spec.Cancel {
		return x.finish(ctx, op, a, v1alpha1.OperationCancelled, "Operation terminated", nil)
	}
	if a.Annotations[holdAnnotation] == "true" {
		if op.Status.Phase == v1alpha1.OperationRunning {
			return nil
		}
		now := metav1.Now()
		op.Status.Phase, op.Status.Message, op.Status.StartedAt = v1alpha1.OperationRunning, "one or more tasks are running", &now
		if err := x.Kube.Status().Update(ctx, op); err != nil {
			return err
		}
		a.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: op.Name, Phase: v1alpha1.OperationRunning, Message: op.Status.Message, StartedAt: &now}
		return x.Kube.Status().Update(ctx, a)
	}
	sync := op.Spec.Sync
	if sync == nil {
		return x.finish(ctx, op, a, v1alpha1.OperationError, "no sync operation", nil)
	}
	src := sourceOf(a)
	rev := firstNonEmpty(sync.Revision, src.TargetRevision)
	if op.Spec.Rollback != nil {
		found := false
		for _, h := range a.Status.History {
			if h.ID == op.Spec.Rollback.ID && h.Source != nil {
				src, rev, found = *h.Source.DeepCopy(), h.Revision, true
			}
		}
		if !found {
			return x.finish(ctx, op, a, v1alpha1.OperationFailed, "history entry not found", nil)
		}
	}
	targets, err := x.objects(src.Path, rev, sync.Manifests, a.Spec.Destination.Namespace)
	if err != nil {
		return x.finish(ctx, op, a, v1alpha1.OperationError, err.Error(), nil)
	}
	result := &v1alpha1.SyncOperationResult{Revision: fakeSHA(rev), Source: src.DeepCopy()}
	if sync.Manifests != nil {
		result.Revision = ""
	}
	for _, tgt := range targets {
		gvr, gvk, err := x.resourceFor(tgt)
		if err != nil {
			return x.finish(ctx, op, a, v1alpha1.OperationError, err.Error(), nil)
		}
		if !selected(sync.Resources, gvk.Group, gvk.Kind, tgt.GetNamespace(), tgt.GetName()) {
			continue
		}
		rr := v1alpha1.ResourceResult{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: tgt.GetNamespace(), Name: tgt.GetName(),
			Status: v1alpha1.ResultCodeSynced, SyncPhase: v1alpha1.SyncPhaseSync, HookPhase: v1alpha1.OperationRunning}
		verb := "configured"
		if live, _, _ := x.live(ctx, tgt); live == nil {
			verb = "created"
		}
		if sync.DryRun {
			verb += " (dry run)"
		} else if _, err := x.Dynamic.Resource(gvr).Namespace(tgt.GetNamespace()).Apply(ctx, tgt.GetName(), tgt, metav1.ApplyOptions{FieldManager: "patrol-standin", Force: true}); err != nil {
			rr.Status, rr.Message = v1alpha1.ResultCodeSyncFailed, err.Error()
			result.Resources = append(result.Resources, rr)
			return x.finish(ctx, op, a, v1alpha1.OperationFailed, "one or more objects failed to apply", result)
		}
		rr.Message = strings.ToLower(gvk.Kind) + "/" + tgt.GetName() + " " + verb
		result.Resources = append(result.Resources, rr)
	}
	return x.finish(ctx, op, a, v1alpha1.OperationSucceeded, "successfully synced (all tasks run)", result)
}

func selected(res []v1alpha1.SyncOperationResource, group, kind, ns, name string) bool {
	if len(res) == 0 {
		return true
	}
	for _, r := range res {
		if r.Group == group && r.Kind == kind && r.Name == name && (r.Namespace == "" || r.Namespace == ns) {
			return true
		}
	}
	return false
}

// finish completes op and mirrors it into the Application status (operation state,
// history of non-dry-run syncs, then a fresh comparison).
func (x *e2eEnv) finish(ctx context.Context, op *v1alpha1.Operation, a *v1alpha1.Application, phase v1alpha1.OperationPhase, msg string, result *v1alpha1.SyncOperationResult) error {
	now := metav1.Now()
	started := op.Status.StartedAt
	if started == nil {
		started = &now
	}
	op.Status.Phase, op.Status.Message, op.Status.StartedAt, op.Status.FinishedAt, op.Status.SyncResult = phase, msg, started, &now, result
	op.Status.ObservedGeneration = op.Generation
	if err := x.Kube.Status().Update(ctx, op); err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	state := &v1alpha1.ApplicationOperationState{OperationName: op.Name, Phase: phase, Message: msg, StartedAt: started, FinishedAt: &now, InitiatedBy: op.Spec.InitiatedBy}
	if result != nil {
		state.SyncResult = &v1alpha1.SyncResultSummary{Revision: result.Revision}
	}
	a.Status.OperationState = state
	if phase == v1alpha1.OperationSucceeded && result != nil && !op.IsDryRun() {
		var next int64
		for _, h := range a.Status.History {
			next = max(next, h.ID+1)
		}
		a.Status.History = append(a.Status.History, v1alpha1.RevisionHistory{
			ID: next, Revision: result.Revision, DeployedAt: now, DeployStartedAt: started, Source: result.Source,
			InitiatedBy: op.Spec.InitiatedBy, OperationName: op.Name,
		})
		a.Status.Sync.Revision = result.Revision
	}
	if err := x.Kube.Status().Update(ctx, a); err != nil {
		return err
	}
	return x.reconcileApp(ctx, a, true)
}

// reconcileAppSet reports a new or changed ApplicationSet as up to date.
func (x *e2eEnv) reconcileAppSet(ctx context.Context, as *v1alpha1.ApplicationSet) error {
	for _, c := range as.Status.Conditions {
		if c.Type == v1alpha1.ApplicationSetConditionResourcesUpToDate && c.ObservedGeneration == as.Generation {
			return nil
		}
	}
	meta.SetStatusCondition(&as.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionTrue, ObservedGeneration: as.Generation,
		Reason: v1alpha1.ApplicationSetReasonApplicationSetUpToDate, Message: "All applications have been generated successfully",
	})
	return x.Kube.Status().Update(ctx, as)
}

// sourceOf returns the single source of a (the first of several).
func sourceOf(a *v1alpha1.Application) v1alpha1.ApplicationSource {
	if srcs := a.Spec.GetSources(); len(srcs) > 0 {
		return srcs[0]
	}
	return v1alpha1.ApplicationSource{}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// decodeJSON decodes CLI JSON output.
func decodeJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(bytes.TrimSpace([]byte(s)), &v), s)
	return v
}
