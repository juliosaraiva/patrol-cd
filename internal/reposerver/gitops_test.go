package reposerver

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func TestResolveRevision(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	c1 := r.commit("one", map[string]string{"a": "1"})
	r.git("tag", "v1.0.0")
	r.git("tag", "-a", "annotated", "-m", "annotated tag")
	r.git("tag", "app1/v1.0.1")
	c2 := r.commit("two", map[string]string{"a": "2"})
	r.git("tag", "v1.2.0")
	r.git("tag", "v2.0.0")
	r.git("tag", "app1/v1.1.0-rc.1")
	r.git("push", "--quiet", "--tags", "origin")
	r.git("checkout", "--quiet", "-b", "feature")
	c3 := r.commit("feature", map[string]string{"b": "1"})
	r.git("push", "--quiet", "origin", "refs/heads/feature:refs/pull/7/head")

	s := newTestService(t)
	c := startServer(t, s)
	for _, tc := range []struct {
		rev, prefix, want string
	}{
		{"main", "", c2},
		{"HEAD", "", c2},
		{"", "", c2},
		{"feature", "", c3},
		{"refs/pull/7/head", "", c3},
		{"v1.0.0", "", c1},
		{"annotated", "", c1},
		{">=1.0.0 <2.0.0", "", c2},
		{"*", "", c2},
		{"1.0.*", "app1/", c1},
		{">=v1.0.0-0", "app1/", c2},
		{c1, "", c1},
		{c1[:8], "", c1},
	} {
		resp, err := c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{
			Repo: r.repo(), Revision: tc.rev,
			Source: v1alpha1.ApplicationSource{RepoURL: r.url(), TagPrefix: tc.prefix},
		})
		require.NoError(t, err, "revision %q", tc.rev)
		assert.Equal(t, tc.want, resp.Revision, "revision %q prefix %q", tc.rev, tc.prefix)
		assert.Equal(t, tc.rev+" ("+tc.want+")", resp.AmbiguousRevision)
	}
	_, err := c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{Repo: r.repo(), Revision: "doesnotexist"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to resolve 'doesnotexist' to a commit SHA")
	_, err = c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{Repo: r.repo(), Revision: "9.*"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to resolve")
}

func TestListRefs(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("one", nil)
	r.git("tag", "v2")
	r.git("tag", "v1")
	r.git("branch", "zeta")
	r.git("branch", "alpha")
	r.git("push", "--quiet", "--all", "origin")
	r.git("push", "--quiet", "--tags", "origin")
	s := newTestService(t)
	c := startServer(t, s)
	refs, err := c.ListRefs(bg(), &apiclient.ListRefsRequest{Repo: r.repo()})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "main", "zeta"}, refs.Branches)
	assert.Equal(t, []string{"v1", "v2"}, refs.Tags)

	empty := newRemote(t)
	refs, err = c.ListRefs(bg(), &apiclient.ListRefsRequest{Repo: empty.repo()})
	require.NoError(t, err)
	assert.Empty(t, refs.Branches)
	assert.Empty(t, refs.Tags)
}

func TestListApps(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("apps", map[string]string{
		"helm/guestbook/Chart.yaml":           "apiVersion: v2\nname: guestbook\nversion: 0.1.0\n",
		"kustomize/base/kustomization.yaml":   "resources: []\n",
		"both/Chart.yaml":                     "apiVersion: v2\nname: both\nversion: 0.1.0\n",
		"both/kustomization.yaml":             "resources: []\n",
		"plain/cm.yaml":                       configMap("cm", nil),
		"deep/nested/app/kustomization.yml":   "resources: []\n",
		"deep/nested/app/x/Chart.yaml":        "apiVersion: v2\nname: x\nversion: 0.1.0\n",
		"deep/nested/app/x/templates/cm.yaml": configMap("x", nil),
	})
	s := newTestService(t)
	c := startServer(t, s)
	list, err := c.ListApps(bg(), &apiclient.ListAppsRequest{Repo: r.repo(), Revision: "main"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"helm/guestbook":    "Helm",
		"kustomize/base":    "Kustomize",
		"both":              "Kustomize",
		"deep/nested/app":   "Kustomize",
		"deep/nested/app/x": "Helm",
	}, list.Apps)

	// Disabled tools are not listed.
	list, err = c.ListApps(bg(), &apiclient.ListAppsRequest{Repo: r.repo(), Revision: "main", EnabledSourceTypes: map[string]bool{"Helm": false}})
	require.NoError(t, err)
	assert.NotContains(t, list.Apps, "helm/guestbook")
	assert.Contains(t, list.Apps, "kustomize/base")
}

func TestGetGitFiles(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	outside := filepath.Join(t.TempDir(), "secret.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"secret":true}`), 0o600))
	r.commit("files", map[string]string{
		"apps/a/config.json":          `{"name":"a"}`,
		"apps/b/config.json":          `{"name":"b"}`,
		"apps/b/deep/x/config.json":   `{"name":"x"}`,
		"apps/c/other.yaml":           "x: 1",
		"apps/link/config.json":       "->" + outside,
		"apps/inside/config.json":     "->../a/config.json",
		"cluster-config/c1/meta.yaml": "name: c1",
	})
	s := newTestService(t)
	c := startServer(t, s)
	get := func(pattern string, newGlob bool) map[string][]byte {
		t.Helper()
		resp, err := c.GetGitFiles(bg(), &apiclient.GitFilesRequest{Repo: r.repo(), Revision: "main", Path: pattern, NewGitFileGlobbingEnabled: newGlob})
		require.NoError(t, err)
		return resp.Map
	}
	keys := func(m map[string][]byte) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	// Legacy mode: git pathspec ("*" crosses "/"); escaping symlinks are never read.
	legacy := get("apps/*/config.json", false)
	assert.Equal(t, []string{"apps/a/config.json", "apps/b/config.json", "apps/b/deep/x/config.json", "apps/inside/config.json"}, keys(legacy))
	assert.JSONEq(t, `{"name":"a"}`, string(legacy["apps/inside/config.json"]))
	// New globbing: "*" stays within a segment, "**" crosses; links leaving the
	// repository are ignored.
	assert.Equal(t, []string{"apps/a/config.json", "apps/b/config.json", "apps/inside/config.json"}, keys(get("apps/*/config.json", true)))
	assert.Equal(t, []string{"apps/a/config.json", "apps/b/config.json", "apps/b/deep/x/config.json", "apps/inside/config.json"}, keys(get("apps/**/config.json", true)))
	assert.Equal(t, []string{"cluster-config/c1/meta.yaml"}, keys(get("cluster-config/**/*.yaml", true)))
	// Cached on the second call.
	before := counter(s.metrics.cacheRequests, "gitfiles", "hit")
	get("apps/*/config.json", true)
	assert.Equal(t, before+1, counter(s.metrics.cacheRequests, "gitfiles", "hit"))

	_, err := c.GetGitFiles(bg(), &apiclient.GitFilesRequest{Revision: "main"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must pass a valid repo")
	assert.True(t, apiclient.IsCode(err, apiclient.CodeInvalidArgument))
}

func TestGetGitDirectories(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("dirs", map[string]string{
		"app/foo/bar/x":      "1",
		"app/bar/.hidden/y":  "1",
		"somedir/z":          "1",
		".github/workflow/w": "1",
	})
	s := newTestService(t)
	resp, err := s.GetGitDirectories(bg(), &apiclient.GitDirectoriesRequest{Repo: r.repo(), Revision: "HEAD"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"app", "app/bar", "app/foo", "app/foo/bar", "somedir"}, resp.Paths)

	hidden := newTestService(t, func(c *Config) { c.IncludeHiddenDirectories = true })
	resp, err = hidden.GetGitDirectories(bg(), &apiclient.GitDirectoriesRequest{Repo: r.repo(), Revision: "HEAD"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"app", "app/bar", "app/bar/.hidden", "app/foo", "app/foo/bar", "somedir", ".github", ".github/workflow"}, resp.Paths)
}

func TestGetRevisionMetadata(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	msg := "Update image\n\nBody text.\n\nArgocd-reference-commit-sha: abcdef1234567\nArgocd-reference-commit-author: Dev <dev@example.com>\nArgocd-reference-commit-subject: Dry change\nArgocd-reference-commit-repourl: https://git.example.com/dry.git\nArgocd-reference-commit-date: 2026-01-02T03:04:05Z\n"
	sha := r.commit(msg, map[string]string{"a": "1"})
	r.git("tag", "v1")
	r.git("push", "--quiet", "--tags", "origin")
	s := newTestService(t)
	c := startServer(t, s)
	md, err := c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: sha})
	require.NoError(t, err)
	assert.Equal(t, "Fixture Author <author@example.com>", md.Author)
	assert.Equal(t, []string{"v1"}, md.Tags)
	assert.Contains(t, md.Message, "Update image")
	assert.WithinDuration(t, time.Now(), md.Date, time.Hour)
	require.Len(t, md.References, 1)
	assert.Equal(t, "abcdef1234567", md.References[0].SHA)
	assert.Equal(t, "Dry change", md.References[0].Subject)
	assert.Equal(t, "https://git.example.com/dry.git", md.References[0].RepoURL)
	assert.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), md.References[0].Date.UTC())

	// Truncated SHA (expanded locally) and cache hit.
	md2, err := c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: sha[:9]})
	require.NoError(t, err)
	assert.Equal(t, md.Author, md2.Author)
	_, err = c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: sha})
	require.NoError(t, err)
	assert.Equal(t, 1.0, counter(s.metrics.cacheRequests, "revisionmetadata", "hit"))

	_, err = c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: "main"})
	require.Error(t, err)
	assert.Equal(t, "revision main must be resolved", err.Error())
}

func TestTestRepository(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("one", nil)
	s := newTestService(t)
	c := startServer(t, s)
	resp, err := c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: r.repo()})
	require.NoError(t, err)
	assert.True(t, resp.VerifiedRepository)

	_, err = c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: apiclient.Repository{URL: "file:///does/not/exist.git"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error testing repository connectivity")

	_, err = c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: apiclient.Repository{URL: r.url(), Type: "svn"}})
	require.Error(t, err)
	assert.Equal(t, `unsupported repository type "svn"`, err.Error())

	_, err = c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: apiclient.Repository{URL: "https://charts.example.com", Type: v1alpha1.RepositoryTypeHelm, EnableOCI: true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OCI Helm repository URL should include hostname and port only")
}

func TestListPlugins(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t, "psock")
	for _, name := range []string{"b-plugin-v1.sock", "a-plugin.sock"} {
		ln, err := net.Listen("unix", filepath.Join(dir, name))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), nil, 0o600))
	s := newTestService(t, func(c *Config) { c.PluginSocketDir = dir })
	c := startServer(t, s)
	list, err := c.ListPlugins(bg())
	require.NoError(t, err)
	got := []string{}
	for _, p := range list.Items {
		got = append(got, p.Name)
	}
	assert.Equal(t, []string{"a-plugin", "b-plugin-v1"}, got)

	missing := newTestService(t)
	_, err = missing.ListPlugins(bg())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get plugins from dir")
}
