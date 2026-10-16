package apiclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// The application client methods put every field where the server binds it.
func TestApplicationClientRequests(t *testing.T) {
	srv, reqs := recorder(t, okJSON(map[string]any{}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, AuthToken: "tok"})
	require.NoError(t, err)
	ctx := context.Background()
	yes := true
	idx := int32(2)

	_, err = c.ListApplications(ctx, &apiclient.ApplicationQuery{Projects: []string{"a", "b"}, Selector: "x=y", Repo: "https://r", AppNamespace: "ns", Limit: 5, Continue: "tok"})
	require.NoError(t, err)
	_, err = c.GetApplication(ctx, &apiclient.ApplicationGetRequest{Name: "app", Refresh: "hard", Project: []string{"p"}})
	require.NoError(t, err)
	app := &v1alpha1.Application{}
	app.Name = "app"
	_, err = c.CreateApplication(ctx, &apiclient.ApplicationCreateRequest{Application: app, Upsert: &yes})
	require.NoError(t, err)
	_, err = c.UpdateApplication(ctx, &apiclient.ApplicationUpdateRequest{Application: app, Validate: new(false)})
	require.NoError(t, err)
	_, err = c.UpdateApplicationSpec(ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "app", Spec: &v1alpha1.ApplicationSpec{Project: "p"}})
	require.NoError(t, err)
	_, err = c.PatchApplication(ctx, &apiclient.ApplicationPatchRequest{Name: "app", Patch: `{"a":1}`, PatchType: apiclient.PatchTypeMerge})
	require.NoError(t, err)
	require.NoError(t, c.DeleteApplication(ctx, &apiclient.ApplicationDeleteRequest{Name: "app", Cascade: new(false)}))
	_, err = c.SyncApplication(ctx, &apiclient.ApplicationSyncRequest{Name: "app", AppNamespace: "ns", Revisions: []string{"v1"}, SourcePositions: []int64{2},
		Prune: true, SyncOptions: &apiclient.ApplicationSyncOptions{Items: []string{"Replace=true"}}})
	require.NoError(t, err)
	_, err = c.RollbackApplication(ctx, &apiclient.ApplicationRollbackRequest{Name: "app", ID: 3})
	require.NoError(t, err)
	_, err = c.TerminateApplicationOperation(ctx, &apiclient.ApplicationRef{Name: "app"})
	require.NoError(t, err)
	_, err = c.GetApplicationOperation(ctx, &apiclient.ApplicationOperationRequest{Name: "app", Operation: "app-sync-x"})
	require.NoError(t, err)
	_, err = c.GetApplicationRevisionMetadata(ctx, &apiclient.ApplicationRevisionRequest{Name: "app", Revision: "feature/x", SourceIndex: &idx})
	require.NoError(t, err)
	_, err = c.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{Name: "app", Revisions: []string{"a", "b"}, SourcePositions: []int64{1, 2}})
	require.NoError(t, err)
	_, err = c.GetApplicationManifestsWithFiles(ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "app", Checksum: "abc"}, strings.NewReader("\x1f\x8bdata"))
	require.NoError(t, err)
	_, err = c.ListApplicationResourceLinks(ctx, &apiclient.ApplicationResourceRequest{Name: "app", Kind: "Pod", ResourceName: "p"})
	require.NoError(t, err)

	got := reqs()
	require.Len(t, got, 15)
	check := func(i int, method, rawPath, query string) recorded {
		t.Helper()
		assert.Equal(t, method, got[i].Method, i)
		assert.Equal(t, rawPath, got[i].RawPath, i)
		assert.Equal(t, query, got[i].Query, i)
		return got[i]
	}
	check(0, http.MethodGet, "/api/v1/applications", "appNamespace=ns&continue=tok&limit=5&projects=a&projects=b&repo=https%3A%2F%2Fr&selector=x%3Dy")
	check(1, http.MethodGet, "/api/v1/applications/app", "project=p&refresh=hard")
	r := check(2, http.MethodPost, "/api/v1/applications", "upsert=true")
	assert.Contains(t, r.Body, `"name":"app"`)
	check(3, http.MethodPut, "/api/v1/applications/app", "validate=false")
	r = check(4, http.MethodPut, "/api/v1/applications/app/spec", "")
	assert.JSONEq(t, `{"destination":{},"project":"p"}`, r.Body)
	r = check(5, http.MethodPatch, "/api/v1/applications/app", "")
	assert.JSONEq(t, `{"patch":"{\"a\":1}","patchType":"merge"}`, r.Body)
	check(6, http.MethodDelete, "/api/v1/applications/app", "cascade=false")
	r = check(7, http.MethodPost, "/api/v1/applications/app/sync", "appNamespace=ns")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.Body), &body))
	assert.Equal(t, true, body["prune"])
	assert.Equal(t, []any{"v1"}, body["revisions"])
	assert.Equal(t, []any{float64(2)}, body["sourcePositions"])
	assert.Equal(t, map[string]any{"items": []any{"Replace=true"}}, body["syncOptions"])
	r = check(8, http.MethodPost, "/api/v1/applications/app/rollback", "")
	assert.JSONEq(t, `{"id":3}`, r.Body)
	check(9, http.MethodDelete, "/api/v1/applications/app/operation", "")
	check(10, http.MethodGet, "/api/v1/applications/app/operations/app-sync-x", "")
	check(11, http.MethodGet, "/api/v1/applications/app/revisions/feature%2Fx/metadata", "sourceIndex=2")
	check(12, http.MethodGet, "/api/v1/applications/app/manifests", "revisions=a&revisions=b&sourcePositions=1&sourcePositions=2")
	r = check(13, http.MethodPost, "/api/v1/applications/app/manifests-with-files", "checksum=abc")
	assert.Equal(t, apiclient.ContentTypeGzip, r.Header.Get("Content-Type"))
	assert.Equal(t, "\x1f\x8bdata", r.Body)
	check(14, http.MethodGet, "/api/v1/applications/app/resource/links", "kind=Pod&resourceName=p")
}

func TestApplicationWatchStream(t *testing.T) {
	srv, reqs := recorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"ADDED","object":{"metadata":{"name":"a"},"spec":{"destination":{},"project":"p"}}}` + "\n\n"))
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	s, err := c.WatchApplications(context.Background(), &apiclient.ApplicationQuery{Name: "a"}, nil)
	require.NoError(t, err)
	var names []string
	require.NoError(t, apiclient.Watch(s, func(ev apiclient.ApplicationWatchEvent) error {
		names = append(names, ev.Type+":"+ev.Object.Name)
		return nil
	}))
	assert.Equal(t, []string{"ADDED:a"}, names)
	assert.Equal(t, "/api/v1/stream/applications", reqs()[0].Path)
	assert.Equal(t, "name=a", reqs()[0].Query)
}
