package webhook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// REPO-124: repository URL equivalence (upstream Test_GetWebURLRegex cases).
func TestWebURLRegex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		match  bool
		webURL string
		repo   string
		name   string
	}{
		{false, "https://example.com/org/a..d", "https://example.com/org/abcd", "dots in repo names are not wildcards"},
		{false, "https://an.example.com/org/repo", "https://an-example.com/org/repo", "dots in host names are not wildcards"},
		{true, "https://example.com/org/repo", "https://example.com/org/repo", "exact"},
		{false, "https://example.com/org/repo", "https://example.com/org/repo-2", "prefix only"},
		{true, "https://example.com/org/repo", "https://example.com/org/repo.git", ".git suffix"},
		{true, "https://example.com/org/repo", "git@example.com:org/repo", "scp form"},
		{true, "https://example.com/org/repo", "user@example.com:org/repo", "scp form with other user"},
		{true, "https://example.com/org/repo", "ssh://git@example.com/org/repo", "ssh scheme"},
		{true, "https://example.com/org/repo", "ssh://git@example.com:22/org/repo", "ssh with port"},
		{true, "https://example.com:443/org/repo", "ssh://git@example.com:22/org/repo", "different ports"},
		{true, "https://example.com:443/org/repo", "ssh://git@ssh.example.com:443/org/repo", "ssh. subdomain"},
		{true, "https://example.com:443/org/repo", "ssh://git@altssh.example.com:443/org/repo", "altssh. subdomain"},
		{false, "https://example.com:443/org/repo", "ssh://git@unknown.example.com:443/org/repo", "unknown subdomain"},
		{true, "https://example.com/org/repo", "ssh://user-name@example.com/org/repo", "user with hyphen"},
		{false, "https://example.com/org/repo", "ssh://-user-name@example.com/org/repo", "user starting with hyphen"},
		{true, "https://example.com:443/org/repo", "GIT@EXAMPLE.COM:22:ORG/REPO", "case-insensitive"},
		{true, "https://example.com/org/repo%20", "https://example.com/org/repo%20", "escapes preserved"},
		{true, "https://user@example.com/org/repo", "http://example.com/org/repo", "https+user vs http"},
		{true, "https://user@example.com/org/repo", "https://example.com/org/repo", "https+user vs https"},
		{true, "http://example.com/org/repo", "https://user@example.com/org/repo", "http vs https+user"},
		{true, "https://example.com/org/repo", "https://user@example.com/org/repo", "https vs https+user"},
		{true, "https://user@example.com/org/repo", "ssh://example.com/org/repo", "https+user vs ssh"},
		{true, "https://ghe.example.com/acme/platform", "git@ghe.example.com:acme/platform.git", "GitHub Enterprise scp"},
		{false, "https://github.com/acme/platform", "https://github.com/acme/platform/sub", "deeper path"},
		{false, "", "", "empty URL does not panic"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			re, err := WebURLRegex(tc.webURL)
			require.NoError(t, err)
			assert.Equal(t, tc.match, re.MatchString(tc.repo), re.String())
		})
	}
	t.Run("unparsable URL", func(t *testing.T) {
		t.Parallel()
		_, err := WebURLRegex("%%")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse URL")
	})
	t.Run("oversized URL", func(t *testing.T) {
		t.Parallel()
		_, err := WebURLRegex("https://example.com/" + strings.Repeat("a", maxURLLength))
		require.Error(t, err)
	})
}

// Property: every common spelling of a repository matches the regex built from its web
// URL, and a different repository name never does.
func TestWebURLRegexProperty(t *testing.T) {
	t.Parallel()
	label := rapid.StringMatching(`[a-z][a-z0-9-]{0,10}[a-z0-9]`)
	rapid.Check(t, func(t *rapid.T) {
		host := label.Draw(t, "host") + "." + label.Draw(t, "tld")
		org := label.Draw(t, "org")
		repo := label.Draw(t, "repo")
		re, err := WebURLRegex(fmt.Sprintf("https://%s/%s/%s", host, org, repo))
		if err != nil {
			t.Fatalf("regex: %v", err)
		}
		for _, form := range []string{
			fmt.Sprintf("https://%s/%s/%s", host, org, repo),
			fmt.Sprintf("https://%s/%s/%s.git", host, org, repo),
			fmt.Sprintf("git@%s:%s/%s.git", host, org, repo),
			fmt.Sprintf("ssh://git@%s:22/%s/%s", host, org, repo),
			strings.ToUpper(fmt.Sprintf("https://%s/%s/%s", host, org, repo)),
		} {
			if !re.MatchString(form) {
				t.Fatalf("%q does not match %s", form, re)
			}
		}
		if re.MatchString(fmt.Sprintf("https://%s/%s/%s-x", host, org, repo)) {
			t.Fatalf("other repository matched")
		}
	})
}

func TestAPIURLRegex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		match       bool
		apiURL, url string
	}{
		{false, "https://an.example.com/", "https://an-example.com/"},
		{true, "https://example.com/", "https://example.com/"},
		{false, "https://example.com/", "ssh://example.com/"},
		{true, "https://user@example.com/", "http://example.com/"},
		{true, "https://user@example.com/", "https://example.com/"},
		{true, "http://example.com/", "https://user@example.com/"},
		{true, "https://example.com/", "https://user@example.com/"},
	}
	for _, tc := range tests {
		re, err := APIURLRegex(tc.apiURL)
		require.NoError(t, err)
		assert.Equal(t, tc.match, re.MatchString(tc.url), "%s vs %s", tc.apiURL, tc.url)
	}
	_, err := APIURLRegex("%%")
	require.Error(t, err)
}

// ASET-106: generator API base vs payload API URL (host comparison; Patrol also accepts
// API bases with a path such as GitHub Enterprise /api/v3/).
func TestAPIHostMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		gen, event string
		match      bool
	}{
		{"https://api.github.com/", "https://api.github.com/repos/acme/platform", true},
		{"https://api.github.com", "https://api.github.com/repos/acme/platform", true},
		{"api.github.com", "https://api.github.com/repos/acme/platform", true},
		{"https://API.GitHub.com/", "https://api.github.com/repos/acme/platform", true},
		{"https://ghe.example.com/api/v3/", "https://ghe.example.com/api/v3/repos/acme/platform", true},
		{"https://user@api.github.com:443/", "https://api.github.com/repos/a/b", true},
		{"https://api.github.com/", "https://ghe.example.com/api/v3/repos/a/b", false},
		{"ssh://api.github.com/", "https://api.github.com/repos/a/b", false},
		{"", "https://api.github.com/repos/a/b", false},
		{"https://api.github.com/", "", false},
		{"%%", "https://api.github.com/", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.match, apiHostMatches(tc.gen, tc.event), "%q vs %q", tc.gen, tc.event)
	}
}

// REPO-125: upstream TestAppRevisionHasChanged cases.
func TestSourceRevisionHasChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		target      string
		revision    string
		touchedHead bool
		want        bool
	}{
		{"empty target, touched head", "", "master", true, true},
		{"empty target, head untouched", "", "master", false, false},
		{"HEAD target, touched head", "HEAD", "feature", true, true},
		{"dev target, master pushed", "dev", "master", true, false},
		{"dev target, dev pushed", "dev", "dev", false, true},
		{"refs/heads/dev, master pushed", "refs/heads/dev", "master", true, false},
		{"refs/heads/dev, dev pushed", "refs/heads/dev", "dev", false, true},
		{"refs/tags/dev, dev pushed", "refs/tags/dev", "dev", false, true},
		{"env/test, env/test pushed", "env/test", "env/test", false, true},
		{"refs/heads/env/test, env/test pushed", "refs/heads/env/test", "env/test", false, true},
		{"refs/tags/env/test, env/test pushed", "refs/tags/env/test", "env/test", false, true},
		{"three/part/rev, rev pushed", "three/part/rev", "rev", false, false},
		{"semver constraint matches", "1.*", "1.1.0", false, true},
		{"refs/tags semver constraint matches", "refs/tags/1.*", "1.1.0", false, true},
		{"semver constraint does not match", "1.*", "2.0.0", false, false},
		{"semver constraint, non-semver push", "1.*", "dev", false, false},
		{"caret constraint", "^1.2.0", "1.4.0", false, true},
		{"tilde constraint", "~1.2.0", "1.3.0", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, sourceRevisionHasChanged(tc.target, tc.revision, tc.touchedHead))
		})
	}
}

// ASET-105: Git generator revisions (no semver; short or full form).
func TestGeneratorRevisionHasChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		gen, rev string
		head     bool
		want     bool
	}{
		{"", "main", true, true},
		{"HEAD", "main", false, false},
		{"main", "main", false, true},
		{"refs/heads/release/1.0", "release/1.0", false, true},
		{"release/1.0", "release/1.0", false, true},
		{"main", "feature", true, false},
		{"1.*", "1.1.0", false, false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, generatorRevisionHasChanged(tc.gen, tc.rev, tc.head), "%q vs %q", tc.gen, tc.rev)
	}
}

// REPO-126: manifest-generate-paths.
func TestRefreshPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		annotation string
		sourcePath string
		sync       bool
		want       []string
	}{
		{"no annotation", "", "apps/web", false, nil},
		{"dot is the source path", ".", "apps/web", false, []string{"apps/web"}},
		{"relative", "../shared", "apps/web", false, []string{"apps/shared"}},
		{"absolute", "/charts/common", "apps/web", false, []string{"charts/common"}},
		{"several with spaces and empties", " .; /charts ;; ../x ", "apps/web", false, []string{"apps/web", "charts", "apps/x"}},
		{"glob", "*.yaml", "apps/web", false, []string{"apps/web/*.yaml"}},
		{"hydrator sync source ignores annotation", "/charts", "hydrated/web", true, []string{"hydrated/web"}},
		{"root source path", ".", "", false, []string{"."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, refreshPaths(tc.annotation, tc.sourcePath, tc.sync))
		})
	}
}

func TestFilesHaveChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		paths   []string
		changed []string
		want    bool
	}{
		{"no changed files refreshes", []string{"apps/web"}, nil, true},
		{"no refresh paths refreshes", nil, []string{"README.md"}, true},
		{"file under path", []string{"apps/web"}, []string{"apps/web/deployment.yaml"}, true},
		{"file deep under path", []string{"apps/web"}, []string{"apps/web/a/b/c.yaml"}, true},
		{"file outside path", []string{"apps/web"}, []string{"apps/webapp/x.yaml", "README.md"}, false},
		{"equal to path", []string{"apps/web/values.yaml"}, []string{"apps/web/values.yaml"}, true},
		{"glob match", []string{"apps/web/*.yaml"}, []string{"apps/web/a.yaml"}, true},
		{"glob mismatch", []string{"apps/web/*.yaml"}, []string{"apps/web/a.json"}, false},
		{"root path matches all", []string{"."}, []string{"anything/at/all.txt"}, true},
		{"absolute changed file", []string{"apps/web"}, []string{"/apps/web/x.yaml"}, true},
		{"one of several paths", []string{"charts", "apps/web"}, []string{"charts/common/values.yaml"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, filesHaveChanged(tc.paths, tc.changed))
		})
	}
}

// REPO-130 / ASET-100: namespace patterns (glob and /regex/).
func TestNamespaceMatcher(t *testing.T) {
	t.Parallel()
	m, warns := newNamespaceMatcher("patrol", []string{"team-*", "/^env-[0-9]+$/", "/(?!bad)/", " "})
	require.Len(t, warns, 1, "the lookaround expression is reported")
	assert.True(t, m.clusterWide())
	for ns, want := range map[string]bool{
		"patrol": true, "team-a": true, "env-12": true, "env-x": false, "other": false, "team": false,
	} {
		assert.Equal(t, want, m.allowed(ns), ns)
	}
	only, warns := newNamespaceMatcher("patrol", nil)
	assert.Empty(t, warns)
	assert.False(t, only.clusterWide())
	assert.True(t, only.allowed("patrol"))
	assert.False(t, only.allowed("team-a"))
}

func TestNormalizeOCI(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"oci://ghcr.io/acme/charts/web", "ghcr.io/acme/charts/web/", "OCI://GHCR.IO/Acme/Charts/Web"} {
		// The scheme is case-sensitive upstream; only the lower-case form is stripped.
		if strings.HasPrefix(in, "OCI://") {
			assert.NotEqual(t, "ghcr.io/acme/charts/web", normalizeOCI(in))
			continue
		}
		assert.Equal(t, "ghcr.io/acme/charts/web", normalizeOCI(in), in)
	}
}
