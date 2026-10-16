package core

import (
	"net/http"
	"reflect"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Route tags.
const (
	tagSession  = "session"
	tagAccounts = "accounts"
	tagSettings = "settings"
	tagVersion  = "version"
)

// SSOCallbackQuery documents the query parameters of the OIDC callback.
type SSOCallbackQuery struct {
	// Code is the authorization code.
	Code string `query:"code" json:"-"`
	// State must match the encrypted state cookie.
	State string `query:"state" json:"-"`
	// Error is the identity provider's error code.
	Error string `query:"error" json:"-"`
	// ErrorDescription is the identity provider's error text.
	ErrorDescription string `query:"error_description" json:"-"`
}

// SSOLoginQuery documents the query of GET /auth/login.
type SSOLoginQuery struct {
	// ReturnURL is where the browser goes after login; it must lie within url or one of
	// additionalUrls.
	ReturnURL string `query:"return_url" json:"-"`
}

// Register adds the session, SSO, account, settings and version routes to t.
func (h *Handlers) Register(t *rest.Table) {
	// Session.
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathSession, OperationID: "createSession",
		Summary: "Local login", Tags: []string{tagSession}, Access: rest.Public(), RateLimited: true,
		Description: "Verifies a local account password (with per-account lockout and a global concurrency limit) and returns a session token; `?cookie=true` also sets the patrol.token cookie.",
	}, h.createSession)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathSession, OperationID: "deleteSession",
		Summary: "Logout (revokes the session)", Tags: []string{tagSession}, Access: rest.Authenticated(),
		NoRenew:     true, // a renewed token would outlive the revoked session
		Description: "Revokes the caller's login or SSO session until it expires, clears the session cookie and returns the identity provider logout URL for SSO sessions when configured.",
	}, h.deleteSession)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathSessionUserInfo, OperationID: "getUserInfo",
		Summary: "Current user", Tags: []string{tagSession}, Access: rest.Public(),
	}, h.getUserInfo)
	t.Add(rest.Route{
		Method: http.MethodGet, Path: apiclient.PathAuthLogin, OperationID: "startSSOLogin",
		Summary: "Start the OIDC login flow (browser navigation)", Tags: []string{tagSession}, Access: rest.Public(),
		Request:   reflect.TypeFor[SSOLoginQuery](),
		Audit:     rest.AuditNever,
		Handler:   http.HandlerFunc(h.startSSOLogin),
		Responses: []rest.ResponseDoc{{Status: http.StatusSeeOther, Description: "Redirect to the identity provider.", Headers: []string{"Location", "Set-Cookie"}}},
	})
	t.Add(rest.Route{
		Method: http.MethodGet, Path: apiclient.PathAuthCallback, OperationID: "ssoCallback",
		Summary: "OIDC callback (IdP redirect target)", Tags: []string{tagSession}, Access: rest.Public(),
		Request:   reflect.TypeFor[SSOCallbackQuery](),
		Audit:     rest.AuditAlways,
		Handler:   http.HandlerFunc(h.ssoCallback),
		Responses: []rest.ResponseDoc{{Status: http.StatusSeeOther, Description: "Session cookie set; redirect to the return URL.", Headers: []string{"Location", "Set-Cookie"}}},
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathSessionOIDCExchange, OperationID: "exchangeOIDCToken",
		Summary: "Exchange an ID token for a Patrol session (CLI SSO)", Tags: []string{tagSession}, Access: rest.Public(), RateLimited: true,
	}, h.exchangeOIDCToken)
	t.Add(rest.Route{
		Path: "/api/dex/", OperationID: "dexProxy", Summary: "Reverse proxy to the bundled Dex",
		Access: rest.Public(), SkipAuth: true, Hidden: true, Audit: rest.AuditNever,
		Handler: http.HandlerFunc(h.dexProxy),
	})

	// Accounts.
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathAccounts, OperationID: "listAccounts",
		Summary: "List accounts", Tags: []string{tagAccounts},
		Access: rest.Access{Mode: rest.AccessAuthenticated, Note: "items filtered by accounts, get, <name> (own account always visible to local sessions)"},
	}, h.listAccounts)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathAccount, OperationID: "getAccount",
		Summary: "Get account", Tags: []string{tagAccounts},
		Access: withNote(rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionGet, "{name}"), "self allowed for local sessions"),
	}, h.getAccount)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathAccountCanI, OperationID: "canI",
		Summary: "RBAC check for the caller (subresource may contain slashes)", Tags: []string{tagAccounts},
		Access: rest.Authenticated(),
	}, h.canI)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: apiclient.PathAccountPassword, OperationID: "updatePassword",
		Summary: "Change password", Tags: []string{tagAccounts},
		Access:  withNote(rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionUpdate, "<name>"), "self allowed for local sessions with the current password"),
		NoRenew: true, // sessions issued before the change stop verifying; never hand out a replacement
	}, h.updatePassword)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathAccountToken, OperationID: "createAccountToken",
		Summary: "Create API token", Tags: []string{tagAccounts},
		Access: withNote(rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionUpdate, "{name}"), "self allowed for local sessions"),
	}, h.createAccountToken)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathAccountTokenID, OperationID: "deleteAccountToken",
		Summary: "Delete API token", Tags: []string{tagAccounts},
		Access: withNote(rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionUpdate, "{name}"), "self allowed for local sessions"),
	}, h.deleteAccountToken)

	// Settings.
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathSettings, OperationID: "getSettings",
		Summary: "UI/CLI settings (public subset when unauthenticated)", Tags: []string{tagSettings}, Access: rest.Public(),
	}, h.getSettings)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathSettingsPlugins, OperationID: "getPluginSettings",
		Summary: "Config management plugins", Tags: []string{tagSettings}, Access: rest.Authenticated(),
	}, h.getPluginSettings)

	// Version.
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathVersion, OperationID: "getVersion",
		Summary: "Build and tool versions", Tags: []string{tagVersion}, Access: rest.Public(),
		Description: "Callers that are not logged in only receive `version`, unless anonymous access is enabled.",
	}, h.getVersion)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathVersionAlias, OperationID: "getVersionAlias",
		Summary: "Build and tool versions (alias)", Tags: []string{tagVersion}, Access: rest.Public(),
	}, h.getVersion)
}

func withNote(a rest.Access, note string) rest.Access {
	a.Note = note
	return a
}
