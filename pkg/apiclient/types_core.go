package apiclient

// Request and response types of the session, account, settings and version routes.
// JSON names follow the Argo CD REST gateway where the semantics match.

// Empty is a request or response without fields ({}).
type Empty struct{}

// SessionCreateRequest is the body of POST /api/v1/session (local login).
type SessionCreateRequest struct {
	// Cookie also sets the patrol.token session cookie (browser login).
	Cookie bool `query:"cookie" json:"-"`

	Username string `json:"username"`
	Password string `json:"password"`
}

// SessionResponse carries a Patrol session token.
type SessionResponse struct {
	Token string `json:"token,omitempty"`
}

// LogoutResponse is returned by DELETE /api/v1/session.
type LogoutResponse struct {
	// LogoutURL is the identity provider's end-session URL for SSO sessions, when
	// oidc.config.logoutURL is configured.
	LogoutURL string `json:"logoutURL,omitempty"`
}

// UserInfo describes the caller (GET /api/v1/session/userinfo).
type UserInfo struct {
	LoggedIn bool `json:"loggedIn"`
	// Username is the account name for local sessions and the email (or user id) for SSO
	// sessions.
	Username string `json:"username,omitempty"`
	// Iss is the issuer of the caller's token: "patrol" for every Patrol-issued session
	// (local and SSO logins, API tokens), the identity provider's issuer for IdP tokens
	// presented directly.
	Iss string `json:"iss,omitempty"`
	// Groups are the values of the claims configured in patrol-rbac-cm "scopes".
	Groups []string `json:"groups,omitempty"`
}

// OIDCExchangeRequest exchanges an identity provider ID token (obtained by the CLI with
// the authorization-code + PKCE flow against the CLI client) for a Patrol session.
type OIDCExchangeRequest struct {
	IDToken string `json:"idToken"`
	// AccessToken is required when oidc.config.enableUserInfoGroups is set: groups are
	// then read from the userinfo endpoint with it.
	AccessToken string `json:"accessToken,omitempty"`
}

// AccountToken is the metadata of an account API token.
type AccountToken struct {
	ID string `json:"id"`
	// IssuedAt is a Unix timestamp (seconds).
	IssuedAt int64 `json:"issuedAt,omitempty"`
	// ExpiresAt is a Unix timestamp (seconds); 0 means the token does not expire.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// Account is a local account.
type Account struct {
	Name         string         `json:"name"`
	Enabled      bool           `json:"enabled"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Tokens       []AccountToken `json:"tokens,omitempty"`
}

// AccountList is the response of GET /api/v1/account.
type AccountList struct {
	Items []Account `json:"items"`
}

// GetAccountRequest addresses GET /api/v1/account/{name}.
type GetAccountRequest struct {
	Name string `path:"name" json:"-"`
}

// CanIRequest addresses GET /api/v1/account/can-i/{resource}/{action}/{subresource...}.
type CanIRequest struct {
	Resource    string `path:"resource" json:"-"`
	Action      string `path:"action" json:"-"`
	Subresource string `path:"subresource" json:"-"`
}

// CanIResponse answers a can-i question.
type CanIResponse struct {
	// Value is "yes" or "no".
	Value string `json:"value"`
}

// UpdatePasswordRequest is the body of PUT /api/v1/account/password.
type UpdatePasswordRequest struct {
	// Name is the account to update; empty means the caller.
	Name            string `json:"name,omitempty"`
	CurrentPassword string `json:"currentPassword,omitempty"`
	NewPassword     string `json:"newPassword"`
}

// CreateAccountTokenRequest is POST /api/v1/account/{name}/token.
type CreateAccountTokenRequest struct {
	Name string `path:"name" json:"-"`

	// ID is the token ID; empty generates a UUID.
	ID string `json:"id,omitempty"`
	// ExpiresIn is the lifetime in seconds; 0 creates a token without expiry.
	ExpiresIn int64 `json:"expiresIn,omitempty"`
}

// TokenResponse carries a newly created API token.
type TokenResponse struct {
	Token string `json:"token"`
}

// DeleteAccountTokenRequest addresses DELETE /api/v1/account/{name}/token/{id}.
type DeleteAccountTokenRequest struct {
	Name string `path:"name" json:"-"`
	ID   string `path:"id" json:"-"`
}

// Settings is the response of GET /api/v1/settings. Fields marked "authenticated" are
// only populated for logged-in callers (or when authentication is disabled);
// PasswordPattern only for logged-in callers.
type Settings struct {
	URL                       string                      `json:"url,omitempty"`
	AdditionalURLs            []string                    `json:"additionalUrls,omitempty"`
	DexConfig                 *DexConfig                  `json:"dexConfig,omitempty"`
	OIDCConfig                *OIDCConfig                 `json:"oidcConfig,omitempty"`
	AppLabelKey               string                      `json:"appLabelKey,omitempty"`
	ResourceOverrides         map[string]ResourceOverride `json:"resourceOverrides,omitempty"`
	StatusBadgeEnabled        bool                        `json:"statusBadgeEnabled,omitempty"`
	StatusBadgeRootURL        string                      `json:"statusBadgeRootUrl,omitempty"`
	GoogleAnalytics           *GoogleAnalyticsConfig      `json:"googleAnalytics,omitempty"`
	KustomizeOptions          *KustomizeOptions           `json:"kustomizeOptions,omitempty"`
	Help                      *Help                       `json:"help,omitempty"`
	UserLoginsDisabled        bool                        `json:"userLoginsDisabled,omitempty"`
	AnonymousEnabled          bool                        `json:"anonymousEnabled,omitempty"`
	KustomizeVersions         []string                    `json:"kustomizeVersions,omitempty"`
	UICSSURL                  string                      `json:"uiCssURL,omitempty"`
	UIBannerContent           string                      `json:"uiBannerContent,omitempty"`
	UIBannerURL               string                      `json:"uiBannerURL,omitempty"`
	UIBannerPermanent         bool                        `json:"uiBannerPermanent,omitempty"`
	UIBannerPosition          string                      `json:"uiBannerPosition,omitempty"`
	PasswordPattern           string                      `json:"passwordPattern,omitempty"`
	TrackingMethod            string                      `json:"trackingMethod,omitempty"`
	ExecEnabled               bool                        `json:"execEnabled,omitempty"`
	ControllerNamespace       string                      `json:"controllerNamespace,omitempty"`
	AppsInAnyNamespaceEnabled bool                        `json:"appsInAnyNamespaceEnabled,omitempty"`
	ImpersonationEnabled      bool                        `json:"impersonationEnabled,omitempty"`
	InstallationID            string                      `json:"installationID,omitempty"`
	HydratorEnabled           bool                        `json:"hydratorEnabled,omitempty"`
	SyncWithReplaceAllowed    bool                        `json:"syncWithReplaceAllowed,omitempty"`
}

// DexConfig is the public part of dex.config.
type DexConfig struct {
	Connectors []DexConnector `json:"connectors,omitempty"`
}

// DexConnector names one Dex connector.
type DexConnector struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// OIDCConfig is the public part of oidc.config (never secrets).
type OIDCConfig struct {
	Name                     string                `json:"name,omitempty"`
	Issuer                   string                `json:"issuer,omitempty"`
	ClientID                 string                `json:"clientID,omitempty"`
	CLIClientID              string                `json:"cliClientID,omitempty"`
	Scopes                   []string              `json:"scopes,omitempty"`
	IDTokenClaims            map[string]*OIDCClaim `json:"idTokenClaims,omitempty"`
	EnablePKCEAuthentication bool                  `json:"enablePKCEAuthentication,omitempty"`
}

// OIDCClaim is one requested ID token claim.
type OIDCClaim struct {
	Essential bool     `json:"essential,omitempty"`
	Value     string   `json:"value,omitempty"`
	Values    []string `json:"values,omitempty"`
}

// ResourceOverride is a resource.customizations entry.
type ResourceOverride struct {
	HealthLua             string              `json:"health.lua,omitempty"`
	UseOpenLibs           bool                `json:"health.lua.useOpenLibs,omitempty"`
	Actions               string              `json:"actions,omitempty"`
	IgnoreDifferences     *OverrideIgnoreDiff `json:"ignoreDifferences,omitempty"`
	IgnoreResourceUpdates *OverrideIgnoreDiff `json:"ignoreResourceUpdates,omitempty"`
	KnownTypeFields       []KnownTypeField    `json:"knownTypeFields,omitempty"`
}

// OverrideIgnoreDiff lists fields ignored by diffing (or by refresh triggering).
type OverrideIgnoreDiff struct {
	JSONPointers          []string `json:"jsonPointers,omitempty"`
	JQPathExpressions     []string `json:"jqPathExpressions,omitempty"`
	ManagedFieldsManagers []string `json:"managedFieldsManagers,omitempty"`
}

// KnownTypeField declares the type of a CRD field.
type KnownTypeField struct {
	Field string `json:"field,omitempty"`
	Type  string `json:"type,omitempty"`
}

// GoogleAnalyticsConfig is ga.*.
type GoogleAnalyticsConfig struct {
	TrackingID     string `json:"trackingID,omitempty"`
	AnonymizeUsers bool   `json:"anonymizeUsers,omitempty"`
}

// KustomizeOptions are global kustomize build options.
type KustomizeOptions struct {
	BuildOptions string `json:"buildOptions,omitempty"`
}

// Help is help.*.
type Help struct {
	ChatURL    string            `json:"chatUrl,omitempty"`
	ChatText   string            `json:"chatText,omitempty"`
	BinaryURLs map[string]string `json:"binaryUrls,omitempty"`
}

// PluginSettings is the response of GET /api/v1/settings/plugins.
type PluginSettings struct {
	Plugins []Plugin `json:"plugins,omitempty"`
}

// Plugin is a config management plugin served by a repo-server sidecar.
type Plugin struct {
	Name string `json:"name"`
}

// Version is the response of GET /api/v1/version. Unauthenticated callers (when
// anonymous access is disabled) only receive Version.
type Version struct {
	Version          string            `json:"version"`
	BuildDate        string            `json:"buildDate,omitempty"`
	GitCommit        string            `json:"gitCommit,omitempty"`
	GitTag           string            `json:"gitTag,omitempty"`
	GitTreeState     string            `json:"gitTreeState,omitempty"`
	GoVersion        string            `json:"goVersion,omitempty"`
	Compiler         string            `json:"compiler,omitempty"`
	Platform         string            `json:"platform,omitempty"`
	KustomizeVersion string            `json:"kustomizeVersion,omitempty"`
	HelmVersion      string            `json:"helmVersion,omitempty"`
	KubectlVersion   string            `json:"kubectlVersion,omitempty"`
	JsonnetVersion   string            `json:"jsonnetVersion,omitempty"`
	ExtraBuildInfo   map[string]string `json:"extraBuildInfo,omitempty"`
}

// WatchEvent is the data of one Server-Sent Event of a watch stream. Watch streams send
// no bookmark or error watch events: a failure ends the stream with an "error"
// Server-Sent Event carrying a problem document (Watch returns it as *APIError).
type WatchEvent[T any] struct {
	// Type is ADDED, MODIFIED or DELETED.
	Type   string `json:"type" jsonschema:"enum=ADDED,enum=MODIFIED,enum=DELETED"`
	Object T      `json:"object"`
}

// Watch event types (the values of WatchEvent.Type; the jsonschema enum tags of the
// watch event types list the same values).
const (
	EventAdded    = "ADDED"
	EventModified = "MODIFIED"
	EventDeleted  = "DELETED"
)
