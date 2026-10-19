package deploy_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// inNamespace returns copies of namespace-scoped objects moved to ns (what a kustomize
// `namespace:` overlay does): metadata.namespace and the namespace of RoleBinding subjects.
func inNamespace(t *testing.T, objs []object, ns string) []object {
	t.Helper()
	out := make([]object, 0, len(objs))
	for _, o := range objs {
		u := o.u.DeepCopy()
		if u.GetNamespace() == "" {
			t.Fatalf("%s: cluster-scoped object in a namespace-scoped variant", o.id())
		}
		u.SetNamespace(ns)
		if u.GetKind() == "RoleBinding" {
			subjects, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			for _, s := range subjects {
				if m := s.(map[string]any); m["kind"] == rbacv1.ServiceAccountKind {
					m["namespace"] = ns
				}
			}
			if err := unstructured.SetNestedSlice(u.Object, subjects, "subjects"); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := yaml.Marshal(u.Object)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, object{raw: raw, u: u})
	}
	return out
}

// TestNamespaceInstallPermissions pins who can apply namespace-install.yaml after a cluster
// administrator installed crds.yaml (manifests.md "Prerequisites", installation.md). The
// namespace's `admin` role is not enough: RBAC escalation prevention refuses the Roles that
// grant the Patrol CRD verbs and the controller's and server's namespace-wide `*` rules. A
// namespace admin who also holds `escalate` and `bind` on Roles of the namespace, or a user
// bound to the `cluster-admin` ClusterRole in the namespace only, can.
func TestNamespaceInstallPermissions(t *testing.T) {
	t.Parallel()
	requireManifests(t)
	k := freshEnv(t, envOptions{name: "persona", crds: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	objs, err := readObjects(filepath.Join(manifestsDir(), "namespace-install.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// The admin ClusterRole aggregates its rules through kube-controller-manager, which
	// envtest does not run: build its effective rules from the aggregated ClusterRoles.
	var adminRules []rbacv1.PolicyRule
	for _, name := range []string{"system:aggregate-to-admin", "system:aggregate-to-edit", "system:aggregate-to-view"} {
		cr, err := k.kube.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		adminRules = append(adminRules, cr.Rules...)
	}
	if _, err := k.kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "persona-namespace-admin"}, Rules: adminRules,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	bind := func(t *testing.T, ns, user, kind, role string) {
		t.Helper()
		if _, err := k.kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: user + "-" + strings.ToLower(kind) + "-" + strings.ReplaceAll(role, ":", "-")},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kind, Name: role},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// allowed waits until the RBAC authorizer answers the SubjectAccessReview as wanted.
	allowed := func(t *testing.T, ns, user, verb, resource string, want bool) {
		t.Helper()
		waitFor(t, 30*time.Second, user+" "+verb+" "+resource, func() (bool, string) {
			r, err := k.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: user, Groups: []string{"system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: rbacv1.GroupName, Resource: resource},
			}}, metav1.CreateOptions{})
			if err != nil {
				return false, err.Error()
			}
			return r.Status.Allowed == want, "allowed=" + map[bool]string{true: "true", false: "false"}[r.Status.Allowed]
		})
	}
	// applyAs server-side applies the variant moved to ns as user and returns the refused
	// objects with their errors.
	applyAs := func(t *testing.T, ns, user string) map[string]error {
		t.Helper()
		cfg := rest.CopyConfig(k.cfg)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:authenticated"}}
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		failed := map[string]error{}
		for _, o := range inNamespace(t, objs, ns) {
			if err := k.apply(ctx, kube.Discovery().RESTClient(), o, ns, applyOptions{fieldManager: "kubectl", force: true}); err != nil {
				failed[o.id()] = err
			}
		}
		return failed
	}

	t.Run("namespace admin is refused", func(t *testing.T) {
		const ns, user = "persona-admin", "ns-admin"
		if err := k.ensureNamespace(ctx, ns, nil); err != nil {
			t.Fatal(err)
		}
		bind(t, ns, user, "ClusterRole", "persona-namespace-admin")
		allowed(t, ns, user, "create", "roles", true)
		failed := applyAs(t, ns, user)
		var escalation []string
		for id, err := range failed {
			kind := strings.SplitN(id, "/", 2)[0]
			switch {
			case kind == "Role" && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "attempting to grant RBAC permissions not currently held"):
				escalation = append(escalation, id)
			case kind == "RoleBinding":
				// Refused because its Role was not created (or for lack of `bind`).
			default:
				t.Errorf("%s refused for another reason: %v", id, err)
			}
		}
		slices.Sort(escalation)
		t.Logf("refused by RBAC escalation prevention: %d Roles %v; %d objects refused in total", len(escalation), escalation, len(failed))
		if len(escalation) == 0 {
			t.Errorf("a namespace admin applied every Role of namespace-install.yaml; update the documented installer permissions")
		}
	})

	t.Run("namespace admin with escalate and bind on Roles", func(t *testing.T) {
		const ns, user = "persona-escalate", "ns-admin-escalate"
		if err := k.ensureNamespace(ctx, ns, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := k.kube.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "rbac-escalate"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"roles"}, Verbs: []string{"escalate", "bind"}}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		bind(t, ns, user, "ClusterRole", "persona-namespace-admin")
		bind(t, ns, user, "Role", "rbac-escalate")
		allowed(t, ns, user, "escalate", "roles", true)
		for id, err := range applyAs(t, ns, user) {
			t.Errorf("%s refused: %v", id, err)
		}
	})

	t.Run("cluster-admin bound in the namespace", func(t *testing.T) {
		const ns, user = "persona-owner", "ns-owner"
		if err := k.ensureNamespace(ctx, ns, nil); err != nil {
			t.Fatal(err)
		}
		bind(t, ns, user, "ClusterRole", "cluster-admin")
		allowed(t, ns, user, "create", "roles", true)
		for id, err := range applyAs(t, ns, user) {
			t.Errorf("%s refused: %v", id, err)
		}
		// Rights end at the namespace.
		allowed(t, "default", user, "create", "roles", false)
	})
}
