package applications_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	k8srest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// installOperationPolicy installs deploy/crds/operations-admission-policy.yaml (cluster
// scoped; it constrains only non-admin identities, so other tests are unaffected).
func installOperationPolicy(t *testing.T, e *testEnv) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "..", "deploy", "crds", "operations-admission-policy.yaml"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	policy := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, dec.Decode(policy))
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, dec.Decode(binding))
	for _, obj := range []client.Object{policy, binding} {
		if err := e.kube.Create(e.ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}
}

// The handlers create Operations as patrol-server's service account and set
// spec.initiatedBy from the verified caller: the Operation admission policy admits that only
// with the "impersonate" verb on operations.
func TestOperationInitiatorAdmission(t *testing.T) {
	const sa = "patrol-server"
	e := newEnv(t, envOptions{clientConfig: func(e *testEnv, cfg *k8srest.Config) {
		cfg.Impersonate = k8srest.ImpersonationConfig{UserName: "system:serviceaccount:" + e.ns + ":" + sa}
	}})
	installOperationPolicy(t, e)
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"applications", "appprojects", "clusters", "repositories", "operations"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"applications", "operations"}, Verbs: []string{"create", "update", "patch"}},
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: e.ns}, Rules: rules}
	require.NoError(t, e.kube.Create(e.ctx, role))
	require.NoError(t, e.kube.Create(e.ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: e.ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: sa},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: e.ns}},
	}))
	e.createApp(e.newApp("adm", "proj-a"))
	syncer := e.client("syncer")

	// Without the verb the policy refuses the Operation: a deployment error (500), not the
	// caller's (the policy may need a moment to become active).
	require.Eventually(t, func() bool {
		_, err := syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "adm"})
		if err == nil {
			e.finishOperations(e.ns, "adm")
			return false
		}
		ae := apiErr(t, err)
		return ae.StatusCode == 500 && ae.Problem.Detail == "internal error"
	}, 20*time.Second, 200*time.Millisecond)
	assert.Empty(t, activeOps(e, "adm"))

	role.Rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"operations"}, Verbs: []string{v1alpha1.OperationInitiatorVerb}})
	require.NoError(t, e.kube.Update(e.ctx, role))
	var resp *apiclient.ApplicationOperationResponse
	require.Eventually(t, func() bool {
		var err error
		resp, err = syncer.SyncApplication(e.ctx, &apiclient.ApplicationSyncRequest{Name: "adm"})
		return err == nil
	}, 20*time.Second, 200*time.Millisecond)
	assert.Equal(t, "syncer", resp.Operation.Spec.InitiatedBy.Username)
	stored := activeOps(e, "adm")
	require.Len(t, stored, 1)
	assert.Equal(t, "syncer", stored[0].Spec.InitiatedBy.Username)
}
