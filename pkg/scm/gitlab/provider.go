// Package gitlab implements the scm contract for GitLab (gitlab.com and self-managed
// GitLab) with the REST API v4 over plain net/http.
//
// It serves exactly what the Argo CD v3.5.5 baseline uses GitLab for: the SCM Provider
// generator (group projects with subgroups, shared projects, topic and archived
// filters; branches; path checks), the Pull Request generator (project merge requests
// with state and label filters) and webhooks (Push, Tag Push, Merge Request and System
// Hook events authenticated with X-Gitlab-Token), plus revision resolution.
//
// Repository references: RepoRef.Owner is the namespace full path ("group/subgroup") and
// RepoRef.Name the project path. A numeric project ID may be passed as
// RepoRef{Name: "<id>"} with an empty Owner.
package gitlab

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "gitlab"

// DefaultAPIURL is used when scm.Config.APIURL is empty.
const DefaultAPIURL = "https://gitlab.com/api/v4"

const (
	perPage = 100
	// branchConcurrency bounds parallel branch lookups during repository listing.
	branchConcurrency = 4
)

// Options are GitLab settings that scm.Config cannot express (SCM Provider generator
// fields). The zero value matches the upstream defaults.
type Options struct {
	// IncludeSubgroups also lists projects of subgroups (upstream includeSubgroups,
	// default false).
	IncludeSubgroups bool
	// IncludeSharedProjects lists projects shared with the group (upstream
	// includeSharedProjects; nil means true, the upstream default).
	IncludeSharedProjects *bool
}

func (o Options) sharedProjects() bool {
	return o.IncludeSharedProjects == nil || *o.IncludeSharedProjects
}

// Provider is the GitLab provider. It is safe for concurrent use.
type Provider struct {
	info   scm.Info
	c      *client
	opts   Options
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

// New is the scm.Factory for GitLab (default Options).
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) {
	return NewProvider(ctx, cfg, Options{})
}

// Register adds the GitLab factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a GitLab provider. It performs no network I/O. A non-nil
// cfg.Token is sent as the PRIVATE-TOKEN header (personal, group or project access
// token), as upstream's client does; without it requests are anonymous.
func NewProvider(_ context.Context, cfg scm.Config, opts Options) (*Provider, error) {
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
				h.Set("PRIVATE-TOKEN", v)
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
		opts:   opts,
		logger: cfg.Logger,
	}, nil
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// APIBaseURL normalizes a configured GitLab URL to the REST API v4 base (with a trailing
// slash), like upstream's client: "" selects gitlab.com, and "/api/v4" is appended
// unless the path already ends with it. Only http and https URLs without user info,
// query or fragment are accepted.
func APIBaseURL(apiURL string) (*url.URL, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid GitLab API URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid GitLab API URL %q: scheme must be http or https and a host is required", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid GitLab API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/api/v4") {
		path += "/api/v4"
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: path + "/"}, nil
}
