package clusters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GCPScopes are the OAuth scopes of GKE tokens: cloud-platform authenticates to Google Cloud
// and userinfo.email makes GKE see the service account e-mail instead of its numeric ID
// (upstream argocd-k8s-auth gcp).
var GCPScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
}

// envGoogleCredentials is the application-default-credentials file variable.
const envGoogleCredentials = "GOOGLE_APPLICATION_CREDENTIALS"

// GCPOptions configures GCPTokenSource.
type GCPOptions struct {
	// Lookup resolves environment variables; nil uses the process environment. When it
	// returns GOOGLE_APPLICATION_CREDENTIALS, that file is used instead of the
	// application-default search.
	Lookup func(string) (string, bool)
	// HTTPClient is used for token requests; nil uses the default client.
	HTTPClient *http.Client
	// Scopes overrides GCPScopes.
	Scopes []string
}

// GCPTokenSource returns GKE access tokens from Google application default credentials
// (Workload Identity, a GOOGLE_APPLICATION_CREDENTIALS file, gcloud credentials or the metadata
// server). Credentials are discovered on the first Token call and then reused; the underlying
// oauth2 source refreshes the access token before it expires.
func GCPTokenSource(opts GCPOptions) TokenSource {
	g := &gcpSource{opts: opts}
	return TokenSourceFunc(g.token)
}

type gcpSource struct {
	opts GCPOptions

	mu sync.Mutex
	ts oauth2.TokenSource
}

func (g *gcpSource) token(ctx context.Context) (Token, error) {
	ts, err := g.source(ctx)
	if err != nil {
		return Token{}, err
	}
	tok, err := ts.Token()
	if err != nil {
		return Token{}, fmt.Errorf("get GCP access token: %w", err)
	}
	return Token{Value: tok.AccessToken, Expiry: tok.Expiry}, nil
}

func (g *gcpSource) source(ctx context.Context) (oauth2.TokenSource, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ts != nil {
		return g.ts, nil
	}
	scopes := g.opts.Scopes
	if len(scopes) == 0 {
		scopes = GCPScopes
	}
	// The token source keeps this context for refreshes, so it must outlive the request.
	bg := context.WithoutCancel(ctx)
	if g.opts.HTTPClient != nil {
		bg = context.WithValue(bg, oauth2.HTTPClient, g.opts.HTTPClient)
	}
	lookup := g.opts.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var (
		creds *google.Credentials
		err   error
	)
	if path, ok := lookup(envGoogleCredentials); ok && path != "" {
		var data []byte
		data, err = os.ReadFile(path) //nolint:gosec // path is operator configuration, as for the exec plugin
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", envGoogleCredentials, err)
		}
		creds, err = gcpCredentialsFromFile(bg, data, scopes)
	} else {
		creds, err = google.FindDefaultCredentials(bg, scopes...)
	}
	if err != nil {
		return nil, fmt.Errorf("find GCP default credentials: %w", err)
	}
	g.ts = creds.TokenSource
	return g.ts, nil
}

// gcpCredentialTypes are the credential file types application-default credentials accept.
var gcpCredentialTypes = map[google.CredentialsType]bool{
	google.ServiceAccount:                true,
	google.AuthorizedUser:                true,
	google.ExternalAccount:               true,
	google.ExternalAccountAuthorizedUser: true,
	google.ImpersonatedServiceAccount:    true,
}

// gcpCredentialsFromFile loads a GOOGLE_APPLICATION_CREDENTIALS file the way the application
// default credentials search does, restricted to the known credential types.
func gcpCredentialsFromFile(ctx context.Context, data []byte, scopes []string) (*google.Credentials, error) {
	var f struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse credentials file: %w", err)
	}
	t := google.CredentialsType(f.Type)
	if !gcpCredentialTypes[t] {
		return nil, fmt.Errorf("unsupported credentials type %q", f.Type)
	}
	creds, err := google.CredentialsFromJSONWithType(ctx, data, t, scopes...)
	if err != nil {
		return nil, fmt.Errorf("load credentials file: %w", err)
	}
	return creds, nil
}
