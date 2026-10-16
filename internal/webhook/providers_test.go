package webhook

import (
	"bytes"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// deliverQueued serves one delivery and returns the queued event (nil when none).
func deliverQueued(t *testing.T, h *testHandler, body []byte, hdr map[string]string) (*httpResult, *scm.Event) {
	t.Helper()
	w := h.serve(newRequest(t, "/api/webhook", body, hdr))
	res := &httpResult{code: w.Code, body: w.Body.String()}
	select {
	case qe := <-h.events:
		return res, qe.event
	default:
		return res, nil
	}
}

type httpResult struct {
	code int
	body string
}

// REPO-117..REPO-122: per-provider push normalization through the handler (web URLs,
// revision, default branch, changed files) and Application repoURL equivalence against
// the URLs each provider reports.
func TestProviderPushNormalization(t *testing.T) {
	t.Parallel()
	byName := map[string]providerCase{}
	for _, pc := range providerCases() {
		byName[pc.name] = pc
	}
	tests := []struct {
		provider    string
		webURLs     []string
		touchedHead bool
		files       int
		matching    []string
		notMatching []string
	}{
		{ProviderGitHub, []string{"https://github.com/acme/platform"}, true, 4,
			[]string{"git@github.com:acme/platform.git", "https://github.com/acme/platform.git", "ssh://git@github.com/acme/platform"},
			[]string{"https://github.com/acme/platform-x"}},
		{ProviderGitLab, []string{"https://gitlab.example.com/acme/platform"}, true, 1,
			[]string{"https://gitlab.example.com/acme/platform.git", "git@gitlab.example.com:acme/platform.git"}, nil},
		{ProviderGogs, []string{"https://gogs.example.com/acme/platform"}, true, 1,
			[]string{"git@gogs.example.com:acme/platform.git"}, nil},
		{ProviderGitea, []string{"https://gitea.example.com/acme/platform"}, true, 1,
			[]string{"https://gitea.example.com/acme/platform"}, nil},
		// Without the diffstat callback (disabled in unit tests) every path is changed and
		// the default branch is assumed touched, as upstream.
		{ProviderBitbucketCloud, []string{"https://bitbucket.org/acme/platform"}, true, 0,
			[]string{"git@bitbucket.org:acme/platform.git"}, nil},
		// Both clone links, each matched separately; they keep .git, so an app repoURL
		// without .git does not match (upstream edge case).
		{ProviderBitbucketServer, []string{"https://bitbucket.example.com/scm/acme/platform.git", "ssh://git@bitbucket.example.com:7999/acme/platform.git"}, true, 0,
			[]string{"https://bitbucket.example.com/scm/acme/platform.git", "ssh://git@bitbucket.example.com:7999/acme/platform.git"},
			[]string{"https://bitbucket.example.com/scm/acme/platform"}},
		{ProviderAzureDevOps, []string{"https://dev.azure.com/acme/Platform/_git/platform"}, true, 0,
			[]string{"https://dev.azure.com/acme/Platform/_git/platform"}, []string{"https://dev.azure.com/acme/Platform/_git/other"}},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			pc := byName[tc.provider]
			h := newTestHandler(t, Config{Settings: staticSettings(pc.settings(goodSecret))})
			body := fixture(t, pc.fixture)
			res, ev := deliverQueued(t, h, body, pc.headers(body, goodSecret, "n-1"))
			require.Equal(t, http.StatusOK, res.code, res.body)
			require.NotNil(t, ev)
			assert.Equal(t, scm.EventPush, ev.Type)
			assert.Equal(t, "main", ev.Revision)
			assert.Equal(t, tc.touchedHead, ev.TouchedDefaultBranch)
			assert.Len(t, ev.ChangedFiles, tc.files)
			assert.Equal(t, tc.webURLs, pushWebURLs(ev))
			var regexps []*regexp.Regexp
			for _, u := range pushWebURLs(ev) {
				re, err := WebURLRegex(u)
				require.NoError(t, err)
				regexps = append(regexps, re)
			}
			matches := func(repoURL string) bool {
				for _, re := range regexps {
					if _, ok := matchAppPush(gitApp(testNS, "a", repoURL, "main", "apps/web"), re, ev); ok {
						return true
					}
				}
				return false
			}
			for _, u := range tc.matching {
				assert.True(t, matches(u), u)
			}
			for _, u := range tc.notMatching {
				assert.False(t, matches(u), u)
			}
		})
	}
}

// Provider event variants: GitLab system and tag hooks, GitHub tag pushes, merge requests
// and pings.
func TestProviderEventVariants(t *testing.T) {
	t.Parallel()
	gitlabPush := fixture(t, "gitlab_push.json")
	githubPush := fixture(t, "github_push.json")
	tests := []struct {
		name     string
		body     []byte
		headers  map[string]string
		status   int
		wantType scm.EventType // "" when nothing is queued
		check    func(t *testing.T, ev *scm.Event)
	}{
		{
			name: "GitLab system hook push", body: gitlabPush,
			headers: map[string]string{"X-Gitlab-Event": "System Hook"}, status: http.StatusOK, wantType: scm.EventPush,
			check: func(t *testing.T, ev *scm.Event) { assert.Equal(t, "main", ev.Revision) },
		},
		{
			name: "GitLab tag push", body: bytes.Replace(gitlabPush, []byte(`"refs/heads/main"`), []byte(`"refs/tags/v1.0"`), 1),
			headers: map[string]string{"X-Gitlab-Event": "Tag Push Hook"}, status: http.StatusOK, wantType: scm.EventPush,
			check: func(t *testing.T, ev *scm.Event) {
				assert.Equal(t, "v1.0", ev.Revision)
				assert.False(t, ev.TouchedDefaultBranch)
			},
		},
		{
			name: "GitHub tag push never touches HEAD", body: bytes.Replace(githubPush, []byte(`"refs/heads/main"`), []byte(`"refs/tags/v1.0"`), 1),
			headers: map[string]string{"X-GitHub-Event": "push"}, status: http.StatusOK, wantType: scm.EventPush,
			check: func(t *testing.T, ev *scm.Event) {
				assert.Equal(t, "v1.0", ev.Revision)
				assert.False(t, ev.TouchedDefaultBranch)
			},
		},
		{
			name: "GitLab merge request update", body: fixture(t, "gitlab_merge_request.json"),
			headers: map[string]string{"X-Gitlab-Event": "Merge Request Hook"}, status: http.StatusOK, wantType: scm.EventPullRequest,
			check: func(t *testing.T, ev *scm.Event) {
				pr := newPRInfo(ev)
				require.NotNil(t, pr)
				assert.Equal(t, "15", pr.id)
			},
		},
		{
			name:    "GitLab merge request approval is ignored",
			body:    bytes.Replace(fixture(t, "gitlab_merge_request.json"), []byte(`"action": "update"`), []byte(`"action": "approved"`), 1),
			headers: map[string]string{"X-Gitlab-Event": "Merge Request Hook"}, status: http.StatusOK,
		},
		{
			name: "GitHub pull request opened", body: fixture(t, "github_pull_request.json"),
			headers: map[string]string{"X-GitHub-Event": "pull_request"}, status: http.StatusOK, wantType: scm.EventPullRequest,
		},
		{
			name: "Bitbucket Server ping is acknowledged and ignored", body: []byte(`{"test": true}`),
			headers: map[string]string{"X-Event-Key": "diagnostics:ping", "X-Request-Id": "ping-1"}, status: http.StatusOK,
		},
		{
			name: "Azure DevOps unsupported event", body: []byte(`{"eventType":"build.complete","resource":{}}`),
			headers: map[string]string{"X-Vss-Activityid": "a1"}, status: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{})})
			res, ev := deliverQueued(t, h, tc.body, tc.headers)
			require.Equal(t, tc.status, res.code, res.body)
			if tc.wantType == "" {
				assert.Nil(t, ev)
				return
			}
			require.NotNil(t, ev)
			assert.Equal(t, tc.wantType, ev.Type)
			if tc.check != nil {
				tc.check(t, ev)
			}
		})
	}
}
