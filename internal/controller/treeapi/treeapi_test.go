package treeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// Compile-time checks: the sharding helpers plug into the client router.
var (
	_ ShardResolver  = sharding.StatusShardResolver{}
	_ HolderResolver = (*sharding.LeaseHolders)(nil)
	_ Router         = (*ShardRouter)(nil)
	_ Router         = StaticRouter{}
)

const testToken = "s3cr3t-internal-token"

func obj(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	var u unstructured.Unstructured
	require.NoError(t, json.Unmarshal([]byte(s), &u.Object))
	return &u
}

// fakeProvider serves fixed state per Application.
type fakeProvider struct {
	mu       sync.Mutex
	trees    map[AppKey]*compare.ApplicationTree
	managed  map[AppKey][]compare.ManagedResource
	live     map[string]*unstructured.Unstructured // by group/kind/namespace/name
	err      error
	ssdCalls [][2][]*unstructured.Unstructured
	liveRefs []compare.ResourceRef
	ssd      func(live, target []*unstructured.Unstructured) ([]diff.Result, error)
}

func (f *fakeProvider) Tree(_ context.Context, app AppKey) (*compare.ApplicationTree, error) {
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.trees[app]
	if !ok {
		return nil, fmt.Errorf("application %s: %w", app, ErrNotFound)
	}
	return t, nil
}

func (f *fakeProvider) ManagedResources(_ context.Context, app AppKey) ([]compare.ManagedResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	m, ok := f.managed[app]
	if !ok {
		return nil, fmt.Errorf("application %s: %w", app, ErrNotFound)
	}
	return m, nil
}

func (f *fakeProvider) ServerSideDiff(_ context.Context, _ AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error) {
	f.mu.Lock()
	f.ssdCalls = append(f.ssdCalls, [2][]*unstructured.Unstructured{live, target})
	f.mu.Unlock()
	return f.ssd(live, target)
}

func (f *fakeProvider) LiveResource(_ context.Context, _ AppKey, ref compare.ResourceRef) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	f.liveRefs = append(f.liveRefs, ref)
	f.mu.Unlock()
	o, ok := f.live[ref.Group+"/"+ref.Kind+"/"+ref.Namespace+"/"+ref.Name]
	if !ok {
		return nil, fmt.Errorf("live object: %w", ErrNotFound)
	}
	return o.DeepCopy(), nil
}

func (f *fakeProvider) SensitiveAnnotations() []string { return []string{"secret.example/token"} }

var app1 = AppKey{Namespace: "apps", Name: "guestbook"}

func application(key AppKey) *v1alpha1.Application {
	return &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
}

const (
	deployJSON       = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default"},"spec":{"replicas":2}}`
	deployLiveJSON   = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default","uid":"u-web"},"spec":{"replicas":3}}`
	secretTargetJSON = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"default","annotations":{"secret.example/token":"tok-value"}},"data":{"password":"cGxhaW4tcGFzc3dvcmQ="}}`
	secretLiveJSON   = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"default","uid":"u-creds","annotations":{"secret.example/token":"tok-value"}},"data":{"password":"b2xkLXBhc3N3b3Jk"}}`
)

func newFixture(t *testing.T) *fakeProvider {
	t.Helper()
	tree := &compare.ApplicationTree{
		Nodes: []compare.ResourceNode{
			{ResourceRef: compare.ResourceRef{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "default", Name: "web", UID: "u-web"}},
			{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "Secret", Namespace: "default", Name: "creds", UID: "u-creds"}},
			{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "default", Name: "missing"}},
		},
		Hosts: []compare.HostInfo{{Name: "node-1"}},
	}
	return &fakeProvider{
		trees: map[AppKey]*compare.ApplicationTree{app1: tree},
		managed: map[AppKey][]compare.ManagedResource{app1: {
			{
				Key:    compare.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "web"},
				Target: obj(t, deployJSON), Live: obj(t, deployLiveJSON),
				Diff:            diff.Result{Modified: true, NormalizedLive: []byte(deployLiveJSON), PredictedLive: []byte(deployJSON)},
				ResourceVersion: "42",
			},
			{
				Key:    compare.ResourceKey{Kind: "Secret", Namespace: "default", Name: "creds"},
				Target: obj(t, secretTargetJSON), Live: obj(t, secretLiveJSON),
				Diff: diff.Result{Modified: true, NormalizedLive: []byte(secretLiveJSON), PredictedLive: []byte(secretTargetJSON)},
			},
			{
				Key:    compare.ResourceKey{Group: "batch", Kind: "Job", Namespace: "default", Name: "pre-sync"},
				Hook:   true,
				Target: obj(t, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"pre-sync","namespace":"default"}}`),
			},
			{
				Key:    compare.ResourceKey{Kind: "ConfigMap", Namespace: "default", Name: "missing"},
				Target: obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"missing","namespace":"default"}}`),
				Diff:   diff.Result{Modified: true, NormalizedLive: []byte("null"), PredictedLive: []byte(`{"kind":"ConfigMap"}`)},
			},
		}},
		live: map[string]*unstructured.Unstructured{
			"apps/Deployment/default/web": obj(t, deployLiveJSON),
			"/Secret/default/creds":       obj(t, secretLiveJSON),
		},
		ssd: func(live, target []*unstructured.Unstructured) ([]diff.Result, error) {
			out := make([]diff.Result, len(target))
			for i := range target {
				pred, _ := json.Marshal(target[i])
				norm, _ := json.Marshal(live[i])
				out[i] = diff.Result{Modified: i%2 == 0, PredictedLive: pred, NormalizedLive: norm}
			}
			return out, nil
		},
	}
}

type testServer struct {
	srv     *httptest.Server
	client  *Client
	reg     *prometheus.Registry
	logs    *bytes.Buffer
	handler http.Handler
}

func startServer(t *testing.T, p StateProvider, opts HandlerOptions, clientOpts ...ClientOption) *testServer {
	t.Helper()
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	if opts.Token == "" {
		opts.Token = testToken
	}
	opts.Registerer = reg
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	h, err := NewHandler(p, opts)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(StaticRouter{URL: srv.URL}, testToken, clientOpts...)
	require.NoError(t, err)
	return &testServer{srv: srv, client: c, reg: reg, logs: &logs, handler: h}
}

func TestNewHandlerAndClientValidation(t *testing.T) {
	t.Parallel()
	_, err := NewHandler(nil, HandlerOptions{Token: "x"})
	require.Error(t, err)
	_, err = NewHandler(newFixture(t), HandlerOptions{})
	require.ErrorContains(t, err, "internal token is required")
	_, err = NewClient(nil, "x")
	require.Error(t, err)
	_, err = NewClient(StaticRouter{}, "")
	require.Error(t, err)
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	ts := startServer(t, newFixture(t), HandlerOptions{})
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"valid", "Bearer " + testToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.srv.URL+PathTree+"?namespace=apps&name=guestbook", nil)
			require.NoError(t, err)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, tc.want, resp.StatusCode)
			if tc.want == http.StatusUnauthorized {
				assert.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))
				assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")
			}
		})
	}
	resp, err := http.Get(ts.srv.URL + PathHealthz) //nolint:noctx // test
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "health check needs no token")

	bad, err := NewClient(StaticRouter{URL: ts.srv.URL}, "wrong")
	require.NoError(t, err)
	_, err = bad.Tree(context.Background(), application(app1))
	require.ErrorIs(t, err, ErrUnauthenticated)
	assert.Equal(t, 1.0, counterValue(t, ts.reg, "tree", "401"))
}

func counterValue(t *testing.T, reg *prometheus.Registry, route, code string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "patrol_controller_treeapi_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["route"] == route && labels["code"] == code {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestTreeRoundTrip(t *testing.T) {
	t.Parallel()
	p := newFixture(t)
	ts := startServer(t, p, HandlerOptions{})
	ctx := context.Background()
	tree, etag, changed, err := ts.client.TreeIfChanged(ctx, application(app1), "")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, p.trees[app1], tree)
	require.NotEmpty(t, etag)

	tree, etag2, changed, err := ts.client.TreeIfChanged(ctx, application(app1), etag)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, tree)
	assert.Equal(t, etag, etag2)

	_, err = ts.client.Tree(ctx, application(AppKey{Namespace: "apps", Name: "unknown"}))
	require.ErrorIs(t, err, ErrNotFound)
	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Status)
	assert.Equal(t, "application apps/unknown: not found", apiErr.Message)

	_, err = ts.client.Tree(ctx, application(AppKey{Namespace: "Bad_NS", Name: "x"}))
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func TestProviderErrorMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err    error
		target error
		status int
	}{
		{fmt.Errorf("state not computed yet: %w", ErrUnavailable), ErrUnavailable, http.StatusServiceUnavailable},
		{fmt.Errorf("x: %w", ErrPermissionDenied), ErrPermissionDenied, http.StatusForbidden},
		{errors.New("boom"), ErrInternal, http.StatusInternalServerError},
		{context.DeadlineExceeded, ErrDeadlineExceeded, http.StatusGatewayTimeout},
		{&NotOwnedError{Shard: -1}, ErrNotOwned, http.StatusMisdirectedRequest},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			t.Parallel()
			p := newFixture(t)
			p.err = tc.err
			ts := startServer(t, p, HandlerOptions{})
			_, err := ts.client.Tree(context.Background(), application(app1))
			require.ErrorIs(t, err, tc.target)
			var apiErr *Error
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tc.status, apiErr.Status)
			if tc.status == http.StatusInternalServerError {
				assert.Contains(t, ts.logs.String(), "controller tree API request failed")
			}
		})
	}
}

func TestManagedResources(t *testing.T) {
	t.Parallel()
	ts := startServer(t, newFixture(t), HandlerOptions{})
	ctx := context.Background()
	items, err := ts.client.ManagedResources(ctx, application(app1), ManagedResourcesQuery{})
	require.NoError(t, err)
	require.Len(t, items, 3, "hooks are excluded")

	web := items[0]
	assert.Equal(t, ResourceDiff{
		Group: "apps", Kind: "Deployment", Namespace: "default", Name: "web",
		TargetState: deployJSONCompact(t, deployJSON), LiveState: deployJSONCompact(t, deployLiveJSON),
		NormalizedLiveState: deployLiveJSON, PredictedLiveState: deployJSON, ResourceVersion: "42", Modified: true,
	}, web)

	secret := items[1]
	all := secret.TargetState + secret.LiveState + secret.NormalizedLiveState + secret.PredictedLiveState
	for _, plain := range []string{"cGxhaW4tcGFzc3dvcmQ=", "b2xkLXBhc3N3b3Jk", "tok-value"} {
		assert.NotContains(t, all, plain, "Secret values must be masked")
	}
	assert.Contains(t, secret.TargetState, `"password":"++++++++"`)
	assert.Contains(t, secret.LiveState, `"password":"++++++++++++"`, "different values get different masks")
	assert.Contains(t, secret.TargetState, `"secret.example/token":"++++++++"`)
	assert.True(t, secret.Modified)

	missing := items[2]
	assert.Equal(t, "null", missing.LiveState)
	assert.Equal(t, "null", missing.NormalizedLiveState)

	items, err = ts.client.ManagedResources(ctx, application(app1), ManagedResourcesQuery{Kind: "Secret"})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "creds", items[0].Name)
	items, err = ts.client.ManagedResources(ctx, application(app1), ManagedResourcesQuery{Kind: "Job"})
	require.NoError(t, err)
	assert.Empty(t, items, "hooks never match")
	items, err = ts.client.ManagedResources(ctx, application(app1), ManagedResourcesQuery{Group: "apps", ResourceNamespace: "default", ResourceName: "web"})
	require.NoError(t, err)
	require.Len(t, items, 1)

	_, err = ts.client.ManagedResources(ctx, application(app1), ManagedResourcesQuery{Kind: strings.Repeat("k", 300)})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func deployJSONCompact(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(obj(t, s).Object)
	require.NoError(t, err)
	return string(b)
}

func liveEntry(group, kind, ns, name, state string) ResourceDiff {
	return ResourceDiff{Group: group, Kind: kind, Namespace: ns, Name: name, LiveState: state}
}

func TestServerSideDiff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("aligned diff with masking and metadata", func(t *testing.T) {
		t.Parallel()
		p := newFixture(t)
		ts := startServer(t, p, HandlerOptions{})
		resp, err := ts.client.ServerSideDiff(ctx, application(app1),
			[]ResourceDiff{
				liveEntry("apps", "Deployment", "default", "web", deployLiveJSON),
				liveEntry("", "Secret", "default", "creds", secretLiveJSON),
				{},
			},
			[]string{deployJSON, secretTargetJSON, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"default"}}`})
		require.NoError(t, err)
		require.Len(t, resp.Items, 3)
		assert.True(t, resp.Modified)
		assert.Equal(t, "web", resp.Items[0].Name)
		assert.True(t, resp.Items[0].Modified)
		assert.Contains(t, resp.Items[0].TargetState, `"replicas":2`)
		assert.NotContains(t, resp.Items[1].TargetState+resp.Items[1].LiveState, "cGxhaW4tcGFzc3dvcmQ=")
		assert.NotContains(t, resp.Items[1].TargetState+resp.Items[1].LiveState, "b2xkLXBhc3N3b3Jk")
		assert.Equal(t, ResourceDiff{Kind: "Service", Namespace: "default", Name: "svc", Modified: true, TargetState: resp.Items[2].TargetState, LiveState: "null"}, resp.Items[2], "metadata from the target when there is no live entry")
		require.Len(t, p.ssdCalls, 1)
		assert.Nil(t, p.ssdCalls[0][0][2], "a missing live state is a create")
	})
	t.Run("spoofed metadata cannot bypass Secret masking", func(t *testing.T) {
		t.Parallel()
		ts := startServer(t, newFixture(t), HandlerOptions{})
		resp, err := ts.client.ServerSideDiff(ctx, application(app1),
			[]ResourceDiff{{Kind: "ConfigMap", Namespace: "default", Name: "creds"}}, []string{secretTargetJSON})
		require.NoError(t, err)
		assert.NotContains(t, resp.Items[0].TargetState, "cGxhaW4tcGFzc3dvcmQ=")
		assert.NotContains(t, resp.Items[0].TargetState, "tok-value")
	})
	for _, tc := range []struct {
		name    string
		live    []ResourceDiff
		targets []string
		target  error
		message string
	}{
		{"name mismatch", []ResourceDiff{liveEntry("apps", "Deployment", "default", "other", deployLiveJSON)}, []string{deployJSON}, ErrInvalidArgument, "name mismatch: expected other, got web for live resource apps/Deployment/other"},
		{"namespace mismatch", []ResourceDiff{liveEntry("apps", "Deployment", "prod", "web", deployLiveJSON)}, []string{deployJSON}, ErrInvalidArgument, "namespace mismatch: expected prod, got default"},
		{"group mismatch", []ResourceDiff{liveEntry("extensions", "Deployment", "default", "web", deployLiveJSON)}, []string{deployJSON}, ErrInvalidArgument, "group mismatch: expected extensions, got apps"},
		{"kind mismatch", []ResourceDiff{liveEntry("apps", "StatefulSet", "default", "web", deployLiveJSON)}, []string{deployJSON}, ErrInvalidArgument, "kind mismatch: expected StatefulSet, got Deployment"},
		{"not a managed resource", []ResourceDiff{liveEntry("", "ConfigMap", "kube-system", "cluster-info", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cluster-info","namespace":"kube-system"}}`)}, []string{"null"}, ErrPermissionDenied, "/ConfigMap cluster-info not found as part of application guestbook"},
		{"bad target", []ResourceDiff{{}}, []string{"{not json"}, ErrInvalidArgument, "error unmarshaling target manifest 0"},
		{"bad live", []ResourceDiff{{Kind: "X", Name: "y", LiveState: "[1]"}}, []string{""}, ErrInvalidArgument, "error unmarshaling live state for X/y"},
		{"not aligned", []ResourceDiff{{}, {}}, []string{deployJSON}, ErrInvalidArgument, "must be index-aligned: got 2 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newFixture(t)
			ts := startServer(t, p, HandlerOptions{})
			_, err := ts.client.ServerSideDiff(ctx, application(app1), tc.live, tc.targets)
			require.ErrorIs(t, err, tc.target)
			assert.Contains(t, err.Error(), tc.message)
			assert.Empty(t, p.ssdCalls, "the dry run must not run for rejected input")
		})
	}
	t.Run("limits", func(t *testing.T) {
		t.Parallel()
		ts := startServer(t, newFixture(t), HandlerOptions{MaxDiffResources: 2, MaxRequestBytes: 4096})
		_, err := ts.client.ServerSideDiff(ctx, application(app1), make([]ResourceDiff, 3), make([]string, 3))
		require.ErrorIs(t, err, ErrTooLarge)
		_, err = ts.client.ServerSideDiff(ctx, application(app1), []ResourceDiff{{}}, []string{strings.Repeat(" ", 5000) + "null"})
		require.ErrorIs(t, err, ErrTooLarge)
		assert.Contains(t, err.Error(), "request body exceeds 4096 bytes")
	})
	t.Run("provider result count is checked", func(t *testing.T) {
		t.Parallel()
		p := newFixture(t)
		p.ssd = func(_, _ []*unstructured.Unstructured) ([]diff.Result, error) { return nil, nil }
		ts := startServer(t, p, HandlerOptions{})
		_, err := ts.client.ServerSideDiff(ctx, application(app1), []ResourceDiff{{}}, []string{deployJSON})
		require.ErrorIs(t, err, ErrInternal)
	})
	t.Run("content type", func(t *testing.T) {
		t.Parallel()
		ts := startServer(t, newFixture(t), HandlerOptions{})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.srv.URL+PathServerSideDiff, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestResource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newFixture(t)
	ts := startServer(t, p, HandlerOptions{})
	manifest, err := ts.client.Resource(ctx, application(app1), ResourceQuery{Group: "apps", Kind: "Deployment", ResourceNamespace: "default", ResourceName: "web", Version: "v1beta2"})
	require.NoError(t, err)
	assert.JSONEq(t, deployLiveJSON, manifest)
	require.Len(t, p.liveRefs, 1)
	assert.Equal(t, compare.ResourceRef{Group: "apps", Version: "v1beta2", Kind: "Deployment", Namespace: "default", Name: "web", UID: "u-web"}, p.liveRefs[0], "the requested version overrides the node's")

	manifest, err = ts.client.Resource(ctx, application(app1), ResourceQuery{Kind: "Secret", ResourceNamespace: "default", ResourceName: "creds"})
	require.NoError(t, err)
	assert.NotContains(t, manifest, "b2xkLXBhc3N3b3Jk")
	assert.NotContains(t, manifest, "tok-value")
	assert.Contains(t, manifest, `"password":"++++++++"`)

	_, err = ts.client.Resource(ctx, application(app1), ResourceQuery{Kind: "ConfigMap", ResourceNamespace: "kube-system", ResourceName: "cluster-info"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	assert.Contains(t, err.Error(), "ConfigMap  cluster-info not found as part of application guestbook")
	_, err = ts.client.Resource(ctx, application(app1), ResourceQuery{Kind: "ConfigMap", ResourceNamespace: "default", ResourceName: "missing"})
	require.ErrorIs(t, err, ErrInvalidArgument, "a node without UID (missing resource) has no live object")
	_, err = ts.client.Resource(ctx, application(app1), ResourceQuery{Kind: "Deployment"})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// mapRouter routes by Application name and records invalidations.
type mapRouter struct {
	mu          sync.Mutex
	shards      map[string]int
	urls        map[int]string
	invalidated []int
}

func (r *mapRouter) Shard(_ context.Context, app *v1alpha1.Application) (int, error) {
	return r.shards[app.Name], nil
}

func (r *mapRouter) Endpoint(_ context.Context, shard int) (string, error) {
	u, ok := r.urls[shard]
	if !ok {
		return "", fmt.Errorf("no replica for shard %d", shard)
	}
	return u, nil
}

func (r *mapRouter) Invalidate(shard int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidated = append(r.invalidated, shard)
}

// ownerProvider answers like a controller replica that owns some shards only.
type ownerProvider struct {
	*fakeProvider
	owns  bool
	shard int
	hits  int
	mu    sync.Mutex
}

func (o *ownerProvider) Tree(ctx context.Context, app AppKey) (*compare.ApplicationTree, error) {
	o.mu.Lock()
	o.hits++
	o.mu.Unlock()
	if !o.owns {
		return nil, &NotOwnedError{Shard: o.shard}
	}
	return o.fakeProvider.Tree(ctx, app)
}

func TestClientReroutesMisdirectedRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wrong := &ownerProvider{fakeProvider: newFixture(t), owns: false, shard: 1}
	right := &ownerProvider{fakeProvider: newFixture(t), owns: true}
	s0 := startServer(t, wrong, HandlerOptions{})
	s1 := startServer(t, right, HandlerOptions{})
	router := &mapRouter{shards: map[string]int{"guestbook": 0}, urls: map[int]string{0: s0.srv.URL, 1: s1.srv.URL}}
	c, err := NewClient(router, testToken)
	require.NoError(t, err)

	tree, err := c.Tree(ctx, application(app1))
	require.NoError(t, err)
	assert.Len(t, tree.Nodes, 3)
	assert.Equal(t, 1, wrong.hits)
	assert.Equal(t, 1, right.hits)
	assert.Equal(t, []int{0}, router.invalidated)

	// Both replicas disown it: one retry only, then the 421 is returned.
	right.owns, right.shard = false, 0
	_, err = c.Tree(ctx, application(app1))
	require.ErrorIs(t, err, ErrNotOwned)
	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.NotNil(t, apiErr.Shard)
	assert.Equal(t, 0, *apiErr.Shard)

	// A dead endpoint is retried once after invalidation, then reported as unavailable.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	router.urls[2] = dead.URL
	router.shards["dead"] = 2
	_, err = c.Tree(ctx, application(AppKey{Namespace: "apps", Name: "dead"}))
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, []int{0, 0, 2, 2}, router.invalidated, "first misdirect of each call and both dead attempts")
}

func TestClientResponseCap(t *testing.T) {
	t.Parallel()
	ts := startServer(t, newFixture(t), HandlerOptions{}, WithMaxResponseBytes(64))
	_, err := ts.client.Tree(context.Background(), application(app1))
	require.ErrorIs(t, err, ErrTooLarge)
}

type fixedShards int

func (f fixedShards) ShardForApplication(context.Context, *v1alpha1.Application) (int, error) {
	return int(f), nil
}

type fakeHolders struct {
	holder      string
	err         error
	invalidated []int
}

func (f *fakeHolders) ShardHolder(context.Context, int) (string, error) { return f.holder, f.err }
func (f *fakeHolders) Invalidate(shard int)                             { f.invalidated = append(f.invalidated, shard) }

func TestShardRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	app := application(app1)
	r := &ShardRouter{Shards: fixedShards(2), Namespace: "patrol"}
	shard, err := r.Shard(ctx, app)
	require.NoError(t, err)
	assert.Equal(t, 2, shard)
	u, err := r.Endpoint(ctx, shard)
	require.NoError(t, err)
	assert.Equal(t, "http://patrol-controller-2.patrol-controller.patrol.svc:8090", u)

	h := &fakeHolders{holder: "patrol-controller-0"}
	r = &ShardRouter{Shards: fixedShards(2), Holders: h, Namespace: "patrol", Service: "patrol-controller-headless", Port: 9443, Scheme: "https"}
	u, err = r.Endpoint(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, "https://patrol-controller-0.patrol-controller-headless.patrol.svc:9443", u, "the Lease holder serves a borrowed shard")
	r.Invalidate(2)
	assert.Equal(t, []int{2}, h.invalidated)

	h.holder = "Not A DNS Label"
	u, err = r.Endpoint(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "https://patrol-controller-1.patrol-controller-headless.patrol.svc:9443", u, "invalid holder identities are ignored")
	h.err, h.holder = errors.New("api down"), ""
	u, err = r.Endpoint(ctx, 1)
	require.NoError(t, err)
	assert.Contains(t, u, "patrol-controller-1.")

	_, err = (&ShardRouter{}).Endpoint(ctx, 0)
	require.Error(t, err)
	shard, err = (&ShardRouter{Namespace: "x"}).Shard(ctx, app)
	require.NoError(t, err)
	assert.Equal(t, 0, shard)
}

func TestErrorTypes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "application is served by another controller replica (shard 3)", (&NotOwnedError{Shard: 3}).Error())
	assert.ErrorIs(t, fmt.Errorf("wrap: %w", &NotOwnedError{Shard: 3}), ErrNotOwned)
	e := &Error{Status: 503, Code: CodeUnavailable, Message: "cache warming"}
	assert.ErrorIs(t, e, ErrUnavailable)
	assert.NotErrorIs(t, e, ErrNotFound)
	assert.Equal(t, "controller tree API: cache warming (503 unavailable)", e.Error())
	resp := &response{status: http.StatusBadGateway, header: http.Header{}, body: []byte("<html>")}
	got := decodeError(resp)
	assert.Equal(t, CodeUnavailable, got.Code)
}
