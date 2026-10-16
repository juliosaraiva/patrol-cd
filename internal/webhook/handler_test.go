package webhook

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// providerCase describes how to deliver a signed fixture for one provider.
type providerCase struct {
	name     string
	fixture  string
	settings func(secret string) settings.Webhook
	// headers returns the delivery headers for body signed with secret (secret "" means
	// unsigned) and delivery ID id.
	headers func(body []byte, secret, id string) map[string]string
	// signatureHeader is removed for the "missing signature" case.
	signatureHeader string
	// bodySigned is true when the signature covers the body (HMAC); token-based
	// providers authenticate the sender only, so a tampered body is accepted.
	bodySigned bool
	// unauthorizedStatus is the status of signature failures.
	unauthorizedStatus int
}

const (
	goodSecret  = "s3cr3t-webhook-value"
	wrongSecret = "not-the-secret"
)

func providerCases() []providerCase {
	sens := settings.NewSensitive
	return []providerCase{
		{
			name: ProviderGitHub, fixture: "github_push.json", signatureHeader: "X-Hub-Signature-256", bodySigned: true, unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{GitHubSecret: sens(s)} },
			headers: func(body []byte, s, id string) map[string]string {
				h := map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": id}
				if s != "" {
					h["X-Hub-Signature-256"] = "sha256=" + hmacHex(s, body)
				}
				return h
			},
		},
		{
			name: ProviderGitLab, fixture: "gitlab_push.json", signatureHeader: "X-Gitlab-Token", unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{GitLabSecret: sens(s)} },
			headers: func(_ []byte, s, id string) map[string]string {
				h := map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Event-UUID": id}
				if s != "" {
					h["X-Gitlab-Token"] = s
				}
				return h
			},
		},
		{
			name: ProviderGogs, fixture: "gogs_push.json", signatureHeader: "X-Gogs-Signature", bodySigned: true, unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{GogsSecret: sens(s)} },
			headers: func(body []byte, s, id string) map[string]string {
				// Gogs also sends GitHub-compatible headers; it must not be routed to GitHub.
				h := map[string]string{"X-Gogs-Event": "push", "X-Gogs-Delivery": id, "X-GitHub-Event": "push"}
				if s != "" {
					h["X-Gogs-Signature"] = hmacHex(s, body)
				}
				return h
			},
		},
		{
			name: ProviderGitea, fixture: "gitea_push.json", signatureHeader: "X-Gitea-Signature", bodySigned: true, unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{GogsSecret: sens(s)} },
			headers: func(body []byte, s, id string) map[string]string {
				h := map[string]string{"X-Gitea-Event": "push", "X-Gogs-Event": "push", "X-GitHub-Event": "push", "X-Gitea-Delivery": id}
				if s != "" {
					h["X-Gitea-Signature"] = hmacHex(s, body)
				}
				return h
			},
		},
		{
			name: ProviderBitbucketCloud, fixture: "bitbucket_push.json", signatureHeader: "X-Hook-UUID", unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{BitbucketUUID: sens(s)} },
			headers: func(_ []byte, s, id string) map[string]string {
				// Bitbucket Cloud always sends X-Hook-UUID; an unsigned delivery carries the
				// hook's UUID but no secret is configured to compare it with.
				uuid := s
				if uuid == "" {
					uuid = "{unchecked}"
				}
				return map[string]string{"X-Event-Key": "repo:push", "X-Request-UUID": id, "X-Hook-UUID": uuid}
			},
		},
		{
			name: ProviderBitbucketServer, fixture: "bitbucketserver_push.json", signatureHeader: "X-Hub-Signature", bodySigned: true, unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook { return settings.Webhook{BitbucketServerSecret: sens(s)} },
			headers: func(body []byte, s, id string) map[string]string {
				h := map[string]string{"X-Event-Key": "repo:refs_changed", "X-Request-Id": id}
				if s != "" {
					h["X-Hub-Signature"] = "sha256=" + hmacHex(s, body)
				}
				return h
			},
		},
		{
			name: ProviderAzureDevOps, fixture: "azuredevops_push.json", signatureHeader: "Authorization", unauthorizedStatus: 400,
			settings: func(s string) settings.Webhook {
				return settings.Webhook{AzureDevOpsUsername: sens("patrol"), AzureDevOpsPassword: sens(s)}
			},
			headers: func(_ []byte, s, id string) map[string]string {
				h := map[string]string{"X-Vss-Activityid": id}
				if s != "" {
					h["Authorization"] = basicAuth("patrol", s)
				}
				return h
			},
		},
		{
			name: ProviderGHCR, fixture: "ghcr_package.json", signatureHeader: "X-Hub-Signature-256", bodySigned: true, unauthorizedStatus: 401,
			settings: func(s string) settings.Webhook { return settings.Webhook{GitHubSecret: sens(s)} },
			headers: func(body []byte, s, id string) map[string]string {
				h := map[string]string{"X-GitHub-Event": "package", "X-GitHub-Delivery": id}
				if s != "" {
					h["X-Hub-Signature-256"] = "sha256=" + hmacHex(s, body)
				}
				return h
			},
		},
	}
}

// T9/T10, REPO-114..REPO-123: every provider with valid, tampered, wrong-secret,
// unsigned, oversize, replayed and rate-limited deliveries. Signatures are computed over
// the authored fixtures with the secret configured in settings.
func TestProviderDeliveries(t *testing.T) {
	t.Parallel()
	for _, pc := range providerCases() {
		t.Run(pc.name, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, pc.fixture)
			newH := func(cfg Config) *testHandler {
				if cfg.Settings == nil {
					cfg.Settings = staticSettings(pc.settings(goodSecret))
				}
				return newTestHandler(t, cfg)
			}

			t.Run("valid", func(t *testing.T) {
				h := newH(Config{})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "delivery-1")))
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, 1.0, h.requests(pc.name, resultAccepted))
				require.Len(t, h.events, 1)
				qe := <-h.events
				if qe.event != nil {
					assert.True(t, qe.event.Verified)
				} else {
					assert.True(t, qe.registry.Verified)
				}
				assert.NotContains(t, h.logs.String(), goodSecret)
			})

			t.Run("explicit provider path", func(t *testing.T) {
				h := newH(Config{})
				w := h.serve(newRequest(t, "/api/webhook/"+pc.name, body, pc.headers(body, goodSecret, "delivery-1")))
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, 1.0, h.requests(pc.name, resultAccepted))
			})

			t.Run("tampered body", func(t *testing.T) {
				h := newH(Config{})
				hdr := pc.headers(body, goodSecret, "delivery-1")
				tampered := bytes.Replace(body, []byte("acme"), []byte("evil"), 1)
				require.NotEqual(t, body, tampered)
				w := h.serve(newRequest(t, "/api/webhook", tampered, hdr))
				if pc.bodySigned {
					assert.Equal(t, pc.unauthorizedStatus, w.Code)
					assert.Equal(t, 1.0, h.requests(pc.name, resultUnauthorized))
					assert.Empty(t, h.events)
				} else {
					// Token-based providers authenticate the sender, not the body.
					assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				}
			})

			t.Run("wrong secret", func(t *testing.T) {
				h := newH(Config{})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, wrongSecret, "delivery-1")))
				assert.Equal(t, pc.unauthorizedStatus, w.Code)
				assert.Equal(t, 1.0, h.requests(pc.name, resultUnauthorized))
				assert.Empty(t, h.events)
				assert.Contains(t, h.logs.String(), `"security":"high"`)
				assert.NotContains(t, h.logs.String(), goodSecret)
				assert.NotContains(t, h.logs.String(), wrongSecret)
			})

			t.Run("missing signature", func(t *testing.T) {
				h := newH(Config{})
				hdr := pc.headers(body, goodSecret, "delivery-1")
				delete(hdr, pc.signatureHeader)
				w := h.serve(newRequest(t, "/api/webhook", body, hdr))
				assert.Equal(t, pc.unauthorizedStatus, w.Code)
				assert.Empty(t, h.events)
			})

			t.Run("unsigned without secret is accepted", func(t *testing.T) {
				h := newH(Config{Settings: staticSettings(settings.Webhook{})})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, "", "delivery-1")))
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Len(t, h.events, 1)
				qe := <-h.events
				if qe.event != nil {
					assert.False(t, qe.event.Verified)
				}
			})

			t.Run("unsigned rejected when signatures are required", func(t *testing.T) {
				h := newH(Config{Settings: staticSettings(settings.Webhook{}), RequireSignatures: true})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, "", "delivery-1")))
				assert.Equal(t, pc.unauthorizedStatus, w.Code)
				assert.Equal(t, 1.0, h.requests(pc.name, resultUnsigned))
				assert.Empty(t, h.events)
			})

			t.Run("oversize body", func(t *testing.T) {
				ws := pc.settings(goodSecret)
				ws.MaxPayloadSize = 64
				h := newH(Config{Settings: staticSettings(ws)})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "delivery-1")))
				assert.Equal(t, http.StatusBadRequest, w.Code)
				assert.Equal(t, "Webhook processing failed: payload must be valid JSON under 0 MB\n", w.Body.String())
				assert.Equal(t, 1.0, h.requests(pc.name, resultTooLarge))
				// Without Content-Length the cap applies while reading.
				req := newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "delivery-2"))
				req.ContentLength = -1
				w = h.serve(req)
				assert.Equal(t, http.StatusBadRequest, w.Code)
				assert.Equal(t, 2.0, h.requests(pc.name, resultTooLarge))
				assert.Empty(t, h.events)
			})

			t.Run("replayed delivery ID", func(t *testing.T) {
				h := newH(Config{})
				for i := range 3 {
					w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "delivery-dup")))
					assert.Equal(t, http.StatusOK, w.Code, "attempt %d", i)
				}
				assert.Len(t, h.events, 1, "redeliveries are processed once")
				assert.Equal(t, 2.0, h.requests(pc.name, resultDuplicate))
			})

			t.Run("unsigned rejected when another provider has a secret", func(t *testing.T) {
				// T9: the route is chosen by headers, so a secret for any provider makes
				// signatures mandatory on every route (default policy).
				other := settings.Webhook{GitLabSecret: settings.NewSensitive(goodSecret)}
				if pc.name == ProviderGitLab {
					other = settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)}
				}
				h := newH(Config{Settings: staticSettings(other)})
				w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, "", "delivery-1")))
				assert.Equal(t, pc.unauthorizedStatus, w.Code)
				assert.Equal(t, 1.0, h.requests(pc.name, resultUnsigned))
				assert.Empty(t, h.events)
				h = newH(Config{Settings: staticSettings(other), AllowUnsignedPerProvider: true})
				w = h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, "", "delivery-1")))
				assert.Equal(t, http.StatusOK, w.Code, "upstream per-provider policy")
			})

			t.Run("rate limited", func(t *testing.T) {
				h := newH(Config{RateLimit: RateLimit{RequestsPerSecond: 0.001, Burst: 2}})
				var codes []int
				for i := range 3 {
					w := h.serve(newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "rl-"+string(rune('a'+i)))))
					codes = append(codes, w.Code)
				}
				assert.Equal(t, []int{200, 200, http.StatusTooManyRequests}, codes)
				// Another source has its own bucket.
				req := newRequest(t, "/api/webhook", body, pc.headers(body, goodSecret, "rl-other"))
				req.RemoteAddr = "198.51.100.7:1"
				assert.Equal(t, http.StatusOK, h.serve(req).Code)
			})
		})
	}
}

// REPO-114: upstream TestInvalidMethod, TestInvalidEvent, TestUnknownEvent and the
// remaining status codes.
func TestHTTPSemantics(t *testing.T) {
	t.Parallel()
	t.Run("GET with provider headers is 405", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		req := newRequest(t, "/api/webhook", nil, map[string]string{"X-GitHub-Event": "push"})
		req.Method = http.MethodGet
		w := h.serve(req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		assert.Equal(t, "Webhook processing failed\n", w.Body.String())
		assert.Equal(t, http.MethodPost, w.Header().Get("Allow"))
	})
	t.Run("empty GitHub push is 400 invalid JSON", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook", nil, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		// Upstream says "under 50 MB" (its default); Patrol states the effective limit,
		// the 50 MB default clamped to the 25 MiB provider cap (see TestEffectivePayloadLimit).
		assert.Equal(t, "Webhook processing failed: payload must be valid JSON under 25 MB\n", w.Body.String())
	})
	t.Run("unknown event headers are 400", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook", nil, map[string]string{"X-Unknown-Event": "push"}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "Unknown webhook event\n", w.Body.String())
		assert.Equal(t, 1.0, h.requests(providerUnknown, resultUnknownProvider))
	})
	t.Run("unknown provider path is 404", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook/svn", nil, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
	t.Run("explicit provider with foreign headers is 400", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook/gitlab", nil, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "Unknown webhook event\n", w.Body.String())
	})
	t.Run("GitHub ping is acknowledged and ignored", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		body := fixture(t, "github_ping.json")
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "ping", "X-GitHub-Delivery": "p1"}))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultIgnored))
		assert.Empty(t, h.events)
		assert.Contains(t, h.logs.String(), "ignoring webhook event")
	})
	t.Run("unsupported GitHub event is 400", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook", []byte(`{}`), map[string]string{"X-GitHub-Event": "issues"}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "Webhook processing failed\n", w.Body.String())
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultUnsupported))
	})
	t.Run("pull request with a non-refreshing action is ignored", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		body := bytes.Replace(fixture(t, "github_pull_request.json"), []byte(`"opened"`), []byte(`"edited"`), 1)
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "pull_request"}))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultIgnored))
	})
	t.Run("Gogs push without repository is ignored", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		w := h.serve(newRequest(t, "/api/webhook", []byte(`{"ref":"refs/heads/main"}`), map[string]string{"X-Gogs-Event": "push"}))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, h.events)
	})
	t.Run("oversized delivery ID is 400", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		body := fixture(t, "github_push.json")
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": strings.Repeat("x", 200)}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("full queue is 503 and the delivery can be retried", func(t *testing.T) {
		h := newTestHandler(t, Config{QueueSize: 1, Settings: staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)})})
		body := fixture(t, "github_push.json")
		sign := func(id string) map[string]string {
			return map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": id, "X-Hub-Signature-256": "sha256=" + hmacHex(goodSecret, body)}
		}
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, sign("q1"))).Code)
		w := h.serve(newRequest(t, "/api/webhook", body, sign("q2")))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "Queue is full, discarding webhook payload\n", w.Body.String())
		<-h.events
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, sign("q2"))).Code, "a rejected delivery is not remembered as seen")
	})
	t.Run("queued byte budget is 503", func(t *testing.T) {
		h := newTestHandler(t, Config{MaxQueuedBytes: 100})
		body := fixture(t, "github_push.json")
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
	t.Run("Register mounts both paths", func(t *testing.T) {
		h := newTestHandler(t, Config{})
		mux := http.NewServeMux()
		h.Register(mux)
		for _, p := range []string{"/api/webhook", "/api/webhook/github"} {
			w := httpRecord(mux, newRequest(t, p, nil, map[string]string{"X-GitHub-Event": "push"}))
			assert.Equal(t, http.StatusBadRequest, w.Code, p)
		}
	})
}

// REPO-116 / ADR-0010: SCMProvider webhook secrets (rotation, unavailable secrets fail
// closed, tenant namespaces untrusted by default).
func TestSCMProviderSecrets(t *testing.T) {
	t.Parallel()
	body := fixture(t, "github_push.json")
	signed := func(secret, id string) map[string]string {
		return map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": id, "X-Hub-Signature-256": "sha256=" + hmacHex(secret, body)}
	}
	scmProvider := func(ns, name, secretName string) *v1alpha1.SCMProvider {
		return &v1alpha1.SCMProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, Webhook: &v1alpha1.SCMProviderWebhook{
				SecretRef: v1alpha1.SecretKeyReference{Name: secretName, Key: "secret"},
			}},
		}
	}
	t.Run("old and new secrets during rotation", func(t *testing.T) {
		h := newTestHandler(t, Config{Settings: staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive("old-secret")})},
			scmProvider(testNS, "gh", "gh-hook"), secretObj(testNS, "gh-hook", "secret", "new-secret\n"))
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, signed("old-secret", "r1"))).Code)
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, signed("new-secret", "r2"))).Code)
		assert.Equal(t, http.StatusBadRequest, h.serve(newRequest(t, "/api/webhook", body, signed("other", "r3"))).Code)
		assert.Equal(t, http.StatusBadRequest, h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code,
			"an SCMProvider secret alone makes signatures mandatory")
		assert.NotContains(t, h.logs.String(), "new-secret")
	})
	t.Run("missing secret fails closed", func(t *testing.T) {
		h := newTestHandler(t, Config{}, scmProvider(testNS, "gh", "absent"))
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultUnauthorized))
	})
	t.Run("tenant namespaces are not trusted by default", func(t *testing.T) {
		h := newTestHandler(t, Config{}, scmProvider("team-a", "gh", "gh-hook"), secretObj("team-a", "gh-hook", "secret", "tenant"))
		// No trusted secret: unsigned deliveries are accepted (upstream default) and the
		// tenant secret is not consulted.
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code)
		h2 := newTestHandler(t, Config{SCMProviderNamespaces: []string{testNS, "team-a"}}, scmProvider("team-a", "gh", "gh-hook"), secretObj("team-a", "gh-hook", "secret", "tenant"))
		assert.Equal(t, http.StatusBadRequest, h2.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code)
		assert.Equal(t, http.StatusOK, h2.serve(newRequest(t, "/api/webhook", body, signed("tenant", "t1"))).Code)
	})
	t.Run("listing failure is 503", func(t *testing.T) {
		c := fakeClient(t)
		failing := interceptorClient(c, errors.New("apiserver unavailable"))
		h := newTestHandler(t, Config{Client: failing})
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
	t.Run("patrol-secret not loaded fails closed", func(t *testing.T) {
		notLoaded := func() settings.Webhook { return settings.Webhook{MaxPayloadSize: DefaultMaxPayloadSize} }
		h := newTestHandler(t, Config{Settings: notLoaded})
		w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"}))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "10", w.Header().Get("Retry-After"))
		assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultSecretsUnavailable))
		assert.Empty(t, h.events)
		// GHCR package events too.
		ghcr := fixture(t, "ghcr_package.json")
		w = h.serve(newRequest(t, "/api/webhook", ghcr, map[string]string{"X-GitHub-Event": "package"}))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		// An SCMProvider secret still verifies deliveries meanwhile; unsigned ones are
		// rejected.
		h2 := newTestHandler(t, Config{Settings: notLoaded},
			scmProvider(testNS, "gh", "gh-hook"), secretObj(testNS, "gh-hook", "secret", "provider-secret"))
		assert.Equal(t, http.StatusOK, h2.serve(newRequest(t, "/api/webhook", body, signed("provider-secret", "nl1"))).Code)
		assert.Equal(t, http.StatusBadRequest, h2.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code)
		// CurrentSettings before the first snapshot reports secrets as not loaded.
		assert.False(t, CurrentSettings(nilSettings{})().SecretsLoaded)
	})
	t.Run("secret rotation applies without restart", func(t *testing.T) {
		current := settings.Webhook{SecretsLoaded: true}
		src := func() settings.Webhook { return current }
		h := newTestHandler(t, Config{Settings: src})
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code)
		current = settings.Webhook{SecretsLoaded: true, GitHubSecret: settings.NewSensitive(goodSecret)}
		assert.Equal(t, http.StatusBadRequest, h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"})).Code)
		assert.Equal(t, http.StatusOK, h.serve(newRequest(t, "/api/webhook", body, signed(goodSecret, "rot"))).Code)
	})
}

func interceptorClient(c client.WithWatch, err error) client.Client {
	return interceptor.NewClient(c, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return err },
	})
}

func httpRecord(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Start can run once and stops within the shutdown timeout.
func TestStartLifecycle(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, Config{ShutdownTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()
	eventually(t, 5*time.Second, func() bool { return h.started.Load() }, "started")
	require.Error(t, h.Start(context.Background()), "second Start fails")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return")
	}
	body := fixture(t, "github_push.json")
	w := h.serve(newRequest(t, "/api/webhook", body, map[string]string{"X-GitHub-Event": "push"}))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "no events are accepted after shutdown")
}

func TestNewHandlerValidation(t *testing.T) {
	t.Parallel()
	c := fakeClient(t)
	ss := staticSettings(settings.Webhook{})
	for name, cfg := range map[string]Config{
		"namespace": {Client: c, Settings: ss},
		"client":    {Namespace: "x", Settings: ss},
		"settings":  {Namespace: "x", Client: c},
		"bitbucket": {Namespace: "x", Client: c, Settings: ss, BitbucketCloud: BitbucketCloudOptions{AllowedAPIURLs: []string{"ftp://x"}}},
	} {
		_, err := NewHandler(cfg)
		assert.Error(t, err, name)
	}
}

// nilSettings is a settings manager without a snapshot yet.
type nilSettings struct{}

func (nilSettings) Current() *settings.Settings { return nil }
