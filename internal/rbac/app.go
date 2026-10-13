package rbac

import "strings"

// AppObject returns the RBAC object of an Application or ApplicationSet:
// "<project>/<name>" when the object lives in the control-plane namespace (or the
// namespace is unknown), "<project>/<namespace>/<name>" otherwise (apps in any
// namespace).
func AppObject(controlPlaneNamespace, project, namespace, name string) string {
	if controlPlaneNamespace != "" && namespace != "" && namespace != controlPlaneNamespace {
		return project + "/" + namespace + "/" + name
	}
	return project + "/" + name
}

// SubResourceAction returns the fine-grained application action for a managed resource:
// "<verb>/<group>/<kind>/<namespace>/<name>" (verb is "update" or "delete"). The group
// is empty for core resources.
func SubResourceAction(verb, group, kind, namespace, name string) string {
	return strings.Join([]string{verb, group, kind, namespace, name}, "/")
}

// CustomAction returns the action name for running a resource action:
// "action/<group>/<kind>/<action-name>" ("action//Pod/restart" for core kinds).
func CustomAction(group, kind, action string) string {
	return strings.Join([]string{ActionAction, group, kind, action}, "/")
}

// AppResource identifies a resource managed by an application.
type AppResource struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// EnforceAppResource authorizes verb on a resource managed by the application whose
// RBAC object is appObject.
//
// For "update" and "delete" the fine-grained action "<verb>/<group>/<kind>/<ns>/<name>"
// is checked. When inheritFromApp is true (upstream
// server.rbac.disableApplicationFineGrainedRBACInheritance=false, the pre-v3 behavior)
// an allow for the plain verb on the application is tried first and suffices, so a deny
// on the sub-resource cannot restrict it; when false (the default) only the
// fine-grained action counts, so "update" on the application does not grant updates of
// its resources and a deny of "update/*" wins. Other verbs are checked as given.
func (e *Enforcer) EnforceAppResource(s Subjects, verb, appObject string, r AppResource, inheritFromApp bool) Decision {
	if verb != ActionUpdate && verb != ActionDelete {
		return e.Enforce(s, ResourceApplications, verb, appObject)
	}
	fine := SubResourceAction(verb, r.Group, r.Kind, r.Namespace, r.Name)
	if inheritFromApp {
		if d := e.Enforce(s, ResourceApplications, verb, appObject); d.Allowed {
			return d
		}
	}
	return e.Enforce(s, ResourceApplications, fine, appObject)
}
