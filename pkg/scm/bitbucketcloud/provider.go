// Package bitbucketcloud implements the scm contract for Bitbucket Cloud
// (bitbucket.org) with the REST API 2.0 over plain net/http.
//
// It serves what the Argo CD v3.5.5 baseline uses Bitbucket Cloud for: the SCM Provider
// generator (workspace repositories the user is a member of, branches, path checks), the
// Pull Request generator (open pull requests) and webhooks (repo:push and pull request
// events authenticated by the webhook UUID), plus revision resolution and the diffstat
// callback upstream uses to compute the files changed by a push.
//
// Repository references: RepoRef.Owner is the workspace slug and RepoRef.Name the
// repository slug.
package bitbucketcloud

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "bitbucketcloud"

// DefaultAPIURL is used when scm.Config.APIURL is empty (upstream's default).
const DefaultAPIURL = "https://api.bitbucket.org/2.0"

// TokenAuthUsername is the user name that marks the password as an access token (sent
// as a bearer token), as upstream's webhook callback does for repository credentials.
const TokenAuthUsername = "x-token-auth"

const (
	repoPageLen = 100
	// prPageLen is the maximum page length of the pull request listing.
	prPageLen = 50
	// concurrency bounds parallel branch lookups during repository listing.
	concurrency = 4
)

// Options are Bitbucket Cloud settings that scm.Config cannot express.
type Options struct {
	// Username selects HTTP basic authentication with the cfg.Token value as the app
	// password (upstream SCM generator `user` + `appPasswordRef`, Pull Request generator
	// basicAuth). When empty, or equal to TokenAuthUsername, a cfg.Token is sent as a
	// bearer token (upstream bearerToken). Without a token requests are anonymous.
	Username string
}

// Provider is the Bitbucket Cloud provider. It is safe for concurrent use.
type Provider struct {
	info   scm.Info
	c      *client
	logger *slog.Logger
	WebhookVerifier
}

var (
	_ scm.Provider          = (*Provider)(nil)
	_ scm.RepositoryLister  = (*Provider)(nil)
	_ scm.BranchLister      = (*Provider)(nil)
	_ scm.RevisionResolver  = (*Provider)(nil)
	_ scm.PathChecker       = (*Provider)(nil)
	_ scm.PullRequestLister = (*Provider)(nil)
	_ scm.WebhookVerifier   = (*Provider)(nil)
)

// New is the scm.Factory for Bitbucket Cloud (bearer or anonymous authentication).
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) {
	return NewProvider(ctx, cfg, Options{})
}

// Register adds the Bitbucket Cloud factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a Bitbucket Cloud provider. It performs no network I/O.
func NewProvider(_ context.Context, cfg scm.Config, opts Options) (*Provider, error) {
	cfg = cfg.WithDefaults()
	base, err := APIBaseURL(cfg.APIURL)
	if err != nil {
		return nil, invalidErr("New", err)
	}
	if strings.ContainsAny(opts.Username, ":\r\n") {
		return nil, invalidErr("New", errors.New("basic auth username must not contain ':' or line breaks"))
	}
	var auth headerFunc
	if cfg.Token != nil {
		tok := tokenSource(cfg.Token)
		user := opts.Username
		auth = func(ctx context.Context, h http.Header) error {
			v, err := tok(ctx)
			if err != nil {
				return err
			}
			switch {
			case user != "" && user != TokenAuthUsername:
				h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+v)))
			case v != "":
				h.Set("Authorization", "Bearer "+v)
			}
			return nil
		}
	}
	c, err := newClient(cfg, base, auth)
	if err != nil {
		return nil, err
	}
	return &Provider{
		info: scm.Info{
			Type:            Type,
			Name:            cfg.Name,
			APIURL:          strings.TrimSuffix(base.String(), "/"),
			ContractVersion: scm.ContractVersion,
			Capabilities: []scm.Capability{
				scm.CapabilityRepositories, scm.CapabilityBranches, scm.CapabilityRevisions,
				scm.CapabilityPaths, scm.CapabilityPullRequests, scm.CapabilityWebhooks,
			},
		},
		c:      c,
		logger: cfg.Logger,
	}, nil
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// APIBaseURL validates a Bitbucket Cloud API URL and returns it with a trailing slash;
// "" selects DefaultAPIURL. The path is used as given (upstream's `api` is the full API
// base, e.g. "https://api.bitbucket.org/2.0").
func APIBaseURL(apiURL string) (*url.URL, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("error parsing base url of %s: %w", scm.RedactURLString(apiURL), err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid Bitbucket Cloud API URL %q: scheme must be http or https and a host is required", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Bitbucket Cloud API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimRight(u.Path, "/") + "/"}, nil
}
