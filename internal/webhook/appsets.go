package webhook

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

// Default API URLs of Pull Request generators without an explicit api, as upstream.
const (
	defaultGitHubAPI = "https://api.github.com/"
	defaultGitLabAPI = "https://gitlab.com/"
)

// maxInterpolations bounds the parameter sets interpolated into the second child of a
// Matrix generator for one ApplicationSet; beyond it the ApplicationSet is refreshed
// (conservatively) instead of evaluated further.
const maxInterpolations = 10000

// prActionsRefreshing are the normalized pull request actions that refresh Pull Request
// generators (upstream's GitHub list; GitLab open/close/reopen/update/merge and the Azure
// DevOps created/updated/merged events normalize onto it).
var prActionsRefreshing = []string{
	scm.PRActionOpened, scm.PRActionClosed, scm.PRActionSynchronize,
	scm.PRActionLabeled, scm.PRActionReopened, scm.PRActionUnlabeled,
}

// pushInfo is what Git generators are matched against.
type pushInfo struct {
	repoRegexps []*regexp.Regexp
	revision    string
	touchedHead bool
}

// prInfo is what Pull Request generators are matched against.
type prInfo struct {
	// provider is the scm provider type of the event.
	provider    string
	owner, repo string
	// id is the provider repository ID (GitLab: target project ID).
	id string
	// apiURL is the repository API URL (GitHub), webURL the repository web URL.
	apiURL, webURL string
}

// appSetEvent is an accepted event reduced to what ApplicationSet generators use.
type appSetEvent struct {
	push *pushInfo
	pr   *prInfo
}

func newPushInfo(ev *scm.Event, regexps []*regexp.Regexp) *pushInfo {
	if len(regexps) == 0 {
		return nil
	}
	return &pushInfo{repoRegexps: regexps, revision: ev.Revision, touchedHead: ev.TouchedDefaultBranch}
}

// newPRInfo returns nil for events that do not refresh Pull Request generators.
func newPRInfo(ev *scm.Event) *prInfo {
	if ev.Type != scm.EventPullRequest || ev.Repository == nil || ev.PullRequest == nil {
		return nil
	}
	if !slices.Contains(prActionsRefreshing, ev.Action) {
		return nil
	}
	r := ev.Repository
	web := r.HTMLURL
	if web == "" {
		web = r.CloneURL
	}
	return &prInfo{provider: ev.Provider, owner: r.Owner, repo: r.Name, id: r.ID, apiURL: r.APIURL, webURL: web}
}

// appSetMatcher evaluates the generators of one ApplicationSet against an event.
type appSetMatcher struct {
	ctx    context.Context
	appSet *v1alpha1.ApplicationSet
	ev     *appSetEvent
	eval   GeneratorEvaluator
	log    *slog.Logger
	// providerAPI resolves the API URL of an SCMProvider referenced by a generator
	// (namespace of the ApplicationSet); "" when unknown.
	providerAPI func(name string) string
	budget      int
}

// matches reports whether any top-level generator is affected (ASET-105..107).
func (m *appSetMatcher) matches() bool {
	if m.ev == nil || (m.ev.push == nil && m.ev.pr == nil) {
		return false
	}
	m.budget = maxInterpolations
	for i := range m.appSet.Spec.Generators {
		g := &m.appSet.Spec.Generators[i]
		if m.git(g.Git, false) || m.pullRequest(g.PullRequest, false) || g.Plugin != nil ||
			m.matrix(g.Matrix, false) || m.merge(g.Merge, false) {
			return true
		}
	}
	return false
}

// templated reports a field still holding template markers.
func templated(s string) bool { return strings.Contains(s, "{{") }

// field reports whether a generator field equals want; with wildcard a templated field
// matches anything.
func field(value string, wildcard bool, eq func(string) bool) bool {
	if wildcard && templated(value) {
		return true
	}
	return eq(value)
}

func (m *appSetMatcher) git(gen *v1alpha1.GitGenerator, wildcard bool) bool {
	p := m.ev.push
	if gen == nil || p == nil {
		return false
	}
	if !field(gen.RepoURL, wildcard, func(v string) bool {
		for _, re := range p.repoRegexps {
			if re.MatchString(v) {
				return true
			}
		}
		return false
	}) {
		return false
	}
	return field(gen.Revision, wildcard, func(v string) bool {
		return generatorRevisionHasChanged(v, p.revision, p.touchedHead)
	})
}

func (m *appSetMatcher) apiFor(api string, ref *v1alpha1.SCMProviderReference, def string) string {
	if api != "" {
		return api
	}
	if ref != nil && m.providerAPI != nil {
		if v := m.providerAPI(ref.Name); v != "" {
			return v
		}
	}
	return def
}

func eqFold(want string) func(string) bool {
	return func(v string) bool { return strings.EqualFold(v, want) }
}

func eqExact(want string) func(string) bool {
	return func(v string) bool { return v == want }
}

// pullRequest matches a Pull Request generator (ASET-106). GitHub: owner and repository
// (case-insensitive) and API host; GitLab: project (target project ID, or its path, a
// Patrol extension) and API host; Azure DevOps: project and repository names. Gitea,
// Bitbucket Server and Bitbucket Cloud pull request events are Patrol extensions
// (upstream's ApplicationSet webhook ignores them): owner/project and repository
// (case-insensitive), and the API host where the generator has one.
func (m *appSetMatcher) pullRequest(gen *v1alpha1.PullRequestGenerator, wildcard bool) bool {
	pr := m.ev.pr
	if gen == nil || pr == nil {
		return false
	}
	hostIs := func(host string) func(string) bool {
		return func(v string) bool { return apiHostMatches(v, host) }
	}
	// optionalHost skips the host check when the generator has no API URL.
	optionalHost := func(host string) func(string) bool {
		return func(v string) bool { return v == "" || apiHostMatches(v, host) }
	}
	switch pr.provider {
	case github.Type:
		g := gen.Github
		if g == nil {
			return false
		}
		api := m.apiFor(g.API, g.ProviderRef, defaultGitHubAPI)
		return field(g.Owner, wildcard, eqFold(pr.owner)) &&
			field(g.Repo, wildcard, eqFold(pr.repo)) &&
			field(api, wildcard, hostIs(pr.apiURL))
	case gitlab.Type:
		g := gen.GitLab
		if g == nil {
			return false
		}
		api := m.apiFor(g.API, g.ProviderRef, defaultGitLabAPI)
		full := pr.owner + "/" + pr.repo
		return field(g.Project, wildcard, func(v string) bool { return v == pr.id || (pr.owner != "" && v == full) }) &&
			field(api, wildcard, hostIs(pr.webURL))
	case azuredevops.Type:
		g := gen.AzureDevOps
		if g == nil {
			return false
		}
		return field(g.Project, wildcard, eqExact(pr.owner)) && field(g.Repo, wildcard, eqExact(pr.repo))
	case gitea.Type:
		g := gen.Gitea
		if g == nil {
			return false
		}
		api := m.apiFor(g.API, g.ProviderRef, "")
		return field(g.Owner, wildcard, eqFold(pr.owner)) && field(g.Repo, wildcard, eqFold(pr.repo)) &&
			field(api, wildcard, optionalHost(pr.webURL))
	case bitbucketserver.Type:
		g := gen.BitbucketServer
		if g == nil {
			return false
		}
		api := m.apiFor(g.API, g.ProviderRef, "")
		return field(g.Project, wildcard, eqFold(pr.owner)) && field(g.Repo, wildcard, eqFold(pr.repo)) &&
			field(api, wildcard, optionalHost(pr.webURL))
	case bitbucketcloud.Type:
		g := gen.Bitbucket
		if g == nil {
			return false
		}
		return field(g.Owner, wildcard, eqFold(pr.owner)) && field(g.Repo, wildcard, eqFold(pr.repo))
	}
	return false
}

// matrix mirrors upstream's Matrix handling: the first child matches directly (Git, Pull
// Request, nested Matrix or Merge); otherwise the second child is checked after
// interpolation with each parameter set of the first child (when a GeneratorEvaluator
// is configured) and finally as written. Without an evaluator the second child is
// checked with templated fields as wildcards.
func (m *appSetMatcher) matrix(gen *v1alpha1.MatrixGenerator, wildcard bool) bool {
	if gen == nil || len(gen.Generators) != 2 {
		return false
	}
	g0, g1 := &gen.Generators[0], &gen.Generators[1]
	if m.git(g0.Git, wildcard) || m.pullRequest(g0.PullRequest, wildcard) {
		return true
	}
	mat0, mer0, ok := m.nested(g0)
	if !ok {
		return false
	}
	if m.matrix(mat0, wildcard) || m.merge(mer0, wildcard) {
		return true
	}
	mat1, mer1, ok := m.nested(g1)
	if !ok {
		return false
	}
	if m.eval == nil || wildcard {
		return m.git(g1.Git, true) || m.pullRequest(g1.PullRequest, true) || g1.Plugin != nil ||
			m.matrix(mat1, true) || m.merge(mer1, true)
	}
	params, err := m.eval.GenerateParams(m.ctx, m.appSet, toGenerator(g0, mat0, mer0))
	if err != nil {
		m.log.Info("webhook: matrix generator evaluation failed; ApplicationSet not refreshed",
			"applicationSet", m.appSet.Namespace+"/"+m.appSet.Name, "error", err)
		return false
	}
	req1 := toGenerator(g1, mat1, mer1)
	for _, p := range params {
		if m.budget--; m.budget < 0 {
			m.log.Info("webhook: matrix interpolation budget exhausted; refreshing ApplicationSet",
				"applicationSet", m.appSet.Namespace+"/"+m.appSet.Name, "limit", maxInterpolations)
			return true
		}
		ig, err := m.eval.InterpolateGenerator(m.appSet, req1, p)
		if err != nil {
			m.log.Info("webhook: matrix generator interpolation failed; ApplicationSet not refreshed",
				"applicationSet", m.appSet.Namespace+"/"+m.appSet.Name, "error", err)
			return false
		}
		if ig != nil && (m.git(ig.Git, false) || m.pullRequest(ig.PullRequest, false) || ig.Plugin != nil ||
			m.matrix(ig.Matrix, false) || m.merge(ig.Merge, false)) {
			return true
		}
	}
	return m.git(g1.Git, false) || m.pullRequest(g1.PullRequest, false) || g1.Plugin != nil ||
		m.matrix(mat1, false) || m.merge(mer1, false)
}

// merge checks every child for Git or Pull Request generators and nested Matrix/Merge
// generators; Plugin children do not refresh, as upstream.
func (m *appSetMatcher) merge(gen *v1alpha1.MergeGenerator, wildcard bool) bool {
	if gen == nil {
		return false
	}
	for i := range gen.Generators {
		g := &gen.Generators[i]
		if m.git(g.Git, wildcard) || m.pullRequest(g.PullRequest, wildcard) {
			return true
		}
		mat, mer, ok := m.nested(g)
		if !ok {
			return false
		}
		if m.matrix(mat, wildcard) || m.merge(mer, wildcard) {
			return true
		}
	}
	return false
}

// nested decodes the raw Matrix/Merge of a nested generator. ok is false (and the
// ApplicationSet is not refreshed, as upstream) when either is malformed; the
// ApplicationSet controller reports such errors.
func (m *appSetMatcher) nested(g *v1alpha1.ApplicationSetNestedGenerator) (*v1alpha1.MatrixGenerator, *v1alpha1.MergeGenerator, bool) {
	var mat *v1alpha1.MatrixGenerator
	var mer *v1alpha1.MergeGenerator
	if g.Matrix != nil {
		nm, err := v1alpha1.ToNestedMatrixGenerator(g.Matrix)
		if err != nil {
			m.log.Info("webhook: malformed nested matrix generator", "applicationSet", m.appSet.Namespace+"/"+m.appSet.Name, "error", err)
			return nil, nil, false
		}
		mat = &v1alpha1.MatrixGenerator{Generators: terminalToNested(nm.Generators)}
	}
	if g.Merge != nil {
		nm, err := v1alpha1.ToNestedMergeGenerator(g.Merge)
		if err != nil {
			m.log.Info("webhook: malformed nested merge generator", "applicationSet", m.appSet.Namespace+"/"+m.appSet.Name, "error", err)
			return nil, nil, false
		}
		mer = &v1alpha1.MergeGenerator{Generators: terminalToNested(nm.Generators), MergeKeys: nm.MergeKeys}
	}
	return mat, mer, true
}

func terminalToNested(in v1alpha1.ApplicationSetTerminalGenerators) []v1alpha1.ApplicationSetNestedGenerator {
	out := make([]v1alpha1.ApplicationSetNestedGenerator, len(in))
	for i, t := range in {
		out[i] = v1alpha1.ApplicationSetNestedGenerator{
			List:                    t.List,
			Clusters:                t.Clusters,
			Git:                     t.Git,
			SCMProvider:             t.SCMProvider,
			ClusterDecisionResource: t.ClusterDecisionResource,
			PullRequest:             t.PullRequest,
			Plugin:                  t.Plugin,
			Selector:                t.Selector,
		}
	}
	return out
}

// toGenerator builds the top-level form of a nested generator for evaluation.
func toGenerator(g *v1alpha1.ApplicationSetNestedGenerator, mat *v1alpha1.MatrixGenerator, mer *v1alpha1.MergeGenerator) *v1alpha1.ApplicationSetGenerator {
	return &v1alpha1.ApplicationSetGenerator{
		List:                    g.List,
		Clusters:                g.Clusters,
		Git:                     g.Git,
		SCMProvider:             g.SCMProvider,
		ClusterDecisionResource: g.ClusterDecisionResource,
		PullRequest:             g.PullRequest,
		Plugin:                  g.Plugin,
		Matrix:                  mat,
		Merge:                   mer,
		Selector:                g.Selector,
	}
}
