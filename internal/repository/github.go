package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

// githubRequestTimeout bounds one GitHub API attempt (upstream ARGOCD_GIT_REQUEST_TIMEOUT
// default, 15s).
const githubRequestTimeout = 15 * time.Second

// githubAppToken returns an installation token of the repository's GitHub App, through the
// pkg/scm/github CredentialSource. Without an installation ID the installation is discovered
// for the organization in the repository URL. Tokens are cached until shortly before they
// expire; the git user name is "x-access-token". The App JWT is sent only to the endpoint
// of the object that supplied the private key (access.appEndpoint), over https.
func (r *Resolver) githubAppToken(ctx context.Context, a *access) (mintedToken, error) {
	installationID := a.githubAppInstallationID
	owner := ""
	if installationID == 0 {
		o, err := ownerFromURL(a.url)
		if err != nil {
			return mintedToken{}, configError(ErrInvalidConfiguration, "GitHub App installation discovery: %s", err)
		}
		owner = o
	}
	ep := a.appEndpoint()
	apiURL := strings.TrimRight(strings.TrimSpace(ep.baseURL), "/")
	if apiURL != "" {
		if err := checkEndpointURL("GitHub App API URL", apiURL, nil); err != nil {
			return mintedToken{}, err
		}
	}
	// The endpoint's settings are part of the key: a token minted through one proxy or TLS
	// configuration is not handed to an access that would not have reached the same API.
	key := cacheKey(tokenKindGitHubApp, apiURL, strconv.FormatInt(a.githubAppID, 10),
		strconv.FormatInt(installationID, 10), owner, a.githubAppPrivateKey,
		strconv.FormatBool(ep.insecure), string(ep.caData), ep.proxy, ep.noProxy)
	return r.tokens.get(ctx, tokenKindGitHubApp, key, 0, func(ctx context.Context) (mintedToken, error) {
		client, err := r.httpClient(transportOptions{
			insecure:       ep.insecure,
			caData:         ep.caData,
			clientCertData: a.tlsClientCertData,
			clientCertKey:  a.tlsClientCertKey,
			proxy:          r.safeProxy(ep.proxy, "githubApp"),
			noProxy:        ep.noProxy,
			what:           "GitHub API",
		}, 0)
		if err != nil {
			return mintedToken{}, err
		}
		p, err := github.NewProvider(ctx, scm.Config{
			Type:       github.Type,
			Name:       "repository",
			APIURL:     apiURL,
			HTTPClient: client,
			Timeout:    githubRequestTimeout,
			Retry:      scm.RetryPolicy{MaxAttempts: 3, MaxWait: 10 * time.Second},
			GitHubApp: &scm.GitHubAppConfig{
				AppID:          a.githubAppID,
				InstallationID: installationID,
				PrivateKey:     []byte(a.githubAppPrivateKey),
				Owner:          owner,
			},
			UserAgent: r.userAgent,
			Logger:    r.logger,
			Clock:     r.now,
		})
		if err != nil {
			return mintedToken{}, fmt.Errorf("failed to initialize GitHub installation transport: %w: %w", ErrInvalidCredentials, err)
		}
		tok, err := p.Token(ctx)
		if err != nil {
			return mintedToken{}, tokenFailure("failed to get GitHub App installation token", err)
		}
		username := tok.Username
		if username == "" {
			username = github.GitUsername
		}
		return mintedToken{value: tok.Value, username: username, expiresAt: tok.ExpiresAt}, nil
	})
}
