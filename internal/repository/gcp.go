package repository

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GCPScope is the OAuth scope of tokens for Google Cloud Source Repositories (upstream
// GoogleCloudCreds).
const GCPScope = "https://www.googleapis.com/auth/cloud-platform"

// gcpRequestTimeout bounds one token request.
const gcpRequestTimeout = 30 * time.Second

// errGCPInvalid is upstream's message for unusable Google credentials.
const errGCPInvalid = "credentials for Google Cloud Source repositories are invalid"

// gcpCredentials is what Patrol reads from a Google credentials JSON before using it.
type gcpCredentials struct {
	credType google.CredentialsType
	email    string
	// endpoints are the URLs the credentials make the minting component call (token_uri,
	// service_account_impersonation_url, and those of source_credentials).
	endpoints []gcpEndpoint
}

type gcpEndpoint struct{ field, url string }

// gcpCredentialType maps the "type" of a credentials JSON to the accepted credential types
// and collects the endpoints it names. Upstream also accepts external_account; Patrol rejects
// it because its credential_source can make the minting component read local files, run
// executables or call arbitrary URLs (this runs in the controller and API server, which hold
// Kubernetes credentials).
func gcpCredentialType(keyJSON []byte) (gcpCredentials, error) {
	var f struct {
		Type                           string          `json:"type"`
		ClientEmail                    string          `json:"client_email"`
		PrivateKey                     string          `json:"private_key"`
		TokenURI                       string          `json:"token_uri"`
		ServiceAccountImpersonationURL string          `json:"service_account_impersonation_url"`
		SourceCredentials              json.RawMessage `json:"source_credentials"`
	}
	if err := json.Unmarshal(keyJSON, &f); err != nil {
		return gcpCredentials{}, fmt.Errorf("%s: %w", errGCPInvalid, ErrInvalidCredentials)
	}
	var out gcpCredentials
	if f.TokenURI != "" {
		out.endpoints = append(out.endpoints, gcpEndpoint{"token_uri", f.TokenURI})
	}
	out.email = f.ClientEmail
	switch f.Type {
	case "", string(google.ServiceAccount):
		if f.ClientEmail == "" {
			return gcpCredentials{}, fmt.Errorf("%s: client_email is missing: %w", errGCPInvalid, ErrInvalidCredentials)
		}
		if !validPrivateKey(f.PrivateKey) {
			return gcpCredentials{}, fmt.Errorf("%s: private_key is not a PEM encoded PKCS#8 or PKCS#1 RSA key: %w", errGCPInvalid, ErrInvalidCredentials)
		}
		out.credType = google.ServiceAccount
	case string(google.AuthorizedUser):
		out.credType = google.AuthorizedUser
	case string(google.ImpersonatedServiceAccount):
		var src struct {
			Type     string `json:"type"`
			TokenURI string `json:"token_uri"`
		}
		if err := json.Unmarshal(f.SourceCredentials, &src); err != nil ||
			(src.Type != string(google.ServiceAccount) && src.Type != string(google.AuthorizedUser)) {
			return gcpCredentials{}, fmt.Errorf("%s: impersonated_service_account source credentials must be service_account or authorized_user: %w", errGCPInvalid, ErrInvalidCredentials)
		}
		if src.TokenURI != "" {
			out.endpoints = append(out.endpoints, gcpEndpoint{"source_credentials.token_uri", src.TokenURI})
		}
		if f.ServiceAccountImpersonationURL != "" {
			out.endpoints = append(out.endpoints, gcpEndpoint{"service_account_impersonation_url", f.ServiceAccountImpersonationURL})
		}
		out.credType = google.ImpersonatedServiceAccount
	default:
		t := f.Type
		if len(t) > 64 {
			t = t[:64]
		}
		return gcpCredentials{}, fmt.Errorf("%s: unsupported credentials type %q (supported: service_account, authorized_user, impersonated_service_account): %w", errGCPInvalid, t, ErrInvalidCredentials)
	}
	return out, nil
}

// validPrivateKey reports whether s is a PEM encoded PKCS#8 or PKCS#1 private key, the
// forms Google service account keys use. Checking it up front classifies a broken key as a
// configuration error instead of a token endpoint failure.
func validPrivateKey(s string) bool {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return false
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return true
	}
	_, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	return err == nil
}

// gcpToken returns an OAuth access token for a Google service account key and the account
// e-mail used as git user name (upstream GoogleCloudCreds). Tokens are cached per key digest
// until shortly before they expire. The endpoints named by the credentials must be https
// URLs on Google's hosts (or Options.GoogleTokenHosts); the HTTP client enforces the same
// list for every request, redirects included.
func (r *Resolver) gcpToken(ctx context.Context, keyJSON string) (mintedToken, error) {
	creds, err := gcpCredentialType([]byte(keyJSON))
	if err != nil {
		return mintedToken{}, err
	}
	for _, ep := range creds.endpoints {
		if err := checkEndpointURL("Google credentials "+ep.field, ep.url, r.googleHosts); err != nil {
			return mintedToken{}, err
		}
	}
	key := cacheKey(tokenKindGCP, keyJSON)
	return r.tokens.get(ctx, tokenKindGCP, key, 0, func(ctx context.Context) (mintedToken, error) {
		client, err := r.httpClient(transportOptions{what: "Google token endpoint", allow: r.googleHosts}, gcpRequestTimeout)
		if err != nil {
			return mintedToken{}, err
		}
		cctx := context.WithValue(ctx, oauth2.HTTPClient, client)
		gc, err := google.CredentialsFromJSONWithType(cctx, []byte(keyJSON), creds.credType, GCPScope)
		if err != nil {
			// The google error describes the parse failure without key material.
			return mintedToken{}, fmt.Errorf("%s: %w: %w", errGCPInvalid, ErrInvalidCredentials, err)
		}
		tok, err := gc.TokenSource.Token()
		if err != nil {
			return mintedToken{}, tokenFailure("failed to get access token from Google credentials", err)
		}
		return mintedToken{value: tok.AccessToken, username: creds.email, expiresAt: tok.Expiry}, nil
	})
}
