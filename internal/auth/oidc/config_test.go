package oidc

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	ok := Config{Issuer: "https://idp.example.com", ClientID: "c"}
	tests := []struct {
		name   string
		mutate func(*Config)
		valid  bool
	}{
		{name: "minimal", mutate: func(*Config) {}, valid: true},
		{name: "http issuer", mutate: func(c *Config) { c.Issuer = "http://127.0.0.1:5556/dex" }, valid: true},
		{name: "no issuer", mutate: func(c *Config) { c.Issuer = "" }},
		{name: "relative issuer", mutate: func(c *Config) { c.Issuer = "/dex" }},
		{name: "bad scheme", mutate: func(c *Config) { c.Issuer = "ftp://idp" }},
		{name: "no client", mutate: func(c *Config) { c.ClientID = "" }},
		{name: "userinfo path without slash", mutate: func(c *Config) { c.UserInfoPath = "userinfo" }},
		{name: "negative cache", mutate: func(c *Config) { c.UserInfoCacheExpiration = -time.Second }},
		{name: "userinfo base url", mutate: func(c *Config) { c.UserInfoBaseURL = "https://users.example.com" }, valid: true},
		{name: "bad userinfo base url", mutate: func(c *Config) { c.UserInfoBaseURL = "users.example.com" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := ok
			tc.mutate(&c)
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate = %v, valid=%v", err, tc.valid)
			}
			if err != nil && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error does not wrap ErrInvalidConfig: %v", err)
			}
			if _, perr := NewProvider(c); (perr == nil) != tc.valid {
				t.Fatalf("NewProvider = %v", perr)
			}
		})
	}
}

func TestConfigScopesAudiencesLogout(t *testing.T) {
	t.Parallel()
	if got := (Config{}).Scopes(); !slices.Equal(got, DefaultScopes) {
		t.Fatalf("default scopes = %v", got)
	}
	if got := (Config{RequestedScopes: []string{"email"}}).Scopes(); !slices.Equal(got, []string{"openid", "email"}) {
		t.Fatalf("scopes = %v", got)
	}
	if got := (Config{RequestedScopes: []string{"email", "openid"}}).Scopes(); !slices.Equal(got, []string{"email", "openid"}) {
		t.Fatalf("scopes = %v", got)
	}
	if got := (Config{ClientID: "a"}).AllowedAudiences(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("aud = %v", got)
	}
	if got := (Config{ClientID: "a", CLIClientID: "b"}).AllowedAudiences(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("aud = %v", got)
	}
	if got := (Config{ClientID: "a", CLIClientID: "b", Audiences: []string{"z"}}).AllowedAudiences(); !slices.Equal(got, []string{"z"}) {
		t.Fatalf("aud = %v", got)
	}
	c := Config{LogoutURL: "https://idp/logout?id_token_hint={{token}}&post_logout_redirect_uri={{logoutRedirectURL}}"}
	if got, want := c.BuildLogoutURL("a.b.c", "https://patrol.example.com/p?x=1"),
		"https://idp/logout?id_token_hint=a.b.c&post_logout_redirect_uri=https%3A%2F%2Fpatrol.example.com%2Fp%3Fx%3D1"; got != want {
		t.Fatalf("logout = %q, want %q", got, want)
	}
	if (Config{}).BuildLogoutURL("t", "r") != "" {
		t.Fatal("logout URL without template")
	}
}

func TestDexHelpers(t *testing.T) {
	t.Parallel()
	iss, err := DexIssuerURL("https://patrol.example.com/")
	if err != nil || iss != "https://patrol.example.com/api/dex" {
		t.Fatalf("issuer = %q %v", iss, err)
	}
	if _, err := DexIssuerURL("patrol.example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad url: %v", err)
	}
	s1, err := DexClientSecret(serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	s1b, _ := DexClientSecret(serverSecret)
	s2, _ := DexClientSecret(serverSecretPrev)
	if len(s1) != 40 || s1 != s1b || s1 == s2 {
		t.Fatalf("secrets: %q %q %q", s1, s1b, s2)
	}
	if _, err := DexClientSecret([]byte("short")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short secret: %v", err)
	}
	cfg, err := DexConfig("https://patrol.example.com", serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != "https://patrol.example.com/api/dex" || cfg.ClientID != DexClientID || cfg.CLIClientID != DexCLIClientID ||
		cfg.ClientSecret != s1 || !slices.Contains(cfg.Scopes(), DexFederatedScope) ||
		!slices.Equal(cfg.AllowedAudiences(), []string{"patrol", "patrol-cli"}) {
		t.Fatalf("dex config = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := DexConfig("https://patrol.example.com", nil); err == nil {
		t.Fatal("missing secret accepted")
	}
}
