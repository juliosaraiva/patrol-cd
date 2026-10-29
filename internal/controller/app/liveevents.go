package app

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// orphanLookupTimeout bounds the lookup of the Applications monitoring orphaned resources,
// which runs on the event goroutine of a cluster cache.
const orphanLookupTimeout = 5 * time.Second

// OnClusterCacheCreated registers the live-state change handler on a new cluster cache. Pass
// it as cache.ManagerOptions.OnCacheCreated; clusterKey is the cache key (compare.ClusterKey,
// the normalized server URL).
func (r *Reconciler) OnClusterCacheCreated(clusterKey string, cc cache.ClusterCache) {
	cc.OnResourceUpdated(func(newRes, oldRes *cache.Resource, nsResources map[cache.ResourceKey]*cache.Resource) {
		r.onResourceUpdated(clusterKey, newRes, oldRes, nsResources)
	})
}

// onResourceUpdated turns a live object change into Application refresh requests (upstream
// live state cache OnResourceUpdated and handleObjectUpdated; APP-085, APP-110, DIFF-033,
// DIFF-036, DIFF-037, DIFF-087), in order:
//
//  1. An update (never a create or a delete) that changes neither the health status nor the
//     manifest hashed under the ignoreResourceUpdates rules is dropped before attribution, so
//     the rules also cover untracked descendants that carry the ignore-resource-updates
//     annotation (the only untracked objects the populator hashes).
//  2. The Applications of the old and of the new object are collected: a tracked root's own
//     tracking, else the owner chain within the namespace (compare.AppOf). An object that moved
//     from one Application to another refreshes both. Core Endpoints count for none.
//  3. An Application whose root (managed) resource changed is compared at level recent (logged
//     at info); one whose descendant changed gets a tree-only refresh (logged at debug). The
//     changed Application object itself and Applications outside the allowed namespaces are
//     skipped.
//  4. A namespaced object attributed to no Application refreshes the tree of the Applications
//     that monitor orphaned resources in its namespace on this cluster (refreshOrphanMonitors).
//
// Shard, skip-reconcile and project checks of the attributed Applications happen in Reconcile.
func (r *Reconciler) onResourceUpdated(clusterKey string, newRes, oldRes *cache.Resource, nsResources map[cache.ResourceKey]*cache.Resource) {
	changed := newRes
	if changed == nil {
		changed = oldRes
	}
	if changed == nil {
		return
	}
	ref := changed.ResourceKey()
	ctx := context.Background()
	if newRes != nil && oldRes != nil {
		if st := r.cfg.Settings.Current(); st != nil && st.IgnoreResourceUpdatesEnabled() && compare.SkipResourceUpdate(compare.InfoOf(oldRes), compare.InfoOf(newRes)) {
			if r.log.Enabled(ctx, slog.LevelDebug) {
				r.log.Debug("ignoring change of object because none of the watched resource fields have changed", objectAttrs(clusterKey, ref)...)
			}
			return
		}
	}
	// Instance name -> whether the changed object is that Application's root (managed) node.
	apps := map[string]bool{}
	for _, res := range []*cache.Resource{newRes, oldRes} {
		if res == nil || skipAppRequeuing(res.ResourceKey()) {
			continue
		}
		inst := compare.AppOf(res, nsResources)
		if inst == "" {
			continue
		}
		apps[inst] = apps[inst] || isRootAppNode(res)
	}
	if len(apps) == 0 {
		if ref.Namespace != "" {
			ctx, cancel := context.WithTimeout(ctx, orphanLookupTimeout)
			defer cancel()
			r.refreshOrphanMonitors(ctx, clusterKey, ref)
		}
		return
	}
	for inst, root := range apps {
		name, namespace := tracking.ParseInstanceName(inst, r.cfg.Namespace)
		key := types.NamespacedName{Namespace: namespace, Name: name}
		if isSelfReferencedApp(ref, key) || !r.cfg.Namespaces().Allowed(namespace) {
			continue
		}
		r.requestLiveRefresh(ctx, key, root, clusterKey, ref)
	}
}

// refreshOrphanMonitors requests a tree-only refresh of the Applications that monitor orphaned
// resources (their project sets spec.orphanedResources) in the namespace of changed, an
// object attributed to no Application, on the cluster of clusterKey (upstream
// handleObjectUpdated with the orphaned index). Applications outside the allowed namespaces,
// of another shard, or that are the changed object are skipped. It reads the manager's
// informer cache and the cluster registry only: it runs on the cluster cache's event
// goroutine.
func (r *Reconciler) refreshOrphanMonitors(ctx context.Context, clusterKey string, changed cache.ResourceKey) {
	var projects v1alpha1.AppProjectList
	if err := r.c.List(ctx, &projects, client.InNamespace(r.cfg.Namespace),
		client.MatchingFields{indexOrphanedResources: orphanMonitoringKey}, client.UnsafeDisableDeepCopy); err != nil {
		r.log.Warn("cannot list projects monitoring orphaned resources", "error", err)
		return
	}
	if len(projects.Items) == 0 {
		return
	}
	monitoring := make(map[string]bool, len(projects.Items))
	for i := range projects.Items {
		monitoring[projects.Items[i].Name] = true
	}
	// The listed objects are shared with the informer cache: read only.
	var apps v1alpha1.ApplicationList
	if err := r.c.List(ctx, &apps, client.MatchingFields{indexDestinationNamespace: changed.Namespace}, client.UnsafeDisableDeepCopy); err != nil {
		r.log.Warn("cannot list applications monitoring orphaned resources", "namespace", changed.Namespace, "error", err)
		return
	}
	st := r.cfg.Settings.Current()
	for i := range apps.Items {
		app := &apps.Items[i]
		key := client.ObjectKeyFromObject(app)
		if !monitoring[app.Spec.GetProject()] || isSelfReferencedApp(changed, key) || !r.cfg.Namespaces().Allowed(key.Namespace) {
			continue
		}
		cluster, err := r.cfg.DestinationCluster(ctx, app.Spec.Destination, st)
		if err != nil || compare.ClusterKey(cluster) != clusterKey {
			continue
		}
		if r.cfg.OwnsCluster != nil && !r.cfg.OwnsCluster(cluster) {
			continue
		}
		r.requestLiveRefresh(ctx, key, false, clusterKey, changed)
	}
}

// requestLiveRefresh requests a recent comparison (root) or a tree-only refresh of key. As
// upstream, the request is logged at info for a root (managed) object, whose change triggers a
// comparison, and at debug otherwise, to keep descendant and orphan churn out of info logs.
func (r *Reconciler) requestLiveRefresh(ctx context.Context, key types.NamespacedName, root bool, clusterKey string, changed cache.ResourceKey) {
	level, logLevel := "tree", slog.LevelDebug
	if root {
		r.refresh.RequestRefresh(key, core.CompareRecent, 0)
		level, logLevel = core.CompareRecent.String(), slog.LevelInfo
	} else {
		r.refresh.RequestTreeRefresh(key)
	}
	if r.log.Enabled(ctx, logLevel) {
		// Not "level": the JSON handler already writes the record level under that key.
		r.log.Log(ctx, logLevel, "requesting app refresh caused by object update",
			append([]any{"application", key.String(), "comparisonLevel", level}, objectAttrs(clusterKey, changed)...)...)
	}
}

// objectAttrs are the log attributes identifying a changed object (never its content).
func objectAttrs(clusterKey string, k cache.ResourceKey) []any {
	namespace := k.Namespace
	if namespace == "" {
		namespace = "(cluster-scoped)"
	}
	return []any{"server", clusterKey, "group", k.Group, "kind", k.Kind, "namespace", namespace, "name", k.Name}
}

// isRootAppNode reports whether r is the root node of the Application tracking it: tracked
// and without owners (upstream isRootAppNode).
func isRootAppNode(r *cache.Resource) bool {
	return compare.InfoOf(r).AppName != "" && len(r.OwnerRefs) == 0
}

// skipAppRequeuing reports kinds whose changes never refresh the Application they belong to:
// core Endpoints, which churn and do not affect the Application (upstream skipAppRequeuing).
func skipAppRequeuing(k cache.ResourceKey) bool {
	return k.Group == "" && k.Kind == "Endpoints"
}

// isSelfReferencedApp reports whether the changed object is the Application app itself;
// refreshing it would loop on its own status writes.
func isSelfReferencedApp(changed cache.ResourceKey, app types.NamespacedName) bool {
	return changed.Group == v1alpha1.GroupName && changed.Kind == "Application" &&
		changed.Name == app.Name && changed.Namespace == app.Namespace
}
