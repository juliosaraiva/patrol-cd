package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Account API limits.
const (
	// maxTokenIDLength bounds caller-chosen API token IDs.
	maxTokenIDLength = 128
	// maxTokenLifetime bounds expiresIn (100 years; 0 still means no expiry).
	maxTokenLifetime = 100 * 365 * 24 * time.Hour
	// tokenVisibilityWait bounds how long account writes (tokens, passwords) wait for the
	// settings snapshot (the patrol-secret informer plus the settings debounce) to reflect
	// them, so a created token authenticates once it is returned, a deleted one no longer
	// does once the deletion returns, and a changed password is the one logins accept.
	tokenVisibilityWait = 5 * time.Second
	// tokenVisibilityPoll is the polling interval of that wait.
	tokenVisibilityPoll = 20 * time.Millisecond
)

// accountToAPI converts an account; tokens are sorted by issue time, newest first
// (upstream).
func accountToAPI(a auth.Account) apiclient.Account {
	out := apiclient.Account{Name: a.Name, Enabled: a.Enabled}
	for _, c := range a.Capabilities {
		out.Capabilities = append(out.Capabilities, string(c))
	}
	for _, t := range a.Tokens {
		out.Tokens = append(out.Tokens, apiclient.AccountToken{ID: t.ID, IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt})
	}
	slices.SortStableFunc(out.Tokens, func(x, y apiclient.AccountToken) int {
		switch {
		case x.IssuedAt > y.IssuedAt:
			return -1
		case x.IssuedAt < y.IssuedAt:
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// listAccounts serves GET /api/v1/account: the accounts the caller may get (a local
// caller always sees its own account), sorted by name.
func (h *Handlers) listAccounts(ctx context.Context, _ *apiclient.Empty) (*apiclient.AccountList, error) {
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	id := rest.IdentityFrom(ctx)
	accts := s.Accounts()
	out := &apiclient.AccountList{Items: []apiclient.Account{}}
	for _, name := range accts.Names() {
		if !id.IsLocal(name) && !rest.Can(ctx, rbac.ResourceAccounts, rbac.ActionGet, name) {
			continue
		}
		out.Items = append(out.Items, accountToAPI(accts[name]))
	}
	return out, nil
}

// authorizeAccount enforces accounts/<action>/<name>, exempting a local caller acting on
// its own account. The check runs before the account is looked up so that a denied
// caller cannot probe which accounts exist.
func authorizeAccount(ctx context.Context, action, name string) error {
	if rest.IdentityFrom(ctx).IsLocal(name) {
		return nil
	}
	return rest.Authorize(ctx, rbac.ResourceAccounts, action, name)
}

// getAccount serves GET /api/v1/account/{name}.
func (h *Handlers) getAccount(ctx context.Context, req *apiclient.GetAccountRequest) (*apiclient.Account, error) {
	if err := authorizeAccount(ctx, rbac.ActionGet, req.Name); err != nil {
		return nil, err
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	a, err := s.Accounts().Get(req.Name)
	if err != nil {
		return nil, rest.NotFound("account '%s' does not exist", req.Name).WithCause(err)
	}
	out := accountToAPI(a)
	return &out, nil
}

// canI serves GET /api/v1/account/can-i/{resource}/{action}/{subresource...}.
func (h *Handlers) canI(ctx context.Context, req *apiclient.CanIRequest) (*apiclient.CanIResponse, error) {
	if err := rbac.ValidateResourceAction(req.Resource, req.Action); err != nil {
		return nil, rest.InvalidArgument("%s", err.Error()).WithCause(err)
	}
	object := req.Subresource
	if rbac.IsProjectScoped(req.Resource) {
		// "<project>/<namespace>/<name>" collapses to "<project>/<name>" in the
		// control-plane namespace (apps in any namespace).
		if parts := strings.Split(object, "/"); len(parts) == 3 {
			object = rbac.AppObject(h.d.Config.Namespace, parts[0], parts[1], parts[2])
		}
	}
	if rest.Can(ctx, req.Resource, req.Action, object) {
		return &apiclient.CanIResponse{Value: "yes"}, nil
	}
	return &apiclient.CanIResponse{Value: "no"}, nil
}

// updatePassword serves PUT /api/v1/account/password (upstream AccountService.UpdatePassword):
//   - name "" means the caller; project tokens cannot change passwords;
//   - a local caller acting on its own account needs no RBAC permission; any other change
//     (another account, or an SSO caller) needs accounts/update/<name>;
//   - a local caller must prove its own current password (also when changing another
//     account), through the throttled login path;
//   - an SSO caller's session must be younger than SSOTokenMaxAgeForPasswordChange;
//   - the new password must satisfy passwordPattern.
//
// Every session and API token of the account issued before the change stops verifying.
func (h *Handlers) updatePassword(ctx context.Context, req *apiclient.UpdatePasswordRequest) (*apiclient.Empty, error) {
	id := rest.IdentityFrom(ctx)
	if !id.LoggedIn() {
		return nil, rest.Unauthenticated("no session information")
	}
	p := id.Principal
	if p.Kind == auth.KindProject {
		return nil, rest.PermissionDenied("project tokens cannot change passwords")
	}
	local := p.Kind == auth.KindLogin || p.Kind == auth.KindAPIKey
	caller := p.Subject
	name := req.Name
	if name == "" {
		if !local {
			return nil, rest.InvalidArgument("name is required for SSO users")
		}
		name = caller
	}
	audit := rest.AuditFrom(ctx)
	audit.SetCheck(rbac.ResourceAccounts, rbac.ActionUpdate, name)
	if !local || name != caller {
		if err := rest.Authorize(ctx, rbac.ResourceAccounts, rbac.ActionUpdate, name); err != nil {
			return nil, err
		}
	}
	if req.NewPassword == "" {
		return nil, rest.InvalidArgument("newPassword is required")
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	if local {
		if h.d.Login == nil {
			return nil, rest.Unavailable("password verification is not available").WithCause(errNotConfigured)
		}
		if req.CurrentPassword == "" {
			return nil, rest.InvalidArgument("currentPassword is required")
		}
		if _, err := h.d.Login.Authenticate(ctx, caller, req.CurrentPassword); err != nil {
			if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrAccountDisabled) || errors.Is(err, auth.ErrMissingCapability) {
				return nil, rest.InvalidArgument("current password does not match").WithCause(err)
			}
			return nil, h.loginError(ctx, caller, err)
		}
	} else if h.now().Sub(p.IssuedAt) > SSOTokenMaxAgeForPasswordChange {
		return nil, rest.Unauthenticated("SSO token is too old. Please use 'patrol relogin' to get a new token")
	}
	target, err := s.Accounts().Get(name)
	if err != nil {
		return nil, rest.NotFound("account '%s' does not exist", name).WithCause(err)
	}
	policy, err := auth.NewPasswordPolicy(s.PasswordPattern())
	if err != nil {
		return nil, rest.FailedPrecondition("the configured passwordPattern is invalid").WithCause(err)
	}
	if err := policy.Validate(req.NewPassword); err != nil {
		return nil, rest.InvalidArgument("new password does not match the required pattern %s", policy.Pattern()).WithCause(err)
	}
	if h.d.Secrets == nil {
		return nil, rest.Unavailable("the settings Secret store is not configured").WithCause(errNotConfigured)
	}
	now := h.now()
	err = h.d.Secrets.Update(ctx, func(data map[string]string) (map[string]string, error) {
		return auth.ChangePassword(data, target, req.NewPassword, policy, now)
	})
	if err != nil {
		if errors.Is(err, auth.ErrPasswordPolicy) {
			return nil, rest.InvalidArgument("new password does not match the required pattern %s", policy.Pattern()).WithCause(err)
		}
		return nil, secretError(err)
	}
	audit.SetMessage("%s changed the password of account %s", caller, name)
	// Return once logins accept the new password (and reject sessions issued before it).
	if !h.waitAccount(ctx, name, func(a auth.Account, found bool) bool { return !found || a.PasswordHash != target.PasswordHash }) {
		h.log().Warn("the new password is not in effect yet: the settings snapshot still has the previous one", "account", name, "wait", tokenVisibilityWait)
	}
	return &apiclient.Empty{}, nil
}

// createAccountToken serves POST /api/v1/account/{name}/token.
func (h *Handlers) createAccountToken(ctx context.Context, req *apiclient.CreateAccountTokenRequest) (*apiclient.TokenResponse, error) {
	if err := authorizeAccount(ctx, rbac.ActionUpdate, req.Name); err != nil {
		return nil, err
	}
	if req.ExpiresIn < 0 {
		return nil, rest.InvalidArgument("expiresIn must not be negative")
	}
	expiresIn := time.Duration(req.ExpiresIn) * time.Second
	if req.ExpiresIn > int64(maxTokenLifetime/time.Second) {
		return nil, rest.InvalidArgument("expiresIn must be at most %d seconds", int64(maxTokenLifetime/time.Second))
	}
	if err := validateTokenID(req.ID); err != nil {
		return nil, err
	}
	if h.d.Tokens == nil || h.d.Secrets == nil {
		return nil, rest.Unavailable("API tokens are not available").WithCause(errNotConfigured)
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	acct, err := s.Accounts().Get(req.Name)
	if err != nil {
		return nil, rest.NotFound("account '%s' does not exist", req.Name).WithCause(err)
	}
	if !acct.HasCapability(auth.CapabilityAPIKey) {
		return nil, rest.InvalidArgument("account '%s' does not have apiKey capability", req.Name)
	}
	if !acct.Enabled {
		return nil, rest.FailedPrecondition("account '%s' is disabled", req.Name)
	}
	var token, tokenID string
	err = h.d.Secrets.Update(ctx, func(data map[string]string) (map[string]string, error) {
		iss, meta, err := h.d.Tokens.IssueAccountToken(ctx, acct, req.ID, expiresIn)
		if err != nil {
			return nil, err
		}
		next, err := auth.AddToken(data, acct, meta)
		if err != nil {
			return nil, err
		}
		token, tokenID = iss.Token, meta.ID
		return next, nil
	})
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrTokenExists):
		return nil, rest.AlreadyExists("account already has token with id '%s'", req.ID).WithCause(err)
	case errors.Is(err, auth.ErrCorruptTokenList):
		return nil, rest.FailedPrecondition("the stored token list of account '%s' is invalid", req.Name).WithCause(err)
	case errors.Is(err, auth.ErrMissingCapability):
		return nil, rest.InvalidArgument("account '%s' does not have apiKey capability", req.Name).WithCause(err)
	case errors.Is(err, auth.ErrAccountDisabled):
		return nil, rest.FailedPrecondition("account '%s' is disabled", req.Name).WithCause(err)
	default:
		return nil, secretError(err)
	}
	rest.AuditFrom(ctx).SetMessage("token created for account %s", req.Name)
	if !h.waitTokenListed(ctx, req.Name, tokenID, true) {
		h.log().Warn("the new API token is not usable yet: the settings snapshot does not list it", "account", req.Name, "wait", tokenVisibilityWait)
	}
	return &apiclient.TokenResponse{Token: token}, nil
}

// deleteAccountToken serves DELETE /api/v1/account/{name}/token/{id}; removing the
// metadata revokes the token.
func (h *Handlers) deleteAccountToken(ctx context.Context, req *apiclient.DeleteAccountTokenRequest) (*apiclient.Empty, error) {
	if err := authorizeAccount(ctx, rbac.ActionUpdate, req.Name); err != nil {
		return nil, err
	}
	if h.d.Secrets == nil {
		return nil, rest.Unavailable("API tokens are not available").WithCause(errNotConfigured)
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	acct, err := s.Accounts().Get(req.Name)
	if err != nil {
		return nil, rest.NotFound("account '%s' does not exist", req.Name).WithCause(err)
	}
	err = h.d.Secrets.Update(ctx, func(data map[string]string) (map[string]string, error) {
		return auth.DeleteToken(data, acct, req.ID)
	})
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrTokenNotFound):
		return nil, rest.NotFound("token with id '%s' does not exist", req.ID).WithCause(err)
	case errors.Is(err, auth.ErrCorruptTokenList):
		return nil, rest.FailedPrecondition("the stored token list of account '%s' is invalid", req.Name).WithCause(err)
	default:
		return nil, secretError(err)
	}
	rest.AuditFrom(ctx).SetMessage("token deleted from account %s", req.Name)
	if !h.waitTokenListed(ctx, req.Name, req.ID, false) {
		h.log().Warn("the deleted API token is still listed by the settings snapshot", "account", req.Name, "wait", tokenVisibilityWait)
	}
	return &apiclient.Empty{}, nil
}

// waitAccount waits until the settings snapshot's view of an account satisfies done (found
// is false when the account does not exist or is not usable). The token service and the
// login path read accounts from that snapshot, which follows patrol-secret through an
// informer and a debounce, so account writes wait for it before returning. It returns false
// when the context ends or tokenVisibilityWait passes first.
func (h *Handlers) waitAccount(ctx context.Context, account string, done func(a auth.Account, found bool) bool) bool {
	deadline := time.NewTimer(tokenVisibilityWait)
	defer deadline.Stop()
	tick := time.NewTicker(tokenVisibilityPoll)
	defer tick.Stop()
	for {
		if s := h.d.Settings.Current(); s != nil {
			a, err := s.Accounts().Get(account)
			if done(a, err == nil) {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// waitTokenListed waits until the settings snapshot lists (listed) or no longer lists
// (!listed) token id of account (see waitAccount).
func (h *Handlers) waitTokenListed(ctx context.Context, account, id string, listed bool) bool {
	return h.waitAccount(ctx, account, func(a auth.Account, found bool) bool {
		return (found && a.TokenIndex(id) >= 0) == listed
	})
}

func validateTokenID(id string) error {
	if len(id) > maxTokenIDLength {
		return rest.InvalidArgument("token id must be at most %d characters", maxTokenIDLength)
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return rest.InvalidArgument("token id must not contain whitespace or control characters")
		}
	}
	return nil
}
