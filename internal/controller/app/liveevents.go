package app

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// OnClusterCacheCreated registers the live-state change handler on a new cluster cache. Pass
// it as cache.ManagerOptions.OnCacheCreated.
func (r *Reconciler) OnClusterCacheCreated(_ string, cc cache.ClusterCache) {
	cc.OnResourceUpdated(r.onResourceUpdated)
}

// onResourceUpdated maps a live object change to the Application tracking it (directly or
// through the ownership hierarchy) and requests a refresh at level recent when the changed
// object is a managed (root) resource (APP-085). Changes of descendants only affect the
// resource tree and request a tree-only refresh. Updates that
// change neither health nor the manifest under ignoreResourceUpdates rules are ignored
// (APP-110), and so are changes of the Application object itself (self-managed apps).
func (r *Reconciler) onResourceUpdated(newRes, oldRes *cache.Resource, nsResources map[cache.ResourceKey]*cache.Resource) {
	res := newRes
	if res == nil {
		res = oldRes
	}
	if res == nil {
		return
	}
	info := compare.InfoOf(res)
	if info.AppName == "" {
		// A descendant of a managed resource (found through the ownership hierarchy) only
		// changes the resource tree: tree-only refresh (upstream comparison level "nothing").
		if owner := compare.AppOf(res, nsResources); owner != "" {
			name, namespace := tracking.ParseInstanceName(owner, r.cfg.Namespace)
			if r.cfg.Namespaces().Allowed(namespace) {
				r.refresh.RequestTreeRefresh(types.NamespacedName{Namespace: namespace, Name: name})
			}
		}
		return
	}
	if newRes != nil && oldRes != nil {
		if st := r.cfg.Settings.Current(); st != nil && st.IgnoreResourceUpdatesEnabled() && compare.SkipResourceUpdate(compare.InfoOf(oldRes), info) {
			return
		}
	}
	name, namespace := tracking.ParseInstanceName(info.AppName, r.cfg.Namespace)
	key := res.ResourceKey()
	if key.Group == v1alpha1.GroupName && key.Kind == "Application" && key.Name == name && key.Namespace == namespace {
		return
	}
	if !r.cfg.Namespaces().Allowed(namespace) {
		return
	}
	r.refresh.RequestRefresh(types.NamespacedName{Namespace: namespace, Name: name}, core.CompareRecent, 0)
}
