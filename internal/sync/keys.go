package sync

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
)

// ResourceKey identifies an object independently of its API version. It is the key type
// of internal/cache, so the managed live objects returned by the cluster cache can be
// passed to the engine unchanged.
type ResourceKey = cache.ResourceKey

// Annotation keys and well-known values read or written by the engine that are not Patrol
// keys (Helm compatibility) or are engine-specific.
const (
	// AnnotationKeyHelmHook is Helm's hook annotation (pre-install, post-upgrade, ...).
	AnnotationKeyHelmHook = "helm.sh/hook"
	// AnnotationKeyHelmHookWeight is Helm's hook weight, used as sync wave when the Patrol
	// sync-wave annotation is absent or not an integer.
	AnnotationKeyHelmHookWeight = "helm.sh/hook-weight"
	// AnnotationKeyHelmHookDeletePolicy is Helm's hook delete policy annotation.
	AnnotationKeyHelmHookDeletePolicy = "helm.sh/hook-delete-policy"

	// AnnotationKeyOperation ("cd.patrol.example/operation") is set on every hook object
	// created by a sync to "<operation UID>/<attempt>/<sync phase>". A restarted engine that
	// finds a hook with its own value adopts it instead of re-creating it
	// (docs/architecture/sync-state-machine.md, "Idempotency and restart recovery").
	AnnotationKeyOperation = v1alpha1.Prefix + "operation"

	// DefaultManager is the field manager used for server-side apply and client-side apply.
	DefaultManager = "patrol-controller"
	// DefaultClientSideApplyMigrationManager is the client-side apply field manager whose
	// fields are migrated to DefaultManager before a server-side apply.
	DefaultClientSideApplyMigrationManager = "kubectl-client-side-apply"

	// EnvSyncWaveDelay is the environment variable holding the delay between sync waves in
	// seconds (upstream ARGOCD_SYNC_WAVE_DELAY).
	EnvSyncWaveDelay = "PATROL_SYNC_WAVE_DELAY"
	// DefaultWaveDelay is the delay between sync waves when EnvSyncWaveDelay is unset.
	DefaultWaveDelay = 2 * time.Second
	// DefaultPollInterval is how long a waiting step asks to be re-queued after.
	DefaultPollInterval = 5 * time.Second
	// DefaultCRDEstablishTimeout bounds the wait for a CRD's Established condition.
	DefaultCRDEstablishTimeout = 3 * time.Second

	kindNamespace = "Namespace"
	kindCRD       = "CustomResourceDefinition"
	groupCRD      = "apiextensions.k8s.io"
)

// keyOf returns the key of obj as it is written (no namespace defaulting).
func keyOf(obj *unstructured.Unstructured) ResourceKey {
	gvk := obj.GroupVersionKind()
	return ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// resultKey identifies one result row: a resource in one sync phase.
func resultKey(k ResourceKey, phase v1alpha1.SyncPhase) string {
	return k.String() + ":" + string(phase)
}

// rowKey returns the result key of a persisted result row.
func rowKey(r v1alpha1.ResourceResult) string {
	return resultKey(ResourceKey{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}, r.SyncPhase)
}

func isCRD(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GroupVersionKind()
	return gvk.Group == groupCRD && gvk.Kind == kindCRD
}

func isNamespaceKind(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GroupVersionKind().Group == "" && obj.GetKind() == kindNamespace
}

func isNamespaceWithName(obj *unstructured.Unstructured, name string) bool {
	return isNamespaceKind(obj) && obj.GetName() == name
}

// crdGroupKind returns "<spec.group>/<spec.names.kind>" of a CRD manifest.
func crdGroupKind(obj *unstructured.Unstructured) (string, bool) {
	if !isCRD(obj) {
		return "", false
	}
	group, ok, err := unstructured.NestedString(obj.Object, "spec", "group")
	if err != nil || !ok {
		return "", false
	}
	kind, ok, err := unstructured.NestedString(obj.Object, "spec", "names", "kind")
	if err != nil || !ok {
		return "", false
	}
	return group + "/" + kind, true
}

// crdScopeNamespaced returns whether a CRD manifest declares a namespaced resource.
func crdScopeNamespaced(obj *unstructured.Unstructured) (namespaced, ok bool) {
	scope, found, err := unstructured.NestedString(obj.Object, "spec", "scope")
	if err != nil || !found {
		return false, false
	}
	return scope == "Namespaced", true
}

// resourceImages returns the container images of workload-like objects, best effort,
// like upstream's GetResourceImages.
func resourceImages(obj *unstructured.Unstructured) []string {
	if obj == nil {
		return nil
	}
	paths := [][]string{
		{"spec", "containers"},
		{"spec", "template", "spec", "containers"},
		{"spec", "jobTemplate", "spec", "template", "spec", "containers"},
	}
	var containers []any
	found := false
	for _, p := range paths {
		c, ok, err := unstructured.NestedSlice(obj.Object, p...)
		if ok && err == nil {
			containers, found = c, true
			break
		}
	}
	if !found {
		return nil
	}
	var images []string
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if img, ok := m["image"].(string); ok {
			images = append(images, img)
		}
	}
	return images
}
