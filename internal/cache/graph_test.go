package cache

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"
)

func stsObj(ns, name, uid string, templates ...string) *Resource {
	var tpl []any
	for _, t := range templates {
		tpl = append(tpl, map[string]any{"metadata": map[string]any{"name": t}})
	}
	opts := []objOpt{}
	if len(tpl) > 0 {
		opts = append(opts, withField(tpl, "spec", "volumeClaimTemplates"))
	}
	return mkRes("apps/v1", "StatefulSet", ns, name, uid, opts...)
}

func TestGraphCopyOnWrite(t *testing.T) {
	t.Parallel()
	g := newGraph()
	a := mkRes("v1", "ConfigMap", "ns", "a", "ua")
	g.upsert(a)
	g.seal()
	published := g.ns["ns"]
	snapshot := maps.Clone(published)

	g.upsert(mkRes("v1", "ConfigMap", "ns", "b", "ub"))
	g.remove(a.ResourceKey())
	g.seal()

	assert.Equal(t, snapshot, published, "a published namespace map must never change")
	assert.NotContains(t, g.ns["ns"], a.ResourceKey())
	assert.Contains(t, g.ns["ns"], NewResourceKey("", "ConfigMap", "ns", "b"))

	// Within one write section the map is cloned only once.
	g.upsert(mkRes("v1", "ConfigMap", "ns", "c", "uc"))
	first := g.ns["ns"]
	g.upsert(mkRes("v1", "ConfigMap", "ns", "d", "ud"))
	second := g.ns["ns"]
	assert.Equal(t, fmt.Sprintf("%p", first), fmt.Sprintf("%p", second))
	g.seal()

	g.remove(NewResourceKey("", "ConfigMap", "ns", "b"))
	g.remove(NewResourceKey("", "ConfigMap", "ns", "c"))
	g.remove(NewResourceKey("", "ConfigMap", "ns", "d"))
	g.seal()
	assert.NotContains(t, g.ns, "ns", "empty namespaces are dropped")
	assert.Zero(t, g.count)
}

func TestGraphReplaceScope(t *testing.T) {
	t.Parallel()
	g := newGraph()
	cmGK := schema.GroupKind{Kind: "ConfigMap"}
	g.upsert(mkRes("v1", "ConfigMap", "a", "keep", "u1", withRV("5")))
	g.upsert(mkRes("v1", "ConfigMap", "a", "update", "u2", withRV("5")))
	g.upsert(mkRes("v1", "ConfigMap", "a", "drop", "u3"))
	g.upsert(mkRes("v1", "ConfigMap", "b", "other-ns", "u4"))
	g.upsert(mkRes("v1", "Secret", "a", "other-kind", "u5"))
	g.seal()
	keep := g.get(NewResourceKey("", "ConfigMap", "a", "keep"))

	changes := g.replaceScope(cmGK, "a", []*Resource{
		mkRes("v1", "ConfigMap", "a", "keep", "u1", withRV("5")),
		mkRes("v1", "ConfigMap", "a", "update", "u2", withRV("6")),
		mkRes("v1", "ConfigMap", "a", "new", "u6"),
	})
	g.seal()

	type summary struct{ newKey, oldKey string }
	var got []summary
	for _, ch := range changes {
		var s summary
		if ch.newRes != nil {
			s.newKey = ch.newRes.ResourceKey().Name
		}
		if ch.oldRes != nil {
			s.oldKey = ch.oldRes.ResourceKey().Name
		}
		got = append(got, s)
	}
	assert.ElementsMatch(t, []summary{{"update", "update"}, {"new", ""}, {"", "drop"}}, got)
	assert.Same(t, keep, g.get(keep.ResourceKey()), "unchanged resourceVersion keeps the existing resource")
	assert.NotNil(t, g.get(NewResourceKey("", "ConfigMap", "b", "other-ns")), "other namespaces are untouched")
	assert.NotNil(t, g.get(NewResourceKey("", "Secret", "a", "other-kind")), "other kinds are untouched")

	changes = g.replaceScope(cmGK, "", nil)
	g.seal()
	assert.Len(t, changes, 4)
	assert.Equal(t, 1, g.count)

	g.upsert(mkRes("v1", "ConfigMap", "a", "x", "u7"))
	assert.Len(t, g.removeGK(cmGK), 1)
	assert.Equal(t, 1, g.count)
}

func TestGraphStatefulSetInference(t *testing.T) {
	t.Parallel()
	pvcKey := NewResourceKey("", "PersistentVolumeClaim", "ns", "data-web-0")
	otherKey := NewResourceKey("", "PersistentVolumeClaim", "ns", "unrelated")

	t.Run("pvc after statefulset", func(t *testing.T) {
		t.Parallel()
		g := newGraph()
		g.upsert(stsObj("ns", "web", "s1", "data"))
		g.upsert(mkRes("v1", "PersistentVolumeClaim", "ns", "data-web-0", "p1"))
		g.upsert(mkRes("v1", "PersistentVolumeClaim", "ns", "unrelated", "p2"))
		g.seal()
		require.Len(t, g.get(pvcKey).OwnerRefs, 1)
		assert.Equal(t, types.UID("s1"), g.get(pvcKey).OwnerRefs[0].UID)
		assert.Empty(t, g.get(otherKey).OwnerRefs)
		assert.Equal(t, []ResourceKey{pvcKey}, childKeys(g, NewResourceKey("apps", "StatefulSet", "ns", "web")))
	})

	t.Run("statefulset after pvc, delete and recreate", func(t *testing.T) {
		t.Parallel()
		g := newGraph()
		g.upsert(mkRes("v1", "PersistentVolumeClaim", "ns", "data-web-0", "p1"))
		g.seal()
		before := g.ns["ns"]
		g.upsert(stsObj("ns", "web", "s1", "data"))
		g.seal()
		assert.Empty(t, before[pvcKey].OwnerRefs, "published PVC must not be mutated")
		require.Len(t, g.get(pvcKey).OwnerRefs, 1)

		// A status update of the StatefulSet keeps the reference.
		g.upsert(stsObj("ns", "web", "s1", "data"))
		g.seal()
		require.Len(t, g.get(pvcKey).OwnerRefs, 1)

		g.remove(NewResourceKey("apps", "StatefulSet", "ns", "web"))
		g.seal()
		assert.Empty(t, g.get(pvcKey).OwnerRefs)

		g.upsert(stsObj("ns", "web", "s2", "data"))
		g.seal()
		require.Len(t, g.get(pvcKey).OwnerRefs, 1)
		assert.Equal(t, types.UID("s2"), g.get(pvcKey).OwnerRefs[0].UID)

		// Templates removed: the reference goes away.
		g.upsert(stsObj("ns", "web", "s2"))
		g.seal()
		assert.Empty(t, g.get(pvcKey).OwnerRefs)
		checkGraph(t, g)
	})
}

func childKeys(g *graph, parent ResourceKey) []ResourceKey {
	var out []ResourceKey
	for _, c := range g.children(g.get(parent)) {
		out = append(out, c.ResourceKey())
	}
	return out
}

// fataler is the subset of testing.TB implemented by both *testing.T and *rapid.T.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// checkGraph recomputes every index from the namespace maps and compares.
func checkGraph(t fataler, g *graph) {
	t.Helper()
	count := 0
	byGK := map[schema.GroupKind]keySet{}
	byUID := map[types.UID]keySet{}
	byOwner := map[ResourceKey]keySet{}
	pvcs := map[string]keySet{}
	sts := map[string]nsMap{}
	for ns, m := range g.ns {
		if len(m) == 0 {
			t.Fatalf("empty namespace map %q retained", ns)
		}
		for k, r := range m {
			if k != r.ResourceKey() || k.Namespace != ns {
				t.Fatalf("resource %s stored under key %s in namespace %q", r.ResourceKey(), k, ns)
			}
			count++
			addToSet(byGK, k.GroupKind(), k)
			for i := range r.OwnerRefs {
				if uid := r.OwnerRefs[i].UID; uid != "" {
					addToSet(byUID, uid, k)
				} else if ok, ok2 := ownerKeyOf(k, &r.OwnerRefs[i]); ok {
					addToSet(byOwner, ok2, k)
				}
			}
			if isPVCKey(k) {
				addToSet(pvcs, ns, k)
			}
			if r.inferredChildren != nil {
				if sts[ns] == nil {
					sts[ns] = nsMap{}
				}
				sts[ns][k] = r
			}
		}
	}
	if count != g.count {
		t.Fatalf("count %d, recomputed %d", g.count, count)
	}
	for name, pair := range map[string][2]any{
		"byGK": {byGK, g.byGK}, "byUID": {byUID, g.byUID}, "byOwner": {byOwner, g.byOwner},
		"pvcs": {pvcs, g.pvcs}, "stsParents": {sts, g.stsParents},
	} {
		if !assert.ObjectsAreEqual(pair[0], pair[1]) {
			t.Fatalf("index %s mismatch:\nwant %v\ngot  %v", name, pair[0], pair[1])
		}
	}
	// Inferred references match the StatefulSets present.
	for ns, set := range g.pvcs {
		for k := range set {
			pvc := g.ns[ns][k]
			for _, p := range g.stsParents[ns] {
				want := p.inferredChildren(k)
				has := hasOwnerUID(pvc.OwnerRefs, p.Ref.UID)
				if want && !has {
					t.Fatalf("PVC %s lacks inferred owner %s", k, p.ResourceKey())
				}
			}
			for _, uid := range pvc.inferredUIDs {
				found := false
				for _, p := range g.stsParents[ns] {
					if p.Ref.UID == uid && p.inferredChildren(k) {
						found = true
					}
				}
				if !found {
					t.Fatalf("PVC %s keeps stale inferred owner %s", k, uid)
				}
			}
		}
	}
}

// --- property tests -------------------------------------------------------------------------

type kindSpec struct {
	apiVersion, kind string
	namespaced       bool
}

var propKinds = []kindSpec{
	{"v1", "ConfigMap", true},
	{"v1", "PersistentVolumeClaim", true},
	{"apps/v1", "StatefulSet", true},
	{"apps/v1", "Deployment", true},
	{"v1", "Endpoints", true},
	{"v1", "Namespace", false},
	{"rbac.authorization.k8s.io/v1", "ClusterRole", false},
}

var (
	propNamespaces = []string{"a", "b"}
	propNames      = []string{"web", "x", "data-web-0", "data-web-1", "logs-web-0"}
)

func drawResource(t *rapid.T, uniqueUIDs bool) *Resource {
	ks := rapid.SampledFrom(propKinds).Draw(t, "kind")
	ns := ""
	if ks.namespaced {
		ns = rapid.SampledFrom(propNamespaces).Draw(t, "ns")
	}
	name := rapid.SampledFrom(propNames).Draw(t, "name")
	key := NewResourceKey(schema.FromAPIVersionAndKind(ks.apiVersion, ks.kind).Group, ks.kind, ns, name)
	uid := "uid-" + key.String()
	if !uniqueUIDs {
		uid = rapid.SampledFrom([]string{uid, uid, "shared-1", "shared-2"}).Draw(t, "uid")
	}
	var opts []objOpt
	opts = append(opts, withRV(fmt.Sprint(rapid.IntRange(1, 3).Draw(t, "rv"))))
	nOwners := rapid.IntRange(0, 2).Draw(t, "owners")
	for i := range nOwners {
		oks := rapid.SampledFrom(propKinds).Draw(t, fmt.Sprintf("ownerKind%d", i))
		oname := rapid.SampledFrom(propNames).Draw(t, fmt.Sprintf("ownerName%d", i))
		ons := ""
		if oks.namespaced {
			ons = ns
		}
		okey := NewResourceKey(schema.FromAPIVersionAndKind(oks.apiVersion, oks.kind).Group, oks.kind, ons, oname)
		ouid := "uid-" + okey.String()
		if rapid.Bool().Draw(t, fmt.Sprintf("ownerNoUID%d", i)) {
			ouid = ""
		}
		opts = append(opts, withOwner(oks.apiVersion, oks.kind, oname, ouid))
	}
	if ks.kind == "StatefulSet" {
		tpls := rapid.SliceOfDistinct(rapid.SampledFrom([]string{"data", "logs"}), rapid.ID[string]).Draw(t, "templates")
		var tpl []any
		for _, n := range tpls {
			tpl = append(tpl, map[string]any{"metadata": map[string]any{"name": n}})
		}
		if len(tpl) > 0 {
			opts = append(opts, withField(tpl, "spec", "volumeClaimTemplates"))
		}
	}
	return newResource(mkObj(ks.apiVersion, ks.kind, ns, name, uid, opts...), &Settings{})
}

func TestGraphInvariantsProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		g := newGraph()
		var published []nsMap
		var snapshots []nsMap
		steps := rapid.IntRange(1, 40).Draw(t, "steps")
		for i := range steps {
			// Record every published map before a write section.
			for _, m := range g.ns {
				published = append(published, m)
				snapshots = append(snapshots, maps.Clone(m))
			}
			ops := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("ops%d", i))
			for range ops {
				switch rapid.IntRange(0, 3).Draw(t, "op") {
				case 0, 1:
					g.upsert(drawResource(t, false))
				case 2:
					r := drawResource(t, false)
					g.remove(r.ResourceKey())
				case 3:
					ks := rapid.SampledFrom(propKinds).Draw(t, "replaceKind")
					gk := schema.FromAPIVersionAndKind(ks.apiVersion, ks.kind).GroupKind()
					ns := ""
					if ks.namespaced && rapid.Bool().Draw(t, "scoped") {
						ns = rapid.SampledFrom(propNamespaces).Draw(t, "replaceNS")
					}
					var items []*Resource
					seen := keySet{}
					for range rapid.IntRange(0, 3).Draw(t, "items") {
						r := drawResource(t, false)
						if r.Ref.GroupKind() != gk || (ns != "" && r.Ref.Namespace != ns) {
							continue
						}
						if _, dup := seen[r.ResourceKey()]; dup {
							continue
						}
						seen[r.ResourceKey()] = struct{}{}
						items = append(items, r)
					}
					g.replaceScope(gk, ns, items)
				}
			}
			g.seal()
			checkGraph(t, g)
			for j := range published {
				if !maps.Equal(published[j], snapshots[j]) {
					t.Fatalf("published namespace map mutated after step %d", i)
				}
			}
		}
	})
}

// naiveReachable computes descendants by scanning every resource's owner references.
func naiveReachable(g *graph, starts []ResourceKey) keySet {
	all := map[ResourceKey]*Resource{}
	for _, m := range g.ns {
		maps.Copy(all, m)
	}
	isChild := func(p, c *Resource) bool {
		pk, ck := p.ResourceKey(), c.ResourceKey()
		if pk == ck || (pk.Namespace != "" && pk.Namespace != ck.Namespace) {
			return false
		}
		for i := range c.OwnerRefs {
			ref := &c.OwnerRefs[i]
			if ref.UID != "" && ref.UID == p.Ref.UID {
				return true
			}
			if ok, ok2 := ownerKeyOf(ck, ref); ref.UID == "" && ok && ok2 == pk {
				return true
			}
		}
		return false
	}
	out := keySet{}
	var queue []*Resource
	for _, k := range starts {
		if r := all[k]; r != nil {
			if _, ok := out[k]; !ok {
				out[k] = struct{}{}
				queue = append(queue, r)
			}
		}
	}
	for i := 0; i < len(queue); i++ {
		p := queue[i]
		for _, k := range slices.SortedFunc(maps.Keys(all), compareKeys) {
			c := all[k]
			if _, ok := out[k]; !ok && isChild(p, c) {
				out[k] = struct{}{}
				queue = append(queue, c)
			}
		}
	}
	return out
}

func TestHierarchyProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		g := newGraph()
		for range rapid.IntRange(0, 30).Draw(t, "n") {
			g.upsert(drawResource(t, true))
		}
		g.seal()
		var starts []ResourceKey
		for range rapid.IntRange(0, 4).Draw(t, "starts") {
			r := drawResource(t, true)
			starts = append(starts, r.ResourceKey())
		}
		snap := g.captureHierarchy(starts)
		visited := keySet{}
		snap.walk(func(r *Resource, ns map[ResourceKey]*Resource) bool {
			k := r.ResourceKey()
			if _, dup := visited[k]; dup {
				t.Fatalf("visited %s twice", k)
			}
			if ns[k] != r {
				t.Fatalf("namespace map of %s does not contain it", k)
			}
			visited[k] = struct{}{}
			return true
		})
		want := naiveReachable(g, starts)
		if !maps.Equal(want, visited) {
			t.Fatalf("visited %v, want %v", slices.Collect(maps.Keys(visited)), slices.Collect(maps.Keys(want)))
		}

		// Pruning: refusing every resource visits exactly the start keys present.
		pruned := keySet{}
		snap.walk(func(r *Resource, _ map[ResourceKey]*Resource) bool {
			pruned[r.ResourceKey()] = struct{}{}
			return false
		})
		for k := range pruned {
			if !slices.Contains(starts, k) {
				t.Fatalf("pruned walk visited non-start %s", k)
			}
		}
	})
}
