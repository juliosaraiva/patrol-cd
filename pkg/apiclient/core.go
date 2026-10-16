package apiclient

import (
	"context"
	"net/http"
)

// Route patterns of the session, account, settings and version API.
const (
	PathSession             = "/api/v1/session"
	PathSessionUserInfo     = "/api/v1/session/userinfo"
	PathSessionOIDCExchange = "/api/v1/session/oidc-exchange"
	PathAuthLogin           = "/api/v1/auth/login"
	PathAuthCallback        = "/api/v1/auth/callback"
	PathAccounts            = "/api/v1/account"
	PathAccount             = "/api/v1/account/{name}"
	PathAccountCanI         = "/api/v1/account/can-i/{resource}/{action}/{subresource...}"
	PathAccountPassword     = "/api/v1/account/password"
	PathAccountToken        = "/api/v1/account/{name}/token"
	PathAccountTokenID      = "/api/v1/account/{name}/token/{id}"
	PathSettings            = "/api/v1/settings"
	PathSettingsPlugins     = "/api/v1/settings/plugins"
	PathVersion             = "/api/v1/version"
	PathVersionAlias        = "/api/version"
)

// Login creates a local session and, on success, makes the client use its token.
func (c *Client) Login(ctx context.Context, username, password string) (*SessionResponse, error) {
	var out SessionResponse
	if err := c.Do(ctx, http.MethodPost, PathSession, &SessionCreateRequest{Username: username, Password: password}, &out); err != nil {
		return nil, err
	}
	if out.Token != "" {
		c.SetToken(out.Token)
	}
	return &out, nil
}

// Logout revokes the client's session and clears its token. A renewed token on the
// logout response is ignored (OnTokenRenewed is not called).
func (c *Client) Logout(ctx context.Context) (*LogoutResponse, error) {
	var out LogoutResponse
	if err := c.Do(context.WithValue(ctx, noRenewalKey{}, true), http.MethodDelete, PathSession, nil, &out); err != nil {
		return nil, err
	}
	c.SetToken("")
	return &out, nil
}

// UserInfo describes the caller.
func (c *Client) UserInfo(ctx context.Context) (*UserInfo, error) {
	var out UserInfo
	return &out, c.Do(ctx, http.MethodGet, PathSessionUserInfo, nil, &out)
}

// ExchangeOIDCToken exchanges an identity provider ID token for a Patrol session and, on
// success, makes the client use it.
func (c *Client) ExchangeOIDCToken(ctx context.Context, req *OIDCExchangeRequest) (*SessionResponse, error) {
	var out SessionResponse
	if err := c.Do(ctx, http.MethodPost, PathSessionOIDCExchange, req, &out); err != nil {
		return nil, err
	}
	if out.Token != "" {
		c.SetToken(out.Token)
	}
	return &out, nil
}

// ListAccounts lists the accounts the caller may see.
func (c *Client) ListAccounts(ctx context.Context) (*AccountList, error) {
	var out AccountList
	return &out, c.Do(ctx, http.MethodGet, PathAccounts, nil, &out)
}

// GetAccount returns one account.
func (c *Client) GetAccount(ctx context.Context, name string) (*Account, error) {
	var out Account
	return &out, c.Do(ctx, http.MethodGet, PathAccount, &GetAccountRequest{Name: name}, &out)
}

// CanI reports whether the caller may perform action on resource/subresource.
func (c *Client) CanI(ctx context.Context, resource, action, subresource string) (bool, error) {
	if subresource == "" {
		subresource = "*"
	}
	var out CanIResponse
	if err := c.Do(ctx, http.MethodGet, PathAccountCanI, &CanIRequest{Resource: resource, Action: action, Subresource: subresource}, &out); err != nil {
		return false, err
	}
	return out.Value == "yes", nil
}

// UpdatePassword changes an account password (name "" means the caller).
func (c *Client) UpdatePassword(ctx context.Context, req *UpdatePasswordRequest) error {
	return c.Do(ctx, http.MethodPut, PathAccountPassword, req, nil)
}

// CreateAccountToken creates an API token for an account.
func (c *Client) CreateAccountToken(ctx context.Context, req *CreateAccountTokenRequest) (*TokenResponse, error) {
	var out TokenResponse
	return &out, c.Do(ctx, http.MethodPost, PathAccountToken, req, &out)
}

// DeleteAccountToken deletes (revokes) an account API token.
func (c *Client) DeleteAccountToken(ctx context.Context, name, id string) error {
	return c.Do(ctx, http.MethodDelete, PathAccountTokenID, &DeleteAccountTokenRequest{Name: name, ID: id}, nil)
}

// GetSettings returns the UI/CLI settings (public subset when unauthenticated).
func (c *Client) GetSettings(ctx context.Context) (*Settings, error) {
	var out Settings
	return &out, c.Do(ctx, http.MethodGet, PathSettings, nil, &out)
}

// GetPluginSettings lists the config management plugins.
func (c *Client) GetPluginSettings(ctx context.Context) (*PluginSettings, error) {
	var out PluginSettings
	return &out, c.Do(ctx, http.MethodGet, PathSettingsPlugins, nil, &out)
}

// Version returns the server version (only Version when the caller is not logged in and
// anonymous access is disabled).
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var out Version
	return &out, c.Do(ctx, http.MethodGet, PathVersion, nil, &out)
}
