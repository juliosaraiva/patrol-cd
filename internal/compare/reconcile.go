package compare

import (
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/internal/cache"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// reconciliation pairs desired and live objects (upstream gitops-engine sync.Reconcile):
// Target[i] and Live[i] describe one resource; either may be nil.
type reconciliation struct {
	Target []*unstructured.Unstructured
	Live   []*unstructured.Unstructured
	Hooks  []*unstructured.Unstructured
}

// splitHooks separates sync hooks from regular targets and drops hooks without a valid type.
func splitHooks(targets []*unstructured.Unstructured) (objs, hooks []*unstructured.Unstructured) {
	for _, obj := range targets {
		if obj == nil || syncpkg.IsIgnored(obj) {
			continue
		}
		if syncpkg.IsHook(obj) {
			hooks = append(hooks, obj)
		} else {
			objs = append(objs, obj)
		}
	}
	return objs, hooks
}

// dedupLiveResources removes live objects that share a UID with another live object (the same
// object served by two API groups), keeping those that have a target; when none has one, the
// object with the smallest key is kept (upstream keeps an arbitrary one).
func dedupLiveResources(targets []*unstructured.Unstructured, live map[ResourceKey]*unstructured.Unstructured) {
	targetKeys := map[ResourceKey]bool{}
	for _, t := range targets {
		targetKeys[cache.KeyOf(t)] = true
	}
	byUID := map[types.UID][]ResourceKey{}
	for k, obj := range live {
		if obj != nil && obj.GetUID() != "" {
			byUID[obj.GetUID()] = append(byUID[obj.GetUID()], k)
		}
	}
	for _, keys := range byUID {
		if len(keys) < 2 {
			continue
		}
		slices.SortFunc(keys, func(a, b ResourceKey) int { return strings.Compare(a.String(), b.String()) })
		left := len(keys)
		for _, k := range keys {
			if left == 1 {
				break
			}
			if !targetKeys[k] {
				delete(live, k)
				left--
			}
		}
	}
}

// reconcile pairs targets with live objects by key. Kinds of unknown scope are looked up both
// as namespaced and as cluster-scoped. Unpaired live objects follow the targets, sorted by key.
// live is consumed.
func reconcile(targets []*unstructured.Unstructured, live map[ResourceKey]*unstructured.Unstructured, namespace string, isNamespaced scopeFunc) reconciliation {
	objs, hooks := splitHooks(targets)
	dedupLiveResources(objs, live)
	pairedLive := make([]*unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		gvk := obj.GroupVersionKind()
		ns := firstNonEmpty(obj.GetNamespace(), namespace)
		namespaced, err := isNamespaced(gvk.GroupKind())
		unknown := err != nil
		var keys []ResourceKey
		if namespaced || unknown {
			keys = append(keys, ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: ns, Name: obj.GetName()})
		}
		if !namespaced || unknown {
			keys = append(keys, ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Name: obj.GetName()})
		}
		for _, k := range keys {
			if l, ok := live[k]; ok {
				pairedLive[i] = l
				delete(live, k)
				break
			}
		}
	}
	rest := slices.SortedFunc(maps.Keys(live), func(a, b ResourceKey) int { return strings.Compare(a.String(), b.String()) })
	for _, k := range rest {
		objs = append(objs, nil)
		pairedLive = append(pairedLive, live[k])
	}
	return reconciliation{Target: objs, Live: pairedLive, Hooks: hooks}
}
