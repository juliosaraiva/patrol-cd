package sharding

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

// fakeLeaseAPI is an in-memory Lease store with optimistic concurrency (fake clientsets do
// not check resourceVersion).
type fakeLeaseAPI struct {
	mu    sync.Mutex
	items map[string]*coordinationv1.Lease
	rv    int
}

func newFakeLeaseAPI() *fakeLeaseAPI { return &fakeLeaseAPI{items: map[string]*coordinationv1.Lease{}} }

var leaseGR = schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}

func (f *fakeLeaseAPI) get(name string) *coordinationv1.Lease {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.items[name]; ok {
		return l.DeepCopy()
	}
	return nil
}

// client returns a LeaseClient view; while *down is true every call fails (a partition).
func (f *fakeLeaseAPI) client(down *atomic.Bool) LeaseClient {
	return &fakeLeaseClient{api: f, down: down}
}

type fakeLeaseClient struct {
	api  *fakeLeaseAPI
	down *atomic.Bool
}

var errPartition = errors.New("connection refused")

func (c *fakeLeaseClient) check() error {
	if c.down != nil && c.down.Load() {
		return errPartition
	}
	return nil
}

func (c *fakeLeaseClient) List(context.Context) ([]coordinationv1.Lease, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	c.api.mu.Lock()
	defer c.api.mu.Unlock()
	var out []coordinationv1.Lease
	for _, l := range c.api.items {
		out = append(out, *l.DeepCopy())
	}
	return out, nil
}

func (c *fakeLeaseClient) Get(_ context.Context, name string) (*coordinationv1.Lease, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if l := c.api.get(name); l != nil {
		return l, nil
	}
	return nil, apierrors.NewNotFound(leaseGR, name)
}

func (c *fakeLeaseClient) Create(_ context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	c.api.mu.Lock()
	defer c.api.mu.Unlock()
	if _, ok := c.api.items[l.Name]; ok {
		return nil, apierrors.NewAlreadyExists(leaseGR, l.Name)
	}
	c.api.rv++
	n := l.DeepCopy()
	n.ResourceVersion = strconv.Itoa(c.api.rv)
	c.api.items[l.Name] = n
	return n.DeepCopy(), nil
}

func (c *fakeLeaseClient) Update(_ context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	c.api.mu.Lock()
	defer c.api.mu.Unlock()
	cur, ok := c.api.items[l.Name]
	if !ok {
		return nil, apierrors.NewNotFound(leaseGR, l.Name)
	}
	if l.ResourceVersion != cur.ResourceVersion {
		return nil, apierrors.NewConflict(leaseGR, l.Name, errors.New("the object has been modified"))
	}
	c.api.rv++
	n := l.DeepCopy()
	n.ResourceVersion = strconv.Itoa(c.api.rv)
	c.api.items[l.Name] = n
	return n.DeepCopy(), nil
}

// mutableReplicas is a ReplicaSource tests can change.
type mutableReplicas struct {
	n   atomic.Int64
	err atomic.Pointer[error]
}

func (r *mutableReplicas) Replicas(context.Context) (int, error) {
	if e := r.err.Load(); e != nil && *e != nil {
		return 0, *e
	}
	return int(r.n.Load()), nil
}

type ownershipLog struct {
	mu     sync.Mutex
	events []string
}

func (o *ownershipLog) add(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *ownershipLog) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.events
	o.events = nil
	return out
}

type testReplica struct {
	m    *LeaseManager
	down *atomic.Bool
	log  *ownershipLog
}

func newTestReplica(t *testing.T, api *fakeLeaseAPI, clk *testingclock.FakeClock, identity string, replicas ReplicaSource, mutate func(*LeaseManagerOptions)) *testReplica {
	t.Helper()
	down := &atomic.Bool{}
	log := &ownershipLog{}
	opts := LeaseManagerOptions{
		Client:     api.client(down),
		Namespace:  testNS,
		Identity:   identity,
		Config:     Config{Shard: -1, Hostname: identity, Heartbeat: 10 * time.Second},
		Replicas:   replicas,
		Clock:      clk,
		Registerer: prometheus.NewRegistry(),
		OnAcquired: func(_ context.Context, s int) { log.add(fmt.Sprintf("+%d", s)) },
		OnLost:     func(s int) { log.add(fmt.Sprintf("-%d", s)) },
	}
	if mutate != nil {
		mutate(&opts)
	}
	m, err := NewLeaseManager(opts)
	require.NoError(t, err)
	return &testReplica{m: m, down: down, log: log}
}

func TestNewLeaseManagerValidation(t *testing.T) {
	t.Parallel()
	api := newFakeLeaseAPI()
	_, err := NewLeaseManager(LeaseManagerOptions{})
	require.Error(t, err)
	_, err = NewLeaseManager(LeaseManagerOptions{Client: api.client(nil), Namespace: "ns", Identity: "x", Replicas: StaticReplicas(1), LeaseDuration: 5 * time.Second})
	require.ErrorContains(t, err, "must be longer than the heartbeat")
}

func TestLeaseManagerAcquiresAndRenewsPreferredShard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	var replicaCounts []int
	r := newTestReplica(t, api, clk, "patrol-controller-1", StaticReplicas(2), func(o *LeaseManagerOptions) {
		o.OnReplicasChanged = func(n int) { replicaCounts = append(replicaCounts, n) }
	})

	r.m.tick(ctx)
	assert.Equal(t, []string{"+1"}, r.log.take())
	assert.True(t, r.m.OwnsShard(1))
	assert.False(t, r.m.OwnsShard(0))
	assert.Equal(t, []int{1}, r.m.OwnedShards())
	assert.Equal(t, 1, r.m.PreferredShard())
	assert.Equal(t, 2, r.m.Replicas())
	assert.Equal(t, []int{2}, replicaCounts)
	require.NoError(t, r.m.Healthz())

	l := api.get(LeaseName(1))
	require.NotNil(t, l)
	assert.Equal(t, "patrol-controller-1", *l.Spec.HolderIdentity)
	assert.Equal(t, int32(30), *l.Spec.LeaseDurationSeconds)
	assert.Equal(t, map[string]string{"app.kubernetes.io/part-of": "patrol-cd", "app.kubernetes.io/component": "controller"}, l.Labels)
	assert.Equal(t, int32(0), *l.Spec.LeaseTransitions)

	// Heartbeats renew; shard 0 has no Lease and is not taken before the standby wait
	// (lease duration + one heartbeat per held shard = 40s).
	for range 3 {
		clk.Step(10 * time.Second)
		r.m.tick(ctx)
	}
	assert.Equal(t, clk.Now(), api.get(LeaseName(1)).Spec.RenewTime.Time)
	assert.Nil(t, api.get(LeaseName(0)))
	assert.Empty(t, r.log.take())
	clk.Step(10 * time.Second)
	r.m.tick(ctx)
	assert.Equal(t, []string{"+0"}, r.log.take(), "an orphaned shard is taken over after the standby wait")
	assert.Equal(t, []int{0, 1}, r.m.OwnedShards())
	assert.Equal(t, []int{2}, replicaCounts)
	assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.owned.WithLabelValues("0")))
	assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.transitions.WithLabelValues("1", eventAcquired)))
}

// A replica partitioned from the API loses its shard after the renewal deadline; another
// replica takes the Lease over after it stayed unrenewed for the lease duration (plus the
// standby wait); when the preferred replica returns, it asks for the shard and the borrower
// hands it back.
func TestLeaseManagerFailoverAndHandoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	r0 := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(2), nil)
	r1 := newTestReplica(t, api, clk, "patrol-controller-1", StaticReplicas(2), nil)
	both := func() {
		r0.m.tick(ctx)
		r1.m.tick(ctx)
	}
	both()
	assert.Equal(t, []string{"+0"}, r0.log.take())
	assert.Equal(t, []string{"+1"}, r1.log.take())

	r1.down.Store(true)
	var lostAt, takenAt time.Duration
	start := clk.Now()
	for i := 1; i <= 8 && takenAt == 0; i++ {
		clk.Step(10 * time.Second)
		both()
		if ev := r1.log.take(); len(ev) > 0 {
			assert.Equal(t, []string{"-1"}, ev)
			lostAt = clk.Since(start)
		}
		if ev := r0.log.take(); len(ev) > 0 {
			assert.Equal(t, []string{"+1"}, ev)
			takenAt = clk.Since(start)
		}
	}
	assert.Equal(t, 20*time.Second, lostAt, "the partitioned holder stops at the renewal deadline")
	// r0 first saw r1's Lease at t=10s (r1 created it after r0's first heartbeat), so it waits
	// until 10s + lease duration (30s) + one heartbeat per held shard (10s).
	assert.Equal(t, 50*time.Second, takenAt, "the standby takes over after lease duration + one heartbeat from its own observation")
	assert.Less(t, lostAt, takenAt)
	require.Error(t, r1.m.Healthz())
	assert.Contains(t, r1.m.Healthz().Error(), "error while updating the heartbeat for the shard leases")
	assert.Equal(t, []int{0, 1}, r0.m.OwnedShards())
	assert.Empty(t, r1.m.OwnedShards())
	assert.Equal(t, int32(1), *api.get(LeaseName(1)).Spec.LeaseTransitions)

	// The preferred replica comes back: it requests the shard, the borrower releases it, the
	// preferred replica acquires it.
	r1.down.Store(false)
	clk.Step(10 * time.Second)
	r1.m.tick(ctx)
	require.NoError(t, r1.m.Healthz())
	assert.Equal(t, "patrol-controller-1", api.get(LeaseName(1)).Annotations[AnnotationShardHandoff])
	assert.Empty(t, r1.log.take())
	r0.m.tick(ctx)
	assert.Equal(t, []string{"-1"}, r0.log.take())
	assert.Nil(t, api.get(LeaseName(1)).Spec.HolderIdentity)
	r1.m.tick(ctx)
	assert.Equal(t, []string{"+1"}, r1.log.take())
	l := api.get(LeaseName(1))
	assert.Equal(t, "patrol-controller-1", *l.Spec.HolderIdentity)
	assert.NotContains(t, l.Annotations, AnnotationShardHandoff)
	assert.Equal(t, []int{0}, r0.m.OwnedShards())
	assert.Equal(t, []int{1}, r1.m.OwnedShards())

	// Stable afterwards.
	for range 6 {
		clk.Step(10 * time.Second)
		both()
	}
	assert.Empty(t, r0.log.take())
	assert.Empty(t, r1.log.take())
}

func TestLeaseManagerReplicaCountChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	reps := &mutableReplicas{}
	reps.n.Store(3)
	var counts []int
	r2 := newTestReplica(t, api, clk, "patrol-controller-2", reps, func(o *LeaseManagerOptions) {
		o.OnReplicasChanged = func(n int) { counts = append(counts, n) }
	})
	r2.m.tick(ctx)
	assert.Equal(t, []string{"+2"}, r2.log.take())

	// Scale down to 2: shard 2 disappears and its Lease is released.
	reps.n.Store(2)
	clk.Step(10 * time.Second)
	r2.m.tick(ctx)
	assert.Equal(t, []string{"-2"}, r2.log.take())
	assert.Nil(t, api.get(LeaseName(2)).Spec.HolderIdentity)
	assert.Equal(t, -1, r2.m.PreferredShard(), "ordinal beyond the replica count: standby")
	assert.Equal(t, []int{3, 2}, counts)

	// A replica-count read error keeps the last count and fails readiness.
	readErr := errors.New("statefulset unavailable")
	reps.err.Store(&readErr)
	clk.Step(10 * time.Second)
	r2.m.tick(ctx)
	assert.Equal(t, 2, r2.m.Replicas())
	require.ErrorIs(t, r2.m.Healthz(), readErr)
	reps.err.Store(nil)

	// Scale up again: shard 2 is preferred again.
	reps.n.Store(3)
	clk.Step(10 * time.Second)
	r2.m.tick(ctx)
	assert.Equal(t, []string{"+2"}, r2.log.take())
	assert.Equal(t, []int{3, 2, 3}, counts)
	require.NoError(t, r2.m.Healthz())
}

// blockingReleaseClient holds the Update that clears a Lease's holder until release is closed.
type blockingReleaseClient struct {
	LeaseClient
	blocked chan struct{} // closed when a release Update is waiting
	release chan struct{}
	once    sync.Once
}

func (c *blockingReleaseClient) Update(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if l.Spec.HolderIdentity == nil {
		c.once.Do(func() { close(c.blocked) })
		<-c.release
	}
	return c.LeaseClient.Update(ctx, l)
}

// A replica stops processing a shard and stops reporting it as owned before it clears the
// Lease holder, so no other replica can start the shard while it still runs here. A reader of
// the Lease can therefore still see the old holder right after OwnedShards changed (CI-F19).
func TestLeaseManagerReleaseStopsOwningFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	reps := &mutableReplicas{}
	reps.n.Store(3)
	bc := &blockingReleaseClient{blocked: make(chan struct{}), release: make(chan struct{})}
	r2 := newTestReplica(t, api, clk, "patrol-controller-2", reps, func(o *LeaseManagerOptions) {
		bc.LeaseClient = o.Client
		o.Client = bc
	})
	r2.m.tick(ctx)
	require.Equal(t, []string{"+2"}, r2.log.take())

	reps.n.Store(2)
	clk.Step(10 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r2.m.tick(ctx)
	}()
	select {
	case <-bc.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the release Update was never sent")
	}
	assert.Empty(t, r2.m.OwnedShards())
	assert.Equal(t, []string{"-2"}, r2.log.take(), "OnLost runs before the release Update")
	assert.Equal(t, "patrol-controller-2", ptr.Deref(api.get(LeaseName(2)).Spec.HolderIdentity, ""))

	close(bc.release)
	<-done
	assert.Nil(t, api.get(LeaseName(2)).Spec.HolderIdentity)
}

func TestLeaseManagerStandbyRaceHasOneWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	// Three standbys (explicit shards beyond the replica count) compete for shard 0.
	var rs []*testReplica
	for i := range 3 {
		rs = append(rs, newTestReplica(t, api, clk, fmt.Sprintf("standby-%d", i), StaticReplicas(2), func(o *LeaseManagerOptions) {
			o.Config.Shard = 5
			o.MaxShards = 1
		}))
	}
	tickAll := func() {
		var wg sync.WaitGroup
		for _, r := range rs {
			wg.Go(func() { r.m.tick(ctx) })
		}
		wg.Wait()
	}
	tickAll()
	for range 4 {
		clk.Step(10 * time.Second)
		tickAll()
	}
	owners := map[int]int{}
	for _, r := range rs {
		for _, s := range r.m.OwnedShards() {
			owners[s]++
		}
		assert.LessOrEqual(t, len(r.m.OwnedShards()), 1, "MaxShards")
	}
	assert.Equal(t, map[int]int{0: 1, 1: 1}, owners, "every shard has exactly one owner")
}

func TestLeaseManagerRunReleasesOnShutdown(t *testing.T) {
	t.Parallel()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.m.Run(ctx) }()
	require.Eventually(t, func() bool { return r.m.OwnsShard(0) }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.False(t, r.m.OwnsShard(0))
	assert.Nil(t, api.get(LeaseName(0)).Spec.HolderIdentity, "the Lease is released for an immediate takeover")
	assert.Equal(t, []string{"+0", "-0"}, r.log.take())
}

func TestLeaseManagerHolderChangedElsewhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), nil)
	r.m.tick(ctx)
	r.log.take()
	// Someone else overwrote the holder: ownership is dropped at the next heartbeat and the
	// preferred shard is then requested.
	l := api.get(LeaseName(0))
	l.Spec.HolderIdentity = ptr.To("intruder")
	_, err := api.client(nil).Update(ctx, l)
	require.NoError(t, err)
	clk.Step(10 * time.Second)
	r.m.tick(ctx)
	assert.Equal(t, []string{"-0"}, r.log.take())
	assert.Equal(t, "patrol-controller-0", api.get(LeaseName(0)).Annotations[AnnotationShardHandoff])
	// The intruder never renews: after the lease duration the preferred replica takes it.
	for range 3 {
		clk.Step(10 * time.Second)
		r.m.tick(ctx)
	}
	assert.Equal(t, []string{"+0"}, r.log.take())
}

func TestWorkloadReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kube := fake.NewClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "patrol-controller"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](3)}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "zero"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](0)}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "unset"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "patrol-controller"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)}},
	)
	n, err := WorkloadReplicas{Client: kube, Namespace: testNS, Name: "patrol-controller"}.Replicas(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = WorkloadReplicas{Client: kube, Namespace: testNS, Name: "patrol-controller", Kind: WorkloadDeployment}.Replicas(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	_, err = WorkloadReplicas{Client: kube, Namespace: testNS, Name: "missing"}.Replicas(ctx)
	require.ErrorContains(t, err, "(dynamic cluster distribution) failed to get app controller statefulset")
	_, err = WorkloadReplicas{Client: kube, Namespace: testNS, Name: "zero"}.Replicas(ctx)
	require.ErrorContains(t, err, "application controller statefulset replicas is not set or is less than 0, replicas: 0")
	require.ErrorIs(t, err, ErrInvalidReplicas)
	_, err = WorkloadReplicas{Client: kube, Namespace: testNS, Name: "unset"}.Replicas(ctx)
	require.ErrorContains(t, err, "failed to get app controller statefulset replica count")

	n, err = StaticReplicas(0).Replicas(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
