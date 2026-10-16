package core_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func accountNames(l *apiclient.AccountList) []string {
	var out []string
	for _, a := range l.Items {
		out = append(out, a.Name)
	}
	return out
}

func TestListAndGetAccounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	all := []string{"admin", "alice", "bob", "ci", "dave"}

	l, err := e.client("admin").ListAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, all, accountNames(l))

	l, err = e.client("alice").ListAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, all, accountNames(l), "role:readonly may get every account")

	dave := e.client("dave")
	l, err = dave.ListAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"dave"}, accountNames(l), "a local user always sees its own account")

	a, err := dave.GetAccount(ctx, "dave")
	require.NoError(t, err)
	assert.Equal(t, apiclient.Account{Name: "dave", Enabled: true, Capabilities: []string{"login"}}, *a)

	_, err = dave.GetAccount(ctx, "alice")
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "permission denied: accounts, get, alice, sub: dave")
	denied := e.audit.byOperation("getAccount")
	require.NotEmpty(t, denied)
	last := denied[len(denied)-1]
	assert.Equal(t, rest.AuditDenied, last.Result)
	assert.Equal(t, "dave", last.Actor)
	assert.Equal(t, "alice", last.Object)

	_, err = dave.GetAccount(ctx, "nobody")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "denied callers cannot probe for accounts")
	_, err = e.client("admin").GetAccount(ctx, "nobody")
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)

	_, err = e.client("").ListAccounts(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}

func TestCanI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	alice := e.client("alice")
	tests := []struct {
		resource, action, sub string
		want                  bool
	}{
		{"applications", "get", "*/*", true},
		{"applications", "get", "default/guestbook", true},
		{"applications", "get", "default/" + testNamespace + "/guestbook", true},
		{"applications", "sync", "default/guestbook", false},
		{"clusters", "get", "https://kubernetes.default.svc", true},
		{"accounts", "update", "alice", false},
	}
	for _, tc := range tests {
		got, err := alice.CanI(ctx, tc.resource, tc.action, tc.sub)
		require.NoError(t, err, "%+v", tc)
		assert.Equal(t, tc.want, got, "%+v", tc)
	}
	got, err := e.client("admin").CanI(ctx, "applications", "sync", "default/guestbook")
	require.NoError(t, err)
	assert.True(t, got)

	_, err = alice.CanI(ctx, "nonsense", "get", "x")
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = alice.CanI(ctx, "applications", "fly", "x")
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = e.client("").CanI(ctx, "applications", "get", "x")
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}

func TestAccountTokenLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	alice := e.client("alice")

	// Self-service: a local session manages its own tokens without RBAC.
	tr, err := alice.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "alice", ID: "t1", ExpiresIn: 3600})
	require.NoError(t, err)
	p, err := e.tokens.Verify(ctx, tr.Token)
	require.NoError(t, err, "the token verifies once its metadata is persisted")
	assert.Equal(t, auth.KindAPIKey, p.Kind)
	assert.Equal(t, "t1", p.TokenID)
	assert.WithinDuration(t, e.clock.Now().Add(time.Hour), p.ExpiresAt, time.Second)
	assert.Contains(t, e.secrets.Data()["accounts.alice.tokens"], `"id":"t1"`)

	api := e.clientWithToken(tr.Token)
	ui, err := api.UserInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "alice", ui.Username)

	e.clock.Advance(time.Second)
	_, err = alice.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "alice", ID: "t2"})
	require.NoError(t, err)
	a, err := alice.GetAccount(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, a.Tokens, 2)
	assert.Equal(t, "t2", a.Tokens[0].ID, "newest first")
	assert.Zero(t, a.Tokens[0].ExpiresAt, "expiresIn 0 creates a token without expiry")
	assert.Equal(t, "t1", a.Tokens[1].ID)

	_, err = alice.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "alice", ID: "t1"})
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusConflict, ae.StatusCode)
	assert.Equal(t, apiclient.CodeAlreadyExists, ae.Code())
	assert.Contains(t, ae.Problem.Detail, "account already has token with id 't1'")

	// Other accounts need accounts/update.
	_, err = e.client("dave").CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "alice"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	err = e.client("dave").DeleteAccountToken(ctx, "dave", "x")
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusNotFound, ae.StatusCode, "self-service delete reaches the lookup")
	assert.Contains(t, ae.Problem.Detail, "token with id 'x' does not exist")

	// The admin role may manage other accounts' tokens; capability rules still apply.
	admin := e.client("admin")
	ciTok, err := admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "ci", ID: "deploy"})
	require.NoError(t, err)
	_, err = e.tokens.Verify(ctx, ciTok.Token)
	require.NoError(t, err)
	_, err = admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "dave"})
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "does not have apiKey capability")
	_, err = admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "nobody"})
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)
	_, err = admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "ci", ExpiresIn: -1})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "ci", ID: "bad id"})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)

	// Deleting the metadata revokes the token.
	require.NoError(t, alice.DeleteAccountToken(ctx, "alice", "t1"))
	_, err = api.UserInfo(ctx)
	require.NoError(t, err)
	ui, err = api.UserInfo(ctx)
	require.NoError(t, err)
	assert.False(t, ui.LoggedIn, "a deleted token no longer authenticates")
	_, err = api.ListAccounts(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
	err = alice.DeleteAccountToken(ctx, "alice", "t1")
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)

	// Expiry is honoured.
	e.clock.Advance(2 * time.Hour)
	_, err = e.tokens.Verify(ctx, ciTok.Token)
	require.NoError(t, err, "tokens without expiry stay valid")

	creates := e.audit.byOperation("createAccountToken")
	require.NotEmpty(t, creates)
	assert.Equal(t, rest.AuditSucceeded, creates[0].Result)
	assert.Equal(t, "alice", creates[0].Actor)
	for _, a := range creates {
		assert.NotContains(t, a.Message, tr.Token, "tokens never reach the audit log")
	}
}

func TestUpdatePassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	alice := e.client("alice")
	old := alice.Token()
	e.clock.Advance(2 * time.Second)

	err := alice.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{CurrentPassword: "wrong-pass", NewPassword: "N3w-password"})
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "current password does not match")

	err = alice.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{CurrentPassword: testPassword, NewPassword: "short"})
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "does not match the required pattern")

	err = alice.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{CurrentPassword: testPassword, NewPassword: "N3w-password"})
	require.NoError(t, err)
	_, err = e.tokens.Verify(ctx, old)
	require.ErrorIs(t, err, auth.ErrSessionInvalidated, "sessions issued before the change stop verifying")
	_, err = e.client("").Login(ctx, "alice", "N3w-password")
	require.NoError(t, err)

	// Changing another account needs accounts/update; the caller proves its own password.
	dave := e.client("dave")
	err = dave.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "alice", CurrentPassword: testPassword, NewPassword: "Ev1l-password"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	admin := e.client("admin")
	err = admin.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "alice", CurrentPassword: "N3w-password", NewPassword: "Adm1n-chosen"})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode, "the admin's own current password is required")
	err = admin.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "alice", CurrentPassword: testPassword, NewPassword: "Adm1n-chosen"})
	require.NoError(t, err)
	err = admin.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "nobody", CurrentPassword: testPassword, NewPassword: "Adm1n-chosen"})
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)

	// Project tokens cannot change passwords.
	pt, err := e.tokens.IssueProjectToken(ctx, "default", "ci", "", 0)
	require.NoError(t, err)
	err = e.clientWithToken(pt.Token).UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "dave", NewPassword: "Pr0j-password"})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)

	// SSO callers need a recent session and accounts/update.
	sso, err := e.tokens.IssueSSOSession(ctx, auth.SSOIdentity{Subject: "u-1", Groups: []string{"admins"}}, time.Hour)
	require.NoError(t, err)
	ssoClient := e.clientWithToken(sso.Token)
	err = ssoClient.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{NewPassword: "Ss0-password"})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode, "SSO users must name the account")
	err = ssoClient.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "dave", NewPassword: "Ss0-password"})
	require.NoError(t, err)
	e.clock.Advance(6 * time.Minute)
	err = ssoClient.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{Name: "dave", NewPassword: "Ss0-password2"})
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusUnauthorized, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "SSO token is too old")

	updates := e.audit.byOperation("updatePassword")
	require.NotEmpty(t, updates)
	for _, u := range updates {
		assert.NotContains(t, u.Message, "N3w-password")
	}
}

// TestAccountTokenVisibleWhenReturned is the regression test of integration finding I-2: the
// settings snapshot lags behind patrol-secret writes (informer and 200 ms debounce), so a
// token returned by POST /api/v1/account/{name}/token was rejected as "token revoked" until
// the snapshot listed it, and a deleted token kept working after DELETE returned.
func TestAccountTokenVisibleWhenReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{secretLag: 300 * time.Millisecond})
	ctx := context.Background()
	alice := e.client("alice")

	tr, err := alice.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "alice", ID: "now"})
	require.NoError(t, err)
	_, err = e.tokens.Verify(ctx, tr.Token)
	require.NoError(t, err, "the token authenticates as soon as it is returned")
	ui, err := e.clientWithToken(tr.Token).UserInfo(ctx)
	require.NoError(t, err)
	assert.True(t, ui.LoggedIn)

	require.NoError(t, alice.DeleteAccountToken(ctx, "alice", "now"))
	_, err = e.tokens.Verify(ctx, tr.Token)
	require.Error(t, err, "the token is revoked as soon as the deletion returns")
}

// TestUpdatePasswordInEffectWhenReturned is the regression test of integration finding I-2
// for passwords: with the settings snapshot lagging behind patrol-secret, a login with the
// new password failed ("Invalid username or password") right after the change returned, and
// sessions issued before it kept verifying.
func TestUpdatePasswordInEffectWhenReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{secretLag: 300 * time.Millisecond})
	ctx := context.Background()
	alice := e.client("alice")
	old := alice.Token()
	e.clock.Advance(2 * time.Second)

	require.NoError(t, alice.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{CurrentPassword: testPassword, NewPassword: "N3w-password"}))
	_, err := e.client("").Login(ctx, "alice", "N3w-password")
	require.NoError(t, err, "the new password is accepted as soon as the change returns")
	_, err = e.tokens.Verify(ctx, old)
	require.ErrorIs(t, err, auth.ErrSessionInvalidated, "earlier sessions stop verifying as soon as the change returns")
}
