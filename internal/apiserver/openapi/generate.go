// Package openapi generates the OpenAPI 3.1 document of the public API
// (api/openapi/openapi.json) from the route table of internal/apiserver/rest.
//
// Request and response schemas are reflected from the routes' Go types with
// github.com/invopop/jsonschema. Kubernetes leaf types (metav1.Time, intstr.IntOrString,
// resource.Quantity, runtime.RawExtension, apiextensionsv1.JSON,
// unstructured.Unstructured, ...) are mapped to their wire schemas. CRD types are never
// reflected: the CRD roots (Application, AppProject, ...) are the openAPIV3Schema of
// deploy/crds/*.yaml, and every api/v1alpha1 struct reachable from a CRD root is a JSON
// pointer reference into that schema, so the API document describes exactly what the
// Kubernetes API server validates.
//
// The CRD schemas leave metadata optional because they also describe request bodies.
// Objects the server returns always carry metadata, so response schemas (operation
// responses, stream events and every type reflected for them, plus the CRD list types)
// reference a "Stored<Kind>" schema instead: the CRD schema with metadata required.
//
// Server-Sent Event routes document the data of their default events as the
// text/event-stream schema and list the named events (message, error) in the
// x-patrol-events extension of that media type.
//
// Output is deterministic: object keys are sorted and nothing depends on map iteration
// or the environment.
package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Options configures Generate.
type Options struct {
	Title       string
	Version     string
	Description string
	// CRDs are the CRD schemas by kind (LoadCRDSchemas). Every kind of PatrolCRDs must be
	// present.
	CRDs map[string]CRDSchema
	// CommentDirs maps Go import paths to source directories whose doc comments become
	// schema and parameter descriptions.
	CommentDirs map[string]string
}

const (
	schemasPrefix = "#/components/schemas/"
	defsPrefix    = "#/$defs/"
	problemRef    = "#/components/responses/Problem"
	// StoredPrefix prefixes the response-side schema name of a CRD kind
	// ("StoredApplication"): the CRD schema with metadata required.
	StoredPrefix = "Stored"
)

type generator struct {
	opts     Options
	schemas  map[string]any
	names    map[string]reflect.Type
	crdRefs  map[reflect.Type]string
	lists    map[reflect.Type]string
	comments map[string]string
	errs     []error
	// storedRefs maps CRD root types to their Stored<Kind> schema, used while response
	// schemas are reflected (response is set).
	storedRefs map[reflect.Type]string
	response   bool
	// problem is the schema reference of apiclient.Problem.
	problem any
}

// enumer is implemented by string types that list their values (apiclient.ErrorCode).
type enumer interface{ JSONSchemaEnum() []any }

var enumerType = reflect.TypeFor[enumer]()

// Generate returns the OpenAPI 3.1 document for routes as indented JSON.
func Generate(routes []rest.Route, opts Options) ([]byte, error) {
	g := &generator{
		opts:       opts,
		schemas:    map[string]any{},
		names:      map[string]reflect.Type{},
		crdRefs:    map[reflect.Type]string{},
		lists:      map[reflect.Type]string{},
		comments:   map[string]string{},
		storedRefs: map[reflect.Type]string{},
	}
	importPaths := make([]string, 0, len(opts.CommentDirs))
	for ip := range opts.CommentDirs {
		importPaths = append(importPaths, ip)
	}
	slices.Sort(importPaths)
	for _, ip := range importPaths {
		if err := goComments(ip, opts.CommentDirs[ip], g.comments); err != nil {
			return nil, err
		}
	}
	if err := g.addCRDs(); err != nil {
		return nil, err
	}
	problem := g.schemaFor(reflect.TypeFor[apiclient.Problem]())
	g.problem = problem

	paths := map[string]any{}
	tagSet := map[string]bool{}
	for i := range routes {
		rt := &routes[i]
		if rt.Hidden || rt.Method == "" {
			continue
		}
		op, err := g.operation(rt)
		if err != nil {
			g.errs = append(g.errs, fmt.Errorf("route %s: %w", rt.Pattern(), err))
			continue
		}
		p := openAPIPath(rt.Path)
		item, _ := paths[p].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[p] = item
		}
		item[strings.ToLower(rt.Method)] = op
		for _, t := range rt.Tags {
			tagSet[t] = true
		}
	}
	if len(g.errs) > 0 {
		return nil, errors.Join(g.errs...)
	}
	tags := make([]any, 0, len(tagSet))
	for _, t := range slices.Sorted(maps.Keys(tagSet)) {
		tags = append(tags, map[string]any{"name": t})
	}
	doc := map[string]any{
		"openapi":           "3.1.0",
		"jsonSchemaDialect": "https://spec.openapis.org/oas/3.1/dialect/base",
		"info": map[string]any{
			"title":       opts.Title,
			"version":     opts.Version,
			"description": opts.Description,
			"license":     map[string]any{"name": "Apache-2.0", "identifier": "Apache-2.0"},
		},
		"servers": []any{map[string]any{"url": "/", "description": "The server root (plus --rootpath when served under a sub-path)."}},
		"tags":    tags,
		"paths":   paths,
		"security": []any{
			map[string]any{"bearerAuth": []any{}},
			map[string]any{"cookieAuth": []any{}},
		},
		"components": map[string]any{
			"schemas": g.schemas,
			"responses": map[string]any{
				"Problem": map[string]any{
					"description": "Error (RFC 9457 problem details).",
					"content":     map[string]any{apiclient.ProblemContentType: map[string]any{"schema": problem}},
				},
			},
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT",
					"description": "Patrol session, account API or project-role token (or an identity provider ID token)."},
				"cookieAuth": map[string]any{"type": "apiKey", "in": "cookie", "name": "patrol.token",
					"description": "Browser session cookie; state-changing requests must also send X-Requested-With: patrol."},
			},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode OpenAPI document: %w", err)
	}
	return buf.Bytes(), nil
}

// openAPIPath converts a ServeMux pattern path to an OpenAPI path template.
func openAPIPath(p string) string { return strings.ReplaceAll(p, "...}", "}") }

func (g *generator) addCRDs() error {
	objectMeta := g.schemaFor(reflect.TypeFor[metav1.ObjectMeta]())
	listMeta := g.schemaFor(reflect.TypeFor[metav1.ListMeta]())
	w := &crdWalker{refs: g.crdRefs, canon: map[reflect.Type][]byte{}, isLeaf: isLeafType}
	for _, c := range PatrolCRDs() {
		crd, ok := g.opts.CRDs[c.Kind]
		if !ok {
			return fmt.Errorf("CRD schema for kind %s not found", c.Kind)
		}
		schema, ok := deepCopyJSON(crd.Schema).(map[string]any)
		if !ok {
			return fmt.Errorf("CRD %s: schema is not an object", c.Kind)
		}
		if props, ok := schema["properties"].(map[string]any); ok {
			props["metadata"] = objectMeta
			if av, ok := props["apiVersion"].(map[string]any); ok {
				av["default"] = crd.Group + "/" + crd.Version
			}
		}
		schema["x-kubernetes-group-version-kind"] = []any{map[string]any{"group": crd.Group, "version": crd.Version, "kind": c.Kind}}
		if err := g.claim(c.Kind, c.Type); err != nil {
			return err
		}
		g.schemas[c.Kind] = schema
		w.walk(c.Type, schema, schemasPrefix+escapePointer(c.Kind))
		g.crdRefs[c.Type] = schemasPrefix + c.Kind

		// Objects returned by the server always carry metadata; request bodies keep the
		// CRD schema, where it is optional.
		storedName := StoredPrefix + c.Kind
		if err := g.claim(storedName, c.Type); err != nil {
			return err
		}
		g.schemas[storedName] = map[string]any{
			"description": c.Kind + " as returned by the server (a stored object, or one the server rendered): metadata is always present. " +
				"Request bodies use " + c.Kind + ", whose metadata is optional.",
			"allOf":    []any{map[string]any{"$ref": schemasPrefix + c.Kind}},
			"required": []any{"metadata"},
		}
		g.storedRefs[c.Type] = schemasPrefix + storedName

		listName := c.Kind + "List"
		if err := g.claim(listName, c.ListType); err != nil {
			return err
		}
		g.schemas[listName] = map[string]any{
			"type":        "object",
			"description": c.Kind + " list.",
			"required":    []any{"items"},
			"properties": map[string]any{
				"apiVersion": map[string]any{"type": "string"},
				"kind":       map[string]any{"type": "string"},
				"metadata":   listMeta,
				"items":      map[string]any{"type": "array", "items": map[string]any{"$ref": schemasPrefix + storedName}},
			},
		}
		g.lists[c.ListType] = schemasPrefix + listName
	}
	return nil
}

func (g *generator) claim(name string, t reflect.Type) error {
	if prev, ok := g.names[name]; ok && prev != t {
		return fmt.Errorf("schema name %q used by both %s and %s", name, prev, t)
	}
	g.names[name] = t
	return nil
}

// Leaf types with fixed wire schemas.
var leafSchemas = map[reflect.Type]func() *jsonschema.Schema{
	reflect.TypeFor[metav1.Time]():      func() *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Format: "date-time"} },
	reflect.TypeFor[metav1.MicroTime](): func() *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Format: "date-time"} },
	reflect.TypeFor[metav1.Duration](): func() *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", Description: "Go duration, e.g. 1h30m."}
	},
	reflect.TypeFor[intstr.IntOrString](): func() *jsonschema.Schema {
		return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "integer"}, {Type: "string"}},
			Extras: map[string]any{"x-kubernetes-int-or-string": true}}
	},
	reflect.TypeFor[resource.Quantity](): func() *jsonschema.Schema {
		return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "integer"}, {Type: "string"}},
			Pattern: `^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`,
			Extras:  map[string]any{"x-kubernetes-int-or-string": true}}
	},
	reflect.TypeFor[runtime.RawExtension](): func() *jsonschema.Schema {
		return &jsonschema.Schema{Type: "object", Extras: map[string]any{"x-kubernetes-preserve-unknown-fields": true}}
	},
	reflect.TypeFor[apiextensionsv1.JSON](): func() *jsonschema.Schema {
		return &jsonschema.Schema{Extras: map[string]any{"x-kubernetes-preserve-unknown-fields": true}}
	},
	reflect.TypeFor[unstructured.Unstructured](): func() *jsonschema.Schema {
		return &jsonschema.Schema{Type: "object", Description: "A Kubernetes object.",
			Extras: map[string]any{"x-kubernetes-preserve-unknown-fields": true}}
	},
	reflect.TypeFor[unstructured.UnstructuredList](): func() *jsonschema.Schema {
		return &jsonschema.Schema{Type: "object", Description: "A list of Kubernetes objects.",
			Extras: map[string]any{"x-kubernetes-preserve-unknown-fields": true}}
	},
	reflect.TypeFor[metav1.FieldsV1](): func() *jsonschema.Schema { return &jsonschema.Schema{Type: "object"} },
	reflect.TypeFor[json.RawMessage](): func() *jsonschema.Schema { return &jsonschema.Schema{} },
}

func isLeafType(t reflect.Type) bool {
	_, ok := leafSchemas[t]
	return ok
}

func (g *generator) mapper(t reflect.Type) *jsonschema.Schema {
	if g.response {
		if ref, ok := g.storedRefs[t]; ok {
			return &jsonschema.Schema{Ref: ref}
		}
	}
	if ref, ok := g.crdRefs[t]; ok {
		return &jsonschema.Schema{Ref: ref}
	}
	if ref, ok := g.lists[t]; ok {
		return &jsonschema.Schema{Ref: ref}
	}
	if f, ok := leafSchemas[t]; ok {
		return f()
	}
	if t.Kind() == reflect.String && t.Implements(enumerType) {
		vals := reflect.Zero(t).Interface().(enumer).JSONSchemaEnum()
		return &jsonschema.Schema{Type: "string", Enum: vals}
	}
	return nil
}

var qualifierRE = regexp.MustCompile(`[A-Za-z0-9_./-]*\.`)

func (g *generator) namer(t reflect.Type) string {
	name := t.Name()
	if strings.ContainsAny(name, "[]") {
		name = qualifierRE.ReplaceAllString(name, "")
		name = strings.NewReplacer("[", "_", "]", "", ",", "_", "*", "", " ", "").Replace(name)
	}
	if name == "" {
		return ""
	}
	if err := g.claim(name, t); err != nil {
		g.errs = append(g.errs, err)
	}
	return name
}

func (g *generator) lookupComment(t reflect.Type, field string) string {
	key := t.PkgPath() + "." + t.Name()
	if field != "" {
		key += "." + field
	}
	return g.comments[key]
}

func (g *generator) reflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{
		Anonymous:                 true,
		AllowAdditionalProperties: true,
		Mapper:                    g.mapper,
		Namer:                     g.namer,
		LookupComment:             g.lookupComment,
	}
}

// schemaFor returns an inline schema (usually a $ref) for t, adding the definitions it
// needs to the components.
func (g *generator) schemaFor(t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s := g.mapper(t); s != nil {
		return toJSON(s, &g.errs)
	}
	root := g.reflector().ReflectFromType(t)
	defs := root.Definitions
	root.Definitions = nil
	root.Version = ""
	root.ID = ""
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		def := rewriteRefs(toJSON(defs[name], &g.errs))
		if prev, ok := g.schemas[name]; ok {
			if !reflect.DeepEqual(prev, def) {
				g.errs = append(g.errs, fmt.Errorf("schema %q generated twice with different content "+
					"(a type holding a CRD object used in both a request and a response?)", name))
			}
			continue
		}
		g.schemas[name] = def
	}
	return rewriteRefs(toJSON(root, &g.errs))
}

// responseSchemaFor is schemaFor for response bodies and stream events: CRD objects are
// their Stored<Kind> schemas (metadata required).
func (g *generator) responseSchemaFor(t reflect.Type) any {
	g.response = true
	defer func() { g.response = false }()
	return g.schemaFor(t)
}

// streamEvents documents the named Server-Sent Events of a stream whose default events
// carry data (rest.ServeSSE and the log streams).
func (g *generator) streamEvents(data any) map[string]any {
	return map[string]any{
		"message": map[string]any{
			"description": "Default (unnamed) event: the data field is one JSON document of the media type's schema.",
			"schema":      deepCopyJSON(data),
		},
		"error": map[string]any{
			"description": "Ends a failed stream (session no longer valid, access lost, client too slow, server error): the data field is a problem document.",
			"schema":      deepCopyJSON(g.problem),
		},
	}
}

func toJSON(v any, errs *[]error) any {
	b, err := json.Marshal(v)
	if err != nil {
		*errs = append(*errs, err)
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		*errs = append(*errs, err)
		return map[string]any{}
	}
	return out
}

func rewriteRefs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "$ref" {
				if s, ok := val.(string); ok && strings.HasPrefix(s, defsPrefix) {
					x[k] = schemasPrefix + strings.TrimPrefix(s, defsPrefix)
				}
				continue
			}
			x[k] = rewriteRefs(val)
		}
	case []any:
		for i := range x {
			x[i] = rewriteRefs(x[i])
		}
	}
	return v
}

func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopyJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCopyJSON(x[i])
		}
		return out
	}
	return v
}

func (g *generator) operation(rt *rest.Route) (map[string]any, error) {
	op := map[string]any{
		"operationId": rt.OperationID,
		"responses":   map[string]any{"default": map[string]any{"$ref": problemRef}},
	}
	if rt.Summary != "" {
		op["summary"] = rt.Summary
	}
	if rt.Description != "" {
		op["description"] = rt.Description
	}
	if len(rt.Tags) > 0 {
		tags := make([]any, len(rt.Tags))
		for i, t := range rt.Tags {
			tags[i] = t
		}
		op["tags"] = tags
	}
	if rt.Deprecated {
		op["deprecated"] = true
	}
	g.access(rt, op)

	var params []any
	if rt.Request != nil {
		b, err := rest.BindingOf(rt.Request)
		if err != nil {
			return nil, err
		}
		for _, name := range rt.PathParams() {
			t := b.PathParams[name]
			p := map[string]any{"name": name, "in": "path", "required": true, "schema": scalarSchema(t)}
			if strings.Contains(rt.Path, "{"+name+"...}") {
				p["description"] = "May contain slashes."
				p["x-patrol-multi-segment"] = true
			}
			if d := g.fieldComment(rt.Request, "path", name); d != "" {
				p["description"] = strings.TrimSpace(d + " " + stringOr(p["description"]))
			}
			params = append(params, p)
		}
		qs := slices.Clone(b.QueryParams)
		sort.Slice(qs, func(i, j int) bool { return qs[i].Name < qs[j].Name })
		for _, q := range qs {
			var schema any
			if q.Multi {
				schema = map[string]any{"type": "array", "items": scalarSchema(q.Type.Elem())}
			} else {
				schema = scalarSchema(q.Type)
			}
			p := map[string]any{"name": q.Name, "in": "query", "schema": schema}
			if q.Multi {
				p["explode"] = true
				p["style"] = "form"
			}
			if d := g.fieldComment(rt.Request, "query", q.Name); d != "" {
				p["description"] = d
			}
			params = append(params, p)
		}
		if b.BodyType != nil {
			var schema any
			if b.BodyIsStruct {
				schema = g.schemaFor(rt.Request)
			} else {
				schema = g.schemaFor(b.BodyType)
			}
			ct := rt.RequestContentType
			if ct == "" {
				ct = "application/json"
			}
			op["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{ct: map[string]any{"schema": schema}},
			}
		}
	} else {
		for _, name := range rt.PathParams() {
			p := map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}}
			if strings.Contains(rt.Path, "{"+name+"...}") {
				p["description"] = "May contain slashes."
				p["x-patrol-multi-segment"] = true
			}
			params = append(params, p)
		}
	}
	if _, typed := op["requestBody"]; !typed && rt.RequestContentType != "" {
		// A body the handler reads itself (an upload, a proxied request): its content is
		// not JSON bound to the request type, so the media type carries no schema (any
		// bytes, OpenAPI 3.1 binary content).
		body := map[string]any{
			"required": !rt.RequestBodyOptional,
			"content":  map[string]any{rt.RequestContentType: map[string]any{}},
		}
		if rt.RequestDescription != "" {
			body["description"] = rt.RequestDescription
		}
		op["requestBody"] = body
	}
	if len(params) > 0 {
		op["parameters"] = params
	}

	responses := op["responses"].(map[string]any)
	status := rt.SuccessStatus
	if status == 0 {
		status = http.StatusOK
	}
	ok := map[string]any{"description": http.StatusText(status)}
	switch {
	case rt.Stream:
		ok["description"] = "Server-Sent Events: one JSON document per data field; heartbeat comments every 15 s; an \"error\" event with a problem document ends a failed stream (x-patrol-events)."
		data := any(map[string]any{})
		if rt.Response != nil {
			data = g.responseSchemaFor(rt.Response)
		}
		ok["content"] = map[string]any{"text/event-stream": map[string]any{
			"schema":          data,
			"x-patrol-events": g.streamEvents(data),
		}}
		op["x-patrol-stream"] = true
	case rt.Response != nil:
		ct := rt.ResponseContentType
		if ct == "" {
			ct = "application/json"
		}
		ok["content"] = map[string]any{ct: map[string]any{"schema": g.responseSchemaFor(rt.Response)}}
	case rt.ResponseContentType != "":
		ok["content"] = map[string]any{rt.ResponseContentType: map[string]any{"schema": map[string]any{"type": "string"}}}
	}
	if !rt.Stream && rt.Response == nil && len(rt.Responses) > 0 {
		// Raw routes that only document explicit responses (e.g. redirects).
		ok = nil
	}
	if ok != nil {
		responses[fmt.Sprint(status)] = ok
	}
	for _, rd := range rt.Responses {
		resp := map[string]any{"description": rd.Description}
		if rd.Description == "" {
			resp["description"] = http.StatusText(rd.Status)
		}
		if rd.Type != nil {
			ct := rd.ContentType
			if ct == "" {
				ct = "application/json"
			}
			resp["content"] = map[string]any{ct: map[string]any{"schema": g.responseSchemaFor(rd.Type)}}
		} else if rd.ContentType != "" {
			resp["content"] = map[string]any{rd.ContentType: map[string]any{"schema": map[string]any{"type": "string"}}}
		}
		if len(rd.Headers) > 0 {
			hs := map[string]any{}
			for _, h := range rd.Headers {
				hs[h] = map[string]any{"schema": map[string]any{"type": "string"}}
			}
			resp["headers"] = hs
		}
		key := fmt.Sprint(rd.Status)
		if prev, found := responses[key].(map[string]any); found {
			// Another representation of the same status (e.g. the download variant of a
			// stream): the media types and headers are merged.
			if err := mergeResponse(prev, resp); err != nil {
				return nil, fmt.Errorf("response %s: %w", key, err)
			}
			continue
		}
		responses[key] = resp
	}
	if len(rt.Extensions) > 0 {
		for _, k := range slices.Sorted(maps.Keys(rt.Extensions)) {
			if _, taken := op[k]; taken {
				return nil, fmt.Errorf("extension %s collides with a generated field", k)
			}
			op[k] = toJSON(rt.Extensions[k], &g.errs)
		}
	}
	return op, nil
}

// mergeResponse adds the media types, headers and description of add to prev, which
// documents the same status.
func mergeResponse(prev, add map[string]any) error {
	for _, field := range []string{"content", "headers"} {
		extra, _ := add[field].(map[string]any)
		if len(extra) == 0 {
			continue
		}
		cur, _ := prev[field].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
			prev[field] = cur
		}
		for k, v := range extra {
			if _, dup := cur[k]; dup {
				return fmt.Errorf("%s %q documented twice", field, k)
			}
			cur[k] = v
		}
	}
	if d := stringOr(add["description"]); d != "" {
		prev["description"] = strings.TrimSpace(stringOr(prev["description"]) + " " + d)
	}
	return nil
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

func (g *generator) access(rt *rest.Route, op map[string]any) {
	a := rt.Access
	op["x-patrol-access"] = a.Mode.String()
	switch {
	case rt.SkipAuth:
		// Patrol credentials are ignored entirely (not even verified).
		op["security"] = []any{}
	case a.Mode == rest.AccessPublic:
		op["security"] = []any{
			map[string]any{},
			map[string]any{"bearerAuth": []any{}},
			map[string]any{"cookieAuth": []any{}},
		}
	}
	if a.Mode == rest.AccessRBAC {
		r := map[string]any{"resource": a.Resource, "action": a.Action, "object": a.Object, "enforcedBy": "middleware"}
		if a.InHandler {
			r["enforcedBy"] = "handler"
		}
		if a.Note != "" {
			r["note"] = a.Note
		}
		op["x-patrol-rbac"] = r
	} else if a.Note != "" {
		op["x-patrol-access-note"] = a.Note
	}
	if rt.RateLimited {
		op["x-patrol-rate-limited"] = true
	}
}

// fieldComment returns the doc comment of the request field bound to a parameter.
func (g *generator) fieldComment(t reflect.Type, tag, name string) string {
	for _, f := range reflect.VisibleFields(t) {
		if f.Tag.Get(tag) == name {
			return g.comments[t.PkgPath()+"."+t.Name()+"."+f.Name]
		}
	}
	return ""
}

func scalarSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return map[string]any{"type": "integer", "format": "int32"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	if t.Kind() == reflect.String && t.Implements(enumerType) {
		return map[string]any{"type": "string", "enum": reflect.Zero(t).Interface().(enumer).JSONSchemaEnum()}
	}
	return map[string]any{"type": "string"}
}
