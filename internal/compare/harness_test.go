package compare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

const controlPlaneNS = "patrol"

// fakeRepoServer serves canned manifests per source (repoURL + "|" + path) and stamps tracking
// metadata on them like the repo-server does.
type fakeRepoServer struct {
	apiclient.Client // unimplemented methods panic if called

	mu        sync.Mutex
	manifests map[string][]*unstructured.Unstructured
	revisions map[string]string
	types     map[string]string
	errs      map[string]error
	requests  []apiclient.ManifestRequest
	tracker   *tracking.ResourceTracker
}

func newFakeRepoServer() *fakeRepoServer {
	return &fakeRepoServer{
		manifests: map[string][]*unstructured.Unstructured{},
		revisions: map[string]string{},
		types:     map[string]string{},
		errs:      map[string]error{},
		tracker:   tracking.New(tracking.Config{ControlPlaneNamespace: controlPlaneNS}),
	}
}

func sourceKey(repoURL, path string) string { return repoURL + "|" + path }

func (f *fakeRepoServer) set(repoURL, path, revision string, objs ...*unstructured.Unstructured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := sourceKey(repoURL, path)
	f.manifests[k] = objs
	f.revisions[k] = revision
	delete(f.errs, k)
}

func (f *fakeRepoServer) fail(repoURL, path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[sourceKey(repoURL, path)] = err
}

func (f *fakeRepoServer) lastRequests() []apiclient.ManifestRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiclient.ManifestRequest(nil), f.requests...)
}

func (f *fakeRepoServer) GenerateManifest(_ context.Context, req *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, *req)
	k := sourceKey(req.Source.RepoURL, req.Source.Path)
	if err := f.errs[k]; err != nil {
		return nil, err
	}
	resp := &apiclient.ManifestResponse{Revision: f.revisions[k], SourceType: "Directory", Manifests: []string{}}
	if t, ok := f.types[k]; ok {
		resp.SourceType = t
	}
	for _, o := range f.manifests[k] {
		cp := o.DeepCopy()
		if req.AppName != "" && !tracking.IsCRD(cp) {
			if err := f.tracker.SetAppInstance(cp, req.AppName, req.Namespace, tracking.Method(req.TrackingMethod), req.InstallationID); err != nil {
				return nil, err
			}
		}
		b, err := cp.MarshalJSON()
		if err != nil {
			return nil, err
		}
		resp.Manifests = append(resp.Manifests, string(b))
	}
	return resp, nil
}

func (f *fakeRepoServer) UpdateRevisionForPaths(context.Context, *apiclient.UpdateRevisionForPathsRequest) (*apiclient.UpdateRevisionForPathsResponse, error) {
	return &apiclient.UpdateRevisionForPathsResponse{Changes: true}, nil
}

type fakeRepositories struct{}

func (fakeRepositories) GetRepository(_ context.Context, repoURL, project string) (*apiclient.Repository, error) {
	return &apiclient.Repository{URL: repoURL, Project: project}, nil
}

func (fakeRepositories) ListDependencyRepositories(context.Context, *v1alpha1.AppProject, bool) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error) {
	return nil, nil, nil
}

type fakeClusters struct{ cluster *v1alpha1.Cluster }

func (f fakeClusters) DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error) {
	if dest.Server != f.cluster.Spec.Server && dest.Name != f.cluster.Spec.Name {
		return nil, fmt.Errorf("cluster %q not found", dest.Server)
	}
	return f.cluster.DeepCopy(), nil
}

func (fakeClusters) GetProjectClusters(context.Context, string) ([]*v1alpha1.Cluster, error) {
	return nil, nil
}

// harness wires a Manager to the shared envtest API server with a per-test namespace.
type harness struct {
	t        *testing.T
	ctx      context.Context
	cfg      *rest.Config
	kube     kubernetes.Interface
	dyn      dynamic.Interface
	mapper   meta.RESTMapper
	ns       string
	repo     *fakeRepoServer
	cluster  *v1alpha1.Cluster
	settings *settings.Settings
	live     *LiveStateCache
	cacheMgr *cache.Manager
	mgr      *Manager
	tracker  *tracking.ResourceTracker
}

type harnessOption func(*harnessConfig)

type harnessConfig struct {
	cmData     map[string]string
	deps       func(*Deps)
	base       cache.Settings
	serverSide bool
}

func withSettings(data map[string]string) harnessOption {
	return func(c *harnessConfig) { c.cmData = data }
}

func withDeps(fn func(*Deps)) harnessOption { return func(c *harnessConfig) { c.deps = fn } }

func withServerSideDiff() harnessOption { return func(c *harnessConfig) { c.serverSide = true } }

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	cfg := envConfig(t)
	var hc harnessConfig
	for _, o := range opts {
		o(&hc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err)

	ns := "cmp-" + rand.String(6)
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: controlPlaneNS, Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       hc.cmData,
	}
	st := settings.Parse(settings.Input{Namespace: controlPlaneNS, ConfigMaps: []*corev1.ConfigMap{cm}})
	for _, issue := range st.Validate() {
		require.False(t, issue.IsError() && issue.Object == "patrol-cm" && issue.Key != "", "settings issue: %s", issue)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cacheMgr := cache.NewManager(cache.ManagerOptions{Logger: logger})
	t.Cleanup(cacheMgr.Close)
	cluster := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "in-cluster", Namespace: controlPlaneNS}, Spec: v1alpha1.ClusterSpec{Server: cfg.Host, Name: "in-cluster"}}
	configFn := func(context.Context, *v1alpha1.Cluster) (*rest.Config, error) { return rest.CopyConfig(cfg), nil }
	base := hc.base
	if base.EventProcessingInterval == 0 {
		base.EventProcessingInterval = 20 * time.Millisecond
	}
	live, err := NewLiveStateCache(LiveStateCacheOptions{
		Manager: cacheMgr, Config: configFn, Settings: StaticSettings{Settings: st},
		ControlPlaneNamespace: controlPlaneNS, Base: base, Logger: logger,
	})
	require.NoError(t, err)

	repo := newFakeRepoServer()
	d := Deps{
		RepoServer:            repo,
		Repositories:          fakeRepositories{},
		Settings:              StaticSettings{Settings: st},
		Clusters:              fakeClusters{cluster: cluster},
		LiveState:             live,
		ControlPlaneNamespace: controlPlaneNS,
		PersistResourceHealth: true,
		Registerer:            prometheus.NewRegistry(),
		Logger:                logger,
	}
	if hc.serverSide {
		ssd, err := NewKubeServerSideDiff(configFn, live, 0)
		require.NoError(t, err)
		d.ServerSideDiff = ssd
	}
	if hc.deps != nil {
		hc.deps(&d)
	}
	mgr, err := New(d)
	require.NoError(t, err)

	return &harness{
		t: t, ctx: ctx, cfg: cfg, kube: kube, dyn: dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
		ns:     ns, repo: repo, cluster: cluster, settings: st, live: live, cacheMgr: cacheMgr, mgr: mgr,
		tracker: tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: controlPlaneNS}),
	}
}

// app returns an Application named name plus the test's random suffix: tests share the API
// server (and repeat with -count), so instance names must not collide.
func (h *harness) app(name string, sources ...v1alpha1.ApplicationSource) *v1alpha1.Application {
	name = name + "-" + strings.TrimPrefix(h.ns, "cmp-")
	app := &v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlPlaneNS, UID: types.UID(name + "-uid")},
		Spec: v1alpha1.ApplicationSpec{
			Destination: v1alpha1.ApplicationDestination{Server: h.cluster.Spec.Server, Namespace: h.ns},
			Project:     "default",
		},
	}
	if len(sources) == 1 {
		app.Spec.Source = &sources[0]
	} else {
		app.Spec.Sources = sources
	}
	return app
}

func permissiveProject() *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: controlPlaneNS},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:              []string{"*"},
			Destinations:             []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			ClusterResourceWhitelist: []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}},
		},
	}
}

func (h *harness) compare(in Input) *Result {
	h.t.Helper()
	if in.Project == nil {
		in.Project = permissiveProject()
	}
	res, err := h.mgr.Compare(h.ctx, in)
	require.NoError(h.t, err)
	return res
}

// eventually re-runs the comparison until cond holds (the cache converges asynchronously).
func (h *harness) eventually(in Input, cond func(*Result) bool, msg string) *Result {
	h.t.Helper()
	var last *Result
	ok := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		last = h.compare(in)
		if cond(last) {
			ok = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		h.t.Fatalf("condition not met: %s\nlast result: sync=%s health=%s conditions=%v resources=%+v", msg, last.SyncStatus.Status, last.Health, last.Conditions, last.Resources)
	}
	return last
}

func (h *harness) resourceInterface(u *unstructured.Unstructured) dynamic.ResourceInterface {
	h.t.Helper()
	gvk := u.GroupVersionKind()
	m, err := h.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	require.NoError(h.t, err)
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return h.dyn.Resource(m.Resource).Namespace(u.GetNamespace())
	}
	return h.dyn.Resource(m.Resource)
}

func (h *harness) create(u *unstructured.Unstructured) *unstructured.Unstructured {
	h.t.Helper()
	out, err := h.resourceInterface(u).Create(h.ctx, u.DeepCopy(), metav1.CreateOptions{})
	require.NoError(h.t, err)
	h.t.Cleanup(func() {
		if out.GetNamespace() == "" {
			_ = h.resourceInterface(out).Delete(context.Background(), out.GetName(), metav1.DeleteOptions{})
		}
	})
	return out
}

func (h *harness) mutate(u *unstructured.Unstructured, fn func(*unstructured.Unstructured)) *unstructured.Unstructured {
	h.t.Helper()
	ri := h.resourceInterface(u)
	for range 5 {
		cur, err := ri.Get(h.ctx, u.GetName(), metav1.GetOptions{})
		require.NoError(h.t, err)
		fn(cur)
		out, err := ri.Update(h.ctx, cur, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		require.NoError(h.t, err)
		return out
	}
	h.t.Fatalf("update of %s kept conflicting", u.GetName())
	return nil
}

// track stamps the tracking metadata of app on a copy of u.
func (h *harness) track(u *unstructured.Unstructured, app *v1alpha1.Application) *unstructured.Unstructured {
	h.t.Helper()
	cp := u.DeepCopy()
	require.NoError(h.t, h.tracker.SetAppInstance(cp, h.tracker.InstanceName(tracking.AppOf(app)), app.Spec.Destination.Namespace, h.settings.TrackingMethod(), h.settings.InstallationID()))
	return cp
}

// createTargets creates every non-hook target of a result, as a sync would.
func (h *harness) createTargets(res *Result) {
	h.t.Helper()
	for _, tgt := range res.Targets {
		if isHookObject(tgt) {
			continue
		}
		h.create(tgt)
	}
}

// waitCached waits until the live-state cache holds the object.
func (h *harness) waitCached(group, kind, namespace, name string) {
	h.t.Helper()
	cc, err := h.live.ClusterCache(h.ctx, h.cluster)
	require.NoError(h.t, err)
	key := ResourceKey{Group: group, Kind: kind, Namespace: namespace, Name: name}
	require.Eventually(h.t, func() bool {
		_, ok := cc.FindResources(namespace, func(r *cache.Resource) bool { return r.ResourceKey() == key })[key]
		return ok
	}, 30*time.Second, 20*time.Millisecond, "cache never observed %s", key)
}

func findResource(res *Result, kind, name string) *v1alpha1.ResourceStatus {
	for i := range res.Resources {
		if res.Resources[i].Kind == kind && res.Resources[i].Name == name {
			return &res.Resources[i]
		}
	}
	return nil
}

func findManaged(res *Result, kind, name string) *ManagedResource {
	for i := range res.Managed {
		if res.Managed[i].Key.Kind == kind && res.Managed[i].Key.Name == name {
			return &res.Managed[i]
		}
	}
	return nil
}

func conditionMessages(res *Result, t string) string {
	return strings.Join(res.ConditionMessages(t), "\n")
}

var errBoom = errors.New("rpc error: repository not reachable")
