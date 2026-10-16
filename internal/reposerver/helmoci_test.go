package reposerver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// chartRepo serves an HTTP Helm repository with chart "demo" in several versions.
type chartRepo struct {
	url       string
	downloads atomic.Int32
	// failCharts makes chart downloads (not the index) answer 503.
	failCharts atomic.Bool
}

func demoChart(t testing.TB, version, replicas string) []byte {
	return tarGz(t, [][2]string{
		{"demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: " + version + "\ndescription: Demo chart\nhome: https://example.com/demo\nmaintainers:\n- name: Jane\n  email: jane@example.com\n- name: Bob\n"},
		{"demo/values.yaml", "replicas: " + replicas + "\nimage:\n  tag: v1\n"},
		{"demo/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\ndata:\n  replicas: {{ .Values.replicas | quote }}\n  version: {{ .Chart.Version | quote }}\n"},
	})
}

func newChartRepo(t testing.TB) *chartRepo {
	t.Helper()
	cr := &chartRepo{}
	charts := map[string][]byte{
		"/demo-1.0.0.tgz": demoChart(t, "1.0.0", "1"),
		"/demo-1.2.0.tgz": demoChart(t, "1.2.0", "2"),
		"/demo-2.0.0.tgz": demoChart(t, "2.0.0", "3"),
	}
	index := `apiVersion: v1
entries:
  demo:
  - name: demo
    version: 2.0.0
    urls: [demo-2.0.0.tgz]
  - name: demo
    version: 1.2.0
    urls: [demo-1.2.0.tgz]
  - name: demo
    version: 1.0.0
    urls: [demo-1.0.0.tgz]
  other:
  - name: other
    version: 0.1.0
    urls: [other-0.1.0.tgz]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			_, _ = w.Write([]byte(index))
			return
		}
		if data, ok := charts[r.URL.Path]; ok {
			if cr.failCharts.Load() {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			cr.downloads.Add(1)
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cr.url = srv.URL
	return cr
}

func (cr *chartRepo) repo() apiclient.Repository {
	return apiclient.Repository{URL: cr.url, Type: v1alpha1.RepositoryTypeHelm}
}

func TestHelmRepositoryOperations(t *testing.T) {
	t.Parallel()
	cr := newChartRepo(t)
	s := newTestService(t)
	c := startServer(t, s)
	for rev, want := range map[string]string{"1.*": "1.2.0", ">=2.0.0": "2.0.0", "1.0.0": "1.0.0", "*": "2.0.0"} {
		resp, err := c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{
			Repo: cr.repo(), Revision: rev, Source: v1alpha1.ApplicationSource{RepoURL: cr.url, Chart: "demo"},
		})
		require.NoError(t, err, rev)
		assert.Equal(t, want, resp.Revision, rev)
	}
	_, err := c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{
		Repo: cr.repo(), Revision: "5.*", Source: v1alpha1.ApplicationSource{RepoURL: cr.url, Chart: "demo"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid revision")

	charts, err := c.GetHelmCharts(bg(), &apiclient.HelmChartsRequest{Repo: cr.repo()})
	require.NoError(t, err)
	assert.Equal(t, []apiclient.HelmChart{
		{Name: "demo", Versions: []string{"2.0.0", "1.2.0", "1.0.0"}},
		{Name: "other", Versions: []string{"0.1.0"}},
	}, charts.Items)

	_, err = c.GetHelmCharts(bg(), &apiclient.HelmChartsRequest{Repo: apiclient.Repository{URL: "registry.example.com/charts", EnableOCI: true}})
	require.Error(t, err)

	tr, err := c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: cr.repo()})
	require.NoError(t, err)
	assert.True(t, tr.VerifiedRepository)
	_, err = c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: apiclient.Repository{URL: cr.url + "/missing", Type: v1alpha1.RepositoryTypeHelm}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error testing repository connectivity")
}

func TestHelmChartSourceGenerateAndDetails(t *testing.T) {
	t.Parallel()
	requireHelm(t)
	cr := newChartRepo(t)
	s := newTestService(t)
	c := startServer(t, s)
	req := &apiclient.ManifestRequest{
		Repo: cr.repo(), AppName: "release", Namespace: "ns",
		Source: v1alpha1.ApplicationSource{RepoURL: cr.url, Chart: "demo", TargetRevision: "1.*",
			Helm: &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "replicas", Value: "7"}}}},
	}
	resp, err := c.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, "1.2.0", resp.Revision)
	assert.Equal(t, "Helm", resp.SourceType)
	cm := findObj(t, objects(t, resp), "ConfigMap", "release")
	assert.Equal(t, map[string]any{"replicas": "7", "version": "1.2.0"}, cm.Object["data"])
	assert.NotEmpty(t, resp.Commands)

	// Cached: no new download, no render.
	downloads := cr.downloads.Load()
	_, err = c.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, downloads, cr.downloads.Load())

	// Hard refresh re-downloads the chart.
	hard := *req
	hard.NoCache = true
	_, err = c.GenerateManifest(bg(), &hard)
	require.NoError(t, err)
	assert.Greater(t, cr.downloads.Load(), downloads)

	details, err := c.GetRevisionChartDetails(bg(), &apiclient.ChartDetailsRequest{Repo: cr.repo(), Name: "demo", Revision: "2.0.0"})
	require.NoError(t, err)
	assert.Equal(t, &apiclient.ChartDetails{Description: "Demo chart", Home: "https://example.com/demo", Maintainers: []string{"Jane <jane@example.com>", "Bob"}}, details)

	ad, err := c.GetAppDetails(bg(), &apiclient.AppDetailsQuery{Repo: cr.repo(), Source: v1alpha1.ApplicationSource{RepoURL: cr.url, Chart: "demo", TargetRevision: "1.0.0"}})
	require.NoError(t, err)
	assert.Equal(t, "Helm", ad.Type)
	require.NotNil(t, ad.Helm)
	assert.Equal(t, []string{"values.yaml"}, ad.Helm.ValueFiles)
	assert.Contains(t, ad.Helm.Values, "replicas: 1")
	assert.Contains(t, ad.Helm.Parameters, v1alpha1.HelmParameter{Name: "replicas", Value: "1"})
	assert.Contains(t, ad.Helm.Parameters, v1alpha1.HelmParameter{Name: "image.tag", Value: "v1"})
}

func TestGetAppDetailsGit(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("apps", map[string]string{
		"chart/Chart.yaml":           "apiVersion: v2\nname: c\nversion: 0.1.0\n",
		"chart/values.yaml":          "replicas: 1\nlist:\n- name: a\n",
		"chart/values-prod.yaml":     "replicas: 3\n",
		"chart/env/my-values.YML":    "x: 1\n",
		"chart/templates/cm.yaml":    "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n",
		"kust/kustomization.yaml":    "resources:\n- deploy.yaml\n",
		"kust/deploy.yaml":           deployment("web", "nginx:1.25"),
		"plain/cm.yaml":              configMap("cm", nil),
		"cronjob/kustomization.yaml": "resources:\n- cron.yaml\nimages:\n- name: worker\n  newTag: v9\n",
		"cronjob/cron.yaml":          "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: cron\nspec:\n  schedule: \"* * * * *\"\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n          - name: w\n            image: worker\n          restartPolicy: Never\n",
	})
	s := newTestService(t)
	c := startServer(t, s)
	q := func(path string, src *v1alpha1.ApplicationSource) *apiclient.AppDetailsQuery {
		s := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: path, TargetRevision: "main"}
		if src != nil {
			s = *src
		}
		return &apiclient.AppDetailsQuery{Repo: r.repo(), Source: s, AppName: "app"}
	}

	d, err := c.GetAppDetails(bg(), q("plain", nil))
	require.NoError(t, err)
	assert.Equal(t, "Directory", d.Type)

	if helmBin() != "" {
		src := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "chart", TargetRevision: "main",
			Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"values-prod.yaml", "missing.yaml"}}}
		d, err = c.GetAppDetails(bg(), q("", &src))
		require.NoError(t, err)
		assert.Equal(t, "Helm", d.Type)
		assert.ElementsMatch(t, []string{"values.yaml", "values-prod.yaml", "env/my-values.YML"}, d.Helm.ValueFiles)
		assert.Contains(t, d.Helm.Parameters, v1alpha1.HelmParameter{Name: "replicas", Value: "3"})
		assert.Contains(t, d.Helm.Parameters, v1alpha1.HelmParameter{Name: "list[0].name", Value: "a"})
		// Cached.
		_, err = c.GetAppDetails(bg(), q("", &src))
		require.NoError(t, err)
		assert.Equal(t, 1.0, counter(s.metrics.cacheRequests, "appdetails", "hit"))
	}
	if kustomizeBin(t) != "" {
		d, err = c.GetAppDetails(bg(), q("kust", nil))
		require.NoError(t, err)
		assert.Equal(t, "Kustomize", d.Type)
		assert.Equal(t, []string{"busybox:1.36", "nginx:1.25"}, d.Kustomize.Images)
		d, err = c.GetAppDetails(bg(), q("cronjob", nil))
		require.NoError(t, err)
		assert.Equal(t, []string{"worker:v9"}, d.Kustomize.Images)
	}
	// Disabled tools fall back to Directory for explicit blocks.
	src := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "kust", TargetRevision: "main", Kustomize: &v1alpha1.ApplicationSourceKustomize{NamePrefix: "x-"}}
	query := q("", &src)
	query.EnabledSourceTypes = map[string]bool{"Kustomize": false}
	d, err = c.GetAppDetails(bg(), query)
	require.NoError(t, err)
	assert.Equal(t, "Directory", d.Type)

	_, err = c.GetAppDetails(bg(), q("nope", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope: app path does not exist")
}

func TestOCISource(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	layer := tarGz(t, [][2]string{{"app/cm.yaml", configMap("from-oci", nil)}, {"other/x.yaml", configMap("x", nil)}})
	annotations := map[string]string{
		"org.opencontainers.image.created":     "2026-01-01T00:00:00Z",
		"org.opencontainers.image.authors":     "Team",
		"org.opencontainers.image.version":     "1.0.0",
		"org.opencontainers.image.description": "manifests",
		"org.opencontainers.image.source":      "https://git.example.com/app",
	}
	d1 := reg.PushArtifact("team/app", "1.0.0", "application/vnd.oci.image.config.v1+json", []byte("{}"),
		[]ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: layer}}, annotations)
	d2 := reg.PushArtifact("team/app", "1.1.0", "application/vnd.oci.image.config.v1+json", []byte("{}"),
		[]ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: layer}}, map[string]string{"org.opencontainers.image.version": "1.1.0"})
	url := "oci://" + reg.Host() + "/team/app"
	repo := apiclient.Repository{URL: url, Type: v1alpha1.RepositoryTypeOCI, InsecureOCIForceHTTP: true}
	s := newTestService(t)
	c := startServer(t, s)

	rr, err := c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{Repo: repo, Revision: "1.0.0", Source: v1alpha1.ApplicationSource{RepoURL: url}})
	require.NoError(t, err)
	assert.Equal(t, d1.Digest.String(), rr.Revision)
	rr, err = c.ResolveRevision(bg(), &apiclient.ResolveRevisionRequest{Repo: repo, Revision: "1.*", Source: v1alpha1.ApplicationSource{RepoURL: url}})
	require.NoError(t, err)
	assert.Equal(t, d2.Digest.String(), rr.Revision)

	resp, err := c.GenerateManifest(bg(), &apiclient.ManifestRequest{
		Repo: repo, AppName: "app", Source: v1alpha1.ApplicationSource{RepoURL: url, Path: "app", TargetRevision: "1.0.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, d1.Digest.String(), resp.Revision)
	assert.Equal(t, []string{"ConfigMap/from-oci"}, names(objects(t, resp)))

	tags, err := c.ListOCITags(bg(), &apiclient.ListRefsRequest{Repo: repo})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"1.0.0", "1.1.0"}, tags.Tags)
	before := reg.Requests("tags")
	_, err = c.ListOCITags(bg(), &apiclient.ListRefsRequest{Repo: repo})
	require.NoError(t, err)
	assert.Equal(t, before, reg.Requests("tags"), "tag list cached")

	md, err := c.GetOCIMetadata(bg(), &apiclient.ChartDetailsRequest{Repo: repo, Revision: d1.Digest.String()})
	require.NoError(t, err)
	assert.Equal(t, "Team", md.Authors)
	assert.Equal(t, "1.0.0", md.Version)
	assert.Equal(t, "manifests", md.Description)
	assert.Equal(t, "https://git.example.com/app", md.SourceURL)
	assert.Equal(t, "2026-01-01T00:00:00Z", md.CreatedAt)

	tr, err := c.TestRepository(bg(), &apiclient.TestRepositoryRequest{Repo: repo})
	require.NoError(t, err)
	assert.True(t, tr.VerifiedRepository)

	_, err = c.GenerateManifest(bg(), &apiclient.ManifestRequest{
		Repo: repo, AppName: "app", Source: v1alpha1.ApplicationSource{RepoURL: url, Path: "app", TargetRevision: "9.9.9"},
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), `failed to resolve revision "9.9.9"`), "%v", err)
}

// fakeKustomize installs a kustomize stand-in that records its build environment in
// the returned file and prints one Deployment.
func fakeKustomize(t *testing.T) (bin, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kustomize")
	envDump = filepath.Join(dir, "env.txt")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ]; then echo "v5.7.1"; exit 0; fi
/usr/bin/env > %q
cat <<'YAML'
%sYAML
`, envDump, deployment("web", "nginx:1.25"))
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, envDump
}

// TestGetAppDetailsRemoteBaseCredentials checks that only git sources offer their
// repository credentials to Kustomize remote-base fetches: registry and Helm
// repository credentials never reach git hosts named by artifact content.
func TestGetAppDetailsRemoteBaseCredentials(t *testing.T) {
	t.Parallel()
	files := map[string]string{"kustomization.yaml": "resources:\n- deploy.yaml\n", "deploy.yaml": deployment("web", "nginx:1.25")}

	t.Run("oci", func(t *testing.T) {
		t.Parallel()
		bin, envDump := fakeKustomize(t)
		reg := ocitest.New(t, ocitest.Options{})
		layer := tarGz(t, [][2]string{{"app/kustomization.yaml", files["kustomization.yaml"]}, {"app/deploy.yaml", files["deploy.yaml"]}})
		reg.PushArtifact("team/kust", "1.0.0", "application/vnd.oci.image.config.v1+json", []byte("{}"),
			[]ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: layer}}, nil)
		url := "oci://" + reg.Host() + "/team/kust"
		repo := apiclient.Repository{URL: url, Type: v1alpha1.RepositoryTypeOCI, InsecureOCIForceHTTP: true, Username: "robot", Password: "registry-secret"}
		s := newTestService(t, func(c *Config) { c.KustomizePath = bin })
		d, err := startServer(t, s).GetAppDetails(bg(), &apiclient.AppDetailsQuery{
			Repo: repo, AppName: "app", Source: v1alpha1.ApplicationSource{RepoURL: url, Path: "app", TargetRevision: "1.0.0"},
		})
		require.NoError(t, err)
		assert.Equal(t, "Kustomize", d.Type)
		assert.Equal(t, []string{"busybox:1.36", "nginx:1.25"}, d.Kustomize.Images)
		env, err := os.ReadFile(envDump)
		require.NoError(t, err)
		assert.NotContains(t, string(env), "GIT_ASKPASS=")
		assert.NotContains(t, string(env), "registry-secret")
	})

	t.Run("git", func(t *testing.T) {
		t.Parallel()
		bin, envDump := fakeKustomize(t)
		r := newRemote(t)
		r.commit("kust", map[string]string{"app/kustomization.yaml": files["kustomization.yaml"], "app/deploy.yaml": files["deploy.yaml"]})
		repo := r.repo()
		repo.Username, repo.Password = "git-user", "git-secret"
		s := newTestService(t, func(c *Config) { c.KustomizePath = bin })
		d, err := startServer(t, s).GetAppDetails(bg(), &apiclient.AppDetailsQuery{
			Repo: repo, AppName: "app", Source: v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "app", TargetRevision: "main"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"busybox:1.36", "nginx:1.25"}, d.Kustomize.Images)
		env, err := os.ReadFile(envDump)
		require.NoError(t, err)
		assert.Contains(t, string(env), "GIT_ASKPASS=", "git sources authenticate remote bases with their own credentials (upstream)")
		assert.NotContains(t, string(env), "git-secret", "credentials travel in a 0600 file, not the environment")
	})
}

// flakyProxy forwards to target and answers 503 to blob requests while fail is set.
func flakyProxy(t *testing.T, target string) (host string, fail *atomic.Bool) {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	fail = &atomic.Bool{}
	proxy := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && strings.Contains(r.URL.Path, "/blobs/") {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), fail
}
