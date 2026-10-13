// Package oidc implements Patrol CD's OpenID Connect relying party: lazy provider
// discovery with retry, the authorization-code flow with state, nonce and PKCE kept in an
// encrypted short-lived cookie, ID token verification with audience rules, group claim
// extraction (including the userinfo endpoint), and helpers for the optional bundled Dex.
//
// The package returns verified Claims; minting a Patrol session from them is done by the
// caller with auth.TokenService.IssueSSOSession.
package oidc

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Errors returned by this package. Errors caused by user input or the IdP's answer wrap
// one of these sentinels; callers map ErrInvalidState, ErrStateMismatch, ErrNonceMismatch,
// ErrIDTokenInvalid and ErrProviderError to 4xx/401 and everything else to 5xx.
var (
	ErrInvalidConfig       = errors.New("invalid OIDC configuration")
	ErrDiscovery           = errors.New("OIDC provider discovery failed")
	ErrInvalidState        = errors.New("invalid or expired OIDC state cookie")
	ErrStateMismatch       = errors.New("OIDC state does not match")
	ErrNonceMismatch       = errors.New("ID token nonce does not match")
	ErrMissingCode         = errors.New("authorization code missing from callback")
	ErrExchange            = errors.New("authorization code exchange failed")
	ErrMissingIDToken      = errors.New("no id_token in token response")
	ErrIDTokenInvalid      = errors.New("ID token verification failed")
	ErrAudience            = errors.New("ID token audience not allowed")
	ErrProviderError       = errors.New("identity provider returned an error")
	ErrUserInfo            = errors.New("userinfo request failed")
	ErrUserInfoUnavailable = errors.New("userinfo groups required but not available")
	ErrTooManyGroups       = errors.New("too many groups in claims")
)

// Claim is an entry of the OIDC "claims" request parameter (OIDC Core §5.5.1).
type Claim struct {
	Essential bool     `json:"essential,omitempty"`
	Value     string   `json:"value,omitempty"`
	Values    []string `json:"values,omitempty"`
}

// Config is the relying-party configuration (the "oidc.config" settings block, or the
// result of DexConfig).
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// CLIClientID is the client used by the CLI; it is accepted as an audience by default.
	CLIClientID string
	// RequestedScopes defaults to DefaultScopes. "openid" is always requested.
	RequestedScopes []string
	// RequestedIDTokenClaims is sent as the "claims" authorization parameter.
	RequestedIDTokenClaims map[string]*Claim
	// RootCA is a PEM bundle. When set, only these CAs are trusted for the IdP (upstream
	// semantics).
	RootCA     string
	EnablePKCE bool
	// LogoutURL may contain {{token}} and {{logoutRedirectURL}} placeholders.
	LogoutURL string
	// UserInfoPath is appended to UserInfoBaseURL (default: the issuer URL) to reach the
	// userinfo endpoint; empty selects the discovered userinfo_endpoint.
	UserInfoPath string
	// UserInfoBaseURL optionally overrides the base URL for UserInfoPath.
	UserInfoBaseURL string
	// EnableUserInfoGroups fetches groups from the userinfo endpoint at login.
	EnableUserInfoGroups bool
	// UserInfoCacheExpiration caps how long userinfo groups are cached (0: until the ID
	// token expires).
	UserInfoCacheExpiration time.Duration
	// InsecureSkipVerify disables TLS verification towards the IdP. Never use in
	// production.
	InsecureSkipVerify bool
	// DomainHint is sent as the "domain_hint" authorization parameter.
	DomainHint string
	// Audiences lists accepted "aud" values. Empty means ClientID plus CLIClientID.
	Audiences []string
	// SkipAudienceCheckWhenTokenHasNoAudience accepts ID tokens without "aud".
	SkipAudienceCheckWhenTokenHasNoAudience bool
	// GroupsClaim is the claim holding group membership (default "groups"). A dotted
	// name that is not a top-level claim is resolved through nested objects
	// (e.g. "realm_access.roles").
	GroupsClaim string
}

// DefaultScopes are requested when RequestedScopes is empty (upstream default).
var DefaultScopes = []string{"openid", "profile", "email", "groups"}

// DefaultGroupsClaim is the default group claim name.
const DefaultGroupsClaim = "groups"

// Validate checks the configuration for required and well-formed fields.
func (c Config) Validate() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%w: issuer must be an absolute http(s) URL", ErrInvalidConfig)
	}
	if c.ClientID == "" {
		return fmt.Errorf("%w: clientID is required", ErrInvalidConfig)
	}
	if c.UserInfoBaseURL != "" {
		if b, err := url.Parse(c.UserInfoBaseURL); err != nil || b.Host == "" || (b.Scheme != "https" && b.Scheme != "http") {
			return fmt.Errorf("%w: userInfoBaseURL must be an absolute http(s) URL", ErrInvalidConfig)
		}
	}
	if c.UserInfoPath != "" && !strings.HasPrefix(c.UserInfoPath, "/") {
		return fmt.Errorf("%w: userInfoPath must start with '/'", ErrInvalidConfig)
	}
	if c.UserInfoCacheExpiration < 0 {
		return fmt.Errorf("%w: userInfoCacheExpiration must not be negative", ErrInvalidConfig)
	}
	return nil
}

// Scopes returns the scopes to request, always including "openid".
func (c Config) Scopes() []string {
	scopes := c.RequestedScopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	if !slices.Contains(scopes, "openid") {
		return append([]string{"openid"}, scopes...)
	}
	return slices.Clone(scopes)
}

// AllowedAudiences returns the accepted "aud" values.
func (c Config) AllowedAudiences() []string {
	if len(c.Audiences) > 0 {
		return slices.Clone(c.Audiences)
	}
	out := []string{c.ClientID}
	if c.CLIClientID != "" && c.CLIClientID != c.ClientID {
		out = append(out, c.CLIClientID)
	}
	return out
}

func (c Config) groupsClaim() string {
	if c.GroupsClaim == "" {
		return DefaultGroupsClaim
	}
	return c.GroupsClaim
}

// BuildLogoutURL fills the LogoutURL template: {{token}} with idToken and
// {{logoutRedirectURL}} with redirectURL (both query-escaped). It returns "" when no
// LogoutURL is configured.
func (c Config) BuildLogoutURL(idToken, redirectURL string) string {
	if c.LogoutURL == "" {
		return ""
	}
	return strings.NewReplacer(
		"{{token}}", url.QueryEscape(idToken),
		"{{logoutRedirectURL}}", url.QueryEscape(redirectURL),
	).Replace(c.LogoutURL)
}
