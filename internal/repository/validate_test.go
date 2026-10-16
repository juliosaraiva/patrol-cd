package repository

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clienttesting "k8s.io/client-go/testing"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// TestValidate covers REPO-040, REPO-043 and REPO-044 at the library level: the resolved
// access is tested on the repo-server, failures produce upstream's connection messages and
// secret values echoed by the repo-server are redacted.
func TestValidate(t *testing.T) {
	t.Parallel()
	rs := &fakeRepoServer{fn: func(r apiclient.Repository) (*apiclient.TestRepositoryResponse, error) {
		switch {
		case strings.HasSuffix(r.URL, "/down"):
			return nil, errors.New("authentication required for user u with password hunter2-secret")
		case strings.HasSuffix(r.URL, "/unverified"):
			return &apiclient.TestRepositoryResponse{}, nil
		default:
			return &apiclient.TestRepositoryResponse{VerifiedRepository: true}, nil
		}
	}}
	clock := newFakeClock(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	f := newFixture(t, []runtime.Object{
		secret("creds", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "u", "password": "hunter2-secret"}),
		secret("tpl", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"username": "tu", "password": "template-secret"}),
		secret("write-tpl", v1alpha1.LabelValueSecretTypeRepoCredsWrite, map[string]string{"password": "write-template-secret"}),
		repo("ok", "https://git.example/org/ok", withCreds("creds")),
		repo("down", "https://git.example/org/down", withCreds("creds")),
		repo("unverified", "https://git.example/org/unverified", withCreds("creds")),
		repo("missing", "https://git.example/org/missing", withCreds("nope")),
		repo("inherits", "https://tpl.example/org/app"),
		cred("tpl", "https://tpl.example/", credWithCreds("tpl")),
		cred("write-tpl", "https://tpl.example/", credWithPurpose(write), credWithCreds("write-tpl")),
	}, withClock(clock), withRepoServer(rs))
	ctx := t.Context()
	get := func(name string) *v1alpha1.Repository {
		t.Helper()
		r, err := f.lister.Repository(name)
		require.NoError(t, err)
		return r
	}

	state, err := f.resolver.Validate(ctx, get("ok"))
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, state.Status)
	assert.Empty(t, state.Message)
	require.NotNil(t, state.AttemptedAt)
	assert.True(t, state.AttemptedAt.Time.Equal(clock.Now()))
	assert.Equal(t, "hunter2-secret", rs.lastCall(t).Password)

	state, err = f.resolver.Validate(ctx, get("down"))
	require.Error(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, state.Status)
	assert.True(t, strings.HasPrefix(state.Message, MessageConnectionFailedPrefix), state.Message)
	assert.NotContains(t, state.Message, "hunter2-secret")
	assert.Contains(t, state.Message, "******")
	assert.NotContains(t, err.Error(), "hunter2-secret")
	assert.Equal(t, v1alpha1.ReasonConnectionFailed, ConditionReason(err))

	state, err = f.resolver.Validate(ctx, get("unverified"))
	require.Error(t, err)
	assert.Equal(t, MessageConnectionFailedPrefix+"repository could not be verified", state.Message)

	calls := rs.callCount()
	state, err = f.resolver.Validate(ctx, get("missing"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSecretNotFound)
	assert.Equal(t, MessageConfigurationError, state.Message)
	assert.Equal(t, calls, rs.callCount(), "configuration errors never reach the repo-server")

	// A stored repository without credentials inherits the template of its purpose.
	_, err = f.resolver.Validate(ctx, get("inherits"))
	require.NoError(t, err)
	assert.Equal(t, "template-secret", rs.lastCall(t).Password)

	// Unsaved input: the data replaces the referenced Secret; read input without credentials
	// inherits templates, write input does not (upstream ValidateWriteAccess).
	input := repo("input", "https://git.example/org/ok", withCreds("does-not-exist"))
	_, err = f.resolver.ValidateWithSecretData(ctx, input, map[string][]byte{"password": []byte("typed-in")})
	require.NoError(t, err)
	assert.Equal(t, "typed-in", rs.lastCall(t).Password)
	_, err = f.resolver.ValidateWithSecretData(ctx, repo("input", "https://tpl.example/org/new"), nil)
	require.NoError(t, err)
	assert.Equal(t, "template-secret", rs.lastCall(t).Password)
	_, err = f.resolver.ValidateWithSecretData(ctx, repo("input", "https://tpl.example/org/new", withPurpose(write)), nil)
	require.NoError(t, err)
	assert.Empty(t, rs.lastCall(t).Password)

	assert.InDelta(t, 5, f.counterValue(t, "patrol_repository_validations_total", map[string]string{"result": "Successful"}), 0)
	assert.InDelta(t, 3, f.counterValue(t, "patrol_repository_validations_total", map[string]string{"result": "Failed"}), 0)
	assert.NotContains(t, f.logs.String(), "hunter2-secret")
	assert.NotContains(t, f.logs.String(), "template-secret")
}

// TestValidateWithoutRepoServer checks the error when no repo-server client is configured.
func TestValidateWithoutRepoServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{repo("r", "https://git.example/r")})
	r, err := f.lister.Repository("r")
	require.NoError(t, err)
	state, err := f.resolver.Validate(t.Context(), r)
	assert.ErrorIs(t, err, ErrNoRepoServer)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, state.Status)
	assert.ErrorIs(t, f.resolver.TestRepository(t.Context(), &apiclient.Repository{URL: "x"}), ErrNoRepoServer)
}

// TestApplyConnectionStatus checks the conditions derived from a connection state.
func TestApplyConnectionStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	st := &v1alpha1.RepositoryStatus{}
	applyConnectionStatus(st, 3, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful}, nil, now)
	ready := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, v1alpha1.ReasonConnectionSuccessful, ready.Reason)
	assert.Equal(t, int64(3), ready.ObservedGeneration)
	assert.Equal(t, int64(3), st.ObservedGeneration)
	failed := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionConnectionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, metav1.ConditionFalse, failed.Status)

	cause := configError(ErrSecretKeyMissing, "secret %q has no key %q", "s", "password")
	later := now.Add(time.Minute)
	applyConnectionStatus(st, 4, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, Message: MessageConfigurationError}, cause, later)
	ready = meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v1alpha1.ReasonSecretKeyMissing, ready.Reason)
	assert.Contains(t, ready.Message, `has no key "password"`)
	assert.True(t, ready.LastTransitionTime.Time.Equal(later))
	assert.Equal(t, MessageConfigurationError, st.ConnectionState.Message)

	applyConnectionStatus(st, 4, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, Message: strings.Repeat("x", maxConditionMessage+10)}, nil, later)
	ready = meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady)
	assert.Equal(t, v1alpha1.ReasonConnectionFailed, ready.Reason)
	assert.Len(t, ready.Message, maxConditionMessage)

	applyConnectionStatus(st, 5, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusUnknown}, nil, later)
	ready = meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady)
	assert.Equal(t, metav1.ConditionUnknown, ready.Status)
	assert.Equal(t, v1alpha1.ReasonCheckPending, ready.Reason)
}

// TestStatusWriterRetriesConflicts checks the status writer over a fake client: the object
// is re-read after a conflict, and an unchanged status is not written again.
func TestStatusWriterRetriesConflicts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{repo("r", "https://git.example/r")})
	var conflicts, updates atomic.Int32
	f.dyn.PrependReactor("update", "repositories", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "status" {
			return false, nil, nil
		}
		updates.Add(1)
		if conflicts.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(RepositoryGVR.GroupResource(), "r", errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	clock := newFakeClock(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	w := NewStatusWriter(f.dyn, WithStatusClock(clock.Now), WithStatusBackoff(wait.Backoff{Steps: 3, Duration: time.Millisecond, Factor: 1}))
	at := metav1.NewTime(clock.Now())
	state := v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: &at}
	validated, err := f.lister.Repository("r")
	require.NoError(t, err)
	out, err := w.WriteConnectionState(t.Context(), validated, state, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, out.Status.ConnectionState.Status)
	assert.True(t, meta.IsStatusConditionTrue(out.Status.Conditions, v1alpha1.ConditionReady))
	assert.Equal(t, int32(2), updates.Load())

	_, err = w.WriteConnectionState(t.Context(), validated, state, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), updates.Load(), "an unchanged status is not written")

	_, err = w.WriteConnectionState(t.Context(), repo("missing", "https://git.example/missing"), state, nil)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}

// TestFacade checks the adapters used by the hydrator and the commit-server path.
func TestFacade(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("read", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "read-pass"}),
		ownedBy(secret("write", v1alpha1.LabelValueSecretTypeRepositoryWrite, map[string]string{"password": "write-pass"}), "dry-write"),
		secret("helm", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "u", "password": "helm-pass"}),
		secret("tpl", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"username": "u", "password": "tpl-pass"}),
		repo("dry", "https://git.example/org/dry", withCreds("read")),
		repo("dry-write", "https://git.example/org/dry", withPurpose(write), withCreds("write"), withProject("team")),
		repo("stable", "https://charts.example/stable", withType(v1alpha1.RepositoryTypeHelm), withCreds("helm"), func(r *v1alpha1.Repository) { r.Spec.Name = "stable" }),
		repo("oci", "oci://registry.example/charts", withType(v1alpha1.RepositoryTypeOCI)),
		cred("helm-tpl", "https://charts.example/", credWithType(v1alpha1.RepositoryTypeHelm), credWithCreds("tpl")),
		cred("oci-tpl", "oci://registry.example/", credWithType(v1alpha1.RepositoryTypeOCI), credWithCreds("tpl")),
	})
	// Method sets matching the hydrator's dependency interfaces.
	var _ interface {
		GetRepository(context.Context, string, string) (*apiclient.Repository, error)
		GetWriteRepository(context.Context, string, string) (*apiclient.Repository, error)
		ListHelmRepositories(context.Context, *v1alpha1.AppProject) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error)
	} = f.resolver
	ctx := t.Context()

	r, err := f.resolver.GetRepository(ctx, "https://git.example/org/dry", "team")
	require.NoError(t, err)
	assert.Equal(t, "read-pass", r.Password)
	w, err := f.resolver.GetWriteRepository(ctx, "https://git.example/org/dry", "team")
	require.NoError(t, err)
	assert.Equal(t, "write-pass", w.Password)
	assert.Equal(t, v1alpha1.RepositoryTypeGit, w.Type)
	w, err = f.resolver.GetWriteRepository(ctx, "https://git.example/org/dry", "other")
	require.NoError(t, err)
	require.NotNil(t, w, "no write credentials yields a stub, never nil")
	assert.False(t, w.HasCredentials())
	assert.Equal(t, "https://git.example/org/dry", w.URL)
	_, err = f.resolver.GetWriteRepository(ctx, "", "team")
	assert.ErrorIs(t, err, ErrInvalidConfiguration)

	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}}}
	repos, creds, err := f.resolver.ListHelmRepositories(ctx, proj)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "helm-pass", repos[0].Password)
	require.Len(t, creds, 1)
	assert.Equal(t, "https://charts.example/", creds[0].URLPrefix)
	repos, creds, err = f.resolver.ListDependencyRepositories(ctx, proj, true)
	require.NoError(t, err)
	assert.Len(t, repos, 2)
	assert.Len(t, creds, 2)
	_, _, err = f.resolver.ListHelmRepositories(ctx, nil)
	assert.Error(t, err)
}
