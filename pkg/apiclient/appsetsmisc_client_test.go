package apiclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestApplicationSetClientRequests checks the HTTP form of every ApplicationSet client
// method (method, path, query and body) against the route contract.
func TestApplicationSetClientRequests(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, okJSON(map[string]any{}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, AuthToken: "tok"})
	require.NoError(t, err)
	ctx := context.Background()
	as := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}

	_, err = c.ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{Projects: []string{"a", "b"}, Selector: "x=y", AppsetNamespace: "ns", Limit: 5, Continue: "tok"})
	require.NoError(t, err)
	_, err = c.GetApplicationSet(ctx, "my/set", "ns")
	require.NoError(t, err)
	_, err = c.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: as, Upsert: true, DryRun: true})
	require.NoError(t, err)
	require.NoError(t, c.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "set", AppsetNamespace: "ns", Cascade: ptr.To(false)}))
	require.NoError(t, c.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "set", PropagationPolicy: "foreground"}))
	_, err = c.GenerateApplicationSet(ctx, as)
	require.NoError(t, err)
	_, err = c.GetApplicationSetResourceTree(ctx, "set", "")
	require.NoError(t, err)
	_, err = c.ListApplicationSetEvents(ctx, "set", "ns")
	require.NoError(t, err)

	got := reqs()
	require.Len(t, got, 8)
	type want struct{ method, path, query string }
	for i, w := range []want{
		{http.MethodGet, "/api/v1/applicationsets", "appsetNamespace=ns&continue=tok&limit=5&projects=a&projects=b&selector=x%3Dy"},
		{http.MethodGet, "/api/v1/applicationsets/my%2Fset", "appsetNamespace=ns"},
		{http.MethodPost, "/api/v1/applicationsets", "dryRun=true&upsert=true"},
		{http.MethodDelete, "/api/v1/applicationsets/set", "appsetNamespace=ns&cascade=false"},
		{http.MethodDelete, "/api/v1/applicationsets/set", "propagationPolicy=foreground"},
		{http.MethodPost, "/api/v1/applicationsets/generate", ""},
		{http.MethodGet, "/api/v1/applicationsets/set/resource-tree", ""},
		{http.MethodGet, "/api/v1/applicationsets/set/events", "appsetNamespace=ns"},
	} {
		require.Equal(t, w.method, got[i].Method, i)
		require.Equal(t, w.path, got[i].RawPath, i)
		q, _ := url.ParseQuery(got[i].Query)
		require.Equal(t, w.query, q.Encode(), i)
		require.Equal(t, "Bearer tok", got[i].Header.Get("Authorization"))
	}
	// Create sends the object itself; generate wraps it.
	var created v1alpha1.ApplicationSet
	require.NoError(t, json.Unmarshal([]byte(got[2].Body), &created))
	require.Equal(t, "set", created.Name)
	var gen map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got[5].Body), &gen))
	require.Contains(t, gen, "applicationSet")
	require.Empty(t, got[3].Body, "DELETE carries no body")
}

func TestWatchApplicationSetsQuery(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"ADDED\",\"object\":{\"metadata\":{\"name\":\"a\"}}}\n\n")
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	s, err := c.WatchApplicationSets(context.Background(), &apiclient.ApplicationSetWatchRequest{Name: "a", Projects: []string{"p"}, AppsetNamespace: "ns", ResourceVersion: "42"}, &apiclient.StreamOptions{LastEventID: "41"})
	require.NoError(t, err)
	var names []string
	require.NoError(t, apiclient.Watch(s, func(ev apiclient.WatchEvent[v1alpha1.ApplicationSet]) error {
		names = append(names, ev.Type+" "+ev.Object.Name)
		return nil
	}))
	require.Equal(t, []string{"ADDED a"}, names)
	r := reqs()[0]
	require.Equal(t, "/api/v1/stream/applicationsets", r.Path)
	q, _ := url.ParseQuery(r.Query)
	require.Equal(t, url.Values{"name": {"a"}, "projects": {"p"}, "appsetNamespace": {"ns"}, "resourceVersion": {"42"}}, q)
	require.Equal(t, "41", r.Header.Get("Last-Event-ID"))
}

func TestNotificationClient(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, okJSON(map[string]any{"items": []map[string]string{{"name": "x", "description": "d"}}}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()
	svcs, err := c.ListNotificationServices(ctx)
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationService{{Name: "x"}}, svcs.Items)
	trs, err := c.ListNotificationTriggers(ctx)
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationTrigger{{Name: "x", Description: "d"}}, trs.Items)
	tps, err := c.ListNotificationTemplates(ctx)
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationTemplate{{Name: "x"}}, tps.Items)
	var paths []string
	for _, r := range reqs() {
		paths = append(paths, r.Method+" "+r.Path)
	}
	require.Equal(t, []string{"GET /api/v1/notifications/services", "GET /api/v1/notifications/triggers", "GET /api/v1/notifications/templates"}, paths)
}

func TestBadgeClient(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "missing" {
			w.Header().Set("Content-Type", apiclient.ProblemContentType)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":404,"code":"not_found","detail":"status badges are disabled"}`)
			return
		}
		w.Header().Set("Content-Type", apiclient.BadgeContentType)
		_, _ = io.WriteString(w, "<svg/>")
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	svg, err := c.Badge(context.Background(), &apiclient.BadgeRequest{Name: "a", AppNamespace: "ns", Projects: []string{"p", "q"}, Revision: true, ShowAppName: true, KeepFullRevision: true, Width: ptr.To(200)})
	require.NoError(t, err)
	require.Equal(t, "<svg/>", string(svg))
	q, _ := url.ParseQuery(reqs()[0].Query)
	require.Equal(t, url.Values{"name": {"a"}, "appNamespace": {"ns"}, "project": {"p", "q"}, "revision": {"true"}, "showAppName": {"true"}, "keepFullRevision": {"true"}, "width": {"200"}}, q)
	require.Equal(t, apiclient.PathBadge, reqs()[0].Path)
	_, err = c.Badge(context.Background(), &apiclient.BadgeRequest{Name: "missing"})
	require.True(t, apiclient.IsNotFound(err))
}

func TestInvokeExtensionClient(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/denied") {
			w.Header().Set("Content-Type", apiclient.ProblemContentType)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status":403,"code":"permission_denied","detail":"Unauthorized extension request"}`)
			return
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "backend body")
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL + "/root", AuthToken: "tok"})
	require.NoError(t, err)
	ctx := context.Background()
	resp, err := c.InvokeExtension(ctx, &apiclient.ExtensionCall{
		Method: http.MethodPost, Extension: "metrics", Path: "/api/v1/a b/c%d", Query: url.Values{"q": {"up"}},
		AppNamespace: "ns", AppName: "app", Project: "proj",
		Header: http.Header{"Accept": {"text/plain"}}, Body: strings.NewReader("payload"), ContentType: "text/plain",
	})
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusTeapot, resp.StatusCode, "backend statuses pass through")
	require.Equal(t, "backend body", string(b))
	require.NoError(t, apiclient.ResponseError(&http.Response{StatusCode: http.StatusOK}))

	r := reqs()[0]
	require.Equal(t, http.MethodPost, r.Method)
	require.Equal(t, "/root/extensions/metrics/api/v1/a%20b/c%25d", r.RawPath)
	require.Equal(t, "q=up", r.Query)
	require.Equal(t, "payload", r.Body)
	require.Equal(t, "ns:app", r.Header.Get(apiclient.HeaderExtensionApplication))
	require.Equal(t, "proj", r.Header.Get(apiclient.HeaderExtensionProject))
	require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
	require.Equal(t, "patrol", r.Header.Get(apiclient.CSRFHeader))
	require.Equal(t, "text/plain", r.Header.Get("Content-Type"))
	require.Equal(t, "text/plain", r.Header.Get("Accept"))

	resp, err = c.InvokeExtension(ctx, &apiclient.ExtensionCall{Extension: "metrics", Path: "denied", AppNamespace: "ns", AppName: "app", Project: "proj"})
	require.NoError(t, err)
	perr := apiclient.ResponseError(resp)
	_ = resp.Body.Close()
	require.True(t, apiclient.IsPermissionDenied(perr), "%v", perr)
	require.Equal(t, http.MethodGet, reqs()[1].Method, "GET by default")

	_, err = c.InvokeExtension(ctx, &apiclient.ExtensionCall{})
	require.Error(t, err)
}
