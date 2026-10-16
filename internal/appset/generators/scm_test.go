package generators

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

// recordingFactory returns the fake provider and records construction requests.
type recordingFactory struct {
	mu       sync.Mutex
	provider scm.Provider
	requests []ProviderRequest
}

func (f *recordingFactory) build(_ context.Context, req ProviderRequest) (scm.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.provider, nil
}

func (f *recordingFactory) last(t *testing.T) ProviderRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.requests)
	return f.requests[len(f.requests)-1]
}

func seededSCM() *fake.Provider {
	p := fake.New(fake.Options{Name: "fake"})
	p.AddRepo(fake.Repo{
		Owner: "org", Name: "myapp-api", ID: 1, DefaultBranch: "main",
		Branches: map[string]string{"main": "0123456789abcdef", "release-1": "aaaaaaaaaaaa", "feature": "bbbbbb"},
		Files: map[string][]string{
			"0123456789abcdef": {"kustomization.yaml", "deploy/app.yaml"},
			"aaaaaaaaaaaa":     {"kustomization.yaml"},
		},
		Topics: []string{"team-a", "deploy-ok"},
	})
	p.AddRepo(fake.Repo{
		Owner: "org", Name: "other-myapp", ID: 2, DefaultBranch: "master",
		Branches: map[string]string{"master": "abc"},
		Files:    map[string][]string{"abc": {"disabledrepo.txt", "kustomization.yaml"}},
	})
	p.AddRepo(fake.Repo{Owner: "org", Name: "empty", ID: 3, DefaultBranch: "main"})
	p.AddRepo(fake.Repo{Owner: "org", Name: "archived", ID: 4, DefaultBranch: "main", Branches: map[string]string{"main": "def"}, Archived: true})
	return p
}

func scmGenerator(t *testing.T, factory *recordingFactory, cfg SCMConfig, objs ...any) *SCMProviderGenerator {
	t.Helper()
	cfg.NewProvider = factory.build
	return &SCMProviderGenerator{cfg: Config{Client: fakeClient(t, toObjects(objs)...), Namespace: controlNS, SCM: cfg}}
}

func TestSCMProviderParams(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededSCM()}
	g := scmGenerator(t, f, SCMConfig{Enabled: true})
	gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
		Github: &v1alpha1.SCMProviderGeneratorGithub{Organization: "org"},
		Values: map[string]string{"name": "{{repository}}-{{branch}}"},
	}}
	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 3, "the empty repository yields nothing")
	assert.Equal(t, map[string]any{
		"organization": "org", "repository": "archived", "repository_id": int64(4),
		"url": "git@fake.example:org/archived.git", "branch": "main", "sha": "def",
		"short_sha": "def", "short_sha_7": "def", "labels": "", "branchNormalized": "main",
		"values.name": "archived-main",
	}, got[0])
	assert.Equal(t, map[string]any{
		"organization": "org", "repository": "myapp-api", "repository_id": int64(1),
		"url": "git@fake.example:org/myapp-api.git", "branch": "main", "sha": "0123456789abcdef",
		"short_sha": "01234567", "short_sha_7": "0123456", "labels": "team-a,deploy-ok", "branchNormalized": "main",
		"values.name": "myapp-api-main",
	}, got[1])

	req := f.last(t)
	assert.Equal(t, "github", req.Type)
	assert.Empty(t, req.Config.APIURL)
	assert.Nil(t, req.Config.Token, "no tokenRef: anonymous")
	assert.Equal(t, DefaultSCMProviderRequeueAfter, g.RequeueAfter(gen))
	gen.SCMProvider.RequeueAfterSeconds = ptr.To[int64](60)
	assert.EqualValues(t, 60_000_000_000, g.RequeueAfter(gen))

	gen.SCMProvider.CloneProtocol = "https"
	gen.SCMProvider.Github.ExcludeArchivedRepos = true
	got, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "https://fake.example/org/myapp-api.git", got[0]["url"])

	gen.SCMProvider.CloneProtocol = "ftp"
	_, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, "error listing repos")
}

func TestSCMProviderFilters(t *testing.T) {
	t.Parallel()
	f := &recordingFactory{provider: seededSCM()}
	g := scmGenerator(t, f, SCMConfig{Enabled: true})
	tests := []struct {
		name        string
		allBranches bool
		filters     []v1alpha1.SCMProviderGeneratorFilter
		want        []string // repository@branch
		wantErr     string
	}{
		{name: "anchored repositoryMatch", filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("^myapp")}}, want: []string{"myapp-api@main"}},
		{name: "unanchored repositoryMatch", filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("myapp")}}, want: []string{"myapp-api@main", "other-myapp@master"}},
		{name: "labelMatch", filters: []v1alpha1.SCMProviderGeneratorFilter{{LabelMatch: ptr.To("deploy-ok")}}, want: []string{"myapp-api@main"}},
		{name: "two repo filters are OR", filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("^other")}, {LabelMatch: ptr.To("team-a")}}, want: []string{"myapp-api@main", "other-myapp@master"}},
		{name: "pathsExist all required", filters: []v1alpha1.SCMProviderGeneratorFilter{{PathsExist: []string{"kustomization.yaml", "deploy/"}}}, want: []string{"myapp-api@main"}},
		{name: "pathsDoNotExist", filters: []v1alpha1.SCMProviderGeneratorFilter{{PathsDoNotExist: []string{"disabledrepo.txt"}}}, want: []string{"archived@main", "myapp-api@main"}},
		{name: "repo and branch filters in separate entries are AND", allBranches: true,
			filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("api")}, {BranchMatch: ptr.To("^release-")}}, want: []string{"myapp-api@release-1"}},
		{name: "branch filter with repositoryMatch evaluated per branch", allBranches: true,
			filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("myapp"), PathsExist: []string{"kustomization.yaml"}}},
			want:    []string{"myapp-api@main", "myapp-api@release-1", "other-myapp@master"}},
		{name: "all branches without filters", allBranches: true,
			want: []string{"archived@main", "myapp-api@feature", "myapp-api@main", "myapp-api@release-1", "other-myapp@master"}},
		{name: "empty filter ignored", filters: []v1alpha1.SCMProviderGeneratorFilter{{}}, want: []string{"archived@main", "myapp-api@main", "other-myapp@master"}},
		{name: "bad regexp", filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("(")}}, wantErr: "error compiling RepositoryMatch regexp"},
		{name: "bad label regexp", filters: []v1alpha1.SCMProviderGeneratorFilter{{LabelMatch: ptr.To("(")}}, wantErr: "error compiling LabelMatch regexp"},
		{name: "bad branch regexp", filters: []v1alpha1.SCMProviderGeneratorFilter{{BranchMatch: ptr.To("(")}}, wantErr: "error compiling BranchMatch regexp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
				Github:  &v1alpha1.SCMProviderGeneratorGithub{Organization: "org", AllBranches: tc.allBranches},
				Filters: tc.filters,
			}}
			got, err := g.GenerateParams(context.Background(), gen, appSet(false))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			var names []string
			for _, p := range got {
				names = append(names, p["repository"].(string)+"@"+p["branch"].(string))
			}
			assert.Equal(t, tc.want, names)
		})
	}
}

func TestSCMProviderCredentialsAndPolicy(t *testing.T) {
	t.Parallel()
	objs := []any{
		secret(appSetNS, "gh", nil, map[string]string{"token": "s3cr3t"}),
		secret(appSetNS, "labelled", map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeSCMCreds}, map[string]string{"token": "ok"}),
		secret(controlNS, "gh", nil, map[string]string{"token": "wrong-namespace"}),
		secret(controlNS, "gh-app", nil, map[string]string{"githubAppID": "42", "githubAppInstallationID": "7", "githubAppPrivateKey": "KEY", "githubAppEnterpriseBaseUrl": "https://ghe/api/v3"}),
		secret(controlNS, "gh-app-nokey", nil, map[string]string{"githubAppID": "42"}),
		configMap(appSetNS, "ca", map[string]string{"ca.crt": testCAPEM}),
	}
	newGen := func(cfg SCMConfig) (*SCMProviderGenerator, *recordingFactory) {
		f := &recordingFactory{provider: seededSCM()}
		return scmGenerator(t, f, cfg, objs...), f
	}

	t.Run("tokenRef read from the ApplicationSet namespace", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: true})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{
			Organization: "org", API: "https://ghe.example/api/v3", TokenRef: &v1alpha1.SecretRef{SecretName: "gh", Key: "token"},
		}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		req := f.last(t)
		tok, err := req.Config.Token(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t", tok)
		assert.Equal(t, "https://ghe.example/api/v3", req.Config.APIURL)
	})

	t.Run("strict mode", func(t *testing.T) {
		g, _ := newGen(SCMConfig{Enabled: true, TokenRefStrictMode: true})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{
			Organization: "org", TokenRef: &v1alpha1.SecretRef{SecretName: "gh", Key: "token"},
		}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, `secret team-a/gh is not a valid SCM creds secret: secret must have label "cd.patrol.example/secret-type"="scm-creds"`)
		gen.SCMProvider.Github.TokenRef.SecretName = "labelled"
		_, err = g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
	})

	t.Run("missing secret and key", func(t *testing.T) {
		g, _ := newGen(SCMConfig{Enabled: true})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Gitlab: &v1alpha1.SCMProviderGeneratorGitlab{
			Group: "org", TokenRef: &v1alpha1.SecretRef{SecretName: "nope", Key: "token"},
		}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, "error fetching Gitlab token: error fetching secret team-a/nope")
		gen.SCMProvider.Gitlab.TokenRef = &v1alpha1.SecretRef{SecretName: "gh", Key: "nokey"}
		_, err = g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, `key "nokey" in secret team-a/gh not found`)
	})

	t.Run("disabled", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: false})
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			Github: &v1alpha1.SCMProviderGeneratorGithub{Organization: "org"},
		}}, appSet(false))
		require.ErrorIs(t, err, ErrSCMProvidersDisabled)
		assert.Empty(t, f.requests)
	})

	t.Run("allow-list", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: true, AllowedProviders: []string{"https://git.example.com"}})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Gitea: &v1alpha1.SCMProviderGeneratorGitea{
			Owner: "org", API: "https://evil.example.com",
		}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.EqualError(t, err, `scm provider not allowed: scm provider "https://evil.example.com" not allowed, must use one of the following: https://git.example.com`)
		gen.SCMProvider.Gitea.API = "https://git.example.com/"
		_, err = g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, "not allowed", "trailing slash differs")
		assert.Empty(t, f.requests, "no provider is built for a disallowed URL")
		gen.SCMProvider.Gitea.API = "https://git.example.com"
		_, err = g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		// An empty api (provider default) is always allowed.
		_, err = g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			Github: &v1alpha1.SCMProviderGeneratorGithub{Organization: "org"},
		}}, appSet(false))
		require.NoError(t, err)
	})

	t.Run("github app", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: true})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{
			Organization: "org", AppSecretName: "gh-app", TokenRef: &v1alpha1.SecretRef{SecretName: "gh", Key: "token"},
		}}}
		// The GitHub App Secret holds the control plane's credentials: tenants cannot name it.
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorIs(t, err, ErrAppSecretNotPermitted)
		assert.ErrorContains(t, err, "use providerRef with an SCMProvider in namespace "+appSetNS)
		assert.Empty(t, f.requests, "no provider is built with the control plane's GitHub App")

		controlPlane := appSet(false)
		controlPlane.Namespace = controlNS
		_, err = g.GenerateParams(context.Background(), gen, controlPlane)
		require.NoError(t, err)
		req := f.last(t)
		require.NotNil(t, req.Config.GitHubApp)
		assert.Equal(t, scm.GitHubAppConfig{AppID: 42, InstallationID: 7, PrivateKey: []byte("KEY"), EnterpriseBaseURL: "https://ghe/api/v3", Owner: "org"}, *req.Config.GitHubApp)
		assert.Nil(t, req.Config.Token, "the app takes precedence over tokenRef")

		gen.SCMProvider.Github.AppSecretName = "gh-app-nokey"
		_, err = g.GenerateParams(context.Background(), gen, controlPlane)
		require.ErrorContains(t, err, "error fetching Github app secret: no github app found for gh-app-nokey")
		gen.SCMProvider.Github.AppSecretName = "missing"
		_, err = g.GenerateParams(context.Background(), gen, controlPlane)
		require.ErrorContains(t, err, "error fetching Github app secret: error getting creds for missing")
	})

	t.Run("gitlab options and CA", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: true, ProxyURL: "http://proxy:3128", NoProxy: "internal"})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Gitlab: &v1alpha1.SCMProviderGeneratorGitlab{
			Group: "org", API: "https://gitlab.example", IncludeSubgroups: true, IncludeSharedProjects: ptr.To(false),
			Topic: "deploy", Insecure: true, IncludeArchivedRepos: true, CARef: &v1alpha1.ConfigMapKeyRef{ConfigMapName: "ca", Key: "ca.crt"},
		}}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		assert.Empty(t, got, "no repository carries the topic")
		req := f.last(t)
		assert.Equal(t, "gitlab", req.Type)
		assert.True(t, req.GitLab.IncludeSubgroups)
		assert.Equal(t, ptr.To(false), req.GitLab.IncludeSharedProjects)
		assert.True(t, req.Config.Insecure)
		assert.Contains(t, string(req.Config.CAData), "BEGIN CERTIFICATE")
		assert.Equal(t, "http://proxy:3128", req.Config.ProxyURL)
		assert.Equal(t, "internal", req.Config.NoProxy)

		gen.SCMProvider.Gitlab.CARef.Key = "missing"
		_, err = g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, "error fetching CA certificates from ConfigMap: key missing not found in ConfigMap ca")
	})

	t.Run("bitbucket server auth precedence and no proxy for azure", func(t *testing.T) {
		g, f := newGen(SCMConfig{Enabled: true, ProxyURL: "http://proxy:3128"})
		gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{BitbucketServer: &v1alpha1.SCMProviderGeneratorBitbucketServer{
			Project: "org", API: "https://bbs.example",
			BasicAuth:   &v1alpha1.BasicAuthBitbucketServer{Username: "user", PasswordRef: &v1alpha1.SecretRef{SecretName: "gh", Key: "token"}},
			BearerToken: &v1alpha1.BearerTokenBitbucket{TokenRef: &v1alpha1.SecretRef{SecretName: "labelled", Key: "token"}},
		}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		req := f.last(t)
		assert.Empty(t, req.BitbucketServer.Username, "bearer token wins")
		tok, _ := req.Config.Token(context.Background())
		assert.Equal(t, "ok", tok)
		assert.Equal(t, "http://proxy:3128", req.Config.ProxyURL)

		gen = &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{AzureDevOps: &v1alpha1.SCMProviderGeneratorAzureDevOps{
			Organization: "azorg", TeamProject: "org", AccessTokenRef: &v1alpha1.SecretRef{SecretName: "gh", Key: "token"},
		}}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		req = f.last(t)
		assert.Equal(t, "azuredevops", req.Type)
		assert.Equal(t, "azorg", req.AzureDevOps.Organization)
		assert.Empty(t, req.Config.ProxyURL, "Azure DevOps is not proxied")
		assert.Equal(t, "azorg", got[0]["organization"], "the organization parameter is the Azure organization")
	})

	t.Run("no provider", func(t *testing.T) {
		g, _ := newGen(SCMConfig{Enabled: true})
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{}}, appSet(false))
		require.EqualError(t, err, "no SCM provider implementation configured")
	})
}

func TestSCMProviderRefThroughRegistry(t *testing.T) {
	t.Parallel()
	registry := scm.NewRegistry()
	p := seededSCM()
	require.NoError(t, fake.Register(registry, p))
	objs := []any{
		&v1alpha1.SCMProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "fake", Namespace: appSetNS},
			Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeFake, APIURL: "https://scm.example",
				Auth: &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "tok", Key: "token"}}},
		},
		&v1alpha1.SCMProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "gitlab", Namespace: appSetNS},
			Spec:       v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitLab},
		},
		secret(appSetNS, "tok", nil, map[string]string{"token": "t"}),
	}
	g := &SCMProviderGenerator{cfg: Config{Client: fakeClient(t, toObjects(objs)...), Namespace: controlNS,
		SCM: SCMConfig{Enabled: true, Registry: registry, AllowedProviders: []string{"https://scm.example"}}}}
	gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{
		Organization: "org", ProviderRef: &v1alpha1.SCMProviderReference{Name: "fake"},
	}}}
	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.Equal(t, 1, p.Calls(fake.OpListRepositories))

	gen.SCMProvider.Github.ProviderRef.Name = "gitlab"
	_, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, `SCMProvider team-a/gitlab has type "gitlab", but the generator requires "github"`)

	gen.SCMProvider.Github.ProviderRef.Name = "missing"
	_, err = g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, "error getting SCMProvider team-a/missing")
}

// TestSCMProviderAllowListWithRealProvider checks with the real GitHub provider that a
// disallowed API URL causes no outbound request and an allowed one is used.
func TestSCMProviderAllowListWithRealProvider(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/orgs/org/repos":
			_, _ = w.Write([]byte(`[{"id":9,"name":"svc","full_name":"org/svc","owner":{"login":"org"},"default_branch":"main","ssh_url":"git@ghe:org/svc.git","clone_url":"https://ghe/org/svc.git","topics":["x"]}]`))
		case "/api/v3/repos/org/svc/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","commit":{"sha":"feedfacecafe"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	api := srv.URL + "/api/v3"

	g := &SCMProviderGenerator{cfg: Config{Client: fakeClient(t), Namespace: controlNS,
		SCM: SCMConfig{Enabled: true, AllowedProviders: []string{"https://other.example"}}}}
	gen := &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{Organization: "org", API: api}}}
	_, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.ErrorContains(t, err, "not allowed")
	mu.Lock()
	assert.Empty(t, paths, "no request reached the SCM")
	mu.Unlock()

	g.cfg.SCM.AllowedProviders = []string{api}
	got, err := g.GenerateParams(context.Background(), gen, appSet(true))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "svc", got[0]["repository"])
	assert.Equal(t, "feedfacecafe", got[0]["sha"])
	assert.Equal(t, "feedface", got[0]["short_sha"])
	assert.Equal(t, "x", got[0]["labels"])
}
