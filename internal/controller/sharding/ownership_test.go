package sharding

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// fakeDestinations resolves names through a fixed table and servers as is (like the registry).
type fakeDestinations map[string]string

func (f fakeDestinations) ResolveDestination(dest v1alpha1.ApplicationDestination) (string, error) {
	switch {
	case dest.Server != "" && dest.Name != "":
		return "", errors.New("both set")
	case dest.Server != "":
		return clusters.NormalizeServer(dest.Server), nil
	case dest.Name != "":
		if s, ok := f[dest.Name]; ok {
			return s, nil
		}
		return "", errors.New("there are no clusters with this name: " + dest.Name)
	}
	return "", errors.New(clusters.ErrDestinationMissing)
}

type ownedShards map[int]bool

func (o ownedShards) OwnsShard(s int) bool { return o[s] }

func newOwnershipFixture(t *testing.T, owned ownedShards) (*Ownership, *Assigner) {
	t.Helper()
	a := newTestAssigner(t, AlgorithmLegacy, 5, nil)
	skip := clusterObj("4", "https://skipped")
	skip.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
	a.Init([]*v1alpha1.Cluster{clusterObj("1", "https://c1"), clusterObj("2", "https://c2"), skip, syntheticInCluster()}, nil)
	o, err := NewOwnership(a, owned, fakeDestinations{"named": "https://c2"}, nil)
	require.NoError(t, err)
	return o, a
}

func TestOwnershipShardForApplication(t *testing.T) {
	t.Parallel()
	o, _ := newOwnershipFixture(t, ownedShards{1: true})
	for _, tc := range []struct {
		name    string
		dest    v1alpha1.ApplicationDestination
		shard   int
		managed bool
		owned   bool
	}{
		{"server", v1alpha1.ApplicationDestination{Server: "https://c1"}, 4, true, false},
		{"name", v1alpha1.ApplicationDestination{Name: "named"}, 1, true, true},
		{"server with trailing slash", v1alpha1.ApplicationDestination{Server: "https://c2/"}, 1, true, true},
		{"in-cluster", v1alpha1.ApplicationDestination{Server: clusters.InClusterServer}, 0, true, false},
		{"skip-reconcile cluster", v1alpha1.ApplicationDestination{Server: "https://skipped"}, 2, false, false},
		{"unresolvable name goes to shard 0", v1alpha1.ApplicationDestination{Name: "nope"}, 0, true, false},
		{"unregistered server goes to shard 0", v1alpha1.ApplicationDestination{Server: "https://unknown"}, 0, true, false},
		{"missing destination goes to shard 0", v1alpha1.ApplicationDestination{}, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Destination: tc.dest}}
			shard, managed := o.ShardForApplication(app)
			assert.Equal(t, tc.shard, shard)
			assert.Equal(t, tc.managed, managed)
			assert.Equal(t, tc.owned, o.OwnsApplication(app))
		})
	}
	assert.True(t, o.OwnsCluster("https://c2"))
	assert.False(t, o.OwnsCluster("https://c1"))
	assert.True(t, o.OwnsClusterObject(clusterObj("2", "https://c2/")))
	assert.False(t, o.OwnsClusterObject(nil), "unresolvable destinations belong to shard 0")
	o0, _ := newOwnershipFixture(t, ownedShards{0: true, 2: true})
	assert.True(t, o0.OwnsCluster(clusters.InClusterServer))
	assert.False(t, o0.OwnsCluster("https://skipped"), "skip-reconcile clusters are owned by nobody")
}

func TestOwnershipPredicates(t *testing.T) {
	t.Parallel()
	o, _ := newOwnershipFixture(t, ownedShards{1: true})
	mine := appObj("apps", "mine", "https://c2")
	other := appObj("apps", "other", "https://c1")
	p := o.ApplicationPredicate()
	assert.True(t, p.Create(event.CreateEvent{Object: mine}))
	assert.False(t, p.Create(event.CreateEvent{Object: other}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: mine, ObjectNew: other}), "leaving the shard is seen by the old owner")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: other, ObjectNew: mine}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: other, ObjectNew: other}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: mine}))
	assert.False(t, p.Generic(event.GenericEvent{Object: other}))
	assert.False(t, p.Create(event.CreateEvent{Object: &v1alpha1.Cluster{}}), "other kinds are filtered out")

	apps := map[string]*v1alpha1.Application{"mine": mine, "other": other}
	readErr := errors.New("cache not synced")
	get := func(_ context.Context, ns, name string) (*v1alpha1.Application, error) {
		if name == "broken" {
			return nil, readErr
		}
		if a, ok := apps[name]; ok && a.Namespace == ns {
			return a, nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "applications"}, name)
	}
	op := func(app string) *v1alpha1.Operation {
		return &v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "op-" + app}, Spec: v1alpha1.OperationSpec{Application: app}}
	}
	opPred := o.OperationPredicate(get)
	assert.True(t, opPred.Create(event.CreateEvent{Object: op("mine")}))
	assert.False(t, opPred.Create(event.CreateEvent{Object: op("other")}))
	assert.True(t, opPred.Create(event.CreateEvent{Object: op("broken")}), "lookup errors let the event through")
	assert.False(t, opPred.Create(event.CreateEvent{Object: op("gone")}), "orphan operations belong to shard 0")
	o0, _ := newOwnershipFixture(t, ownedShards{0: true})
	assert.True(t, o0.OwnsOperation(context.Background(), op("gone"), get))
}

type fakeStatusUpdater struct {
	mu      sync.Mutex
	objects map[string]*v1alpha1.Cluster
	writes  []string
	fail    error
}

func (f *fakeStatusUpdater) Update(_ context.Context, ns, name string, mutate func(*v1alpha1.Cluster) error) (*v1alpha1.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	c, ok := f.objects[ns+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "clusters"}, name)
	}
	before := ptr.Deref(c.Status.AssignedShard, -1)
	if err := mutate(c); err != nil {
		return nil, err
	}
	if ptr.Deref(c.Status.AssignedShard, -1) != before {
		f.writes = append(f.writes, name)
	}
	return c.DeepCopy(), nil
}

func TestAssignedShardWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := newTestAssigner(t, AlgorithmLegacy, 5, nil)
	c1, c2 := clusterObj("1", "https://c1"), clusterObj("2", "https://c2")
	a.Init([]*v1alpha1.Cluster{c1, c2, syntheticInCluster(), clusterObj("gone", "https://gone")}, nil)
	up := &fakeStatusUpdater{objects: map[string]*v1alpha1.Cluster{testNS + "/" + c1.Name: c1.DeepCopy(), testNS + "/" + c2.Name: c2.DeepCopy()}}
	owner := &switchableOwner{owned: map[int]bool{1: true, 4: true, 0: true}}
	w, err := NewAssignedShardWriter(a, owner, up, nil)
	require.NoError(t, err)

	require.NoError(t, w.Sync(ctx))
	assert.Equal(t, int64(4), *up.objects[testNS+"/c-1"].Status.AssignedShard)
	assert.Equal(t, int64(1), *up.objects[testNS+"/c-2"].Status.AssignedShard)
	require.NoError(t, w.Sync(ctx))
	assert.Len(t, up.writes, 2, "values already written are not rewritten")

	// Only the owner of the cluster's shard writes it.
	owner.set(map[int]bool{4: true})
	a.SetReplicas(3)
	w.Trigger()
	require.NoError(t, w.Sync(ctx))
	assert.Equal(t, int64(4), *up.objects[testNS+"/c-1"].Status.AssignedShard, "not owned any more: left for the new owner")

	up.fail = errors.New("apiserver unavailable")
	owner.set(map[int]bool{0: true, 1: true, 2: true})
	w.Trigger()
	err = w.Sync(ctx)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "write assigned shard of cluster patrol/c-"))
}

type switchableOwner struct {
	mu    sync.Mutex
	owned map[int]bool
}

func (s *switchableOwner) OwnsShard(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owned[n]
}

func (s *switchableOwner) set(m map[int]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owned = m
}

type fakeClusterLookup map[string]*v1alpha1.Cluster

func (f fakeClusterLookup) DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error) {
	if c, ok := f[dest.Server]; ok {
		return c, nil
	}
	return nil, errors.New("not found")
}

func TestStatusShardResolver(t *testing.T) {
	t.Parallel()
	assigned := clusterObj("1", "https://a")
	assigned.Status.AssignedShard = ptr.To[int64](3)
	assigned.Spec.Shard = ptr.To[int64](1)
	pinned := clusterObj("2", "https://p")
	pinned.Spec.Shard = ptr.To[int64](2)
	r := StatusShardResolver{Clusters: fakeClusterLookup{"https://a": assigned, "https://p": pinned, "https://n": clusterObj("3", "https://n")}}
	for server, want := range map[string]int{"https://a": 3, "https://p": 2, "https://n": 0, "https://missing": 0} {
		got, err := r.ShardForApplication(context.Background(), appObj("ns", "x", server))
		require.NoError(t, err)
		assert.Equal(t, want, got, server)
	}
	_, err := StatusShardResolver{}.ShardForApplication(context.Background(), appObj("ns", "x", "https://a"))
	require.Error(t, err)
}

func TestLeaseHolders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := newFakeLeaseAPI()
	clk := testingclock.NewFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	c := api.client(nil)
	_, err := c.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName(1)},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("patrol-controller-0"), LeaseDurationSeconds: ptr.To[int32](30),
			RenewTime: &metav1.MicroTime{Time: clk.Now()},
		},
	})
	require.NoError(t, err)
	h := NewLeaseHolders(c, time.Second, clk)
	got, err := h.ShardHolder(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "patrol-controller-0", got)
	got, err = h.ShardHolder(ctx, 2)
	require.NoError(t, err)
	assert.Empty(t, got, "no Lease")

	// Expired holders are not returned (after the cache entry ends).
	clk.Step(31 * time.Second)
	got, err = h.ShardHolder(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, got)

	down := &atomic.Bool{}
	down.Store(true)
	hd := NewLeaseHolders(api.client(down), 0, clk)
	_, err = hd.ShardHolder(ctx, 1)
	require.ErrorIs(t, err, errPartition)
	h.Invalidate(1)
}
