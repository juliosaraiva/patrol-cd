// Package v1alpha1 contains the Patrol CD API types for group cd.patrol.example, version v1alpha1.
//
// The kinds mirror the Argo CD v3.5.5 data model where semantics match (field names and JSON tags
// are kept so manifests migrate by changing apiVersion/kind/keys only), with the differences recorded
// in ADR-0003: sync requests live in the Operation kind instead of Application.operation, and
// repositories, credential templates, clusters and SCM providers are typed kinds that reference
// Secrets instead of being labeled Secrets.
//
// +kubebuilder:object:generate=true
// +groupName=cd.patrol.example
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the API group of every Patrol CD kind. See ADR-0002 before changing it.
	GroupName = "cd.patrol.example"
	// Version is the only served and storage version.
	Version = "v1alpha1"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}
	// SchemeGroupVersion is GroupVersion under the name Kubernetes code generators expect.
	SchemeGroupVersion = GroupVersion

	// SchemeBuilder registers the Go types of this package with a runtime.Scheme. It is the
	// apimachinery builder rather than controller-runtime's (deprecated) one, so importing the API
	// package does not pull in controller-runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// knownTypes is filled by the init functions of the kind files.
	knownTypes []runtime.Object
)

// register records kinds to add to schemes; called from init functions.
func register(objects ...runtime.Object) {
	knownTypes = append(knownTypes, objects...)
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, knownTypes...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Resource takes an unqualified resource name and returns a group-qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

// Kind takes an unqualified kind and returns a group-qualified GroupKind.
func Kind(kind string) schema.GroupKind {
	return GroupVersion.WithKind(kind).GroupKind()
}
