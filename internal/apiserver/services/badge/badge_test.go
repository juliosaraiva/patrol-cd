package badge_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/badge"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestMain(m *testing.M) {
	os.Exit(miscapitest.Main(m, miscapitest.CRDDir(4)))
}

type fixture struct {
	*miscapitest.Env
	tenant, disabled string
}

func newFixture(t *testing.T, enabled bool) *fixture {
	t.Helper()
	return newFixtureWith(t, enabled, nil, nil)
}

// newFixtureWith builds the badge with a Config adjusted by config and the Reader returned by
// reader (default: the uncached client).
func newFixtureWith(t *testing.T, enabled bool, config func(*badge.Config), reader func(env *miscapitest.Env) client.Reader) *fixture {
	t.Helper()
	f := &fixture{tenant: miscapitest.Unique("apps"), disabled: miscapitest.Unique("off")}
	cm := map[string]string{}
	if enabled {
		cm["statusbadge.enabled"] = "true"
	}
	f.Env = miscapitest.New(t, miscapitest.Options{Kube: true, CM: cm}, func(env *miscapitest.Env) []miscapitest.Registrar {
		cfg := badge.Config{Namespace: env.Namespace, ApplicationNamespaces: []string{f.tenant}}
		if config != nil {
			config(&cfg)
		}
		var r client.Reader = env.Kube
		if reader != nil {
			r = reader(env)
		}
		return []miscapitest.Registrar{badge.New(badge.Deps{Config: cfg, Settings: env.Settings, Reader: r})}
	})
	f.CreateNamespace(f.tenant)
	f.CreateNamespace(f.disabled)
	return f
}

// app creates an Application with the given project and status.
func (f *fixture) app(ns, name, project string, health v1alpha1.HealthStatusCode, sync v1alpha1.SyncStatusCode, revisions ...string) {
	f.T.Helper()
	ctx := context.Background()
	a := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ApplicationSpec{
			Project:     project,
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/a.git", Path: "."},
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "x"},
		},
	}
	require.NoError(f.T, f.Kube.Create(ctx, a))
	a.Status.Health.Status = health
	a.Status.Sync.Status = sync
	if len(revisions) > 0 {
		a.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: name + "-op", SyncResult: &v1alpha1.SyncResultSummary{Revision: revisions[0], Revisions: revisions[1:]}}
	}
	require.NoError(f.T, f.Kube.Status().Update(ctx, a))
}

// get fetches a badge without credentials and returns the response and body.
func (f *fixture) get(t *testing.T, query string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(f.URL + apiclient.PathBadge + "?" + query)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, string(b)
}

var textRE = regexp.MustCompile(`<text id="(\w+)"[^>]*>([^<]*)</text>`)

func texts(svg string) map[string]string {
	out := map[string]string{}
	for _, m := range textRE.FindAllStringSubmatch(svg, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func fill(svg, id string) string {
	m := regexp.MustCompile(`id="` + id + `"[^>]*fill="([^"]*)"`).FindStringSubmatch(svg)
	if m == nil {
		return ""
	}
	return m[1]
}

func width(svg string) string {
	return regexp.MustCompile(`^<svg [^>]*width="(\d+)"`).FindStringSubmatch(svg)[1]
}

const (
	green  = "rgb(11, 97, 42)"
	orange = "rgb(189, 115, 0)"
	red    = "rgb(167, 46, 38)"
	purple = "rgb(115, 31, 77)"
)

func TestBadgeDisabledIs404WithoutLookup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	f.app(f.Namespace, "guestbook", "default", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
	for _, q := range []string{"name=guestbook", "name=missing", "project=default", "name=INVALID_NAME", ""} {
		resp, body := f.get(t, q)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, q)
		require.NotContains(t, body, "<svg", q)
		require.NotContains(t, body, "guestbook", q)
	}
	_, err := f.Anonymous().Badge(context.Background(), &apiclient.BadgeRequest{Name: "guestbook"})
	require.True(t, apiclient.IsNotFound(err))

	// Enabling the badge needs no restart.
	f.Settings.UpdateCM(func(cm map[string]string) { cm["statusbadge.enabled"] = "true" })
	svg, err := f.Anonymous().Badge(context.Background(), &apiclient.BadgeRequest{Name: "guestbook"})
	require.NoError(t, err)
	require.Equal(t, "Healthy", texts(string(svg))["leftText"])
}

func TestBadgeApplication(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	f.app(f.Namespace, "guestbook", "default", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced, "0123456789abcdef")
	f.app(f.Namespace, "broken", "default", v1alpha1.HealthStatusDegraded, v1alpha1.SyncStatusCodeOutOfSync)
	f.app(f.tenant, "tenant-app", "default", v1alpha1.HealthStatusProgressing, v1alpha1.SyncStatusCodeSynced)
	f.app(f.disabled, "hidden", "default", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)

	resp, body := f.get(t, "name=guestbook")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	require.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"))
	require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	require.True(t, strings.HasPrefix(body, "<svg "))
	tx := texts(body)
	require.Equal(t, "Healthy", tx["leftText"])
	require.Equal(t, "Synced", tx["rightText"])
	require.Equal(t, green, fill(body, "leftRect"))
	require.Equal(t, green, fill(body, "rightRect"))
	require.Equal(t, "131", width(body))
	require.NotContains(t, body, "revisionText")

	_, body = f.get(t, "name=broken")
	require.Equal(t, "Degraded", texts(body)["leftText"])
	require.Equal(t, red, fill(body, "leftRect"))
	require.Equal(t, orange, fill(body, "rightRect"))

	cases := []struct {
		query string
		check func(t *testing.T, body string)
	}{
		{"name=guestbook&revision=true", func(t *testing.T, body string) {
			require.Equal(t, "(0123456)", texts(body)["revisionText"])
			require.Equal(t, "192", width(body))
		}},
		{"name=guestbook&revision=TRUE&keepFullRevision=true", func(t *testing.T, body string) {
			require.Equal(t, "(0123456789abcdef)", texts(body)["revisionText"])
			require.Equal(t, "400", width(body))
		}},
		{"name=guestbook&width=300", func(t *testing.T, body string) { require.Equal(t, "300", width(body)) }},
		{"name=guestbook&width=abc", func(t *testing.T, body string) { require.Equal(t, "131", width(body)) }},
		{"name=guestbook&width=999999", func(t *testing.T, body string) { require.Equal(t, "131", width(body)) }},
		{"name=guestbook&showAppName=true", func(t *testing.T, body string) {
			require.Equal(t, "guestbook", texts(body)["titleText"])
			require.Contains(t, body, `height="40"`)
		}},
		{"name=broken&revision=true", func(t *testing.T, body string) {
			require.NotContains(t, body, "revisionText", "no sync result, no revision")
		}},
		{"name=missing", func(t *testing.T, body string) {
			require.Equal(t, "Not Found", texts(body)["leftText"])
			require.Empty(t, texts(body)["rightText"])
		}},
		{"name=tenant-app&appNamespace=" + f.tenant, func(t *testing.T, body string) {
			require.Equal(t, "Progressing", texts(body)["leftText"])
		}},
		{"name=tenant-app&namespace=" + f.tenant, func(t *testing.T, body string) {
			require.Equal(t, "Progressing", texts(body)["leftText"], "upstream parameter name")
		}},
		{"name=hidden&appNamespace=" + f.disabled, func(t *testing.T, body string) {
			require.Equal(t, "Not Found", texts(body)["leftText"], "namespace not enabled")
		}},
		{"name=missing&showAppName=true&revision=true", func(t *testing.T, body string) {
			require.NotContains(t, body, "titleText")
			require.NotContains(t, body, "revisionText")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp, body := f.get(t, tc.query)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			tc.check(t, body)
		})
	}
}

func TestBadgeProjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	f.app(f.Namespace, "a1", "alpha", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
	f.app(f.Namespace, "a2", "alpha", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
	f.app(f.Namespace, "b1", "beta", v1alpha1.HealthStatusProgressing, v1alpha1.SyncStatusCodeOutOfSync)
	f.app(f.tenant, "t1", "alpha", v1alpha1.HealthStatusDegraded, v1alpha1.SyncStatusCodeOutOfSync)

	cases := []struct{ query, health, sync string }{
		{"project=alpha", "Healthy", "Synced"},
		{"project=alpha&project=beta", "Degraded", "OutOfSync"},
		{"project=gamma", "Unknown", "Unknown"},
		{"project=ALPHA", "Unknown", "Unknown"}, // valid (case-insensitive check), matches nothing
		{"project=alpha&appNamespace=" + f.tenant, "Degraded", "OutOfSync"},
		{"name=a1&project=beta", "Degraded", "OutOfSync"}, // aggregation overrides the app
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			_, body := f.get(t, tc.query)
			tx := texts(body)
			require.Equal(t, tc.health, tx["leftText"])
			require.Equal(t, tc.sync, tx["rightText"])
		})
	}
	_, body := f.get(t, "project=gamma")
	require.Equal(t, purple, fill(body, "leftRect"))

	// Through the Go client, repeated project parameters.
	svg, err := f.Anonymous().Badge(context.Background(), &apiclient.BadgeRequest{Projects: []string{"alpha"}, Width: ptr.To(200)})
	require.NoError(t, err)
	require.Equal(t, "200", width(string(svg)))
}

func TestBadgeValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	for _, q := range []string{
		"name=" + url.QueryEscape("Not_Valid"),
		"name=" + url.QueryEscape("../etc"),
		"name=" + strings.Repeat("a", 300),
		"appNamespace=" + url.QueryEscape("Bad NS"),
		"namespace=" + url.QueryEscape("x/y"),
		"project=" + url.QueryEscape("bad project"),
		"project=" + url.QueryEscape("<script>"),
		"name=a&" + strings.Repeat("project=p&", 60),
	} {
		resp, body := f.get(t, q)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, q)
		require.NotContains(t, body, "<svg")
	}
	resp, _ := f.get(t, "name=a&x="+strings.Repeat("y", 5000))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "oversized query")
	resp, _ = f.get(t, "name=%zz")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "malformed query")
}

func TestBadgeIsPublicAndIgnoresCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	f.app(f.Namespace, "pub", "default", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
	req, err := http.NewRequest(http.MethodGet, f.URL+apiclient.PathBadge+"?name=pub", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer not.a.valid-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "an invalid token does not break the public badge")
	require.Empty(t, f.Audit.ByOperation("getBadge"), "badge requests are not audited")
}

func TestBadgeUnavailableWithoutSettings(t *testing.T) {
	t.Parallel()
	env := miscapitest.New(t, miscapitest.Options{}, func(*miscapitest.Env) []miscapitest.Registrar {
		return []miscapitest.Registrar{badge.New(badge.Deps{})}
	})
	resp, err := http.Get(env.URL + apiclient.PathBadge + "?name=x")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// newCache starts a controller-runtime cache (the reader an API server wires for
// Applications) for the duration of the test.
func newCache(t *testing.T) client.Reader {
	t.Helper()
	c, err := cache.New(miscapitest.Config(t), cache.Options{Scheme: miscapitest.Scheme()})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.Start(ctx) }()
	require.True(t, c.WaitForCacheSync(ctx))
	return c
}

// TestBadgeProjectsPaging aggregates more Applications than one page, through the uncached
// client (continue tokens) and through a controller-runtime cache (no pagination, Limit
// truncates), and keeps the MaxApplications cap with both.
func TestBadgeProjectsPaging(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"uncached", "cache"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var reader func(env *miscapitest.Env) client.Reader
			if name == "cache" {
				reader = func(*miscapitest.Env) client.Reader { return newCache(t) }
			}
			f := newFixtureWith(t, true, func(c *badge.Config) { c.PageSize = 2; c.MaxApplications = 6 }, reader)
			capped := newFixtureWith(t, true, func(c *badge.Config) { c.PageSize = 2; c.MaxApplications = 4 }, reader)
			for _, e := range []*fixture{f, capped} {
				for i := range 4 {
					e.app(e.Namespace, fmt.Sprintf("ok%d", i), "alpha", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
				}
				// One Application of alpha is degraded: it is found wherever it falls in the pages.
				e.app(e.Namespace, "bad", "alpha", v1alpha1.HealthStatusDegraded, v1alpha1.SyncStatusCodeSynced)
				e.app(e.Namespace, "beta1", "beta", v1alpha1.HealthStatusHealthy, v1alpha1.SyncStatusCodeSynced)
			}
			want := map[string][2]string{
				"project=alpha": {"Degraded", "Synced"},
				"project=beta":  {"Healthy", "Synced"},
				"name=ok1":      {"Healthy", "Synced"},
			}
			for q, w := range want {
				// A cache is eventually consistent: wait for it to see every Application.
				require.Eventually(t, func() bool {
					_, body := f.get(t, q)
					tx := texts(body)
					return tx["leftText"] == w[0] && tx["rightText"] == w[1]
				}, 20*time.Second, 50*time.Millisecond, q)
			}
			// Six Applications in the namespace, more than MaxApplications=4: Unknown, even
			// once the cache has every Application.
			require.Eventually(t, func() bool {
				_, body := capped.get(t, "name=beta1")
				return texts(body)["leftText"] == "Healthy"
			}, 20*time.Second, 50*time.Millisecond)
			time.Sleep(200 * time.Millisecond)
			_, body := capped.get(t, "project=beta")
			tx := texts(body)
			require.Equal(t, "Unknown", tx["leftText"])
			require.Equal(t, "Unknown", tx["rightText"])
		})
	}
}
