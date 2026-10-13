package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cpNamespace, project, namespace, name, want string
	}{
		{"patrol", "default", "patrol", "web", "default/web"},
		{"patrol", "default", "", "web", "default/web"},
		{"patrol", "default", "team-ns", "web", "default/team-ns/web"},
		{"", "default", "team-ns", "web", "default/web"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, AppObject(tt.cpNamespace, tt.project, tt.namespace, tt.name))
	}
	assert.Equal(t, "update/apps/Deployment/ns/web", SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", "web"))
	assert.Equal(t, "delete//Pod/ns/web-0", SubResourceAction(ActionDelete, "", "Pod", "ns", "web-0"))
	assert.Equal(t, "action/extensions/DaemonSet/restart", CustomAction("extensions", "DaemonSet", "restart"))
	assert.Equal(t, "action//Pod/maintenance-off", CustomAction("", "Pod", "maintenance-off"))
}

// The policies of the "Fine-grained Permissions for update/delete action" section of the
// RBAC documentation.
func TestApplicationFineGrainedSubResources(t *testing.T) {
	t.Parallel()
	pod := AppResource{Group: "", Kind: "Pod", Namespace: "prod", Name: "web-0"}
	deploy := AppResource{Group: "apps", Kind: "Deployment", Namespace: "prod", Name: "web"}
	tests := []struct {
		name     string
		policy   string
		verb     string
		resource *AppResource // nil: the application itself
		inherit  bool
		allowed  bool
	}{
		{name: "delete only pods: pod", policy: "p, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, resource: &pod, allowed: true},
		{name: "delete only pods: deployment", policy: "p, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, resource: &deploy, allowed: false},
		{name: "delete only pods: the app", policy: "p, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, allowed: false},

		{name: "update all resources but not the app: resource", policy: "p, u, applications, update/*, default/prod-app, allow", verb: ActionUpdate, resource: &deploy, allowed: true},
		{name: "update all resources but not the app: app", policy: "p, u, applications, update/*, default/prod-app, allow", verb: ActionUpdate, allowed: false},

		{name: "deny app delete, allow pods: pod", policy: "p, u, applications, delete, default/prod-app, deny\np, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, resource: &pod, allowed: true},
		{name: "deny app delete, allow pods: app", policy: "p, u, applications, delete, default/prod-app, deny\np, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, allowed: false},

		{name: "allow app update, deny sub-resources: app", policy: "p, u, applications, update, default/prod-app, allow\np, u, applications, update/*, default/prod-app, deny", verb: ActionUpdate, allowed: true},
		{name: "allow app update, deny sub-resources: resource", policy: "p, u, applications, update, default/prod-app, allow\np, u, applications, update/*, default/prod-app, deny", verb: ActionUpdate, resource: &deploy, allowed: false},

		// v3 default: "update" on the application does not extend to its resources.
		{name: "no inheritance", policy: "p, u, applications, update, default/prod-app, allow", verb: ActionUpdate, resource: &deploy, allowed: false},
		// Legacy inheritance: the plain verb on the app suffices, even against a
		// sub-resource deny.
		{name: "inheritance", policy: "p, u, applications, update, default/prod-app, allow", verb: ActionUpdate, resource: &deploy, inherit: true, allowed: true},
		{name: "inheritance beats sub-resource deny", policy: "p, u, applications, delete, default/prod-app, allow\np, u, applications, delete/*/Pod/*, default/prod-app, deny", verb: ActionDelete, resource: &pod, inherit: true, allowed: true},
		{name: "inheritance falls back to fine-grained", policy: "p, u, applications, delete/*/Pod/*/*, default/prod-app, allow", verb: ActionDelete, resource: &pod, inherit: true, allowed: true},
		// Glob does not treat '/' as a separator: a lowercase kind segment can match a
		// namespace position.
		{name: "slash is not a separator", policy: "p, u, applications, delete/*/kind/*, default/prod-app, allow", verb: ActionDelete, resource: &AppResource{Group: "g", Kind: "Other", Namespace: "kind", Name: "x"}, allowed: true},
		// Other verbs are checked as given.
		{name: "get resource", policy: "p, u, applications, get, default/*, allow", verb: ActionGet, resource: &deploy, allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnforcer(t, "", tt.policy, "", "")
			var d Decision
			if tt.resource == nil {
				d = e.Enforce(user("u"), ResourceApplications, tt.verb, "default/prod-app")
			} else {
				d = e.EnforceAppResource(user("u"), tt.verb, "default/prod-app", *tt.resource, tt.inherit)
			}
			assert.Equal(t, tt.allowed, d.Allowed, "%s (%s)", d.Reason, d.MatchedPolicy)
		})
	}
}
