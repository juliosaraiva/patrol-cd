package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

const maxProviderErrorText = 256

func (p *Provider) oauth2Config(ctx context.Context, redirectURL string) (*oauth2.Config, error) {
	prov, err := p.provider(ctx)
	if err != nil {
		return nil, err
	}
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		Scopes:       p.cfg.Scopes(),
		RedirectURL:  redirectURL,
	}, nil
}

// BuildAuthURL returns the IdP authorization URL for the authorization-code flow with
// the given state, nonce and (when non-empty) PKCE S256 challenge derived from
// pkceVerifier. redirectURL is the callback URL registered with the IdP. When PKCE is
// enabled in the configuration a verifier is required.
func (p *Provider) BuildAuthURL(ctx context.Context, state, nonce, pkceVerifier, redirectURL string) (string, error) {
	if state == "" || nonce == "" {
		return "", errors.New("state and nonce are required")
	}
	if p.cfg.EnablePKCE && pkceVerifier == "" {
		return "", fmt.Errorf("%w: PKCE is enabled but no verifier was supplied", ErrInvalidConfig)
	}
	if u, err := url.Parse(redirectURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%w: redirect URL must be an absolute http(s) URL", ErrInvalidConfig)
	}
	cfg, err := p.oauth2Config(ctx, redirectURL)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{gooidc.Nonce(nonce)}
	if pkceVerifier != "" {
		opts = append(opts, oauth2.S256ChallengeOption(pkceVerifier))
	}
	if len(p.cfg.RequestedIDTokenClaims) > 0 {
		b, err := json.Marshal(struct {
			IDToken map[string]*Claim `json:"id_token"`
		}{p.cfg.RequestedIDTokenClaims})
		if err != nil {
			return "", fmt.Errorf("encode claims request: %w", err)
		}
		opts = append(opts, oauth2.SetAuthURLParam("claims", string(b)))
	}
	if hint := strings.TrimSpace(p.cfg.DomainHint); hint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("domain_hint", hint))
	}
	return cfg.AuthCodeURL(state, opts...), nil
}

// LoginRequest describes a browser login.
type LoginRequest struct {
	// RedirectURL is the callback URL (e.g. https://patrol.example.com/auth/callback).
	RedirectURL string
	// ReturnURL is where the browser goes after login. It must pass
	// auth.ValidateRedirectURL against AllowedReturnBaseURLs; empty is allowed.
	ReturnURL             string
	AllowedReturnBaseURLs []string
}

// StartLogin validates the return URL, generates state/nonce/PKCE, and returns the
// authorization URL and the encoded state for the StateCookie.
func (p *Provider) StartLogin(ctx context.Context, codec *StateCodec, req LoginRequest) (authURL, stateCookieValue string, err error) {
	if err := auth.ValidateRedirectURL(req.ReturnURL, req.AllowedReturnBaseURLs); err != nil {
		return "", "", err
	}
	st, err := NewLoginState(req.RedirectURL, req.ReturnURL, p.cfg.EnablePKCE, p.now())
	if err != nil {
		return "", "", err
	}
	authURL, err = p.BuildAuthURL(ctx, st.State, st.Nonce, st.PKCEVerifier, st.RedirectURL)
	if err != nil {
		return "", "", err
	}
	cookie, err := codec.Encode(st)
	if err != nil {
		return "", "", err
	}
	return authURL, cookie, nil
}

// Callback holds the parameters the IdP sent to the callback URL.
type Callback struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

// CallbackFromRequest reads the callback parameters from the request query.
func CallbackFromRequest(r *http.Request) Callback {
	q := r.URL.Query()
	return Callback{
		Code:             q.Get("code"),
		State:            q.Get("state"),
		Error:            q.Get("error"),
		ErrorDescription: q.Get("error_description"),
	}
}

// ProviderError is an error reported by the IdP on the callback. Its fields are
// untrusted text: escape before rendering.
type ProviderError struct {
	Code        string
	Description string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrProviderError, e.Code, e.Description)
}

// Unwrap returns ErrProviderError.
func (e *ProviderError) Unwrap() error { return ErrProviderError }

// Result is the outcome of a successful callback.
type Result struct {
	Claims Claims
	// ReturnURL is the validated post-login destination ("" means the default).
	ReturnURL string
	// RawIDToken is the IdP ID token (e.g. for the logout URL {{token}}). Never log it.
	RawIDToken string
}

// HandleCallback completes the authorization-code flow: it authenticates the state
// cookie, compares the state parameter in constant time, exchanges the code (sending the
// PKCE verifier), verifies the ID token (signature, issuer, expiry, audience, nonce and,
// when present, at_hash), and extracts claims. With EnableUserInfoGroups the groups are
// fetched from the userinfo endpoint (fresh, then cached). The caller must clear the
// state cookie whatever the outcome.
func (p *Provider) HandleCallback(ctx context.Context, cb Callback, stateCookie string, codec *StateCodec) (*Result, error) {
	if cb.Error != "" {
		return nil, &ProviderError{Code: truncate(cb.Error), Description: truncate(cb.ErrorDescription)}
	}
	st, err := codec.Decode(stateCookie)
	if err != nil {
		return nil, err
	}
	if cb.State == "" || subtle.ConstantTimeCompare([]byte(cb.State), []byte(st.State)) != 1 {
		return nil, ErrStateMismatch
	}
	if cb.Code == "" {
		return nil, ErrMissingCode
	}
	if p.cfg.EnablePKCE && st.PKCEVerifier == "" {
		return nil, fmt.Errorf("%w: PKCE verifier missing from state", ErrInvalidState)
	}
	cfg, err := p.oauth2Config(ctx, st.RedirectURL)
	if err != nil {
		return nil, err
	}
	var opts []oauth2.AuthCodeOption
	if st.PKCEVerifier != "" {
		opts = append(opts, oauth2.VerifierOption(st.PKCEVerifier))
	}
	tok, err := cfg.Exchange(gooidc.ClientContext(ctx, p.client), cb.Code, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExchange, err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, ErrMissingIDToken
	}
	idt, err := p.VerifyIDToken(ctx, rawID)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		return nil, ErrNonceMismatch
	}
	if idt.AccessTokenHash != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIDTokenInvalid, err)
		}
	}
	raw := map[string]any{}
	if err := idt.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIDTokenInvalid, err)
	}
	claims, err := claimsFromRaw(raw, p.cfg.groupsClaim())
	if err != nil {
		return nil, err
	}
	if p.cfg.EnableUserInfoGroups {
		groups, err := p.fetchUserInfoGroups(ctx, tok.AccessToken, claims.Sub, idt.Expiry)
		if err != nil {
			return nil, err
		}
		claims.Groups = groups
		claims.Raw[p.cfg.groupsClaim()] = slices.Clone(groups)
	}
	return &Result{Claims: claims, ReturnURL: st.ReturnURL, RawIDToken: rawID}, nil
}

func truncate(s string) string {
	if len(s) > maxProviderErrorText {
		return s[:maxProviderErrorText]
	}
	return s
}
