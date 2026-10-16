package project

import (
	"context"
	"fmt"
	"path"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// IsGroupKindPermitted reports whether proj permits managing resources of gk, independent of
// the resource name (see IsGroupKindNamePermitted).
func IsGroupKindPermitted(proj *v1alpha1.AppProject, gk schema.GroupKind, namespaced bool) bool {
	return IsGroupKindNamePermitted(proj, gk, "", namespaced)
}

// IsGroupKindNamePermitted reports whether proj permits managing the resource (upstream
// AppProject.IsGroupKindNamePermitted). Group, kind and name entries are path.Match patterns
// ("*" for all); the core group is "".
//
//   - Namespaced resources: permitted when spec.namespaceResourceWhitelist is nil (absent) or
//     non-empty and matching, and no spec.namespaceResourceBlacklist entry matches. A
//     non-nil empty whitelist permits nothing.
//   - Cluster-scoped resources: permitted only when spec.clusterResourceWhitelist is
//     non-empty and has a matching entry, and no spec.clusterResourceBlacklist entry matches.
//     An entry with an empty name matches every name; otherwise its name pattern is matched
//     against name (so a name-restricted entry never matches an empty name).
//
// Malformed patterns never match.
func IsGroupKindNamePermitted(proj *v1alpha1.AppProject, gk schema.GroupKind, name string, namespaced bool) bool {
	if proj == nil {
		return false
	}
	res := metav1.GroupKind{Group: gk.Group, Kind: gk.Kind}
	if namespaced {
		wl, bl := proj.Spec.NamespaceResourceWhitelist, proj.Spec.NamespaceResourceBlacklist
		whitelisted := wl == nil || len(wl) != 0 && groupKindInList(res, wl)
		blacklisted := len(bl) != 0 && groupKindInList(res, bl)
		return whitelisted && !blacklisted
	}
	wl, bl := proj.Spec.ClusterResourceWhitelist, proj.Spec.ClusterResourceBlacklist
	whitelisted := len(wl) != 0 && namedResourceInList(res, name, wl)
	blacklisted := len(bl) != 0 && namedResourceInList(res, name, bl)
	return whitelisted && !blacklisted
}

func pathMatch(pattern, value string) bool {
	ok, err := path.Match(pattern, value)
	return ok && err == nil
}

func groupKindInList(res metav1.GroupKind, list []metav1.GroupKind) bool {
	for _, item := range list {
		if pathMatch(item.Kind, res.Kind) && pathMatch(item.Group, res.Group) {
			return true
		}
	}
	return false
}

func namedResourceInList(res metav1.GroupKind, name string, list []v1alpha1.ClusterResourceRestrictionItem) bool {
	for _, item := range list {
		if !pathMatch(item.Kind, res.Kind) || !pathMatch(item.Group, res.Group) {
			continue
		}
		if item.Name == "" || pathMatch(item.Name, name) {
			return true
		}
	}
	return false
}

// IsResourcePermitted reports whether a resource may be managed by Applications of proj
// deploying to cluster (upstream AppProject.IsResourcePermitted): its kind (and name) must
// be permitted, and a namespaced resource (namespace != "") must target a permitted
// destination namespace on cluster. A cluster-scoped resource only needs the kind check.
func IsResourcePermitted(ctx context.Context, proj *v1alpha1.AppProject, gk schema.GroupKind, name, namespace string, cluster *v1alpha1.Cluster, lister ProjectClusterLister) (bool, error) {
	if !IsGroupKindNamePermitted(proj, gk, name, namespace != "") {
		return false, nil
	}
	if namespace == "" {
		return true, nil
	}
	return IsDestinationPermitted(ctx, proj, DestinationOf(cluster, namespace), lister)
}

// IsLiveResourcePermitted reports whether a live object found on cluster is permitted by
// proj (upstream AppProject.IsLiveResourcePermitted). Non-permitted live objects are dropped
// from comparison, tree traversal, pruning and cascade deletion by the controller.
func IsLiveResourcePermitted(ctx context.Context, proj *v1alpha1.AppProject, obj *unstructured.Unstructured, cluster *v1alpha1.Cluster, lister ProjectClusterLister) (bool, error) {
	if obj == nil {
		return false, nil
	}
	return IsResourcePermitted(ctx, proj, obj.GroupVersionKind().GroupKind(), obj.GetName(), obj.GetNamespace(), cluster, lister)
}

// LiveResourceCheckErrorMessage is the ComparisonError message the controller reports when
// IsLiveResourcePermitted fails.
func LiveResourceCheckErrorMessage(kind, project string, err error) string {
	return fmt.Sprintf("Failed to check if live resource %q is permitted in project %q: %v", kind, project, err)
}

// ResourceScope is what API discovery says about a resource kind on the destination cluster.
type ResourceScope struct {
	// Known is false when discovery has no information about the kind.
	Known bool
	// Namespaced is the discovered scope.
	Namespaced bool
}

// ValidateSyncPermissions checks one sync task's object against proj before it is applied,
// pruned or run as a hook (upstream controller validateSyncPermissions). The scope comes
// from discovery, not from the manifest: a cluster-scoped kind is checked against the cluster
// lists whatever namespace the manifest carries. Errors carry the upstream wording:
//
//   - "failed to get API resource info for <g>/<k>: unable to verify permissions"
//   - "resource <g>:<k> is not permitted in project <p>"
//   - "namespace <ns> is not permitted in project '<p>'"
func ValidateSyncPermissions(ctx context.Context, proj *v1alpha1.AppProject, obj *unstructured.Unstructured, scope ResourceScope, cluster *v1alpha1.Cluster, lister ProjectClusterLister) error {
	if obj == nil {
		return fmt.Errorf("no object to verify")
	}
	gvk := obj.GroupVersionKind()
	if !scope.Known {
		return fmt.Errorf("failed to get API resource info for %s/%s: unable to verify permissions", gvk.Group, gvk.Kind)
	}
	projName := ""
	if proj != nil {
		projName = proj.Name
	}
	if !IsGroupKindNamePermitted(proj, gvk.GroupKind(), obj.GetName(), scope.Namespaced) {
		return fmt.Errorf("resource %s:%s is not permitted in project %s", gvk.Group, gvk.Kind, projName)
	}
	if scope.Namespaced {
		ok, err := IsDestinationPermitted(ctx, proj, DestinationOf(cluster, obj.GetNamespace()), lister)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("namespace %v is not permitted in project '%s'", obj.GetNamespace(), projName)
		}
	}
	return nil
}

// MissingNamespaceMessage is the InvalidSpecError message for a namespaced target object that
// still has no namespace after defaulting from spec.destination.namespace (APP-005).
func MissingNamespaceMessage(name string, gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("Namespace for %s %s is missing.", name, gvk.String())
}
