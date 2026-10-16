package admin

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// patrolKind describes one Patrol CD kind handled by export, import and migrate-group.
type patrolKind struct {
	Kind     string
	Resource string
	// AppScoped kinds may live in namespaces other than the control-plane namespace
	// (Applications and ApplicationSets in any namespace).
	AppScoped bool
}

// Patrol kinds in dependency order: owners before the objects that reference them.
var (
	kindAppProject           = patrolKind{Kind: "AppProject", Resource: "appprojects"}
	kindSCMProvider          = patrolKind{Kind: "SCMProvider", Resource: "scmproviders"}
	kindRepository           = patrolKind{Kind: "Repository", Resource: "repositories"}
	kindRepositoryCredential = patrolKind{Kind: "RepositoryCredential", Resource: "repositorycredentials"}
	kindCluster              = patrolKind{Kind: "Cluster", Resource: "clusters"}
	kindApplicationSet       = patrolKind{Kind: "ApplicationSet", Resource: "applicationsets", AppScoped: true}
	kindApplication          = patrolKind{Kind: "Application", Resource: "applications", AppScoped: true}
	kindOperation            = patrolKind{Kind: "Operation", Resource: "operations", AppScoped: true}

	// configKinds are exported and imported as configuration (Operations are execution
	// records and are handled separately).
	configKinds = []patrolKind{
		kindAppProject, kindSCMProvider, kindRepository, kindRepositoryCredential, kindCluster,
		kindApplicationSet, kindApplication,
	}
	allPatrolKinds = append(append([]patrolKind{}, configKinds...), kindOperation)
)

// patrolKindByName returns the Patrol kind named kind.
func patrolKindByName(kind string) (patrolKind, bool) {
	for _, k := range allPatrolKinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return patrolKind{}, false
}

func (k patrolKind) gvr(group string) schema.GroupVersionResource {
	return patrolGVR(group, k.Resource)
}

// currentGroup is the API group of the installed Patrol CD kinds.
const currentGroup = v1alpha1.GroupName

// argoGroup is the API group of Argo CD kinds.
const argoGroup = "argoproj.io"

// objectGroup returns the API group of obj.
func objectGroup(obj *unstructured.Unstructured) string {
	return obj.GroupVersionKind().Group
}

// objectRef formats "<group>/<kind> <name>" as upstream import messages do.
func objectRef(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return gvk.Group + "/" + gvk.Kind + " " + obj.GetName()
}

// isArgoGroup reports whether group is Argo CD's API group.
func isArgoGroup(group string) bool {
	return group == argoGroup || strings.HasSuffix(group, "."+argoGroup)
}
