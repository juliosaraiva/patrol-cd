// Package gitea implements the scm contract for Gitea (and Forgejo, which serves the
// same API) with the REST API v1 over plain net/http.
//
// It serves what the Argo CD v3.5.5 baseline uses Gitea for: the SCM Provider generator
// (organization repositories with repository labels, archived exclusion, branches, path
// checks), the Pull Request generator (open pull requests with a client-side label
// filter) and push/pull request webhooks (HMAC-SHA256 signatures), plus revision
// resolution.
//
// Repository references: RepoRef.Owner is the organization or user, RepoRef.Name the
// repository name.
package gitea

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "gitea"

const (
	// pageLimit is Gitea's default maximum page size (MAX_RESPONSE_ITEMS).
	pageLimit = 50
	// concurrency bounds parallel per-repository lookups during repository listing.
	concurrency = 4
)

// Provider is the Gitea provider. It is safe for concurrent use.
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

// New is the scm.Factory for Gitea.
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) { return NewProvider(ctx, cfg) }

// Register adds the Gitea factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a Gitea provider. It performs no network I/O (upstream's SDK
// queries the server version at construction; Patrol does not). cfg.APIURL is required
// (upstream's `api` field). A non-nil cfg.Token is sent as "Authorization: token <t>".
func NewProvider(_ context.Context, cfg scm.Config) (*Provider, error) {
	cfg = cfg.WithDefaults()
	base, err := APIBaseURL(cfg.APIURL)
	if err != nil {
		return nil, invalidErr("New", err)
	}
	var auth headerFunc
	if cfg.Token != nil {
		tok := tokenSource(cfg.Token)
		auth = func(ctx context.Context, h http.Header) error {
			v, err := tok(ctx)
			if err != nil {
				return err
			}
			if v != "" {
				h.Set("Authorization", "token "+v)
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

// APIBaseURL normalizes a Gitea server URL to its API base ("<server>/api/v1/"), like
// upstream's SDK. The URL is required; a URL already ending in /api/v1 is kept.
func APIBaseURL(apiURL string) (*url.URL, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		return nil, errors.New("the Gitea API URL is required")
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Gitea API URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid Gitea API URL %q: scheme must be http or https and a host is required", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Gitea API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/api/v1") {
		path += "/api/v1"
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: path + "/"}, nil
}
