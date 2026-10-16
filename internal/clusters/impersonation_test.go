package clusters

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestImpersonationHeader: ImpersonatingConfig on a tuned config sends Impersonate-User once.
func TestImpersonationHeader(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)
	cfg, err := RestConfig(context.Background(), cluster("i", srv.URL), map[string][]byte{"caData": srv.caPEM(), "bearerToken": []byte("admin")}, Options{})
	require.NoError(t, err)
	imp, err := ImpersonatingConfig(cfg, ServiceAccountUsername("apps", "deployer"))
	require.NoError(t, err)
	assert.Empty(t, cfg.Impersonate.UserName, "base config is not modified")
	_, err = Probe(context.Background(), imp)
	require.NoError(t, err)
	h := srv.lastHeader(t)
	assert.Equal(t, []string{"system:serviceaccount:apps:deployer"}, h.Values("Impersonate-User"))
	assert.Equal(t, "Bearer admin", h.Get("Authorization"))

	_, err = ImpersonatingConfig(nil, "u")
	require.Error(t, err)
	_, err = ImpersonatingConfig(cfg, " ")
	require.Error(t, err)
}

// TestImpersonationEnvtest runs impersonation against a real API server with RBAC: the admin
// identity impersonating a service account gets exactly that account's permissions.
func TestImpersonationEnvtest(t *testing.T) {
	admin := envConfig(t)
	ctx := context.Background()
	kube, err := kubernetes.NewForConfig(admin)
	require.NoError(t, err)
	ns := "imp-" + rand.String(6)
	other := "imp-other-" + rand.String(6)
	for _, n := range []string{ns, other} {
		_, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err = kube.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "deployer"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kube.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "cm-reader"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cm-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "cm-reader"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "deployer", Namespace: ns}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	imp, err := ImpersonatingConfig(admin, ServiceAccountUsername(ns, "deployer"))
	require.NoError(t, err)
	ikube, err := kubernetes.NewForConfig(imp)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := ikube.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		return err == nil
	}, waitTimeout, pollInterval, "RBAC grants list in the bound namespace")
	_, err = ikube.CoreV1().ConfigMaps(other).List(ctx, metav1.ListOptions{})
	require.True(t, apierrors.IsForbidden(err), "expected Forbidden outside the namespace, got %v", err)
	_, err = ikube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	require.True(t, apierrors.IsForbidden(err), "expected Forbidden for secrets, got %v", err)
	_, err = kubernetes.NewForConfigOrDie(rest.CopyConfig(admin)).CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "the base identity is unaffected")
}
