package cache

// hierarchySnapshot is the subgraph reachable from a set of start keys, captured under the
// cache read lock so that the caller's callback can run without locks.
type hierarchySnapshot struct {
	starts   []*Resource
	children map[ResourceKey][]*Resource
	ns       map[string]nsMap
}

// captureHierarchy collects every resource reachable from keys through owner relations.
// Namespace maps are captured as published (immutable) maps; call with the read lock held.
func (g *graph) captureHierarchy(keys []ResourceKey) *hierarchySnapshot {
	s := &hierarchySnapshot{children: map[ResourceKey][]*Resource{}, ns: map[string]nsMap{}}
	seen := keySet{}
	var queue []*Resource
	for _, k := range keys {
		r := g.get(k)
		if r == nil {
			continue
		}
		s.starts = append(s.starts, r)
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			queue = append(queue, r)
		}
	}
	for i := 0; i < len(queue); i++ {
		r := queue[i]
		k := r.Ref.ResourceKey
		if _, ok := s.ns[k.Namespace]; !ok {
			s.ns[k.Namespace] = g.ns[k.Namespace]
		}
		ch := g.children(r)
		if len(ch) == 0 {
			continue
		}
		s.children[k] = ch
		for _, c := range ch {
			ck := c.Ref.ResourceKey
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				queue = append(queue, c)
			}
		}
	}
	return s
}

// walk runs a depth-first pre-order traversal from each start resource. fn is called at
// most once per resource; returning false skips that resource's children. Owner cycles are
// harmless because visited resources are never revisited.
func (s *hierarchySnapshot) walk(fn func(r *Resource, namespaceResources map[ResourceKey]*Resource) bool) {
	called := keySet{}
	var stack []*Resource
	for _, start := range s.starts {
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			r := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			k := r.Ref.ResourceKey
			if _, ok := called[k]; ok {
				continue
			}
			called[k] = struct{}{}
			if !fn(r, s.ns[k.Namespace]) {
				continue
			}
			ch := s.children[k]
			for i := len(ch) - 1; i >= 0; i-- {
				if _, ok := called[ch[i].Ref.ResourceKey]; !ok {
					stack = append(stack, ch[i])
				}
			}
		}
	}
}
