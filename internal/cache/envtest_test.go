package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const waitTimeout = 20 * time.Second

var (
	cmGK     = schema.GroupKind{Kind: "ConfigMap"}
	secretGK = schema.GroupKind{Kind: "Secret"}
)

type kube struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

func newKube(t *testing.T, cfg *rest.Config) *kube {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	cfg.WarningHandler = rest.NoWarnings{}
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	return &kube{cs: cs, dyn: dyn}
}

func (k *kube) namespace(t *testing.T) string {
	t.Helper()
	name := "cache-" + rand.String(8)
	_, err := k.cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return name
}

func (k *kube) configMap(t *testing.T, ns, name string, mutate ...func(*corev1.ConfigMap)) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string]string{"k": "v"}}
	for _, m := range mutate {
		m(cm)
	}
	out, err := k.cs.CoreV1().ConfigMaps(ns).Create(context.Background(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	return out
}

func ownedBy(apiVersion, kind, name string, uid types.UID) func(*corev1.ConfigMap) {
	return func(cm *corev1.ConfigMap) {
		cm.OwnerReferences = append(cm.OwnerReferences, metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid})
	}
}

func startCache(t *testing.T, cfg *rest.Config, s Settings, opts ...Option) *Cluster {
	t.Helper()
	if s.EventProcessingInterval == 0 {
		s.EventProcessingInterval = 10 * time.Millisecond
	}
	c, err := New(cfg, s, append([]Option{WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, c.EnsureSynced(ctx))
	return c
}

func key(group, kind, ns, name string) ResourceKey { return NewResourceKey(group, kind, ns, name) }

func cached(c *Cluster, k ResourceKey) *Resource {
	return c.FindResources(k.Namespace, func(r *Resource) bool { return r.ResourceKey() == k })[k]
}

func waitCached(t *testing.T, c *Cluster, keys ...ResourceKey) {
	t.Helper()
	eventually(t, waitTimeout, func() bool {
		for _, k := range keys {
			if cached(c, k) == nil {
				return false
			}
		}
		return true
	}, "resources %v not cached", keys)
}

type update struct {
	newRes, oldRes *Resource
	inNS           bool
}

type updateLog struct {
	mu      sync.Mutex
	entries []update
}

func (l *updateLog) handler(ns string) ResourceUpdatedHandler {
	return func(newRes, oldRes *Resource, nsRes map[ResourceKey]*Resource) {
		r := newRes
		if r == nil {
			r = oldRes
		}
		if r.Ref.Namespace != ns {
			return
		}
		u := update{newRes: newRes, oldRes: oldRes}
		if newRes != nil {
			_, u.inNS = nsRes[newRes.ResourceKey()]
		} else {
			_, present := nsRes[oldRes.ResourceKey()]
			u.inNS = !present
		}
		l.mu.Lock()
		l.entries = append(l.entries, u)
		l.mu.Unlock()
	}
}

func (l *updateLog) find(pred func(update) bool) (update, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, u := range l.entries {
		if pred(u) {
			return u, true
		}
	}
	return update{}, false
}

func (l *updateLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func TestEnvtestInitialSyncAndEvents(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ns := k.namespace(t)
	pre := k.configMap(t, ns, "pre-existing", func(cm *corev1.ConfigMap) {
		cm.Labels = map[string]string{"app.kubernetes.io/instance": "demo", "unrelated": "x"}
	})

	var rawEvents, batches atomic.Int64
	c := startCache(t, cfg, Settings{TrackingLabelKeys: []string{"app.kubernetes.io/instance"}})
	c.OnEvent(func(watch.EventType, *unstructured.Unstructured) { rawEvents.Add(1) })
	c.OnProcessEvents(func(time.Duration, int) { batches.Add(1) })
	log := &updateLog{}
	unsub := c.OnResourceUpdated(log.handler(ns))

	r := cached(c, key("", "ConfigMap", ns, "pre-existing"))
	require.NotNil(t, r, "objects existing before the sync are cached")
	assert.Equal(t, pre.UID, r.Ref.UID)
	assert.Equal(t, "v1", r.Ref.APIVersion)
	assert.Equal(t, map[string]string{"app.kubernetes.io/instance": "demo"}, r.Labels)
	assert.Nil(t, r.Resource, "unmanaged objects keep no manifest")
	assert.NotNil(t, cached(c, key("", "Namespace", "", ns)), "cluster-scoped objects are cached")

	info := c.GetClusterInfo()
	assert.True(t, info.Synced)
	assert.NoError(t, info.SyncError)
	assert.True(t, strings.HasPrefix(info.K8SVersion, "v1."), info.K8SVersion)
	assert.Equal(t, info.K8SVersion, c.GetServerVersion())
	assert.Greater(t, info.APIsCount, 10)
	assert.Positive(t, info.ResourcesCount)
	assert.Positive(t, info.ActiveWatches)
	assert.Equal(t, cfg.Host, info.Server)
	namespaced, err := c.IsNamespaced(cmGK)
	require.NoError(t, err)
	assert.True(t, namespaced)
	namespaced, err = c.IsNamespaced(schema.GroupKind{Kind: "Namespace"})
	require.NoError(t, err)
	assert.False(t, namespaced)
	assert.True(t, slices.ContainsFunc(c.GetAPIResources(), func(a APIResourceInfo) bool {
		return a.GroupVersionResource == schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	}))

	// Create.
	created := k.configMap(t, ns, "created")
	ck := key("", "ConfigMap", ns, "created")
	eventually(t, waitTimeout, func() bool {
		_, ok := log.find(func(u update) bool { return u.newRes != nil && u.newRes.ResourceKey() == ck && u.oldRes == nil })
		return ok
	}, "create event")
	assert.Equal(t, created.UID, cached(c, ck).Ref.UID)

	// Update.
	created.Data = map[string]string{"k": "v2"}
	updated, err := k.cs.CoreV1().ConfigMaps(ns).Update(context.Background(), created, metav1.UpdateOptions{})
	require.NoError(t, err)
	eventually(t, waitTimeout, func() bool {
		u, ok := log.find(func(u update) bool {
			return u.newRes != nil && u.oldRes != nil && u.newRes.ResourceKey() == ck && u.newRes.ResourceVersion == updated.ResourceVersion
		})
		return ok && u.inNS && u.oldRes.ResourceVersion == created.ResourceVersion
	}, "update event")

	// Delete.
	require.NoError(t, k.cs.CoreV1().ConfigMaps(ns).Delete(context.Background(), "created", metav1.DeleteOptions{}))
	eventually(t, waitTimeout, func() bool {
		u, ok := log.find(func(u update) bool { return u.newRes == nil && u.oldRes != nil && u.oldRes.ResourceKey() == ck })
		return ok && u.inNS
	}, "delete event")
	assert.Nil(t, cached(c, ck))
	assert.Positive(t, rawEvents.Load())
	assert.Positive(t, batches.Load())

	// Unsubscribed handlers are no longer called.
	unsub()
	before := log.len()
	k.configMap(t, ns, "after-unsubscribe")
	waitCached(t, c, key("", "ConfigMap", ns, "after-unsubscribe"))
	assert.Equal(t, before, log.len())
}

func TestEnvtestHierarchy(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	c := startCache(t, cfg, Settings{})

	labels := map[string]string{"app": "web"}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.invalid/img"}}}
	deploy, err := k.cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	rs, err := k.cs.AppsV1().ReplicaSets(ns).Create(ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "Deployment", Name: deploy.Name, UID: deploy.UID, Controller: ptr.To(true)}}},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1-a", Labels: labels, OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID}}},
		Spec: podSpec,
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Cluster-scoped owner of a cluster-scoped owner of a namespaced object, plus a direct
	// cross-namespace child.
	cr1, err := k.cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ns + "-parent"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	cr2, err := k.cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ns + "-child",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: cr1.Name, UID: cr1.UID}}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	k.configMap(t, ns, "owned-by-cluster-role", ownedBy("rbac.authorization.k8s.io/v1", "ClusterRole", cr1.Name, cr1.UID))
	k.configMap(t, ns, "grandchild", ownedBy("rbac.authorization.k8s.io/v1", "ClusterRole", cr2.Name, cr2.UID))

	// Inferred relations: Service → Endpoints, StatefulSet → PVC.
	_, err = k.cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.cs.CoreV1().Endpoints(ns).Create(ctx, &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "svc"}}, metav1.CreateOptions{}) //nolint:staticcheck // Endpoints inference is under test
	require.NoError(t, err)
	_, err = k.cs.AppsV1().StatefulSets(ns).Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
		Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0"},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}, metav1.CreateOptions{})
	require.NoError(t, err)

	deployKey := key("apps", "Deployment", ns, "web")
	rsKey := key("apps", "ReplicaSet", ns, "web-1")
	podKey := key("", "Pod", ns, "web-1-a")
	cr1Key := key("rbac.authorization.k8s.io", "ClusterRole", "", cr1.Name)
	cr2Key := key("rbac.authorization.k8s.io", "ClusterRole", "", cr2.Name)
	ownedKey := key("", "ConfigMap", ns, "owned-by-cluster-role")
	grandKey := key("", "ConfigMap", ns, "grandchild")
	svcKey, epKey := key("", "Service", ns, "svc"), key("", "Endpoints", ns, "svc")
	stsKey, pvcKey := key("apps", "StatefulSet", ns, "db"), key("", "PersistentVolumeClaim", ns, "data-db-0")
	waitCached(t, c, deployKey, rsKey, podKey, cr1Key, cr2Key, ownedKey, grandKey, svcKey, epKey, stsKey, pvcKey)

	walk := func(start []ResourceKey, stopAt string) []ResourceKey {
		var out []ResourceKey
		c.IterateHierarchy(start, func(r *Resource, nsRes map[ResourceKey]*Resource) bool {
			require.Same(t, r, nsRes[r.ResourceKey()], "namespace map contains the visited resource")
			out = append(out, r.ResourceKey())
			return r.Ref.Kind != stopAt
		})
		return out
	}
	assert.Equal(t, []ResourceKey{deployKey, rsKey, podKey}, walk([]ResourceKey{deployKey}, ""), "depth-first order")
	assert.Equal(t, []ResourceKey{deployKey, rsKey}, walk([]ResourceKey{deployKey}, "ReplicaSet"), "false prunes children")
	assert.Equal(t, []ResourceKey{rsKey, podKey, deployKey}, walk([]ResourceKey{rsKey, deployKey, key("", "Pod", ns, "missing")}, ""),
		"each resource is visited once; missing start keys are ignored")
	assert.ElementsMatch(t, []ResourceKey{cr1Key, cr2Key, ownedKey, grandKey}, walk([]ResourceKey{cr1Key}, ""),
		"cluster-scoped owners reach namespaced children across levels")
	assert.Equal(t, []ResourceKey{svcKey, epKey}, walk([]ResourceKey{svcKey}, ""), "Endpoints are inferred children of their Service")
	assert.Equal(t, []ResourceKey{stsKey, pvcKey}, walk([]ResourceKey{stsKey}, ""), "PVCs are inferred children of their StatefulSet")
	pvc := cached(c, pvcKey)
	require.Len(t, pvc.OwnerRefs, 1)
	assert.Equal(t, "StatefulSet", pvc.OwnerRefs[0].Kind)

	top := c.FindResources(ns, TopLevelResource)
	assert.Contains(t, top, deployKey)
	assert.NotContains(t, top, rsKey)
	assert.NotContains(t, top, ownedKey)
	assert.Len(t, c.FindResources(ns, ResourceOfGroupKind("apps", "ReplicaSet")), 1)
}

func crdObject(group, kind, plural string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group,
			"scope": "Namespaced",
			"names": map[string]any{"kind": kind, "plural": plural, "singular": strings.ToLower(kind), "listKind": kind + "List"},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object",
					"properties": map[string]any{"spec": map[string]any{
						"type": "object", "properties": map[string]any{"size": map[string]any{"type": "integer"}}}},
				}},
			}},
		},
	}}
}

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

func TestEnvtestCRDLifecycle(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	c := startCache(t, cfg, Settings{})

	group := rand.String(6) + ".patrol.test"
	gk := schema.GroupKind{Group: group, Kind: "Widget"}
	gvr := schema.GroupVersionResource{Group: group, Version: "v1", Resource: "widgets"}
	_, err := k.dyn.Resource(crdGVR).Create(ctx, crdObject(group, "Widget", "widgets"), metav1.CreateOptions{})
	require.NoError(t, err)

	eventually(t, waitTimeout, func() bool {
		namespaced, err := c.IsNamespaced(gk)
		return err == nil && namespaced
	}, "discovery refresh after CRD creation")

	var cr *unstructured.Unstructured
	eventually(t, waitTimeout, func() bool {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": group + "/v1", "kind": "Widget",
			"metadata": map[string]any{"name": "w1", "namespace": ns}, "spec": map[string]any{"size": int64(3)},
		}}
		cr, err = k.dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	}, "create custom resource")
	crKey := key(group, "Widget", ns, "w1")
	waitCached(t, c, crKey)
	assert.Equal(t, cr.GetUID(), cached(c, crKey).Ref.UID)

	eventually(t, waitTimeout, func() bool {
		p, err := c.GetGVKParser(ctx)
		return err == nil && p.Type(gk.WithVersion("v1")) != nil
	}, "OpenAPI schema reloaded with the new CRD")

	require.NoError(t, k.dyn.Resource(crdGVR).Delete(ctx, "widgets."+group, metav1.DeleteOptions{}))
	eventually(t, waitTimeout, func() bool {
		_, err := c.IsNamespaced(gk)
		return errors.Is(err, ErrUnknownGroupKind) && cached(c, crKey) == nil
	}, "CRD deletion stops the watch and drops its objects")
	assert.Empty(t, c.FindResources("", ResourceOfGroupKind(group, "Widget")))
}

func TestEnvtestExcludedResources(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	k.configMap(t, ns, "before")
	_, err := k.cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}}, metav1.CreateOptions{})
	require.NoError(t, err)

	var listed sync.Map
	c := startCache(t, cfg, Settings{ResourceFilter: excludeFilter{cmGK: true}},
		WithListerWatcherWrapper(func(api APIResourceInfo, nsName string, lw ListerWatcher) ListerWatcher {
			listed.Store(api.GroupKind, true)
			return lw
		}))
	k.configMap(t, ns, "after")
	waitCached(t, c, key("", "Secret", ns, "s"))

	_, touched := listed.Load(cmGK)
	assert.False(t, touched, "excluded kinds are never listed or watched")
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, c.FindResources(ns, ResourceOfGroupKind("", "ConfigMap")))
	namespaced, err := c.IsNamespaced(cmGK)
	require.NoError(t, err)
	assert.True(t, namespaced, "scope of excluded kinds is still known")

	// Targets of excluded kinds are fetched directly.
	target := mkObj("v1", "ConfigMap", ns, "after", "")
	live, err := c.GetManagedLiveObjs(ctx, []*unstructured.Unstructured{target, mkObj("v1", "ConfigMap", ns, "missing", "")}, nil)
	require.NoError(t, err)
	require.Contains(t, live, key("", "ConfigMap", ns, "after"))
	assert.NotContains(t, live, key("", "ConfigMap", ns, "missing"))
	data, _, _ := unstructured.NestedStringMap(live[key("", "ConfigMap", ns, "after")].Object, "data")
	assert.Equal(t, map[string]string{"k": "v"}, data)
}

func TestEnvtestNamespaceRestricted(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	nsA, nsB := k.namespace(t), k.namespace(t)
	k.configMap(t, nsA, "a")
	k.configMap(t, nsB, "b")

	var scopes sync.Map
	c := startCache(t, cfg, Settings{Namespaces: []string{nsA}},
		WithListerWatcherWrapper(func(api APIResourceInfo, nsName string, lw ListerWatcher) ListerWatcher {
			scopes.Store(nsName, true)
			return lw
		}))
	waitCached(t, c, key("", "ConfigMap", nsA, "a"))
	k.configMap(t, nsA, "a2")
	k.configMap(t, nsB, "b2")
	waitCached(t, c, key("", "ConfigMap", nsA, "a2"))

	all := c.FindResources("")
	for k := range all {
		assert.Equal(t, nsA, k.Namespace, "only the managed namespace is cached: %s", k)
	}
	scopes.Range(func(k, _ any) bool {
		assert.Equal(t, nsA, k, "watches are namespace-scoped")
		return true
	})

	_, err := c.GetManagedLiveObjs(ctx, []*unstructured.Unstructured{mkObj("v1", "ConfigMap", nsB, "b", "")}, nil)
	assert.ErrorIs(t, err, ErrOutOfScope)
	assert.EqualError(t, err, fmt.Sprintf("namespace %q for ConfigMap %q is not managed", nsB, "b"))
	_, err = c.GetManagedLiveObjs(ctx, []*unstructured.Unstructured{mkObj("v1", "Namespace", "", nsA, "")}, nil)
	assert.ErrorIs(t, err, ErrOutOfScope)

	withCluster := startCache(t, cfg, Settings{Namespaces: []string{nsA}, ClusterResources: true})
	waitCached(t, withCluster, key("", "Namespace", "", nsA), key("", "Namespace", "", nsB))
	assert.Nil(t, cached(withCluster, key("", "ConfigMap", nsB, "b")))
	_, err = withCluster.GetManagedLiveObjs(ctx, []*unstructured.Unstructured{mkObj("v1", "Namespace", "", nsA, "")}, nil)
	assert.NoError(t, err)
}

// expiringLW makes the first watch of its scope fail with 410 Gone.
type expiringLW struct {
	ListerWatcher
	expired *atomic.Bool
	lists   *atomic.Int32
}

func (e *expiringLW) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	e.lists.Add(1)
	return e.ListerWatcher.List(ctx, opts)
}

func (e *expiringLW) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	if e.expired.CompareAndSwap(false, true) {
		fw := watch.NewFakeWithChanSize(1, false)
		fw.Error(expiredStatus(true))
		return fw, nil
	}
	return e.ListerWatcher.Watch(ctx, opts)
}

func TestEnvtestRelistAfterExpired(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ns := k.namespace(t)
	k.configMap(t, ns, "initial")

	var lists atomic.Int32
	var expiredOnce atomic.Bool
	var relists sync.Map
	c := startCache(t, cfg, Settings{
		Namespaces: []string{ns},
		Metrics: Metrics{Relisted: func(gk schema.GroupKind, _ string, reason string) {
			relists.Store(gk.String()+"/"+reason, true)
		}},
	}, WithListerWatcherWrapper(func(api APIResourceInfo, _ string, lw ListerWatcher) ListerWatcher {
		if api.GroupKind != cmGK {
			return lw
		}
		return &expiringLW{ListerWatcher: lw, expired: &expiredOnce, lists: &lists}
	}))
	eventually(t, waitTimeout, func() bool {
		_, ok := relists.Load(cmGK.String() + "/" + relistExpired)
		return ok && lists.Load() >= 2
	}, "410 triggers a relist (initial list + relist)")
	k.configMap(t, ns, "after-relist")
	waitCached(t, c, key("", "ConfigMap", ns, "initial"), key("", "ConfigMap", ns, "after-relist"))
}

func TestEnvtestManagedObjects(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	const managedLabel = "patrol.test/managed"
	lastApplied := `{"apiVersion":"v1","kind":"ConfigMap"}`
	k.configMap(t, ns, "managed", func(cm *corev1.ConfigMap) {
		cm.Labels = map[string]string{managedLabel: "true"}
		cm.Annotations = map[string]string{AnnotationLastApplied: lastApplied, "other": "kept"}
	})
	k.configMap(t, ns, "unmanaged")
	k.configMap(t, ns, "child", ownedBy("v1", "ConfigMap", "managed", "00000000-0000-0000-0000-000000000000"), func(cm *corev1.ConfigMap) {
		cm.Labels = map[string]string{managedLabel: "true"}
	})
	_, err := k.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).Create(ctx, &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "hpa"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	isManagedUn := func(un *unstructured.Unstructured) bool { return un.GetLabels()[managedLabel] == "true" }
	c := startCache(t, cfg, Settings{Namespaces: []string{ns}, IsManaged: isManagedUn})
	managedKey, unmanagedKey := key("", "ConfigMap", ns, "managed"), key("", "ConfigMap", ns, "unmanaged")
	hpaKey := key("autoscaling", "HorizontalPodAutoscaler", ns, "hpa")
	waitCached(t, c, managedKey, unmanagedKey, hpaKey)

	m := cached(c, managedKey)
	require.NotNil(t, m.Resource)
	assert.Empty(t, m.Resource.GetManagedFields(), "managedFields are dropped from retained manifests")
	assert.NotContains(t, m.Resource.GetAnnotations(), AnnotationLastApplied)
	assert.Equal(t, "kept", m.Resource.GetAnnotations()["other"])
	assert.Equal(t, lastApplied, m.LastAppliedConfiguration)
	assert.Nil(t, cached(c, unmanagedKey).Resource, "unmanaged objects retain no manifest")
	assert.Equal(t, "autoscaling/v2", cached(c, hpaKey).Ref.APIVersion, "watched in the preferred version")

	isManaged := func(r *Resource) bool { return r.Resource != nil && r.Resource.GetLabels()[managedLabel] == "true" }
	targets := []*unstructured.Unstructured{
		mkObj("v1", "ConfigMap", ns, "managed", ""),
		mkObj("v1", "ConfigMap", ns, "unmanaged", ""),
		mkObj("v1", "ConfigMap", ns, "missing", ""),
		mkObj("autoscaling/v1", "HorizontalPodAutoscaler", ns, "hpa", ""),
	}
	live, err := c.GetManagedLiveObjs(ctx, targets, isManaged)
	require.NoError(t, err)
	assert.ElementsMatch(t, []ResourceKey{managedKey, unmanagedKey, hpaKey}, slices.Collect(func(yield func(ResourceKey) bool) {
		for k := range live {
			if !yield(k) {
				return
			}
		}
	}), "child resources are not returned as managed roots; missing targets are absent")
	assert.Equal(t, lastApplied, live[managedKey].GetAnnotations()[AnnotationLastApplied], "last-applied is restored on returned copies")
	assert.NotEmpty(t, live[unmanagedKey].GetManagedFields(), "objects without retained manifest are fetched with GET")
	assert.Equal(t, "autoscaling/v1", live[hpaKey].GetAPIVersion(), "targets are returned in their requested version")

	live[managedKey].SetLabels(map[string]string{"mutated": "yes"})
	assert.Equal(t, "true", cached(c, managedKey).Resource.GetLabels()[managedLabel], "returned objects are copies")
}

func TestEnvtestRespectRBAC(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	k.configMap(t, ns, "visible")
	_, err := k.cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "listed-only"}}, metav1.CreateOptions{})
	require.NoError(t, err)

	user, err := testEnv.AddUser(envtest.User{Name: "cache-" + ns, Groups: []string{"cache-" + ns}}, nil)
	require.NoError(t, err)
	_, err = k.cs.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "cache"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.cs.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cache"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "cache"},
		Subjects: []rbacv1.Subject{{Kind: "User", Name: "cache-" + ns, APIGroup: "rbac.authorization.k8s.io"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	userCfg := user.Config()
	userCS, err := kubernetes.NewForConfig(userCfg)
	require.NoError(t, err)
	eventually(t, waitTimeout, func() bool {
		_, err1 := userCS.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		_, err2 := userCS.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		return err1 == nil && err2 == nil
	}, "RBAC binding propagated")

	t.Run("none fails the sync", func(t *testing.T) {
		c, err := New(rest.CopyConfig(userCfg), Settings{Namespaces: []string{ns}, RespectRBAC: RespectRBACNone}, WithLogger(slog.New(slog.DiscardHandler)))
		require.NoError(t, err)
		defer c.Close()
		eventually(t, waitTimeout, func() bool {
			err := c.EnsureSynced(ctx)
			return err != nil && apierrors.IsForbidden(err)
		}, "sync fails with forbidden")
		assert.NotErrorIs(t, c.EnsureSynced(ctx), ErrClusterUnreachable)
	})

	for _, mode := range []RespectRBAC{RespectRBACNormal, RespectRBACStrict} {
		t.Run(string(mode), func(t *testing.T) {
			var c *Cluster
			eventually(t, waitTimeout, func() bool {
				var err error
				c, err = New(rest.CopyConfig(userCfg), Settings{Namespaces: []string{ns}, RespectRBAC: mode, EventProcessingInterval: 10 * time.Millisecond},
					WithLogger(slog.New(slog.DiscardHandler)))
				require.NoError(t, err)
				if err := c.EnsureSynced(ctx); err != nil {
					c.Close()
					return false // RBAC bindings may take a moment to propagate
				}
				return true
			}, "sync with %s", mode)
			defer c.Close()
			waitCached(t, c, key("", "ConfigMap", ns, "visible"), key("", "Secret", ns, "listed-only"))
			assert.Nil(t, cached(c, key("", "Namespace", "", ns)))

			eventually(t, waitTimeout, func() bool {
				var listDenied, watchDenied bool
				for _, u := range c.GetClusterInfo().UnavailableAPIs {
					listDenied = listDenied || (u.GroupKind == schema.GroupKind{Kind: "Pod"} && u.Reason == ReasonListForbidden && u.Namespace == ns)
					watchDenied = watchDenied || (u.GroupKind == secretGK && u.Reason == ReasonWatchForbidden)
				}
				return listDenied && watchDenied
			}, "unavailable APIs surfaced in ClusterInfo")
			assert.True(t, c.GetClusterInfo().Synced)
		})
	}
}

func TestEnvtestInvalidateResyncAndRecovery(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ns := k.namespace(t)
	k.configMap(t, ns, "cm")

	c, err := New(closedPortConfig(t), Settings{Namespaces: []string{ns}, EventProcessingInterval: 10 * time.Millisecond}, WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)
	t.Cleanup(c.Close)
	require.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClusterUnreachable)

	// Invalidate with a working config retries immediately despite the retry window.
	c.Invalidate(WithConfig(cfg))
	require.NoError(t, c.EnsureSynced(context.Background()))
	cmKey := key("", "ConfigMap", ns, "cm")
	waitCached(t, c, cmKey)

	log := &updateLog{}
	c.OnResourceUpdated(log.handler(ns))
	c.Invalidate(WithSettings(Settings{Namespaces: []string{ns}, ResourceFilter: excludeFilter{cmGK: true},
		ClusterResyncPeriod: 300 * time.Millisecond, EventProcessingInterval: 10 * time.Millisecond}))
	assert.False(t, c.GetClusterInfo().Synced)
	require.NoError(t, c.EnsureSynced(context.Background()))
	assert.Nil(t, cached(c, cmKey), "new settings apply after invalidation")
	eventually(t, waitTimeout, func() bool {
		_, ok := log.find(func(u update) bool { return u.newRes == nil && u.oldRes.ResourceKey() == cmKey })
		return ok
	}, "resync notifies removals")

	first := *c.GetClusterInfo().LastCacheSyncTime
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, c.EnsureSynced(context.Background()), "a due periodic resync does not block callers")
	eventually(t, waitTimeout, func() bool {
		info := c.GetClusterInfo()
		return info.LastCacheSyncTime.After(first) && info.Synced
	}, "background resync")
}

func TestEnvtestOpenAPI(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	c, err := New(cfg, Settings{}, WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)
	defer c.Close()
	// The OpenAPI data is loaded lazily and does not require a sync.
	p, err := c.GetGVKParser(context.Background())
	require.NoError(t, err)
	pt := p.Type(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.NotNil(t, pt)
	_, err = pt.FromUnstructured(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "x"}, "spec": map[string]any{"replicas": int64(1)}})
	assert.NoError(t, err)
	schemas, err := c.GetOpenAPISchema(context.Background())
	require.NoError(t, err)
	assert.Contains(t, schemas, "io.k8s.api.apps.v1.Deployment")
	p2, err := c.GetGVKParser(context.Background())
	require.NoError(t, err)
	assert.Same(t, p, p2, "parser is cached")
}

func TestEnvtestConcurrencyStress(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k := newKube(t, cfg)
	ctx := context.Background()
	ns := k.namespace(t)
	root := k.configMap(t, ns, "root")
	c := startCache(t, cfg, Settings{Namespaces: []string{ns}, ClusterResources: true,
		IsManaged: func(un *unstructured.Unstructured) bool { return un.GetName() == "root" }})

	var handled atomic.Int64
	c.OnResourceUpdated(func(newRes, oldRes *Resource, nsRes map[ResourceKey]*Resource) {
		handled.Add(1)
		for range nsRes { // handlers may iterate the snapshot freely
		}
		_ = c.FindResources(ns) // and call back into the cache
	})

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := range 8 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch i % 4 {
				case 0:
					for _, r := range c.FindResources(ns, TopLevelResource) {
						_ = r.ResourceVersion
					}
				case 1:
					c.IterateHierarchy([]ResourceKey{key("", "ConfigMap", ns, "root")}, func(r *Resource, nsRes map[ResourceKey]*Resource) bool {
						_ = len(nsRes)
						return true
					})
				case 2:
					_ = c.GetClusterInfo()
					_, _ = c.IsNamespaced(cmGK)
					_ = c.GetAPIResources()
				case 3:
					_, _ = c.GetManagedLiveObjs(ctx, []*unstructured.Unstructured{mkObj("v1", "ConfigMap", ns, "root", "")},
						func(r *Resource) bool { return r.Resource != nil })
				}
			}
		})
	}

	const n = 40
	var writers sync.WaitGroup
	for w := range 4 {
		writers.Go(func() {
			cms := k.cs.CoreV1().ConfigMaps(ns)
			for i := w; i < n; i += 4 {
				name := fmt.Sprintf("cm-%02d", i)
				cm, err := cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "v1", Kind: "ConfigMap", Name: "root", UID: root.UID}}}, Data: map[string]string{"k": "v"}}, metav1.CreateOptions{})
				if !assert.NoError(t, err) {
					return
				}
				cm.Data["k"] = "updated"
				_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
				assert.NoError(t, err)
				if i%3 == 0 {
					assert.NoError(t, cms.Delete(ctx, name, metav1.DeleteOptions{}))
				}
			}
		})
	}
	// Invalidate and resync while writes are in flight.
	time.Sleep(100 * time.Millisecond)
	c.Invalidate()
	require.NoError(t, c.EnsureSynced(ctx))
	writers.Wait()

	eventually(t, waitTimeout, func() bool {
		list, err := k.cs.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		want := map[string]string{}
		for _, cm := range list.Items {
			want[cm.Name] = cm.ResourceVersion
		}
		got := map[string]string{}
		for k, r := range c.FindResources(ns, ResourceOfGroupKind("", "ConfigMap")) {
			got[k.Name] = r.ResourceVersion
		}
		return assert.ObjectsAreEqual(want, got)
	}, "cache converges to the API server state")

	// The resync above may have delivered the writes as one list, so no event need have reached the
	// handler yet (on a fast machine the writes are still in flight at the resync and it has run; on a
	// slow one they are done). A change after the resync must reach it.
	_, err := k.cs.CoreV1().ConfigMaps(ns).Patch(ctx, "root", types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"stress":"done"}}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	eventually(t, waitTimeout, func() bool { return handled.Load() > 0 }, "the update handler runs for a change after the resync")
	close(stop)
	readers.Wait()

	var children []string
	c.IterateHierarchy([]ResourceKey{key("", "ConfigMap", ns, "root")}, func(r *Resource, _ map[ResourceKey]*Resource) bool {
		children = append(children, r.Ref.Name)
		return true
	})
	deleted := 0
	for i := range n {
		if i%3 == 0 {
			deleted++
		}
	}
	assert.Len(t, children, 1+n-deleted, "root plus surviving children")
	assert.Positive(t, handled.Load())
}

func TestEnvtestManager(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	var created atomic.Int32
	m := NewManager(ManagerOptions{
		Logger:         slog.New(slog.DiscardHandler),
		OnCacheCreated: func(string, ClusterCache) { created.Add(1) },
	})
	defer m.Close()
	ctx := context.Background()
	s := Settings{Namespaces: []string{"default"}}

	c1, err := m.Get(ctx, "in-cluster", cfg, s)
	require.NoError(t, err)
	c2, err := m.Get(ctx, "in-cluster", cfg, s)
	require.NoError(t, err)
	assert.Same(t, c1, c2)
	assert.EqualValues(t, 1, created.Load())
	assert.True(t, c1.GetClusterInfo().Synced)

	m.Invalidate("in-cluster")
	assert.ErrorIs(t, c1.EnsureSynced(ctx), ErrClosed, "invalidated caches are closed")
	c3, err := m.Get(ctx, "in-cluster", cfg, s)
	require.NoError(t, err)
	assert.NotSame(t, c1, c3)
	assert.EqualValues(t, 2, created.Load())

	_, err = m.Get(ctx, "unreachable", closedPortConfig(t), s)
	assert.ErrorIs(t, err, ErrClusterUnreachable)
	infos := m.ClustersInfo()
	require.Len(t, infos, 2, "failed caches stay registered for retry")
	assert.True(t, infos[0].Synced)
	assert.False(t, infos[1].Synced)

	m.Close()
	_, err = m.Get(ctx, "in-cluster", cfg, s)
	assert.ErrorIs(t, err, ErrManagerClosed)
}
