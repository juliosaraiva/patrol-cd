package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// defaultWait bounds asynchronous assertions.
const defaultWait = 10 * time.Second

func testutilCounter(h *testHandler, kind, result string) float64 {
	return testutil.ToFloat64(h.metrics.refreshes.WithLabelValues(kind, result))
}

// paddedPush returns the GitHub push fixture grown to exactly size bytes with a padding
// field, so it stays valid JSON.
func paddedPush(t testing.TB, size int) []byte {
	t.Helper()
	base := fixture(t, "github_push.json")
	pad := size - len(base) - len(`"padding":"",`)
	require.Positive(t, pad)
	out := make([]byte, 0, size)
	out = append(out, `{"padding":"`...)
	out = append(out, bytes.Repeat([]byte("x"), pad)...)
	out = append(out, `",`...)
	out = append(out, base[1:]...)
	require.Len(t, out, size)
	return out
}

// REPO-115, SEC-WH-4: webhook.maxPayloadSizeMB is clamped to the 25 MiB cap of the
// pkg/scm verifiers, so the handler's limit, the verifiers' limit and the error message
// agree (a 30 MiB delivery used to be read in full and then rejected by the verifier with
// "under 50 MB").
func TestEffectivePayloadLimit(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	t.Run("clamp", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{})
		for _, tc := range []struct{ configured, want int64 }{
			{0, MaxEffectivePayloadSize},
			{-1, MaxEffectivePayloadSize},
			{64, 64},
			{10 * mib, 10 * mib},
			{MaxEffectivePayloadSize, MaxEffectivePayloadSize},
			{DefaultMaxPayloadSize, MaxEffectivePayloadSize},
			{100 * mib, MaxEffectivePayloadSize},
		} {
			assert.Equal(t, tc.want, h.payloadLimit(tc.configured), "configured %d", tc.configured)
		}
		assert.Equal(t, int64(scm.MaxWebhookPayloadBytes), int64(MaxEffectivePayloadSize))
		assert.Equal(t, 1, strings.Count(h.logs.String(), "exceeds the largest payload"), "the clamp is reported once")
	})

	sign := func(body []byte, id string) map[string]string {
		return map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": id, "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, body)}
	}
	const want = "Webhook processing failed: payload must be valid JSON under 25 MB\n"
	big := paddedPush(t, 30*mib)
	for _, configured := range []int64{0, DefaultMaxPayloadSize, 100 * mib} {
		t.Run(fmt.Sprintf("signed 30 MiB push, configured %d MiB", configured/mib), func(t *testing.T) {
			t.Parallel()
			// Zero exercises the handler's own default (50 MB, as upstream).
			ws := settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret), MaxPayloadSize: configured, SecretsLoaded: true}
			h := newTestHandler(t, Config{Settings: func() settings.Webhook { return ws }})
			// With Content-Length: rejected before reading.
			w := h.serve(newRequest(t, "/api/webhook", big, sign(big, "big-1")))
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, want, w.Body.String())
			// Without Content-Length: the read stops at the effective limit.
			req := newRequest(t, "/api/webhook", big, sign(big, "big-2"))
			req.ContentLength = -1
			w = h.serve(req)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, want, w.Body.String())
			assert.Equal(t, 2.0, h.requests(ProviderGitHub, resultTooLarge))
			assert.Empty(t, h.events)
			assert.Zero(t, h.bodies.used.Load(), "body budget released")
		})
	}
	t.Run("GHCR uses the same limit", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)})})
		hdr := map[string]string{"X-GitHub-Event": "package", "X-GitHub-Delivery": "pkg", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, big)}
		w := h.serve(newRequest(t, "/api/webhook", big, hdr))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, want, w.Body.String())
		assert.Equal(t, 1.0, h.requests(ProviderGHCR, resultTooLarge))
	})
	t.Run("a signed push of exactly the effective limit is accepted", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)})})
		body := paddedPush(t, MaxEffectivePayloadSize)
		w := h.serve(newRequest(t, "/api/webhook", body, sign(body, "max")))
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultAccepted))
		require.Len(t, h.events, 1)
		assert.True(t, (<-h.events).event.Verified)
		// One byte more is too large, whatever the setting.
		over := paddedPush(t, MaxEffectivePayloadSize+1)
		w = h.serve(newRequest(t, "/api/webhook", over, sign(over, "max+1")))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, want, w.Body.String())
	})
}

// Untrusted payload fields are clipped in log records: an unsigned delivery (accepted
// when no secret is configured) cannot make the handler write megabytes of logs.
func TestLogValuesBounded(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	t.Run("clip", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "short", clip("short", 10))
		assert.Equal(t, "abc...[truncated 3 bytes]", clip("abcdef", 3))
		// Never splits a UTF-8 sequence.
		s := strings.Repeat("é", 10) // 2 bytes each
		c := clip(s, 5)
		assert.True(t, utf8.ValidString(c), c)
		assert.True(t, strings.HasPrefix(c, "éé..."), c)
		assert.Empty(t, logErr(nil))
		assert.Len(t, logErr(errors.New(strings.Repeat("e", 5000))), maxLogErrorBytes+len("...[truncated 3976 bytes]"))
	})
	t.Run("push with a 2 MiB ref", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{})
		h.start(t)
		body := bytes.Replace(fixture(t, "github_push.json"), []byte(`"refs/heads/main"`), []byte(`"refs/heads/`+strings.Repeat("a", 2*mib)+`"`), 1)
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "huge-ref"}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		eventually(t, defaultWait, func() bool { return strings.Contains(h.logs.String(), "webhook: event processed") }, "audit record")
		logs := h.logs.String()
		assert.Less(t, len(logs), 16<<10, "log output stays bounded")
		assert.Contains(t, logs, "truncated")
		assert.NotContains(t, logs, strings.Repeat("a", maxLogValueBytes+1))
	})
	t.Run("pull request with a 1 MiB action", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{})
		body := bytes.Replace(fixture(t, "github_pull_request.json"), []byte(`"opened"`), []byte(`"`+strings.Repeat("b", mib)+`"`), 1)
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "huge-action"}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultIgnored))
		logs := h.logs.String()
		assert.Less(t, len(logs), 4<<10, "log output stays bounded")
		assert.Contains(t, logs, "truncated")
	})
	t.Run("unsupported event name is clipped in the error", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook", []byte(`{}`), map[string]string{"X-GitHub-Event": strings.Repeat("z", 64<<10)}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Less(t, len(h.logs.String()), 4<<10)
	})
}

// T10: the bytes of request bodies being read and verified concurrently are bounded
// across all sources; an exhausted budget answers 503 with Retry-After, and the budget is
// released when each request ends.
func TestInFlightBodyBudget(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	newH := func(t *testing.T, budget int64) *testHandler {
		return newTestHandler(t, Config{
			MaxInFlightBodyBytes: budget,
			Settings:             staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)}),
		})
	}
	headers := map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "d", "X-Hub-Signature-256": "sha256=00"}
	// stalled returns a request whose body blocks until the returned writer is closed.
	stalled := func(t *testing.T, contentLength int64) (*http.Request, *io.PipeWriter) {
		pr, pw := io.Pipe()
		req := newRequest(t, "/api/webhook", nil, headers)
		req.Body, req.ContentLength = pr, contentLength
		t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
		return req, pw
	}
	serveAsync := func(h *testHandler, req *http.Request) <-chan int {
		done := make(chan int, 1)
		go func() { done <- h.serve(req).Code }()
		return done
	}

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		_, err := NewHandler(Config{Namespace: "x", Client: fakeClient(t), Settings: staticSettings(settings.Webhook{}), MaxInFlightBodyBytes: MaxEffectivePayloadSize - 1})
		require.Error(t, err)
	})

	t.Run("concurrent requests with Content-Length", func(t *testing.T) {
		t.Parallel()
		h := newH(t, 32*mib)
		// Two slow 20 MiB deliveries from different sources: the second exceeds the
		// budget before any of its bytes is read.
		req1, pw1 := stalled(t, 20*mib)
		done1 := serveAsync(h, req1)
		eventually(t, defaultWait, func() bool { return h.bodies.used.Load() == 20*mib }, "first request charged")
		req2, _ := stalled(t, 20*mib)
		req2.RemoteAddr = "198.51.100.9:1"
		w := h.serve(req2)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "1", w.Header().Get("Retry-After"))
		assert.Equal(t, "Webhook server is busy\n", w.Body.String())
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultBusy))
		// Small deliveries still fit.
		small := fixture(t, "github_push.json")
		w = h.serve(newRequest(t, "/api/webhook", small, map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "s", "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, small)}))
		assert.Equal(t, http.StatusOK, w.Code)
		// The first request ends (short body: invalid payload) and releases its charge.
		_, _ = pw1.Write([]byte(`{"ref":`))
		require.NoError(t, pw1.Close())
		assert.Equal(t, http.StatusBadRequest, <-done1)
		assert.Zero(t, h.bodies.used.Load())
		// Now a 20 MiB delivery fits again.
		req3, pw3 := stalled(t, 20*mib)
		done3 := serveAsync(h, req3)
		eventually(t, defaultWait, func() bool { return h.bodies.used.Load() == 20*mib }, "third request charged")
		require.NoError(t, pw3.Close())
		assert.Equal(t, http.StatusBadRequest, <-done3)
		assert.Zero(t, h.bodies.used.Load())
	})

	t.Run("concurrent chunked requests are charged while reading", func(t *testing.T) {
		t.Parallel()
		h := newH(t, MaxEffectivePayloadSize+mib)
		chunk := bytes.Repeat([]byte("x"), 15*mib)
		req1, pw1 := stalled(t, -1)
		done1 := serveAsync(h, req1)
		go func() { _, _ = pw1.Write(chunk) }()
		eventually(t, defaultWait, func() bool { return h.bodies.used.Load() == 15*mib }, "first chunked request charged")
		req2, pw2 := stalled(t, -1)
		req2.RemoteAddr = "198.51.100.9:1"
		done2 := serveAsync(h, req2)
		go func() { _, _ = pw2.Write(chunk) }()
		assert.Equal(t, http.StatusServiceUnavailable, <-done2, "budget exhausted while reading")
		_ = pw2.CloseWithError(errors.New("client gone"))
		eventually(t, defaultWait, func() bool { return h.bodies.used.Load() == 15*mib }, "second request released")
		require.NoError(t, pw1.Close())
		assert.Equal(t, http.StatusBadRequest, <-done1)
		assert.Zero(t, h.bodies.used.Load())
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultBusy))
	})

	t.Run("many concurrent oversized claims", func(t *testing.T) {
		t.Parallel()
		h := newH(t, 64*mib)
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			codes = map[int]int{}
		)
		writers := make([]*io.PipeWriter, 0, 20)
		for i := range 20 {
			req, pw := stalled(t, 20*mib)
			req.RemoteAddr = "198.51.100." + string(rune('1'+i%9)) + ":1"
			writers = append(writers, pw)
			wg.Go(func() {
				code := h.serve(req).Code
				mu.Lock()
				codes[code]++
				mu.Unlock()
			})
		}
		eventually(t, defaultWait, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return codes[http.StatusServiceUnavailable] == 17
		}, "all but three requests refused")
		assert.LessOrEqual(t, h.bodies.used.Load(), int64(64*mib), "never more than the budget in flight")
		for _, pw := range writers {
			_ = pw.Close()
		}
		wg.Wait()
		assert.Equal(t, 3, codes[http.StatusBadRequest])
		assert.Zero(t, h.bodies.used.Load())
	})
}

// T9, SEC-WH-3: once any webhook secret is configured, unsigned deliveries are rejected
// for every provider, because the route is chosen by request headers and the repository
// by the unsigned payload (a forged GitHub-shaped delivery naming a GitLab repository
// would otherwise bypass the GitLab secret). AllowUnsignedPerProvider restores upstream.
func TestUnsignedPolicy(t *testing.T) {
	t.Parallel()
	const gitlabRepo = "https://gitlab.example.com/acme/platform"
	forgedGitHub := bytes.ReplaceAll(fixture(t, "github_push.json"), []byte(platformRepo), []byte(gitlabRepo))
	gogsBody := fixture(t, "gogs_push.json")
	githubHdr := map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "forged"}
	gogsHdr := map[string]string{"X-Gogs-Event": "push", "X-Gogs-Delivery": "forged"}
	scmProvider := func(ns string, typ v1alpha1.SCMProviderType, secretName string) *v1alpha1.SCMProvider {
		return &v1alpha1.SCMProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: string(typ)},
			Spec: v1alpha1.SCMProviderSpec{Type: typ, Webhook: &v1alpha1.SCMProviderWebhook{
				SecretRef: v1alpha1.SecretKeyReference{Name: secretName, Key: "secret"},
			}},
		}
	}
	tests := []struct {
		name     string
		ws       settings.Webhook
		objs     []client.Object
		cfg      Config
		provider string
		body     []byte
		headers  map[string]string
		wantCode int
	}{
		{
			name: "no secret anywhere: accepted (upstream)", provider: ProviderGitHub,
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusOK,
		},
		{
			name: "GitLab secret: forged GitHub delivery for a GitLab repository rejected", provider: ProviderGitHub,
			ws:   settings.Webhook{GitLabSecret: settings.NewSensitive(goodSecret)},
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusBadRequest,
		},
		{
			name: "GitLab secret with AllowUnsignedPerProvider: accepted (upstream)", provider: ProviderGitHub,
			ws:   settings.Webhook{GitLabSecret: settings.NewSensitive(goodSecret)},
			cfg:  Config{AllowUnsignedPerProvider: true},
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusOK,
		},
		{
			name: "Azure DevOps credentials: unsigned Gogs delivery rejected", provider: ProviderGogs,
			ws:   settings.Webhook{AzureDevOpsUsername: settings.NewSensitive("u"), AzureDevOpsPassword: settings.NewSensitive("p")},
			body: gogsBody, headers: gogsHdr, wantCode: http.StatusBadRequest,
		},
		{
			name: "Gitea SCMProvider secret: unsigned Gogs delivery rejected", provider: ProviderGogs,
			objs: []client.Object{scmProvider(testNS, v1alpha1.SCMProviderTypeGitea, "gitea-hook"), secretObj(testNS, "gitea-hook", "secret", goodSecret)},
			body: gogsBody, headers: gogsHdr, wantCode: http.StatusBadRequest,
		},
		{
			name: "Gitea SCMProvider secret with AllowUnsignedPerProvider: accepted", provider: ProviderGogs,
			objs: []client.Object{scmProvider(testNS, v1alpha1.SCMProviderTypeGitea, "gitea-hook"), secretObj(testNS, "gitea-hook", "secret", goodSecret)},
			cfg:  Config{AllowUnsignedPerProvider: true},
			body: gogsBody, headers: gogsHdr, wantCode: http.StatusOK,
		},
		{
			name: "unreadable SCMProvider secret of another provider still counts", provider: ProviderGitHub,
			objs: []client.Object{scmProvider(testNS, v1alpha1.SCMProviderTypeGitLab, "absent")},
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusBadRequest,
		},
		{
			name: "SCMProvider in an untrusted namespace does not count", provider: ProviderGitHub,
			objs: []client.Object{scmProvider("team-a", v1alpha1.SCMProviderTypeGitLab, "gl-hook"), secretObj("team-a", "gl-hook", "secret", goodSecret)},
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusOK,
		},
		{
			name: "RequireSignatures without any secret: rejected", provider: ProviderGitHub,
			cfg:  Config{RequireSignatures: true},
			body: forgedGitHub, headers: githubHdr, wantCode: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.Settings = staticSettings(tc.ws)
			h := newTestHandler(t, cfg, tc.objs...)
			w := h.serve(newRequest(t, "/api/webhook", tc.body, tc.headers))
			assert.Equal(t, tc.wantCode, w.Code, w.Body.String())
			if tc.wantCode == http.StatusOK {
				assert.Len(t, h.events, 1)
				return
			}
			assert.Equal(t, 1.0, h.requests(tc.provider, resultUnsigned))
			assert.Empty(t, h.events)
			assert.Contains(t, h.logs.String(), `"security":"high"`)
		})
	}

	// End to end: the GitLab-hosted Application is refreshed by the forged delivery only
	// under the upstream per-provider policy.
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("effect with AllowUnsignedPerProvider=%v", allow), func(t *testing.T) {
			t.Parallel()
			app := gitApp(testNS, "gl", gitlabRepo+".git", "main", "apps/web")
			h := newTestHandler(t, Config{
				AllowUnsignedPerProvider: allow,
				Settings:                 staticSettings(settings.Webhook{GitLabSecret: settings.NewSensitive(goodSecret)}),
			}, app)
			h.start(t)
			w := h.serve(newRequest(t, "/api/webhook", forgedGitHub, githubHdr))
			if !allow {
				assert.Equal(t, http.StatusBadRequest, w.Code)
				assert.NotContains(t, annotations(t, h.client, app), v1alpha1.AnnotationKeyRefresh)
				return
			}
			assert.Equal(t, http.StatusOK, w.Code)
			eventually(t, defaultWait, func() bool {
				return annotations(t, h.client, app)[v1alpha1.AnnotationKeyRefresh] == "normal"
			}, "upstream policy accepts the forged delivery")
		})
	}
}

// staleReader serves a fixed snapshot of objects, as a lagging cache would.
type staleReader struct{ client.Reader }

// REPO-127, SEC-WH-6: a pending hard refresh is never downgraded and a consumed one is
// not re-requested, whatever happens between the read and the patch: the merge patch
// carries the resourceVersion read, so a concurrent change makes the API server answer
// 409 and the request is re-decided on a fresh read.
func TestRefreshConditionalPatch(t *testing.T) {
	t.Parallel()
	appKey := client.ObjectKey{Namespace: testNS, Name: "web"}
	setRefresh := func(t *testing.T, c client.Client, value string) {
		t.Helper()
		var app v1alpha1.Application
		require.NoError(t, c.Get(context.Background(), appKey, &app))
		if value == "" {
			delete(app.Annotations, v1alpha1.AnnotationKeyRefresh)
		} else {
			if app.Annotations == nil {
				app.Annotations = map[string]string{}
			}
			app.Annotations[v1alpha1.AnnotationKeyRefresh] = value
		}
		require.NoError(t, c.Update(context.Background(), &app))
	}
	// changeBeforeFirstPatch changes the refresh annotation right before the first patch
	// reaches the API (after the handler read the object).
	changeBeforeFirstPatch := func(live client.WithWatch, value string, patches *int) client.Client {
		var mu sync.Mutex
		return interceptor.NewClient(live, interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				mu.Lock()
				*patches++
				first := *patches == 1
				mu.Unlock()
				if first {
					var app v1alpha1.Application
					if err := cl.Get(ctx, appKey, &app); err != nil {
						return err
					}
					if value == "" {
						delete(app.Annotations, v1alpha1.AnnotationKeyRefresh)
					} else {
						app.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: value}
					}
					if err := cl.Update(ctx, &app); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		})
	}

	t.Run("stale list cache does not matter", func(t *testing.T) {
		t.Parallel()
		// The reviewer's case: the live object has refresh=hard, the (cache-backed)
		// Reader still shows none.
		stale := fakeClient(t, gitApp(testNS, "web", platformRepo, "main", "apps/web"))
		live := fakeClient(t, gitApp(testNS, "web", platformRepo, "main", "apps/web"))
		setRefresh(t, live, "hard")
		h := newTestHandler(t, Config{Client: live, Reader: staleReader{stale}})
		require.NoError(t, h.refreshApplication(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "web"}))
		assert.Equal(t, "hard", annotations(t, live, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "web"}})[v1alpha1.AnnotationKeyRefresh])
	})
	t.Run("stale API reader conflicts instead of downgrading", func(t *testing.T) {
		t.Parallel()
		stale := fakeClient(t, gitApp(testNS, "web", platformRepo, "main", "apps/web"))
		live := fakeClient(t, gitApp(testNS, "web", platformRepo, "main", "apps/web"))
		setRefresh(t, live, "hard")
		h := newTestHandler(t, Config{Client: live, APIReader: staleReader{stale}})
		err := h.refreshApplication(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "web"})
		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err), "%v", err)
		assert.Equal(t, "hard", annotations(t, live, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "web"}})[v1alpha1.AnnotationKeyRefresh])
		h.applyRefresh(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "web"})
		assert.Equal(t, 1.0, testutilCounter(h, kindApplication, refreshError))
	})
	for _, tc := range []struct {
		name, initial, concurrent, want string
	}{
		{name: "hard requested between read and patch is kept", initial: "", concurrent: "hard", want: "hard"},
		{name: "hard consumed between read and patch is not re-requested", initial: "hard", concurrent: "", want: "normal"},
		{name: "normal consumed between read and patch is requested again", initial: "normal", concurrent: "", want: "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live := fakeClient(t, gitApp(testNS, "web", platformRepo, "main", "apps/web"))
			if tc.initial != "" {
				setRefresh(t, live, tc.initial)
			}
			patches := 0
			h := newTestHandler(t, Config{Client: changeBeforeFirstPatch(live, tc.concurrent, &patches), APIReader: live})
			require.NoError(t, h.refreshApplication(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "web"}))
			assert.Equal(t, 2, patches, "the first patch conflicted and was re-decided")
			assert.Equal(t, tc.want, annotations(t, live, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "web"}})[v1alpha1.AnnotationKeyRefresh])
		})
	}
	t.Run("pending hard hydration is kept", func(t *testing.T) {
		t.Parallel()
		app := hydrated(platformRepo, "main", "apps/web", "env/prod", "hydrated/web")
		app.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: string(v1alpha1.HydrateTypeHard)}
		live := fakeClient(t, app)
		h := newTestHandler(t, Config{Client: live})
		require.NoError(t, h.refreshApplication(context.Background(), refreshRequest{kind: kindApplication, namespace: testNS, name: "hydrated", hydrate: true}))
		got := annotations(t, live, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "hydrated"}})
		assert.Equal(t, "hard", got[v1alpha1.AnnotationKeyHydrate])
		assert.Equal(t, "normal", got[v1alpha1.AnnotationKeyRefresh])
	})
	t.Run("refreshAnnotations", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			cur     map[string]string
			hydrate bool
			want    map[string]string
		}{
			{nil, false, map[string]string{v1alpha1.AnnotationKeyRefresh: "normal"}},
			{map[string]string{v1alpha1.AnnotationKeyRefresh: "hard"}, false, map[string]string{v1alpha1.AnnotationKeyRefresh: "hard"}},
			{map[string]string{v1alpha1.AnnotationKeyRefresh: "bogus"}, false, map[string]string{v1alpha1.AnnotationKeyRefresh: "normal"}},
			{nil, true, map[string]string{v1alpha1.AnnotationKeyRefresh: "normal", v1alpha1.AnnotationKeyHydrate: "normal"}},
			{map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}, true, map[string]string{v1alpha1.AnnotationKeyRefresh: "normal", v1alpha1.AnnotationKeyHydrate: "hard"}},
			{map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}, false, map[string]string{v1alpha1.AnnotationKeyRefresh: "normal"}},
		} {
			assert.Equal(t, tc.want, refreshAnnotations(tc.cur, tc.hydrate), "%v hydrate=%v", tc.cur, tc.hydrate)
		}
	})
}
