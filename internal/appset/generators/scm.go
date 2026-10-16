package generators

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/awscodecommit"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

// SCMProviderGenerator produces one parameter set per repository (or repository branch) found
// through an SCM API.
type SCMProviderGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: requeueAfterSeconds, else 30 minutes.
func (g *SCMProviderGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return requeueSeconds(gen.SCMProvider.RequeueAfterSeconds, DefaultSCMProviderRequeueAfter)
}

// Template implements Generator.
func (g *SCMProviderGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.SCMProvider.Template
}

// scmTarget is a constructed provider plus how to query it.
type scmTarget struct {
	provider scm.Provider
	query    scm.RepoQuery
	// organization overrides the organization parameter (Azure DevOps: the organization, not the
	// team project the provider reports).
	organization string
}

// SCMAPIURL returns the custom API URL the allow-list applies to, in upstream precedence
// (github, gitlab, gitea, bitbucketServer, azureDevOps). Bitbucket Cloud and AWS CodeCommit
// have no custom API URL.
func SCMAPIURL(cfg *v1alpha1.SCMProviderGenerator) string {
	switch {
	case cfg.Github != nil:
		return cfg.Github.API
	case cfg.Gitlab != nil:
		return cfg.Gitlab.API
	case cfg.Gitea != nil:
		return cfg.Gitea.API
	case cfg.BitbucketServer != nil:
		return cfg.BitbucketServer.API
	case cfg.AzureDevOps != nil:
		return cfg.AzureDevOps.API
	}
	return ""
}

// GenerateParams implements Generator.
//
// Order of checks: SCM providers enabled, provider reference, allowed API URL, credentials,
// repository listing (repository-level filters pushed down before branch expansion), then
// branch-level filters. Parameters: organization, repository, repository_id, url, branch, sha,
// short_sha, short_sha_7, labels (comma-joined), branchNormalized and values.
func (g *SCMProviderGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.SCMProvider == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	if !g.cfg.SCM.Enabled {
		return nil, ErrSCMProvidersDisabled
	}
	cfg := gen.SCMProvider
	target, err := g.build(ctx, cfg, appSet)
	if err != nil {
		return nil, err
	}
	repos, err := listRepos(ctx, target, cfg.Filters, scm.CloneProtocol(cfg.CloneProtocol))
	if err != nil {
		return nil, fmt.Errorf("error listing repos: %w", err)
	}
	opts := TemplateOptions(appSet)
	res := make([]map[string]any, 0, len(repos))
	for _, r := range repos {
		org := r.Organization
		if target.organization != "" {
			org = target.organization
		}
		params := map[string]any{
			"organization":     org,
			"repository":       r.Name,
			"repository_id":    r.RepositoryID,
			"url":              r.URL,
			"branch":           r.Branch,
			"sha":              r.SHA,
			"short_sha":        scm.ShortSHA(r.SHA, 8),
			"short_sha_7":      scm.ShortSHA(r.SHA, 7),
			"labels":           strings.Join(r.Labels, ","),
			"branchNormalized": template.SanitizeName(r.Branch),
		}
		if err := appendTemplatedValues(cfg.Values, params, opts); err != nil {
			return nil, fmt.Errorf("failed to append templated values: %w", err)
		}
		res = append(res, params)
	}
	return res, nil
}

// build selects the provider block (precedence github, gitlab, gitea, bitbucketServer,
// azureDevOps, bitbucket, awsCodeCommit) and constructs the provider.
func (g *SCMProviderGenerator) build(ctx context.Context, cfg *v1alpha1.SCMProviderGenerator, appSet *v1alpha1.ApplicationSet) (*scmTarget, error) {
	log := g.cfg.logger()
	creds := credentials{client: g.cfg.Client, namespace: g.cfg.Namespace, strict: g.cfg.SCM.TokenRefStrictMode}
	ns := appSet.Namespace
	sc := g.cfg.SCM
	base := scm.Config{Name: appSet.Namespace + "/" + appSet.Name, Logger: sc.Logger}
	proxied := func(c scm.Config) scm.Config {
		c.ProxyURL, c.NoProxy = sc.ProxyURL, sc.NoProxy
		return c
	}
	newProvider := func(req ProviderRequest, wrap string) (scm.Provider, error) {
		p, err := sc.factory()(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", wrap, err)
		}
		return p, nil
	}

	switch {
	case cfg.Github != nil:
		c := cfg.Github
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitHub, c.Organization)
		if err != nil {
			return nil, fmt.Errorf("scm provider: %w", err)
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		conf := proxied(base)
		conf.APIURL = api
		if sc.Observer != nil {
			conf.Observer = sc.Observer(appSet.Namespace, appSet.Name)
		}
		typ := github.Type
		switch {
		case ref != nil:
			typ = ref.scmType
			applyRef(&conf, ref)
		case c.AppSecretName != "":
			app, err := creds.githubApp(ctx, appSet.Namespace, c.AppSecretName, c.Organization)
			if err != nil {
				return nil, fmt.Errorf("scm provider: error fetching Github app secret: %w", err)
			}
			conf.GitHubApp = app
		default:
			token, err := creds.secretRef(ctx, c.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("scm provider: error fetching Github token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf}, "scm provider")
		if err != nil {
			return nil, err
		}
		q := scm.RepoQuery{Organization: c.Organization, AllBranches: c.AllBranches}
		if c.ExcludeArchivedRepos {
			q.Archived = scm.ArchivedExclude
		}
		return &scmTarget{provider: p, query: q}, nil

	case cfg.Gitlab != nil:
		c := cfg.Gitlab
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitLab, c.Group)
		if err != nil {
			return nil, fmt.Errorf("error initializing Gitlab service: %w", err)
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		caRef, err := creds.configMapData(ctx, c.CARef, ns)
		if err != nil {
			return nil, fmt.Errorf("error fetching CA certificates from ConfigMap: %w", err)
		}
		conf := proxied(base)
		conf.APIURL = api
		conf.Insecure = c.Insecure
		conf.CAData = caBundle(log, caRef, sc.rootCA(log))
		typ := gitlab.Type
		if ref != nil {
			typ = ref.scmType
			applyRef(&conf, ref)
		} else {
			token, err := creds.secretRef(ctx, c.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Gitlab token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf, GitLab: gitlab.Options{
			IncludeSubgroups:      c.IncludeSubgroups,
			IncludeSharedProjects: c.IncludeSharedProjects,
		}}, "error initializing Gitlab service")
		if err != nil {
			return nil, err
		}
		q := scm.RepoQuery{Organization: c.Group, AllBranches: c.AllBranches}
		if c.Topic != "" {
			q.Topics = []string{c.Topic}
		}
		if c.IncludeArchivedRepos {
			q.Archived = scm.ArchivedInclude
		}
		return &scmTarget{provider: p, query: q}, nil

	case cfg.Gitea != nil:
		c := cfg.Gitea
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitea, c.Owner)
		if err != nil {
			return nil, fmt.Errorf("error initializing Gitea service: %w", err)
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		conf := proxied(base)
		conf.APIURL = api
		conf.Insecure = c.Insecure
		typ := gitea.Type
		if ref != nil {
			typ = ref.scmType
			applyRef(&conf, ref)
		} else {
			token, err := creds.secretRef(ctx, c.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Gitea token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf}, "error initializing Gitea service")
		if err != nil {
			return nil, err
		}
		q := scm.RepoQuery{Organization: c.Owner, AllBranches: c.AllBranches}
		if c.ExcludeArchivedRepos {
			q.Archived = scm.ArchivedExclude
		}
		return &scmTarget{provider: p, query: q}, nil

	case cfg.BitbucketServer != nil:
		c := cfg.BitbucketServer
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeBitbucketServer, c.Project)
		if err != nil {
			return nil, fmt.Errorf("error initializing Bitbucket Server service: %w", err)
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		caRef, err := creds.configMapData(ctx, c.CARef, ns)
		if err != nil {
			return nil, fmt.Errorf("error fetching CA certificates from ConfigMap: %w", err)
		}
		conf := proxied(base)
		conf.APIURL = api
		conf.Insecure = c.Insecure
		conf.CAData = caBundle(log, caRef, sc.rootCA(log))
		typ := bitbucketserver.Type
		var opts bitbucketserver.Options
		switch {
		case ref != nil:
			typ = ref.scmType
			applyRef(&conf, ref)
			opts.Username = ref.username
		case c.BearerToken != nil:
			token, err := creds.secretRef(ctx, c.BearerToken.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Secret Bearer token: %w", err)
			}
			conf.Token = staticToken(token)
		case c.BasicAuth != nil:
			password, err := creds.secretRef(ctx, c.BasicAuth.PasswordRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Secret token: %w", err)
			}
			conf.Token = staticToken(password)
			opts.Username = c.BasicAuth.Username
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf, BitbucketServer: opts}, "error initializing Bitbucket Server service")
		if err != nil {
			return nil, err
		}
		return &scmTarget{provider: p, query: scm.RepoQuery{Organization: c.Project, AllBranches: c.AllBranches}}, nil

	case cfg.AzureDevOps != nil:
		c := cfg.AzureDevOps
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeAzureDevOps, c.Organization)
		if err != nil {
			return nil, fmt.Errorf("error initializing Azure Devops service: %w", err)
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		conf := base
		conf.APIURL = api
		typ := azuredevops.Type
		if ref != nil {
			typ = ref.scmType
			applyRef(&conf, ref)
		} else {
			token, err := creds.secretRef(ctx, c.AccessTokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Azure Devops access token: %w", err)
			}
			if token == "" {
				return nil, fmt.Errorf("error initializing Azure Devops service: no access token provided")
			}
			conf.Token = staticToken(token)
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf, AzureDevOps: azuredevops.Options{Organization: c.Organization}},
			"error initializing Azure Devops service")
		if err != nil {
			return nil, err
		}
		return &scmTarget{provider: p, query: scm.RepoQuery{Organization: c.TeamProject, AllBranches: c.AllBranches}, organization: c.Organization}, nil

	case cfg.Bitbucket != nil:
		c := cfg.Bitbucket
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeBitbucketCloud, c.Owner)
		if err != nil {
			return nil, fmt.Errorf("error initializing Bitbucket cloud service: %w", err)
		}
		conf := base
		typ := bitbucketcloud.Type
		opts := bitbucketcloud.Options{}
		if ref != nil {
			typ = ref.scmType
			applyRef(&conf, ref)
			if err := sc.checkAllowed(log, appSet, ref.apiURL); err != nil {
				return nil, err
			}
			opts.Username = ref.username
		} else {
			password, err := creds.secretRef(ctx, c.AppPasswordRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Bitbucket cloud appPassword: %w", err)
			}
			conf.Token = staticToken(password)
			opts.Username = c.User
		}
		p, err := newProvider(ProviderRequest{Type: typ, Config: conf, BitbucketCloud: opts}, "error initializing Bitbucket cloud service")
		if err != nil {
			return nil, err
		}
		return &scmTarget{provider: p, query: scm.RepoQuery{Organization: c.Owner, AllBranches: c.AllBranches}}, nil

	case cfg.AWSCodeCommit != nil:
		c := cfg.AWSCodeCommit
		filters := make([]awscodecommit.TagFilter, 0, len(c.TagFilters))
		for _, f := range c.TagFilters {
			filters = append(filters, awscodecommit.TagFilter{Key: f.Key, Value: f.Value})
		}
		p, err := newProvider(ProviderRequest{Type: awscodecommit.Type, Config: base, CodeCommit: awscodecommit.Options{
			Region: c.Region, Role: c.Role, TagFilters: filters,
		}}, "error initializing AWS codecommit service")
		if err != nil {
			return nil, err
		}
		return &scmTarget{provider: p, query: scm.RepoQuery{AllBranches: c.AllBranches}}, nil
	}
	return nil, errNoProvider
}

// apiOf returns the effective API URL: the referenced SCMProvider's when set, else the
// generator's.
func apiOf(ref *resolvedProviderRef, api string) string {
	if ref != nil && ref.apiURL != "" {
		return ref.apiURL
	}
	return api
}

// applyRef applies a referenced SCMProvider's TLS settings and credentials.
func applyRef(conf *scm.Config, ref *resolvedProviderRef) {
	if ref.apiURL != "" {
		conf.APIURL = ref.apiURL
	}
	conf.Insecure = conf.Insecure || ref.insecure
	if len(ref.caData) > 0 {
		conf.CAData = append(append([]byte(nil), ref.caData...), conf.CAData...)
	}
	conf.Token = staticToken(ref.token)
	conf.GitHubApp = ref.githubApp
}

// scmFilter is a compiled SCM generator filter.
type scmFilter struct {
	repositoryMatch *regexp.Regexp
	labelMatch      *regexp.Regexp
	branchMatch     *regexp.Regexp
	pathsExist      []string
	pathsDoNotExist []string
	branchLevel     bool
	set             bool
}

// compileSCMFilters compiles the filters and splits them into repository-level (only
// repositoryMatch/labelMatch set) and branch-level (any of pathsExist, pathsDoNotExist,
// branchMatch set) filters. Filters with no condition are ignored.
func compileSCMFilters(filters []v1alpha1.SCMProviderGeneratorFilter) (repo, branch []*scmFilter, err error) {
	for _, f := range filters {
		c := &scmFilter{}
		if f.RepositoryMatch != nil {
			if c.repositoryMatch, err = regexp.Compile(*f.RepositoryMatch); err != nil {
				return nil, nil, fmt.Errorf("error compiling RepositoryMatch regexp %q: %w", *f.RepositoryMatch, err)
			}
			c.set = true
		}
		if f.LabelMatch != nil {
			if c.labelMatch, err = regexp.Compile(*f.LabelMatch); err != nil {
				return nil, nil, fmt.Errorf("error compiling LabelMatch regexp %q: %w", *f.LabelMatch, err)
			}
			c.set = true
		}
		if f.PathsExist != nil {
			c.pathsExist = f.PathsExist
			c.set, c.branchLevel = true, true
		}
		if f.PathsDoNotExist != nil {
			c.pathsDoNotExist = f.PathsDoNotExist
			c.set, c.branchLevel = true, true
		}
		if f.BranchMatch != nil {
			if c.branchMatch, err = regexp.Compile(*f.BranchMatch); err != nil {
				return nil, nil, fmt.Errorf("error compiling BranchMatch regexp %q: %w", *f.BranchMatch, err)
			}
			c.set, c.branchLevel = true, true
		}
		switch {
		case !c.set:
		case c.branchLevel:
			branch = append(branch, c)
		default:
			repo = append(repo, c)
		}
	}
	return repo, branch, nil
}

func (f *scmFilter) matchesStatic(r scm.Repository) bool {
	if f.repositoryMatch != nil && !f.repositoryMatch.MatchString(r.Name) {
		return false
	}
	if f.branchMatch != nil && !f.branchMatch.MatchString(r.Branch) {
		return false
	}
	if f.labelMatch != nil && !slices.ContainsFunc(r.Labels, f.labelMatch.MatchString) {
		return false
	}
	return true
}

func (f *scmFilter) matches(ctx context.Context, provider scm.Provider, r scm.Repository) (bool, error) {
	if !f.matchesStatic(r) {
		return false, nil
	}
	if len(f.pathsExist) == 0 && len(f.pathsDoNotExist) == 0 {
		return true, nil
	}
	checker, err := scm.As[scm.PathChecker](provider)
	if err != nil {
		return false, err
	}
	for _, p := range f.pathsExist {
		ok, err := checker.PathExists(ctx, r.Ref(), r.Branch, strings.TrimRight(p, "/"))
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for _, p := range f.pathsDoNotExist {
		ok, err := checker.PathExists(ctx, r.Ref(), r.Branch, strings.TrimRight(p, "/"))
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	return true, nil
}

// listRepos lists repositories with the filters: a repository is kept when it matches any
// repository-level filter (none: all), branches are expanded, and a branch is kept when it
// matches any branch-level filter (none: all).
func listRepos(ctx context.Context, t *scmTarget, filters []v1alpha1.SCMProviderGeneratorFilter, protocol scm.CloneProtocol) ([]scm.Repository, error) {
	repoFilters, branchFilters, err := compileSCMFilters(filters)
	if err != nil {
		return nil, err
	}
	lister, err := scm.As[scm.RepositoryLister](t.provider)
	if err != nil {
		return nil, err
	}
	q := t.query
	q.CloneProtocol = protocol
	if len(repoFilters) > 0 {
		q.Match = func(r scm.Repository) bool {
			for _, f := range repoFilters {
				if f.matchesStatic(r) {
					return true
				}
			}
			return false
		}
	}
	repos, err := lister.ListRepositories(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(branchFilters) == 0 {
		return repos, nil
	}
	out := make([]scm.Repository, 0, len(repos))
	for _, r := range repos {
		for _, f := range branchFilters {
			ok, err := f.matches(ctx, t.provider, r)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}
