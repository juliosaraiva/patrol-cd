// Package bitbucketserver implements the scm contract for Bitbucket Server and
// Bitbucket Data Center with the REST API 1.0 over plain net/http.
//
// It serves what the Argo CD v3.5.5 baseline uses Bitbucket Server for: the SCM
// Provider generator (project repositories with their default branch, branches, path
// checks), the Pull Request generator (open pull requests) and webhooks
// (repo:refs_changed and pull request events signed with X-Hub-Signature HMAC-SHA256),
// plus revision resolution.
//
// Repository references: RepoRef.Owner is the project key and RepoRef.Name the
// repository slug.
package bitbucketserver

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
const Type = "bitbucketserver"

const (
	pageLimit = 100
	// concurrency bounds parallel per-repository lookups during repository listing.
	concurrency = 4
)

// Options are Bitbucket Server settings that scm.Config cannot express.
type Options struct {
	// Username selects HTTP basic authentication with the cfg.Token value as password
	// (upstream basicAuth). When empty, a cfg.Token is sent as a bearer token (upstream
	// bearerToken, which upstream prefers when both are configured: callers pass an empty
	// Username in that case). Without a token requests are anonymous.
	Username string
}

// Provider is the Bitbucket Server provider. It is safe for concurrent use.
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

// New is the scm.Factory for Bitbucket Server (bearer or anonymous authentication).
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) {
	return NewProvider(ctx, cfg, Options{})
}

// Register adds the Bitbucket Server factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a Bitbucket Server provider. It performs no network I/O.
// cfg.APIURL is required (upstream's `api`). Authenticated requests also send
// "X-Atlassian-Token: no-check" and "X-Requested-With: XMLHttpRequest" (XSRF bypass,
// as upstream).
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
			case user != "":
				h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+v)))
			case v != "":
				h.Set("Authorization", "Bearer "+v)
			default:
				return nil
			}
			h.Set("X-Atlassian-Token", "no-check")
			h.Set("X-Requested-With", "XMLHttpRequest")
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

// APIBaseURL normalizes a Bitbucket Server URL to the REST API 1.0 base. As upstream,
// "/rest" is appended unless present ("https://bbs.example.com" →
// "https://bbs.example.com/rest/api/1.0/"); a URL already ending in /rest/api/1.0 is
// kept. The URL is required.
func APIBaseURL(apiURL string) (*url.URL, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		return nil, errors.New("the Bitbucket Server API URL is required")
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitbucket Server API URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid Bitbucket Server API URL %q: scheme must be http or https and a host is required", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Bitbucket Server API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/api/1.0")
	if !strings.HasSuffix(path, "/rest") {
		path += "/rest"
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: path + "/api/1.0/"}, nil
}
