package integration

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/pprof"
	"slices"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/operation"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

const (
	waitTimeout = 90 * time.Second
	pollEvery   = 100 * time.Millisecond
)

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// fakeRepo is a repo-server holding commits per source (repoURL + path): branch heads move
// with commit, manifests are stamped with tracking metadata like the real repo-server.
type fakeRepo struct {
	apiclient.Client // unimplemented methods panic

	mu       gosync.Mutex
	heads    map[string]string
	commits  map[string][]*unstructured.Unstructured
	fail     map[string]error
	requests int
	seq      int
	tracker  *tracking.ResourceTracker
}

func newFakeRepo(controlNS string) *fakeRepo {
	return &fakeRepo{
		heads:   map[string]string{},
		commits: map[string][]*unstructured.Unstructured{},
		fail:    map[string]error{},
		tracker: tracking.New(tracking.Config{ControlPlaneNamespace: controlNS}),
	}
}

func srcKey(repo, path string) string { return repo + "|" + path }

// commit records manifests as a new commit of branch and returns its SHA.
func (f *fakeRepo) commit(repo, path, branch string, objs ...*unstructured.Unstructured) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	sum := sha1.Sum(fmt.Appendf(nil, "%s|%s|%d|%d", repo, path, f.seq, time.Now().UnixNano()))
	sha := hex.EncodeToString(sum[:])
	f.commits[srcKey(repo, path)+"|"+sha] = objs
	f.heads[srcKey(repo, path)+"|"+branch] = sha
	return sha
}

func (f *fakeRepo) setError(repo, path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, srcKey(repo, path))
		return
	}
	f.fail[srcKey(repo, path)] = err
}

func (f *fakeRepo) resolve(repo, path, rev string) (string, error) {
	if _, ok := f.commits[srcKey(repo, path)+"|"+rev]; ok {
		return rev, nil
	}
	if rev == "" {
		rev = "HEAD"
	}
	if sha, ok := f.heads[srcKey(repo, path)+"|"+rev]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("unable to resolve %q to a commit SHA", rev)
}

func (f *fakeRepo) GenerateManifest(_ context.Context, req *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	key := srcKey(req.Source.RepoURL, req.Source.Path)
	if err := f.fail[key]; err != nil {
		return nil, err
	}
	sha, err := f.resolve(req.Source.RepoURL, req.Source.Path, req.Revision)
	if err != nil {
		return nil, err
	}
	resp := &apiclient.ManifestResponse{Revision: sha, SourceType: "Directory", Manifests: []string{}}
	for _, o := range f.commits[key+"|"+sha] {
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

func (f *fakeRepo) UpdateRevisionForPaths(context.Context, *apiclient.UpdateRevisionForPathsRequest) (*apiclient.UpdateRevisionForPathsResponse, error) {
	return &apiclient.UpdateRevisionForPathsResponse{Changes: true}, nil
}

type fakeRepositories struct{}

func (fakeRepositories) GetRepository(_ context.Context, repoURL, proj string) (*apiclient.Repository, error) {
	return &apiclient.Repository{URL: repoURL, Project: proj}, nil
}

func (fakeRepositories) ListDependencyRepositories(context.Context, *v1alpha1.AppProject, bool) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error) {
	return nil, nil, nil
}

// recorder captures Events as "Reason: note".
type recorder struct {
	mu     gosync.Mutex
	events []string
}

func (r *recorder) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, note string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, reason+": "+fmt.Sprintf(note, args...))
}

func (r *recorder) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// env is one test's namespaces, repo-server and clients.
type env struct {
	t       *testing.T
	ctx     context.Context
	cfg     *rest.Config
	c       client.Client
	dyn     dynamic.Interface
	sfx     string
	ns      string // control-plane namespace
	dest    string // destination namespace
	repo    *fakeRepo
	repoURL string
	log     *slog.Logger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	cfg := rest.CopyConfig(testCfg)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	sfx := rand.String(5)
	e := &env{t: t, ctx: ctx, cfg: cfg, c: c, dyn: dyn, sfx: sfx, ns: "patrol-" + sfx, dest: "dest-" + sfx,
		repoURL: "https://git.example.com/" + sfx + ".git"}
	e.repo = newFakeRepo(e.ns)
	level := slog.LevelWarn
	if os.Getenv("PATROL_TEST_DEBUG") != "" {
		level = slog.LevelDebug
	}
	e.log = slog.New(slog.NewTextHandler(logWriter(), &slog.HandlerOptions{Level: level})).With("test", t.Name())
	for _, ns := range []string{e.ns, e.dest} {
		e.createNamespace(ns)
	}
	require.NoError(t, c.Create(ctx, permissiveProject(e.ns, "default")))
	require.NoError(t, c.Create(ctx, &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "dest", Namespace: e.ns},
		Spec:       v1alpha1.ClusterSpec{Server: cfg.Host, Name: "dest-" + sfx},
	}))
	e.startFakeGC(e.dest)
	return e
}

func logWriter() io.Writer {
	if os.Getenv("PATROL_TEST_DEBUG") != "" {
		return os.Stderr
	}
	return io.Discard
}

func (e *env) createNamespace(name string) {
	e.t.Helper()
	err := e.c.Create(e.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(e.t, err)
	}
}

func permissiveProject(ns, name string) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:              []string{"*"},
			Destinations:             []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			ClusterResourceWhitelist: []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}},
		},
	}
}

var fakeGCResources = []schema.GroupVersionResource{
	{Version: "v1", Resource: "configmaps"},
	{Version: "v1", Resource: "secrets"},
	{Version: "v1", Resource: "services"},
	{Version: "v1", Resource: "pods"},
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "batch", Version: "v1", Resource: "jobs"},
}

// startFakeGC releases foreground/orphan deletions in namespaces (envtest runs no garbage
// collector, so those finalizers would never be removed).
func (e *env) startFakeGC(namespaces ...string) {
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	go func() {
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			for _, ns := range namespaces {
				for _, gvr := range fakeGCResources {
					list, err := e.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
					if err != nil {
						continue
					}
					for i := range list.Items {
						o := &list.Items[i]
						if o.GetDeletionTimestamp() == nil {
							continue
						}
						fins := slices.DeleteFunc(slices.Clone(o.GetFinalizers()), func(f string) bool {
							return f == metav1.FinalizerDeleteDependents || f == metav1.FinalizerOrphanDependents
						})
						if len(fins) == len(o.GetFinalizers()) {
							continue
						}
						o.SetFinalizers(fins)
						_, _ = e.dyn.Resource(gvr).Namespace(ns).Update(ctx, o, metav1.UpdateOptions{})
					}
				}
			}
		}
	}()
}

// controllerOptions tune a controller instance.
type controllerOptions struct {
	params      core.Params
	cmData      map[string]string
	ownsCluster func(*v1alpha1.Cluster) bool
}

type controllerOption func(*controllerOptions)

func withParams(fn func(*core.Params)) controllerOption {
	return func(o *controllerOptions) { fn(&o.params) }
}

func withOwnsCluster(fn func(*v1alpha1.Cluster) bool) controllerOption {
	return func(o *controllerOptions) { o.ownsCluster = fn }
}

func withSettings(data map[string]string) controllerOption {
	return func(o *controllerOptions) {
		for k, v := range data {
			o.cmData[k] = v
		}
	}
}

// controller is one running controller process.
type controller struct {
	e        *env
	cancel   context.CancelFunc
	done     chan error
	cacheMgr *cache.Manager
	events   *recorder
	registry *prometheus.Registry
	appRec   *app.Reconciler
}

// startController starts both reconcilers in a new manager (a controller "process").
func (e *env) startController(opts ...controllerOption) *controller {
	e.t.Helper()
	o := controllerOptions{params: core.DefaultParams(), cmData: map[string]string{
		"timeout.reconciliation":        "1h",
		"timeout.reconciliation.jitter": "0s",
	}}
	o.params.StatusProcessors = 4
	o.params.OperationProcessors = 4
	for _, fn := range opts {
		fn(&o)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: e.ns, Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       o.cmData,
	}
	st := settings.Parse(settings.Input{Namespace: e.ns, ConfigMaps: []*corev1.ConfigMap{cm}})
	matcher := core.NewNamespaceMatcher(e.ns, o.params.ApplicationNamespaces)
	appNS := map[string]crcache.Config{e.ns: {}}
	if matcher.ClusterWide() {
		appNS = map[string]crcache.Config{crcache.AllNamespaces: {}}
	}
	mgr, err := ctrl.NewManager(e.cfg, manager.Options{
		Scheme:                  scheme,
		Logger:                  logr.Discard(),
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		GracefulShutdownTimeout: ptr.To(20 * time.Second),
		Cache: crcache.Options{ByObject: map[client.Object]crcache.ByObject{
			&v1alpha1.Application{}: {Namespaces: appNS},
			&v1alpha1.Operation{}:   {Namespaces: appNS},
			&v1alpha1.AppProject{}:  {Namespaces: map[string]crcache.Config{e.ns: {}}},
			&v1alpha1.Cluster{}:     {Namespaces: map[string]crcache.Config{e.ns: {}}},
			&v1alpha1.Repository{}:  {Namespaces: map[string]crcache.Config{e.ns: {}}},
		}},
	})
	require.NoError(e.t, err)

	reg := prometheus.NewRegistry()
	metrics, err := core.NewMetrics(reg)
	require.NoError(e.t, err)
	clusterConfig := func(context.Context, *v1alpha1.Cluster) (*rest.Config, error) { return rest.CopyConfig(e.cfg), nil }
	var appRec *app.Reconciler
	cacheMgr := cache.NewManager(cache.ManagerOptions{Logger: e.log, OnCacheCreated: func(key string, cc cache.ClusterCache) {
		if appRec != nil {
			appRec.OnClusterCacheCreated(key, cc)
		}
	}})
	ss := compare.StaticSettings{Settings: st}
	live, err := compare.NewLiveStateCache(compare.LiveStateCacheOptions{
		Manager: cacheMgr, Config: clusterConfig, Settings: ss, ControlPlaneNamespace: e.ns,
		Base: cache.Settings{EventProcessingInterval: 20 * time.Millisecond}, Logger: e.log,
	})
	require.NoError(e.t, err)
	lookup := project.NewKubeLookup(e.c, e.ns, true)
	cmp, err := compare.New(compare.Deps{
		RepoServer: e.repo, Repositories: fakeRepositories{}, Settings: ss,
		Clusters: core.LookupClusterResolver{Lookup: lookup}, LiveState: live,
		ControlPlaneNamespace: e.ns, PersistResourceHealth: true, Registerer: reg, Logger: e.log,
	})
	require.NoError(e.t, err)
	events := &recorder{}
	deps := core.Deps{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: events, Comparer: cmp, Settings: ss,
		LiveState: live, ClusterConfig: clusterConfig, Namespace: e.ns, Params: o.params, Metrics: metrics, Logger: e.log,
		OwnsCluster: o.ownsCluster,
	}
	appRec, err = app.New(app.Config{Deps: deps, DeletionPoll: 300 * time.Millisecond})
	require.NoError(e.t, err)
	engineCfg := syncpkg.DefaultConfig()
	engineCfg.WaveDelay = 0
	engineCfg.PollInterval = 200 * time.Millisecond
	engineCfg.Logger = e.log
	engines, err := operation.NewKubeEngines(operation.KubeEnginesOptions{Config: clusterConfig, Engine: &engineCfg, Metrics: metrics, Logger: e.log})
	require.NoError(e.t, err)
	opDeps := deps
	opDeps.Refresher = appRec.Refresher()
	opRec, err := operation.New(operation.Config{Deps: opDeps, Engines: engines, PollInterval: 200 * time.Millisecond})
	require.NoError(e.t, err)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(e.t, core.IndexOperations(ctx, mgr.GetFieldIndexer()))
	require.NoError(e.t, app.IndexFields(ctx, mgr.GetFieldIndexer()))
	require.NoError(e.t, appRec.SetupWithManager(mgr))
	require.NoError(e.t, opRec.SetupWithManager(mgr))
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	c := &controller{e: e, cancel: cancel, done: done, cacheMgr: cacheMgr, events: events, registry: reg, appRec: appRec}
	e.t.Cleanup(c.stop)
	return c
}

// stop stops the manager and drops the live-state caches (a process exit).
func (c *controller) stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	select {
	case err := <-c.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			var buf strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
			c.e.t.Logf("controller manager stopped with error: %v\ngoroutines:\n%s", err, buf.String())
		}
	case <-time.After(stopTimeout):
		var buf strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
		c.e.t.Errorf("controller manager did not stop; goroutines:\n%s", buf.String())
	}
	c.cacheMgr.Close()
	c.cancel = nil
}

// --- object builders ---------------------------------------------------------------------

func configMap(name string, data map[string]string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name}}}
	if data != nil {
		d := map[string]any{}
		for k, v := range data {
			d[k] = v
		}
		u.Object["data"] = d
	}
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	return u
}

func withWave(u *unstructured.Unstructured, wave int) *unstructured.Unstructured {
	a := u.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[v1alpha1.AnnotationKeySyncWave] = fmt.Sprint(wave)
	u.SetAnnotations(a)
	return u
}

func withAnnotation(u *unstructured.Unstructured, k, v string) *unstructured.Unstructured {
	a := u.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[k] = v
	u.SetAnnotations(a)
	return u
}

func hookPod(generateName string, hook v1alpha1.HookType) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"generateName": generateName, "annotations": map[string]any{v1alpha1.AnnotationKeyHook: string(hook)}},
		"spec":     map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "main", "image": "busybox:1.36"}}},
	}}
	return u
}

func namedHookPod(name string, hook v1alpha1.HookType) *unstructured.Unstructured {
	u := hookPod("", hook)
	unstructured.RemoveNestedField(u.Object, "metadata", "generateName")
	u.SetName(name)
	return u
}

func deployment(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "main", "image": "nginx:1.27"}}},
			},
		},
	}}
}

// --- Application helpers -----------------------------------------------------------------

func (e *env) appName(base string) string { return base + "-" + e.sfx }

// newApp returns an Application of the env's repository (path base) syncing to the
// destination namespace.
func (e *env) newApp(base string, mutate ...func(*v1alpha1.Application)) *v1alpha1.Application {
	a := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: e.appName(base), Namespace: e.ns},
		Spec: v1alpha1.ApplicationSpec{
			Source:      &v1alpha1.ApplicationSource{RepoURL: e.repoURL, Path: base, TargetRevision: "main"},
			Destination: v1alpha1.ApplicationDestination{Server: e.cfg.Host, Namespace: e.dest},
			Project:     "default",
		},
	}
	for _, fn := range mutate {
		fn(a)
	}
	return a
}

func (e *env) create(obj client.Object) {
	e.t.Helper()
	require.NoError(e.t, e.c.Create(e.ctx, obj))
}

func (e *env) getApp(a *v1alpha1.Application) *v1alpha1.Application {
	e.t.Helper()
	out := &v1alpha1.Application{}
	require.NoError(e.t, e.c.Get(e.ctx, client.ObjectKeyFromObject(a), out))
	return out
}

// updateApp applies fn to the latest Application and updates it. The running controller also
// writes the object, so a read followed by an update can lose the race for the resource version;
// the whole read-modify-write is retried on a conflict.
func (e *env) updateApp(a *v1alpha1.Application, fn func(*v1alpha1.Application)) {
	e.t.Helper()
	require.NoError(e.t, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur := &v1alpha1.Application{}
		if err := e.c.Get(e.ctx, client.ObjectKeyFromObject(a), cur); err != nil {
			return err
		}
		fn(cur)
		return e.c.Update(e.ctx, cur)
	}))
}

// waitApp polls the Application until cond holds.
func (e *env) waitApp(a *v1alpha1.Application, msg string, cond func(*v1alpha1.Application) bool) *v1alpha1.Application {
	e.t.Helper()
	var last *v1alpha1.Application
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		cur := &v1alpha1.Application{}
		if err := e.c.Get(e.ctx, client.ObjectKeyFromObject(a), cur); err == nil {
			last = cur
			if cond(cur) {
				return cur
			}
		}
		time.Sleep(pollEvery)
	}
	e.t.Fatalf("application %s: %s\nlast status: %s", a.Name, msg, describeApp(last))
	return nil
}

func describeApp(a *v1alpha1.Application) string {
	if a == nil {
		return "<none>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sync=%s rev=%s health=%s reconciledAt=%v gen=%d/%d finalizers=%v", a.Status.Sync.Status, a.Status.Sync.Revision,
		a.Status.Health.Status, a.Status.ReconciledAt, a.Generation, a.Status.ObservedGeneration, a.Finalizers)
	if os := a.Status.OperationState; os != nil {
		fmt.Fprintf(&b, " op=%s phase=%s msg=%q retries=%d", os.OperationName, os.Phase, os.Message, os.RetryCount)
	}
	for _, c := range a.Status.Conditions {
		fmt.Fprintf(&b, "\n  condition %s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message)
	}
	for _, r := range a.Status.Resources {
		fmt.Fprintf(&b, "\n  resource %s/%s/%s %s", r.Kind, r.Namespace, r.Name, r.Status)
	}
	return b.String()
}

func synced(rev string) func(*v1alpha1.Application) bool {
	return func(a *v1alpha1.Application) bool {
		return a.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced && (rev == "" || a.Status.Sync.Revision == rev)
	}
}

func outOfSync(a *v1alpha1.Application) bool {
	return a.Status.Sync.Status == v1alpha1.SyncStatusCodeOutOfSync
}

func hasCondition(a *v1alpha1.Application, t string) *metav1.Condition {
	for i := range a.Status.Conditions {
		if a.Status.Conditions[i].Type == t {
			return &a.Status.Conditions[i]
		}
	}
	return nil
}

// refresh sets the refresh annotation (what the API and webhooks do).
func (e *env) refresh(a *v1alpha1.Application, kind v1alpha1.RefreshType) {
	e.t.Helper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, v1alpha1.AnnotationKeyRefresh, kind)
	require.NoError(e.t, e.c.Patch(e.ctx, a.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(patch))))
}

// sync creates a manual sync Operation of a.
func (e *env) sync(a *v1alpha1.Application, mutate ...func(*v1alpha1.Operation)) *v1alpha1.Operation {
	e.t.Helper()
	op := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{GenerateName: a.Name + "-sync-", Namespace: a.Namespace},
		Spec: v1alpha1.OperationSpec{
			Application: a.Name,
			Sync:        &v1alpha1.SyncOperation{},
			InitiatedBy: v1alpha1.OperationInitiator{Username: "tester"},
		},
	}
	for _, fn := range mutate {
		fn(op)
	}
	require.NoError(e.t, e.c.Create(e.ctx, op))
	return op
}

func (e *env) waitOp(op *v1alpha1.Operation, msg string, cond func(*v1alpha1.Operation) bool) *v1alpha1.Operation {
	e.t.Helper()
	var last *v1alpha1.Operation
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		cur := &v1alpha1.Operation{}
		if err := e.c.Get(e.ctx, client.ObjectKeyFromObject(op), cur); err == nil {
			last = cur
			if cond(cur) {
				return cur
			}
		}
		time.Sleep(pollEvery)
	}
	e.t.Fatalf("operation %s: %s\nlast: %s", op.Name, msg, describeOp(last))
	return nil
}

func describeOp(op *v1alpha1.Operation) string {
	if op == nil {
		return "<none>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s message=%q retries=%d started=%v finished=%v", op.Status.Phase, op.Status.Message, op.Status.RetryCount, op.Status.StartedAt, op.Status.FinishedAt)
	if p := op.Status.Progress; p != nil {
		fmt.Fprintf(&b, " progress=%s/%d %d/%d", p.SyncPhase, p.Wave, p.CompletedTasks, p.TotalTasks)
	}
	if sr := op.Status.SyncResult; sr != nil {
		for _, r := range sr.Resources {
			fmt.Fprintf(&b, "\n  result %s/%s %s %s %s %q", r.Kind, r.Name, r.SyncPhase, r.Status, r.HookPhase, r.Message)
		}
	}
	for _, c := range op.Status.Conditions {
		fmt.Fprintf(&b, "\n  condition %s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message)
	}
	return b.String()
}

func phaseIs(p v1alpha1.OperationPhase) func(*v1alpha1.Operation) bool {
	return func(op *v1alpha1.Operation) bool { return op.Status.Phase == p }
}

// operations lists the Operations of a, oldest first.
func (e *env) operations(a *v1alpha1.Application) []v1alpha1.Operation {
	e.t.Helper()
	ops, err := core.ListOperationsLive(e.ctx, e.c, a.Namespace, a.Name)
	require.NoError(e.t, err)
	return ops
}

// waitOps polls the Operations of a until cond holds.
func (e *env) waitOps(a *v1alpha1.Application, msg string, cond func([]v1alpha1.Operation) bool) []v1alpha1.Operation {
	e.t.Helper()
	var last []v1alpha1.Operation
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		last = e.operations(a)
		if cond(last) {
			return last
		}
		time.Sleep(pollEvery)
	}
	var b strings.Builder
	for i := range last {
		fmt.Fprintf(&b, "\n%s: %s", last[i].Name, describeOp(&last[i]))
	}
	e.t.Fatalf("operations of %s: %s%s", a.Name, msg, b.String())
	return nil
}

// --- destination helpers -----------------------------------------------------------------

var (
	gvrConfigMap  = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	gvrPod        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrDeployment = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

func (e *env) live(gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, bool) {
	e.t.Helper()
	obj, err := e.dyn.Resource(gvr).Namespace(e.dest).Get(e.ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(e.t, err)
	return obj, true
}

func (e *env) waitLive(gvr schema.GroupVersionResource, name, msg string, cond func(*unstructured.Unstructured, bool) bool) *unstructured.Unstructured {
	e.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		obj, ok := e.live(gvr, name)
		if cond(obj, ok) {
			return obj
		}
		time.Sleep(pollEvery)
	}
	e.t.Fatalf("%s %s/%s: %s", gvr.Resource, e.dest, name, msg)
	return nil
}

func exists(_ *unstructured.Unstructured, ok bool) bool { return ok }
func gone(_ *unstructured.Unstructured, ok bool) bool   { return !ok }

// mutateLive updates a live object with retries on conflict.
func (e *env) mutateLive(gvr schema.GroupVersionResource, name string, fn func(*unstructured.Unstructured)) {
	e.t.Helper()
	for range 10 {
		obj, err := e.dyn.Resource(gvr).Namespace(e.dest).Get(e.ctx, name, metav1.GetOptions{})
		require.NoError(e.t, err)
		fn(obj)
		_, err = e.dyn.Resource(gvr).Namespace(e.dest).Update(e.ctx, obj, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		require.NoError(e.t, err)
		return
	}
	e.t.Fatalf("update of %s kept conflicting", name)
}

// setPodPhase sets a Pod's status phase (no kubelet in envtest).
func (e *env) setPodPhase(name string, phase corev1.PodPhase) {
	e.t.Helper()
	for range 10 {
		obj, err := e.dyn.Resource(gvrPod).Namespace(e.dest).Get(e.ctx, name, metav1.GetOptions{})
		require.NoError(e.t, err)
		require.NoError(e.t, unstructured.SetNestedField(obj.Object, string(phase), "status", "phase"))
		_, err = e.dyn.Resource(gvrPod).Namespace(e.dest).UpdateStatus(e.ctx, obj, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		require.NoError(e.t, err)
		return
	}
	e.t.Fatalf("status update of pod %s kept conflicting", name)
}

// podsWithPrefix lists the destination Pods whose name starts with prefix.
func (e *env) podsWithPrefix(prefix string) []unstructured.Unstructured {
	e.t.Helper()
	list, err := e.dyn.Resource(gvrPod).Namespace(e.dest).List(e.ctx, metav1.ListOptions{})
	require.NoError(e.t, err)
	var out []unstructured.Unstructured
	for _, p := range list.Items {
		if strings.HasPrefix(p.GetName(), prefix) {
			out = append(out, p)
		}
	}
	return out
}

// markDeploymentAvailable writes the status a deployment controller would (none runs in
// envtest), so the Deployment's health becomes Healthy.
func (e *env) markDeploymentAvailable(name string) {
	e.t.Helper()
	for range 10 {
		obj, err := e.dyn.Resource(gvrDeployment).Namespace(e.dest).Get(e.ctx, name, metav1.GetOptions{})
		require.NoError(e.t, err)
		obj.Object["status"] = map[string]any{
			"observedGeneration": obj.GetGeneration(),
			"replicas":           int64(1), "updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1),
			"conditions": []any{map[string]any{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable"}},
		}
		_, err = e.dyn.Resource(gvrDeployment).Namespace(e.dest).UpdateStatus(e.ctx, obj, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		require.NoError(e.t, err)
		return
	}
	e.t.Fatalf("status update of deployment %s kept conflicting", name)
}

func keyOf(a *v1alpha1.Application) client.ObjectKey { return client.ObjectKeyFromObject(a) }

func keyOfOp(op *v1alpha1.Operation) client.ObjectKey { return client.ObjectKeyFromObject(op) }

// stopTimeout bounds a controller shutdown; it exceeds the manager's graceful shutdown
// timeout so a stuck runnable is reported by Start rather than by this wait.
var stopTimeout = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("PATROL_TEST_STOP_TIMEOUT")); err == nil {
		return v
	}
	return 45 * time.Second
}()
