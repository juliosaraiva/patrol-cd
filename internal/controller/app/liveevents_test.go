package app

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

const testServer = "https://k8s"

// requested is a pending refresh request: its comparison level and whether a tree-only
// refresh was asked.
type requested struct {
	level core.CompareLevel
	tree  bool
}

var (
	recentReq = requested{level: core.CompareRecent}
	treeReq   = requested{tree: true}
)

// pendingRequests returns the refresh requests queued in q.
func pendingRequests(q *refreshQueue) map[types.NamespacedName]requested {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[types.NamespacedName]requested{}
	for k, p := range q.pending {
		out[k] = requested{level: p.level, tree: p.tree}
	}
	return out
}

func appKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "patrol", Name: name}
}

// liveEventsReconciler returns a Reconciler of control-plane namespace "patrol" with the
// patrol-cm data cm, over a fake client holding objs with the field indexes of IndexFields
// the live-event handler uses.
func liveEventsReconciler(t *testing.T, cm map[string]string, objs ...client.Object) *Reconciler {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	copies := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		// The fake client sets resourceVersions on the objects it is given.
		copies = append(copies, o.DeepCopyObject().(client.Object))
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(copies...).
		WithIndex(&v1alpha1.Application{}, indexDestinationNamespace, destinationNamespaceKeys).
		WithIndex(&v1alpha1.AppProject{}, indexOrphanedResources, orphanMonitoringKeys).
		Build()
	st := settings.Parse(settings.Input{Namespace: "patrol", ConfigMaps: []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: "patrol", Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       cm,
	}}})
	return &Reconciler{
		cfg:     Config{Deps: core.Deps{Client: c, APIReader: c, Namespace: "patrol", Settings: compare.StaticSettings{Settings: st}}},
		c:       c,
		log:     slog.New(slog.DiscardHandler),
		refresh: newRefreshQueue(clocktesting.NewFakeClock(now)),
	}
}

// liveRes is a cached resource with info (nil: none) and owners.
func liveRes(group, kind, namespace, name string, info *compare.ResourceInfo, owners ...metav1.OwnerReference) *cache.Resource {
	r := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: cache.NewResourceKey(group, kind, namespace, name)}, OwnerRefs: owners}
	if info != nil {
		r.Info = info
	}
	return r
}

func ownedBy(apiVersion, kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name}
}

func nsResources(rs ...*cache.Resource) map[cache.ResourceKey]*cache.Resource {
	out := map[cache.ResourceKey]*cache.Resource{}
	for _, r := range rs {
		out[r.ResourceKey()] = r
	}
	return out
}

func TestOnResourceUpdated(t *testing.T) {
	t.Parallel()
	progressing := &health.HealthStatus{Status: health.StatusProgressing}
	degraded := &health.HealthStatus{Status: health.StatusDegraded}
	healthy := &health.HealthStatus{Status: health.StatusHealthy}

	depA := liveRes("apps", "Deployment", "web", "da", &compare.ResourceInfo{AppName: "a", Health: healthy, ManifestHash: "da"})
	rsA := liveRes("apps", "ReplicaSet", "web", "rs-a", nil, ownedBy("apps/v1", "Deployment", "da"))
	depB := liveRes("apps", "Deployment", "web", "db", &compare.ResourceInfo{AppName: "b", Health: healthy, ManifestHash: "db"})
	rsB := liveRes("apps", "ReplicaSet", "web", "rs-b", nil, ownedBy("apps/v1", "Deployment", "db"))
	svcA := liveRes("", "Service", "web", "svc", &compare.ResourceInfo{AppName: "a", ManifestHash: "svc"})
	ns := nsResources(depA, rsA, depB, rsB, svcA)

	podOf := func(rs string, info *compare.ResourceInfo) *cache.Resource {
		return liveRes("", "Pod", "web", "p", info, ownedBy("apps/v1", "ReplicaSet", rs))
	}
	cm := func(appName, hash string) *cache.Resource {
		return liveRes("", "ConfigMap", "web", "cm", &compare.ResourceInfo{AppName: appName, ManifestHash: hash})
	}
	application := func(appName string) *cache.Resource {
		return liveRes(v1alpha1.GroupName, "Application", "patrol", "a", &compare.ResourceInfo{AppName: appName, ManifestHash: appName})
	}
	endpoints := func(hash string) *cache.Resource {
		return liveRes("", "Endpoints", "web", "svc", &compare.ResourceInfo{ManifestHash: hash}, ownedBy("v1", "Service", "svc"))
	}
	// A tracked PersistentVolumeClaim that the cache later gave a StatefulSet owner: not the
	// Application's root node any more (upstream isRootAppNode).
	pvc := func(hash string) *cache.Resource {
		return liveRes("", "PersistentVolumeClaim", "web", "data-web-0", &compare.ResourceInfo{AppName: "a", ManifestHash: hash}, ownedBy("apps/v1", "StatefulSet", "web"))
	}

	for _, tc := range []struct {
		name           string
		cm             map[string]string
		newRes, oldRes *cache.Resource
		want           map[types.NamespacedName]requested
	}{
		{name: "managed root changed", newRes: cm("a", "2"), oldRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq}},
		{name: "unannotated descendant changed", newRes: podOf("rs-a", &compare.ResourceInfo{Health: progressing}), oldRes: podOf("rs-a", &compare.ResourceInfo{Health: progressing}),
			want: map[types.NamespacedName]requested{appKey("a"): treeReq}},
		{name: "annotated descendant with same hash and health is ignored",
			newRes: podOf("rs-a", &compare.ResourceInfo{Health: progressing, ManifestHash: "p"}), oldRes: podOf("rs-a", &compare.ResourceInfo{Health: progressing, ManifestHash: "p"}),
			want: map[types.NamespacedName]requested{}},
		{name: "annotated descendant whose health changed",
			newRes: podOf("rs-a", &compare.ResourceInfo{Health: degraded, ManifestHash: "p"}), oldRes: podOf("rs-a", &compare.ResourceInfo{Health: progressing, ManifestHash: "p"}),
			want: map[types.NamespacedName]requested{appKey("a"): treeReq}},
		{name: "managed root with same hash and health is ignored", newRes: cm("a", "1"), oldRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{}},
		{name: "suppression disabled", cm: map[string]string{settings.KeyIgnoreResourceUpdates: "false"}, newRes: cm("a", "1"), oldRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq}},
		{name: "root tracking moved to another application", newRes: cm("b", "2"), oldRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq, appKey("b"): recentReq}},
		{name: "descendant moved to another application", newRes: podOf("rs-b", nil), oldRes: podOf("rs-a", nil),
			want: map[types.NamespacedName]requested{appKey("a"): treeReq, appKey("b"): treeReq}},
		{name: "managed root deleted", oldRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq}},
		{name: "managed root created", newRes: cm("a", "1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq}},
		{name: "self-managed application object", newRes: application("a"), oldRes: liveRes(v1alpha1.GroupName, "Application", "patrol", "a", &compare.ResourceInfo{AppName: "a", ManifestHash: "old"}),
			want: map[types.NamespacedName]requested{}},
		{name: "application managed by a parent", newRes: application("parent"), oldRes: liveRes(v1alpha1.GroupName, "Application", "patrol", "a", &compare.ResourceInfo{AppName: "parent", ManifestHash: "old"}),
			want: map[types.NamespacedName]requested{appKey("parent"): recentReq}},
		{name: "application namespace not allowed", newRes: cm("other_x", "2"), oldRes: cm("other_x", "1"),
			want: map[types.NamespacedName]requested{}},
		{name: "endpoints of a managed service", newRes: endpoints("2"), oldRes: endpoints("1"),
			want: map[types.NamespacedName]requested{}},
		{name: "tracked object with an inferred owner", newRes: pvc("2"), oldRes: pvc("1"),
			want: map[types.NamespacedName]requested{appKey("a"): treeReq}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := liveEventsReconciler(t, tc.cm)
			r.onResourceUpdated(testServer, tc.newRes, tc.oldRes, ns)
			assert.Equal(t, tc.want, pendingRequests(r.refresh))
		})
	}
}

// TestOnResourceUpdatedPopulatedInfo runs the handler on ResourceInfo computed by the cache
// populator of CacheSettings with an ignoreResourceUpdates rule (APP-110, DIFF-036): an
// annotated untracked Pod whose status changes without a health change triggers nothing.
func TestOnResourceUpdatedPopulatedInfo(t *testing.T) {
	t.Parallel()
	st := settings.Parse(settings.Input{Namespace: "patrol", ConfigMaps: []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: "patrol", Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       map[string]string{"resource.customizations.ignoreResourceUpdates.all": "jsonPointers:\n  - /status\n"},
	}}})
	populate := compare.CacheSettings(st, &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: testServer}}, "patrol", cache.Settings{}, nil).PopulateResourceInfo
	require.NotNil(t, populate)
	dep := liveRes("apps", "Deployment", "web", "web", &compare.ResourceInfo{AppName: "guestbook"})
	ns := nsResources(dep)
	pod := func(annotated bool, phase, message string) *cache.Resource {
		un := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": "web-1", "namespace": "web", "resourceVersion": message,
				"ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web", "uid": "dep-uid"}},
			},
			"spec":   map[string]any{"containers": []any{map[string]any{"name": "main", "image": "nginx:1.27"}}},
			"status": map[string]any{"phase": phase, "message": message},
		}}
		if annotated {
			un.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyIgnoreResourceUpdates: "true"})
		}
		info, _ := populate(un, false)
		return &cache.Resource{Ref: cache.ResourceRef{ResourceKey: cache.KeyOf(un)}, OwnerRefs: un.GetOwnerReferences(), Info: info}
	}
	healthOf := func(r *cache.Resource) health.Status {
		if h := compare.InfoOf(r).Health; h != nil {
			return h.Status
		}
		return ""
	}

	for _, tc := range []struct {
		name           string
		newRes, oldRes *cache.Resource
		want           map[types.NamespacedName]requested
	}{
		{name: "annotated, status message only", newRes: pod(true, "Pending", "two"), oldRes: pod(true, "Pending", "one"),
			want: map[types.NamespacedName]requested{}},
		{name: "not annotated, status message only", newRes: pod(false, "Pending", "two"), oldRes: pod(false, "Pending", "one"),
			want: map[types.NamespacedName]requested{appKey("guestbook"): treeReq}},
		{name: "annotated, health changed", newRes: pod(true, "Failed", "two"), oldRes: pod(true, "Pending", "one"),
			want: map[types.NamespacedName]requested{appKey("guestbook"): treeReq}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			oldInfo, newInfo := compare.InfoOf(tc.oldRes), compare.InfoOf(tc.newRes)
			assert.Empty(t, newInfo.AppName, "untracked descendants are not attributed by the populator")
			// Only status and resourceVersion differ: equal hashes when hashed at all.
			assert.Equal(t, oldInfo.ManifestHash, newInfo.ManifestHash)
			r := liveEventsReconciler(t, nil)
			r.onResourceUpdated(testServer, tc.newRes, tc.oldRes, ns)
			assert.Equal(t, tc.want, pendingRequests(r.refresh))
		})
	}
	assert.Equal(t, health.StatusProgressing, healthOf(pod(true, "Pending", "one")))
	assert.Equal(t, health.StatusDegraded, healthOf(pod(true, "Failed", "one")))
	assert.Empty(t, compare.InfoOf(pod(false, "Pending", "one")).ManifestHash, "unannotated untracked objects are not hashed")
}

func TestRefreshOrphanMonitors(t *testing.T) {
	t.Parallel()
	project := func(name string, monitor bool) *v1alpha1.AppProject {
		p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"}}
		if monitor {
			p.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
		}
		return p
	}
	cluster := func(name, server string) *v1alpha1.Cluster {
		return &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"}, Spec: v1alpha1.ClusterSpec{Server: server, Name: name}}
	}
	app := func(namespace, name, proj string, dest v1alpha1.ApplicationDestination) *v1alpha1.Application {
		return &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: v1alpha1.ApplicationSpec{Project: proj, Destination: dest}}
	}
	server := func(s, ns string) v1alpha1.ApplicationDestination {
		return v1alpha1.ApplicationDestination{Server: s, Namespace: ns}
	}
	objs := []client.Object{
		cluster("k8s", testServer), cluster("other", "https://other"),
		app("patrol", "mon", "watch", server(testServer+"/", "web")),
		app("patrol", "byname", "watch", v1alpha1.ApplicationDestination{Name: "k8s", Namespace: "web"}),
		app("patrol", "plain", "plain", server(testServer, "web")),
		app("patrol", "othercluster", "watch", server("https://other", "web")),
		app("patrol", "otherns", "watch", server(testServer, "db")),
		app("patrol", "unknown", "missing", server(testServer, "web")),
		app("team", "outside", "watch", server(testServer, "web")),
		app("patrol", "self", "watch", server(testServer, "patrol")),
		app("patrol", "peer", "watch", server(testServer, "patrol")),
	}
	monitors := []client.Object{project("watch", true), project("plain", false)}

	unmanaged := func(hash string) *cache.Resource {
		return liveRes("", "ConfigMap", "web", "stray", &compare.ResourceInfo{ManifestHash: hash})
	}
	managed := func(hash string) *cache.Resource {
		return liveRes("", "ConfigMap", "web", "cm", &compare.ResourceInfo{AppName: "a", ManifestHash: hash})
	}
	svc := liveRes("", "Service", "web", "svc", &compare.ResourceInfo{AppName: "a"})
	rs := liveRes("apps", "ReplicaSet", "web", "rs", nil)
	ns := nsResources(svc, rs)
	web := map[types.NamespacedName]requested{appKey("mon"): treeReq, appKey("byname"): treeReq}

	for _, tc := range []struct {
		name           string
		clusterKey     string
		noMonitors     bool
		ownsCluster    func(*v1alpha1.Cluster) bool
		newRes, oldRes *cache.Resource
		want           map[types.NamespacedName]requested
	}{
		{name: "unmanaged object created", newRes: unmanaged(""), want: web},
		{name: "unmanaged object deleted", oldRes: unmanaged(""), want: web},
		{name: "unmanaged object updated", newRes: unmanaged(""), oldRes: unmanaged(""), want: web},
		{name: "unmanaged update suppressed", newRes: unmanaged("h"), oldRes: unmanaged("h"),
			want: map[types.NamespacedName]requested{}},
		{name: "descendant of an unmanaged object", newRes: liveRes("", "Pod", "web", "p", nil, ownedBy("apps/v1", "ReplicaSet", "rs")), want: web},
		{name: "managed object", newRes: managed("2"), oldRes: managed("1"),
			want: map[types.NamespacedName]requested{appKey("a"): recentReq}},
		{name: "endpoints count for no application", newRes: liveRes("", "Endpoints", "web", "svc", nil, ownedBy("v1", "Service", "svc")), want: web},
		{name: "cluster-scoped object", newRes: liveRes("rbac.authorization.k8s.io", "ClusterRole", "", "cr", nil),
			want: map[types.NamespacedName]requested{}},
		{name: "event of another cluster", clusterKey: "https://other", newRes: unmanaged(""),
			want: map[types.NamespacedName]requested{appKey("othercluster"): treeReq}},
		{name: "cluster of another shard", ownsCluster: func(*v1alpha1.Cluster) bool { return false }, newRes: unmanaged(""),
			want: map[types.NamespacedName]requested{}},
		{name: "monitoring application object itself", newRes: liveRes(v1alpha1.GroupName, "Application", "patrol", "self", &compare.ResourceInfo{ManifestHash: "2"}),
			oldRes: liveRes(v1alpha1.GroupName, "Application", "patrol", "self", &compare.ResourceInfo{ManifestHash: "1"}),
			want:   map[types.NamespacedName]requested{appKey("peer"): treeReq}},
		{name: "no project monitors orphaned resources", noMonitors: true, newRes: unmanaged(""),
			want: map[types.NamespacedName]requested{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			all := objs
			if !tc.noMonitors {
				all = append(append([]client.Object{}, objs...), monitors...)
			}
			r := liveEventsReconciler(t, nil, all...)
			r.cfg.OwnsCluster = tc.ownsCluster
			key := tc.clusterKey
			if key == "" {
				key = testServer
			}
			r.onResourceUpdated(key, tc.newRes, tc.oldRes, ns)
			assert.Equal(t, tc.want, pendingRequests(r.refresh))
		})
	}
}
