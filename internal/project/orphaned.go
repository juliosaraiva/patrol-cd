package project

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// OrphanedResourcesEnabled reports whether orphaned-resource monitoring is on for proj
// (spec.orphanedResources present, even empty).
func OrphanedResourcesEnabled(proj *v1alpha1.AppProject) bool {
	return proj != nil && proj.Spec.OrphanedResources != nil
}

// OrphanedResourcesWarn reports whether Applications of proj get the OrphanedResourceWarning
// condition when orphans exist (warn defaults to false).
func OrphanedResourcesWarn(proj *v1alpha1.AppProject) bool {
	return proj != nil && proj.Spec.OrphanedResources.IsWarn()
}

// IsKnownOrphanedResourceExclusion reports whether a top-level resource is never reported as
// orphaned (upstream isKnownOrphanedResourceExclusion): Service "kubernetes" in namespace
// "default", any ServiceAccount "default", any ConfigMap "kube-root-ca.crt", and the
// spec.orphanedResources.ignore entries. An ignore entry with an empty kind matches every
// kind (else kind is a glob); its group is a glob matched against the resource group, so an
// omitted group only matches the core group; an empty name matches every name (else a glob).
func IsKnownOrphanedResourceExclusion(proj *v1alpha1.AppProject, key v1alpha1.ResourceKey) bool {
	if key.Namespace == "default" && key.Group == "" && key.Kind == "Service" && key.Name == "kubernetes" {
		return true
	}
	if key.Group == "" && key.Kind == "ServiceAccount" && key.Name == "default" {
		return true
	}
	if key.Group == "" && key.Kind == "ConfigMap" && key.Name == "kube-root-ca.crt" {
		return true
	}
	if proj == nil || proj.Spec.OrphanedResources == nil {
		return false
	}
	for _, item := range proj.Spec.OrphanedResources.Ignore {
		if item.Kind != "" && !globMatch(item.Kind, key.Kind) {
			continue
		}
		if !globMatch(item.Group, key.Group) {
			continue
		}
		if item.Name == "" || globMatch(item.Name, key.Name) {
			return true
		}
	}
	return false
}

// IsOrphanCandidate reports whether a top-level live resource of an Application's destination
// namespace, not managed by the Application, is reported as orphaned before hierarchy and
// ownership checks: it must be namespaced, of a kind the project permits for namespaced
// resources, and not excluded.
func IsOrphanCandidate(proj *v1alpha1.AppProject, key v1alpha1.ResourceKey) bool {
	return key.Namespace != "" &&
		IsGroupKindNamePermitted(proj, schema.GroupKind{Group: key.Group, Kind: key.Kind}, key.Name, true) &&
		!IsKnownOrphanedResourceExclusion(proj, key)
}

// OrphanedResourceWarningMessage is the OrphanedResourceWarning condition message.
func OrphanedResourceWarningMessage(count int) string {
	return fmt.Sprintf("Application has %d orphaned resources", count)
}
