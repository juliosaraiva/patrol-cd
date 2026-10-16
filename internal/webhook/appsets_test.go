package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func gitGen(repo, rev string) *v1alpha1.GitGenerator {
	return &v1alpha1.GitGenerator{RepoURL: repo, Revision: rev}
}

func appSetWith(ns, name string, gens ...v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSet {
	return &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ApplicationSetSpec{Generators: gens},
	}
}

func rawJSON(t testing.TB, v any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return &apiextensionsv1.JSON{Raw: b}
}

func listGen() *v1alpha1.ListGenerator {
	return &v1alpha1.ListGenerator{Elements: []apiextensionsv1.JSON{{Raw: []byte(`{"env":"prod"}`)}}}
}

func pluginGen() *v1alpha1.PluginGenerator {
	return &v1alpha1.PluginGenerator{ConfigMapRef: v1alpha1.PluginConfigMapRef{Name: "plugin"}}
}

func pushAppSetEvent(t testing.TB, webURL, revision string, touchedHead bool) *appSetEvent {
	t.Helper()
	re, err := WebURLRegex(webURL)
	require.NoError(t, err)
	return &appSetEvent{push: &pushInfo{repoRegexps: []*regexp.Regexp{re}, revision: revision, touchedHead: touchedHead}}
}

// prEvent builds a normalized pull request event (nil prInfo for non-refreshing actions).
func prEvent(provider, action string, repo scm.EventRepo) *scm.Event {
	return &scm.Event{Provider: provider, Type: scm.EventPullRequest, Action: action, Repository: &repo, PullRequest: &scm.PullRequest{Number: 1}}
}

var (
	githubPRRepo = scm.EventRepo{
		RepoRef: scm.RepoRef{Owner: "Acme", Name: "Platform"}, HTMLURL: "https://github.com/Acme/Platform",
		APIURL: "https://api.github.com/repos/Acme/Platform",
	}
	gheRepo = scm.EventRepo{
		RepoRef: scm.RepoRef{Owner: "acme", Name: "platform"}, HTMLURL: "https://ghe.example.com/acme/platform",
		APIURL: "https://ghe.example.com/api/v3/repos/acme/platform",
	}
	gitlabPRRepo = scm.EventRepo{
		RepoRef: scm.RepoRef{Owner: "acme", Name: "platform"}, ID: "15", HTMLURL: "https://gitlab.example.com/acme/platform",
	}
	azurePRRepo = scm.EventRepo{RepoRef: scm.RepoRef{Owner: "Platform", Name: "platform"}, CloneURL: "https://dev.azure.com/acme/Platform/_git/platform"}
	giteaPRRepo = scm.EventRepo{RepoRef: scm.RepoRef{Owner: "acme", Name: "platform"}, HTMLURL: "https://gitea.example.com/acme/platform"}
	bbsPRRepo   = scm.EventRepo{RepoRef: scm.RepoRef{Owner: "PRJ", Name: "platform"}, CloneURL: "https://bitbucket.example.com/scm/prj/platform.git"}
	bbcPRRepo   = scm.EventRepo{RepoRef: scm.RepoRef{Owner: "acme", Name: "platform"}, HTMLURL: "https://bitbucket.org/acme/platform"}
)

// stubEvaluator stands in for internal/appset: GenerateParams returns params (or err) and
// InterpolateGenerator substitutes "{{key}}" in the Git generator's repoURL and revision
// and in the GitHub PR generator's repo.
type stubEvaluator struct {
	params []map[string]any
	err    error
	calls  int
}

func (s *stubEvaluator) GenerateParams(context.Context, *v1alpha1.ApplicationSet, *v1alpha1.ApplicationSetGenerator) ([]map[string]any, error) {
	s.calls++
	return s.params, s.err
}

func (s *stubEvaluator) InterpolateGenerator(_ *v1alpha1.ApplicationSet, gen *v1alpha1.ApplicationSetGenerator, params map[string]any) (*v1alpha1.ApplicationSetGenerator, error) {
	out := gen.DeepCopy()
	sub := func(v string) string {
		for k, p := range params {
			v = strings.ReplaceAll(v, "{{"+k+"}}", p.(string))
		}
		return v
	}
	if out.Git != nil {
		out.Git.RepoURL, out.Git.Revision = sub(out.Git.RepoURL), sub(out.Git.Revision)
	}
	if out.PullRequest != nil && out.PullRequest.Github != nil {
		out.PullRequest.Github.Repo = sub(out.PullRequest.Github.Repo)
	}
	return out, nil
}

func matchAppSet(t testing.TB, as *v1alpha1.ApplicationSet, ev *appSetEvent, eval GeneratorEvaluator, providerAPI func(string) string) bool {
	t.Helper()
	m := &appSetMatcher{ctx: context.Background(), appSet: as, ev: ev, eval: eval, log: slog.New(slog.DiscardHandler), providerAPI: providerAPI}
	return m.matches()
}

// ASET-105: Git generators on push events.
func TestAppSetGitGenerator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gen  *v1alpha1.GitGenerator
		ev   *appSetEvent
		want bool
	}{
		{"HEAD revision, default branch push", gitGen(platformRepo, "HEAD"), pushAppSetEvent(t, platformRepo, "main", true), true},
		{"empty revision, default branch push", gitGen(platformRepo, ""), pushAppSetEvent(t, platformRepo, "main", true), true},
		{"HEAD revision, other branch push", gitGen(platformRepo, "HEAD"), pushAppSetEvent(t, platformRepo, "feature", false), false},
		{"scp repoURL", gitGen("git@github.com:acme/platform.git", "HEAD"), pushAppSetEvent(t, platformRepo, "main", true), true},
		{"refs/heads revision", gitGen(platformRepo, "refs/heads/release/1.0"), pushAppSetEvent(t, platformRepo, "release/1.0", false), true},
		{"short revision", gitGen(platformRepo, "release/1.0"), pushAppSetEvent(t, platformRepo, "release/1.0", false), true},
		{"tag revision", gitGen(platformRepo, "v1.0.0"), pushAppSetEvent(t, platformRepo, "v1.0.0", false), true},
		{"other revision", gitGen(platformRepo, "main"), pushAppSetEvent(t, platformRepo, "feature", false), false},
		{"no semver for generators", gitGen(platformRepo, "1.*"), pushAppSetEvent(t, platformRepo, "1.1.0", false), false},
		{"other repository", gitGen("https://github.com/acme/other", "HEAD"), pushAppSetEvent(t, platformRepo, "main", true), false},
		{"templated repoURL without matrix", gitGen("{{url}}", "HEAD"), pushAppSetEvent(t, platformRepo, "main", true), false},
		{"pull request event", gitGen(platformRepo, "HEAD"), &appSetEvent{pr: newPRInfo(prEvent("github", "opened", githubPRRepo))}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			as := appSetWith(testNS, "git", v1alpha1.ApplicationSetGenerator{Git: tc.gen})
			assert.Equal(t, tc.want, matchAppSet(t, as, tc.ev, nil, nil))
		})
	}
}

// ASET-106: Pull Request generators on pull request events (shouldRefreshPRGenerator).
func TestAppSetPullRequestGenerator(t *testing.T) {
	t.Parallel()
	gh := func(owner, repo, api string) *v1alpha1.PullRequestGenerator {
		return &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: owner, Repo: repo, API: api}}
	}
	gl := func(project, api string) *v1alpha1.PullRequestGenerator {
		return &v1alpha1.PullRequestGenerator{GitLab: &v1alpha1.PullRequestGeneratorGitLab{Project: project, API: api}}
	}
	az := func(project, repo string) *v1alpha1.PullRequestGenerator {
		return &v1alpha1.PullRequestGenerator{AzureDevOps: &v1alpha1.PullRequestGeneratorAzureDevOps{Organization: "acme", Project: project, Repo: repo}}
	}
	ghRef := &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "acme", Repo: "platform", ProviderRef: &v1alpha1.SCMProviderReference{Name: "ghe"}}}
	providerAPI := func(name string) string {
		if name == "ghe" {
			return "https://ghe.example.com/api/v3"
		}
		return ""
	}
	tests := []struct {
		name string
		gen  *v1alpha1.PullRequestGenerator
		ev   *scm.Event
		want bool
	}{
		{"github opened", gh("acme", "platform", ""), prEvent("github", "opened", githubPRRepo), true},
		{"github closed", gh("acme", "platform", ""), prEvent("github", "closed", githubPRRepo), true},
		{"github synchronize", gh("acme", "platform", ""), prEvent("github", "synchronize", githubPRRepo), true},
		{"github labeled", gh("acme", "platform", ""), prEvent("github", "labeled", githubPRRepo), true},
		{"github unlabeled", gh("acme", "platform", ""), prEvent("github", "unlabeled", githubPRRepo), true},
		{"github reopened", gh("acme", "platform", ""), prEvent("github", "reopened", githubPRRepo), true},
		{"github edited is ignored", gh("acme", "platform", ""), prEvent("github", "edited", githubPRRepo), false},
		{"github assigned is ignored", gh("acme", "platform", ""), prEvent("github", "assigned", githubPRRepo), false},
		{"github owner and repo are case-insensitive", gh("ACME", "platform", ""), prEvent("github", "opened", githubPRRepo), true},
		{"github other repo", gh("acme", "other", ""), prEvent("github", "opened", githubPRRepo), false},
		{"github explicit api", gh("acme", "platform", "https://api.github.com/"), prEvent("github", "opened", githubPRRepo), true},
		{"github api on another host", gh("acme", "platform", "https://ghe.example.com/"), prEvent("github", "opened", githubPRRepo), false},
		{"github enterprise default api does not match", gh("acme", "platform", ""), prEvent("github", "opened", gheRepo), false},
		{"github enterprise api with path (Patrol extension)", gh("acme", "platform", "https://ghe.example.com/api/v3/"), prEvent("github", "opened", gheRepo), true},
		{"github providerRef api", ghRef, prEvent("github", "opened", gheRepo), true},
		{"github generator on a gitlab event", gh("acme", "platform", ""), prEvent("gitlab", "opened", gitlabPRRepo), false},
		{"gitlab project id", gl("15", "https://gitlab.example.com/"), prEvent("gitlab", "synchronize", gitlabPRRepo), true},
		{"gitlab project path (Patrol extension)", gl("acme/platform", "https://gitlab.example.com"), prEvent("gitlab", "closed", gitlabPRRepo), true},
		{"gitlab other project id", gl("16", "https://gitlab.example.com/"), prEvent("gitlab", "opened", gitlabPRRepo), false},
		{"gitlab default api is gitlab.com", gl("15", ""), prEvent("gitlab", "opened", gitlabPRRepo), false},
		{"gitlab non-refreshing action", gl("15", "https://gitlab.example.com/"), prEvent("gitlab", "approved", gitlabPRRepo), false},
		{"azure project and repo", az("Platform", "platform"), prEvent("azuredevops", "opened", azurePRRepo), true},
		{"azure names are case-sensitive", az("platform", "platform"), prEvent("azuredevops", "opened", azurePRRepo), false},
		{"azure other repo", az("Platform", "other"), prEvent("azuredevops", "synchronize", azurePRRepo), false},
		// Patrol extensions: upstream's ApplicationSet webhook ignores these providers.
		{"gitea owner and repo", &v1alpha1.PullRequestGenerator{Gitea: &v1alpha1.PullRequestGeneratorGitea{Owner: "ACME", Repo: "platform", API: "https://gitea.example.com/"}}, prEvent("gitea", "opened", giteaPRRepo), true},
		{"gitea other host", &v1alpha1.PullRequestGenerator{Gitea: &v1alpha1.PullRequestGeneratorGitea{Owner: "acme", Repo: "platform", API: "https://gitea.other.com/"}}, prEvent("gitea", "opened", giteaPRRepo), false},
		{"bitbucket server project and repo", &v1alpha1.PullRequestGenerator{BitbucketServer: &v1alpha1.PullRequestGeneratorBitbucketServer{Project: "prj", Repo: "platform", API: "https://bitbucket.example.com/rest"}}, prEvent("bitbucketserver", "synchronize", bbsPRRepo), true},
		{"bitbucket server other repo", &v1alpha1.PullRequestGenerator{BitbucketServer: &v1alpha1.PullRequestGeneratorBitbucketServer{Project: "prj", Repo: "other", API: "https://bitbucket.example.com/rest"}}, prEvent("bitbucketserver", "opened", bbsPRRepo), false},
		{"bitbucket cloud owner and repo", &v1alpha1.PullRequestGenerator{Bitbucket: &v1alpha1.PullRequestGeneratorBitbucket{Owner: "acme", Repo: "platform"}}, prEvent("bitbucketcloud", "closed", bbcPRRepo), true},
		{"bitbucket cloud other owner", &v1alpha1.PullRequestGenerator{Bitbucket: &v1alpha1.PullRequestGeneratorBitbucket{Owner: "other", Repo: "platform"}}, prEvent("bitbucketcloud", "closed", bbcPRRepo), false},
		{"gitea generator on a github event", &v1alpha1.PullRequestGenerator{Gitea: &v1alpha1.PullRequestGeneratorGitea{Owner: "acme", Repo: "platform"}}, prEvent("github", "opened", githubPRRepo), false},
		{"unknown provider", gh("acme", "platform", ""), prEvent("gogs", "opened", githubPRRepo), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := &appSetEvent{pr: newPRInfo(tc.ev)}
			as := appSetWith(testNS, "pr", v1alpha1.ApplicationSetGenerator{PullRequest: tc.gen})
			assert.Equal(t, tc.want, matchAppSet(t, as, ev, nil, providerAPI))
		})
	}
	t.Run("push events do not refresh pull request generators", func(t *testing.T) {
		t.Parallel()
		as := appSetWith(testNS, "pr", v1alpha1.ApplicationSetGenerator{PullRequest: gh("acme", "platform", "")})
		assert.False(t, matchAppSet(t, as, pushAppSetEvent(t, platformRepo, "main", true), nil, nil))
	})
}

// ASET-107: Plugin, Matrix and Merge generators, including nested combinations.
func TestAppSetCombinationGenerators(t *testing.T) {
	t.Parallel()
	push := pushAppSetEvent(t, platformRepo, "main", true)
	match := gitGen(platformRepo, "HEAD")
	noMatch := gitGen("https://github.com/acme/other", "HEAD")
	templated := gitGen("{{url}}", "HEAD")

	matrix := func(a, b v1alpha1.ApplicationSetNestedGenerator) v1alpha1.ApplicationSetGenerator {
		return v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{a, b}}}
	}
	merge := func(gens ...v1alpha1.ApplicationSetNestedGenerator) v1alpha1.ApplicationSetGenerator {
		return v1alpha1.ApplicationSetGenerator{Merge: &v1alpha1.MergeGenerator{Generators: gens, MergeKeys: []string{"env"}}}
	}
	nestedMatrix := func(t testing.TB, a, b v1alpha1.ApplicationSetTerminalGenerator) v1alpha1.ApplicationSetNestedGenerator {
		return v1alpha1.ApplicationSetNestedGenerator{Matrix: rawJSON(t, v1alpha1.NestedMatrixGenerator{Generators: v1alpha1.ApplicationSetTerminalGenerators{a, b}})}
	}
	nestedMerge := func(t testing.TB, gens ...v1alpha1.ApplicationSetTerminalGenerator) v1alpha1.ApplicationSetNestedGenerator {
		return v1alpha1.ApplicationSetNestedGenerator{Merge: rawJSON(t, v1alpha1.NestedMergeGenerator{Generators: gens, MergeKeys: []string{"env"}})}
	}
	list := v1alpha1.ApplicationSetNestedGenerator{List: listGen()}

	tests := []struct {
		name string
		gen  v1alpha1.ApplicationSetGenerator
		eval *stubEvaluator
		ev   *appSetEvent
		want bool
	}{
		{"top-level plugin refreshes on any push", v1alpha1.ApplicationSetGenerator{Plugin: pluginGen()}, nil, push, true},
		{"top-level plugin refreshes on pull request events", v1alpha1.ApplicationSetGenerator{Plugin: pluginGen()}, nil, &appSetEvent{pr: newPRInfo(prEvent("github", "opened", githubPRRepo))}, true},
		{"list only", v1alpha1.ApplicationSetGenerator{List: listGen()}, nil, push, false},
		{"matrix: first child git", matrix(v1alpha1.ApplicationSetNestedGenerator{Git: match}, list), nil, push, true},
		{"matrix: second child git, no params", matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: match}), &stubEvaluator{}, push, true},
		{"matrix: second child plugin", matrix(list, v1alpha1.ApplicationSetNestedGenerator{Plugin: pluginGen()}), &stubEvaluator{}, push, true},
		{"matrix: first child plugin does not refresh", matrix(v1alpha1.ApplicationSetNestedGenerator{Plugin: pluginGen()}, list), &stubEvaluator{}, push, false},
		{"matrix: unrelated children", matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: noMatch}), &stubEvaluator{}, push, false},
		{
			"matrix: templated second child interpolated to the pushed repo",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: templated}),
			&stubEvaluator{params: []map[string]any{{"url": "https://github.com/acme/other"}, {"url": "git@github.com:acme/platform.git"}}}, push, true,
		},
		{
			"matrix: templated second child interpolated to other repos",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: templated}),
			&stubEvaluator{params: []map[string]any{{"url": "https://github.com/acme/other"}}}, push, false,
		},
		{
			"matrix: evaluation error does not refresh",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: templated}),
			&stubEvaluator{err: errors.New("repo-server unavailable")}, push, false,
		},
		{
			"matrix without evaluator: templated second child is a wildcard",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: templated}), nil, push, true,
		},
		{
			"matrix without evaluator: non-templated mismatch still excluded",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: noMatch}), nil, push, false,
		},
		{
			"matrix: nested matrix first child",
			matrix(nestedMatrix(t, v1alpha1.ApplicationSetTerminalGenerator{List: listGen()}, v1alpha1.ApplicationSetTerminalGenerator{Git: match}), list),
			&stubEvaluator{}, push, true,
		},
		{
			"matrix: nested merge first child",
			matrix(nestedMerge(t, v1alpha1.ApplicationSetTerminalGenerator{List: listGen()}, v1alpha1.ApplicationSetTerminalGenerator{Git: match}), list),
			&stubEvaluator{}, push, true,
		},
		{
			"matrix: nested matrix second child",
			matrix(list, nestedMatrix(t, v1alpha1.ApplicationSetTerminalGenerator{List: listGen()}, v1alpha1.ApplicationSetTerminalGenerator{Git: match})),
			&stubEvaluator{}, push, true,
		},
		{
			"matrix: malformed nested matrix",
			matrix(v1alpha1.ApplicationSetNestedGenerator{Matrix: &apiextensionsv1.JSON{Raw: []byte(`{"generators":[{}]}`)}}, v1alpha1.ApplicationSetNestedGenerator{Git: match}),
			&stubEvaluator{}, push, false,
		},
		{
			"matrix: wrong child count",
			v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{{Git: match}}}},
			nil, push, false,
		},
		{"merge: git child", merge(list, v1alpha1.ApplicationSetNestedGenerator{Git: match}), nil, push, true},
		{"merge: plugin child does not refresh", merge(list, v1alpha1.ApplicationSetNestedGenerator{Plugin: pluginGen()}), nil, push, false},
		{"merge: unrelated git child", merge(list, v1alpha1.ApplicationSetNestedGenerator{Git: noMatch}), nil, push, false},
		{
			"merge: nested matrix child",
			merge(list, nestedMatrix(t, v1alpha1.ApplicationSetTerminalGenerator{Git: match}, v1alpha1.ApplicationSetTerminalGenerator{List: listGen()})),
			&stubEvaluator{}, push, true,
		},
		{
			"merge: nested merge child",
			merge(list, nestedMerge(t, v1alpha1.ApplicationSetTerminalGenerator{List: listGen()}, v1alpha1.ApplicationSetTerminalGenerator{Git: match})),
			nil, push, true,
		},
		{
			"merge: pull request child",
			merge(list, v1alpha1.ApplicationSetNestedGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "acme", Repo: "platform"}}}),
			nil, &appSetEvent{pr: newPRInfo(prEvent("github", "labeled", githubPRRepo))}, true,
		},
		{
			"matrix: templated PR generator interpolated",
			matrix(list, v1alpha1.ApplicationSetNestedGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "acme", Repo: "{{repo}}"}}}),
			&stubEvaluator{params: []map[string]any{{"repo": "platform"}}}, &appSetEvent{pr: newPRInfo(prEvent("github", "opened", githubPRRepo))}, true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var eval GeneratorEvaluator
			if tc.eval != nil {
				eval = tc.eval
			}
			assert.Equal(t, tc.want, matchAppSet(t, appSetWith(testNS, "combo", tc.gen), tc.ev, eval, nil))
		})
	}
	t.Run("interpolation budget refreshes conservatively", func(t *testing.T) {
		t.Parallel()
		params := make([]map[string]any, maxInterpolations+1)
		for i := range params {
			params[i] = map[string]any{"url": "https://github.com/acme/other"}
		}
		gen := matrix(list, v1alpha1.ApplicationSetNestedGenerator{Git: templated})
		assert.True(t, matchAppSet(t, appSetWith(testNS, "big", gen), push, &stubEvaluator{params: params}, nil))
	})
}

// ASET-104/ASET-011 and ASET-100 through the handler: matching ApplicationSets in allowed
// namespaces get cd.patrol.example/application-set-refresh=true.
func TestEventsRefreshApplicationSets(t *testing.T) {
	t.Parallel()
	prGen := v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "acme", Repo: "platform"}}}
	objs := []client.Object{
		appSetWith(testNS, "git", v1alpha1.ApplicationSetGenerator{Git: gitGen("git@github.com:acme/platform.git", "HEAD")}),
		appSetWith(testNS, "git-branch", v1alpha1.ApplicationSetGenerator{Git: gitGen(platformRepo, "stable")}),
		appSetWith(testNS, "pr", prGen),
		appSetWith(testNS, "plugin", v1alpha1.ApplicationSetGenerator{Plugin: pluginGen()}),
		appSetWith("team-a", "tenant-pr", prGen),
		appSetWith("team-b", "not-allowed", prGen),
		appSetWith(testNS, "pr-provider-ref", v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{
			Owner: "acme", Repo: "platform", ProviderRef: &v1alpha1.SCMProviderReference{Name: "github-com"},
		}}}),
		appSetWith(testNS, "pr-ghe-ref", v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{
			Owner: "acme", Repo: "platform", ProviderRef: &v1alpha1.SCMProviderReference{Name: "ghe"},
		}}}),
		&v1alpha1.SCMProvider{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github-com"}, Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: "https://api.github.com"}},
		&v1alpha1.SCMProvider{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "ghe"}, Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: "https://ghe.example.com/api/v3"}},
	}
	h := newTestHandler(t, Config{
		ApplicationSetNamespaces: []string{"team-a"},
		Settings:                 staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)}),
	}, objs...)
	h.start(t)
	get := func(ns, name string) map[string]string {
		return annotations(t, h.client, &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
	}
	refreshed := func(ns, name string) bool {
		return get(ns, name)[v1alpha1.AnnotationKeyApplicationSetRefresh] == "true"
	}

	body := fixture(t, "github_pull_request.json")
	w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{
		"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "pr-1", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, body),
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	eventually(t, 10*time.Second, func() bool {
		return refreshed(testNS, "pr") && refreshed(testNS, "plugin") && refreshed("team-a", "tenant-pr") && refreshed(testNS, "pr-provider-ref")
	}, "pull request generators refreshed")
	eventually(t, 5*time.Second, func() bool { return containsAll(h.logs.String(), `"applicationSets":4`) }, "audit log")
	assert.False(t, refreshed(testNS, "pr-ghe-ref"), "the referenced SCMProvider API is on another host")
	assert.False(t, refreshed(testNS, "git"), "pull request events do not refresh git generators")
	assert.False(t, refreshed("team-b", "not-allowed"))

	push := fixture(t, "github_push.json")
	w = h.serve(newRequest(t, "/api/webhook", push, map[string]string{
		"X-GitHub-Event": "push", "X-GitHub-Delivery": "push-1", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, push),
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	eventually(t, 10*time.Second, func() bool { return refreshed(testNS, "git") }, "git generator refreshed")
	assert.False(t, refreshed(testNS, "git-branch"))
}
