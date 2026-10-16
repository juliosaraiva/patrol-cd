// Package azuredevops implements the scm contract for Azure DevOps Services and Azure
// DevOps Server with the REST API 7.1 over plain net/http.
//
// It serves what the Argo CD v3.5.5 baseline uses Azure DevOps for: the SCM Provider
// generator (team project repositories, branches, path checks), the Pull Request
// generator (project pull requests filtered to one repository, with a client-side label
// filter) and webhooks (git.push and pull request service hooks authenticated with HTTP
// basic auth), plus revision resolution.
//
// A provider instance is bound to one organization (collection). Repository
// references: RepoRef.Owner is the team project name and RepoRef.Name the repository
// name; RepoQuery.Organization is the team project.
package azuredevops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "azuredevops"

// DefaultAPIURL is used when scm.Config.APIURL is empty (upstream's default).
const DefaultAPIURL = "https://dev.azure.com"

const (
	apiVersion = "7.1"
	// prPageSize is the $top of pull request pages.
	prPageSize = 100
	// concurrency bounds parallel branch lookups during repository listing.
	concurrency = 4
)

// Options are Azure DevOps settings that scm.Config cannot express.
type Options struct {
	// Organization is the Azure DevOps organization (or Azure DevOps Server collection),
	// appended to the API URL as upstream does ("<api>/<organization>"). When empty,
	// cfg.APIURL must already be the organization URL (for example
	// "https://dev.azure.com/acme"), which is how the registry factory is used.
	Organization string
}

// Provider is the Azure DevOps provider. It is safe for concurrent use.
type Provider struct {
	info   scm.Info
	c      *client
	logger *slog.Logger
	org    string
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

// New is the scm.Factory for Azure DevOps; cfg.APIURL must be the organization URL.
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) {
	return NewProvider(ctx, cfg, Options{})
}

// Register adds the Azure DevOps factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds an Azure DevOps provider. It performs no network I/O. A non-nil
// cfg.Token is a personal access token sent with HTTP basic authentication and an empty
// user name (as upstream's PAT connection); without it requests are anonymous (public
// projects). Upstream's SCM Provider generator requires a token ("no access token
// provided"); that check belongs to the generator.
func NewProvider(_ context.Context, cfg scm.Config, opts Options) (*Provider, error) {
	cfg = cfg.WithDefaults()
	base, org, err := OrganizationURL(cfg.APIURL, opts.Organization)
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
				h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+v)))
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
		org:    org,
	}, nil
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// Organization returns the organization (collection) the provider is bound to. The SCM
// Provider generator's `organization` template parameter is this value (upstream),
// while Repository.Organization carries the team project (see ListRepositories).
func (p *Provider) Organization() string { return p.org }

// OrganizationURL builds the organization base URL like upstream: apiURL (default
// https://dev.azure.com) joined with organization. When organization is empty the last
// path segment of apiURL is the organization. The result has a trailing slash.
func OrganizationURL(apiURL, organization string) (*url.URL, string, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, "", fmt.Errorf("got an invalid URL for the Azure SCM generator: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", fmt.Errorf("got an invalid URL for the Azure SCM generator: %q needs an http(s) scheme and a host", scm.RedactURLString(apiURL))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", fmt.Errorf("invalid Azure DevOps API URL %q: user info, query and fragment are not allowed", scm.RedactURLString(apiURL))
	}
	path := strings.TrimRight(u.Path, "/")
	organization = strings.TrimSpace(organization)
	if organization == "" {
		i := strings.LastIndexByte(path, '/')
		if i < 0 || path[i+1:] == "" {
			return nil, "", errors.New("the Azure DevOps organization is required (Options.Organization or the last path segment of the API URL)")
		}
		organization, err = url.PathUnescape(path[i+1:])
		if err != nil {
			return nil, "", fmt.Errorf("invalid Azure DevOps organization in API URL: %w", err)
		}
		path = path[:i]
	}
	if err := validateName("organization", organization); err != nil {
		return nil, "", err
	}
	base := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: path + "/" + organization + "/"}
	base.RawPath = path + "/" + url.PathEscape(organization) + "/"
	return base, organization, nil
}

// validateName validates an Azure DevOps organization, project or repository name taken
// from untrusted input before it is path-escaped into a URL. Azure names may contain
// spaces; separators, control characters and "."/".." are rejected.
func validateName(what, s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 256 || strings.ContainsAny(s, `/\`) ||
		strings.IndexFunc(s, unicode.IsControl) >= 0 || strings.TrimSpace(s) != s {
		return &scm.Error{Kind: scm.KindInvalid, Op: "Validate", Err: fmt.Errorf("invalid %s name %q", what, s)}
	}
	return nil
}
