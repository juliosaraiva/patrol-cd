package openapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/openapi"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const crdDir = "../../../deploy/crds"

// Leaves exercises the leaf type mapper.
type Leaves struct {
	When     metav1.Time                `json:"when"`
	Micro    metav1.MicroTime           `json:"micro,omitempty"`
	Every    metav1.Duration            `json:"every,omitempty"`
	Port     intstr.IntOrString         `json:"port"`
	Quantity resource.Quantity          `json:"quantity"`
	Raw      runtime.RawExtension       `json:"raw,omitempty"`
	JSON     *apiextensionsv1.JSON      `json:"json,omitempty"`
	Object   *unstructured.Unstructured `json:"object,omitempty"`
	Message  json.RawMessage            `json:"message,omitempty"`
	Code     apiclient.ErrorCode        `json:"code,omitempty"`
}

// AppRequest addresses an application.
type AppRequest struct {
	// Name is the application name.
	Name string `path:"name" json:"-"`
	// Refresh forces a refresh.
	Refresh string   `query:"refresh" json:"-"`
	Fields  []string `query:"fields" json:"-"`
}

// SpecRequest carries a spec body.
type SpecRequest struct {
	Name string                    `path:"name" json:"-"`
	Spec *v1alpha1.ApplicationSpec `body:"true" json:"-"`
}

// CreateAppRequest carries an Application body.
type CreateAppRequest struct {
	App *v1alpha1.Application `body:"true" json:"-"`
}

// UploadRequest addresses an upload whose body the handler reads itself.
type UploadRequest struct {
	Name string `path:"name" json:"-"`
}

// ProxyRequest addresses a proxied path.
type ProxyRequest struct {
	Path string `path:"path" json:"-"`
}

// AppHolder holds a CRD object.
type AppHolder struct {
	App v1alpha1.Application `json:"app"`
}

func routes(t *testing.T) []rest.Route {
	t.Helper()
	tbl := rest.NewTable()
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/applications/{name}", OperationID: "getApplication",
		Summary: "Get", Tags: []string{"applications"}, Access: rest.RBACInHandler(rbac.ResourceApplications, rbac.ActionGet, "<project>/{name}")},
		func(context.Context, *AppRequest) (*v1alpha1.Application, error) { return nil, nil })
	rest.Register(tbl, rest.Route{Method: http.MethodPut, Path: "/api/v1/applications/{name}/spec", OperationID: "updateSpec",
		Tags: []string{"applications"}, Access: rest.RBACPathValue(rbac.ResourceApplications, rbac.ActionUpdate, "name")},
		func(context.Context, *SpecRequest) (*v1alpha1.ApplicationSpec, error) { return nil, nil })
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/leaves", OperationID: "leaves", Tags: []string{"misc"}, Access: rest.Public(), RateLimited: true},
		func(context.Context, *apiclient.Empty) (*Leaves, error) { return nil, nil })
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/apps", OperationID: "listApps", Tags: []string{"applications"}, Access: rest.Authenticated()},
		func(context.Context, *apiclient.Empty) (*v1alpha1.ApplicationList, error) { return nil, nil })
	rest.RegisterStream(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/stream/apps", OperationID: "watchApps", Tags: []string{"applications"}, Access: rest.Authenticated()},
		rest.StreamOptions[apiclient.WatchEvent[v1alpha1.Application]]{},
		func(context.Context, *apiclient.Empty, *rest.Sink[apiclient.WatchEvent[v1alpha1.Application]]) error {
			return nil
		})
	tbl.Add(rest.Route{Method: http.MethodGet, Path: "/api/v1/files/{path...}", OperationID: "files", Access: rest.Public(),
		ResponseContentType: "application/octet-stream", Handler: http.NotFoundHandler()})
	rest.Register(tbl, rest.Route{Method: http.MethodPost, Path: "/api/v1/session", OperationID: "createSession", Access: rest.Public()},
		func(context.Context, *apiclient.SessionCreateRequest) (*apiclient.SessionResponse, error) {
			return nil, nil
		})
	tbl.Add(rest.Route{Path: "/hidden/", OperationID: "hidden", Access: rest.Public(), Hidden: true, Handler: http.NotFoundHandler()})
	rest.Register(tbl, rest.Route{Method: http.MethodPost, Path: "/api/v1/apps", OperationID: "createApp", Access: rest.Authenticated()},
		func(context.Context, *CreateAppRequest) (*v1alpha1.Application, error) { return nil, nil })
	tbl.Add(rest.Route{Method: http.MethodPost, Path: "/api/v1/apps/{name}/upload", OperationID: "upload", Access: rest.Authenticated(),
		Request: reflect.TypeFor[UploadRequest](), Response: reflect.TypeFor[apiclient.Empty](),
		RequestContentType: "application/gzip", RequestDescription: "Gzipped tar.", Handler: http.NotFoundHandler()})
	tbl.Add(rest.Route{Method: http.MethodPut, Path: "/proxy/{path...}", OperationID: "proxy", Access: rest.Authenticated(),
		Request: reflect.TypeFor[ProxyRequest](), RequestContentType: "*/*", RequestBodyOptional: true, ResponseContentType: "*/*",
		Handler: http.NotFoundHandler()})
	tbl.Add(rest.Route{Method: http.MethodGet, Path: "/api/v1/logs", OperationID: "logs", Access: rest.Authenticated(),
		Response: reflect.TypeFor[apiclient.LogEntry](), Stream: true, Handler: http.NotFoundHandler(),
		Responses: []rest.ResponseDoc{{Status: http.StatusOK, Description: "With download=true: a text attachment.", ContentType: "application/octet-stream"}}})
	rest.RegisterStream(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/stream/appsets", OperationID: "watchAppSets", Access: rest.Authenticated()},
		rest.StreamOptions[apiclient.ApplicationSetWatchEvent]{},
		func(context.Context, *apiclient.Empty, *rest.Sink[apiclient.ApplicationSetWatchEvent]) error {
			return nil
		})
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/api/v1/terminal", OperationID: "terminal", Access: rest.Authenticated()},
		func(context.Context, *apiclient.Empty) (*apiclient.TerminalMessage, error) { return nil, nil })
	tbl.Add(rest.Route{Method: http.MethodPost, Path: "/hook", OperationID: "hook", Access: rest.Public(), SkipAuth: true, DocumentationOnly: true,
		RequestContentType: "application/json", Extensions: map[string]any{"x-receivers": []map[string]any{{"name": "github", "headers": []string{"X-GitHub-Event"}}}},
		Responses: []rest.ResponseDoc{{Status: http.StatusOK, Description: "Accepted."}}})
	require.NoError(t, tbl.Err())
	return tbl.Routes()
}

func generate(t *testing.T) map[string]any {
	t.Helper()
	crds, err := openapi.LoadCRDSchemas(crdDir)
	require.NoError(t, err)
	doc, err := openapi.Generate(routes(t), openapi.Options{Title: "T", Version: "v1", CRDs: crds,
		CommentDirs: map[string]string{"github.com/juliosaraiva/patrol-cd/pkg/apiclient": "../../../pkg/apiclient"}})
	require.NoError(t, err)
	again, err := openapi.Generate(routes(t), openapi.Options{Title: "T", Version: "v1", CRDs: crds,
		CommentDirs: map[string]string{"github.com/juliosaraiva/patrol-cd/pkg/apiclient": "../../../pkg/apiclient"}})
	require.NoError(t, err)
	require.Equal(t, string(doc), string(again), "deterministic output")
	var out map[string]any
	require.NoError(t, json.Unmarshal(doc, &out))
	return out
}

func get(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		require.True(t, ok, "at %q in %v", p, path)
		v, ok = m[p]
		require.True(t, ok, "missing %q in %v", p, path)
	}
	return v
}

func TestLoadCRDSchemas(t *testing.T) {
	t.Parallel()
	crds, err := openapi.LoadCRDSchemas(crdDir)
	require.NoError(t, err)
	for _, c := range openapi.PatrolCRDs() {
		s, ok := crds[c.Kind]
		require.True(t, ok, c.Kind)
		assert.Equal(t, "cd.patrol.example", s.Group)
		assert.Equal(t, "v1alpha1", s.Version)
		assert.Contains(t, s.Schema, "properties")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("kind: CustomResourceDefinition\nspec: [\n"), 0o600))
	_, err = openapi.LoadCRDSchemas(dir)
	require.Error(t, err)
}

func TestGenerateDocument(t *testing.T) {
	t.Parallel()
	doc := generate(t)
	assert.Equal(t, "3.1.0", doc["openapi"])
	paths := get(t, doc, "paths").(map[string]any)
	assert.NotContains(t, paths, "/hidden/")
	assert.Contains(t, paths, "/api/v1/files/{path}")

	op := get(t, doc, "paths", "/api/v1/applications/{name}", "get").(map[string]any)
	assert.Equal(t, "getApplication", op["operationId"])
	assert.Equal(t, map[string]any{"resource": "applications", "action": "get", "object": "<project>/{name}", "enforcedBy": "handler"}, op["x-patrol-rbac"])
	params := op["parameters"].([]any)
	require.Len(t, params, 3)
	assert.Equal(t, "name", get(t, params[0], "name"))
	assert.Equal(t, true, get(t, params[0], "required"))
	assert.Equal(t, "fields", get(t, params[1], "name"), "query parameters sorted by name")
	assert.Equal(t, "array", get(t, params[1], "schema", "type"))
	assert.Equal(t, "#/components/schemas/StoredApplication", get(t, op, "responses", "200", "content", "application/json", "schema", "$ref"),
		"responses return stored objects (metadata required)")
	assert.Equal(t, "#/components/responses/Problem", get(t, op, "responses", "default", "$ref"))

	put := get(t, doc, "paths", "/api/v1/applications/{name}/spec", "put").(map[string]any)
	assert.Equal(t, "#/components/schemas/Application/properties/spec",
		get(t, put, "requestBody", "content", "application/json", "schema", "$ref"), "CRD sub-types reference the CRD schema")
	assert.Equal(t, "middleware", get(t, put, "x-patrol-rbac", "enforcedBy"))

	session := get(t, doc, "paths", "/api/v1/session", "post").(map[string]any)
	assert.Equal(t, "Cookie also sets the patrol.token session cookie (browser login).", get(t, session["parameters"].([]any)[0], "description"),
		"doc comments of request fields describe parameters")
	assert.Equal(t, "SessionCreateRequest is the body of POST /api/v1/session (local login).",
		get(t, doc, "components", "schemas", "SessionCreateRequest", "description"))
	assert.NotContains(t, get(t, doc, "components", "schemas", "SessionCreateRequest", "properties"), "cookie", "query fields are not body properties")

	leaves := get(t, doc, "paths", "/api/v1/leaves", "get").(map[string]any)
	assert.Equal(t, true, leaves["x-patrol-rate-limited"])
	assert.Equal(t, "public", leaves["x-patrol-access"])
	props := get(t, doc, "components", "schemas", "Leaves", "properties").(map[string]any)
	assert.Equal(t, map[string]any{"type": "string", "format": "date-time"}, props["when"])
	assert.Equal(t, "date-time", get(t, props, "micro", "format"))
	assert.Equal(t, "string", get(t, props, "every", "type"))
	assert.Equal(t, true, get(t, props, "port", "x-kubernetes-int-or-string"))
	assert.Equal(t, true, get(t, props, "quantity", "x-kubernetes-int-or-string"))
	assert.Equal(t, true, get(t, props, "raw", "x-kubernetes-preserve-unknown-fields"))
	assert.Equal(t, true, get(t, props, "json", "x-kubernetes-preserve-unknown-fields"))
	assert.Equal(t, true, get(t, props, "object", "x-kubernetes-preserve-unknown-fields"))
	assert.Len(t, get(t, props, "code", "enum"), len(apiclient.ErrorCodes()))

	list := get(t, doc, "paths", "/api/v1/apps", "get", "responses", "200", "content", "application/json", "schema", "$ref")
	assert.Equal(t, "#/components/schemas/ApplicationList", list)
	assert.Equal(t, "#/components/schemas/StoredApplication", get(t, doc, "components", "schemas", "ApplicationList", "properties", "items", "items", "$ref"))

	watch := get(t, doc, "paths", "/api/v1/stream/apps", "get").(map[string]any)
	assert.Equal(t, true, watch["x-patrol-stream"])
	ev := get(t, watch, "responses", "200", "content", "text/event-stream", "schema", "$ref").(string)
	evSchema := get(t, doc, "components", "schemas", strings.TrimPrefix(ev, "#/components/schemas/"))
	assert.Equal(t, "#/components/schemas/StoredApplication", get(t, evSchema, "properties", "object", "$ref"))
	watchTypes := []any{apiclient.EventAdded, apiclient.EventModified, apiclient.EventDeleted}
	assert.Equal(t, watchTypes, get(t, evSchema, "properties", "type", "enum"), "watch event types (apiclient.Event*)")
	assert.Equal(t, map[string]any{"$ref": ev}, get(t, watch, "responses", "200", "content", "text/event-stream", "x-patrol-events", "message", "schema"))
	assert.Equal(t, "#/components/schemas/Problem", get(t, watch, "responses", "200", "content", "text/event-stream", "x-patrol-events", "error", "schema", "$ref"))
	appSetEv := get(t, doc, "components", "schemas", "ApplicationSetWatchEvent")
	assert.Equal(t, watchTypes, get(t, appSetEv, "properties", "type", "enum"))
	assert.Equal(t, "#/components/schemas/StoredApplicationSet", get(t, appSetEv, "properties", "object", "$ref"))

	files := get(t, doc, "paths", "/api/v1/files/{path}", "get").(map[string]any)
	assert.Equal(t, true, get(t, files["parameters"].([]any)[0], "x-patrol-multi-segment"))
	assert.Contains(t, get(t, files, "responses", "200", "content").(map[string]any), "application/octet-stream")

	app := get(t, doc, "components", "schemas", "Application").(map[string]any)
	assert.Equal(t, "cd.patrol.example/v1alpha1", get(t, app, "properties", "apiVersion", "default"))
	assert.Equal(t, "#/components/schemas/ObjectMeta", get(t, app, "properties", "metadata", "$ref"))
	assert.NotContains(t, app["required"], "metadata", "the CRD schema (request bodies) leaves metadata optional")

	assertRefsResolve(t, doc)
}

// TestStoredObjects checks the response-side CRD schemas (UI-1): requests use the CRD
// schema, responses the Stored<Kind> schema with metadata required.
func TestStoredObjects(t *testing.T) {
	t.Parallel()
	doc := generate(t)
	for _, c := range openapi.PatrolCRDs() {
		stored := get(t, doc, "components", "schemas", openapi.StoredPrefix+c.Kind).(map[string]any)
		assert.Equal(t, []any{map[string]any{"$ref": "#/components/schemas/" + c.Kind}}, stored["allOf"], c.Kind)
		assert.Equal(t, []any{"metadata"}, stored["required"], c.Kind)
		assert.NotContains(t, stored, "type", "no own type: generated types intersect the CRD type")
		assert.Equal(t, "#/components/schemas/"+openapi.StoredPrefix+c.Kind,
			get(t, doc, "components", "schemas", c.Kind+"List", "properties", "items", "items", "$ref"), c.Kind)
	}
	create := get(t, doc, "paths", "/api/v1/apps", "post").(map[string]any)
	assert.Equal(t, "#/components/schemas/Application", get(t, create, "requestBody", "content", "application/json", "schema", "$ref"))
	assert.Equal(t, "#/components/schemas/StoredApplication", get(t, create, "responses", "200", "content", "application/json", "schema", "$ref"))

	crds, err := openapi.LoadCRDSchemas(crdDir)
	require.NoError(t, err)
	tbl := rest.NewTable()
	rest.Register(tbl, rest.Route{Method: http.MethodPost, Path: "/a", OperationID: "a", Access: rest.Public()},
		func(context.Context, *AppHolder) (*apiclient.Empty, error) { return nil, nil })
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/b", OperationID: "b", Access: rest.Public()},
		func(context.Context, *apiclient.Empty) (*AppHolder, error) { return nil, nil })
	require.NoError(t, tbl.Err())
	_, err = openapi.Generate(tbl.Routes(), openapi.Options{Title: "T", Version: "v1", CRDs: crds})
	require.ErrorContains(t, err, `schema "AppHolder" generated twice`, "a type holding a CRD object cannot be both a request and a response")
}

// TestRawBodiesStreamsAndExtensions covers raw request bodies (UI-3), stream documents
// with a second representation (UI-2), enums of terminal messages (UI-5) and
// documentation-only routes with extensions (UI-6).
func TestRawBodiesStreamsAndExtensions(t *testing.T) {
	t.Parallel()
	doc := generate(t)

	upload := get(t, doc, "paths", "/api/v1/apps/{name}/upload", "post").(map[string]any)
	assert.Equal(t, map[string]any{"required": true, "description": "Gzipped tar.", "content": map[string]any{"application/gzip": map[string]any{}}},
		upload["requestBody"], "binary body: media type without a schema")
	proxy := get(t, doc, "paths", "/proxy/{path}", "put").(map[string]any)
	assert.Equal(t, map[string]any{"required": false, "content": map[string]any{"*/*": map[string]any{}}}, proxy["requestBody"])
	assert.NotContains(t, get(t, doc, "paths", "/api/v1/applications/{name}", "get"), "requestBody")

	logs := get(t, doc, "paths", "/api/v1/logs", "get").(map[string]any)
	ok := get(t, logs, "responses", "200").(map[string]any)
	assert.Equal(t, "#/components/schemas/LogEntry", get(t, ok, "content", "text/event-stream", "schema", "$ref"))
	assert.Equal(t, "#/components/schemas/Problem", get(t, ok, "content", "text/event-stream", "x-patrol-events", "error", "schema", "$ref"))
	assert.Contains(t, ok["content"], "application/octet-stream", "the download representation is merged into the stream response")
	assert.True(t, strings.HasPrefix(ok["description"].(string), "Server-Sent Events"))
	assert.True(t, strings.HasSuffix(ok["description"].(string), "With download=true: a text attachment."))
	assert.Equal(t, true, logs["x-patrol-stream"])

	term := get(t, doc, "components", "schemas", "TerminalMessage").(map[string]any)
	assert.Equal(t, []any{apiclient.TerminalOpStdin, apiclient.TerminalOpStdout, apiclient.TerminalOpResize}, get(t, term, "properties", "operation", "enum"))
	assert.Equal(t, []any{"operation"}, term["required"], "data is optional (resize frames omit it)")

	hook := get(t, doc, "paths", "/hook", "post").(map[string]any)
	assert.Equal(t, []any{}, hook["security"], "SkipAuth routes ignore Patrol credentials")
	assert.Equal(t, []any{map[string]any{"name": "github", "headers": []any{"X-GitHub-Event"}}}, hook["x-receivers"])
	assert.Equal(t, map[string]any{"description": "Accepted."}, get(t, hook, "responses", "200"))

	crds, err := openapi.LoadCRDSchemas(crdDir)
	require.NoError(t, err)
	tbl := rest.NewTable()
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public(),
		Extensions: map[string]any{"x-patrol-access": "mine"}}, func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { return nil, nil })
	require.NoError(t, tbl.Err())
	_, err = openapi.Generate(tbl.Routes(), openapi.Options{Title: "T", Version: "v1", CRDs: crds})
	require.ErrorContains(t, err, "extension x-patrol-access collides")
}

// assertRefsResolve checks that every local $ref of doc points at an existing node.
func assertRefsResolve(t *testing.T, doc map[string]any) {
	t.Helper()
	var walk func(v any)
	n := 0
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				n++
				require.True(t, strings.HasPrefix(ref, "#/"), ref)
				var cur any = doc
				for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
					m, ok := cur.(map[string]any)
					require.True(t, ok, "unresolvable %s", ref)
					cur, ok = m[tok]
					require.True(t, ok, "unresolvable %s", ref)
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	assert.Positive(t, n)
}

func TestGenerateErrors(t *testing.T) {
	t.Parallel()
	_, err := openapi.Generate(routes(t), openapi.Options{CRDs: map[string]openapi.CRDSchema{}})
	require.ErrorContains(t, err, "CRD schema for kind Application not found")
}

// TestCheckedInDocumentRefsResolve validates the repository's OpenAPI document.
func TestCheckedInDocumentRefsResolve(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../../api/openapi/openapi.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	assertRefsResolve(t, doc)
	for p, item := range doc["paths"].(map[string]any) {
		for m, op := range item.(map[string]any) {
			o := op.(map[string]any)
			assert.NotEmpty(t, o["operationId"], "%s %s", m, p)
			assert.NotEmpty(t, o["x-patrol-access"], "%s %s documents its access rule", m, p)
		}
	}
}
