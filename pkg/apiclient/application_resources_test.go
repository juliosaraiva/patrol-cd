package apiclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// The resource client methods put every field where the server binds it.
func TestApplicationResourceClientRequests(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, okJSON(map[string]any{}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, AuthToken: "tok"})
	require.NoError(t, err)
	ctx := context.Background()
	yes := true
	tail := int64(10)

	_, err = c.GetApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: "app", AppNamespace: "ns", Project: "p"})
	require.NoError(t, err)
	_, err = c.GetApplicationManagedResources(ctx, &apiclient.ApplicationManagedResourcesQuery{Name: "app", Group: "apps", Kind: "Deployment", Namespace: "n", ResourceName: "web"})
	require.NoError(t, err)
	_, err = c.ApplicationServerSideDiff(ctx, &apiclient.ApplicationServerSideDiffRequest{Name: "app", LiveResources: []apiclient.ResourceDiff{{Kind: "ConfigMap", Name: "c", LiveState: "null"}}, TargetManifests: []string{"{}"}})
	require.NoError(t, err)
	_, err = c.ListApplicationEvents(ctx, &apiclient.ApplicationEventsQuery{Name: "app", ResourceNamespace: "n", ResourceName: "p", ResourceUID: "u"})
	require.NoError(t, err)
	_, err = c.GetApplicationResource(ctx, &apiclient.ApplicationResourceRequest{Name: "app", Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "n", ResourceName: "web"})
	require.NoError(t, err)
	_, err = c.PatchApplicationResource(ctx, &apiclient.ApplicationResourcePatchRequest{Name: "app", Kind: "Deployment", Group: "apps", Namespace: "n", ResourceName: "web", Patch: `{"a":1}`, PatchType: apiclient.PatchTypeStrategic})
	require.NoError(t, err)
	require.NoError(t, c.DeleteApplicationResource(ctx, &apiclient.ApplicationResourceDeleteRequest{Name: "app", Kind: "Pod", Namespace: "n", ResourceName: "p", Force: &yes, Orphan: &yes}))
	_, err = c.ListApplicationResourceActions(ctx, &apiclient.ApplicationResourceRequest{Name: "app", Kind: "Pod", ResourceName: "p"})
	require.NoError(t, err)
	require.NoError(t, c.RunApplicationResourceAction(ctx, &apiclient.ResourceActionRunRequest{Name: "app", Group: "apps", Kind: "Deployment", ResourceName: "web", Action: "scale",
		ResourceActionParameters: []apiclient.ResourceActionParameter{{Name: "replicas", Value: "3"}}}))

	got := reqs()
	require.Len(t, got, 9)
	check := func(i int, method, path, query string) recorded {
		t.Helper()
		assert.Equal(t, method, got[i].Method, i)
		assert.Equal(t, path, got[i].Path, i)
		assert.Equal(t, query, got[i].Query, i)
		assert.Equal(t, "Bearer tok", got[i].Header.Get("Authorization"))
		return got[i]
	}
	check(0, http.MethodGet, "/api/v1/applications/app/resource-tree", "appNamespace=ns&project=p")
	check(1, http.MethodGet, "/api/v1/applications/app/managed-resources", "group=apps&kind=Deployment&namespace=n&resourceName=web")
	r := check(2, http.MethodPost, "/api/v1/applications/app/server-side-diff", "")
	assert.JSONEq(t, `{"liveResources":[{"kind":"ConfigMap","name":"c","liveState":"null"}],"targetManifests":["{}"]}`, r.Body)
	check(3, http.MethodGet, "/api/v1/applications/app/events", "resourceName=p&resourceNamespace=n&resourceUID=u")
	check(4, http.MethodGet, "/api/v1/applications/app/resource", "group=apps&kind=Deployment&namespace=n&resourceName=web&version=v1")
	r = check(5, http.MethodPost, "/api/v1/applications/app/resource", "group=apps&kind=Deployment&namespace=n&resourceName=web")
	assert.JSONEq(t, `{"patch":"{\"a\":1}","patchType":"strategic"}`, r.Body)
	check(6, http.MethodDelete, "/api/v1/applications/app/resource", "force=true&kind=Pod&namespace=n&orphan=true&resourceName=p")
	check(7, http.MethodGet, "/api/v1/applications/app/resource/actions", "kind=Pod&resourceName=p")
	r = check(8, http.MethodPost, "/api/v1/applications/app/resource/actions", "group=apps&kind=Deployment&resourceName=web")
	assert.JSONEq(t, `{"action":"scale","resourceActionParameters":[{"name":"replicas","value":"3"}]}`, r.Body)

	// Log streams and downloads.
	logSrv, logReqs := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("download") == "true" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "a\nb\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []apiclient.LogEntry{{Content: "one", PodName: "p"}, {Content: "two", PodName: "p"}, {Last: true}, {Content: "after last"}} {
			b, _ := json.Marshal(e)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	})
	lc, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: logSrv.URL})
	require.NoError(t, err)
	s, err := lc.StreamApplicationLogs(ctx, &apiclient.ApplicationLogsQuery{Name: "app", Kind: "Deployment", ResourceName: "web", TailLines: &tail, Follow: &yes, Filter: "!x"})
	require.NoError(t, err)
	var lines []string
	require.NoError(t, apiclient.ReadLogs(s, func(e apiclient.LogEntry) error { lines = append(lines, e.Content); return nil }))
	assert.Equal(t, []string{"one", "two"}, lines, "reading stops at the last entry")
	s, err = lc.StreamPodLogs(ctx, &apiclient.ApplicationPodLogsQuery{Name: "app", Pod: "web-0", Container: "c"})
	require.NoError(t, err)
	require.NoError(t, apiclient.ReadLogs(s, func(apiclient.LogEntry) error { return apiclient.ErrStopWatch }))
	body, err := lc.DownloadApplicationLogs(ctx, &apiclient.ApplicationLogsQuery{Name: "app", PodName: "p"})
	require.NoError(t, err)
	b, err := io.ReadAll(body)
	require.NoError(t, err)
	_ = body.Close()
	assert.Equal(t, "a\nb\n", string(b))
	lr := logReqs()
	require.Len(t, lr, 3)
	assert.Equal(t, "/api/v1/applications/app/logs", lr[0].Path)
	assert.Equal(t, "filter=%21x&follow=true&kind=Deployment&resourceName=web&tailLines=10", lr[0].Query)
	assert.Equal(t, "/api/v1/applications/app/pods/web-0/logs", lr[1].Path)
	assert.Equal(t, "container=c", lr[1].Query)
	assert.Equal(t, "download=true&podName=p", lr[2].Query)
}

func TestApplicationTreeFindNode(t *testing.T) {
	t.Parallel()
	tree := &apiclient.ApplicationTree{
		Nodes:         []apiclient.ResourceNode{{ResourceRef: apiclient.ResourceRef{Group: "apps", Kind: "Deployment", Namespace: "n", Name: "web"}}},
		OrphanedNodes: []apiclient.ResourceNode{{ResourceRef: apiclient.ResourceRef{Kind: "ConfigMap", Namespace: "n", Name: "stray"}}},
	}
	assert.NotNil(t, tree.FindNode("apps", "Deployment", "n", "web"))
	assert.NotNil(t, tree.FindNode("", "ConfigMap", "n", "stray"))
	assert.Nil(t, tree.FindNode("", "Deployment", "n", "web"))
	assert.Nil(t, (*apiclient.ApplicationTree)(nil).FindNode("", "", "", ""))
}

// The terminal client speaks TerminalMessage frames and reports pre-upgrade problems.
func TestTerminalClient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal" || r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"title":"Forbidden","status":403,"detail":"permission denied","code":"permission_denied"}`)
			return
		}
		if r.URL.Query().Get("pod") != "web-0" || r.URL.Query().Get("appName") != "app" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m apiclient.TerminalMessage
			_ = json.Unmarshal(b, &m)
			if m.Operation == apiclient.TerminalOpStdin && m.Data == "bye" {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
			reply, _ := json.Marshal(apiclient.TerminalMessage{Operation: apiclient.TerminalOpStdout, Data: fmt.Sprintf("%s:%s:%dx%d", m.Operation, m.Data, m.Cols, m.Rows)})
			_ = c.Write(ctx, websocket.MessageText, reply)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, AuthToken: "tok"})
	require.NoError(t, err)
	sess, err := c.OpenTerminal(ctx, &apiclient.TerminalRequest{AppName: "app", ProjectName: "p", Namespace: "n", Pod: "web-0", Container: "c"})
	require.NoError(t, err)
	require.NoError(t, sess.SendStdin(ctx, "ls"))
	out, err := sess.ReadUntil(ctx, "stdin:ls")
	require.NoError(t, err)
	assert.Equal(t, "stdin:ls:0x0", out)
	require.NoError(t, sess.Resize(ctx, 24, 80))
	m, err := sess.Recv(ctx)
	require.NoError(t, err)
	assert.Equal(t, "resize::80x24", m.Data)
	require.NoError(t, sess.SendStdin(ctx, "bye"))
	_, err = sess.Recv(ctx)
	assert.ErrorIs(t, err, io.EOF)

	anon, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	_, err = anon.OpenTerminal(ctx, &apiclient.TerminalRequest{AppName: "app", Pod: "web-0"})
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "%v", err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.Equal(t, "permission denied", ae.Problem.Detail)
	code, _ := apiclient.CloseStatus(errors.New("x"))
	assert.Equal(t, -1, code)
	_ = strings.TrimSpace
}
