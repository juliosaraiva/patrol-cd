package applications_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const secretJSON = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","annotations":{"token":"t0p"}},"data":{"password":"aHVudGVyMg=="}}`

func TestGetManifests(t *testing.T) {
	e := newEnv(t, envOptions{cm: map[string]string{"resource.sensitive.mask.annotations": "token"}})
	e.createApp(e.newApp("m", "proj-a"))
	e.repo.mu.Lock()
	e.repo.manifests = map[string][]string{"guestbook": {secretJSON, configMapJSON("cm")}}
	e.repo.mu.Unlock()

	resp, err := e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m", Revision: "v3", NoCache: true})
	require.NoError(t, err)
	require.Len(t, resp.Manifests, 2)
	assert.NotContains(t, resp.Manifests[0], "aHVudGVyMg==")
	assert.NotContains(t, resp.Manifests[0], "t0p")
	assert.Contains(t, resp.Manifests[0], "++++++++")
	assert.Equal(t, configMapJSON("cm"), resp.Manifests[1])
	assert.Equal(t, "rev-v3", resp.Revision)
	assert.Equal(t, inCluster, resp.Server)
	gen := e.repo.lastGenerate()
	assert.Equal(t, "v3", gen.Revision)
	assert.True(t, gen.NoCache)
	assert.Equal(t, "m", gen.AppName)
	assert.Equal(t, "default", gen.Namespace)
	assert.Equal(t, string(v1alpha1.TrackingMethodAnnotation), gen.TrackingMethod)

	_, err = e.client("bob").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m", Revisions: []string{"x"}, SourcePositions: []int64{1}})
	requireProblem(t, err, 400, "can only be used with a multi-source application")

	// Repo-server failures: generation errors are 400 with the message, outages 503.
	e.repo.mu.Lock()
	e.repo.genErr = &reposerver.Error{Code: reposerver.CodeInternal, Message: "helm template failed: boom"}
	e.repo.mu.Unlock()
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	requireProblem(t, err, 400, "error generating manifests: helm template failed: boom")
	e.repo.mu.Lock()
	e.repo.genErr = &reposerver.Error{Code: reposerver.CodeUnavailable, Message: "dial tcp: refused"}
	e.repo.mu.Unlock()
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	ae := requireProblem(t, err, 503, "")
	assert.NotContains(t, ae.Detail, "dial tcp")
	e.repo.mu.Lock()
	e.repo.genErr = nil
	e.repo.mu.Unlock()

	// An Application applied with kubectl whose repository the project does not permit is
	// not rendered with the project's credentials.
	rogue := e.newApp("rogue", "proj-a")
	rogue.Spec.Source.RepoURL = "https://user:pw@gitlab.com/private/repo"
	e.createApp(rogue)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "rogue"})
	ae = requireProblem(t, err, 403, "application repo https://gitlab.com/private/repo is not permitted in project 'proj-a'")
	assert.NotContains(t, ae.Detail, "pw@")

	// Multi-source: positions override revisions; refs are resolved.
	ms := e.newApp("ms", "proj-a")
	ms.Spec.Source = nil
	ms.Spec.Sources = v1alpha1.ApplicationSources{
		{RepoURL: repoURL, Path: "base", TargetRevision: "main", Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/v.yaml"}}},
		{RepoURL: "https://github.com/org/values", TargetRevision: "v1", Ref: "values"},
	}
	e.createApp(ms)
	resp, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "ms", Revisions: []string{"v9"}, SourcePositions: []int64{1}})
	require.NoError(t, err)
	assert.Equal(t, []string{"rev-v9", "v1"}, resp.Revisions)
	gen = e.repo.lastGenerate()
	assert.True(t, gen.HasMultipleSources)
	require.Contains(t, gen.RefSources, "$values")
	assert.Equal(t, "v1", gen.RefSources["$values"].TargetRevision)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "ms", Revisions: []string{"v9"}, SourcePositions: []int64{5}})
	requireProblem(t, err, 400, "source position is out of range")

	// Hydrator Applications render the dry source.
	h := e.newApp("hyd", "proj-a")
	h.Spec.Source = nil
	h.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "dry"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "prod"},
	}
	e.createApp(h)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "hyd"})
	require.NoError(t, err)
	assert.Equal(t, "dry", e.repo.lastGenerate().Source.Path)
}

func tarGz(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, err := zw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return b.Bytes()
}

func TestManifestsWithFiles(t *testing.T) {
	e := newEnv(t, envOptions{config: func(_ *testEnv, c *applicationsConfig) { c.MaxLocalFilesBytes = 64 << 10 }})
	e.createApp(e.newApp("loc", "proj-a"))
	e.repo.mu.Lock()
	e.repo.manifests = map[string][]string{"local": {secretJSON}}
	e.repo.mu.Unlock()
	body := tarGz(t, "fake tar")
	sum := sha256.Sum256(body)

	resp, err := e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc", Checksum: hex.EncodeToString(sum[:])}, bytes.NewReader(body))
	require.NoError(t, err)
	require.Len(t, resp.Manifests, 1)
	assert.NotContains(t, resp.Manifests[0], "aHVudGVyMg==")
	e.repo.mu.Lock()
	assert.Equal(t, body, e.repo.filesBodies[len(e.repo.filesBodies)-1])
	e.repo.mu.Unlock()
	assert.Equal(t, "guestbook", e.repo.lastGenerate().Source.Path)
	audits := e.audit.byOperation("getApplicationManifestsWithFiles")
	require.NotEmpty(t, audits)
	assert.Equal(t, "override", audits[len(audits)-1].Action)

	_, err = e.client("reader").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body))
	requireProblem(t, err, 403, "permission denied: applications, override, proj-a/loc")
	_, err = e.client("bob").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body))
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc", Checksum: strings.Repeat("0", 64)}, bytes.NewReader(body))
	requireProblem(t, err, 400, "checksum mismatch")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, strings.NewReader("plain text"))
	requireProblem(t, err, 400, "the request body is not a gzip stream")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(append(tarGz(t, ""), make([]byte, 70<<10)...)))
	requireProblem(t, err, 413, "")
	// Content type.
	resp2, err := e.client("alice").DoRaw(e.ctx, http.MethodPost, apiclient.PathApplicationManifestsWithFiles,
		&apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body), "text/plain")
	if resp2 != nil {
		_ = resp2.Body.Close()
	}
	requireProblem(t, err, 415, "")
}

func TestRevisionMetadata(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.createApp(e.newApp("rev", "proj-a"))
	app.Status.History = v1alpha1.RevisionHistories{
		{ID: 1, Revision: "old", DeployedAt: metav1.Now(), Source: &v1alpha1.ApplicationSource{RepoURL: "https://github.com/org/old", Path: "p"}},
	}
	require.NoError(t, e.kube.Status().Update(e.ctx, app))
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e.repo.mu.Lock()
	e.repo.metadata = &reposerver.RevisionMetadata{Author: "Jane <jane@example.com>", Date: when, Message: "fix", Tags: []string{"v1"},
		References: []reposerver.RevisionReference{{RepoURL: "https://u:p@github.com/org/dry", SHA: "abc"}}}
	e.repo.chart = &reposerver.ChartDetails{Description: "chart", Home: "https://example.com"}
	e.repo.oci = &reposerver.OCIMetadata{Version: "1.2.3"}
	e.repo.mu.Unlock()
	reader := e.client("reader")

	md, err := reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha})
	require.NoError(t, err)
	assert.Equal(t, "Jane <jane@example.com>", md.Author)
	require.NotNil(t, md.Date)
	assert.True(t, when.Equal(md.Date.Time))
	require.Len(t, md.References, 1)
	assert.Equal(t, "https://github.com/org/dry", md.References[0].RepoURL)
	e.repo.mu.Lock()
	last := e.repo.metaReqs[len(e.repo.metaReqs)-1]
	e.repo.mu.Unlock()
	assert.Equal(t, sha, last.Revision)
	assert.Equal(t, repoURL, last.Repo.URL)

	// versionId selects the history entry's source.
	vid := int32(1)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "old", VersionID: &vid})
	require.NoError(t, err)
	e.repo.mu.Lock()
	last = e.repo.metaReqs[len(e.repo.metaReqs)-1]
	e.repo.mu.Unlock()
	assert.Equal(t, "https://github.com/org/old", last.Repo.URL)

	idx := int32(1)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha, SourceIndex: &idx})
	requireProblem(t, err, 400, "source index 1 not found because there is only 1 source")
	missing := int32(9)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha, VersionID: &missing})
	requireProblem(t, err, 400, "revision history not found for version ID 9")
	_, err = e.client("bob").GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha})
	requireProblem(t, err, 403, "permission denied")

	_, err = reader.GetApplicationRevisionChartDetails(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "1.0.0"})
	requireProblem(t, err, 400, "no chart found for application: rev")
	chartApp := e.newApp("chart", "proj-a")
	chartApp.Spec.Source = &v1alpha1.ApplicationSource{RepoURL: "https://github.com/org/charts", Chart: "nginx", TargetRevision: "1.0.0"}
	e.createApp(chartApp)
	cd, err := reader.GetApplicationRevisionChartDetails(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "chart", Revision: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "chart", cd.Description)
	oci, err := reader.GetApplicationRevisionOCIMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "chart", Revision: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", oci.Version)
	e.repo.mu.Lock()
	assert.Equal(t, "nginx", e.repo.chartReqs[len(e.repo.chartReqs)-1].Name)
	e.repo.mu.Unlock()

	e.repo.mu.Lock()
	e.repo.metadata = nil
	e.repo.mu.Unlock()
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "nope"})
	requireProblem(t, err, 400, "error getting revision metadata: revision not found")
}

func TestApplicationLinks(t *testing.T) {
	e := newEnv(t, envOptions{cm: map[string]string{
		"application.links": `- url: https://grafana.example.com/d/{{ .app.metadata.name }}?ns={{ .app.spec.destination.namespace }}
  title: Grafana
- url: https://only-prod.example.com
  title: Prod only
  if: app.metadata.labels.env == "prod"
- url: '{{ .managedByURL }}/applications/{{ .app.metadata.name }}'
  title: Self
- url: 'javascript:alert(1)'
  title: Script
- url: https://bad.example.com
  title: Bad condition
  if: '"not a bool"'`,
		"resource.links": `- url: https://logs.example.com/{{ .resource.metadata.name }}/{{ .resource.data.password }}
  title: Logs
  if: resource.kind == "Secret"
- url: https://apps.example.com/{{ .resource.metadata.name }}/{{ .project.metadata.name }}
  title: App
  if: resource.kind == "Application"`,
	}})
	app := e.newApp("lnk", "proj-a")
	app.Labels = map[string]string{"env": "dev"}
	e.createApp(app)
	reader := e.client("reader")

	l, err := reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	require.NoError(t, err)
	var titles []string
	for _, i := range l.Items {
		titles = append(titles, i.Title)
	}
	assert.Equal(t, []string{"Grafana", "Self"}, titles)
	assert.Equal(t, "https://grafana.example.com/d/lnk?ns=default", l.Items[0].URL)
	assert.Equal(t, "https://patrol.example.com/applications/lnk", l.Items[1].URL)

	e.updateApp("lnk", func(a *v1alpha1.Application) {
		a.Labels["env"] = "prod"
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyManagedByURL: "https://other.example.com"}
	})
	l, err = reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	require.NoError(t, err)
	require.Len(t, l.Items, 3)
	assert.Equal(t, "https://other.example.com/applications/lnk", l.Items[2].URL)

	_, err = e.client("bob").ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	requireProblem(t, err, 403, "permission denied")

	// Resource links: the Application itself.
	rl, err := reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Group: v1alpha1.GroupName, Kind: "Application", ResourceName: "lnk"})
	require.NoError(t, err)
	require.Len(t, rl.Items, 1)
	assert.Equal(t, "https://apps.example.com/lnk/proj-a", rl.Items[0].URL)

	// A managed Secret: its data is masked before templates see it.
	e.live.objs["/Secret/default/db"] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "db", "namespace": "default"},
		"data": map[string]any{"password": "aHVudGVyMg=="},
	}}
	rl, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Kind: "Secret", Namespace: "default", ResourceName: "db", Version: "v1"})
	require.NoError(t, err)
	require.Len(t, rl.Items, 1)
	assert.Equal(t, "https://logs.example.com/db/++++++++", rl.Items[0].URL)

	_, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Kind: "Pod", Namespace: "default", ResourceName: "nope"})
	requireProblem(t, err, 400, "Pod  nope not found as part of application lnk")
	_, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk"})
	requireProblem(t, err, 400, "kind and resourceName are required")

	// The project must permit the destination.
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "elsewhere"}}
	})
	_, err = reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	requireProblem(t, err, 400, "error getting destination cluster")
}

func TestResourceLinksWithoutLiveAccess(t *testing.T) {
	e := newEnv(t, envOptions{noLive: true})
	e.createApp(e.newApp("x", "proj-a"))
	_, err := e.client("reader").ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "x", Kind: "Pod", ResourceName: "p"})
	requireProblem(t, err, 503, "live resource access is not available")
}
