package settings

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	authoidc "github.com/juliosaraiva/patrol-cd/internal/auth/oidc"
)

// OIDCConfig is oidc.config with "$" secret references resolved (TEN-116).
type OIDCConfig struct {
	Name                     string
	Issuer                   string
	ClientID                 string
	ClientSecret             Sensitive
	CLIClientID              string
	EnableUserInfoGroups     bool
	UserInfoBaseURL          string
	UserInfoPath             string
	UserInfoCacheExpiration  time.Duration
	RequestedScopes          []string
	RequestedIDTokenClaims   map[string]*authoidc.Claim
	LogoutURL                string
	RootCA                   string
	EnablePKCEAuthentication bool
	DomainHint               string
	Azure                    *AzureOIDCConfig
	RefreshTokenThreshold    time.Duration
	// AllowedAudiences are the accepted "aud" values (empty: clientID plus cliClientID).
	AllowedAudiences []string
	// SkipAudienceCheckWhenTokenHasNoAudience is nil when unset (false).
	SkipAudienceCheckWhenTokenHasNoAudience *bool
}

// AzureOIDCConfig is the azure block of oidc.config.
type AzureOIDCConfig struct {
	UseWorkloadIdentity                  bool
	EnableUserGroupOverageClaim          bool
	GraphAPIEndpoint                     string
	UserGroupOverageClaimCacheExpiration time.Duration
}

func (c *OIDCConfig) clone() *OIDCConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.RequestedScopes = slices.Clone(c.RequestedScopes)
	out.AllowedAudiences = slices.Clone(c.AllowedAudiences)
	if c.RequestedIDTokenClaims != nil {
		out.RequestedIDTokenClaims = make(map[string]*authoidc.Claim, len(c.RequestedIDTokenClaims))
		for k, v := range c.RequestedIDTokenClaims {
			if v != nil {
				cl := *v
				cl.Values = slices.Clone(v.Values)
				v = &cl
			}
			out.RequestedIDTokenClaims[k] = v
		}
	}
	if c.Azure != nil {
		az := *c.Azure
		out.Azure = &az
	}
	if c.SkipAudienceCheckWhenTokenHasNoAudience != nil {
		b := *c.SkipAudienceCheckWhenTokenHasNoAudience
		out.SkipAudienceCheckWhenTokenHasNoAudience = &b
	}
	return &out
}

// oidcConfigDoc is the YAML shape of oidc.config (upstream keys).
type oidcConfigDoc struct {
	Name                     string                     `json:"name,omitempty"`
	Issuer                   string                     `json:"issuer,omitempty"`
	ClientID                 string                     `json:"clientID,omitempty"`
	ClientSecret             string                     `json:"clientSecret,omitempty"`
	CLIClientID              string                     `json:"cliClientID,omitempty"`
	EnableUserInfoGroups     bool                       `json:"enableUserInfoGroups,omitempty"`
	UserInfoBaseURL          string                     `json:"userInfoBaseURL,omitempty"`
	UserInfoPath             string                     `json:"userInfoPath,omitempty"`
	UserInfoCacheExpiration  string                     `json:"userInfoCacheExpiration,omitempty"`
	RequestedScopes          []string                   `json:"requestedScopes,omitempty"`
	RequestedIDTokenClaims   map[string]*authoidc.Claim `json:"requestedIDTokenClaims,omitempty"`
	LogoutURL                string                     `json:"logoutURL,omitempty"`
	RootCA                   string                     `json:"rootCA,omitempty"`
	EnablePKCEAuthentication bool                       `json:"enablePKCEAuthentication,omitempty"`
	DomainHint               string                     `json:"domainHint,omitempty"`
	Azure                    *struct {
		UseWorkloadIdentity                  bool   `json:"useWorkloadIdentity,omitempty"`
		EnableUserGroupOverageClaim          bool   `json:"enableUserGroupOverageClaim,omitempty"`
		GraphAPIEndpoint                     string `json:"graphApiEndpoint,omitempty"`
		UserGroupOverageClaimCacheExpiration string `json:"userGroupOverageClaimCacheExpiration,omitempty"`
	} `json:"azure,omitempty"`
	RefreshTokenThreshold                   string   `json:"refreshTokenThreshold,omitempty"`
	AllowedAudiences                        []string `json:"allowedAudiences,omitempty"`
	SkipAudienceCheckWhenTokenHasNoAudience *bool    `json:"skipAudienceCheckWhenTokenHasNoAudience,omitempty"`
}

// knownGraphAPIHosts are the trusted Microsoft Graph API hosts (upstream list).
var knownGraphAPIHosts = map[string]bool{
	"graph.microsoft.com":             true,
	"graph.microsoft.us":              true,
	"graph.microsoft.de":              true,
	"microsoftgraph.chinacloudapi.cn": true,
}

// DefaultAzureGraphAPIEndpoint is used when azure.graphApiEndpoint is unset.
const DefaultAzureGraphAPIEndpoint = "https://graph.microsoft.com/v1.0"

// ValidateAzureGraphAPIEndpoint returns an error unless endpoint is an https URL of a known
// Microsoft Graph host (upstream wording).
func ValidateAzureGraphAPIEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid graphApiEndpoint URL %q: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("graphApiEndpoint %q must use https", endpoint)
	}
	if !knownGraphAPIHosts[u.Hostname()] {
		return fmt.Errorf("graphApiEndpoint %q hostname is not a known Microsoft Graph API host", endpoint)
	}
	return nil
}

// parseOIDC applies oidc.config.
func (p *parser) parseOIDC(c *cmKeys) {
	src := SourceConfigMap
	c.apply(KeyOIDCConfig, func(v string) error {
		if v == "" {
			p.s.oidcConfig = nil
			return nil
		}
		var raw map[string]any
		if err := yaml.Unmarshal([]byte(v), &raw); err != nil {
			return fmt.Errorf("invalid oidc config: %w", err)
		}
		if raw == nil {
			return errors.New("invalid oidc config: not a YAML map")
		}
		resolved := p.replaceMapSecrets(src, KeyOIDCConfig, raw)
		b, err := yaml.Marshal(resolved)
		if err != nil {
			return fmt.Errorf("invalid oidc config: %w", err)
		}
		var doc oidcConfigDoc
		if err := yaml.UnmarshalStrict(b, &doc); err != nil {
			if err := yaml.Unmarshal(b, &doc); err != nil {
				return fmt.Errorf("invalid oidc config: %w", err)
			}
			p.warn(src, KeyOIDCConfig, "unknown or malformed fields are ignored: %s", redactStrictError(err))
		}
		p.s.oidcConfig = p.oidcFromDoc(doc)
		return nil
	})
}

// redactStrictError keeps only the field names of a strict-decoding error.
func redactStrictError(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

func (p *parser) oidcFromDoc(doc oidcConfigDoc) *OIDCConfig {
	src, key := SourceConfigMap, KeyOIDCConfig
	cfg := &OIDCConfig{
		Name:                                    doc.Name,
		Issuer:                                  doc.Issuer,
		ClientID:                                doc.ClientID,
		ClientSecret:                            NewSensitive(doc.ClientSecret),
		CLIClientID:                             doc.CLIClientID,
		EnableUserInfoGroups:                    doc.EnableUserInfoGroups,
		UserInfoBaseURL:                         doc.UserInfoBaseURL,
		UserInfoPath:                            doc.UserInfoPath,
		RequestedScopes:                         doc.RequestedScopes,
		RequestedIDTokenClaims:                  doc.RequestedIDTokenClaims,
		LogoutURL:                               doc.LogoutURL,
		RootCA:                                  doc.RootCA,
		EnablePKCEAuthentication:                doc.EnablePKCEAuthentication,
		DomainHint:                              doc.DomainHint,
		AllowedAudiences:                        doc.AllowedAudiences,
		SkipAudienceCheckWhenTokenHasNoAudience: doc.SkipAudienceCheckWhenTokenHasNoAudience,
	}
	if err := ValidateExternalURL(doc.Issuer); err != nil {
		p.warn(src, key, "Failed to validate OIDC config: issuer: %v", err)
	}
	if err := ValidateExternalURL(doc.UserInfoBaseURL); err != nil {
		p.warn(src, key, "Failed to validate OIDC config: userInfoBaseURL: %v", err)
	}
	if doc.Issuer == "" {
		p.warn(src, key, "issuer is not set; OIDC login cannot work")
	}
	if doc.ClientID == "" {
		p.warn(src, key, "clientID is not set; OIDC login cannot work")
	}
	if doc.UserInfoCacheExpiration != "" {
		d, err := time.ParseDuration(doc.UserInfoCacheExpiration)
		if err != nil {
			p.warn(src, key, "Failed to parse 'oidc.config.userInfoCacheExpiration' key: %v", err)
		}
		cfg.UserInfoCacheExpiration = d
	}
	if doc.RefreshTokenThreshold != "" {
		d, err := time.ParseDuration(doc.RefreshTokenThreshold)
		if err != nil {
			p.warn(src, key, "Failed to parse 'oidc.config.refreshTokenThreshold' key: %v", err)
		}
		cfg.RefreshTokenThreshold = d
	}
	if doc.RootCA != "" {
		if ok := x509.NewCertPool().AppendCertsFromPEM([]byte(doc.RootCA)); !ok {
			p.warn(src, key, "failed to append certificates from PEM: proceeding without custom rootCA")
			cfg.RootCA = ""
		}
	}
	if doc.Azure != nil {
		az := &AzureOIDCConfig{
			UseWorkloadIdentity:         doc.Azure.UseWorkloadIdentity,
			EnableUserGroupOverageClaim: doc.Azure.EnableUserGroupOverageClaim,
			GraphAPIEndpoint:            doc.Azure.GraphAPIEndpoint,
		}
		if az.GraphAPIEndpoint != "" {
			if err := ValidateAzureGraphAPIEndpoint(az.GraphAPIEndpoint); err != nil {
				p.warn(src, key, "Invalid graphApiEndpoint, Azure groups overage feature disabled: %v", err)
				az.GraphAPIEndpoint = ""
				az.EnableUserGroupOverageClaim = false
			}
		} else {
			az.GraphAPIEndpoint = DefaultAzureGraphAPIEndpoint
		}
		if v := doc.Azure.UserGroupOverageClaimCacheExpiration; v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				p.warn(src, key, "Failed to parse 'oidc.config.azure.userGroupOverageClaimCacheExpiration' key: %v", err)
			}
			az.UserGroupOverageClaimCacheExpiration = d
		}
		cfg.Azure = az
	}
	return cfg
}

// DexConnector summarizes one connector of dex.config (API-041).
type DexConnector struct {
	ID   string
	Name string
	Type string
}

// parseDex applies dex.config.
func (p *parser) parseDex(c *cmKeys) {
	src := SourceConfigMap
	c.apply(KeyDexConfig, func(v string) error {
		cfg, err := UnmarshalDexConfig(v)
		if err != nil {
			return fmt.Errorf("invalid dex.config: %w", err)
		}
		var connectors []DexConnector
		if list, ok := cfg["connectors"].([]any); ok {
			for i, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					p.warn(src, KeyDexConfig, "connectors[%d] is not a map", i)
					continue
				}
				conn := DexConnector{}
				conn.ID, _ = m["id"].(string)
				conn.Name, _ = m["name"].(string)
				conn.Type, _ = m["type"].(string)
				connectors = append(connectors, conn)
			}
		} else if _, present := cfg["connectors"]; present {
			p.warn(src, KeyDexConfig, "connectors is not a list")
		}
		p.s.dexConfig = v
		p.s.dexConnectors = connectors
		p.s.dexConfigured = p.s.url != "" && len(cfg) > 0
		if len(cfg) > 0 && p.s.url == "" {
			p.warn(src, KeyDexConfig, "dex.config is set but url is empty; Dex SSO is disabled")
		}
		return nil
	})
}

// UnmarshalDexConfig decodes dex.config ("" yields an empty map).
func UnmarshalDexConfig(config string) (map[string]any, error) {
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DexConfig returns the raw dex.config (secret references unresolved).
func (s *Settings) DexConfig() string { return s.dexConfig }

// DexConnectors returns the connectors of dex.config.
func (s *Settings) DexConnectors() []DexConnector { return slices.Clone(s.dexConnectors) }

// IsDexConfigured reports whether url is set and dex.config is a non-empty map (TEN-113).
func (s *Settings) IsDexConfigured() bool { return s.dexConfigured }

// OIDCConfig returns oidc.config with secrets resolved, or nil when unset or invalid.
func (s *Settings) OIDCConfig() *OIDCConfig { return s.oidcConfig.clone() }

// IsSSOConfigured reports whether Dex or OIDC is configured.
func (s *Settings) IsSSOConfigured() bool { return s.dexConfigured || s.oidcConfig != nil }

// OIDCTLSInsecureSkipVerify reports oidc.tls.insecure.skip.verify.
func (s *Settings) OIDCTLSInsecureSkipVerify() bool { return s.oidcTLSInsecureSkip }

// DexConfigResolved returns dex.config with "$" references resolved from patrol-secret and
// labelled Secrets. Escaping of "$" for Dex's environment expansion is the Dex launcher's job.
func (s *Settings) DexConfigResolved() (map[string]any, error) {
	cfg, err := UnmarshalDexConfig(s.dexConfig)
	if err != nil {
		return nil, fmt.Errorf("invalid dex.config: %w", err)
	}
	var errs []error
	var walk func(any) any
	walk = func(obj any) any {
		switch v := obj.(type) {
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, x := range v {
				out[k] = walk(x)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, x := range v {
				out[i] = walk(x)
			}
			return out
		case string:
			r, err := s.ResolveSecretRef(v)
			if err != nil {
				errs = append(errs, err)
			}
			return r
		}
		return obj
	}
	out, _ := walk(cfg).(map[string]any)
	return out, errors.Join(errs...)
}

// OIDCProviderConfig returns the relying-party configuration for internal/auth/oidc: the
// external provider from oidc.config, or the bundled Dex when only Dex is configured. ok is
// false when SSO is not configured.
func (s *Settings) OIDCProviderConfig() (cfg authoidc.Config, ok bool, err error) {
	if c := s.oidcConfig; c != nil {
		cfg = authoidc.Config{
			Issuer:                                  c.Issuer,
			ClientID:                                c.ClientID,
			ClientSecret:                            c.ClientSecret.Reveal(),
			CLIClientID:                             c.CLIClientID,
			RequestedScopes:                         slices.Clone(c.RequestedScopes),
			RequestedIDTokenClaims:                  c.clone().RequestedIDTokenClaims,
			RootCA:                                  c.RootCA,
			EnablePKCE:                              c.EnablePKCEAuthentication,
			LogoutURL:                               c.LogoutURL,
			UserInfoPath:                            c.UserInfoPath,
			UserInfoBaseURL:                         c.UserInfoBaseURL,
			EnableUserInfoGroups:                    c.EnableUserInfoGroups,
			UserInfoCacheExpiration:                 c.UserInfoCacheExpiration,
			InsecureSkipVerify:                      s.oidcTLSInsecureSkip,
			DomainHint:                              c.DomainHint,
			Audiences:                               slices.Clone(c.AllowedAudiences),
			SkipAudienceCheckWhenTokenHasNoAudience: c.SkipAudienceCheckWhenTokenHasNoAudience != nil && *c.SkipAudienceCheckWhenTokenHasNoAudience,
		}
		return cfg, true, nil
	}
	if s.dexConfigured {
		cfg, err = authoidc.DexConfig(s.url, s.serverSecretKey.Bytes())
		if err != nil {
			return authoidc.Config{}, true, fmt.Errorf("build Dex relying-party configuration: %w", err)
		}
		cfg.InsecureSkipVerify = s.oidcTLSInsecureSkip
		return cfg, true, nil
	}
	return authoidc.Config{}, false, nil
}
