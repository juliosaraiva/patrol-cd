package repository

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// envtestClients returns clients of the shared envtest API server and a fresh namespace.
func envtestClients(t *testing.T) (dynamic.Interface, kubernetes.Interface, string) {
	t.Helper()
	cfg := envConfig(t)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns, err := kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "patrol-repo-"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kube.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return dyn, kube, ns.Name
}

// createObject creates obj in ns and returns its UID.
func createObject(t *testing.T, dyn dynamic.Interface, ns string, obj runtime.Object) types.UID {
	t.Helper()
	var (
		kind string
		gvr  = RepositoryGVR
	)
	switch v := obj.(type) {
	case *v1alpha1.Repository:
		v.Namespace, kind = ns, "Repository"
	case *v1alpha1.RepositoryCredential:
		v.Namespace, kind, gvr = ns, "RepositoryCredential", RepositoryCredentialGVR
	case *v1alpha1.SCMProvider:
		v.Namespace, kind, gvr = ns, "SCMProvider", SCMProviderGVR
	default:
		t.Fatalf("unsupported object %T", obj)
	}
	u := toUnstructured(t, obj, kind)
	u.SetUID("")
	created, err := dyn.Resource(gvr).Namespace(ns).Create(t.Context(), u, metav1.CreateOptions{})
	require.NoError(t, err)
	return created.GetUID()
}

// TestEnvtestResolveAndValidate exercises the lister, the resolver and the status writer
// against a real API server: CRD defaults (type git, purpose read), Secret label filtering,
// informer updates (credential rotation, deletion fallback, late labeling) and the status
// subresource (REPO-001, REPO-004, REPO-031, REPO-043).
func TestEnvtestResolveAndValidate(t *testing.T) {
	t.Parallel()
	dyn, kube, ns := envtestClients(t)
	ctx := t.Context()
	createSecret := func(s *corev1.Secret) {
		t.Helper()
		s.Namespace = ns
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	secrets := []*corev1.Secret{
		secret("global", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "g", "password": "global-pass"}),
		secret("tpl", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "tpl-pass"}),
		secret("late", "", map[string]string{"password": "late-pass"}),
		secret("cluster", "cluster", map[string]string{"password": "cluster-pass"}),
	}
	secrets[2].Labels = nil
	for _, s := range secrets {
		createSecret(s)
	}
	// Type and purpose are left empty to exercise the CRD defaults.
	app := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "app"}, Spec: v1alpha1.RepositorySpec{URL: "https://git.example/org/app"}}
	app.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: "global"}
	createObject(t, dyn, ns, app)
	// The Secret of a project-scoped Repository is bound to it by an ownerReference, which
	// needs the UID the API server assigned.
	teamUID := createObject(t, dyn, ns, repo("app-team", "https://git.example/org/app.git", withProject("team"), withCreds("team")))
	team := secret("team", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "t", "password": "team-pass"})
	team.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Repository", Name: "app-team", UID: teamUID}}
	createSecret(team)
	// A project-scoped Repository cannot borrow the Secret of another Repository.
	createObject(t, dyn, ns, repo("borrower", "https://attacker.example/x", withProject("team"), withCreds("global")))
	createObject(t, dyn, ns, repo("late", "https://git.example/org/late", withCreds("late")))
	createObject(t, dyn, ns, repo("cluster", "https://git.example/org/cluster", withCreds("cluster")))
	createObject(t, dyn, ns, cred("org", "https://git.example/org", credWithCreds("tpl")))

	l, err := NewLister(dyn, kube, ListerOptions{Namespace: ns, ResyncPeriod: time.Minute})
	require.NoError(t, err)
	lctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	require.NoError(t, l.Start(lctx))
	rs := &fakeRepoServer{}
	r, err := NewResolver(l, Options{RepoServer: rs, Registerer: prometheus.NewRegistry(), Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)

	stored, err := l.Repository("app")
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.RepositoryTypeGit, stored.Spec.Type, "CRD default")
	assert.Equal(t, v1alpha1.RepositoryPurposeRead, stored.Spec.Purpose, "CRD default")

	got, err := r.GetRepository(ctx, "https://git.example/org/app", "team")
	require.NoError(t, err)
	assert.Equal(t, "team-pass", got.Password)
	got, err = r.GetRepository(ctx, "https://git.example/org/app", "")
	require.NoError(t, err)
	assert.Equal(t, "global-pass", got.Password)
	got, err = r.GetRepository(ctx, "https://git.example/org/stub", "")
	require.NoError(t, err)
	assert.Equal(t, "tpl-pass", got.Password)
	urls, err := l.GetProjectRepositoryURLs(ctx, "team")
	require.NoError(t, err)
	assert.Equal(t, []string{"https://git.example/org/app.git", "https://attacker.example/x"}, urls)

	// Unlabeled Secrets and Secrets of other types are invisible.
	_, err = r.GetRepository(ctx, "https://git.example/org/late", "")
	assert.ErrorIs(t, err, ErrSecretNotFound)
	_, err = r.GetRepository(ctx, "https://git.example/org/cluster", "")
	assert.ErrorIs(t, err, ErrSecretNotFound)

	// Labeling the Secret later makes it visible without a restart.
	late, err := kube.CoreV1().Secrets(ns).Get(ctx, "late", metav1.GetOptions{})
	require.NoError(t, err)
	late.Labels = map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeRepository}
	_, err = kube.CoreV1().Secrets(ns).Update(ctx, late, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, err := r.GetRepository(ctx, "https://git.example/org/late", "")
		if assert.NoError(c, err) {
			assert.Equal(c, "late-pass", got.Password)
		}
	}, 10*time.Second, 50*time.Millisecond)

	// Credential rotation is picked up.
	rotated, err := kube.CoreV1().Secrets(ns).Get(ctx, "team", metav1.GetOptions{})
	require.NoError(t, err)
	rotated.Data["password"] = []byte("rotated-pass")
	_, err = kube.CoreV1().Secrets(ns).Update(ctx, rotated, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, err := r.GetRepository(ctx, "https://git.example/org/app", "team")
		if assert.NoError(c, err) {
			assert.Equal(c, "rotated-pass", got.Password)
		}
	}, 10*time.Second, 50*time.Millisecond)

	_, err = r.GetRepository(ctx, "https://attacker.example/x", "team")
	assert.ErrorIs(t, err, ErrInvalidConfiguration)
	assert.Contains(t, err.Error(), `secret "global" is not bound to project "team"`)

	// Validation and status through the status subresource.
	state, verr := r.Validate(ctx, stored)
	require.NoError(t, verr)
	w := NewStatusWriter(dyn)
	out, err := w.WriteConnectionState(ctx, stored, state, verr)
	require.NoError(t, err)
	require.NotNil(t, out.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, out.Status.ConnectionState.Status)
	assert.True(t, meta.IsStatusConditionTrue(out.Status.Conditions, v1alpha1.ConditionReady))
	assert.Equal(t, out.Generation, out.Status.ObservedGeneration)

	rs.fn = func(apiclient.Repository) (*apiclient.TestRepositoryResponse, error) {
		return &apiclient.TestRepositoryResponse{}, nil
	}
	state, verr = r.Validate(ctx, stored)
	require.Error(t, verr)
	out, err = w.WriteConnectionState(ctx, stored, state, verr)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, out.Status.ConnectionState.Status)
	failed := meta.FindStatusCondition(out.Status.Conditions, v1alpha1.ConditionConnectionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, metav1.ConditionTrue, failed.Status)
	assert.Equal(t, v1alpha1.ReasonConnectionFailed, failed.Reason)
	// The spec is untouched by status writes.
	again, err := dyn.Resource(RepositoryGVR).Namespace(ns).Get(ctx, "app", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, stored.Generation, again.GetGeneration())

	// A check of the previous generation is not recorded once the spec changed.
	rs.fn = nil
	state, verr = r.Validate(ctx, stored)
	require.NoError(t, verr)
	require.NoError(t, unstructured.SetNestedField(again.Object, int64(1), "spec", "depth"))
	edited, err := dyn.Resource(RepositoryGVR).Namespace(ns).Update(ctx, again, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Greater(t, edited.GetGeneration(), stored.Generation)
	out, err = w.WriteConnectionState(ctx, stored, state, verr)
	require.ErrorIs(t, err, ErrStaleConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, out.Status.ConnectionState.Status, "the stale success is not written")
	assert.Equal(t, stored.Generation, out.Status.ObservedGeneration)

	// Deleting the scoped Repository falls back to the unscoped one.
	require.NoError(t, dyn.Resource(RepositoryGVR).Namespace(ns).Delete(ctx, "app-team", metav1.DeleteOptions{}))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := r.Resolve(ctx, Query{URL: "https://git.example/org/app", Project: "team"})
		if assert.NoError(c, err) {
			assert.Equal(c, SourceGlobal, res.Source)
			assert.Equal(c, "global-pass", res.Repository.Password)
		}
	}, 10*time.Second, 50*time.Millisecond)
}
