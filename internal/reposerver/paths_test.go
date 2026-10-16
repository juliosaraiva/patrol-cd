package reposerver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func TestRefreshPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		annotation, path string
		want             []string
	}{
		{".", "guestbook", []string{"guestbook"}},
		{"/guestbook", "apps/x", []string{"guestbook"}},
		{".;../shared", "apps/guestbook", []string{"apps/guestbook", "apps/shared"}},
		{" /path ; ", "x", []string{"path"}},
		{"", "x", nil},
		{";;", "x", nil},
		{"/shared/*-secret.yaml", "x", []string{"shared/*-secret.yaml"}},
	} {
		assert.Equal(t, tc.want, RefreshPaths(tc.annotation, tc.path), "%q", tc.annotation)
	}
}

func TestAppFilesHaveChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refresh []string
		changed []string
		want    bool
	}{
		{"no refresh paths", nil, []string{"a"}, true},
		{"no changed files", []string{"a"}, nil, true},
		{"exact file", []string{"app/a.yaml"}, []string{"app/a.yaml"}, true},
		{"below dir", []string{"app"}, []string{"app/sub/a.yaml"}, true},
		{"sibling prefix", []string{"app"}, []string{"application/a.yaml"}, false},
		{"unrelated", []string{"app"}, []string{"other/a.yaml", "README.md"}, false},
		{"glob", []string{"shared/*-secret.yaml"}, []string{"shared/db-secret.yaml"}, true},
		{"glob no cross", []string{"shared/*.yaml"}, []string{"shared/x/db.yaml"}, false},
		{"root", []string{"."}, []string{"anything"}, true},
		{"leading slash", []string{"/app"}, []string{"app/x"}, true},
	} {
		assert.Equal(t, tc.want, AppFilesHaveChanged(tc.refresh, tc.changed), tc.name)
	}
}

func TestUpdateRevisionForPaths(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	base := r.commit("base", map[string]string{
		"app/cm.yaml":    configMap("cm", map[string]string{"v": "1"}),
		"other/cm.yaml":  configMap("other", nil),
		"shared/x.yaml":  "x: 1",
		"README.md":      "readme",
		"app2/cm2.yaml":  configMap("cm2", nil),
		"app2/more.yaml": configMap("more", nil),
	})
	s := newTestService(t)
	c := startServer(t, s)
	src := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "app", TargetRevision: "main"}
	gen := func(rev string) *apiclient.ManifestResponse {
		t.Helper()
		resp, err := c.GenerateManifest(bg(), &apiclient.ManifestRequest{
			Repo: r.repo(), Revision: rev, AppName: "app", Namespace: "ns", Source: src, NoRevisionCache: true,
			APIVersions: []string{"v1", "apps/v1"}, KubeVersion: "1.33.0",
		})
		require.NoError(t, err)
		return resp
	}
	update := func(synced, rev string, paths ...string) *apiclient.UpdateRevisionForPathsResponse {
		t.Helper()
		resp, err := c.UpdateRevisionForPaths(bg(), &apiclient.UpdateRevisionForPathsRequest{
			Repo: r.repo(), AppName: "app", Namespace: "ns", Source: src, SyncedRevision: synced, Revision: rev,
			Paths: paths, NoRevisionCache: true, KubeVersion: "1.33.0",
		})
		require.NoError(t, err)
		return resp
	}
	first := gen(base)
	require.Equal(t, base, first.Revision)

	// An unrelated change: no changes, cached manifests moved to the new revision.
	unrelated := r.commit("docs", map[string]string{"README.md": "readme v2", "other/cm.yaml": configMap("other", map[string]string{"a": "b"})})
	resp := update(base, "main", "app")
	assert.False(t, resp.Changes)
	assert.Equal(t, unrelated, resp.Revision)
	hits := counter(s.metrics.cacheRequests, "manifest", "hit")
	renders := renderCount(s)
	moved := gen("main")
	assert.Equal(t, unrelated, moved.Revision)
	assert.Equal(t, first.Manifests, moved.Manifests)
	assert.Equal(t, hits+1, counter(s.metrics.cacheRequests, "manifest", "hit"), "next generation is a cache hit")
	assert.Equal(t, renders, renderCount(s))

	// A change below a refresh path is a change.
	changed := r.commit("app change", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "2"})})
	resp = update(unrelated, "main", "app")
	assert.True(t, resp.Changes)
	assert.Equal(t, changed, resp.Revision)

	// Glob and root-relative refresh paths.
	r.commit("shared", map[string]string{"shared/x.yaml": "x: 2"})
	assert.True(t, update(changed, "main", "app", "shared/*.yaml").Changes)
	assert.False(t, update(changed, "main", "app").Changes)

	// Same revision: nothing to compare.
	resp = update("main", "main", "app")
	assert.False(t, resp.Changes)

	// No paths: always refresh.
	resp, err := c.UpdateRevisionForPaths(bg(), &apiclient.UpdateRevisionForPathsRequest{Repo: r.repo(), Source: src, SyncedRevision: base, Revision: "main"})
	require.NoError(t, err)
	assert.True(t, resp.Changes)

	// Untyped Helm chart source: no git resolution at all.
	resp, err = c.UpdateRevisionForPaths(bg(), &apiclient.UpdateRevisionForPathsRequest{
		Repo:   apiclient.Repository{URL: "https://charts.invalid.example"},
		Source: v1alpha1.ApplicationSource{RepoURL: "https://charts.invalid.example", Chart: "x", TargetRevision: "1.0.0"},
		Paths:  []string{"x"}, SyncedRevision: "0.9.0", Revision: "1.0.0",
	})
	require.NoError(t, err)
	assert.False(t, resp.Changes)
	assert.Empty(t, resp.Revision)
}

func TestUpdateRevisionForPathsSiblingCacheMiss(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	base := r.commit("base", map[string]string{"a/cm.yaml": configMap("a", nil), "b/cm.yaml": configMap("b", nil)})
	s := newTestService(t)
	src := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "a"}
	r.commit("b changes", map[string]string{"b/cm.yaml": configMap("b", map[string]string{"x": "y"})})
	// Nothing cached for the synced revision: still no changes (upstream #29430).
	resp, err := s.UpdateRevisionForPaths(bg(), &apiclient.UpdateRevisionForPathsRequest{
		Repo: r.repo(), AppName: "a", Source: src, SyncedRevision: base, Revision: "main", Paths: []string{"a"},
	})
	require.NoError(t, err)
	assert.False(t, resp.Changes)
}

func TestUpdateRevisionForPathsRefSources(t *testing.T) {
	t.Parallel()
	app := newRemote(t)
	appSHA := app.commit("app", map[string]string{"chart/Chart.yaml": "apiVersion: v2\nname: c\nversion: 0.1.0\n"})
	values := newRemote(t)
	v1 := values.commit("v1", map[string]string{"env/values.yaml": "a: 1", "docs/readme": "x"})
	v2 := values.commit("v2 docs", map[string]string{"docs/readme": "y"})
	s := newTestService(t)
	src := v1alpha1.ApplicationSource{RepoURL: app.url(), Path: "chart", TargetRevision: appSHA,
		Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/env/values.yaml"}}}
	req := &apiclient.UpdateRevisionForPathsRequest{
		Repo: app.repo(), AppName: "app", Source: src, HasMultipleSources: true,
		SyncedRevision: appSHA, Revision: appSHA, Paths: []string{"env"},
		RefSources:       map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), TargetRevision: v2}},
		SyncedRefSources: map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), TargetRevision: v1}},
	}
	resp, err := s.UpdateRevisionForPaths(bg(), req)
	require.NoError(t, err)
	assert.False(t, resp.Changes)

	v3 := values.commit("v3 values", map[string]string{"env/values.yaml": "a: 2"})
	req.RefSources = map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), TargetRevision: v3}}
	resp, err = s.UpdateRevisionForPaths(bg(), req)
	require.NoError(t, err)
	assert.True(t, resp.Changes)

	req.SyncedRefSources = nil
	resp, err = s.UpdateRevisionForPaths(bg(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `source referenced "$values", but no source has a 'ref' field defined`)
	assert.True(t, resp.Changes)
}
