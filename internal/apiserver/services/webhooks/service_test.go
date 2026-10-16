package webhooks_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/webhooks"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/webhook"
)

// receiverHandler is the real webhook receiver (no secrets configured) and the registry of
// its metrics, whose provider label tells which receiver handled a delivery.
func receiverHandler(t *testing.T) (http.Handler, *prometheus.Registry) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	reg := prometheus.NewRegistry()
	h, err := webhook.NewHandler(webhook.Config{
		Namespace: "patrol",
		Client:    fake.NewClientBuilder().WithScheme(s).Build(),
		Settings: func() settings.Webhook {
			return settings.Webhook{MaxPayloadSize: webhook.DefaultMaxPayloadSize, SecretsLoaded: true}
		},
		BitbucketCloud: webhook.BitbucketCloudOptions{Disabled: true},
		Registerer:     reg,
		Logger:         slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, reg
}

// handledBy returns the provider labels of the receiver's request counter.
func handledBy(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != "patrol_webhook_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider" {
					out = append(out, l.GetValue())
				}
			}
		}
	}
	return out
}

func deliver(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func requiredHeaders(r webhooks.Receiver) map[string]string {
	out := map[string]string{}
	for _, h := range r.RequiredHeaders {
		out[h] = "push"
		if v, ok := r.RequiredHeaderValues[h]; ok {
			out[h] = v
		}
	}
	return out
}

const unknownEvent = "Unknown webhook event"

// TestDocumentedNamesAreAccepted checks every documented receiver name and alias against
// the real handler: it is known (a delivery without the provider's headers is an unknown
// event, 400, not an unknown provider, 404), case-insensitively; other names are 404.
func TestDocumentedNamesAreAccepted(t *testing.T) {
	t.Parallel()
	h, _ := receiverHandler(t)
	names := webhooks.Provider("").JSONSchemaEnum()
	require.NotEmpty(t, names)
	for _, n := range names {
		name := n.(string)
		for _, p := range []string{name, strings.ToUpper(name)} {
			w := deliver(h, webhooks.PathWebhook+"/"+p, nil)
			assert.Equal(t, http.StatusBadRequest, w.Code, p)
			assert.Contains(t, w.Body.String(), unknownEvent, p)
		}
	}
	for _, p := range []string{"bitbucket_cloud", "codecommit", "github/extra"} {
		assert.Equal(t, http.StatusNotFound, deliver(h, webhooks.PathWebhook+"/"+p, nil).Code, p)
	}
}

// TestDocumentedHeadersSelectTheReceiver checks the documented headers: the required
// headers identify the receiver both when it is named and when it is detected, and an
// excluded header or value makes the named receiver reject the delivery.
func TestDocumentedHeadersSelectTheReceiver(t *testing.T) {
	t.Parallel()
	for _, r := range webhooks.Receivers() {
		t.Run(r.Name, func(t *testing.T) {
			t.Parallel()
			h, reg := receiverHandler(t)
			w := deliver(h, webhooks.PathWebhook+"/"+r.Name, requiredHeaders(r))
			assert.NotContains(t, w.Body.String(), unknownEvent, "named")
			w = deliver(h, webhooks.PathWebhook, requiredHeaders(r))
			assert.NotContains(t, w.Body.String(), unknownEvent, "detected")
			assert.Equal(t, []string{r.Name}, handledBy(t, reg), "both deliveries are handled by %s", r.Name)

			for _, ex := range r.ExcludedHeaders {
				hdr := requiredHeaders(r)
				hdr[ex] = "push"
				w := deliver(h, webhooks.PathWebhook+"/"+r.Name, hdr)
				assert.Contains(t, w.Body.String(), unknownEvent, "excluded header %s", ex)
			}
			for k, v := range r.ExcludedHeaderValues {
				hdr := requiredHeaders(r)
				hdr[k] = v
				w := deliver(h, webhooks.PathWebhook+"/"+r.Name, hdr)
				assert.Contains(t, w.Body.String(), unknownEvent, "excluded value %s: %s", k, v)
			}
		})
	}
	h, _ := receiverHandler(t)
	assert.Contains(t, deliver(h, webhooks.PathWebhook, nil).Body.String(), unknownEvent, "no provider headers")
}

// TestRoutesAreDocumentationOnly checks that the service never changes routing.
func TestRoutesAreDocumentationOnly(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	webhooks.New().Register(tbl)
	require.NoError(t, tbl.Err())
	routes := tbl.Routes()
	require.Len(t, routes, 2)
	for _, r := range routes {
		assert.True(t, r.DocumentationOnly, r.OperationID)
		assert.True(t, r.SkipAuth, r.OperationID)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.Extensions, "x-patrol-webhook-receivers")
	}
	mux := http.NewServeMux()
	require.NoError(t, tbl.Mount(mux, &rest.Env{DisableAuth: true}))
	for _, p := range []string{webhooks.PathWebhook, webhooks.PathWebhook + "/github"} {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, p, nil))
		assert.Empty(t, pattern, p)
	}
}

// TestSCMProviderTypes checks that every SCMProvider type with webhooks maps to exactly
// one SCM receiver (the URL the UI shows for a provider).
func TestSCMProviderTypes(t *testing.T) {
	t.Parallel()
	seen := map[v1alpha1.SCMProviderType]string{}
	for _, r := range webhooks.Receivers() {
		if r.Kind != "scm" {
			continue
		}
		for _, typ := range r.SCMProviderTypes {
			prev, dup := seen[typ]
			assert.False(t, dup, "%s maps to %s and %s", typ, prev, r.Name)
			seen[typ] = r.Name
		}
	}
	assert.Equal(t, map[v1alpha1.SCMProviderType]string{
		v1alpha1.SCMProviderTypeGitHub:          webhook.ProviderGitHub,
		v1alpha1.SCMProviderTypeGitLab:          webhook.ProviderGitLab,
		v1alpha1.SCMProviderTypeGitea:           webhook.ProviderGitea,
		v1alpha1.SCMProviderTypeBitbucketCloud:  webhook.ProviderBitbucketCloud,
		v1alpha1.SCMProviderTypeBitbucketServer: webhook.ProviderBitbucketServer,
		v1alpha1.SCMProviderTypeAzureDevOps:     webhook.ProviderAzureDevOps,
	}, seen)
}
