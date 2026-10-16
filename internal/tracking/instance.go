package tracking

import (
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// App identifies an Application by its metadata.name and metadata.namespace.
type App struct {
	Name      string
	Namespace string
}

// AppOf returns the identity of a. A nil Application yields the zero App.
func AppOf(a *v1alpha1.Application) App {
	if a == nil {
		return App{}
	}
	return App{Name: a.Name, Namespace: a.Namespace}
}

// QualifiedName returns "<namespace>/<name>", or the name when the namespace is empty. It is
// the form used in user-facing messages (e.g. SharedResourceWarning).
func (a App) QualifiedName() string {
	if a.Namespace == "" {
		return a.Name
	}
	return a.Namespace + "/" + a.Name
}

// InstanceName returns the instance name of the Application: name when namespace is empty or
// equals controlPlaneNamespace, "<namespace>_<name>" otherwise.
func InstanceName(name, namespace, controlPlaneNamespace string) string {
	if namespace == "" || namespace == controlPlaneNamespace {
		return name
	}
	return namespace + "_" + name
}

// ParseInstanceName splits an instance name on its first '_' into name and namespace. An
// unqualified instance name yields defaultNamespace.
//
// Application names cannot contain '_', but an app named "a_b" would be indistinguishable
// from app "b" in namespace "a"; upstream has the same ambiguity.
func ParseInstanceName(instanceName, defaultNamespace string) (name, namespace string) {
	if ns, n, ok := strings.Cut(instanceName, "_"); ok {
		return n, ns
	}
	return instanceName, defaultNamespace
}

// InstanceNameFromQualified converts a qualified name ("<namespace>/<name>" or "<name>") to
// an instance name; an unqualified name is taken to be in defaultNamespace.
func InstanceNameFromQualified(qualifiedName, defaultNamespace string) string {
	name, ns := qualifiedName, defaultNamespace
	if before, after, ok := strings.Cut(qualifiedName, "/"); ok {
		ns, name = before, after
	}
	return InstanceName(name, ns, defaultNamespace)
}

// DisplayName converts an instance name found in tracking metadata into the form used in
// SharedResourceWarning messages: every '_' becomes '/' ("team_app" -> "team/app"), exactly
// as upstream does.
func DisplayName(instanceName string) string {
	return strings.ReplaceAll(instanceName, "_", "/")
}
