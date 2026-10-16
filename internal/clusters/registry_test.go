package clusters

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"pgregory.net/rapid"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const testNS = "patrol"

func clusterSecret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func withRef(secret string) func(*v1alpha1.Cluster) {
	return func(c *v1alpha1.Cluster) {
		c.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: secret}
	}
}

func toUnstructured(t *testing.T, c *v1alpha1.Cluster) *unstructured.Unstructured {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Cluster"))
	return u
}

type fakeEnv struct {
	reg  *Registry
	dyn  *dynamicfake.FakeDynamicClient
	kube *kubefake.Clientset
}

func newFakeEnv(t *testing.T, clusters []*v1alpha1.Cluster, extra []runtime.Object, secrets []runtime.Object, opts RegistryOptions) *fakeEnv {
	t.Helper()
	// The fake dynamic client converts to typed objects for kinds its scheme knows; an empty
	// scheme keeps everything unstructured, as a real dynamic client does.
	scheme := runtime.NewScheme()
	objs := slices.Clone(extra)
	for _, c := range clusters {
		objs = append(objs, toUnstructured(t, c))
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{ClusterGVR: "ClusterList"}, objs...)
	kube := kubefake.NewClientset(secrets...)
	opts.Namespace = testNS
	opts.ResyncPeriod = time.Hour
	reg, err := NewRegistry(dyn, kube, opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, reg.Start(ctx))
	return &fakeEnv{reg: reg, dyn: dyn, kube: kube}
}

func (e *fakeEnv) apply(t *testing.T, c *v1alpha1.Cluster) {
	t.Helper()
	res := e.dyn.Resource(ClusterGVR).Namespace(testNS)
	u := toUnstructured(t, c)
	if _, err := res.Get(context.Background(), c.Name, metav1.GetOptions{}); err == nil {
		_, err = res.Update(context.Background(), u, metav1.UpdateOptions{})
		require.NoError(t, err)
		return
	}
	_, err := res.Create(context.Background(), u, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (e *fakeEnv) remove(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, e.dyn.Resource(ClusterGVR).Namespace(testNS).Delete(context.Background(), name, metav1.DeleteOptions{}))
}

type eventLog struct {
	ch chan Event
}

func subscribe(t *testing.T, r *Registry) *eventLog {
	t.Helper()
	l := &eventLog{ch: make(chan Event, 256)}
	cancel := r.Subscribe(func(ev Event) { l.ch <- ev })
	t.Cleanup(cancel)
	return l
}

func (l *eventLog) next(t *testing.T) Event {
	t.Helper()
	select {
	case ev := <-l.ch:
		return ev
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for a registry event")
		return Event{}
	}
}

func (l *eventLog) expectNone(t *testing.T) {
	t.Helper()
	select {
	case ev := <-l.ch:
		t.Fatalf("unexpected event %s %s (%+v)", ev.Type, ev.Server, ev.Changes)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRegistryLookups covers REPO-077/078/081 lookups against fake clientsets.
func TestRegistryLookups(t *testing.T) {
	t.Parallel()
	malformed := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Cluster",
		"metadata": map[string]any{"name": "malformed", "namespace": testNS},
		"spec":     map[string]any{"name": "bad", "server": "https://bad", "shard": "abc"},
	}}
	env := newFakeEnv(t, []*v1alpha1.Cluster{
		cluster("a", "https://a.example/", withRef("a-creds")),
		cluster("b", "https://b.example"),
		cluster("c", "https://c.example", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
		cluster("d", "https://d.example", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
		cluster("e", "https://e.example", func(c *v1alpha1.Cluster) { c.Spec.Project = "team" }),
		cluster("f", "https://f.example", withRef("missing")),
	}, []runtime.Object{malformed}, []runtime.Object{
		clusterSecret("a-creds", map[string]string{"bearerToken": "tok"}),
	}, RegistryOptions{})
	r := env.reg

	c, err := r.ByServer("https://a.example///")
	require.NoError(t, err)
	assert.Equal(t, "https://a.example", c.Spec.Server, "trailing slashes are normalized")

	_, err = r.ByServer("https://nope.example")
	require.EqualError(t, err, `cluster "https://nope.example" not found`)
	require.ErrorIs(t, err, ErrClusterNotFound)

	all, err := r.All()
	require.NoError(t, err)
	var servers []string
	for _, c := range all {
		servers = append(servers, c.Spec.Server)
	}
	assert.Equal(t, []string{"https://a.example", "https://b.example", "https://c.example", "https://d.example", "https://e.example", "https://f.example", InClusterServer}, servers,
		"malformed objects are skipped and the synthetic in-cluster cluster is listed")
	assert.Equal(t, InClusterName, all[len(all)-1].Spec.Name)
	assert.True(t, IsSynthetic(all[len(all)-1]))

	_, err = r.ByServer("https://bad")
	require.ErrorIs(t, err, ErrClusterNotFound)

	dup, err := r.ServersByName("dup")
	require.NoError(t, err)
	assert.Equal(t, []string{"https://c.example", "https://d.example"}, dup)
	_, err = r.ByName("dup")
	require.EqualError(t, err, "there are 2 clusters with the same name: [https://c.example https://d.example]")
	require.ErrorIs(t, err, ErrAmbiguousName)
	_, err = r.ByName("nope")
	require.EqualError(t, err, "there are no clusters with this name: nope")
	byName, err := r.ByName("b")
	require.NoError(t, err)
	assert.Equal(t, "https://b.example", byName.Spec.Server)
	inCluster, err := r.ServersByName(InClusterName)
	require.NoError(t, err)
	assert.Equal(t, []string{InClusterServer}, inCluster, "in-cluster resolves without an object")

	proj, err := r.ProjectClusters("team")
	require.NoError(t, err)
	require.Len(t, proj, 1)
	assert.Equal(t, "e", proj[0].Name)

	data, err := r.SecretData(c)
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"bearerToken": []byte("tok")}, data)
	f, err := r.ByServer("https://f.example")
	require.NoError(t, err)
	_, err = r.SecretData(f)
	require.ErrorIs(t, err, ErrSecretNotFound)
	assert.Contains(t, err.Error(), "cd.patrol.example/secret-type=cluster")
	_, err = r.RestConfig(context.Background(), f, Options{})
	require.ErrorIs(t, err, ErrSecretNotFound)
	cfg, err := r.RestConfig(context.Background(), c, Options{})
	require.NoError(t, err)
	assert.Equal(t, "tok", cfg.BearerToken)

	// Destination resolution (upstream messages).
	_, err = r.ResolveDestination(v1alpha1.ApplicationDestination{})
	require.EqualError(t, err, "Destination server missing from app spec")
	_, err = r.ResolveDestination(v1alpha1.ApplicationDestination{Name: "b", Server: "https://b.example"})
	require.EqualError(t, err, "application destination can't have both name and server defined: b https://b.example")
	s, err := r.ResolveDestination(v1alpha1.ApplicationDestination{Server: "https://unknown/"})
	require.NoError(t, err)
	assert.Equal(t, "https://unknown", s)
	_, err = r.DestinationCluster(v1alpha1.ApplicationDestination{Server: "https://unknown"})
	require.EqualError(t, err, `error getting cluster by server "https://unknown": cluster "https://unknown" not found`)
	_, err = r.DestinationCluster(v1alpha1.ApplicationDestination{Name: "dup"})
	require.EqualError(t, err, "there are 2 clusters with the same name: [https://c.example https://d.example]")
	dc, err := r.DestinationCluster(v1alpha1.ApplicationDestination{Name: InClusterName})
	require.NoError(t, err)
	assert.Equal(t, InClusterServer, dc.Spec.Server)

	apps := []v1alpha1.Application{
		{Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: "https://b.example/"}}},
		{Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Name: "b"}}},
		{Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Name: "dup"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "denied"}, Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Name: "b"}}},
	}
	assert.Equal(t, int64(2), r.CountApplications(apps, "https://b.example", func(a *v1alpha1.Application) bool { return a.Name != "denied" }))
}

func TestRegistryNotSynced(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ClusterGVR: "ClusterList"})
	r, err := NewRegistry(dyn, kubefake.NewClientset(), RegistryOptions{Namespace: testNS})
	require.NoError(t, err)
	_, err = r.ByServer("https://a")
	require.ErrorIs(t, err, ErrNotSynced)
	_, err = NewRegistry(dyn, kubefake.NewClientset(), RegistryOptions{})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, r.Start(ctx), ErrNotSynced)
}

// TestRegistryInClusterEvents covers REPO-092/093 including the event semantics of the
// in-cluster server.
func TestRegistryInClusterEvents(t *testing.T) {
	t.Parallel()
	env := newFakeEnv(t, []*v1alpha1.Cluster{cluster("a", "https://a.example")}, nil, nil, RegistryOptions{
		LocalServerVersion: func(context.Context) (string, error) { return "v1.36.0", nil },
	})
	r := env.reg
	events := subscribe(t, r)
	got := map[string]Event{}
	for range 2 {
		ev := events.next(t)
		require.Equal(t, EventAdded, ev.Type)
		got[ev.Server] = ev
	}
	require.Contains(t, got, InClusterServer)
	syn := got[InClusterServer].New
	assert.True(t, IsSynthetic(syn))
	require.NotNil(t, syn.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, syn.Status.ConnectionState.Status)
	assert.Equal(t, "v1.36.0", syn.Status.ServerVersion)

	// An object for the in-cluster server replaces the synthetic cluster (Modified).
	env.apply(t, cluster("local", InClusterServer, func(c *v1alpha1.Cluster) { c.Spec.Namespaces = []string{"apps"} }))
	ev := events.next(t)
	assert.Equal(t, EventModified, ev.Type)
	assert.Equal(t, InClusterServer, ev.Server)
	assert.True(t, ev.Changes.Identity)
	assert.True(t, IsSynthetic(ev.Old))
	assert.Equal(t, "local", ev.New.Name)
	c, err := r.ByServer(InClusterServer)
	require.NoError(t, err)
	assert.Equal(t, "local", c.Spec.Name)
	names, err := r.ServersByName(InClusterName)
	require.NoError(t, err)
	assert.Equal(t, []string{InClusterServer}, names, "upstream: in-cluster still resolves when no cluster carries the name")

	// Deleting it restores the synthetic cluster (Modified).
	env.remove(t, "local")
	ev = events.next(t)
	assert.Equal(t, EventModified, ev.Type)
	assert.Equal(t, "local", ev.Old.Name)
	assert.True(t, IsSynthetic(ev.New))

	// Disabling deletes it.
	r.SetInClusterEnabled(false)
	ev = events.next(t)
	assert.Equal(t, EventDeleted, ev.Type)
	assert.Equal(t, InClusterServer, ev.Server)
	_, err = r.ByServer(InClusterServer)
	require.EqualError(t, err, `cluster "https://kubernetes.default.svc" is disabled`)
	require.ErrorIs(t, err, ErrInClusterDisabled)
	all, err := r.All()
	require.NoError(t, err)
	require.Len(t, all, 1)
	_, err = r.DestinationCluster(v1alpha1.ApplicationDestination{Name: InClusterName})
	require.EqualError(t, err, "there are no clusters with this name: in-cluster")
	_, err = r.DestinationCluster(v1alpha1.ApplicationDestination{Server: InClusterServer})
	require.EqualError(t, err, `error getting cluster by server "https://kubernetes.default.svc": cluster "https://kubernetes.default.svc" is disabled`)

	// A disabled in-cluster object stays hidden.
	env.apply(t, cluster("local", InClusterServer))
	events.expectNone(t)
	names, err = r.ServersByName("local")
	require.NoError(t, err)
	assert.Empty(t, names)

	r.SetInClusterEnabled(true)
	ev = events.next(t)
	assert.Equal(t, EventAdded, ev.Type)
	assert.Equal(t, "local", ev.New.Name)
	assert.True(t, r.InClusterEnabled())
}

// TestRegistryChangeEvents covers spec, metadata, credentials and status-only changes.
func TestRegistryChangeEvents(t *testing.T) {
	t.Parallel()
	env := newFakeEnv(t, []*v1alpha1.Cluster{cluster("a", "https://a.example", withRef("a-creds"))}, nil,
		[]runtime.Object{clusterSecret("a-creds", map[string]string{"bearerToken": "one"})}, RegistryOptions{InClusterEnabled: ptr.To(false)})
	r := env.reg
	events := subscribe(t, r)
	ev := events.next(t)
	require.Equal(t, EventAdded, ev.Type)
	require.Equal(t, "https://a.example", ev.Server)
	events.expectNone(t)

	cur, err := r.ByServer("https://a.example")
	require.NoError(t, err)

	// Status-only update: no event.
	st := cur.DeepCopy()
	st.Status.ServerVersion = "v1.36.0"
	env.apply(t, st)
	events.expectNone(t)

	// Annotation: Metadata.
	md := st.DeepCopy()
	md.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
	env.apply(t, md)
	ev = events.next(t)
	assert.Equal(t, EventModified, ev.Type)
	assert.Equal(t, Changes{Metadata: true}, ev.Changes)
	assert.True(t, IsSkipReconcile(ev.New))

	// Spec: Spec.
	sp := md.DeepCopy()
	sp.Spec.Namespaces = []string{"x"}
	env.apply(t, sp)
	ev = events.next(t)
	assert.Equal(t, Changes{Spec: true}, ev.Changes)
	assert.True(t, DiffCacheSettings(ev.Old, ev.New).Namespaces)

	// Credentials Secret update and deletion: Credentials.
	sec := clusterSecret("a-creds", map[string]string{"bearerToken": "two"})
	sec.ResourceVersion = ""
	_, err = env.kube.CoreV1().Secrets(testNS).Update(context.Background(), sec, metav1.UpdateOptions{})
	require.NoError(t, err)
	ev = events.next(t)
	assert.Equal(t, Changes{Credentials: true}, ev.Changes)
	data, err := r.SecretData(ev.New)
	require.NoError(t, err)
	assert.Equal(t, "two", string(data["bearerToken"]))
	require.NoError(t, env.kube.CoreV1().Secrets(testNS).Delete(context.Background(), "a-creds", metav1.DeleteOptions{}))
	ev = events.next(t)
	assert.Equal(t, Changes{Credentials: true}, ev.Changes)

	// An unrelated secret: no event.
	_, err = env.kube.CoreV1().Secrets(testNS).Create(context.Background(), clusterSecret("other", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	events.expectNone(t)

	// Deletion.
	env.remove(t, "a")
	ev = events.next(t)
	assert.Equal(t, EventDeleted, ev.Type)
	assert.Nil(t, ev.New)
	assert.Equal(t, "a", ev.Old.Name)

	// A late subscriber gets the current state only.
	env.apply(t, cluster("b", "https://b.example"))
	_ = events.next(t)
	late := subscribe(t, r)
	ev = late.next(t)
	assert.Equal(t, EventAdded, ev.Type)
	assert.Equal(t, "https://b.example", ev.Server)
	late.expectNone(t)
}

func TestRegistryLocalProbeFailure(t *testing.T) {
	t.Parallel()
	env := newFakeEnv(t, nil, nil, nil, RegistryOptions{
		LocalServerVersion: func(context.Context) (string, error) { return "", errors.New("connection refused") },
	})
	c, err := env.reg.ByServer(InClusterServer)
	require.NoError(t, err)
	require.NotNil(t, c.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, c.Status.ConnectionState.Status)
	assert.Equal(t, "connection refused", c.Status.ConnectionState.Message)
	assert.NotNil(t, c.Status.ConnectionState.AttemptedAt)
}

// TestRegistryDuplicateServers: the object with the smallest name serves a server declared
// twice (the internal/project ClusterIndex rule).
func TestRegistryDuplicateServers(t *testing.T) {
	t.Parallel()
	env := newFakeEnv(t, []*v1alpha1.Cluster{
		cluster("b-second", "https://dup.example", func(c *v1alpha1.Cluster) { c.Spec.Name = "second" }),
		cluster("a-first", "https://dup.example", func(c *v1alpha1.Cluster) { c.Spec.Name = "first" }),
	}, nil, nil, RegistryOptions{})
	c, err := env.reg.ByServer("https://dup.example")
	require.NoError(t, err)
	assert.Equal(t, "a-first", c.Name)
	s, err := env.reg.ServersByName("second")
	require.NoError(t, err)
	assert.Empty(t, s, "the shadowed duplicate does not answer to its name")
	s, err = env.reg.GetClusterServersByName(context.Background(), "first")
	require.NoError(t, err)
	assert.Equal(t, []string{"https://dup.example"}, s)
}

// --- model-based property test of the registry core -----------------------------------

type registryModel struct {
	objs      map[string]*v1alpha1.Cluster
	secrets   map[string]string // name -> resource version
	inCluster bool
}

func (m *registryModel) effective(server string) *v1alpha1.Cluster {
	if server == InClusterServer && !m.inCluster {
		return nil
	}
	var cands []*v1alpha1.Cluster
	for _, c := range m.objs {
		if c.Spec.Server == server {
			cands = append(cands, c)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Name < cands[j].Name })
	if len(cands) > 0 {
		return cands[0]
	}
	if server == InClusterServer {
		return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Name: InClusterName, Server: InClusterServer}}
	}
	return nil
}

func (m *registryModel) view(servers []string) map[string]string {
	out := map[string]string{}
	for _, s := range servers {
		if c := m.effective(s); c != nil {
			out[s] = c.Name + "/" + c.Spec.Name
		}
	}
	return out
}

func (m *registryModel) serversByName(name string) []string {
	set := map[string]bool{}
	for _, s := range propServers {
		if c := m.effective(s); c != nil && c.Spec.Name == name {
			set[s] = true
		}
	}
	if len(set) == 0 && name == InClusterName && m.inCluster {
		return []string{InClusterServer}
	}
	return slices.Sorted(maps.Keys(set))
}

var (
	propServers = []string{"https://s1.example", "https://s2.example", InClusterServer}
	propNames   = []string{"n1", "n2", InClusterName}
	propObjs    = []string{"c1", "c2", "c3", "c4"}
	propSecrets = []string{"sec1", "sec2"}
)

// TestRegistryEventsMatchModel drives the registry core with random object, Secret and setting
// changes and checks that (1) replaying the published events reproduces the effective view,
// (2) lookups agree with an independent model, (3) status-only updates publish nothing, and
// (4) credential changes publish Modified events with Changes.Credentials for every affected
// server.
func TestRegistryEventsMatchModel(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		clusters := cache.NewIndexer(cache.MetaNamespaceKeyFunc, clusterIndexers())
		secrets := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		r := newRegistry(clusters, secrets, RegistryOptions{Namespace: testNS})
		var published []Event
		r.publishHook = func(ev Event) { published = append(published, ev) }
		r.markReady()
		m := &registryModel{objs: map[string]*v1alpha1.Cluster{}, secrets: map[string]string{}, inCluster: true}
		replay := map[string]string{}
		apply := func(evs []Event) {
			for _, ev := range evs {
				switch ev.Type {
				case EventAdded:
					require.NotContains(rt, replay, ev.Server, "Added for a present server")
					replay[ev.Server] = ev.New.Name + "/" + ev.New.Spec.Name
				case EventModified:
					require.Contains(rt, replay, ev.Server, "Modified for an absent server")
					require.NotEqual(rt, Changes{}, ev.Changes)
					replay[ev.Server] = ev.New.Name + "/" + ev.New.Spec.Name
				case EventDeleted:
					require.Contains(rt, replay, ev.Server, "Deleted for an absent server")
					delete(replay, ev.Server)
				}
			}
		}
		apply(published)
		rv := 0
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			published = nil
			op := rapid.IntRange(0, 5).Draw(rt, "op")
			switch op {
			case 0: // upsert a cluster object
				name := rapid.SampledFrom(propObjs).Draw(rt, "obj")
				c := cluster(name, rapid.SampledFrom(propServers).Draw(rt, "server"), func(c *v1alpha1.Cluster) {
					c.Namespace = testNS
					c.Spec.Name = rapid.SampledFrom(propNames).Draw(rt, "name")
					if ref := rapid.IntRange(0, 2).Draw(rt, "ref"); ref > 0 {
						c.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: propSecrets[ref-1]}
					}
					c.Labels = map[string]string{"l": rapid.SampledFrom([]string{"a", "b"}).Draw(rt, "label")}
				})
				old, existed := m.objs[name]
				m.objs[name] = c
				if existed {
					require.NoError(rt, clusters.Update(c))
					r.onClusterChange(old, c)
				} else {
					require.NoError(rt, clusters.Add(c))
					r.onClusterChange(c)
				}
			case 1: // delete a cluster object
				name := rapid.SampledFrom(propObjs).Draw(rt, "obj")
				old, existed := m.objs[name]
				if !existed {
					continue
				}
				delete(m.objs, name)
				require.NoError(rt, clusters.Delete(old))
				r.onClusterChange(cache.DeletedFinalStateUnknown{Key: testNS + "/" + name, Obj: old})
			case 2: // status-only update
				name := rapid.SampledFrom(propObjs).Draw(rt, "obj")
				old, existed := m.objs[name]
				if !existed {
					continue
				}
				c := old.DeepCopy()
				rv++
				c.Status.ServerVersion = fmt.Sprintf("v1.%d.0", rv)
				m.objs[name] = c
				require.NoError(rt, clusters.Update(c))
				r.onClusterChange(old, c)
				require.Empty(rt, published, "status-only updates publish nothing")
			case 3, 4: // upsert or delete a secret
				name := rapid.SampledFrom(propSecrets).Draw(rt, "secret")
				s := clusterSecret(name, nil)
				if op == 3 {
					rv++
					s.ResourceVersion = fmt.Sprint(rv)
					s.Data["token"] = []byte(s.ResourceVersion)
					m.secrets[name] = s.ResourceVersion
					require.NoError(rt, secrets.Update(s))
				} else {
					if _, ok := m.secrets[name]; !ok {
						continue
					}
					delete(m.secrets, name)
					require.NoError(rt, secrets.Delete(s))
				}
				r.onSecretChange(s)
				want := map[string]bool{}
				for _, srv := range propServers {
					if c := m.effective(srv); c != nil && c.Spec.Config.CredentialsRef != nil && c.Spec.Config.CredentialsRef.Name == name {
						want[srv] = true
					}
				}
				gotCreds := map[string]bool{}
				for _, ev := range published {
					require.Equal(rt, EventModified, ev.Type)
					require.True(rt, ev.Changes.Credentials)
					gotCreds[ev.Server] = true
				}
				require.Equal(rt, want, gotCreds)
			case 5: // toggle in-cluster
				m.inCluster = !m.inCluster
				r.SetInClusterEnabled(m.inCluster)
			}
			apply(published)
			require.Equal(rt, m.view(propServers), replay, "event replay matches the model after op %d", op)
			for _, s := range propServers {
				c, err := r.ByServer(s)
				if want := m.effective(s); want == nil {
					require.ErrorIs(rt, err, ErrClusterNotFound)
				} else {
					require.NoError(rt, err)
					require.Equal(rt, want.Name, c.Name)
					require.Equal(rt, want.Spec.Name, c.Spec.Name)
				}
			}
			for _, n := range propNames {
				got, err := r.ServersByName(n)
				require.NoError(rt, err)
				if len(got) == 0 {
					got = nil
				}
				want := m.serversByName(n)
				if len(want) == 0 {
					want = nil
				}
				require.Equal(rt, want, got, "servers for name %q", n)
			}
		}
	})
}

// TestRegistryEnvtest runs the registry against a real API server with the Cluster CRD:
// informers, label-selected Secrets, change events and destination resolution.
func TestRegistryEnvtest(t *testing.T) {
	cfg := envConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns := createNamespace(t, cfg, "registry")
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	res := dyn.Resource(ClusterGVR).Namespace(ns)
	create := func(c *v1alpha1.Cluster) {
		t.Helper()
		c.Namespace = ns
		_, err := res.Create(ctx, toUnstructured(t, c), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	sec := clusterSecret("prod-creds", map[string]string{"bearerToken": "one"})
	sec.Namespace = ns
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
	require.NoError(t, err)
	unlabeled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: ns}, Data: map[string][]byte{"bearerToken": []byte("x")}}
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, unlabeled, metav1.CreateOptions{})
	require.NoError(t, err)
	create(cluster("prod", "https://prod.example/", withRef("prod-creds"), func(c *v1alpha1.Cluster) { c.Spec.Project = "team" }))
	create(cluster("staging", "https://staging.example", withRef("unlabeled")))

	r, err := NewRegistry(dyn, kube, RegistryOptions{Namespace: ns, LocalServerVersion: func(ctx context.Context) (string, error) {
		return Probe(ctx, cfg)
	}})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx))
	events := subscribe(t, r)
	seen := map[string]bool{}
	for range 3 {
		ev := events.next(t)
		require.Equal(t, EventAdded, ev.Type)
		seen[ev.Server] = true
	}
	assert.Equal(t, map[string]bool{"https://prod.example": true, "https://staging.example": true, InClusterServer: true}, seen)

	prod, err := r.DestinationCluster(v1alpha1.ApplicationDestination{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "https://prod.example", prod.Spec.Server)
	data, err := r.SecretData(prod)
	require.NoError(t, err)
	assert.Equal(t, "one", string(data["bearerToken"]))
	staging, err := r.ByServer("https://staging.example")
	require.NoError(t, err)
	_, err = r.SecretData(staging)
	require.ErrorIs(t, err, ErrSecretNotFound, "unlabeled Secrets are not visible")
	local, err := r.ByServer(InClusterServer)
	require.NoError(t, err)
	require.NotNil(t, local.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, local.Status.ConnectionState.Status)
	assert.Regexp(t, `^v1\.\d+\.\d+$`, local.Status.ServerVersion)
	team, err := r.ProjectClusters("team")
	require.NoError(t, err)
	require.Len(t, team, 1)

	// Credential rotation and a status write.
	cur, err := kube.CoreV1().Secrets(ns).Get(ctx, "prod-creds", metav1.GetOptions{})
	require.NoError(t, err)
	cur.Data["bearerToken"] = []byte("two")
	_, err = kube.CoreV1().Secrets(ns).Update(ctx, cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	ev := events.next(t)
	assert.Equal(t, EventModified, ev.Type)
	assert.Equal(t, Changes{Credentials: true}, ev.Changes)
	_, err = NewStatusWriter(dyn).RecordProbe(ctx, prod, "v1.36.0", nil)
	require.NoError(t, err)
	events.expectNone(t)

	// Skip-reconcile annotation (REPO-097).
	u, err := res.Get(ctx, "prod", metav1.GetOptions{})
	require.NoError(t, err)
	u.SetAnnotations(map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"})
	_, err = res.Update(ctx, u, metav1.UpdateOptions{})
	require.NoError(t, err)
	ev = events.next(t)
	assert.True(t, ev.Changes.Metadata)
	assert.True(t, IsSkipReconcile(ev.New))

	// An object for the in-cluster server overrides the synthetic cluster.
	create(cluster("local", InClusterServer, func(c *v1alpha1.Cluster) { c.Spec.Name = "local" }))
	ev = events.next(t)
	assert.Equal(t, EventModified, ev.Type)
	assert.True(t, IsSynthetic(ev.Old))
	assert.Equal(t, "local", ev.New.Spec.Name)
	require.NoError(t, res.Delete(ctx, "local", metav1.DeleteOptions{}))
	ev = events.next(t)
	assert.True(t, IsSynthetic(ev.New))

	require.NoError(t, res.Delete(ctx, "staging", metav1.DeleteOptions{}))
	ev = events.next(t)
	assert.Equal(t, EventDeleted, ev.Type)
	assert.Equal(t, "https://staging.example", ev.Server)
}
