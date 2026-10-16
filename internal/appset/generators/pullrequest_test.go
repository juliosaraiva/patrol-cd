package generators

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

func seededPRs() *fake.Provider {
	p := fake.New(fake.Options{Name: "fake"})
	p.AddRepo(fake.Repo{
		Owner: "org", Name: "app", ID: 1, DefaultBranch: "main",
		Branches: map[string]string{"main": "aaaa"},
		PullRequests: []scm.PullRequest{
			{Number: 1, Title: "feat: add login", Branch: "feat/Login_Page", TargetBranch: "main", HeadSHA: "0123456789abcdef", Author: "alice", Labels: []string{"preview"}},
			{Number: 2, Title: "fix: crash", Branch: "fix/crash", TargetBranch: "release-1", HeadSHA: "fedcba9876543210", Author: "bob"},
			{Number: 3, Title: "chore: deps", Branch: "renovate/deps", TargetBranch: "main", HeadSHA: "1111111111111111", Author: "bot", State: "closed"},
		},
	})
	return p
}

func prGenerator(t *testing.T, f *recordingFactory, cfg SCMConfig, objs ...any) *PullRequestGenerator {
	t.Helper()
	cfg.NewProvider = f.build
	return &PullRequestGenerator{cfg: Config{Client: fakeClient(t, toObjects(objs)...), Namespace: controlNS, SCM: cfg}}
}

func TestPullRequestParams(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededPRs()}
	g := prGenerator(t, f, SCMConfig{Enabled: true})
	gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
		Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app"},
		Values: map[string]string{"host": "pr-{{number}}"},
	}}
	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 2, "open pull requests only")
	assert.Equal(t, map[string]any{
		"number": "1", "title": "feat: add login", "branch": "feat/Login_Page", "branch_slug": "feat-login-page",
		"target_branch": "main", "target_branch_slug": "main", "head_sha": "0123456789abcdef",
		"head_short_sha": "01234567", "head_short_sha_7": "0123456", "author": "alice", "values.host": "pr-1",
	}, got[0])

	gen.PullRequest.Values = map[string]string{"host": "pr-{{.number}}"}
	got, err = g.GenerateParams(context.Background(), gen, appSet(true))
	require.NoError(t, err)
	assert.Equal(t, []string{"preview"}, got[0]["labels"], "labels are a goTemplate-only parameter")
	assert.Equal(t, []string{}, got[1]["labels"])
	assert.Equal(t, map[string]string{"host": "pr-1"}, got[0]["values"].(map[string]string))

	assert.Equal(t, DefaultPullRequestRequeueAfter, g.RequeueAfter(gen))
	gen.PullRequest.RequeueAfterSeconds = ptr.To[int64](15)
	assert.Equal(t, 15*time.Second, g.RequeueAfter(gen))
	gen.PullRequest.RequeueAfterSeconds = ptr.To[int64](0)
	assert.Equal(t, NoRequeueAfter, g.RequeueAfter(gen))
}

func TestPullRequestFilters(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededPRs()}
	g := prGenerator(t, f, SCMConfig{Enabled: true})
	run := func(filters ...v1alpha1.PullRequestGeneratorFilter) []string {
		t.Helper()
		gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
			Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app"}, Filters: filters,
		}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		var out []string
		for _, p := range got {
			out = append(out, p["number"].(string))
		}
		return out
	}
	assert.Equal(t, []string{"1"}, run(v1alpha1.PullRequestGeneratorFilter{BranchMatch: ptr.To("^feat/")}))
	assert.Equal(t, []string{"2"}, run(v1alpha1.PullRequestGeneratorFilter{TargetBranchMatch: ptr.To("^release-")}))
	assert.Equal(t, []string{"2"}, run(v1alpha1.PullRequestGeneratorFilter{TitleMatch: ptr.To("crash")}))
	assert.Empty(t, run(v1alpha1.PullRequestGeneratorFilter{BranchMatch: ptr.To("^feat/"), TargetBranchMatch: ptr.To("^release-")}), "fields of one filter are ANDed")
	assert.Equal(t, []string{"1", "2"}, run(
		v1alpha1.PullRequestGeneratorFilter{BranchMatch: ptr.To("^feat/")},
		v1alpha1.PullRequestGeneratorFilter{TitleMatch: ptr.To("^fix")},
	), "filters are ORed")
	assert.Equal(t, []string{"1", "2"}, run(v1alpha1.PullRequestGeneratorFilter{}), "an empty filter matches everything")

	gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
		Github:  &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app"},
		Filters: []v1alpha1.PullRequestGeneratorFilter{{TitleMatch: ptr.To("(")}},
	}}
	_, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, "error compiling TitleMatch regexp")
}

func TestPullRequestProviderLabelsAndRepoNotFound(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededPRs()}
	g := prGenerator(t, f, SCMConfig{Enabled: true})
	gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
		Gitea: &v1alpha1.PullRequestGeneratorGitea{Owner: "org", Repo: "app", API: "https://gitea.example", Labels: []string{"preview"}},
	}}
	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 1, "the provider-side labels filter")
	assert.Equal(t, "gitea", f.last(t).Type)

	gen.PullRequest.Gitea.Repo = "missing"
	_, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, "error listing repos")
	gen.PullRequest.ContinueOnRepoNotFoundError = true
	got, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
}

func TestPullRequestProviders(t *testing.T) {
	t.Parallel()
	objs := []any{
		secret(appSetNS, "tok", nil, map[string]string{"token": "t0k"}),
		secret(controlNS, "app", nil, map[string]string{"githubAppID": "1", "githubAppPrivateKey": "K"}),
		configMap(appSetNS, "ca", map[string]string{"ca.crt": testCAPEM}),
	}
	tokenOf := func(t *testing.T, req ProviderRequest) string {
		t.Helper()
		require.NotNil(t, req.Config.Token)
		tok, err := req.Config.Token(context.Background())
		require.NoError(t, err)
		return tok
	}
	ref := &v1alpha1.SecretRef{SecretName: "tok", Key: "token"}
	tests := []struct {
		name  string
		gen   v1alpha1.PullRequestGenerator
		check func(t *testing.T, req ProviderRequest)
	}{
		{"github token", v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app", API: "https://ghe/api/v3", TokenRef: ref}},
			func(t *testing.T, req ProviderRequest) {
				assert.Equal(t, "github", req.Type)
				assert.Equal(t, "https://ghe/api/v3", req.Config.APIURL)
				assert.Equal(t, "t0k", tokenOf(t, req))
			}},
		{"github app", v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app", AppSecretName: "app"}},
			func(t *testing.T, req ProviderRequest) {
				require.NotNil(t, req.Config.GitHubApp)
				assert.EqualValues(t, 1, req.Config.GitHubApp.AppID)
				assert.Equal(t, "org", req.Config.GitHubApp.Owner)
			}},
		{"gitlab", v1alpha1.PullRequestGenerator{GitLab: &v1alpha1.PullRequestGeneratorGitLab{Project: "org/app", API: "https://gitlab.example", TokenRef: ref,
			PullRequestState: "opened", Insecure: true, CARef: &v1alpha1.ConfigMapKeyRef{ConfigMapName: "ca", Key: "ca.crt"}}},
			func(t *testing.T, req ProviderRequest) {
				assert.Equal(t, "gitlab", req.Type)
				assert.True(t, req.Config.Insecure)
				assert.Contains(t, string(req.Config.CAData), "BEGIN CERTIFICATE")
				assert.Equal(t, "t0k", tokenOf(t, req))
			}},
		{"bitbucket server basic", v1alpha1.PullRequestGenerator{BitbucketServer: &v1alpha1.PullRequestGeneratorBitbucketServer{Project: "org", Repo: "app", API: "https://bbs",
			BasicAuth: &v1alpha1.BasicAuthBitbucketServer{Username: "u", PasswordRef: ref}}},
			func(t *testing.T, req ProviderRequest) {
				assert.Equal(t, "bitbucketserver", req.Type)
				assert.Equal(t, "u", req.BitbucketServer.Username)
				assert.Equal(t, "t0k", tokenOf(t, req))
			}},
		{"bitbucket cloud bearer", v1alpha1.PullRequestGenerator{Bitbucket: &v1alpha1.PullRequestGeneratorBitbucket{Owner: "org", Repo: "app",
			BearerToken: &v1alpha1.BearerTokenBitbucketCloud{TokenRef: ref}}},
			func(t *testing.T, req ProviderRequest) {
				assert.Equal(t, "bitbucketcloud", req.Type)
				assert.Empty(t, req.BitbucketCloud.Username)
				assert.Equal(t, "t0k", tokenOf(t, req))
			}},
		{"azure devops", v1alpha1.PullRequestGenerator{AzureDevOps: &v1alpha1.PullRequestGeneratorAzureDevOps{Organization: "azorg", Project: "org", Repo: "app", TokenRef: ref, Labels: []string{"x"}}},
			func(t *testing.T, req ProviderRequest) {
				assert.Equal(t, "azuredevops", req.Type)
				assert.Equal(t, "azorg", req.AzureDevOps.Organization)
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &recordingFactory{provider: seededPRs()}
			g := prGenerator(t, f, SCMConfig{Enabled: true}, objs...)
			gen := tt.gen
			as := appSet(false)
			if gen.Github != nil && gen.Github.AppSecretName != "" {
				as.Namespace = controlNS // GitHub App Secrets are reserved to the control plane
			}
			_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{PullRequest: &gen}, as)
			require.NoError(t, err)
			tt.check(t, f.last(t))
		})
	}

	t.Run("github app from a tenant namespace", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededPRs()}
		g := prGenerator(t, f, SCMConfig{Enabled: true}, objs...)
		gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
			Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "victim-org", Repo: "private", AppSecretName: "app"},
		}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorIs(t, err, ErrAppSecretNotPermitted)
		assert.Empty(t, f.requests, "no provider is built with the control plane's GitHub App")
	})

	t.Run("gitlab invalid state", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededPRs()}
		g := prGenerator(t, f, SCMConfig{Enabled: true}, objs...)
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{
			GitLab: &v1alpha1.PullRequestGeneratorGitLab{Project: "42", PullRequestState: "draft"}}}, appSet(false))
		require.ErrorContains(t, err, "invalid GitLab pullRequestState")
	})
}

func TestPullRequestPolicy(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededPRs()}
	g := prGenerator(t, f, SCMConfig{Enabled: false})
	gen := &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "app"}}}
	_, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.EqualError(t, err, "failed to select pull request service provider: scm providers are disabled")

	g = prGenerator(t, f, SCMConfig{Enabled: true, AllowedProviders: []string{"https://ok.example"}})
	gen.PullRequest.Github.API = "https://evil.example"
	_, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, `scm provider "https://evil.example" not allowed, must use one of the following: https://ok.example`)
	assert.Empty(t, f.requests)

	_, err = g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{}}, appSet(false))
	require.ErrorContains(t, err, "no Pull Request provider implementation configured")
}

func TestGitLabProjectRef(t *testing.T) {
	t.Parallel()
	ref, err := gitLabProjectRef("42")
	require.NoError(t, err)
	assert.Equal(t, scm.RepoRef{Name: "42"}, ref)
	ref, err = gitLabProjectRef("group/sub/app")
	require.NoError(t, err)
	assert.Equal(t, "app", ref.Name)
}
