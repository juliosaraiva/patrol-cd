package sharding

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// partitionable wraps a LeaseClient; while down is true every call fails like a network
// partition between the replica and the API server.
type partitionable struct {
	LeaseClient
	down atomic.Bool
}

func (p *partitionable) List(ctx context.Context) ([]coordinationv1.Lease, error) {
	if p.down.Load() {
		return nil, errPartition
	}
	return p.LeaseClient.List(ctx)
}

func (p *partitionable) Get(ctx context.Context, name string) (*coordinationv1.Lease, error) {
	if p.down.Load() {
		return nil, errPartition
	}
	return p.LeaseClient.Get(ctx, name)
}

func (p *partitionable) Create(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if p.down.Load() {
		return nil, errPartition
	}
	return p.LeaseClient.Create(ctx, l)
}

func (p *partitionable) Update(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if p.down.Load() {
		return nil, errPartition
	}
	return p.LeaseClient.Update(ctx, l)
}

// createControllerStatefulSet creates the controller StatefulSet object (no controller runs
// it in envtest; only spec.replicas matters).
func createControllerStatefulSet(t *testing.T, kube kubernetes.Interface, ns string, replicas int32) *appsv1.StatefulSet {
	t.Helper()
	labels := map[string]string{"app.kubernetes.io/name": "patrol-controller"}
	sts, err := kube.AppsV1().StatefulSets(ns).Create(context.Background(), &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultApplicationControllerName},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: "patrol-cd/patrol-cd:dev"}}},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return sts
}

func envNamespace(t *testing.T, kube kubernetes.Interface) string {
	t.Helper()
	ns, err := kube.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sharding-"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return ns.Name
}

// timeline records ownership callbacks with wall-clock times.
type timeline struct {
	mu     sync.Mutex
	events map[string]time.Time
}

func (tl *timeline) mark(e string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.events == nil {
		tl.events = map[string]time.Time{}
	}
	tl.events[e] = time.Now()
}

func (tl *timeline) at(e string) (time.Time, bool) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	v, ok := tl.events[e]
	return v, ok
}

type envReplica struct {
	m      *LeaseManager
	client *partitionable
	tl     *timeline
	cancel context.CancelFunc
	done   chan struct{}
}

func startEnvReplica(t *testing.T, kube kubernetes.Interface, ns, identity string, replicas ReplicaSource, onReplicas func(int)) *envReplica {
	t.Helper()
	pc := &partitionable{LeaseClient: NewLeaseClient(kube, ns)}
	tl := &timeline{}
	m, err := NewLeaseManager(LeaseManagerOptions{
		Client:            pc,
		Namespace:         ns,
		Identity:          identity,
		Config:            Config{Shard: -1, Hostname: identity, Heartbeat: time.Second},
		Replicas:          replicas,
		OnAcquired:        func(_ context.Context, s int) { tl.mark(fmt.Sprintf("%s+%d", identity, s)) },
		OnLost:            func(s int) { tl.mark(fmt.Sprintf("%s-%d", identity, s)) },
		OnReplicasChanged: onReplicas,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	r := &envReplica{m: m, client: pc, tl: tl, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		_ = m.Run(ctx)
	}()
	t.Cleanup(r.stop)
	return r
}

func (r *envReplica) stop() {
	r.cancel()
	<-r.done
}

func ownedEventually(t *testing.T, r *envReplica, want []int, within time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool { return fmt.Sprint(r.m.OwnedShards()) == fmt.Sprint(want) },
		within, 50*time.Millisecond, "owned shards %v, want %v", r.m.OwnedShards(), want)
}

// Lease-based failover against a real API server: partition, takeover by the standby after
// expiry, handoff back to the preferred replica, and immediate release on shutdown.
func TestLeaseFailoverEnvtest(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	kube := kubernetes.NewForConfigOrDie(cfg)
	ns := envNamespace(t, kube)
	r0 := startEnvReplica(t, kube, ns, "patrol-controller-0", StaticReplicas(2), nil)
	r1 := startEnvReplica(t, kube, ns, "patrol-controller-1", StaticReplicas(2), nil)
	ownedEventually(t, r0, []int{0}, 10*time.Second)
	ownedEventually(t, r1, []int{1}, 10*time.Second)

	lease, err := kube.CoordinationV1().Leases(ns).Get(context.Background(), LeaseName(1), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "patrol-controller-1", ptr.Deref(lease.Spec.HolderIdentity, ""))
	assert.Equal(t, int32(3), ptr.Deref(lease.Spec.LeaseDurationSeconds, 0))

	// Partition replica 1.
	partitioned := time.Now()
	r1.client.down.Store(true)
	ownedEventually(t, r0, []int{0, 1}, 15*time.Second)
	lost, ok := r1.tl.at("patrol-controller-1-1")
	require.True(t, ok, "the partitioned replica must report the loss")
	taken, ok := r0.tl.at("patrol-controller-0+1")
	require.True(t, ok)
	assert.True(t, lost.Before(taken), "the old holder stops (%s) before the new one starts (%s)", lost.Sub(partitioned), taken.Sub(partitioned))
	assert.GreaterOrEqual(t, taken.Sub(partitioned), 3*time.Second, "no takeover before the lease duration")
	assert.Empty(t, r1.m.OwnedShards())
	require.Error(t, r1.m.Healthz())

	// Heal: replica 1 asks for its shard and gets it back.
	r1.client.down.Store(false)
	ownedEventually(t, r1, []int{1}, 15*time.Second)
	ownedEventually(t, r0, []int{0}, 5*time.Second)
	lease, err = kube.CoordinationV1().Leases(ns).Get(context.Background(), LeaseName(1), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "patrol-controller-1", ptr.Deref(lease.Spec.HolderIdentity, ""))
	assert.NotContains(t, lease.Annotations, AnnotationShardHandoff)
	assert.GreaterOrEqual(t, ptr.Deref(lease.Spec.LeaseTransitions, 0), int32(2))

	// Graceful shutdown releases the Lease; the survivor takes it over.
	r1.stop()
	lease, err = kube.CoordinationV1().Leases(ns).Get(context.Background(), LeaseName(1), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, lease.Spec.HolderIdentity)
	ownedEventually(t, r0, []int{0, 1}, 15*time.Second)
}

// Dynamic cluster distribution: the shard count follows the StatefulSet's spec.replicas, new
// shards are taken over and removed shards released without restarting replicas, and the
// Assigner redistributes clusters.
func TestDynamicDistributionEnvtest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := envConfig(t)
	kube := kubernetes.NewForConfigOrDie(cfg)
	ns := envNamespace(t, kube)
	sts := createControllerStatefulSet(t, kube, ns, 2)
	var err error

	assigner := newTestAssigner(t, AlgorithmRoundRobin, 1, nil)
	var list []*v1alpha1.Cluster
	for i := range 6 {
		list = append(list, clusterObj(fmt.Sprintf("uid-%d", i), fmt.Sprintf("https://c%d", i)))
	}
	assigner.Init(list, nil)
	source := WorkloadReplicas{Client: kube, Namespace: ns, Name: DefaultApplicationControllerName}
	r0 := startEnvReplica(t, kube, ns, "patrol-controller-0", source, func(n int) { assigner.SetReplicas(n) })
	r1 := startEnvReplica(t, kube, ns, "patrol-controller-1", source, nil)
	ownedEventually(t, r0, []int{0}, 10*time.Second)
	ownedEventually(t, r1, []int{1}, 10*time.Second)
	require.Eventually(t, func() bool { return assigner.Replicas() == 2 }, 5*time.Second, 50*time.Millisecond)

	// Scale up to 3 without a third pod: a standby takes shard 2 once its (absent) Lease has
	// been seen for a lease duration plus the standby wait.
	sts.Spec.Replicas = ptr.To[int32](3)
	sts, err = kube.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return assigner.Replicas() == 3 }, 5*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		return len(r0.m.OwnedShards())+len(r1.m.OwnedShards()) == 3
	}, 15*time.Second, 50*time.Millisecond)
	count := map[int]int{}
	for _, s := range assigner.Distribution() {
		count[s]++
	}
	assert.Equal(t, map[int]int{0: 2, 1: 2, 2: 2}, count, "round-robin over 3 shards")

	// Scale down to 2: the holder of shard 2 releases it.
	sts.Spec.Replicas = ptr.To[int32](2)
	_, err = kube.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
	require.NoError(t, err)
	ownedEventually(t, r0, []int{0}, 10*time.Second)
	ownedEventually(t, r1, []int{1}, 10*time.Second)
	require.Eventually(t, func() bool { return assigner.Replicas() == 2 }, 5*time.Second, 50*time.Millisecond)
	lease, err := kube.CoordinationV1().Leases(ns).Get(ctx, LeaseName(2), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, lease.Spec.HolderIdentity)

	// Scaling to 0 fails readiness with upstream's message and keeps the last count.
	sts, err = kube.AppsV1().StatefulSets(ns).Get(ctx, DefaultApplicationControllerName, metav1.GetOptions{})
	require.NoError(t, err)
	sts.Spec.Replicas = ptr.To[int32](0)
	_, err = kube.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		err := r0.m.Healthz()
		return err != nil && strings.Contains(err.Error(), "application controller statefulset replicas is not set or is less than 0, replicas: 0")
	}, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, 2, r0.m.Replicas())
}

// With dynamic distribution and no readable replica count at startup, a replica claims no
// shard and reports no count until the controller workload can be read.
func TestDynamicDistributionWaitsForReplicaCountEnvtest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := envConfig(t)
	kube := kubernetes.NewForConfigOrDie(cfg)
	ns := envNamespace(t, kube)
	var (
		mu     sync.Mutex
		counts []int
	)
	source := WorkloadReplicas{Client: kube, Namespace: ns, Name: DefaultApplicationControllerName}
	r := startEnvReplica(t, kube, ns, "patrol-controller-0", source, func(n int) {
		mu.Lock()
		defer mu.Unlock()
		counts = append(counts, n)
	})
	require.Eventually(t, func() bool {
		err := r.m.Healthz()
		return err != nil && strings.Contains(err.Error(), "(dynamic cluster distribution) failed to get app controller statefulset")
	}, 10*time.Second, 50*time.Millisecond)
	require.Never(t, func() bool { return len(r.m.OwnedShards()) > 0 }, 3*time.Second, 100*time.Millisecond)
	leases, err := kube.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, leases.Items, "no shard Lease is claimed")
	mu.Lock()
	assert.Empty(t, counts)
	mu.Unlock()

	createControllerStatefulSet(t, kube, ns, 1)
	ownedEventually(t, r, []int{0}, 10*time.Second)
	require.Eventually(t, func() bool { return r.m.Healthz() == nil }, 5*time.Second, 50*time.Millisecond)
	mu.Lock()
	assert.Equal(t, []int{1}, counts)
	mu.Unlock()
}

// A shard Lease without the shard labels (pre-created, or labels removed while held) is found
// by name, acquired, kept and relabeled.
func TestUnlabeledShardLeaseEnvtest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := envConfig(t)
	kube := kubernetes.NewForConfigOrDie(cfg)
	ns := envNamespace(t, kube)
	_, err := kube.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName(0)}}, metav1.CreateOptions{})
	require.NoError(t, err)
	r := startEnvReplica(t, kube, ns, "patrol-controller-0", StaticReplicas(1), nil)
	ownedEventually(t, r, []int{0}, 10*time.Second)
	require.NoError(t, r.m.Healthz())
	wantLabels := map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf, v1alpha1.LabelKeyComponent: LeaseComponent}
	lease, err := kube.CoordinationV1().Leases(ns).Get(ctx, LeaseName(0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "patrol-controller-0", ptr.Deref(lease.Spec.HolderIdentity, ""))
	assert.Equal(t, wantLabels, lease.Labels)

	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		l, err := kube.CoordinationV1().Leases(ns).Get(ctx, LeaseName(0), metav1.GetOptions{})
		if err != nil {
			return err
		}
		l.Labels = nil
		_, err = kube.CoordinationV1().Leases(ns).Update(ctx, l, metav1.UpdateOptions{})
		return err
	}))
	require.Eventually(t, func() bool {
		l, err := kube.CoordinationV1().Leases(ns).Get(ctx, LeaseName(0), metav1.GetOptions{})
		return err == nil && l.Labels[v1alpha1.LabelKeyComponent] == LeaseComponent
	}, 10*time.Second, 50*time.Millisecond, "labels are restored on renewal")
	assert.Equal(t, []int{0}, r.m.OwnedShards())
	_, lost := r.tl.at("patrol-controller-0-0")
	assert.False(t, lost, "the shard is never reported lost")
	require.NoError(t, r.m.Healthz())
}

// Cluster.status.assignedShard is written through the status subresource of the real CRD.
func TestAssignedShardWriterEnvtest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := envConfig(t)
	kube := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ns := envNamespace(t, kube)
	var created []*v1alpha1.Cluster
	for i := range 4 {
		c := &v1alpha1.Cluster{
			TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster"},
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("cluster-%d", i)},
			Spec:       v1alpha1.ClusterSpec{Server: fmt.Sprintf("https://cluster-%d.example.com", i), Name: fmt.Sprintf("cluster-%d", i)},
		}
		if i == 3 {
			c.Spec.Shard = ptr.To[int64](1)
		}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c)
		require.NoError(t, err)
		out, err := dyn.Resource(clusters.ClusterGVR).Namespace(ns).Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
		require.NoError(t, err)
		var stored v1alpha1.Cluster
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(out.Object, &stored))
		created = append(created, &stored)
	}
	assigner := newTestAssigner(t, AlgorithmLegacy, 3, nil)
	assigner.Init(append(created, syntheticInCluster()), nil)
	w, err := NewAssignedShardWriter(assigner, ownedShards{0: true, 1: true, 2: true}, clusters.NewStatusWriter(dyn), nil)
	require.NoError(t, err)
	require.NoError(t, w.Sync(ctx))

	for _, c := range created {
		out, err := dyn.Resource(clusters.ClusterGVR).Namespace(ns).Get(ctx, c.Name, metav1.GetOptions{})
		require.NoError(t, err)
		var got v1alpha1.Cluster
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(out.Object, &got))
		want, _ := assigner.Lookup(c.Spec.Server)
		require.NotNil(t, got.Status.AssignedShard, c.Name)
		assert.Equal(t, int64(want.Shard), *got.Status.AssignedShard, c.Name)
		info := ClusterInfoFrom(c)
		if c.Spec.Shard == nil {
			assert.Equal(t, LegacyShard(&info, 3), want.Shard, "legacy hash of the object UID")
		} else {
			assert.Equal(t, 1, want.Shard)
		}
	}
}
