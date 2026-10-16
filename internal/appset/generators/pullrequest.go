package generators

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

// PullRequestGenerator produces one parameter set per pull request of a repository.
type PullRequestGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: requeueAfterSeconds, else 30 minutes.
func (g *PullRequestGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return requeueSeconds(gen.PullRequest.RequeueAfterSeconds, DefaultPullRequestRequeueAfter)
}

// Template implements Generator.
func (g *PullRequestGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.PullRequest.Template
}

// PullRequestAPIURL returns the custom API URL the allow-list applies to (github, gitlab, gitea,
// bitbucketServer, bitbucket, azuredevops precedence).
func PullRequestAPIURL(cfg *v1alpha1.PullRequestGenerator) string {
	switch {
	case cfg.Github != nil:
		return cfg.Github.API
	case cfg.GitLab != nil:
		return cfg.GitLab.API
	case cfg.Gitea != nil:
		return cfg.Gitea.API
	case cfg.BitbucketServer != nil:
		return cfg.BitbucketServer.API
	case cfg.Bitbucket != nil:
		return cfg.Bitbucket.API
	case cfg.AzureDevOps != nil:
		return cfg.AzureDevOps.API
	}
	return ""
}

// prTarget is a constructed provider plus the repository and query.
type prTarget struct {
	provider scm.Provider
	repo     scm.RepoRef
	query    scm.PRQuery
}

// prSlug are the branch slug options: gosimple/slug with MaxLength 50 and '_' substituted by '-'.
var prSlug = template.SlugOptions{MaxLength: 50, SmartTruncate: true, Substitutions: map[string]string{"_": "-"}}

// GenerateParams implements Generator.
//
// Parameters: number, title, branch, branch_slug, target_branch, target_branch_slug, head_sha,
// head_short_sha, head_short_sha_7, author, labels (goTemplate only) and values. With
// continueOnRepoNotFoundError a missing repository yields no parameters instead of an error.
func (g *PullRequestGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.PullRequest == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	cfg := gen.PullRequest
	target, err := g.build(ctx, cfg, appSet)
	if err != nil {
		return nil, fmt.Errorf("failed to select pull request service provider: %w", err)
	}
	pulls, err := listPullRequests(ctx, target, cfg.Filters)
	if err != nil {
		if scm.IsNotFound(err) && cfg.ContinueOnRepoNotFoundError {
			g.cfg.logger().WarnContext(ctx, "skipping params generation for this repository since it was not found",
				appSetLogAttrs(appSet), slog.String("repository", target.repo.FullName()), slog.Any("error", err))
			return []map[string]any{}, nil
		}
		return nil, fmt.Errorf("error listing repos: %w", err)
	}
	opts := TemplateOptions(appSet)
	res := make([]map[string]any, 0, len(pulls))
	for _, pr := range pulls {
		params := map[string]any{
			"number":             strconv.FormatInt(pr.Number, 10),
			"title":              pr.Title,
			"branch":             pr.Branch,
			"branch_slug":        template.Slug(pr.Branch, prSlug),
			"target_branch":      pr.TargetBranch,
			"target_branch_slug": template.Slug(pr.TargetBranch, prSlug),
			"head_sha":           pr.HeadSHA,
			"head_short_sha":     scm.ShortSHA(pr.HeadSHA, 8),
			"head_short_sha_7":   scm.ShortSHA(pr.HeadSHA, 7),
			"author":             pr.Author,
		}
		if appSet.Spec.GoTemplate {
			labels := pr.Labels
			if labels == nil {
				labels = []string{}
			}
			params["labels"] = labels
		}
		if err := appendTemplatedValues(cfg.Values, params, opts); err != nil {
			return nil, fmt.Errorf("failed to append templated values: %w", err)
		}
		res = append(res, params)
	}
	return res, nil
}

func (g *PullRequestGenerator) build(ctx context.Context, cfg *v1alpha1.PullRequestGenerator, appSet *v1alpha1.ApplicationSet) (*prTarget, error) {
	sc := g.cfg.SCM
	if !sc.Enabled {
		return nil, ErrSCMProvidersDisabled
	}
	log := g.cfg.logger()
	creds := credentials{client: g.cfg.Client, namespace: g.cfg.Namespace, strict: sc.TokenRefStrictMode}
	ns := appSet.Namespace
	base := scm.Config{Name: appSet.Namespace + "/" + appSet.Name, Logger: sc.Logger}
	proxied := func(c scm.Config) scm.Config {
		c.ProxyURL, c.NoProxy = sc.ProxyURL, sc.NoProxy
		return c
	}
	build := func(req ProviderRequest) (scm.Provider, error) {
		return sc.factory()(ctx, req)
	}

	switch {
	case cfg.Github != nil:
		c := cfg.Github
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitHub, c.Owner)
		if err != nil {
			return nil, err
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
			app, err := creds.githubApp(ctx, appSet.Namespace, c.AppSecretName, c.Owner)
			if err != nil {
				return nil, fmt.Errorf("error getting GitHub App secret: %w", err)
			}
			conf.GitHubApp = app
		default:
			token, err := creds.secretRef(ctx, c.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Secret token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := build(ProviderRequest{Type: typ, Config: conf})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: scm.RepoRef{Owner: c.Owner, Name: c.Repo}, query: scm.PRQuery{Labels: c.Labels}}, nil

	case cfg.GitLab != nil:
		c := cfg.GitLab
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitLab, "")
		if err != nil {
			return nil, err
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
				return nil, fmt.Errorf("error fetching Secret token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		state, err := gitLabPRState(c.PullRequestState)
		if err != nil {
			return nil, err
		}
		repo, err := gitLabProjectRef(c.Project)
		if err != nil {
			return nil, err
		}
		p, err := build(ProviderRequest{Type: typ, Config: conf})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: repo, query: scm.PRQuery{Labels: c.Labels, State: state}}, nil

	case cfg.Gitea != nil:
		c := cfg.Gitea
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeGitea, c.Owner)
		if err != nil {
			return nil, err
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
				return nil, fmt.Errorf("error fetching Secret token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := build(ProviderRequest{Type: typ, Config: conf})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: scm.RepoRef{Owner: c.Owner, Name: c.Repo}, query: scm.PRQuery{Labels: c.Labels}}, nil

	case cfg.BitbucketServer != nil:
		c := cfg.BitbucketServer
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeBitbucketServer, c.Project)
		if err != nil {
			return nil, err
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
		p, err := build(ProviderRequest{Type: typ, Config: conf, BitbucketServer: opts})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: scm.RepoRef{Owner: c.Project, Name: c.Repo}}, nil

	case cfg.Bitbucket != nil:
		c := cfg.Bitbucket
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeBitbucketCloud, c.Owner)
		if err != nil {
			return nil, err
		}
		api := apiOf(ref, c.API)
		if err := sc.checkAllowed(log, appSet, api); err != nil {
			return nil, err
		}
		conf := base
		conf.APIURL = api
		typ := bitbucketcloud.Type
		var opts bitbucketcloud.Options
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
		p, err := build(ProviderRequest{Type: typ, Config: conf, BitbucketCloud: opts})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: scm.RepoRef{Owner: c.Owner, Name: c.Repo}}, nil

	case cfg.AzureDevOps != nil:
		c := cfg.AzureDevOps
		ref, err := creds.providerRef(ctx, c.ProviderRef, ns, v1alpha1.SCMProviderTypeAzureDevOps, c.Organization)
		if err != nil {
			return nil, err
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
			token, err := creds.secretRef(ctx, c.TokenRef, ns)
			if err != nil {
				return nil, fmt.Errorf("error fetching Secret token: %w", err)
			}
			conf.Token = staticToken(token)
		}
		p, err := build(ProviderRequest{Type: typ, Config: conf, AzureDevOps: azuredevops.Options{Organization: c.Organization}})
		if err != nil {
			return nil, err
		}
		return &prTarget{provider: p, repo: scm.RepoRef{Owner: c.Project, Name: c.Repo}, query: scm.PRQuery{Labels: c.Labels}}, nil
	}
	return nil, errors.New("no Pull Request provider implementation configured")
}

// gitLabPRState maps pullRequestState: "" lists every state (upstream), opened/closed/merged/
// locked filter by state.
func gitLabPRState(s string) (scm.PRState, error) {
	switch s {
	case "":
		return scm.PRStateAll, nil
	case "opened":
		return scm.PRStateOpen, nil
	case "closed":
		return scm.PRStateClosed, nil
	case "merged":
		return scm.PRStateMerged, nil
	case "locked":
		return gitlab.PRStateLocked, nil
	}
	return "", fmt.Errorf("invalid GitLab pullRequestState %q: must be one of opened, closed, merged, locked", s)
}

// gitLabProjectRef addresses a GitLab project by numeric ID or by full path.
func gitLabProjectRef(project string) (scm.RepoRef, error) {
	if _, err := strconv.ParseInt(project, 10, 64); err == nil {
		return scm.RepoRef{Name: project}, nil
	}
	return scm.ParseFullName(project)
}

// prFilter is a compiled pull request filter.
type prFilter struct {
	branch, target, title *regexp.Regexp
}

func compilePRFilters(filters []v1alpha1.PullRequestGeneratorFilter) ([]prFilter, error) {
	out := make([]prFilter, 0, len(filters))
	for _, f := range filters {
		var c prFilter
		var err error
		if f.BranchMatch != nil {
			if c.branch, err = regexp.Compile(*f.BranchMatch); err != nil {
				return nil, fmt.Errorf("error compiling BranchMatch regexp %q: %w", *f.BranchMatch, err)
			}
		}
		if f.TargetBranchMatch != nil {
			if c.target, err = regexp.Compile(*f.TargetBranchMatch); err != nil {
				return nil, fmt.Errorf("error compiling TargetBranchMatch regexp %q: %w", *f.TargetBranchMatch, err)
			}
		}
		if f.TitleMatch != nil {
			if c.title, err = regexp.Compile(*f.TitleMatch); err != nil {
				return nil, fmt.Errorf("error compiling TitleMatch regexp %q: %w", *f.TitleMatch, err)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (f prFilter) matches(pr scm.PullRequest) bool {
	if f.branch != nil && !f.branch.MatchString(pr.Branch) {
		return false
	}
	if f.target != nil && !f.target.MatchString(pr.TargetBranch) {
		return false
	}
	if f.title != nil && !f.title.MatchString(pr.Title) {
		return false
	}
	return true
}

// listPullRequests lists the pull requests and keeps those matching any filter (no filters: all;
// an empty filter matches everything).
func listPullRequests(ctx context.Context, t *prTarget, filters []v1alpha1.PullRequestGeneratorFilter) ([]scm.PullRequest, error) {
	compiled, err := compilePRFilters(filters)
	if err != nil {
		return nil, err
	}
	lister, err := scm.As[scm.PullRequestLister](t.provider)
	if err != nil {
		return nil, err
	}
	pulls, err := lister.ListPullRequests(ctx, t.repo, t.query)
	if err != nil {
		return nil, err
	}
	if len(compiled) == 0 {
		return pulls, nil
	}
	out := make([]scm.PullRequest, 0, len(pulls))
	for _, pr := range pulls {
		for _, f := range compiled {
			if f.matches(pr) {
				out = append(out, pr)
				break
			}
		}
	}
	return out, nil
}
