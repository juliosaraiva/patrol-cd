package clusters

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func createNamespace(t *testing.T, cfg *rest.Config, prefix string) string {
	t.Helper()
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	name := prefix + "-" + utilrand.String(6)
	_, err = kube.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return name
}

// legacyToken builds a JWT shaped like a legacy service-account token.
func legacyToken(key []byte, namespace, secret, sa string) (string, error) {
	claims := jwt.MapClaims{
		"iss":                                    "kubernetes/serviceaccount",
		"sub":                                    ServiceAccountUsername(namespace, sa),
		"kubernetes.io/serviceaccount/namespace": namespace,
		"kubernetes.io/serviceaccount/secret.name":          secret,
		"kubernetes.io/serviceaccount/service-account.name": sa,
		"kubernetes.io/serviceaccount/service-account.uid":  "uid-" + sa,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
}

// startTokenController emulates the kube-controller-manager token controller (envtest runs
// none): service-account token Secrets in namespace get data.token, ca.crt and namespace.
func startTokenController(t *testing.T, kube kubernetes.Interface, namespace string) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			list, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeServiceAccountToken)})
			if err != nil {
				continue
			}
			for i := range list.Items {
				s := &list.Items[i]
				if len(s.Data[corev1.ServiceAccountTokenKey]) > 0 {
					continue
				}
				sa := s.Annotations[corev1.ServiceAccountNameKey]
				if s.Data == nil {
					s.Data = map[string][]byte{}
				}
				tok, err := legacyToken(key, namespace, s.Name, sa)
				if err != nil {
					continue
				}
				s.Data[corev1.ServiceAccountTokenKey] = []byte(tok)
				s.Data["ca.crt"] = []byte("fake-ca")
				s.Data["namespace"] = []byte(namespace)
				_, _ = kube.CoreV1().Secrets(namespace).Update(ctx, s, metav1.UpdateOptions{})
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func canI(t *testing.T, kube kubernetes.Interface, namespace, sa, verb, resource, targetNS string) bool {
	t.Helper()
	sar, err := kube.AuthorizationV1().SubjectAccessReviews().Create(context.Background(), &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               ServiceAccountUsername(namespace, sa),
			Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
			ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Resource: resource, Namespace: targetNS},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return sar.Status.Allowed
}

var fastBootstrap = &BootstrapOptions{TokenTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond}

// TestBootstrapEnvtest covers REPO-106/107/103: the manager identity is installed exactly,
// idempotently, in cluster-wide and namespaced form, its token rotated and uninstalled.
func TestBootstrapEnvtest(t *testing.T) {
	cfg := envConfig(t)
	ctx := context.Background()
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	sysNS := createNamespace(t, cfg, "system")
	startTokenController(t, kube, sysNS)

	_, err = EnsureManagerServiceAccount(ctx, kube, sysNS, true, []string{"a"}, fastBootstrap)
	require.ErrorContains(t, err, "mutually exclusive")
	_, err = EnsureManagerServiceAccount(ctx, kube, sysNS, false, nil, fastBootstrap)
	require.ErrorContains(t, err, "at least one namespace")

	// Cluster-wide.
	token, err := EnsureManagerServiceAccount(ctx, kube, sysNS, true, nil, fastBootstrap)
	require.NoError(t, err)
	claims, err := ParseServiceAccountToken(token)
	require.NoError(t, err)
	assert.Equal(t, ManagerServiceAccount+ManagerTokenSecretSuffix, claims.SecretName)
	assert.Equal(t, sysNS, claims.Namespace)

	sa, err := kube.CoreV1().ServiceAccounts(sysNS).Get(ctx, ManagerServiceAccount, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "patrol-cd", sa.Labels["app.kubernetes.io/part-of"])
	cr, err := kube.RbacV1().ClusterRoles().Get(ctx, ManagerRole, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagerClusterPolicyRules(), cr.Rules)
	crb, err := kube.RbacV1().ClusterRoleBindings().Get(ctx, ManagerRoleBinding, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ManagerRole}, crb.RoleRef)
	assert.Equal(t, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ManagerServiceAccount, Namespace: sysNS}}, crb.Subjects)
	tokSecret, err := kube.CoreV1().Secrets(sysNS).Get(ctx, ManagerServiceAccount+ManagerTokenSecretSuffix, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeServiceAccountToken, tokSecret.Type)
	assert.Equal(t, ManagerServiceAccount, tokSecret.Annotations[corev1.ServiceAccountNameKey])
	require.Eventually(t, func() bool { return canI(t, kube, sysNS, ManagerServiceAccount, "list", "nodes", "") }, waitTimeout, pollInterval)

	// Re-running converges: a tampered ClusterRole is restored, the token is reused.
	cr.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}
	_, err = kube.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
	require.NoError(t, err)
	again, err := EnsureManagerServiceAccount(ctx, kube, sysNS, true, nil, fastBootstrap)
	require.NoError(t, err)
	assert.Equal(t, token, again)
	cr, err = kube.RbacV1().ClusterRoles().Get(ctx, ManagerRole, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagerClusterPolicyRules(), cr.Rules)

	// Token rotation (REPO-103).
	var persisted string
	newToken, err := RotateManagerToken(ctx, kube, token, func(_ context.Context, tok string) error {
		persisted = tok
		return nil
	}, fastBootstrap)
	require.NoError(t, err)
	assert.Equal(t, newToken, persisted)
	assert.NotEqual(t, token, newToken)
	newClaims, err := ParseServiceAccountToken(newToken)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(newClaims.SecretName, "patrol-manager-long-lived-"), newClaims.SecretName)
	assert.NotEqual(t, claims.SecretName, newClaims.SecretName)
	_, err = kube.CoreV1().Secrets(sysNS).Get(ctx, claims.SecretName, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "old token secret deleted: %v", err)
	sa, err = kube.CoreV1().ServiceAccounts(sysNS).Get(ctx, ManagerServiceAccount, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []corev1.ObjectReference{{Name: newClaims.SecretName}}, sa.Secrets)

	// A failing persist keeps the current token and removes the unused Secret.
	before, err := kube.CoreV1().Secrets(sysNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	_, err = RotateManagerToken(ctx, kube, newToken, func(context.Context, string) error { return errors.New("store failed") }, fastBootstrap)
	require.ErrorContains(t, err, "persist rotated token: store failed")
	_, err = kube.CoreV1().Secrets(sysNS).Get(ctx, newClaims.SecretName, metav1.GetOptions{})
	require.NoError(t, err, "current token secret kept")
	require.Eventually(t, func() bool {
		after, err := kube.CoreV1().Secrets(sysNS).List(ctx, metav1.ListOptions{})
		return err == nil && len(after.Items) == len(before.Items)
	}, waitTimeout, pollInterval, "the new secret was deleted")

	// Uninstall removes the cluster-wide objects and tolerates a second run.
	require.NoError(t, UninstallManager(ctx, kube, sysNS, nil, fastBootstrap))
	require.NoError(t, UninstallManager(ctx, kube, sysNS, nil, fastBootstrap))
	_, err = kube.RbacV1().ClusterRoles().Get(ctx, ManagerRole, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = kube.RbacV1().ClusterRoleBindings().Get(ctx, ManagerRoleBinding, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = kube.CoreV1().ServiceAccounts(sysNS).Get(ctx, ManagerServiceAccount, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	// Namespaced mode installs a Role and RoleBinding per namespace and no ClusterRole.
	nsA, nsB, nsC := createNamespace(t, cfg, "apps-a"), createNamespace(t, cfg, "apps-b"), createNamespace(t, cfg, "apps-c")
	_, err = EnsureManagerServiceAccount(ctx, kube, sysNS, false, []string{nsA, " ", nsB}, fastBootstrap)
	require.NoError(t, err)
	for _, ns := range []string{nsA, nsB} {
		role, err := kube.RbacV1().Roles(ns).Get(ctx, ManagerRole, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, ManagerNamespacePolicyRules(), role.Rules)
		rb, err := kube.RbacV1().RoleBindings(ns).Get(ctx, ManagerRoleBinding, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ManagerRole}, rb.RoleRef)
		assert.Equal(t, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ManagerServiceAccount, Namespace: sysNS}}, rb.Subjects)
	}
	_, err = kube.RbacV1().ClusterRoles().Get(ctx, ManagerRole, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "no ClusterRole in namespaced mode")
	require.Eventually(t, func() bool { return canI(t, kube, sysNS, ManagerServiceAccount, "delete", "pods", nsA) }, waitTimeout, pollInterval)
	assert.False(t, canI(t, kube, sysNS, ManagerServiceAccount, "list", "pods", nsC))
	assert.False(t, canI(t, kube, sysNS, ManagerServiceAccount, "list", "nodes", ""))

	require.NoError(t, UninstallManager(ctx, kube, sysNS, []string{nsA, nsB}, fastBootstrap))
	_, err = kube.RbacV1().Roles(nsA).Get(ctx, ManagerRole, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

// TestBootstrapTokenTimeout: without a token controller the wait fails with upstream's message.
func TestBootstrapTokenTimeout(t *testing.T) {
	cfg := envConfig(t)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns := createNamespace(t, cfg, "notoken")
	_, err = ServiceAccountBearerToken(context.Background(), kube, ns, "patrol-manager", &BootstrapOptions{TokenTimeout: 300 * time.Millisecond, PollInterval: 50 * time.Millisecond})
	require.ErrorContains(t, err, `failed to get token for serviceaccount "patrol-manager": `)

	// A Secret with the token name that belongs to another service account is rejected.
	_, err = kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "other-long-lived-token", Annotations: map[string]string{corev1.ServiceAccountNameKey: "someone-else"}},
		Type:       corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = ServiceAccountBearerToken(context.Background(), kube, ns, "other", &BootstrapOptions{TokenTimeout: time.Second})
	require.ErrorContains(t, err, `is not a token secret of serviceaccount "other"`)
}

func TestRotateManagerTokenRejectsNonLegacyTokens(t *testing.T) {
	t.Parallel()
	_, err := RotateManagerToken(context.Background(), nil, "", nil, nil)
	require.ErrorIs(t, err, ErrNoBearerToken)
	bound, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "system:serviceaccount:a:b"}).SignedString([]byte("k"))
	require.NoError(t, err)
	_, err = RotateManagerToken(context.Background(), nil, bound, nil, nil)
	require.ErrorIs(t, err, ErrNotLegacyToken)
	_, err = RotateManagerToken(context.Background(), nil, "not-a-jwt", nil, nil)
	require.ErrorContains(t, err, "failed to parse service account token")
}
