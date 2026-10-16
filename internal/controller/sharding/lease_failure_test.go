package sharding

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func testClock() *testingclock.FakeClock {
	return testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
}

// A replica that cannot read the replica count before it ever knew one claims no shard and
// reports no count: guessing one replica would map every cluster to its shard while its peers
// keep theirs (two replicas processing the same clusters). Upstream fails at startup instead.
func TestLeaseManagerFirstReplicaReadFailureClaimsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	r0 := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(2), nil)
	r1 := newTestReplica(t, api, clk, "patrol-controller-1", StaticReplicas(2), nil)
	r0.m.tick(ctx)
	r1.m.tick(ctx)
	require.Equal(t, []int{0}, r0.m.OwnedShards())
	require.Equal(t, []int{1}, r1.m.OwnedShards())

	// pod-0 restarts (same hostname, its Lease still names it) and the StatefulSet read is
	// forbidden.
	reps := &mutableReplicas{}
	var readErr error = apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, DefaultApplicationControllerName, errors.New("RBAC"))
	reps.err.Store(&readErr)
	var counts []int
	restarted := newTestReplica(t, api, clk, "patrol-controller-0", reps, func(o *LeaseManagerOptions) {
		o.Config.DynamicDistribution = true
		o.OnReplicasChanged = func(n int) { counts = append(counts, n) }
	})
	for i := range 5 {
		if i > 0 {
			clk.Step(10 * time.Second)
		}
		restarted.m.tick(ctx)
		r1.m.tick(ctx)
		assert.Empty(t, restarted.m.OwnedShards(), "heartbeat %d", i)
		assert.Empty(t, counts, "no shard count is reported before one was read")
		assert.Equal(t, 0, restarted.m.Replicas())
		assert.Equal(t, -1, restarted.m.PreferredShard())
		require.ErrorIs(t, restarted.m.Healthz(), readErr)
	}
	assert.Empty(t, restarted.log.take())
	// The old pod-0 never renews again: the peer takes shard 0 over after expiry.
	assert.Equal(t, []int{0, 1}, r1.m.OwnedShards())

	// The read recovers: the count is reported once and the preferred shard comes back
	// through a handoff.
	reps.err.Store(nil)
	reps.n.Store(2)
	clk.Step(10 * time.Second)
	restarted.m.tick(ctx)
	r1.m.tick(ctx)
	restarted.m.tick(ctx)
	assert.Equal(t, []int{2}, counts)
	assert.Equal(t, []int{0}, restarted.m.OwnedShards())
	assert.Equal(t, []int{1}, r1.m.OwnedShards())
	require.NoError(t, restarted.m.Healthz())
}

// Shard Leases are found by name: a Lease without the part-of/component labels (pre-created,
// or labels removed while held) is acquired and renewed, and its labels are restored.
func TestLeaseManagerSeesShardLeasesWithoutLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kube := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: LeaseName(0)}})
	clk := testClock()
	log := &ownershipLog{}
	m, err := NewLeaseManager(LeaseManagerOptions{
		Client:     NewLeaseClient(kube, testNS),
		Namespace:  testNS,
		Identity:   "patrol-controller-0",
		Config:     Config{Shard: -1, Hostname: "patrol-controller-0"},
		Replicas:   StaticReplicas(1),
		Clock:      clk,
		Registerer: prometheus.NewRegistry(),
		OnAcquired: func(_ context.Context, s int) { log.add("+" + LeaseName(s)) },
		OnLost:     func(s int) { log.add("-" + LeaseName(s)) },
	})
	require.NoError(t, err)
	m.tick(ctx)
	assert.Equal(t, []int{0}, m.OwnedShards())
	require.NoError(t, m.Healthz())
	wantLabels := map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf, v1alpha1.LabelKeyComponent: LeaseComponent}
	l, err := kube.CoordinationV1().Leases(testNS).Get(ctx, LeaseName(0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "patrol-controller-0", ptr.Deref(l.Spec.HolderIdentity, ""))
	assert.Equal(t, wantLabels, l.Labels)

	// Labels removed from the held Lease: it is still renewed (not treated as deleted).
	l.Labels = nil
	_, err = kube.CoordinationV1().Leases(testNS).Update(ctx, l, metav1.UpdateOptions{})
	require.NoError(t, err)
	for range 3 {
		clk.Step(10 * time.Second)
		m.tick(ctx)
		assert.Equal(t, []int{0}, m.OwnedShards())
	}
	assert.Equal(t, []string{"+" + LeaseName(0)}, log.take())
	l, err = kube.CoordinationV1().Leases(testNS).Get(ctx, LeaseName(0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, wantLabels, l.Labels)
	assert.Equal(t, clk.Now(), l.Spec.RenewTime.Time)
}

// failingCreate makes Create fail with err.
type failingCreate struct {
	LeaseClient
	err error
}

func (f failingCreate) Create(context.Context, *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	return nil, f.err
}

// A replica whose Lease writes are rejected (e.g. RBAC without leases/create) fails readiness
// instead of silently owning nothing.
func TestLeaseManagerAcquireErrorsFailHealthz(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	forbidden := apierrors.NewForbidden(leaseGR, LeaseName(0), errors.New("RBAC"))
	r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), func(o *LeaseManagerOptions) {
		o.Client = failingCreate{LeaseClient: o.Client, err: forbidden}
	})
	r.m.tick(ctx)
	assert.Empty(t, r.m.OwnedShards())
	require.ErrorIs(t, r.m.Healthz(), forbidden)
	assert.Contains(t, r.m.Healthz().Error(), "error while updating the heartbeat for the shard leases")
	assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.errors.WithLabelValues("acquire", reasonError)))
}

// A heartbeat that runs late (slow API call or callback) must not let ownership lapse and come
// back silently: the shard is reported lost and then re-acquired, so the controller stops
// while it was not owned and re-queues afterwards.
func TestLeaseManagerLateHeartbeatReportsLapse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	r := newTestReplica(t, api, clk, "patrol-controller-1", StaticReplicas(2), nil)
	r.m.tick(ctx)
	for range 4 {
		clk.Step(10 * time.Second)
		r.m.tick(ctx)
	}
	require.Equal(t, []int{0, 1}, r.m.OwnedShards(), "preferred shard 1 plus shard 0 as a standby")
	r.log.take()

	clk.Step(25 * time.Second)
	assert.False(t, r.m.OwnsShard(0))
	assert.False(t, r.m.OwnsShard(1))
	r.m.tick(ctx)
	assert.Equal(t, []string{"-0", "-1", "+1", "+0"}, r.log.take())
	assert.Equal(t, []int{0, 1}, r.m.OwnedShards())
	assert.Equal(t, int32(0), ptr.Deref(api.get(LeaseName(0)).Spec.LeaseTransitions, 0), "re-acquiring its own Lease is not a transition")
}

// updateHook fails Update calls for which hook returns an error.
type updateHook struct {
	LeaseClient
	hook atomic.Pointer[func(*coordinationv1.Lease) error]
}

func (u *updateHook) set(f func(*coordinationv1.Lease) error) { u.hook.Store(&f) }

func (u *updateHook) Update(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if f := u.hook.Load(); f != nil && *f != nil {
		if err := (*f)(l); err != nil {
			return nil, err
		}
	}
	return u.LeaseClient.Update(ctx, l)
}

func TestLeaseManagerRenewErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	hook := &updateHook{}
	r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), func(o *LeaseManagerOptions) {
		hook.LeaseClient = o.Client
		o.Client = hook
	})
	r.m.tick(ctx)
	require.Equal(t, []int{0}, r.m.OwnedShards())

	// A lost race (Conflict) is retried at the next heartbeat without failing readiness.
	hook.set(func(*coordinationv1.Lease) error {
		return apierrors.NewConflict(leaseGR, LeaseName(0), errors.New("the object has been modified"))
	})
	clk.Step(10 * time.Second)
	r.m.tick(ctx)
	require.NoError(t, r.m.Healthz())
	assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.errors.WithLabelValues("renew", reasonConflict)))

	// Any other renewal failure fails readiness at once.
	forbidden := apierrors.NewForbidden(leaseGR, LeaseName(0), errors.New("RBAC"))
	hook.set(func(*coordinationv1.Lease) error { return forbidden })
	clk.Step(5 * time.Second)
	r.m.tick(ctx)
	require.ErrorIs(t, r.m.Healthz(), forbidden)
	assert.Contains(t, r.m.Healthz().Error(), "renew shard 0")
	assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.errors.WithLabelValues("renew", reasonError)))
	assert.True(t, r.m.OwnsShard(0), "still within the renewal deadline")

	// Past the deadline the shard is lost; once writes work again it is re-acquired.
	clk.Step(5 * time.Second)
	r.m.tick(ctx)
	assert.False(t, r.m.OwnsShard(0))
	hook.set(nil)
	clk.Step(10 * time.Second)
	r.m.tick(ctx)
	require.NoError(t, r.m.Healthz())
	assert.Equal(t, []string{"+0", "-0", "+0"}, r.log.take())
}

// blockingUpdate makes Update hang until its context ends.
type blockingUpdate struct {
	LeaseClient
	block atomic.Bool
}

func (b *blockingUpdate) Update(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if b.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.LeaseClient.Update(ctx, l)
}

// A hung API call is bounded by CallTimeout, so renewals and the deadline check keep running.
func TestLeaseManagerHungCallIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	bc := &blockingUpdate{}
	r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), func(o *LeaseManagerOptions) {
		bc.LeaseClient = o.Client
		o.Client = bc
		o.CallTimeout = 50 * time.Millisecond
	})
	r.m.tick(ctx)
	require.Equal(t, []int{0}, r.m.OwnedShards())

	bc.block.Store(true)
	clk.Step(10 * time.Second)
	start := time.Now()
	r.m.tick(ctx)
	assert.Less(t, time.Since(start), 5*time.Second)
	require.ErrorIs(t, r.m.Healthz(), context.DeadlineExceeded)
	assert.Contains(t, r.m.Healthz().Error(), "renew shard 0")

	clk.Step(10 * time.Second)
	r.m.tick(ctx)
	assert.Empty(t, r.m.OwnedShards())
	assert.Equal(t, []string{"+0", "-0"}, r.log.take())
}

// labelFilteringClient lists only Leases with the shard labels, like a client using a label
// selector.
type labelFilteringClient struct{ LeaseClient }

func (c labelFilteringClient) List(ctx context.Context) ([]coordinationv1.Lease, error) {
	all, err := c.LeaseClient.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []coordinationv1.Lease
	for _, l := range all {
		if l.Labels[v1alpha1.LabelKeyPartOf] == v1alpha1.LabelValuePartOf && l.Labels[v1alpha1.LabelKeyComponent] == LeaseComponent {
			out = append(out, l)
		}
	}
	return out, nil
}

// With a List that hides a shard Lease, Create reports it exists: the Lease is read and taken
// when free; when held elsewhere, readiness fails after a lease duration of failed attempts.
func TestLeaseManagerHiddenShardLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hidden := func(t *testing.T, holder *string) (*fakeLeaseAPI, *testReplica, *testingclock.FakeClock) {
		t.Helper()
		api := newFakeLeaseAPI()
		_, err := api.client(nil).Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: LeaseName(0)},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: holder},
		})
		require.NoError(t, err)
		clk := testClock()
		r := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(1), func(o *LeaseManagerOptions) {
			o.Client = labelFilteringClient{LeaseClient: o.Client}
		})
		return api, r, clk
	}

	t.Run("free", func(t *testing.T) {
		t.Parallel()
		api, r, clk := hidden(t, nil)
		r.m.tick(ctx)
		assert.Equal(t, []int{0}, r.m.OwnedShards())
		require.NoError(t, r.m.Healthz())
		assert.Equal(t, LeaseComponent, api.get(LeaseName(0)).Labels[v1alpha1.LabelKeyComponent])
		assert.Equal(t, 1.0, testutil.ToFloat64(r.m.metrics.errors.WithLabelValues("acquire", reasonConflict)))
		clk.Step(10 * time.Second)
		r.m.tick(ctx)
		assert.Equal(t, []int{0}, r.m.OwnedShards(), "visible once labeled, and renewed")
		assert.Equal(t, []string{"+0"}, r.log.take())
	})

	t.Run("held elsewhere", func(t *testing.T) {
		t.Parallel()
		_, r, clk := hidden(t, ptr.To("ghost"))
		r.m.tick(ctx)
		require.NoError(t, r.m.Healthz(), "a lost race is not an error yet")
		for range 2 {
			clk.Step(10 * time.Second)
			r.m.tick(ctx)
			require.NoError(t, r.m.Healthz())
		}
		clk.Step(10 * time.Second)
		r.m.tick(ctx)
		err := r.m.Healthz()
		require.ErrorContains(t, err, "cannot acquire the preferred shard 0 for 30s")
		assert.True(t, apierrors.IsAlreadyExists(err))
		assert.Empty(t, r.m.OwnedShards())
		assert.Equal(t, 4.0, testutil.ToFloat64(r.m.metrics.errors.WithLabelValues("acquire", reasonConflict)))
	})
}

// A borrower whose release of a requested shard loses a race does not take its own Lease back:
// the Lease expires and the requester acquires it, never both owning it.
func TestLeaseManagerFailedHandoffReleaseIsNotUndone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testClock()
	hook := &updateHook{}
	r1 := newTestReplica(t, api, clk, "patrol-controller-1", StaticReplicas(2), func(o *LeaseManagerOptions) {
		hook.LeaseClient = o.Client
		o.Client = hook
	})
	r1.m.tick(ctx)
	for range 4 {
		clk.Step(10 * time.Second)
		r1.m.tick(ctx)
	}
	require.Equal(t, []int{0, 1}, r1.m.OwnedShards())
	r1.log.take()

	// pod-0 starts and asks for shard 0; the borrower's release write loses a race.
	r0 := newTestReplica(t, api, clk, "patrol-controller-0", StaticReplicas(2), nil)
	clk.Step(10 * time.Second)
	r0.m.tick(ctx)
	hook.set(func(l *coordinationv1.Lease) error {
		if l.Name == LeaseName(0) && l.Spec.HolderIdentity == nil {
			return apierrors.NewConflict(leaseGR, l.Name, errors.New("the object has been modified"))
		}
		return nil
	})
	r1.m.tick(ctx)
	assert.Equal(t, []string{"-0"}, r1.log.take())
	require.NoError(t, r1.m.Healthz())
	assert.Equal(t, "patrol-controller-1", ptr.Deref(api.get(LeaseName(0)).Spec.HolderIdentity, ""))

	for range 3 {
		clk.Step(10 * time.Second)
		r1.m.tick(ctx)
		r0.m.tick(ctx)
		for s := range 2 {
			assert.False(t, r0.m.OwnsShard(s) && r1.m.OwnsShard(s), "shard %d owned twice", s)
		}
	}
	assert.Empty(t, r1.log.take(), "the borrower must not take the requested shard back")
	assert.Equal(t, []string{"+0"}, r0.log.take(), "the requester takes the shard after expiry")
	assert.Equal(t, []int{0}, r0.m.OwnedShards())
	assert.Equal(t, []int{1}, r1.m.OwnedShards())
}

func TestLeaseManagerCallTimeoutValidation(t *testing.T) {
	t.Parallel()
	api := newFakeLeaseAPI()
	base := LeaseManagerOptions{Client: api.client(nil), Namespace: "ns", Identity: "x", Replicas: StaticReplicas(1)}
	for _, ct := range []time.Duration{-time.Second, 6 * time.Second} {
		o := base
		o.CallTimeout = ct
		_, err := NewLeaseManager(o)
		require.ErrorContains(t, err, "call timeout", ct)
	}
	m, err := NewLeaseManager(base)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, m.callTimeout, "half the default heartbeat")
}
