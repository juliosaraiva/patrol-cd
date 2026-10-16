package cache

import (
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type (
	nsMap  = map[ResourceKey]*Resource
	keySet = map[ResourceKey]struct{}
)

// graph is the cached object graph. It is not safe for concurrent use; Cluster guards it with
// its RWMutex.
//
// Namespace maps are copy-on-write: the first mutation of a namespace within a write
// section clones its map, and seal() ends the section. A map obtained while no write section
// is open is therefore never mutated afterwards and may be read without locks. All other
// fields are only accessed under the lock.
type graph struct {
	ns      map[string]nsMap
	mutable map[string]struct{}

	byGK    map[schema.GroupKind]keySet
	byUID   map[types.UID]keySet   // owner UID → children
	byOwner map[ResourceKey]keySet // owner key (UID-less references) → children

	stsParents map[string]nsMap  // namespace → resources with inferred children
	pvcs       map[string]keySet // namespace → PersistentVolumeClaim keys

	count int
}

// change describes one applied mutation for ResourceUpdated handlers.
type change struct {
	newRes, oldRes *Resource
	ns             nsMap
}

func newGraph() *graph {
	return &graph{
		ns:         map[string]nsMap{},
		mutable:    map[string]struct{}{},
		byGK:       map[schema.GroupKind]keySet{},
		byUID:      map[types.UID]keySet{},
		byOwner:    map[ResourceKey]keySet{},
		stsParents: map[string]nsMap{},
		pvcs:       map[string]keySet{},
	}
}

func (g *graph) get(k ResourceKey) *Resource { return g.ns[k.Namespace][k] }

// mutableNS returns the map of ns that may be mutated in the current write section, cloning
// a sealed map (or creating a new one) on first use.
func (g *graph) mutableNS(ns string) nsMap {
	m := g.ns[ns]
	if _, ok := g.mutable[ns]; ok {
		return m
	}
	clone := make(nsMap, len(m)+1)
	maps.Copy(clone, m)
	g.ns[ns] = clone
	g.mutable[ns] = struct{}{}
	return clone
}

// seal ends the current write section: every namespace map becomes immutable.
func (g *graph) seal() { clear(g.mutable) }

// upsert inserts or replaces r and returns the previous resource with the same key.
func (g *graph) upsert(r *Resource) *Resource {
	key := r.Ref.ResourceKey
	m := g.mutableNS(key.Namespace)
	old := m[key]
	if isPVCKey(key) {
		for _, p := range g.stsParents[key.Namespace] {
			if p.inferredChildren(key) {
				r = r.withInferredOwner(p.ownerRef(), true)
			}
		}
		addToSet(g.pvcs, key.Namespace, key)
	}
	if old != nil {
		g.unindexOwners(key, old)
	} else {
		g.count++
		addToSet(g.byGK, key.GroupKind(), key)
	}
	m[key] = r
	g.indexOwners(key, r)
	if old != nil && old.inferredChildren != nil && (r.inferredChildren == nil || old.Ref.UID != r.Ref.UID) {
		g.unlinkInferred(old)
	}
	if r.inferredChildren != nil {
		g.linkInferred(r)
	}
	return old
}

// remove deletes k and returns the removed resource, or nil.
func (g *graph) remove(k ResourceKey) *Resource {
	old := g.ns[k.Namespace][k]
	if old == nil {
		return nil
	}
	m := g.mutableNS(k.Namespace)
	delete(m, k)
	g.count--
	removeFromSet(g.byGK, k.GroupKind(), k)
	g.unindexOwners(k, old)
	if isPVCKey(k) {
		removeFromSet(g.pvcs, k.Namespace, k)
	}
	if old.inferredChildren != nil {
		g.unlinkInferred(old)
	}
	if len(m) == 0 {
		delete(g.ns, k.Namespace)
		delete(g.mutable, k.Namespace)
	}
	return old
}

// replaceScope makes the cached objects of gk (in namespace ns, or everywhere when ns is
// empty) equal to items. Objects whose resourceVersion is unchanged are kept.
func (g *graph) replaceScope(gk schema.GroupKind, ns string, items []*Resource) []change {
	var changes []change
	seen := make(keySet, len(items))
	for _, r := range items {
		k := r.Ref.ResourceKey
		seen[k] = struct{}{}
		old := g.get(k)
		if old != nil && old.ResourceVersion == r.ResourceVersion {
			continue
		}
		g.upsert(r)
		changes = append(changes, change{newRes: g.get(k), oldRes: old})
	}
	var stale []ResourceKey
	for k := range g.byGK[gk] {
		if ns != "" && k.Namespace != ns {
			continue
		}
		if _, ok := seen[k]; !ok {
			stale = append(stale, k)
		}
	}
	slices.SortFunc(stale, compareKeys)
	for _, k := range stale {
		if old := g.remove(k); old != nil {
			changes = append(changes, change{oldRes: old})
		}
	}
	return changes
}

// removeGK drops every object of gk.
func (g *graph) removeGK(gk schema.GroupKind) []change {
	keys := slices.SortedFunc(maps.Keys(g.byGK[gk]), compareKeys)
	changes := make([]change, 0, len(keys))
	for _, k := range keys {
		if old := g.remove(k); old != nil {
			changes = append(changes, change{oldRes: old})
		}
	}
	return changes
}

// children returns the direct children of p sorted by key. Children of a namespaced parent
// must live in its namespace; a cluster-scoped parent may own objects in any namespace.
// When several keys share a UID (the same object served by two API groups), the smallest
// key wins so the result is stable.
func (g *graph) children(p *Resource) []*Resource {
	pk := p.Ref.ResourceKey
	var keys []ResourceKey
	if p.Ref.UID != "" {
		for k := range g.byUID[p.Ref.UID] {
			keys = append(keys, k)
		}
	}
	for k := range g.byOwner[pk] {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, compareKeys)
	keys = slices.Compact(keys)
	out := make([]*Resource, 0, len(keys))
	seenUID := map[types.UID]struct{}{}
	for _, k := range keys {
		if k == pk || (pk.Namespace != "" && k.Namespace != pk.Namespace) {
			continue
		}
		c := g.get(k)
		if c == nil {
			continue
		}
		if c.Ref.UID != "" {
			if _, dup := seenUID[c.Ref.UID]; dup {
				continue
			}
			seenUID[c.Ref.UID] = struct{}{}
		}
		out = append(out, c)
	}
	return out
}

func (g *graph) indexOwners(child ResourceKey, r *Resource) {
	for i := range r.OwnerRefs {
		if uid := r.OwnerRefs[i].UID; uid != "" {
			addToSet(g.byUID, uid, child)
		} else if ok, ownerKey := ownerKeyOf(child, &r.OwnerRefs[i]); ok {
			addToSet(g.byOwner, ownerKey, child)
		}
	}
}

func (g *graph) unindexOwners(child ResourceKey, r *Resource) {
	for i := range r.OwnerRefs {
		if uid := r.OwnerRefs[i].UID; uid != "" {
			removeFromSet(g.byUID, uid, child)
		} else if ok, ownerKey := ownerKeyOf(child, &r.OwnerRefs[i]); ok {
			removeFromSet(g.byOwner, ownerKey, child)
		}
	}
}

// linkInferred registers p as an inferred parent and updates matching PVCs.
func (g *graph) linkInferred(p *Resource) {
	ns := p.Ref.Namespace
	if g.stsParents[ns] == nil {
		g.stsParents[ns] = nsMap{}
	}
	g.stsParents[ns][p.Ref.ResourceKey] = p
	g.updateClaims(ns, p, func(k ResourceKey) bool { return p.inferredChildren(k) })
}

// unlinkInferred removes p as an inferred parent and drops its inferred references.
func (g *graph) unlinkInferred(p *Resource) {
	ns := p.Ref.Namespace
	if cur := g.stsParents[ns][p.Ref.ResourceKey]; cur != nil && cur.Ref.UID == p.Ref.UID {
		delete(g.stsParents[ns], p.Ref.ResourceKey)
		if len(g.stsParents[ns]) == 0 {
			delete(g.stsParents, ns)
		}
	}
	g.updateClaims(ns, p, func(ResourceKey) bool { return false })
}

func (g *graph) updateClaims(ns string, p *Resource, want func(ResourceKey) bool) {
	if len(g.pvcs[ns]) == 0 {
		return
	}
	// Other registered parents sharing p's UID (the same object served under two API
	// groups) keep their claims.
	var others []*Resource
	for k, q := range g.stsParents[ns] {
		if k != p.Ref.ResourceKey && q.Ref.UID == p.Ref.UID {
			others = append(others, q)
		}
	}
	m := g.mutableNS(ns)
	ref := p.ownerRef()
	for k := range g.pvcs[ns] {
		pvc := m[k]
		if pvc == nil {
			continue
		}
		wanted := want(k)
		for _, q := range others {
			wanted = wanted || q.inferredChildren(k)
		}
		upd := pvc.withInferredOwner(ref, wanted)
		if upd != pvc {
			g.unindexOwners(k, pvc)
			m[k] = upd
			g.indexOwners(k, upd)
		}
	}
}

// diffGraphs returns the changes turning old into cur (resourceVersion based).
func diffGraphs(old, cur *graph) []change {
	var changes []change
	for _, m := range cur.ns {
		for k, r := range m {
			if o := old.get(k); o == nil || o.ResourceVersion != r.ResourceVersion {
				changes = append(changes, change{newRes: r, oldRes: o, ns: m})
			}
		}
	}
	for ns, m := range old.ns {
		for k, o := range m {
			if cur.get(k) == nil {
				changes = append(changes, change{oldRes: o, ns: cur.ns[ns]})
			}
		}
	}
	return changes
}

// ownerKeyOf returns the key of the owner referenced without UID, assumed to live in the
// child's namespace.
func ownerKeyOf(child ResourceKey, ref *metav1.OwnerReference) (bool, ResourceKey) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || ref.Kind == "" || ref.Name == "" {
		return false, ResourceKey{}
	}
	return true, ResourceKey{Group: gv.Group, Kind: ref.Kind, Namespace: child.Namespace, Name: ref.Name}
}

func compareKeys(a, b ResourceKey) int {
	if c := strings.Compare(a.Group, b.Group); c != 0 {
		return c
	}
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func addToSet[K comparable](m map[K]keySet, k K, v ResourceKey) {
	s := m[k]
	if s == nil {
		s = keySet{}
		m[k] = s
	}
	s[v] = struct{}{}
}

func removeFromSet[K comparable](m map[K]keySet, k K, v ResourceKey) {
	s := m[k]
	if s == nil {
		return
	}
	delete(s, v)
	if len(s) == 0 {
		delete(m, k)
	}
}
