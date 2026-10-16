package clusters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Names of the objects `patrol cluster add` installs in a destination cluster (upstream:
// argocd-manager, argocd-manager-role, argocd-manager-role-binding).
const (
	ManagerServiceAccount = "patrol-manager"
	ManagerRole           = "patrol-manager-role"
	ManagerRoleBinding    = "patrol-manager-role-binding"
	// ManagerTokenSecretSuffix is appended to the service account name for its token Secret.
	ManagerTokenSecretSuffix = "-long-lived-token"
	// DefaultSystemNamespace is the default namespace of the manager service account.
	DefaultSystemNamespace = "kube-system"

	// DefaultTokenTimeout bounds the wait for the token controller (upstream BearerTokenTimeout).
	DefaultTokenTimeout = 30 * time.Second
	// DefaultRequestTimeout bounds each API request (upstream ClusterAuthRequestTimeout).
	DefaultRequestTimeout = 10 * time.Second
	// tokenPollInterval is how often the token Secret is polled.
	tokenPollInterval = 500 * time.Millisecond
)

// ManagerClusterPolicyRules grant the manager full access cluster-wide.
func ManagerClusterPolicyRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		{NonResourceURLs: []string{"*"}, Verbs: []string{"*"}},
	}
}

// ManagerNamespacePolicyRules grant the manager full access within a namespace.
func ManagerNamespacePolicyRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
	}
}

// BootstrapOptions tunes EnsureManagerServiceAccount, RotateManagerToken and UninstallManager.
type BootstrapOptions struct {
	// ServiceAccount overrides ManagerServiceAccount.
	ServiceAccount string
	// TokenTimeout overrides DefaultTokenTimeout.
	TokenTimeout time.Duration
	// RequestTimeout overrides DefaultRequestTimeout.
	RequestTimeout time.Duration
	// PollInterval overrides the 500ms token poll interval.
	PollInterval time.Duration
	// Logger records created and updated objects. Nil discards.
	Logger *slog.Logger
}

func (o *BootstrapOptions) defaults() BootstrapOptions {
	out := BootstrapOptions{}
	if o != nil {
		out = *o
	}
	if out.ServiceAccount == "" {
		out.ServiceAccount = ManagerServiceAccount
	}
	if out.TokenTimeout <= 0 {
		out.TokenTimeout = DefaultTokenTimeout
	}
	if out.RequestTimeout <= 0 {
		out.RequestTimeout = DefaultRequestTimeout
	}
	if out.PollInterval <= 0 {
		out.PollInterval = tokenPollInterval
	}
	if out.Logger == nil {
		out.Logger = slog.New(slog.DiscardHandler)
	}
	return out
}

// managedLabels mark the installed objects.
func managedLabels() map[string]string {
	return map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}
}

// EnsureManagerServiceAccount installs the manager identity in the cluster behind kube and
// returns its bearer token. It creates the service account in namespace (existing is fine);
// with clusterWide, the ClusterRole patrol-manager-role (all resources and non-resource URLs)
// and its ClusterRoleBinding; otherwise a Role and RoleBinding with all resources in each of
// namespaces. Roles and bindings are created or updated, so re-running converges. Finally the
// service-account token Secret "<sa>-long-lived-token" is created (or reused) and polled until
// the token controller filled it, for at most TokenTimeout.
func EnsureManagerServiceAccount(ctx context.Context, kube kubernetes.Interface, namespace string, clusterWide bool, namespaces []string, opts *BootstrapOptions) (string, error) {
	o := opts.defaults()
	namespaces = NormalizeNamespaces(namespaces)
	switch {
	case namespace == "":
		return "", errors.New("bootstrap: the service account namespace is required")
	case clusterWide && len(namespaces) > 0:
		return "", errors.New("bootstrap: cluster-wide access and namespaces are mutually exclusive")
	case !clusterWide && len(namespaces) == 0:
		return "", errors.New("bootstrap: cluster-wide access or at least one namespace is required")
	}
	if err := ensureServiceAccount(ctx, kube, namespace, o); err != nil {
		return "", err
	}
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: o.ServiceAccount, Namespace: namespace}
	if clusterWide {
		if err := upsertClusterRole(ctx, kube, ManagerRole, ManagerClusterPolicyRules(), o); err != nil {
			return "", err
		}
		if err := upsertClusterRoleBinding(ctx, kube, ManagerRoleBinding, ManagerRole, subject, o); err != nil {
			return "", err
		}
	} else {
		for _, ns := range namespaces {
			if err := upsertRole(ctx, kube, ns, ManagerRole, ManagerNamespacePolicyRules(), o); err != nil {
				return "", err
			}
			if err := upsertRoleBinding(ctx, kube, ns, ManagerRoleBinding, ManagerRole, subject, o); err != nil {
				return "", err
			}
		}
	}
	return ServiceAccountBearerToken(ctx, kube, namespace, o.ServiceAccount, &o)
}

func withTimeout(ctx context.Context, o BootstrapOptions) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, o.RequestTimeout)
}

func ensureServiceAccount(ctx context.Context, kube kubernetes.Interface, namespace string, o BootstrapOptions) error {
	rctx, cancel := withTimeout(ctx, o)
	defer cancel()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: o.ServiceAccount, Namespace: namespace, Labels: managedLabels()}}
	_, err := kube.CoreV1().ServiceAccounts(namespace).Create(rctx, sa, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		o.Logger.Info("service account already exists", "namespace", namespace, "name", o.ServiceAccount)
		return nil
	case err != nil:
		return fmt.Errorf("failed to create service account %q in namespace %q: %w", o.ServiceAccount, namespace, err)
	}
	o.Logger.Info("service account created", "namespace", namespace, "name", o.ServiceAccount)
	return nil
}

// upsert creates an object, or reads the existing one and updates it when update reports a
// difference. Kind and name are used in errors and logs.
func upsert(ctx context.Context, o BootstrapOptions, kind, name string, create func(context.Context) error, update func(context.Context) (bool, error)) error {
	rctx, cancel := withTimeout(ctx, o)
	defer cancel()
	err := create(rctx)
	switch {
	case err == nil:
		o.Logger.Info("created", "kind", kind, "name", name)
		return nil
	case !apierrors.IsAlreadyExists(err):
		return fmt.Errorf("failed to create %s %q: %w", kind, name, err)
	}
	uctx, ucancel := withTimeout(ctx, o)
	defer ucancel()
	changed, err := update(uctx)
	if err != nil {
		return fmt.Errorf("failed to update %s %q: %w", kind, name, err)
	}
	if changed {
		o.Logger.Info("updated", "kind", kind, "name", name)
	}
	return nil
}

func upsertClusterRole(ctx context.Context, kube kubernetes.Interface, name string, rules []rbacv1.PolicyRule, o BootstrapOptions) error {
	api := kube.RbacV1().ClusterRoles()
	return upsert(ctx, o, "ClusterRole", name, func(ctx context.Context) error {
		_, err := api.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: managedLabels()}, Rules: rules}, metav1.CreateOptions{})
		return err
	}, func(ctx context.Context) (bool, error) {
		cur, err := api.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if equality.Semantic.DeepEqual(cur.Rules, rules) {
			return false, nil
		}
		cur.Rules = rules
		_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
		return err == nil, err
	})
}

func upsertRole(ctx context.Context, kube kubernetes.Interface, namespace, name string, rules []rbacv1.PolicyRule, o BootstrapOptions) error {
	api := kube.RbacV1().Roles(namespace)
	return upsert(ctx, o, "Role", namespace+"/"+name, func(ctx context.Context) error {
		_, err := api.Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: managedLabels()}, Rules: rules}, metav1.CreateOptions{})
		return err
	}, func(ctx context.Context) (bool, error) {
		cur, err := api.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if equality.Semantic.DeepEqual(cur.Rules, rules) {
			return false, nil
		}
		cur.Rules = rules
		_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
		return err == nil, err
	})
}

func upsertClusterRoleBinding(ctx context.Context, kube kubernetes.Interface, name, role string, subject rbacv1.Subject, o BootstrapOptions) error {
	api := kube.RbacV1().ClusterRoleBindings()
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role}
	desired := func() *rbacv1.ClusterRoleBinding {
		return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: managedLabels()}, RoleRef: ref, Subjects: []rbacv1.Subject{subject}}
	}
	return upsert(ctx, o, "ClusterRoleBinding", name, func(ctx context.Context) error {
		_, err := api.Create(ctx, desired(), metav1.CreateOptions{})
		return err
	}, func(ctx context.Context) (bool, error) {
		cur, err := api.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if cur.RoleRef != ref {
			// roleRef is immutable: replace the binding.
			if err := api.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			_, err := api.Create(ctx, desired(), metav1.CreateOptions{})
			return err == nil, err
		}
		if equality.Semantic.DeepEqual(cur.Subjects, []rbacv1.Subject{subject}) {
			return false, nil
		}
		cur.Subjects = []rbacv1.Subject{subject}
		_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
		return err == nil, err
	})
}

func upsertRoleBinding(ctx context.Context, kube kubernetes.Interface, namespace, name, role string, subject rbacv1.Subject, o BootstrapOptions) error {
	api := kube.RbacV1().RoleBindings(namespace)
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role}
	desired := func() *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: managedLabels()}, RoleRef: ref, Subjects: []rbacv1.Subject{subject}}
	}
	return upsert(ctx, o, "RoleBinding", namespace+"/"+name, func(ctx context.Context) error {
		_, err := api.Create(ctx, desired(), metav1.CreateOptions{})
		return err
	}, func(ctx context.Context) (bool, error) {
		cur, err := api.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if cur.RoleRef != ref {
			if err := api.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			_, err := api.Create(ctx, desired(), metav1.CreateOptions{})
			return err == nil, err
		}
		if equality.Semantic.DeepEqual(cur.Subjects, []rbacv1.Subject{subject}) {
			return false, nil
		}
		cur.Subjects = []rbacv1.Subject{subject}
		_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
		return err == nil, err
	})
}

// ServiceAccountBearerToken returns the token of service account sa: it creates the
// kubernetes.io/service-account-token Secret "<sa>-long-lived-token" unless it exists and
// waits until the token controller stored data.token. An existing Secret of that name that
// belongs to another service account is an error.
func ServiceAccountBearerToken(ctx context.Context, kube kubernetes.Interface, namespace, sa string, opts *BootstrapOptions) (string, error) {
	o := opts.defaults()
	name := sa + ManagerTokenSecretSuffix
	secrets := kube.CoreV1().Secrets(namespace)
	rctx, cancel := withTimeout(ctx, o)
	_, err := secrets.Create(rctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      managedLabels(),
			Annotations: map[string]string{corev1.ServiceAccountNameKey: sa},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	cancel()
	switch {
	case apierrors.IsAlreadyExists(err):
		o.Logger.Info("using existing bearer token secret", "namespace", namespace, "name", name, "serviceAccount", sa)
	case err != nil:
		return "", fmt.Errorf("failed to create secret %q for serviceaccount %q: %w", name, sa, err)
	default:
		o.Logger.Info("created bearer token secret", "namespace", namespace, "name", name, "serviceAccount", sa)
	}
	s, err := waitForToken(ctx, kube, namespace, name, sa, o.TokenTimeout, o)
	if err != nil {
		return "", fmt.Errorf("failed to get token for serviceaccount %q: %w", sa, err)
	}
	return string(s.Data[corev1.ServiceAccountTokenKey]), nil
}

// waitForToken polls the Secret until data.token is set.
func waitForToken(ctx context.Context, kube kubernetes.Interface, namespace, name, sa string, timeout time.Duration, o BootstrapOptions) (*corev1.Secret, error) {
	var out *corev1.Secret
	err := wait.PollUntilContextTimeout(ctx, o.PollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		rctx, cancel := withTimeout(ctx, o)
		defer cancel()
		s, err := kube.CoreV1().Secrets(namespace).Get(rctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get secret %q for serviceaccount %q: %w", name, sa, err)
		}
		if s.Type != corev1.SecretTypeServiceAccountToken || s.Annotations[corev1.ServiceAccountNameKey] != sa {
			return false, fmt.Errorf("secret %q is not a token secret of serviceaccount %q", name, sa)
		}
		if len(s.Data[corev1.ServiceAccountTokenKey]) == 0 {
			return false, nil
		}
		out = s
		return true, nil
	})
	return out, err
}

// UninstallManager deletes the ClusterRoleBinding, the ClusterRole and the service account in
// namespace, and the Role and RoleBinding in each of namespaces, tolerating missing objects.
// Unlike upstream (which always uses kube-system and leaves namespaced Roles behind), it
// removes what EnsureManagerServiceAccount installed for the same arguments.
func UninstallManager(ctx context.Context, kube kubernetes.Interface, namespace string, namespaces []string, opts *BootstrapOptions) error {
	o := opts.defaults()
	if namespace == "" {
		return errors.New("uninstall: the service account namespace is required")
	}
	del := func(kind, name string, fn func(context.Context) error) error {
		rctx, cancel := withTimeout(ctx, o)
		defer cancel()
		if err := fn(rctx); err != nil {
			if apierrors.IsNotFound(err) {
				o.Logger.Info("not found", "kind", kind, "name", name)
				return nil
			}
			return fmt.Errorf("failed to delete %s %q: %w", kind, name, err)
		}
		o.Logger.Info("deleted", "kind", kind, "name", name)
		return nil
	}
	type step struct {
		kind, name string
		fn         func(context.Context) error
	}
	rbac := kube.RbacV1()
	steps := []step{
		{"ClusterRoleBinding", ManagerRoleBinding, func(ctx context.Context) error {
			return rbac.ClusterRoleBindings().Delete(ctx, ManagerRoleBinding, metav1.DeleteOptions{})
		}},
		{"ClusterRole", ManagerRole, func(ctx context.Context) error {
			return rbac.ClusterRoles().Delete(ctx, ManagerRole, metav1.DeleteOptions{})
		}},
	}
	for _, ns := range NormalizeNamespaces(namespaces) {
		steps = append(steps,
			step{"RoleBinding", ns + "/" + ManagerRoleBinding, func(ctx context.Context) error {
				return rbac.RoleBindings(ns).Delete(ctx, ManagerRoleBinding, metav1.DeleteOptions{})
			}},
			step{"Role", ns + "/" + ManagerRole, func(ctx context.Context) error {
				return rbac.Roles(ns).Delete(ctx, ManagerRole, metav1.DeleteOptions{})
			}})
	}
	steps = append(steps, step{"ServiceAccount", namespace + "/" + o.ServiceAccount, func(ctx context.Context) error {
		return kube.CoreV1().ServiceAccounts(namespace).Delete(ctx, o.ServiceAccount, metav1.DeleteOptions{})
	}})
	for _, s := range steps {
		if err := del(s.kind, s.name, s.fn); err != nil {
			return err
		}
	}
	return nil
}

// ServiceAccountClaims are the claims of a legacy (Secret-based) service-account token.
type ServiceAccountClaims struct {
	Namespace          string `json:"kubernetes.io/serviceaccount/namespace"`
	SecretName         string `json:"kubernetes.io/serviceaccount/secret.name"`
	ServiceAccountName string `json:"kubernetes.io/serviceaccount/service-account.name"`
	ServiceAccountUID  string `json:"kubernetes.io/serviceaccount/service-account.uid"`
	jwt.RegisteredClaims
}

// ParseServiceAccountToken reads the claims of a service-account token without verifying it
// (the cluster verifies tokens; this only locates the Secret to rotate).
func ParseServiceAccountToken(token string) (*ServiceAccountClaims, error) {
	var claims ServiceAccountClaims
	if _, _, err := jwt.NewParser(jwt.WithoutClaimsValidation()).ParseUnverified(token, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse service account token: %w", err)
	}
	return &claims, nil
}

// RotateManagerToken replaces a legacy service-account token (upstream RotateAuth):
//
//  1. the token's claims name the namespace, Secret and service account;
//  2. a new kubernetes.io/service-account-token Secret is created with GenerateName derived
//     from the old name (its last "-" segment removed) and the old annotations, and polled
//     until the token controller filled it (TokenTimeout);
//  3. persist receives the new token: the caller tests it and stores it in the cluster's
//     credentials Secret. If persist fails the new Secret is deleted and the old token stays
//     valid;
//  4. the old Secret is removed from the service account's secrets list and deleted.
//
// kube must be a client for the destination cluster (normally built from the old token). It
// returns the new token. Tokens without a Secret name (bound or projected tokens) are rejected
// with ErrNotLegacyToken.
func RotateManagerToken(ctx context.Context, kube kubernetes.Interface, oldToken string, persist func(ctx context.Context, newToken string) error, opts *BootstrapOptions) (string, error) {
	o := opts.defaults()
	if oldToken == "" {
		return "", ErrNoBearerToken
	}
	claims, err := ParseServiceAccountToken(oldToken)
	if err != nil {
		return "", err
	}
	if claims.Namespace == "" || claims.SecretName == "" || claims.ServiceAccountName == "" {
		return "", fmt.Errorf("%w: missing namespace, secret name or service account claim", ErrNotLegacyToken)
	}
	secrets := kube.CoreV1().Secrets(claims.Namespace)
	rctx, cancel := withTimeout(ctx, o)
	old, err := secrets.Get(rctx, claims.SecretName, metav1.GetOptions{})
	cancel()
	if err != nil {
		return "", fmt.Errorf("get token secret %q: %w", claims.SecretName, err)
	}
	prefix := claims.SecretName
	if i := strings.LastIndex(prefix, "-"); i >= 0 {
		prefix = prefix[:i]
	}
	annotations := maps.Clone(old.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[corev1.ServiceAccountNameKey] = claims.ServiceAccountName
	// The token controller sets the UID annotation itself; a stale one would make it reject
	// the Secret.
	delete(annotations, corev1.ServiceAccountUIDKey)
	rctx, cancel = withTimeout(ctx, o)
	created, err := secrets.Create(rctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-", Namespace: claims.Namespace, Labels: old.Labels, Annotations: annotations},
		Type:       corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return "", fmt.Errorf("create new token secret: %w", err)
	}
	cleanup := func() {
		dctx, dcancel := withTimeout(context.WithoutCancel(ctx), o)
		defer dcancel()
		if err := secrets.Delete(dctx, created.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			o.Logger.Warn("failed to delete unused token secret", "namespace", claims.Namespace, "name", created.Name, "error", err)
		}
	}
	filled, err := waitForToken(ctx, kube, claims.Namespace, created.Name, claims.ServiceAccountName, o.TokenTimeout, o)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("timed out waiting for secret to generate new token: %w", err)
	}
	newToken := string(filled.Data[corev1.ServiceAccountTokenKey])
	if persist != nil {
		if err := persist(ctx, newToken); err != nil {
			cleanup()
			return "", fmt.Errorf("persist rotated token: %w", err)
		}
	}
	if err := retireTokenSecret(ctx, kube, claims, created.Name, o); err != nil {
		return newToken, err
	}
	o.Logger.Info("rotated service account token", "namespace", claims.Namespace, "old", claims.SecretName, "new", created.Name)
	return newToken, nil
}

// retireTokenSecret replaces the old Secret by the new one in the service account's secrets
// list and deletes the old Secret.
func retireTokenSecret(ctx context.Context, kube kubernetes.Interface, claims *ServiceAccountClaims, newName string, o BootstrapOptions) error {
	sas := kube.CoreV1().ServiceAccounts(claims.Namespace)
	rctx, cancel := withTimeout(ctx, o)
	sa, err := sas.Get(rctx, claims.ServiceAccountName, metav1.GetOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("get service account %q: %w", claims.ServiceAccountName, err)
	}
	refs := make([]corev1.ObjectReference, 0, len(sa.Secrets)+1)
	present := false
	for _, ref := range sa.Secrets {
		if ref.Name == claims.SecretName {
			continue
		}
		present = present || ref.Name == newName
		refs = append(refs, ref)
	}
	if !present {
		refs = append(refs, corev1.ObjectReference{Name: newName})
	}
	sa.Secrets = refs
	rctx, cancel = withTimeout(ctx, o)
	_, err = sas.Update(rctx, sa, metav1.UpdateOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("update secrets of service account %q: %w", claims.ServiceAccountName, err)
	}
	rctx, cancel = withTimeout(ctx, o)
	err = kube.CoreV1().Secrets(claims.Namespace).Delete(rctx, claims.SecretName, metav1.DeleteOptions{})
	cancel()
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete old token secret %q: %w", claims.SecretName, err)
	}
	return nil
}
