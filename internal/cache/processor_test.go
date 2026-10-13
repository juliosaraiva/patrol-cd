package cache

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// newOfflineCluster returns a cache that is never synced, for driving the processor directly.
func newOfflineCluster(t *testing.T, interval time.Duration) *Cluster {
	t.Helper()
	c, err := New(closedPortConfig(t), Settings{EventProcessingInterval: interval}, WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

type changeLog struct {
	mu      sync.Mutex
	changes []string
	batches []int
}

func (l *changeLog) onUpdate(newRes, oldRes *Resource, ns map[ResourceKey]*Resource) {
	s := "update "
	switch {
	case oldRes == nil:
		s = "add "
	case newRes == nil:
		s = "delete "
	}
	r := newRes
	if r == nil {
		r = oldRes
	}
	_ = ns // post-batch snapshot; may already reflect later changes of the same batch
	l.mu.Lock()
	l.changes = append(l.changes, s+r.Ref.Name+"@"+r.ResourceVersion)
	l.mu.Unlock()
}

func (l *changeLog) onBatch(_ time.Duration, n int) {
	l.mu.Lock()
	l.batches = append(l.batches, n)
	l.mu.Unlock()
}

func (l *changeLog) get() ([]string, []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.changes...), append([]int(nil), l.batches...)
}

func TestProcessorAppliesInOrderAndDropsStaleItems(t *testing.T) {
	t.Parallel()
	c := newOfflineCluster(t, -1)
	log := &changeLog{}
	c.OnResourceUpdated(log.onUpdate)
	c.OnResourceUpdated(func(*Resource, *Resource, map[ResourceKey]*Resource) { panic("handler bug") })
	c.OnProcessEvents(log.onBatch)

	c.mu.Lock()
	w := c.registerAPI(APIResourceInfo{GroupKind: cmGK})
	c.mu.Unlock()
	ctx := context.Background()
	res := func(name, rv string) *Resource { return mkRes("v1", "ConfigMap", "ns", name, "uid-"+name, withRV(rv)) }

	require.NoError(t, c.enqueue(ctx, queueItem{kind: qReplace, token: w.token, gk: cmGK, items: []*Resource{res("a", "1"), res("b", "1")}}))
	require.NoError(t, c.enqueue(ctx, queueItem{kind: qEvent, token: w.token, gk: cmGK, eventType: watch.Modified, key: res("a", "2").ResourceKey(), res: res("a", "2")}))
	require.NoError(t, c.enqueue(ctx, queueItem{kind: qEvent, token: w.token + 99, gk: cmGK, eventType: watch.Added, key: res("stale", "1").ResourceKey(), res: res("stale", "1")}))
	require.NoError(t, c.enqueue(ctx, queueItem{kind: qEvent, token: w.token, gk: cmGK, eventType: watch.Deleted, key: res("b", "1").ResourceKey()}))
	require.NoError(t, c.enqueue(ctx, queueItem{kind: qEvent, token: w.token, gk: cmGK, eventType: watch.Deleted, key: res("never-existed", "1").ResourceKey()}))

	eventually(t, 5*time.Second, func() bool { ch, _ := log.get(); return len(ch) == 4 }, "changes dispatched")
	changes, _ := log.get()
	assert.Equal(t, []string{"add a@1", "add b@1", "update a@2", "delete b@1"}, changes)
	assert.Nil(t, cached(c, NewResourceKey("", "ConfigMap", "ns", "stale")), "items of a stale registration are dropped")
	assert.Equal(t, 1, c.GetClusterInfo().ResourcesCount)

	// Pending operations (from syncs and API refreshes) are applied before queued events.
	c.mu.Lock()
	c.pending = append(c.pending, queueItem{kind: qRemoveAPI, gk: cmGK})
	c.mu.Unlock()
	c.wakeProcessor()
	eventually(t, 5*time.Second, func() bool { return c.GetClusterInfo().ResourcesCount == 0 }, "API removal applied")
	changes, batches := log.get()
	assert.Equal(t, "delete a@2", changes[len(changes)-1])
	assert.NotEmpty(t, batches)
}

func TestProcessorBatchesWithinInterval(t *testing.T) {
	t.Parallel()
	c := newOfflineCluster(t, 200*time.Millisecond)
	log := &changeLog{}
	c.OnProcessEvents(log.onBatch)
	c.mu.Lock()
	w := c.registerAPI(APIResourceInfo{GroupKind: cmGK})
	c.mu.Unlock()
	for _, name := range []string{"a", "b", "c"} {
		r := mkRes("v1", "ConfigMap", "ns", name, "uid-"+name)
		require.NoError(t, c.enqueue(context.Background(), queueItem{kind: qEvent, token: w.token, gk: cmGK, eventType: watch.Added, key: r.ResourceKey(), res: r}))
	}
	eventually(t, 5*time.Second, func() bool { _, b := log.get(); return len(b) > 0 }, "batch processed")
	_, batches := log.get()
	assert.Equal(t, []int{3}, batches, "events arriving within the window are applied together")
}

func TestClusterSinkSkipsEndpointsUpdatesAndRefreshesOnCRDs(t *testing.T) {
	t.Parallel()
	c := newOfflineCluster(t, -1)
	c.mu.Lock()
	ep := c.registerAPI(APIResourceInfo{GroupKind: endpointsGK})
	c.mu.Unlock()
	var raws int
	var mu sync.Mutex
	c.OnEvent(func(watch.EventType, *unstructured.Unstructured) { mu.Lock(); raws++; mu.Unlock() })
	sink := &clusterSink{c: c, token: ep.token}
	target := watchTarget{api: APIResourceInfo{GroupKind: endpointsGK}}
	obj := mkObj("v1", "Endpoints", "ns", "svc", "u1")
	sink.raw(watch.Modified, obj)
	require.NoError(t, sink.event(context.Background(), target, watch.Modified, KeyOf(obj), newResource(obj, &Settings{}), obj))
	assert.Empty(t, c.queue, "Endpoints modifications are not applied")
	require.NoError(t, sink.event(context.Background(), target, watch.Added, KeyOf(obj), newResource(obj, &Settings{}), obj))
	eventually(t, 5*time.Second, func() bool { return cached(c, KeyOf(obj)) != nil }, "Endpoints additions are applied")
	mu.Lock()
	assert.Equal(t, 1, raws, "raw handlers still see every event")
	mu.Unlock()

	crdGK := schema.GroupKind{Group: groupAPIExtensions, Kind: kindCRD}
	crd := crdObject("w.example.com", "Widget", "widgets")
	require.NoError(t, sink.event(context.Background(), watchTarget{api: APIResourceInfo{GroupKind: crdGK}}, watch.Added, KeyOf(crd), nil, crd))
	// The refresh loop consumes the expectation (an unsynced cache has nothing to refresh).
	eventually(t, 5*time.Second, func() bool {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		return len(c.refreshExpect) == 0
	}, "refresh requested and processed")
}
