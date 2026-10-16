package applications

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/openapi"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// contractRoutes are the application routes of api-contract.md owned by this package.
var contractRoutes = []string{
	"GET /api/v1/applications",
	"GET /api/v1/stream/applications",
	"POST /api/v1/applications",
	"GET /api/v1/applications/{name}",
	"PUT /api/v1/applications/{name}",
	"PUT /api/v1/applications/{name}/spec",
	"PATCH /api/v1/applications/{name}",
	"DELETE /api/v1/applications/{name}",
	"POST /api/v1/applications/{name}/sync",
	"POST /api/v1/applications/{name}/rollback",
	"DELETE /api/v1/applications/{name}/operation",
	"GET /api/v1/applications/{name}/operations",
	"GET /api/v1/applications/{name}/operations/{op}",
	"GET /api/v1/applications/{name}/syncwindows",
	"GET /api/v1/applications/{name}/revisions/{revision}/metadata",
	"GET /api/v1/applications/{name}/revisions/{revision}/chartdetails",
	"GET /api/v1/applications/{name}/revisions/{revision}/ocimetadata",
	"GET /api/v1/applications/{name}/manifests",
	"POST /api/v1/applications/{name}/manifests-with-files",
	"GET /api/v1/applications/{name}/links",
	"GET /api/v1/applications/{name}/resource/links",
}

func TestRouteTable(t *testing.T) {
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	got := map[string]rest.Route{}
	for _, r := range tbl.Routes() {
		got[r.Pattern()] = r
	}
	// The resource-level routes are checked by TestResourceRouteTable (resource_internal_test.go).
	require.Len(t, got, len(contractRoutes)+len(resourceContractRoutes))
	for _, p := range contractRoutes {
		r, ok := got[p]
		require.True(t, ok, "missing route %s", p)
		assert.Equal(t, rest.AccessRBAC, r.Access.Mode, p)
		assert.True(t, r.Access.InHandler, p)
		assert.Equal(t, rbac.ResourceApplications, r.Access.Resource, p)
		assert.Equal(t, "<project>/<app>", r.Access.Object, p)
	}
	actions := map[string]string{
		"POST /api/v1/applications":                      rbac.ActionCreate,
		"PUT /api/v1/applications/{name}":                rbac.ActionUpdate,
		"PATCH /api/v1/applications/{name}":              rbac.ActionUpdate,
		"DELETE /api/v1/applications/{name}":             rbac.ActionDelete,
		"POST /api/v1/applications/{name}/sync":          rbac.ActionSync,
		"POST /api/v1/applications/{name}/rollback":      rbac.ActionSync,
		"DELETE /api/v1/applications/{name}/operation":   rbac.ActionSync,
		"GET /api/v1/applications/{name}/manifests":      rbac.ActionGet,
		"GET /api/v1/applications/{name}/resource/links": rbac.ActionGet,
	}
	for p, a := range actions {
		assert.Equal(t, a, got[p].Access.Action, p)
	}
	assert.True(t, got["GET /api/v1/stream/applications"].Stream)
	assert.Equal(t, int64(syncMaxBodyBytes), got["POST /api/v1/applications/{name}/sync"].MaxBodyBytes)
	assert.Equal(t, DefaultMaxLocalFilesBytes, got["POST /api/v1/applications/{name}/manifests-with-files"].MaxBodyBytes)
}

// Without collaborators every route answers 503, never panics.
func TestRoutesWithoutDependencies(t *testing.T) {
	tbl := rest.NewTable()
	New(Deps{Config: Config{Namespace: "patrol"}}).Register(tbl)
	h, err := tbl.Handler(&rest.Env{DisableAuth: true})
	require.NoError(t, err)
	for _, p := range contractRoutes {
		method, path, _ := strings.Cut(p, " ")
		path = strings.NewReplacer("{name}", "app", "{op}", "op", "{revision}", "abc").Replace(path)
		var body *strings.Reader
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			body = strings.NewReader(`{}`)
		default:
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "manifests-with-files") {
			req.Header.Set("Content-Type", "application/gzip")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.HasPrefix(path, "/api/v1/stream/") {
			assert.Contains(t, rec.Body.String(), `"status":503`, p)
			continue
		}
		assert.Contains(t, []int{http.StatusServiceUnavailable, http.StatusBadRequest}, rec.Code, "%s: %s", p, rec.Body.String())
	}
}

func TestNamespaceMatcher(t *testing.T) {
	m, warnings := newNamespaceMatcher("patrol", []string{"team-*", "/^ns-(a|b)$/", " ", "/[/", "[", "exact"})
	assert.Len(t, warnings, 2)
	assert.True(t, m.clusterWide())
	for ns, want := range map[string]bool{
		"patrol": true, "team-a": true, "team-": true, "ns-a": true, "ns-b": true, "ns-c": false,
		"exact": true, "exactly": false, "other": false, "": false,
	} {
		assert.Equal(t, want, m.allowed(ns), ns)
	}
	none, _ := newNamespaceMatcher("patrol", nil)
	assert.False(t, none.clusterWide())
	assert.False(t, none.allowed("team-a"))
}

func TestPropagationFinalizer(t *testing.T) {
	for in, want := range map[string]string{
		"":           v1alpha1.FinalizerResources,
		"foreground": v1alpha1.FinalizerResourcesForeground,
		"Foreground": v1alpha1.FinalizerResourcesForeground,
		"BACKGROUND": v1alpha1.FinalizerResourcesBackground,
		"orphan":     "",
	} {
		assert.Equal(t, want, propagationFinalizer(in), in)
	}
}

func TestNewerRV(t *testing.T) {
	assert.True(t, newerRV("10", "9"))
	assert.False(t, newerRV("9", "10"))
	assert.False(t, newerRV("9", "9"))
	assert.True(t, newerRV("b", "a"))
	assert.False(t, newerRV("a", "a"))
}

func TestMaskManifest(t *testing.T) {
	hide := map[string]bool{"token": true}
	cases := []struct {
		name            string
		in              string
		absent, absent2 string
		contains        []string
		unchanged       bool
	}{
		{name: "json secret", in: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"k":"c2VjcmV0"}}`,
			absent: "c2VjcmV0", contains: []string{"++++++++"}},
		{name: "yaml secret with stringData and annotation", in: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  annotations:\n    token: abc\nstringData:\n  k: plain\n",
			absent: "plain", absent2: "abc", contains: []string{"++++++++"}},
		{name: "multi-document", in: "kind: ConfigMap\napiVersion: v1\nmetadata: {name: c}\ndata: {a: visible}\n---\nkind: Secret\napiVersion: v1\nmetadata: {name: s}\ndata: {k: c2VjcmV0}\n",
			absent: "c2VjcmV0", contains: []string{"visible", "++++++++"}},
		{name: "not a core secret", in: `{"apiVersion":"example.com/v1","kind":"Secret","data":{"k":"x"}}`, unchanged: true},
		{name: "configmap", in: `{"apiVersion":"v1","kind":"ConfigMap","data":{"k":"x"}}`, unchanged: true},
		{name: "unparsable", in: "kind: Secret\ndata: [unclosed", absent: "unclosed", contains: []string{"redacted"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := maskManifest(tc.in, hide)
			if tc.unchanged {
				assert.Equal(t, tc.in, out)
				return
			}
			if tc.absent != "" {
				assert.NotContains(t, out, tc.absent)
			}
			if tc.absent2 != "" {
				assert.NotContains(t, out, tc.absent2)
			}
			for _, c := range tc.contains {
				assert.Contains(t, out, c)
			}
		})
	}
}

func TestApplyAppPatch(t *testing.T) {
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "n"},
		Spec: v1alpha1.ApplicationSpec{Project: "p", Source: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "x"}}}
	out, err := applyAppPatch(app, `[{"op":"replace","path":"/spec/source/path","value":"y"}]`, "")
	require.NoError(t, err)
	assert.Equal(t, "y", out.Spec.Source.Path)
	out, err = applyAppPatch(app, `{"spec":{"project":"q"}}`, pubapi.PatchTypeMerge)
	require.NoError(t, err)
	assert.Equal(t, "q", out.Spec.Project)
	assert.Equal(t, "x", app.Spec.Source.Path, "input not modified")
	_, err = applyAppPatch(app, `nope`, pubapi.PatchTypeJSON)
	require.ErrorContains(t, err, "error decoding json patch")
	_, err = applyAppPatch(app, `nope`, pubapi.PatchTypeMerge)
	require.ErrorContains(t, err, "error calculating merge patch")
	_, err = applyAppPatch(app, `{"spec":{"project":5}}`, pubapi.PatchTypeMerge)
	require.ErrorContains(t, err, "error unmarshaling patched app")
}

// TestApplyAppPatchLimits: each "copy" of a node into a new member of itself doubles the
// document, so a body of under 1 KiB could exhaust memory without the copy bound.
func TestApplyAppPatchLimits(t *testing.T) {
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "n"},
		Spec: v1alpha1.ApplicationSpec{Project: "p", Source: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "x"}}}
	selfCopies := func(n int) string {
		ops := make([]string, n)
		for i := range ops {
			ops[i] = fmt.Sprintf(`{"op":"copy","from":"/spec","path":"/spec/x%d"}`, i)
		}
		return "[" + strings.Join(ops, ",") + "]"
	}
	repeatOp := func(n int, op string) string { return "[" + strings.TrimSuffix(strings.Repeat(op+",", n), ",") + "]" }

	for _, tc := range []struct {
		name, patch, typ, detail string
	}{
		{"exponential copies", selfCopies(64), "", "the accumulated size increase of copy"},
		{"too many operations", repeatOp(maxPatchOperations+1, `{"op":"test","path":"/spec/project","value":"p"}`), pubapi.PatchTypeJSON,
			"the allowed maximum operations in a JSON patch is 1000, got 1001"},
		{"oversized merge result", `{"metadata":{"annotations":{"big":"` + strings.Repeat("x", int(maxPatchedBytes)) + `"}}}`, pubapi.PatchTypeMerge,
			"the patched application is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applyAppPatch(app, tc.patch, tc.typ)
			var re *rest.Error
			require.ErrorAs(t, err, &re)
			assert.Equal(t, http.StatusRequestEntityTooLarge, re.Status)
			assert.Equal(t, pubapi.CodeResourceExhausted, re.Code)
			assert.Contains(t, re.Detail, tc.detail)
		})
	}

	// Within the bounds: a few copies (unknown members are dropped on decoding), and
	// exactly maxPatchOperations operations.
	out, err := applyAppPatch(app, selfCopies(3), "")
	require.NoError(t, err)
	assert.Equal(t, app.Spec, out.Spec)
	out, err = applyAppPatch(app, repeatOp(maxPatchOperations, `{"op":"replace","path":"/spec/source/path","value":"y"}`), "")
	require.NoError(t, err)
	assert.Equal(t, "y", out.Spec.Source.Path)

	// A rejected exponential patch allocates a bounded amount (unbounded, 40 copies would
	// need about 2^40 times the spec).
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = applyAppPatch(app, selfCopies(40), "")
	require.Error(t, err)
	runtime.ReadMemStats(&after)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20), "bytes allocated by a rejected exponential patch")
}

func TestRetryValidation(t *testing.T) {
	f := func(n int64) *int64 { return &n }
	assert.NoError(t, validateRetry(nil))
	assert.NoError(t, validateRetry(&v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "10", MaxDuration: "3m", Factor: f(2)}}))
	assert.ErrorContains(t, validateRetry(&v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "-1"}}), "must not be negative")
	assert.ErrorContains(t, validateRetry(&v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{MaxDuration: "-1s"}}), "is not a duration")
	assert.ErrorContains(t, validateRetry(&v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Factor: f(0)}}), "factor must be at least 1")
}

func TestSourceFor(t *testing.T) {
	src := func(p string) v1alpha1.ApplicationSource { return v1alpha1.ApplicationSource{RepoURL: "r", Path: p} }
	single := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "s"}}}
	single.Status.History = v1alpha1.RevisionHistories{{ID: 4, Sources: v1alpha1.ApplicationSources{src("h0"), src("h1")}}, {ID: 5}}
	i32 := func(v int32) *int32 { return &v }
	got, err := sourceFor(single, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "s", got.Path)
	got, err = sourceFor(single, i32(1), i32(4))
	require.NoError(t, err)
	assert.Equal(t, "h1", got.Path)
	_, err = sourceFor(single, i32(2), i32(4))
	require.ErrorContains(t, err, "source index 2 not found because there are only 2 sources")
	_, err = sourceFor(single, nil, i32(5))
	require.ErrorContains(t, err, "revision history 5 has no source")
	_, err = sourceFor(single, i32(-1), nil)
	require.ErrorContains(t, err, "out of range")
	_, err = sourceFor(&v1alpha1.Application{}, nil, i32(1))
	require.ErrorContains(t, err, "the app has no history")
}

func TestBroadcaster(t *testing.T) {
	b := NewBroadcaster(2)
	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	assert.Equal(t, 2, b.Subscribers())
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	b.OnAdd(app, true)
	b.OnUpdate(app, app)
	ev := <-ch1
	assert.Equal(t, pubapi.EventAdded, ev.Type)
	assert.NotSame(t, app, ev.Application, "published objects are copies")
	// ch2 is full (2 events); one more disconnects it, ch1 has room.
	<-ch1
	b.OnDelete(cache.DeletedFinalStateUnknown{Key: "x/a", Obj: app})
	ev = <-ch1
	assert.Equal(t, pubapi.EventDeleted, ev.Type)
	<-ch2
	<-ch2
	_, open := <-ch2
	assert.False(t, open, "slow subscriber disconnected")
	assert.Equal(t, 1, b.Subscribers())
	cancel2() // idempotent after disconnect
	cancel1()
	cancel1()
	_, open = <-ch1
	assert.False(t, open)
	assert.Equal(t, 0, b.Subscribers())
	b.OnAdd("not an application", false) // ignored
}

func TestHelpers(t *testing.T) {
	assert.True(t, jsonEqual(map[string]string(nil), map[string]string{}))
	assert.True(t, jsonEqual([]string(nil), []string{}))
	assert.False(t, jsonEqual(map[string]string{"a": "b"}, nil))
	assert.Equal(t, "https://github.com/x", redactURL("https://user:pass@github.com/x"))
	assert.Equal(t, "git@github.com:x", redactURL("git@github.com:x"))
	v, err := effectiveValue("ns", "", "b")
	require.NoError(t, err)
	assert.Equal(t, "b", v)
	v, err = effectiveValue("ns", "a", "")
	require.NoError(t, err)
	assert.Equal(t, "a", v)
	_, err = effectiveValue("ns", "a", "b")
	require.Error(t, err)
	assert.True(t, commitSHA.MatchString(strings.Repeat("a", 40)))
	assert.False(t, commitSHA.MatchString("main"))
	op := newOperation(&v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("x", 253), Namespace: "n", UID: "u"}}, "sync", v1alpha1.OperationSpec{})
	assert.LessOrEqual(t, len(op.GenerateName), 253-5)
	assert.Empty(t, op.Labels, "names longer than a label value get no label")
}

// The routes produce a valid OpenAPI document (CRD bodies as schema references).
func TestOpenAPIGeneration(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	crds, err := openapi.LoadCRDSchemas(filepath.Join(root, "deploy", "crds"))
	require.NoError(t, err)
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	doc, err := openapi.Generate(tbl.Routes(), openapi.Options{Title: "t", Version: "v1", CRDs: crds,
		CommentDirs: map[string]string{"github.com/juliosaraiva/patrol-cd/pkg/apiclient": filepath.Join(root, "pkg", "apiclient")}})
	require.NoError(t, err)
	for _, id := range []string{"syncApplication", "watchApplications", "getApplicationManifestsWithFiles", "listApplicationOperations"} {
		assert.Contains(t, string(doc), `"operationId": "`+id+`"`)
	}
}

func TestConfigFromParams(t *testing.T) {
	p, issues := settings.ResolveParams(settings.ComponentServer, map[string]string{
		ParamApplicationNamespaces: "team-*, /^x$/",
		ParamSyncReplaceAllowed:    "false",
	}, func(string) (string, bool) { return "", false })
	require.Empty(t, issues)
	c := ConfigFromParams("patrol", p)
	assert.Equal(t, "patrol", c.Namespace)
	assert.Equal(t, []string{"team-*", "/^x$/"}, c.ApplicationNamespaces)
	assert.True(t, c.DisableSyncWithReplace)
	d, _ := settings.ResolveParams(settings.ComponentServer, nil, func(string) (string, bool) { return "", false })
	assert.False(t, ConfigFromParams("patrol", d).DisableSyncWithReplace)
	assert.Empty(t, ConfigFromParams("patrol", nil).ApplicationNamespaces)
}
