package webhook

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

const platformRepo = "https://github.com/acme/platform"

func pushEvent(revision string, touchedHead bool, files ...string) *scm.Event {
	return &scm.Event{
		Provider: "github", Type: scm.EventPush,
		Repository: &scm.EventRepo{RepoRef: scm.RepoRef{Owner: "acme", Name: "platform"}, HTMLURL: platformRepo},
		Ref:        "refs/heads/" + revision, Revision: revision, TouchedDefaultBranch: touchedHead, ChangedFiles: files,
	}
}

func withAnnotation(app *v1alpha1.Application, paths string) *v1alpha1.Application {
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: paths}
	return app
}

func multiSource(sources ...v1alpha1.ApplicationSource) *v1alpha1.Application {
	app := gitApp(testNS, "multi", "", "", "")
	app.Spec.Source = nil
	app.Spec.Sources = sources
	return app
}

func hydrated(dryRepo, dryRev, dryPath, syncBranch, syncPath string) *v1alpha1.Application {
	app := gitApp(testNS, "hydrated", "", "", "")
	app.Spec.Source = nil
	app.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: dryRepo, TargetRevision: dryRev, Path: dryPath},
		SyncSource: v1alpha1.SyncSource{TargetBranch: syncBranch, Path: syncPath},
	}
	return app
}

// REPO-124..REPO-127: Application matching (revision, URL forms, paths, multi-source,
// hydrator).
func TestMatchAppPush(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(mustRegex(t, platformRepo))
	tests := []struct {
		name        string
		app         *v1alpha1.Application
		ev          *scm.Event
		wantMatch   bool
		wantHydrate bool
	}{
		{"HEAD app, default branch push", gitApp(testNS, "a", platformRepo, "HEAD", "apps/web"), pushEvent("main", true), true, false},
		{"empty revision, other branch push", gitApp(testNS, "a", platformRepo, "", "apps/web"), pushEvent("feature", false), false, false},
		{"branch app, same branch", gitApp(testNS, "a", platformRepo, "release/1.0", "apps/web"), pushEvent("release/1.0", false), true, false},
		{"refs/heads app", gitApp(testNS, "a", platformRepo, "refs/heads/dev", "apps/web"), pushEvent("dev", false), true, false},
		{"v-prefixed semver constraint (Masterminds accepts the prefix, as upstream)", gitApp(testNS, "a", platformRepo, "v1.*", "apps/web"), pushEvent("v1.2.0", false), true, false},
		{"tag app, other tag", gitApp(testNS, "a", platformRepo, "v1.0.0", "apps/web"), pushEvent("v1.2.0", false), false, false},
		{"refs/tags app, same tag", gitApp(testNS, "a", platformRepo, "refs/tags/v1.2.0", "apps/web"), pushEvent("v1.2.0", false), true, false},
		{"semver constraint", gitApp(testNS, "a", platformRepo, "1.*", "apps/web"), pushEvent("1.2.0", false), true, false},
		{"ssh repo URL", gitApp(testNS, "a", "git@github.com:acme/platform.git", "main", "apps/web"), pushEvent("main", true), true, false},
		{"other repository", gitApp(testNS, "a", "https://github.com/acme/other", "main", "apps/web"), pushEvent("main", true), false, false},
		{"other host", gitApp(testNS, "a", "https://gitlab.com/acme/platform", "main", "apps/web"), pushEvent("main", true), false, false},
		{
			"paths annotation: change inside", withAnnotation(gitApp(testNS, "a", platformRepo, "main", "apps/web"), "."),
			pushEvent("main", true, "apps/web/deployment.yaml"), true, false,
		},
		{
			"paths annotation: change outside", withAnnotation(gitApp(testNS, "a", platformRepo, "main", "apps/web"), "."),
			pushEvent("main", true, "apps/api/deployment.yaml", "README.md"), false, false,
		},
		{
			"paths annotation: absolute and relative", withAnnotation(gitApp(testNS, "a", platformRepo, "main", "apps/web"), "/charts; ../shared"),
			pushEvent("main", true, "apps/shared/cm.yaml"), true, false,
		},
		{
			"paths annotation: truncated file list", withAnnotation(gitApp(testNS, "a", platformRepo, "main", "apps/web"), "."),
			func() *scm.Event { e := pushEvent("main", true, "README.md"); e.ChangedFilesTruncated = true; return e }(), true, false,
		},
		{
			"paths annotation: provider without file list", withAnnotation(gitApp(testNS, "a", platformRepo, "main", "apps/web"), "."),
			pushEvent("main", true), true, false,
		},
		{
			"multi-source: second source matches", multiSource(
				v1alpha1.ApplicationSource{RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.0.0"},
				v1alpha1.ApplicationSource{RepoURL: platformRepo, TargetRevision: "main", Ref: "values"},
			), pushEvent("main", true, "values/prod.yaml"), true, false,
		},
		{
			"multi-source: annotation relative to the ref source", withAnnotation(multiSource(
				v1alpha1.ApplicationSource{RepoURL: platformRepo, TargetRevision: "main", Path: "apps/web"},
				v1alpha1.ApplicationSource{RepoURL: platformRepo, TargetRevision: "main", Ref: "values"},
			), "/values"), pushEvent("main", true, "values/prod.yaml"), true, false,
		},
		{
			"multi-source: no source matches the revision", multiSource(
				v1alpha1.ApplicationSource{RepoURL: platformRepo, TargetRevision: "stable", Path: "apps/web"},
			), pushEvent("main", true), false, false,
		},
		{
			"hydrator: dry source push hydrates", hydrated(platformRepo, "main", "dry/web", "env/prod", "hydrated/web"),
			pushEvent("main", true, "dry/web/values.yaml"), true, true,
		},
		{
			"hydrator: sync branch push refreshes only", hydrated(platformRepo, "main", "dry/web", "env/prod", "hydrated/web"),
			pushEvent("env/prod", false, "hydrated/web/manifest.yaml"), true, false,
		},
		{
			"hydrator: sync branch push outside the sync path", hydrated(platformRepo, "main", "dry/web", "env/prod", "hydrated/web"),
			pushEvent("env/prod", false, "hydrated/api/manifest.yaml"), false, false,
		},
		{
			"hydrator: sync path ignores the annotation", withAnnotation(hydrated(platformRepo, "main", "dry/web", "env/prod", "hydrated/web"), "/elsewhere"),
			pushEvent("env/prod", false, "hydrated/web/manifest.yaml"), true, false,
		},
		{
			"hydrator: dry source honours the annotation", withAnnotation(hydrated(platformRepo, "main", "dry/web", "env/prod", "hydrated/web"), "."),
			pushEvent("main", true, "dry/api/values.yaml"), false, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hydrate, ok := matchAppPush(tc.app, re, tc.ev)
			assert.Equal(t, tc.wantMatch, ok)
			assert.Equal(t, tc.wantHydrate, hydrate)
		})
	}
}

func mustRegex(t testing.TB, webURL string) string {
	t.Helper()
	re, err := WebURLRegex(webURL)
	require.NoError(t, err)
	return re.String()
}

// REPO-114/REPO-127/REPO-130 end to end against the fake API: a signed GitHub push
// refreshes matching Applications (annotation patch) in allowed namespaces only, the dry
// source of a hydrated Application also gets the hydrate annotation, and a pending hard
// refresh is not downgraded.
func TestPushRefreshesApplications(t *testing.T) {
	t.Parallel()
	hard := gitApp(testNS, "hard", platformRepo, "main", "apps/hard")
	hard.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeHard)}
	objs := []client.Object{
		gitApp(testNS, "web", platformRepo, "HEAD", "apps/web"),
		gitApp(testNS, "other-branch", platformRepo, "stable", "apps/web"),
		gitApp(testNS, "other-repo", "https://github.com/acme/other", "main", "apps/web"),
		withAnnotation(gitApp(testNS, "filtered", platformRepo, "main", "apps/api"), "."),
		gitApp("team-a", "tenant", "git@github.com:acme/platform.git", "main", "apps/web"),
		gitApp("team-b", "not-allowed", platformRepo, "main", "apps/web"),
		hydrated(platformRepo, "main", "apps/web", "env/prod", "hydrated/web"),
		hard,
	}
	h := newTestHandler(t, Config{
		ApplicationNamespaces: []string{"team-a"},
		Settings:              staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)}),
	}, objs...)
	h.start(t)
	body := fixture(t, "github_push.json")
	w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{
		"X-GitHub-Event": "push", "X-GitHub-Delivery": "e2e-1", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, body),
	}))
	require.Equal(t, http.StatusOK, w.Code)

	get := func(ns, name string) map[string]string {
		return annotations(t, h.client, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
	}
	eventually(t, 10*time.Second, func() bool {
		return get(testNS, "web")[v1alpha1.AnnotationKeyRefresh] == "normal" &&
			get("team-a", "tenant")[v1alpha1.AnnotationKeyRefresh] == "normal" &&
			get(testNS, "hydrated")[v1alpha1.AnnotationKeyHydrate] == "normal"
	}, "matching applications refreshed")
	eventually(t, 5*time.Second, func() bool { return h.logs.String() != "" && containsAll(h.logs.String(), `"applications":4`) }, "audit log")
	for _, n := range []struct{ ns, name string }{{testNS, "other-branch"}, {testNS, "other-repo"}, {testNS, "filtered"}, {"team-b", "not-allowed"}} {
		assert.NotContains(t, get(n.ns, n.name), v1alpha1.AnnotationKeyRefresh, n.name)
	}
	assert.Equal(t, "hard", get(testNS, "hard")[v1alpha1.AnnotationKeyRefresh], "a pending hard refresh is kept")
	assert.Equal(t, "normal", get(testNS, "hydrated")[v1alpha1.AnnotationKeyRefresh])
	assert.NotContains(t, get(testNS, "web"), v1alpha1.AnnotationKeyHydrate)

	logs := h.logs.String()
	assert.Contains(t, logs, `"deliveryID":"e2e-1"`)
	assert.Contains(t, logs, `"repo":"https://github.com/acme/platform"`)
	assert.NotContains(t, logs, goodSecret)
	assert.NotContains(t, logs, "kustomization.yaml", "bodies are never logged")
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !regexp.MustCompile(regexp.QuoteMeta(p)).MatchString(s) {
			return false
		}
	}
	return true
}

// REPO-129: refreshes beyond the threshold are delayed by jitter; the first ones are
// immediate.
func TestRefreshJitter(t *testing.T) {
	t.Parallel()
	var objs []client.Object
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
		objs = append(objs, gitApp(testNS, n, platformRepo, "main", "apps/"+n))
	}
	h := newTestHandler(t, Config{
		Settings:  staticSettings(settings.Webhook{RefreshJitter: time.Hour, RefreshJitterThreshold: 2}),
		randFloat: func() float64 { return 0.5 },
	}, objs...)
	n, err := h.refreshApplications(context.Background(), pushEvent("main", true), []*regexp.Regexp{regexp.MustCompile(mustRegex(t, platformRepo))}, h.cfg.Settings())
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 2, h.refreshQ.Len(), "two immediate refreshes; three wait for 30 minutes")

	h2 := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{RefreshJitter: 0, RefreshJitterThreshold: 0})}, objs...)
	_, err = h2.refreshApplications(context.Background(), pushEvent("main", true), []*regexp.Regexp{regexp.MustCompile(mustRegex(t, platformRepo))}, h2.cfg.Settings())
	require.NoError(t, err)
	assert.Equal(t, 5, h2.refreshQ.Len(), "jitter disabled")
}

// Refresh patches retry on conflict and treat a deleted object as done.
func TestPatchRetries(t *testing.T) {
	t.Parallel()
	app := gitApp(testNS, "web", platformRepo, "main", "apps/web")
	c := fakeClient(t, app)
	conflicts := 0
	ic := conflictClient(c, 2, &conflicts)
	h := newTestHandler(t, Config{Client: ic})
	h.applyRefresh(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "web"})
	assert.Equal(t, 2, conflicts)
	assert.Equal(t, "normal", annotations(t, c, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "web"}})[v1alpha1.AnnotationKeyRefresh])

	h.applyRefresh(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "gone"})
	h.applyRefresh(context.Background(), refreshRequest{kind: kindApplicationSet, namespace: testNS, name: "gone"})
	assert.Contains(t, h.logs.String(), "deleted before its refresh")
}
