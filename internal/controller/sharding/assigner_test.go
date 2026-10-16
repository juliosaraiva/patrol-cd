package sharding

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

const testNS = "patrol"

func clusterObj(uid, server string) *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "c-" + uid, UID: types.UID(uid)},
		Spec:       v1alpha1.ClusterSpec{Server: server, Name: "cluster-" + uid},
	}
}

func syntheticInCluster() *v1alpha1.Cluster {
	return &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: testNS}, Spec: v1alpha1.ClusterSpec{Server: clusters.InClusterServer, Name: clusters.InClusterName}}
}

func appObj(ns, name, server string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: server}},
	}
}

func newTestAssigner(t *testing.T, alg Algorithm, replicas int, reg prometheus.Registerer) *Assigner {
	t.Helper()
	a, err := NewAssigner(AssignerOptions{Algorithm: alg, Replicas: replicas, Registerer: reg})
	require.NoError(t, err)
	return a
}

type changeRecorder struct {
	mu      sync.Mutex
	batches [][]Change
}

func (r *changeRecorder) record(c []Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, c)
}

func (r *changeRecorder) take() [][]Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.batches
	r.batches = nil
	return out
}

func TestAssignerLegacy(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	a := newTestAssigner(t, AlgorithmLegacy, 5, reg)
	rec := &changeRecorder{}
	cancel := a.Subscribe(rec.record)
	defer cancel()

	a.Init([]*v1alpha1.Cluster{
		clusterObj("1", "https://c1"), clusterObj("2", "https://c2/"), clusterObj("3", "https://c3"), syntheticInCluster(),
	}, nil)
	assert.Equal(t, map[string]int{"https://c1": 4, "https://c2": 1, "https://c3": 2, clusters.InClusterServer: 0}, a.Distribution())
	batches := rec.take()
	require.Len(t, batches, 1)
	assert.Len(t, batches[0], 4)

	shard, managed := a.ShardForServer("https://c2/")
	assert.Equal(t, 1, shard)
	assert.True(t, managed)

	shard, managed = a.ShardForServer("https://unknown")
	assert.Equal(t, 0, shard, "an unknown cluster maps to shard 0")
	assert.True(t, managed)

	// Pin c1 to shard 3 (explicit spec.shard) and then out of range.
	c1 := clusterObj("1", "https://c1")
	c1.Spec.Shard = ptr.To[int64](3)
	a.SetCluster(c1)
	assert.Equal(t, 3, a.Distribution()["https://c1"])
	c1.Spec.Shard = ptr.To[int64](7)
	a.SetCluster(c1)
	assert.Equal(t, 0, a.Distribution()["https://c1"], "a pin beyond the replica count uses shard 0 (runtime behavior)")
	rec.take()

	// Skip-reconcile: still assigned, managed by no shard.
	c3 := clusterObj("3", "https://c3")
	c3.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
	a.SetCluster(c3)
	shard, managed = a.ShardForServer("https://c3")
	assert.Equal(t, 2, shard)
	assert.False(t, managed)
	batches = rec.take()
	require.Len(t, batches, 1)
	require.Len(t, batches[0], 1)
	assert.Equal(t, Change{Server: "https://c3", Old: &Assignment{Shard: 2}, New: &Assignment{Shard: 2, Skip: true}}, batches[0][0])

	// Setting an identical cluster changes nothing.
	a.SetCluster(c3)
	assert.Empty(t, rec.take())

	// Applications do not matter for legacy.
	a.SetApplication(appObj("apps", "a", "https://c1"))
	assert.Empty(t, rec.take())
	assert.Equal(t, map[string]int64{"https://c1": 1}, a.AppDistribution())

	a.DeleteCluster("https://c2/")
	batches = rec.take()
	require.Len(t, batches, 1)
	assert.Equal(t, []Change{{Server: "https://c2", Old: &Assignment{Shard: 1}}}, batches[0])

	assert.Equal(t, 5.0, testutil.ToFloat64(a.metrics.replicas))
	assert.Equal(t, 1.0, testutil.ToFloat64(a.metrics.shardClusters.WithLabelValues("2")))
	assert.Equal(t, 2.0, testutil.ToFloat64(a.metrics.shardClusters.WithLabelValues("0")))
	n, err := testutil.GatherAndCount(reg, "patrol_controller_cluster_shard")
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestAssignerReplicaChangeRebalances(t *testing.T) {
	t.Parallel()
	a := newTestAssigner(t, AlgorithmLegacy, 1, nil)
	var list []*v1alpha1.Cluster
	for i := range 20 {
		list = append(list, clusterObj(fmt.Sprintf("uid-%02d", i), fmt.Sprintf("https://c%02d", i)))
	}
	a.Init(list, nil)
	for _, s := range a.Distribution() {
		assert.Equal(t, 0, s, "one replica processes all clusters")
	}
	rec := &changeRecorder{}
	defer a.Subscribe(rec.record)()

	assert.True(t, a.SetReplicas(3))
	assert.False(t, a.SetReplicas(3))
	assert.Equal(t, 3, a.Replicas())
	dist := a.Distribution()
	for _, c := range list {
		info := ClusterInfoFrom(c)
		assert.Equal(t, LegacyShard(&info, 3), dist[info.Server])
	}
	batches := rec.take()
	require.Len(t, batches, 1)
	for _, ch := range batches[0] {
		require.NotNil(t, ch.Old)
		require.NotNil(t, ch.New)
		assert.NotEqual(t, ch.Old.Shard, ch.New.Shard)
	}

	assert.True(t, a.SetReplicas(0), "0 counts as one replica")
	assert.Equal(t, 1, a.Replicas())
}

// The baseline's TestClusterSharding_AddRoundRobin_Redistributes: a cluster sorting first
// shifts the others.
func TestAssignerRoundRobinRedistributesOnAdd(t *testing.T) {
	t.Parallel()
	a := newTestAssigner(t, AlgorithmRoundRobin, 2, nil)
	a.Init([]*v1alpha1.Cluster{clusterObj("2", "https://b"), clusterObj("3", "https://c")}, nil)
	assert.Equal(t, map[string]int{"https://b": 0, "https://c": 1}, a.Distribution())
	a.SetCluster(clusterObj("1", "https://a"))
	assert.Equal(t, map[string]int{"https://a": 0, "https://b": 1, "https://c": 0}, a.Distribution())

	// The synthetic in-cluster cluster has the empty ID and sorts first.
	a.SetCluster(syntheticInCluster())
	assert.Equal(t, 0, a.Distribution()[clusters.InClusterServer])
	assert.Equal(t, map[string]int{clusters.InClusterServer: 0, "https://a": 1, "https://b": 0, "https://c": 1}, a.Distribution())
}

func TestAssignerConsistentHashingUsesApplications(t *testing.T) {
	t.Parallel()
	a := newTestAssigner(t, AlgorithmConsistentHashing, 3, nil)
	var list []*v1alpha1.Cluster
	for i := range 12 {
		list = append(list, clusterObj(fmt.Sprintf("u%02d", i), fmt.Sprintf("https://c%02d", i)))
	}
	var apps []*v1alpha1.Application
	for i := range 30 {
		apps = append(apps, appObj("apps", fmt.Sprintf("a%02d", i), fmt.Sprintf("https://c%02d/", i%4)))
	}
	a.Init(list, apps)

	infos := make([]ClusterInfo, 0, len(list))
	for _, c := range list {
		infos = append(infos, ClusterInfoFrom(c))
	}
	counts := map[string]int64{}
	for _, app := range apps {
		counts[strings.TrimSuffix(app.Spec.Destination.Server, "/")]++
	}
	assert.Equal(t, counts, a.AppDistribution())
	want := Distribute(AlgorithmConsistentHashing, infos, counts, 3, nil)
	assert.Equal(t, want, a.Distribution())

	rec := &changeRecorder{}
	defer a.Subscribe(rec.record)()
	// Moving Applications changes loads and so possibly assignments; the table always matches
	// a fresh computation.
	for i := range 10 {
		a.SetApplication(appObj("apps", fmt.Sprintf("a%02d", i), "https://c11"))
	}
	a.DeleteApplication("apps", "a20")
	a.DeleteApplication("apps", "missing")
	counts = a.AppDistribution()
	assert.Equal(t, Distribute(AlgorithmConsistentHashing, infos, counts, 3, nil), a.Distribution())
	assert.Equal(t, int64(10), counts["https://c11"])
}

func TestAssignerHandleClusterEvent(t *testing.T) {
	t.Parallel()
	a := newTestAssigner(t, AlgorithmLegacy, 4, nil)
	a.HandleClusterEvent(clusters.Event{Type: clusters.EventAdded, Server: clusters.InClusterServer, New: syntheticInCluster()})
	assert.Equal(t, 0, a.Distribution()[clusters.InClusterServer])

	obj := clusterObj("7f3c9a52-uid", clusters.InClusterServer)
	a.HandleClusterEvent(clusters.Event{Type: clusters.EventModified, Server: clusters.InClusterServer, Old: syntheticInCluster(), New: obj, Changes: clusters.Changes{Identity: true}})
	info := ClusterInfoFrom(obj)
	assert.Equal(t, LegacyShard(&info, 4), a.Distribution()[clusters.InClusterServer], "an in-cluster object is hashed by its UID")
	assert.Equal(t, []ClusterInfo{info}, a.Clusters())

	a.HandleClusterEvent(clusters.Event{Type: clusters.EventDeleted, Server: clusters.InClusterServer, Old: obj})
	assert.Empty(t, a.Distribution())
	_, ok := a.Lookup(clusters.InClusterServer)
	assert.False(t, ok)
}

func TestAssignerConcurrentUse(t *testing.T) {
	t.Parallel()
	a := newTestAssigner(t, AlgorithmConsistentHashing, 3, prometheus.NewRegistry())
	defer a.Subscribe(func(ch []Change) { _ = a.Distribution() })()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 50 {
				id := fmt.Sprintf("%d-%d", w, i)
				a.SetCluster(clusterObj(id, "https://c"+id))
				a.SetApplication(appObj("ns", id, "https://c"+id))
				_, _ = a.ShardForServer("https://c" + id)
				if i%3 == 0 {
					a.DeleteCluster("https://c" + id)
					a.DeleteApplication("ns", id)
				}
				if i%10 == 0 {
					a.SetReplicas(2 + i%3)
				}
			}
		})
	}
	wg.Wait()
	assert.Len(t, a.Clusters(), 4*(50-17))
}
