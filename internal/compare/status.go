package compare

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// isHookObject reports a sync hook or an Application deletion hook (upstream controller isHook).
func isHookObject(obj *unstructured.Unstructured) bool {
	return syncpkg.IsHook(obj) || syncpkg.IsPreDeleteHook(obj) || syncpkg.IsPostDeleteHook(obj)
}

// isManagedNamespace reports whether obj is the destination namespace managed through
// syncPolicy.managedNamespaceMetadata.
func isManagedNamespace(obj *unstructured.Unstructured, app *v1alpha1.Application) bool {
	return obj != nil && obj.GetKind() == "Namespace" && obj.GroupVersionKind().Group == "" &&
		obj.GetName() == app.Spec.Destination.Namespace &&
		app.Spec.SyncPolicy != nil && app.Spec.SyncPolicy.ManagedNamespaceMetadata != nil
}

// managedNamespaceTarget synthesizes the desired state of a managed namespace that is not in
// the manifests (upstream syncNamespace on a generated Namespace): the managed labels and
// annotations, applied with server-side apply.
func managedNamespaceTarget(name string, md *v1alpha1.ManagedNamespaceMetadata) (*unstructured.Unstructured, error) {
	ns := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ns)
	if err != nil {
		return nil, fmt.Errorf("failed to build managed namespace %q: %w", name, err)
	}
	u := &unstructured.Unstructured{Object: obj}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec")
	unstructured.RemoveNestedField(u.Object, "status")
	if md != nil && md.Labels != nil {
		u.SetLabels(md.Labels)
	}
	annotations := map[string]string{}
	if md != nil {
		for k, v := range md.Annotations {
			annotations[k] = v
		}
	}
	annotations[v1alpha1.AnnotationKeySyncOptions] = v1alpha1.SyncOptionKeyServerSideApply + "=" + v1alpha1.SyncOptionValueTrue
	u.SetAnnotations(annotations)
	return u, nil
}

// changedManagedNamespaceMetadata reports a managed namespace metadata change since the last
// sync (upstream HasChangedManagedNamespaceMetadata).
func changedManagedNamespaceMetadata(app *v1alpha1.Application, last *v1alpha1.SyncOperationResult) bool {
	return app.Spec.SyncPolicy != nil && app.Spec.SyncPolicy.ManagedNamespaceMetadata != nil && last != nil &&
		!reflect.DeepEqual(app.Spec.SyncPolicy.ManagedNamespaceMetadata, last.ManagedNamespaceMetadata)
}

// annotationOptionValue returns the value of key in the sync-options annotation (first match).
func annotationOptionValue(obj *unstructured.Unstructured, key string) (string, bool) {
	for _, opt := range syncpkg.AnnotationOptions(obj, v1alpha1.AnnotationKeySyncOptions) {
		if k, v, ok := v1alpha1.ParseSyncOption(opt); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// requiresDeletionConfirmation reports Delete=confirm or Prune=confirm on the object, falling
// back to the Application's sync options (upstream isObjRequiresDeletionConfirmation).
func requiresDeletionConfirmation(obj *unstructured.Unstructured, app *v1alpha1.Application) bool {
	if obj == nil {
		return false
	}
	for _, key := range []string{v1alpha1.SyncOptionKeyDelete, v1alpha1.SyncOptionKeyPrune} {
		v, ok := annotationOptionValue(obj, key)
		if !ok && app.Spec.SyncPolicy != nil {
			v, ok = app.Spec.SyncPolicy.SyncOptions.GetOptionValue(key)
		}
		if ok && v == v1alpha1.SyncOptionValueConfirm {
			return true
		}
	}
	return false
}

// isSelfReferencedApp reports whether live is the Application itself (an app managing itself
// does not affect its own health).
func isSelfReferencedApp(app *v1alpha1.Application, live *unstructured.Unstructured) bool {
	gvk := live.GroupVersionKind()
	return live.GetUID() == app.UID && live.GetName() == app.Name && live.GetNamespace() == app.Namespace &&
		gvk.Group == v1alpha1.GroupName && gvk.Kind == "Application"
}

// applicationHealth assesses every managed resource and aggregates the application health
// (upstream setApplicationHealth). It fills managed[i].Health and, when persist is set,
// statuses[i].Health. The first assessment error is returned for a ComparisonError.
func applicationHealth(app *v1alpha1.Application, managed []ManagedResource, statuses []v1alpha1.ResourceStatus, overrides health.Overrides, persist bool) (v1alpha1.HealthStatusCode, error) {
	inputs := make([]health.ResourceHealth, len(managed))
	var firstErr error
	for i := range managed {
		mr := &managed[i]
		in := health.ResourceHealth{Group: mr.Key.Group, Kind: mr.Key.Kind, Live: mr.Live != nil}
		in.Hook = (mr.Target != nil && syncpkg.IsSkip(mr.Target)) ||
			(mr.Live != nil && (syncpkg.IsHook(mr.Live) || syncpkg.IsIgnored(mr.Live)))
		if mr.Live != nil && !in.Hook {
			in.IgnoreHealthCheck = health.IgnoresHealthCheck(mr.Live)
			in.SelfReference = isSelfReferencedApp(app, mr.Live)
			if !in.IgnoreHealthCheck && !in.SelfReference {
				hs, err := health.Assess(mr.Live, overrides)
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("failed to get resource health for %q with name %q in namespace %q: %w",
						mr.Live.GetKind(), mr.Live.GetName(), mr.Live.GetNamespace(), err)
				}
				in.Health = hs
			}
		}
		inputs[i] = in
	}
	agg := health.AggregateDetailed(inputs)
	for i, hs := range agg.Resources {
		if hs == nil {
			continue
		}
		h := &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusCode(hs.Status), Message: hs.Message}
		managed[i].Health = h
		if persist {
			c := *h
			statuses[i].Health = &c
		}
	}
	return v1alpha1.HealthStatusCode(agg.Status), firstErr
}

func resourceStatusKey(r v1alpha1.ResourceStatus) string {
	return strings.Join([]string{r.Group, r.Kind, r.Namespace, r.Name}, "/")
}

// boundResources sorts statuses by group/kind/namespace/name and keeps at most limit entries
// (limit < 0: unbounded). Entries that need attention (not Synced, requiring pruning, or not
// Healthy) are kept before others, so a truncated list still shows drift.
func boundResources(statuses []v1alpha1.ResourceStatus, limit int) ([]v1alpha1.ResourceStatus, int) {
	sorted := slices.Clone(statuses)
	slices.SortStableFunc(sorted, func(a, b v1alpha1.ResourceStatus) int {
		return strings.Compare(resourceStatusKey(a), resourceStatusKey(b))
	})
	if limit < 0 || len(sorted) <= limit {
		return sorted, 0
	}
	important := func(r v1alpha1.ResourceStatus) bool {
		return (r.Status != v1alpha1.SyncStatusCodeSynced && r.Status != "") || r.RequiresPruning ||
			(r.Health != nil && r.Health.Status != v1alpha1.HealthStatusHealthy)
	}
	keep := make([]bool, len(sorted))
	n := 0
	for i, r := range sorted {
		if n < limit && important(r) {
			keep[i] = true
			n++
		}
	}
	for i := range sorted {
		if n >= limit {
			break
		}
		if !keep[i] {
			keep[i] = true
			n++
		}
	}
	out := make([]v1alpha1.ResourceStatus, 0, limit)
	for i, r := range sorted {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out, len(sorted) - len(out)
}
