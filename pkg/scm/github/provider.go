// Package github is Patrol CD's first-class GitHub (github.com and GitHub Enterprise
// Server) implementation of the scm contract. It supports anonymous access, personal
// access tokens and GitHub App authentication (JWT → installation token with caching,
// installation auto-discovery), and declares capabilities according to the configured
// authentication: writes (commit statuses) need a token or App, check runs and
// installation listing need an App.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "github"

// GitUsername is the HTTPS user name paired with installation or personal tokens.
const GitUsername = "x-access-token"

const (
	defaultAPIURL    = "https://api.github.com/"
	defaultUploadURL = "https://uploads.github.com/"
	perPage          = 100
	// branchConcurrency bounds parallel branch lookups during repository listing.
	branchConcurrency = 4
)

type authMode int

const (
	authAnonymous authMode = iota
	authToken
	authApp
)

// Provider is the GitHub provider. It is safe for concurrent use.
type Provider struct {
	info     scm.Info
	client   *gh.Client
	mode     authMode
	token    scm.TokenFunc
	app      *appAuth
	maxPages int
	now      func() time.Time
	logger   *slog.Logger
	WebhookVerifier
}

var (
	_ scm.Provider             = (*Provider)(nil)
	_ scm.RepositoryLister     = (*Provider)(nil)
	_ scm.BranchLister         = (*Provider)(nil)
	_ scm.RevisionResolver     = (*Provider)(nil)
	_ scm.PathChecker          = (*Provider)(nil)
	_ scm.PullRequestLister    = (*Provider)(nil)
	_ scm.WebhookVerifier      = (*Provider)(nil)
	_ scm.CommitStatusReporter = (*Provider)(nil)
	_ scm.CheckRunReporter     = (*Provider)(nil)
	_ scm.CredentialSource     = (*Provider)(nil)
	_ scm.InstallationLister   = (*Provider)(nil)
)

// New is the scm.Factory for GitHub.
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) { return NewProvider(ctx, cfg) }

// Register adds the GitHub factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a GitHub provider. It performs no network I/O: App installation
// discovery and token minting happen on first use.
func NewProvider(_ context.Context, cfg scm.Config) (*Provider, error) {
	cfg = cfg.WithDefaults()
	invalid := func(err error) error { return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: "New", Err: err} }

	apiURL := cfg.APIURL
	if apiURL == "" && cfg.GitHubApp != nil {
		apiURL = cfg.GitHubApp.EnterpriseBaseURL
	}
	baseURL, uploadURL, err := APIURLs(apiURL)
	if err != nil {
		return nil, invalid(err)
	}
	host, err := url.Parse(baseURL)
	if err != nil {
		return nil, invalid(err)
	}
	base, err := scm.BaseTransport(cfg)
	if err != nil {
		return nil, err
	}
	attemptTimeout := cfg.Timeout
	if cfg.HTTPClient != nil && cfg.HTTPClient.Timeout > 0 {
		attemptTimeout = cfg.HTTPClient.Timeout
	}
	// The timeout bounds each attempt below authentication, so token acquisition (which
	// has its own attempts and waits) and retry waits are not charged to it.
	limited := scm.LimitResponseBody(scm.AttemptTimeout(base, attemptTimeout), cfg.MaxResponseBytes)
	chain := scm.ChainOptions{
		Provider: Type,
		Logger:   cfg.Logger,
		Limiter:  scm.NewLimiter(cfg.RateLimit),
		Observer: cfg.Observer,
		Retry:    cfg.Retry,
		Clock:    cfg.Clock,
	}
	newClient := func(rt http.RoundTripper) (*gh.Client, error) {
		// No http.Client.Timeout: it would span every retry and every rate-limit wait
		// the retry policy accepts (calls are bounded by their context and CallBudget).
		return gh.NewClient(
			gh.WithHTTPClient(&http.Client{Transport: scm.Chain(rt, chain)}),
			gh.WithURLs(&baseURL, &uploadURL),
			gh.WithUserAgent(cfg.UserAgent),
		)
	}

	p := &Provider{maxPages: cfg.MaxPages, now: cfg.Clock, logger: cfg.Logger}
	mainRT := limited
	switch {
	case cfg.GitHubApp != nil:
		a := cfg.GitHubApp
		if a.AppID <= 0 {
			return nil, invalid(errors.New("GitHub App ID is required"))
		}
		if len(a.PrivateKey) == 0 {
			return nil, invalid(errors.New("GitHub App private key is required"))
		}
		if a.InstallationID < 0 {
			return nil, invalid(errors.New("GitHub App installation ID must not be negative"))
		}
		jwtRT, err := ghinstallation.NewAppsTransport(limited, a.AppID, a.PrivateKey)
		if err != nil {
			// The ghinstallation error never contains key material.
			return nil, invalid(fmt.Errorf("failed to initialize GitHub App transport: %w", err))
		}
		// ghinstallation sets headers on the request it is given; hand it a clone so the
		// JWT never lands on a request object owned by outer layers.
		cloning := roundTripFunc(func(r *http.Request) (*http.Response, error) { return jwtRT.RoundTrip(r.Clone(r.Context())) })
		appClient, err := newClient(scm.HostGuard(host.Host, cloning, limited))
		if err != nil {
			return nil, invalid(err)
		}
		p.app = newAppAuth(a.AppID, a.InstallationID, a.Owner, baseURL, a.PrivateKey, appClient, cfg.Clock, cfg.Retry.CallBudget(attemptTimeout))
		p.mode = authApp
		mainRT = scm.HostGuard(host.Host, scm.BearerTransport(limited, p.app.tokenValue, p.app.invalidate), limited)
	case cfg.Token != nil:
		p.mode = authToken
		p.token = tokenSource(cfg.Token)
		mainRT = scm.HostGuard(host.Host, scm.BearerTransport(limited, p.token, nil), limited)
	}
	p.client, err = newClient(mainRT)
	if err != nil {
		return nil, invalid(err)
	}

	caps := []scm.Capability{
		scm.CapabilityRepositories, scm.CapabilityBranches, scm.CapabilityRevisions,
		scm.CapabilityPaths, scm.CapabilityPullRequests, scm.CapabilityWebhooks,
	}
	if p.mode != authAnonymous {
		caps = append(caps, scm.CapabilityCommitStatuses, scm.CapabilityCredentials)
	}
	if p.mode == authApp {
		caps = append(caps, scm.CapabilityCheckRuns, scm.CapabilityInstallations)
	}
	p.info = scm.Info{
		Type:            Type,
		Name:            cfg.Name,
		APIURL:          strings.TrimSuffix(baseURL, "/"),
		ContractVersion: scm.ContractVersion,
		Capabilities:    caps,
	}
	return p, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// tokenSource normalizes failures of a caller-supplied token function (for example a
// missing Secret) to KindUnauthorized, keeping context errors and *scm.Error values as
// they are, so API calls and Token() report the same kind.
func tokenSource(tok scm.TokenFunc) scm.TokenFunc {
	return func(ctx context.Context) (string, error) {
		v, err := tok(ctx)
		if err == nil {
			return v, nil
		}
		var se *scm.Error
		if errors.As(err, &se) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", &scm.Error{Kind: scm.KindUnauthorized, Provider: Type, Op: "Token", Err: fmt.Errorf("token source failed: %w", err)}
	}
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// Client exposes the underlying go-github client for GitHub-specific operations not
// covered by the contract (e.g. deployments for notifications). It carries the same
// authentication, rate limiting, retries and size caps.
func (p *Provider) Client() *gh.Client { return p.client }

// APIURLs derives the REST base URL and the uploads URL from a configured API URL, with
// the same base-URL rules as go-github's WithEnterpriseURLs (which upstream uses):
// "" / "https://api.github.com" select github.com; a path ending in "/api/v3" is kept;
// hosts starting with "api." (GitHub Enterprise Cloud with data residency,
// "https://api.<tenant>.ghe.com") or containing ".api." keep their path as is; any other
// URL is a GitHub Enterprise Server root and gets "/api/v3/" appended. A trailing slash
// is tolerated. URLs with user info, query or fragment are rejected.
func APIURLs(apiURL string) (base, upload string, err error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		return defaultAPIURL, defaultUploadURL, nil
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid GitHub API URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", "", fmt.Errorf("invalid GitHub API URL %q: scheme must be http or https and a host is required", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("invalid GitHub API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.Path, "/")
	origin := u.Scheme + "://" + u.Host
	switch {
	case host == "api.github.com" && path == "":
		return defaultAPIURL, defaultUploadURL, nil
	case strings.HasSuffix(path, "/api/v3"):
		prefix := strings.TrimSuffix(path, "/api/v3")
		return origin + path + "/", origin + prefix + "/api/uploads/", nil
	case strings.HasPrefix(host, "api."):
		return origin + path + "/", u.Scheme + "://uploads." + u.Host[len("api."):] + path + "/", nil
	case strings.Contains(host, ".api."):
		return origin + path + "/", origin + path + "/", nil
	default:
		return origin + path + "/api/v3/", origin + path + "/api/uploads/", nil
	}
}

func (p *Provider) mapErr(op string, resp *gh.Response, err error) error {
	return mapError(op, resp, err, p.now())
}

// require returns an *scm.UnsupportedError unless capability c is declared, so calling a
// method directly (without scm.As) fails the same way for undeclared capabilities.
func (p *Provider) require(c scm.Capability) error {
	if !p.info.Has(c) {
		return &scm.UnsupportedError{Provider: Type, Name: p.info.Name, Capability: c}
	}
	return nil
}
