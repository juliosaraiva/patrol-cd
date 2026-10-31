package clusterinfo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
)

const (
	controlNS = "patrol"
	// unservedNS is a namespace projects can permit and this controller does not serve.
	unservedNS = "team-unserved"
)

// recorder stores what the Updater decided on a fake client, so a later round reads it back as it
// would through the API: whole-second times, and only what the schema keeps.
type recorder struct {
	c         client.Client
	writes    []string
	labels    []string
	block     bool // RecordClusterInfo waits for the end of its context
	errs      map[string]error
	labelErrs map[string]error
	onWrite   func(name string)
}

func (r *recorder) RecordClusterInfo(ctx context.Context, c *v1alpha1.Cluster, u clusters.StatusUpdate) (*v1alpha1.Cluster, error) {
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.writes = append(r.writes, c.Name)
	if r.onWrite != nil {
		r.onWrite(c.Name)
	}
	if err := r.errs[c.Name]; err != nil {
		return nil, err
	}
	cur := &v1alpha1.Cluster{}
	if err := r.c.Get(ctx, client.ObjectKeyFromObject(c), cur); err != nil {
		return nil, err
	}
	clusters.ApplyClusterInfo(cur, u)
	return cur, r.c.Status().Update(ctx, cur)
}

func (r *recorder) RecordVersionLabel(ctx context.Context, c *v1alpha1.Cluster, version string) (bool, error) {
	r.labels = append(r.labels, c.Name+"="+version)
	if err := r.labelErrs[c.Name]; err != nil {
		return false, err
	}
	cur := &v1alpha1.Cluster{}
	if err := r.c.Get(ctx, client.ObjectKeyFromObject(c), cur); err != nil {
		return false, err
	}
	cur.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] = version
	return true, r.c.Update(ctx, cur)
}

// taken returns the writes since the last call.
func (r *recorder) taken() []string {
	out := r.writes
	r.writes = nil
	return out
}

// serverCounter counts by spec.destination.server; the registry's resolution has its own test.
type serverCounter struct{ rounds atomic.Int32 }

func (s *serverCounter) ApplicationCounts(apps []v1alpha1.Application, permitted func(*v1alpha1.Application) bool) map[string]int64 {
	s.rounds.Add(1)
	out := map[string]int64{}
	for i := range apps {
		if permitted(&apps[i]) {
			out[clusters.NormalizeServer(apps[i].Spec.Destination.Server)]++
		}
	}
	return out
}

type ownedShards map[int]bool

func (o ownedShards) OwnsShard(s int) bool { return o[s] }

type fixture struct {
	*Updater
	rec     *recorder
	reader  client.Client
	counter *serverCounter
	states  map[string]*clusters.CacheState
	owned   ownedShards
	now     time.Time
}

// newFixture serves objs. Clusters among them are assigned to the shard of spec.shard, 0 if unset.
func newFixture(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *fixture {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Cluster{}).WithInterceptorFuncs(funcs).Build()
	assigner, err := sharding.NewAssigner(sharding.AssignerOptions{Replicas: 2})
	require.NoError(t, err)
	var list []*v1alpha1.Cluster
	for _, o := range objs {
		if cl, ok := o.(*v1alpha1.Cluster); ok {
			list = append(list, cl)
		}
	}
	f := &fixture{
		rec: &recorder{c: c, errs: map[string]error{}, labelErrs: map[string]error{}}, reader: c, counter: &serverCounter{},
		states: map[string]*clusters.CacheState{}, owned: ownedShards{0: true},
		now: time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC),
	}
	assigner.Init(append(list, synthetic()), nil)
	f.Updater, err = New(Options{
		Assigner: assigner, Owner: f.owned, Reader: c, Namespace: controlNS, Counter: f.counter, Recorder: f.rec,
		NamespaceAllowed: func(namespace string) bool { return namespace != unservedNS },
		CacheState:       func(server string) *clusters.CacheState { return f.states[server] },
		Now:              func() time.Time { return f.now },
	})
	require.NoError(t, err)
	return f
}

func (f *fixture) status(t *testing.T, name string) v1alpha1.ClusterStatus {
	t.Helper()
	c := &v1alpha1.Cluster{}
	require.NoError(t, f.reader.Get(context.Background(), client.ObjectKey{Namespace: controlNS, Name: name}, c))
	return c.Status
}

func clusterObj(name, server string, mutate ...func(*v1alpha1.Cluster)) *v1alpha1.Cluster {
	c := &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS, Generation: 1},
		Spec:       v1alpha1.ClusterSpec{Name: name, Server: server, Shard: ptr.To[int64](0)},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

// synthetic is the in-cluster cluster, which has no object.
func synthetic() *v1alpha1.Cluster {
	return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Name: clusters.InClusterName, Server: clusters.InClusterServer, Shard: ptr.To[int64](0)}}
}

func appProject(name string, sourceNamespaces ...string) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec:       v1alpha1.AppProjectSpec{SourceNamespaces: sourceNamespaces},
	}
}

func application(namespace, name, projectName, server string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.ApplicationSpec{Project: projectName, Destination: v1alpha1.ApplicationDestination{Server: server}},
	}
}

func synced(at time.Time) *clusters.CacheState {
	return &clusters.CacheState{ServerVersion: "v1.36.0", APIVersions: []string{"v1"}, APIsCount: 2, ResourcesCount: 10, LastCacheSyncTime: &at}
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()
	f := newFixture(t, interceptor.Funcs{})
	_, err := New(f.opts)
	require.NoError(t, err)
	tests := map[string]func(*Options){
		"assigner":         func(o *Options) { o.Assigner = nil },
		"owner":            func(o *Options) { o.Owner = nil },
		"reader":           func(o *Options) { o.Reader = nil },
		"namespace":        func(o *Options) { o.Namespace = "" },
		"namespace filter": func(o *Options) { o.NamespaceAllowed = nil },
		"counter":          func(o *Options) { o.Counter = nil },
		"cache state":      func(o *Options) { o.CacheState = nil },
		"recorder":         func(o *Options) { o.Recorder = nil },
	}
	for name, clear := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := f.opts
			clear(&opts)
			_, err := New(opts)
			require.Error(t, err)
		})
	}
}

func TestSyncWritesOwnedClusters(t *testing.T) {
	t.Parallel()
	skipped := func(c *v1alpha1.Cluster) {
		c.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
		c.Status.ServerVersion, c.Status.Info.APIVersions = "v1.35.0", []string{"v1"}
	}
	unnamed := 0
	f := newFixture(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "" {
				unnamed++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	},
		clusterObj("a-owned", "https://a"),
		clusterObj("b-other", "https://b", func(c *v1alpha1.Cluster) { c.Spec.Shard = ptr.To[int64](1) }),
		clusterObj("c-skipped", "https://c", skipped),
	)
	for _, server := range []string{"https://a", "https://b", "https://c", clusters.InClusterServer} {
		f.states[server] = synced(f.now)
	}
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, []string{"a-owned", "c-skipped"}, f.rec.taken(), "owned shards only")
	assert.Zero(t, unnamed, "the synthetic cluster has no object to read")

	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, f.status(t, "a-owned").ConnectionState.Status)
	assert.Nil(t, f.status(t, "b-other").ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusUnknown, f.status(t, "c-skipped").ConnectionState.Status, "no shard processes it, whatever the cache says")
	assert.Equal(t, "v1.35.0", f.status(t, "c-skipped").ServerVersion, "renders still read it")
	assert.Equal(t, []string{"v1"}, f.status(t, "c-skipped").Info.APIVersions)
}

func TestSyncClusterWithoutCache(t *testing.T) {
	t.Parallel()
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a", "https://a"), clusterObj("b", "https://b"), appProject("default"),
		application(controlNS, "app", "default", "https://b"))
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, clusters.NoApplicationsMessage, f.status(t, "a").ConnectionState.Message)
	assert.Equal(t, v1alpha1.ConnectionStatusUnknown, f.status(t, "b").ConnectionState.Status)
	assert.Empty(t, f.status(t, "b").ConnectionState.Message, "it has an Application, which has not been compared yet")
}

func TestSyncApplicationsCount(t *testing.T) {
	t.Parallel()
	// Sorts after the control namespace: a list across namespaces would let it win.
	elsewhere := appProject("default", "team-*")
	elsewhere.Namespace = "zzz-tenant"
	f := newFixture(t, interceptor.Funcs{},
		clusterObj("a", "https://a/"), clusterObj("b", "https://b"),
		appProject("default"), appProject("open", "team-*"), elsewhere,
		application(controlNS, "counted", "default", "https://a"),
		application(controlNS, "trailing-slash", "default", "https://a/"),
		application(controlNS, "no-project-field", "", "https://a"),
		application("team-a", "source-namespace", "open", "https://a"),
		application("team-a", "namespace-not-permitted", "default", "https://a"),
		application(unservedNS, "namespace-not-served", "open", "https://a"),
		application(controlNS, "project-missing", "gone", "https://a"),
		application(controlNS, "other-cluster", "default", "https://b"),
	)
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, int64(4), f.status(t, "a").Info.ApplicationsCount)
	assert.Equal(t, int64(1), f.status(t, "b").Info.ApplicationsCount)
}

// A round the interval cut short does not start over: with enough clusters that are always due,
// the last ones would never be reached.
func TestSyncResumesACutRound(t *testing.T) {
	t.Parallel()
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a", "https://a"), clusterObj("b", "https://b"), clusterObj("c", "https://c"), clusterObj("d", "https://d"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.rec.onWrite = func(name string) {
		if name == "b" {
			cancel()
		}
	}
	require.ErrorIs(t, f.Sync(ctx), context.Canceled)
	assert.Equal(t, []string{"a", "b"}, f.rec.taken())

	f.rec.onWrite = nil
	round := func() []string {
		t.Helper()
		f.now = f.now.Add(DefaultCacheInfoWriteInterval)
		for _, server := range []string{"https://a", "https://b", "https://c", "https://d"} {
			f.states[server] = synced(f.now)
		}
		require.NoError(t, f.Sync(context.Background()))
		return f.rec.taken()
	}
	assert.Equal(t, []string{"c", "d", "a", "b"}, round(), "from where the last round stopped, then the rest")
	assert.Equal(t, []string{"a", "b", "c", "d"}, round(), "a round that finished leaves nothing to resume")
}

// The Reader's objects are shared with the informer cache.
func TestSyncDoesNotChangeTheClusterItReads(t *testing.T) {
	t.Parallel()
	shared := clusterObj("a", "https://a")
	seen := clusters.ComputeClusterInfo(synced(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)), 0, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	clusters.ApplyClusterInfo(shared, seen)
	want := shared.DeepCopy()
	f := newFixture(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if cl, ok := obj.(*v1alpha1.Cluster); ok && key.Name == shared.Name {
				*cl = *shared // what the cache hands out with UnsafeDisableDeepCopy
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, shared.DeepCopy())
	failed := synced(f.now)
	failed.SyncError = errors.New("unreachable")
	f.states["https://a"] = failed
	f.rec.errs["a"] = errors.New("refused") // the recorder would write to the shared object here
	require.ErrorContains(t, f.Sync(context.Background()), "refused")
	assert.Equal(t, []string{"a"}, f.rec.taken())
	assert.Equal(t, want, shared)
}

func TestSyncSkipsUnchangedAndThrottlesCacheInfo(t *testing.T) {
	t.Parallel()
	const server = "https://a"
	// The object spells its server with a trailing slash: the throttle works on the normalized one.
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a", server+"/"), appProject("default"), application(controlNS, "one", "default", server))
	// A finer sync time than the API stores: the comparison must not take it for a change.
	f.states[server] = synced(f.now.Add(-time.Hour + 123*time.Millisecond))
	round := func(after time.Duration) []string {
		t.Helper()
		f.now = f.now.Add(after)
		require.NoError(t, f.Sync(context.Background()))
		return f.rec.taken()
	}
	assert.Equal(t, []string{"a"}, round(0), "first round")
	assert.Empty(t, round(10*time.Second), "nothing changed")

	f.states[server].ResourcesCount = 11
	assert.Empty(t, round(10*time.Second), "cache statistics alone wait")
	assert.Equal(t, int64(10), f.status(t, "a").Info.CacheInfo.ResourcesCount)
	assert.Equal(t, []string{"a"}, round(DefaultCacheInfoWriteInterval), "until the write interval passed")
	assert.Equal(t, int64(11), f.status(t, "a").Info.CacheInfo.ResourcesCount)
	assert.Empty(t, round(DefaultCacheInfoWriteInterval), "and only when something changed")

	f.states[server].APIVersions = []string{"v1", "apps/v1"}
	assert.Equal(t, []string{"a"}, round(time.Second), "a new API is written at once")

	f.states[server].ServerVersion = "v1.37.0"
	assert.Equal(t, []string{"a"}, round(time.Second), "a new version is written at once")

	c := &v1alpha1.Cluster{}
	require.NoError(t, f.reader.Get(context.Background(), client.ObjectKey{Namespace: controlNS, Name: "a"}, c))
	c.Generation++
	require.NoError(t, f.reader.Update(context.Background(), c))
	f.states[server].ResourcesCount = 12
	assert.Equal(t, []string{"a"}, round(time.Second), "a new generation is written at once, statistics or not")
	assert.Equal(t, c.Generation, f.status(t, "a").ObservedGeneration)

	require.NoError(t, f.reader.Create(context.Background(), application(controlNS, "two", "default", server)))
	assert.Equal(t, []string{"a"}, round(time.Second), "a new Application is written at once")

	f.states[server].SyncError = errors.New("cluster unreachable")
	assert.Equal(t, []string{"a"}, round(time.Second), "a failure is written at once")
	assert.Equal(t, "cluster unreachable", f.status(t, "a").ConnectionState.Message)
	f.states[server].SyncError = nil
	assert.Equal(t, []string{"a"}, round(time.Second), "and so is the recovery")
}

func TestSyncForgetsClustersItNoLongerOwns(t *testing.T) {
	t.Parallel()
	const server = "https://a"
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a", server))
	f.states[server] = synced(f.now)
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, []string{"a"}, f.rec.taken())

	f.owned[0] = false
	require.NoError(t, f.Sync(context.Background()))
	assert.Empty(t, f.lastWrite)

	f.owned[0] = true
	f.states[server].ResourcesCount++
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, []string{"a"}, f.rec.taken(), "a new owner writes what it finds")
}

func TestSyncVersionLabel(t *testing.T) {
	t.Parallel()
	optIn := func(c *v1alpha1.Cluster) { c.Labels = map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"} }
	f := newFixture(t, interceptor.Funcs{},
		clusterObj("a-opted-in", "https://a", optIn),
		clusterObj("b-no-label", "https://b"),
		clusterObj("c-no-version", "https://c", optIn),
		clusterObj("d-no-cache", "https://d", optIn),
		clusterObj("e-skipped", "https://e", optIn, func(c *v1alpha1.Cluster) {
			c.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
		}),
	)
	f.states["https://a"] = synced(f.now)
	f.states["https://b"] = synced(f.now)
	f.states["https://c"] = &clusters.CacheState{}
	f.states["https://e"] = synced(f.now)
	require.NoError(t, f.Sync(context.Background()))
	assert.Equal(t, []string{"a-opted-in=v1.36.0"}, f.rec.labels)

	require.NoError(t, f.Sync(context.Background()))
	assert.Len(t, f.rec.labels, 1, "the label is current")

	// The label follows the version even when the status does not need a write first.
	f.rec.labels = nil
	cur := &v1alpha1.Cluster{}
	require.NoError(t, f.reader.Get(context.Background(), client.ObjectKey{Namespace: controlNS, Name: "a-opted-in"}, cur))
	cur.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] = "v1.35.0"
	require.NoError(t, f.reader.Update(context.Background(), cur))
	f.rec.taken()
	f.now = f.now.Add(time.Second)
	require.NoError(t, f.Sync(context.Background()))
	assert.Empty(t, f.rec.taken())
	assert.Equal(t, []string{"a-opted-in=v1.36.0"}, f.rec.labels)
}

func TestSyncContinuesAfterErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a-fails", "https://a"), clusterObj("b-deleted", "https://b"), clusterObj("c", "https://c"))
	f.rec.errs["a-fails"] = errors.New("boom")
	f.rec.errs["b-deleted"] = apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("clusters").GroupResource(), "b-deleted")
	err := f.Sync(context.Background())
	require.ErrorContains(t, err, "cluster patrol/a-fails: boom")
	assert.NotContains(t, err.Error(), "b-deleted", "a cluster deleted meanwhile is no failure")
	assert.Equal(t, []string{"a-fails", "b-deleted", "c"}, f.rec.taken())
	assert.NotNil(t, f.status(t, "c").ConnectionState)

	_, written := f.lastWrite["https://a"]
	assert.False(t, written, "a failed write is not a write")
}

func TestSyncVersionLabelErrors(t *testing.T) {
	t.Parallel()
	optIn := func(c *v1alpha1.Cluster) { c.Labels = map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"} }
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a-fails", "https://a", optIn), clusterObj("b-deleted", "https://b", optIn))
	f.states["https://a"], f.states["https://b"] = synced(f.now), synced(f.now)
	f.rec.labelErrs["a-fails"] = errors.New("boom")
	f.rec.labelErrs["b-deleted"] = apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("clusters").GroupResource(), "b-deleted")
	err := f.Sync(context.Background())
	require.ErrorContains(t, err, "cluster patrol/a-fails: boom")
	assert.NotContains(t, err.Error(), "b-deleted", "a cluster deleted meanwhile is no failure")
}

func TestSyncClusterMissingFromReader(t *testing.T) {
	t.Parallel()
	f := newFixture(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "a" {
				return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("clusters").GroupResource(), key.Name)
			}
			if key.Name == "b" {
				return errors.New("read failed")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, clusterObj("a", "https://a"), clusterObj("b", "https://b"), clusterObj("c", "https://c"))
	err := f.Sync(context.Background())
	require.ErrorContains(t, err, "cluster patrol/b: read failed")
	assert.NotContains(t, err.Error(), "patrol/a", "a cluster deleted meanwhile is no failure")
	assert.Equal(t, []string{"c"}, f.rec.taken())
}

func TestSyncStopsWhenApplicationsCannotBeCounted(t *testing.T) {
	t.Parallel()
	for _, failing := range []string{"ApplicationList", "AppProjectList"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if strings.HasSuffix(failing, "ApplicationList") == isApplicationList(list) {
						return errors.New("list failed")
					}
					return c.List(ctx, list, opts...)
				},
			}, clusterObj("a", "https://a"))
			require.ErrorContains(t, f.Sync(context.Background()), "list failed")
			assert.Empty(t, f.rec.taken(), "a count of zero would be a guess")
		})
	}
}

func isApplicationList(list client.ObjectList) bool {
	_, ok := list.(*v1alpha1.ApplicationList)
	return ok
}

func TestSyncStopsAtTheEndOfItsContext(t *testing.T) {
	t.Parallel()
	const server = "https://a"
	f := newFixture(t, interceptor.Funcs{}, clusterObj("a", server))
	f.states[server] = synced(f.now)
	require.NoError(t, f.Sync(context.Background()))
	f.rec.taken()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, f.Sync(ctx), context.Canceled)
	assert.Empty(t, f.rec.taken())
	assert.Contains(t, f.lastWrite, server, "clusters a cut round did not reach are still owned")
}

func TestRunInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, clusterObj("a", "https://a"))
		f.opts.Interval = 30 * time.Second
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.Run(ctx)
		}()
		synctest.Wait()
		assert.Equal(t, int32(1), f.counter.rounds.Load(), "the first round runs at once")
		time.Sleep(29 * time.Second)
		synctest.Wait()
		assert.Equal(t, int32(1), f.counter.rounds.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, int32(2), f.counter.rounds.Load())
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.Equal(t, int32(3), f.counter.rounds.Load())
		cancel()
		<-done
	})
}

func TestRunBoundsARound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		f := newFixture(t, interceptor.Funcs{}, clusterObj("a", "https://a"))
		f.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		f.rec.block = true // a write that never answers
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.Run(ctx)
		}()
		time.Sleep(clusters.DefaultClusterInfoInterval + time.Second)
		synctest.Wait()
		assert.Equal(t, int32(2), f.counter.rounds.Load(), "the stuck write ended with its round, and the next one started")
		assert.Contains(t, logs.String(), "cannot record cluster info")
		cancel()
		<-done
		assert.Equal(t, 1, strings.Count(logs.String(), "cannot record cluster info"), "the round stopped by the shutdown is no failure")
	})
}
