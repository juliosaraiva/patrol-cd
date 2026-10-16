package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var fooGR = schema.GroupResource{Group: "foo.example", Resource: "foos"}

func fooObj(apiVersion, name string) *unstructured.Unstructured {
	return mkObj(apiVersion, "Foo", "a", name, "uid-"+name)
}

func unavailable(c *Cluster, reason UnavailableReason) []UnavailableAPI {
	var out []UnavailableAPI
	for _, u := range c.GetClusterInfo().UnavailableAPIs {
		if u.Reason == reason {
			out = append(out, u)
		}
	}
	return out
}

// A registration stopped by a refresh must not leave its objects behind when the API cannot
// be listed again (404 while aggregated discovery still serves it). Before the fix the
// objects stayed cached, unwatched, until the next full resync.
func TestRefreshDropsObjectsOfStoppedAPIWhoseRelistFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fc := newFakeCluster(t, cmResource, fooV1)
	scope := fc.scope(fooV1.gvr(), "")
	scope.set(func(s *fakeScope) { s.items = []*unstructured.Unstructured{fooObj("foo.example/v1", "foo-1")} })
	c := fc.start(t, Settings{})
	fooKey := key("foo.example", "Foo", "a", "foo-1")
	waitCached(t, c, fooKey)

	// The watch saw 404 (registration dead), the server now answers 404, and discovery has
	// not caught up yet.
	c.mu.Lock()
	c.apis[fooGK].dead = true
	c.mu.Unlock()
	scope.set(func(s *fakeScope) { s.listErr = notFound(fooGR) })
	ok, err := c.refreshAPIs(ctx, map[schema.GroupKind]bool{fooGK: false})
	require.NoError(t, err)
	assert.False(t, ok, "discovery still serves the API: the refresh must be retried")
	waitApplied(t, c)
	assert.Nil(t, cached(c, fooKey), "objects of a stopped registration whose relist failed are removed")
	_, registered := registration(c, fooGK)
	assert.False(t, registered)

	// Discovery catches up.
	fc.setResources(cmResource)
	ok, err = c.refreshAPIs(ctx, map[schema.GroupKind]bool{fooGK: false})
	require.NoError(t, err)
	assert.True(t, ok)
	waitApplied(t, c)
	_, err = c.IsNamespaced(fooGK)
	require.ErrorIs(t, err, ErrUnknownGroupKind)
	assert.Empty(t, c.FindResources("", ResourceOfGroupKind(fooGK.Group, fooGK.Kind)))
	var visited []ResourceKey
	c.IterateHierarchy([]ResourceKey{fooKey}, func(r *Resource, _ map[ResourceKey]*Resource) bool {
		visited = append(visited, r.ResourceKey())
		return true
	})
	assert.Empty(t, visited)
	assert.Equal(t, 1, c.GetClusterInfo().APIsCount)
}

// End to end: a watch that gets 404 drops the API's objects at once (upstream semantics), and
// the refresh converges once discovery stops serving the API.
func TestGoneAPIObjectsRemovedWhileDiscoveryLags(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource, fooV1)
	scope := fc.scope(fooV1.gvr(), "")
	scope.set(func(s *fakeScope) { s.items = []*unstructured.Unstructured{fooObj("foo.example/v1", "foo-1")} })
	c := fc.start(t, Settings{})
	fooKey := key("foo.example", "Foo", "a", "foo-1")
	waitCached(t, c, fooKey)

	var mu sync.Mutex
	deletions := 0
	c.OnResourceUpdated(func(newRes, oldRes *Resource, _ map[ResourceKey]*Resource) {
		if newRes == nil && oldRes.ResourceKey() == fooKey {
			mu.Lock()
			deletions++
			mu.Unlock()
		}
	})

	scope.set(func(s *fakeScope) { s.listErr, s.watchErr = notFound(fooGR), notFound(fooGR) })
	scope.stopWatches() // the watcher reconnects and gets 404
	eventually(t, 10*time.Second, func() bool { return cached(c, fooKey) == nil }, "objects dropped when the watch reports 404")
	eventually(t, 10*time.Second, func() bool { _, ok := registration(c, fooGK); return !ok }, "registration stopped")
	ns, err := c.IsNamespaced(fooGK)
	require.NoError(t, err, "discovery still serves the kind")
	assert.True(t, ns)

	fc.setResources(cmResource)
	eventually(t, 20*time.Second, func() bool {
		_, err := c.IsNamespaced(fooGK)
		return err != nil
	}, "the retried refresh converges once discovery drops the API")
	assert.Nil(t, cached(c, fooKey))
	assert.Equal(t, 1, c.GetClusterInfo().APIsCount)
	mu.Lock()
	assert.Equal(t, 1, deletions, "the removal is notified once")
	mu.Unlock()
}

// A transient discovery failure of one group must not tear down that group's watches nor
// drop its objects; discovery data of the failed group is carried over.
func TestRefreshKeepsWatchesOfGroupsWhoseDiscoveryFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		resources []fakeResource
		watched   fakeResource
		failGV    schema.GroupVersion
	}{
		{name: "only version fails", resources: []fakeResource{cmResource, fooV1}, watched: fooV1, failGV: fooV1.gv},
		{name: "preferred version fails while another serves the kind", resources: []fakeResource{cmResource, fooV2, fooV1}, watched: fooV2, failGV: fooV2.gv},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fc := newFakeCluster(t, tc.resources...)
			fc.scope(tc.watched.gvr(), "").set(func(s *fakeScope) {
				s.items = []*unstructured.Unstructured{fooObj(tc.watched.gv.String(), "foo-1")}
			})
			c := fc.start(t, Settings{})
			fooKey := key("foo.example", "Foo", "a", "foo-1")
			waitCached(t, c, fooKey)
			before, ok := registration(c, fooGK)
			require.True(t, ok)
			require.Equal(t, tc.watched.gvr(), before.info.GroupVersionResource)

			fc.failDiscovery(tc.failGV, 503)
			satisfied, err := c.refreshAPIs(ctx, map[schema.GroupKind]bool{fooGK: true})
			require.NoError(t, err)
			assert.False(t, satisfied, "an expectation about a group whose discovery failed is not confirmed")
			waitApplied(t, c)

			after, ok := registration(c, fooGK)
			require.True(t, ok, "the watch is kept")
			assert.Equal(t, before.token, after.token, "the registration is not restarted")
			assert.Equal(t, tc.watched.gvr(), after.info.GroupVersionResource)
			assert.NotNil(t, cached(c, fooKey), "objects are kept")
			ns, err := c.IsNamespaced(fooGK)
			require.NoError(t, err, "discovery data of the failed group is carried over")
			assert.True(t, ns)
			var gvrs []schema.GroupVersionResource
			for _, a := range c.GetAPIResources() {
				gvrs = append(gvrs, a.GroupVersionResource)
			}
			assert.Contains(t, gvrs, tc.watched.gvr())
			failures := unavailable(c, ReasonDiscoveryFailed)
			require.Len(t, failures, 1, "the failure is reported")
			assert.Equal(t, "foo.example", failures[0].GroupKind.Group)
			assert.Equal(t, tc.failGV.Version, failures[0].Version)

			fc.failDiscovery(tc.failGV, 0)
			satisfied, err = c.refreshAPIs(ctx, map[schema.GroupKind]bool{fooGK: true})
			require.NoError(t, err)
			assert.True(t, satisfied)
			waitApplied(t, c)
			recovered, ok := registration(c, fooGK)
			require.True(t, ok)
			assert.Equal(t, before.token, recovered.token)
			assert.NotNil(t, cached(c, fooKey))
			assert.Empty(t, unavailable(c, ReasonDiscoveryFailed))
		})
	}
}

// Discovery that no longer serves a kind (without failures) still stops its watch and drops
// its objects.
func TestRefreshStopsAPIsNoLongerServed(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource, fooV1)
	fc.scope(fooV1.gvr(), "").set(func(s *fakeScope) { s.items = []*unstructured.Unstructured{fooObj("foo.example/v1", "foo-1")} })
	c := fc.start(t, Settings{})
	fooKey := key("foo.example", "Foo", "a", "foo-1")
	waitCached(t, c, fooKey)

	fc.setResources(cmResource)
	satisfied, err := c.refreshAPIs(context.Background(), map[schema.GroupKind]bool{fooGK: false})
	require.NoError(t, err)
	assert.True(t, satisfied)
	waitApplied(t, c)
	_, ok := registration(c, fooGK)
	assert.False(t, ok)
	assert.Nil(t, cached(c, fooKey))
}

func TestDiscoveryInheritFailed(t *testing.T) {
	t.Parallel()
	lw := []string{"list", "watch"}
	s := Settings{}
	gv := func(g, v string) string { return schema.GroupVersion{Group: g, Version: v}.String() }
	prev := buildDiscovery(nil, nil, &s, "")
	prev.namespaced = map[schema.GroupKind]bool{fooGK: true, {Group: "bar.example", Kind: "Bar"}: false}
	prev.byGVK = map[schema.GroupVersionKind]APIResourceInfo{
		{Group: "foo.example", Version: "v2", Kind: "Foo"}: {GroupKind: fooGK},
		{Group: "foo.example", Version: "v1", Kind: "Foo"}: {GroupKind: fooGK},
		{Group: "bar.example", Version: "v1", Kind: "Bar"}: {},
	}
	prev.all = []APIResourceInfo{
		apiInfo(fooV2.gv, metav1.APIResource{Name: "foos", Kind: "Foo", Namespaced: true, Verbs: lw}),
		apiInfo(fooV1.gv, metav1.APIResource{Name: "foos", Kind: "Foo", Namespaced: true, Verbs: lw}),
	}

	cur := buildDiscovery(nil, nil, &s, "")
	cur.failures = []UnavailableAPI{{GroupKind: schema.GroupKind{Group: "foo.example"}, Version: "v2", Reason: ReasonDiscoveryFailed}}
	cur.inheritFailed(prev)
	assert.True(t, cur.namespaced[fooGK], "kinds of a failed group keep their scope")
	assert.NotContains(t, cur.namespaced, schema.GroupKind{Group: "bar.example", Kind: "Bar"}, "healthy groups are not inherited")
	assert.Contains(t, cur.byGVK, schema.GroupVersionKind{Group: "foo.example", Version: "v2", Kind: "Foo"})
	assert.NotContains(t, cur.byGVK, schema.GroupVersionKind{Group: "foo.example", Version: "v1", Kind: "Foo"}, "only failed versions are inherited")
	require.Len(t, cur.all, 1)
	assert.Equal(t, gv("foo.example", "v2"), cur.all[0].GroupVersionResource.GroupVersion().String())

	noFailures := buildDiscovery(nil, nil, &s, "")
	noFailures.inheritFailed(prev)
	assert.Empty(t, noFailures.namespaced)
	noFailures.inheritFailed(nil) // nil previous snapshot is a no-op
}
