package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
)

const (
	bbBefore = "1111111111111111111111111111111111111111"
	bbAfter  = "2222222222222222222222222222222222222222"
)

// fakeBitbucket serves the diffstat and repository endpoints of the Bitbucket Cloud API
// under /2.0 and records the Authorization headers it receives.
type fakeBitbucket struct {
	srv        *httptest.Server
	mainBranch string
	diffStatus int

	mu    sync.Mutex
	auths []string
	calls []string
}

func newFakeBitbucket(t testing.TB, mainBranch string) *fakeBitbucket {
	t.Helper()
	f := &fakeBitbucket{mainBranch: mainBranch, diffStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2.0/repositories/acme/platform/diffstat/{spec}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if r.PathValue("spec") != bbBefore+".."+bbAfter || r.URL.Query().Get("renames") != "true" {
			http.Error(w, "unexpected diffstat request", http.StatusBadRequest)
			return
		}
		if f.diffStatus != http.StatusOK {
			http.Error(w, "boom", f.diffStatus)
			return
		}
		_, _ = fmt.Fprint(w, `{"values":[
			{"status":"modified","old":{"path":"apps/web/deployment.yaml"},"new":{"path":"apps/web/deployment.yaml"}},
			{"status":"renamed","old":{"path":"apps/old/a.yaml"},"new":{"path":"apps/new/a.yaml"}}
		]}`)
	})
	mux.HandleFunc("GET /2.0/repositories/acme/platform", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		_, _ = fmt.Fprintf(w, `{"full_name":"acme/platform","mainbranch":{"name":%q}}`, f.mainBranch)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBitbucket) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.calls = append(f.calls, r.URL.Path)
}

func (f *fakeBitbucket) snapshot() (auths, calls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...), append([]string(nil), f.calls...)
}

func (f *fakeBitbucket) apiBase() string { return f.srv.URL + "/2.0" }

// bitbucketPush returns the Bitbucket Cloud push fixture with links.self pointing at api.
func bitbucketPush(t testing.TB, api string) []byte {
	t.Helper()
	return bytes.ReplaceAll(fixture(t, "bitbucket_push.json"), []byte("API_BASE"), []byte(api))
}

func bitbucketEvent(t testing.TB, api, secret string) *scm.Event {
	t.Helper()
	ev, err := bitbucketcloud.WebhookVerifier{}.VerifyAndParse(http.Header{
		"X-Event-Key": {"repo:push"}, "X-Hook-Uuid": {secret}, "X-Request-Uuid": {"bb-1"},
	}, bitbucketPush(t, api), []byte(secret))
	require.NoError(t, err)
	return ev
}

// REPO-119: verified Bitbucket Cloud pushes call back the diffstat and main branch APIs
// with the repository credentials; failures and disallowed hosts keep the conservative
// defaults (every path changed, default branch touched).
func TestBitbucketCloudCallback(t *testing.T) {
	t.Parallel()
	const uuid = "{hook-uuid}"
	refiner := func(t testing.TB, f *fakeBitbucket, opts BitbucketCloudOptions) *bitbucketRefiner {
		t.Helper()
		if opts.AllowedAPIURLs == nil {
			opts.AllowedAPIURLs = []string{f.apiBase()}
		}
		opts.HTTPClient = f.srv.Client()
		r, err := newBitbucketRefiner(opts, discardLogger())
		require.NoError(t, err)
		return r
	}

	t.Run("changed files and main branch", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "develop")
		r := refiner(t, f, BitbucketCloudOptions{Credentials: func(_ context.Context, repoURL string) (BitbucketCredentials, error) {
			assert.Equal(t, "https://bitbucket.org/acme/platform", repoURL)
			return BitbucketCredentials{Username: "x-token-auth", Password: "repo-token"}, nil
		}})
		ev := bitbucketEvent(t, f.apiBase(), uuid)
		require.True(t, ev.Verified)
		require.Empty(t, ev.ChangedFiles)
		r.refine(context.Background(), ev)
		assert.Equal(t, []string{"apps/web/deployment.yaml", "apps/new/a.yaml", "apps/old/a.yaml"}, ev.ChangedFiles)
		assert.False(t, ev.ChangedFilesTruncated)
		assert.False(t, ev.TouchedDefaultBranch, "main is not the main branch (develop)")
		auths, _ := f.snapshot()
		require.NotEmpty(t, auths)
		for _, a := range auths {
			assert.Equal(t, "Bearer repo-token", a, "x-token-auth sends the password as a bearer token")
		}
	})
	t.Run("basic auth and main branch touched", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "main")
		r := refiner(t, f, BitbucketCloudOptions{Credentials: func(context.Context, string) (BitbucketCredentials, error) {
			return BitbucketCredentials{Username: "alice", Password: "app-password"}, nil
		}})
		ev := bitbucketEvent(t, f.apiBase(), uuid)
		r.refine(context.Background(), ev)
		assert.True(t, ev.TouchedDefaultBranch)
		auths, _ := f.snapshot()
		assert.Equal(t, basicAuth("alice", "app-password"), auths[0])
	})
	t.Run("credential lookup failure calls anonymously", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "main")
		r := refiner(t, f, BitbucketCloudOptions{Credentials: func(context.Context, string) (BitbucketCredentials, error) {
			return BitbucketCredentials{}, errors.New("no repository")
		}})
		ev := bitbucketEvent(t, f.apiBase(), uuid)
		r.refine(context.Background(), ev)
		assert.Len(t, ev.ChangedFiles, 3)
		auths, _ := f.snapshot()
		assert.Equal(t, "", auths[0])
	})
	t.Run("diffstat failure keeps every path changed", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "main")
		f.diffStatus = http.StatusNotFound
		r := refiner(t, f, BitbucketCloudOptions{})
		ev := bitbucketEvent(t, f.apiBase(), uuid)
		r.refine(context.Background(), ev)
		assert.Empty(t, ev.ChangedFiles)
		assert.True(t, ev.TouchedDefaultBranch)
	})
	t.Run("API host not allowed: no call", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "develop")
		r := refiner(t, f, BitbucketCloudOptions{AllowedAPIURLs: []string{bitbucketcloud.DefaultAPIURL}})
		ev := bitbucketEvent(t, f.apiBase(), uuid)
		r.refine(context.Background(), ev)
		_, calls := f.snapshot()
		assert.Empty(t, calls, "credentials are never sent to a host taken from the payload")
		assert.Empty(t, ev.ChangedFiles)
		assert.True(t, ev.TouchedDefaultBranch)
	})
	t.Run("unverified push: no call", func(t *testing.T) {
		t.Parallel()
		f := newFakeBitbucket(t, "develop")
		r := refiner(t, f, BitbucketCloudOptions{})
		ev, err := bitbucketcloud.WebhookVerifier{}.VerifyAndParse(http.Header{
			"X-Event-Key": {"repo:push"}, "X-Hook-Uuid": {uuid},
		}, bitbucketPush(t, f.apiBase()), nil)
		require.NoError(t, err)
		require.False(t, ev.Verified)
		r.refine(context.Background(), ev)
		_, calls := f.snapshot()
		assert.Empty(t, calls)
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		r, err := newBitbucketRefiner(BitbucketCloudOptions{Disabled: true}, discardLogger())
		require.NoError(t, err)
		assert.Nil(t, r)
		r.refine(context.Background(), &scm.Event{}) // a nil refiner is a no-op
	})
	t.Run("credentials are redacted", func(t *testing.T) {
		t.Parallel()
		c := BitbucketCredentials{Username: "alice", Password: "pw-value", BearerToken: "tok-value"}
		assert.NotContains(t, c.String(), "pw-value")
		assert.NotContains(t, c.String(), "tok-value")
		assert.NotContains(t, c.LogValue().String(), "pw-value")
	})
}

// REPO-119 + REPO-126 through the handler: the diffstat result filters Applications by
// manifest-generate-paths.
func TestBitbucketCloudPushRefreshesByChangedFiles(t *testing.T) {
	t.Parallel()
	const uuid = "{hook-uuid}"
	f := newFakeBitbucket(t, "main")
	repo := "https://bitbucket.org/acme/platform"
	objs := []client.Object{
		withAnnotation(gitApp(testNS, "web", repo, "main", "apps/web"), "."),
		withAnnotation(gitApp(testNS, "api", repo, "main", "apps/api"), "."),
		gitApp(testNS, "head", "git@bitbucket.org:acme/platform.git", "HEAD", "apps/other"),
	}
	h := newTestHandler(t, Config{
		Settings:       staticSettings(settings.Webhook{BitbucketUUID: settings.NewSensitive(uuid)}),
		BitbucketCloud: BitbucketCloudOptions{AllowedAPIURLs: []string{f.apiBase()}, HTTPClient: f.srv.Client()},
	}, objs...)
	h.start(t)
	body := bitbucketPush(t, f.apiBase())
	w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-Event-Key": "repo:push", "X-Hook-UUID": uuid, "X-Request-UUID": "bb-e2e"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	get := func(name string) map[string]string {
		return annotations(t, h.client, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}})
	}
	eventually(t, 10*time.Second, func() bool {
		return get("web")[v1alpha1.AnnotationKeyRefresh] == "normal" && get("head")[v1alpha1.AnnotationKeyRefresh] == "normal"
	}, "web and HEAD applications refreshed")
	eventually(t, 5*time.Second, func() bool { return containsAll(h.logs.String(), `"applications":2`) }, "audit log")
	assert.NotContains(t, get("api"), v1alpha1.AnnotationKeyRefresh, "apps/api did not change")
	assert.NotContains(t, h.logs.String(), uuid)
}
