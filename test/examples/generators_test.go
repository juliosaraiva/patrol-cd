package examples_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

// localRepos serves the Git generator from the example directories (exampleRepos).
type localRepos struct{}

var _ generators.RepoService = localRepos{}

func (localRepos) root(repoURL string) (string, error) {
	rel, ok := exampleRepos[repoURL]
	if !ok {
		return "", fmt.Errorf("repository %s is not provided by the examples", repoURL)
	}
	return filepath.Join(examplesDir, filepath.FromSlash(rel)), nil
}

// GetFiles returns the files matching a doublestar pattern relative to the repository root.
func (l localRepos) GetFiles(_ context.Context, req generators.RepoRequest, pattern string) (map[string][]byte, error) {
	root, err := l.root(req.RepoURL)
	if err != nil {
		return nil, err
	}
	fsys := os.DirFS(root)
	matches, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly())
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(matches))
	for _, m := range matches {
		data, err := fs.ReadFile(fsys, m)
		if err != nil {
			return nil, err
		}
		out[m] = data
	}
	return out, nil
}

// GetDirectories returns every directory below the repository root (root excluded).
func (l localRepos) GetDirectories(_ context.Context, req generators.RepoRequest) ([]string, error) {
	root, err := l.root(req.RepoURL)
	if err != nil {
		return nil, err
	}
	var out []string
	err = fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != "." {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// The SCM content the SCM provider and pull request generator examples run against: the
// organization example-org with three repositories and three pull requests on guestbook.
const (
	shaGuestbookMain = "1111111111111111111111111111111111111111"
	shaPR42          = "4242424242424242424242424242424242424242"
	shaPR43          = "4343434343434343434343434343434343434343"
	shaDocsMain      = "2222222222222222222222222222222222222222"
	shaBillingMain   = "3333333333333333333333333333333333333333"
)

func newFakeSCM() *fake.Provider {
	p := fake.New(fake.Options{Name: "examples"})
	chart := []string{"chart/Chart.yaml", "chart/values.yaml", "chart/templates/deployment.yaml"}
	p.AddRepo(fake.Repo{
		Owner: "example-org", Name: "guestbook", ID: 101, DefaultBranch: "main",
		Branches: map[string]string{"main": shaGuestbookMain, "feature/new-ui": shaPR42, "fix/typo": shaPR43},
		Files:    map[string][]string{shaGuestbookMain: chart, shaPR42: chart, shaPR43: chart},
		Topics:   []string{"patrol-cd"},
		PullRequests: []scm.PullRequest{
			// Selected by the examples: open, labelled preview, targets main.
			{Number: 42, Title: "New UI", Branch: "feature/new-ui", TargetBranch: "main", HeadSHA: shaPR42, Author: "octocat", Labels: []string{"preview"}, State: "open"},
			// Not labelled preview.
			{Number: 43, Title: "Fix typo", Branch: "fix/typo", TargetBranch: "main", HeadSHA: shaPR43, Author: "octocat", State: "open"},
			// Closed: its preview environment is cleaned up.
			{Number: 41, Title: "Old experiment", Branch: "experiment", TargetBranch: "main", HeadSHA: shaPR43, Author: "octocat", Labels: []string{"preview"}, State: "closed"},
		},
	})
	// Name matches, but no chart/Chart.yaml (pathsExist filter).
	p.AddRepo(fake.Repo{Owner: "example-org", Name: "guestbook-docs", ID: 102, DefaultBranch: "main",
		Branches: map[string]string{"main": shaDocsMain}, Files: map[string][]string{shaDocsMain: {"README.md"}}})
	// Has a chart, but the name does not match (repositoryMatch filter).
	p.AddRepo(fake.Repo{Owner: "example-org", Name: "billing", ID: 103, DefaultBranch: "main",
		Branches: map[string]string{"main": shaBillingMain}, Files: map[string][]string{shaBillingMain: {"chart/Chart.yaml"}}})
	return p
}

// githubURLs makes the fake provider report github.com clone URLs, as the GitHub provider
// does, so generated Applications point at https://github.com/example-org/<repo>.git.
type githubURLs struct {
	*fake.Provider
}

func (g githubURLs) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	repos, err := g.Provider.ListRepositories(ctx, q)
	for i := range repos {
		for _, s := range []*string{&repos[i].URL, &repos[i].HTTPSURL, &repos[i].WebURL} {
			*s = strings.Replace(*s, "https://fake.example/", "https://github.com/", 1)
		}
		repos[i].SSHURL = strings.Replace(repos[i].SSHURL, "git@fake.example:", "git@github.com:", 1)
	}
	return repos, err
}

// scmRecorder is the generator ProviderFactory: it records the requests (to check that
// credentials were resolved from the referenced Secrets/SCMProviders) and returns the fake.
type scmRecorder struct {
	provider scm.Provider
	mu       sync.Mutex
	requests []generators.ProviderRequest
}

func (s *scmRecorder) factory(_ context.Context, req generators.ProviderRequest) (scm.Provider, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	if req.Type != "github" {
		return nil, fmt.Errorf("the examples only use GitHub, got provider type %q", req.Type)
	}
	return s.provider, nil
}

// pluginService answers the plugin generator (POST <baseUrl>/api/v1/getparams.execute) of
// examples/apps/appset-plugin-generator in place of the tenant catalog service.
type pluginService struct {
	mu    sync.Mutex
	calls int
}

func (p *pluginService) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	reply := func(code int, body any) (*http.Response, error) {
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
	}
	if req.Method != http.MethodPost || req.URL.Host != "tenant-catalog.platform-tools.svc.cluster.local:8080" || req.URL.Path != "/api/v1/getparams.execute" {
		return reply(http.StatusNotFound, map[string]string{"error": "unexpected request " + req.Method + " " + req.URL.String()})
	}
	// The token is the REPLACE_ME placeholder of the example Secret.
	if req.Header.Get("Authorization") != "Bearer REPLACE_ME" {
		return reply(http.StatusUnauthorized, map[string]string{"error": "bad token"})
	}
	var body struct {
		ApplicationSetName string `json:"applicationSetName"`
		Input              struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"input"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Input.Parameters["tier"] != "standard" {
		return reply(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unexpected input %+v (%v)", body, err)})
	}
	return reply(http.StatusOK, map[string]any{"output": map[string]any{"parameters": []map[string]any{
		{"tenant": "alpha", "namespace": "tenant-alpha"},
		{"tenant": "beta", "namespace": "tenant-beta"},
	}}})
}

// expectedApplications lists, per ApplicationSet of the examples, the Applications the
// generators must produce against the example clusters, repositories, SCM content and
// plugin service above.
var expectedApplications = map[string][]string{
	"guestbook-envs":          {"guestbook-list-dev", "guestbook-list-staging"},
	"guestbook-per-cluster":   {"guestbook-prod-us", "guestbook-staging-eu"},
	"guestbook-overlays":      {"guestbook-overlay-dev", "guestbook-overlay-prod"},
	"guestbook-from-config":   {"guestbook-config-dev", "guestbook-config-prod"},
	"team-a-components":       {"team-a-backend-prod-us", "team-a-backend-staging-eu", "team-a-frontend-prod-us", "team-a-frontend-staging-eu"},
	"guestbook-merged":        {"guestbook-merged-prod-us", "guestbook-merged-staging-eu"},
	"guestbook-pull-requests": {"pr-42-guestbook"},
	"org-guestbooks":          {"guestbook-main"},
	"guestbook-rollout":       {"guestbook-rollout-dev-eu", "guestbook-rollout-prod-eu", "guestbook-rollout-prod-us", "guestbook-rollout-staging-eu"},
	"tenants":                 {"tenant-alpha", "tenant-beta"},
	"guestbook-previews":      {"guestbook-pr-42"},
}
