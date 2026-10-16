package webhook

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func ociApp(ns, name, repoURL, revision string) *v1alpha1.Application {
	return gitApp(ns, name, repoURL, revision, ".")
}

// REPO-123: GHCR package events refresh Applications whose OCI source is the published
// repository at a matching tag (semver table from the upstream docs, URL normalization).
func TestRegistryMatching(t *testing.T) {
	t.Parallel()
	const repo = "oci://ghcr.io/acme/charts/web"
	hydratedOCI := hydrated(repo, "1.x", ".", "env/prod", "hydrated/web")
	hydratedOCI.Name = "hydrated-oci"
	objs := []client.Object{
		ociApp(testNS, "exact", repo, "1.2.3"),
		ociApp(testNS, "caret", repo, "^1.2.0"),
		ociApp(testNS, "tilde-match", repo, "~1.2.0"),
		ociApp(testNS, "tilde-miss", repo, "~1.3.0"),
		ociApp(testNS, "gte", repo, ">=1.0.0"),
		ociApp(testNS, "other-tag", repo, "2.0.0"),
		ociApp(testNS, "no-scheme", "ghcr.io/acme/charts/web/", "1.2.3"),
		ociApp(testNS, "upper-case", "oci://GHCR.io/Acme/Charts/Web", "1.2.3"),
		ociApp(testNS, "other-repo", "oci://ghcr.io/acme/charts/api", "1.2.3"),
		ociApp("team-b", "not-allowed", repo, "1.2.3"),
		hydratedOCI,
	}
	h := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)})}, objs...)
	h.start(t)
	body := fixture(t, "ghcr_package.json")
	w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{
		"X-GitHub-Event": "package", "X-GitHub-Delivery": "pkg-1", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, body),
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	get := func(ns, name string) map[string]string {
		return annotations(t, h.client, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
	}
	want := []string{"exact", "caret", "tilde-match", "gte", "no-scheme", "upper-case", "hydrated-oci"}
	eventually(t, 10*time.Second, func() bool {
		for _, n := range want {
			if get(testNS, n)[v1alpha1.AnnotationKeyRefresh] != "normal" {
				return false
			}
		}
		return true
	}, "matching OCI applications refreshed")
	for _, n := range []string{"tilde-miss", "other-tag", "other-repo"} {
		assert.NotContains(t, get(testNS, n), v1alpha1.AnnotationKeyRefresh, n)
	}
	assert.NotContains(t, get("team-b", "not-allowed"), v1alpha1.AnnotationKeyRefresh)
	assert.Equal(t, "normal", get(testNS, "hydrated-oci")[v1alpha1.AnnotationKeyHydrate], "the dry source matched: hydration requested")
	assert.NotContains(t, get(testNS, "exact"), v1alpha1.AnnotationKeyHydrate)
	eventually(t, 5*time.Second, func() bool {
		return containsAll(h.logs.String(), `"event":"package"`, `"applications":7`, `"repo":"oci://ghcr.io/acme/charts/web"`)
	}, "audit log")
}

// REPO-123: skipped events (200, nothing queued) and invalid payloads.
func TestRegistrySkippedEvents(t *testing.T) {
	t.Parallel()
	base := fixture(t, "ghcr_package.json")
	tests := []struct {
		name   string
		body   []byte
		status int
		queued bool
	}{
		{"published container", base, http.StatusOK, true},
		{"npm package", bytes.Replace(base, []byte(`"CONTAINER"`), []byte(`"npm"`), 1), http.StatusOK, false},
		{"updated action", bytes.Replace(base, []byte(`"published"`), []byte(`"updated"`), 1), http.StatusOK, false},
		{"no tag", bytes.Replace(base, []byte(`"name": "1.2.3"`), []byte(`"name": ""`), 1), http.StatusOK, false},
		{"invalid JSON", []byte(`{"action":`), http.StatusBadRequest, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHandler(t, Config{})
			w := h.serve(newRequest(t, "/api/webhook", tc.body, map[string]string{"X-GitHub-Event": "package"}))
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Equal(t, tc.queued, len(h.events) == 1)
		})
	}
}

// REPO-114: a panic while processing an event is recovered; the worker keeps serving and
// the delivery is forgotten so a redelivery is processed.
func TestWorkerPanicRecovery(t *testing.T) {
	t.Parallel()
	app := gitApp(testNS, "web", platformRepo, "HEAD", "apps/web")
	base := fakeClient(t, app)
	panics := 1
	reader := &panickyReader{Reader: base, panics: &panics}
	h := newTestHandler(t, Config{Client: base, Reader: reader, Workers: 1})
	h.start(t)
	body := fixture(t, "github_push.json")
	hdr := map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "boom"}
	require.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, hdr)).Code)
	eventually(t, 5*time.Second, func() bool { return containsAll(h.logs.String(), "panic while processing webhook event") }, "panic logged")
	// Unsigned deliveries are not de-duplicated, so the same ID is processed again.
	require.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, hdr)).Code)
	eventually(t, 5*time.Second, func() bool {
		return annotations(t, base, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "web"}})[v1alpha1.AnnotationKeyRefresh] == "normal"
	}, "second delivery processed by the same worker")
}

// panickyReader panics on the first *panics List calls.
type panickyReader struct {
	client.Reader
	panics *int
}

func (r *panickyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*v1alpha1.ApplicationList); ok && *r.panics > 0 {
		*r.panics--
		panic("injected panic")
	}
	return r.Reader.List(ctx, list, opts...)
}
