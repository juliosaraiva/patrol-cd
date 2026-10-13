package cache

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// maxConcurrentGets bounds parallel GET requests issued by GetManagedLiveObjs.
const maxConcurrentGets = 8

// GetManagedLiveObjs implements ClusterCache.
//
// The result contains a deep copy of every top-level cached object with a retained manifest
// for which isManaged returns true, plus, for every target:
//
//   - the cached manifest when it is retained and in the target's API version;
//   - otherwise a GET at the target's group/version/kind when the object is cached without
//     manifest, cached in another version, or of a kind the cache does not watch (excluded,
//     forbidden or not yet discovered);
//   - nothing when the kind is watched and the object is not cached, or the GET returns 404.
//
// With Namespaces set, a target outside them (or cluster-scoped without ClusterResources)
// fails with ErrOutOfScope.
func (c *Cluster) GetManagedLiveObjs(ctx context.Context, targets []*unstructured.Unstructured, isManaged func(*Resource) bool) (map[ResourceKey]*unstructured.Unstructured, error) {
	c.mu.RLock()
	settings, disc, cl, clErr := c.settings, c.disc, c.clients, c.clientsErr
	nsIdx := make(map[string]nsMap, len(c.graph.ns))
	for ns, m := range c.graph.ns {
		nsIdx[ns] = m
	}
	watched := make(map[schema.GroupKind]bool, len(c.apis))
	for gk := range c.apis {
		watched[gk] = true
	}
	c.mu.RUnlock()

	if clErr != nil {
		return nil, clErr
	}
	if disc == nil {
		return nil, ErrNotSynced
	}
	for _, t := range targets {
		if t == nil || settings.managesNamespace(t.GetNamespace()) {
			continue
		}
		return nil, &OutOfScopeError{Kind: t.GetKind(), Name: t.GetName(), Namespace: t.GetNamespace()}
	}

	result := map[ResourceKey]*unstructured.Unstructured{}
	if isManaged != nil {
		for _, m := range nsIdx {
			for k, r := range m {
				if r.Resource != nil && len(r.OwnerRefs) == 0 && isManaged(r) {
					result[k] = r.liveCopy()
				}
			}
		}
	}

	type fetch struct {
		key       ResourceKey
		gvk       schema.GroupVersionKind
		name, ns  string
		fetched   *unstructured.Unstructured
		hasResult bool
	}
	var fetches []*fetch
	for _, t := range targets {
		if t == nil {
			continue
		}
		key := KeyOf(t)
		gvk := t.GroupVersionKind()
		f := &fetch{key: key, gvk: gvk, name: t.GetName(), ns: t.GetNamespace()}
		if obj := result[key]; obj != nil {
			if sameGroupVersion(obj, gvk) {
				continue
			}
			fetches = append(fetches, f)
			continue
		}
		cached := nsIdx[key.Namespace][key]
		switch {
		case cached != nil && cached.Resource != nil && sameGroupVersion(cached.Resource, gvk):
			result[key] = cached.liveCopy()
		case cached != nil, !watched[key.GroupKind()]:
			fetches = append(fetches, f)
		}
	}
	if len(fetches) == 0 {
		return result, nil
	}

	var resMu sync.Mutex
	discovered := map[schema.GroupVersion]map[string]APIResourceInfo{}
	resolve := func(gvk schema.GroupVersionKind) (APIResourceInfo, bool, error) {
		if info, ok := disc.byGVK[gvk]; ok {
			return info, true, nil
		}
		resMu.Lock()
		byKind, ok := discovered[gvk.GroupVersion()]
		resMu.Unlock()
		if !ok {
			list, err := cl.disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
			if err != nil && !apierrors.IsNotFound(err) {
				return APIResourceInfo{}, false, fmt.Errorf("discover %s: %w", gvk.GroupVersion(), err)
			}
			byKind = map[string]APIResourceInfo{}
			if list != nil {
				for _, r := range list.APIResources {
					if topLevel(&r, gvk.GroupVersion()) {
						byKind[r.Kind] = apiInfo(gvk.GroupVersion(), r)
					}
				}
			}
			resMu.Lock()
			discovered[gvk.GroupVersion()] = byKind
			resMu.Unlock()
		}
		info, ok := byKind[gvk.Kind]
		return info, ok, nil
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentGets)
	for _, f := range fetches {
		g.Go(func() error {
			info, ok, err := resolve(f.gvk)
			if err != nil || !ok {
				return err // unknown kinds are treated as absent
			}
			ri := cl.dyn.Resource(info.GroupVersionResource)
			var obj *unstructured.Unstructured
			if info.Meta.Namespaced {
				obj, err = ri.Namespace(f.ns).Get(gctx, f.name, metav1.GetOptions{})
			} else {
				obj, err = ri.Get(gctx, f.name, metav1.GetOptions{})
			}
			switch {
			case apierrors.IsNotFound(err):
				return nil
			case err != nil:
				return fmt.Errorf("get %s %s/%s: %w", f.gvk.String(), f.ns, f.name, err)
			}
			f.fetched, f.hasResult = obj, true
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("get managed live objects: %w", err)
	}
	for _, f := range fetches {
		if f.hasResult {
			result[f.key] = f.fetched
		}
	}
	return result, nil
}

func sameGroupVersion(obj *unstructured.Unstructured, gvk schema.GroupVersionKind) bool {
	return obj.GroupVersionKind().GroupVersion() == gvk.GroupVersion()
}
