package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// CRDType ties a CRD kind to its Go types.
type CRDType struct {
	Kind     string
	Type     reflect.Type
	ListType reflect.Type
}

// PatrolCRDs are the CRDs whose Go types are referenced by API schemas.
func PatrolCRDs() []CRDType {
	return []CRDType{
		{"Application", reflect.TypeFor[v1alpha1.Application](), reflect.TypeFor[v1alpha1.ApplicationList]()},
		{"AppProject", reflect.TypeFor[v1alpha1.AppProject](), reflect.TypeFor[v1alpha1.AppProjectList]()},
		{"ApplicationSet", reflect.TypeFor[v1alpha1.ApplicationSet](), reflect.TypeFor[v1alpha1.ApplicationSetList]()},
		{"Repository", reflect.TypeFor[v1alpha1.Repository](), reflect.TypeFor[v1alpha1.RepositoryList]()},
		{"RepositoryCredential", reflect.TypeFor[v1alpha1.RepositoryCredential](), reflect.TypeFor[v1alpha1.RepositoryCredentialList]()},
		{"Cluster", reflect.TypeFor[v1alpha1.Cluster](), reflect.TypeFor[v1alpha1.ClusterList]()},
		{"SCMProvider", reflect.TypeFor[v1alpha1.SCMProvider](), reflect.TypeFor[v1alpha1.SCMProviderList]()},
		{"Operation", reflect.TypeFor[v1alpha1.Operation](), reflect.TypeFor[v1alpha1.OperationList]()},
	}
}

// CRDSchema is the structural schema of one CRD version.
type CRDSchema struct {
	Group   string
	Version string
	Kind    string
	Schema  map[string]any
}

type crdDocument struct {
	Kind string `json:"kind"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		Versions []struct {
			Name    string `json:"name"`
			Served  bool   `json:"served"`
			Storage bool   `json:"storage"`
			Schema  struct {
				OpenAPIV3Schema json.RawMessage `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// maxCRDFileBytes bounds one CRD manifest file.
const maxCRDFileBytes = 32 << 20

// LoadCRDSchemas reads every CustomResourceDefinition in the *.yaml files of dir and
// returns the openAPIV3Schema of each kind's storage version, keyed by kind.
func LoadCRDSchemas(dir string) (map[string]CRDSchema, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list CRD files: %w", err)
	}
	slices.Sort(files)
	out := map[string]CRDSchema{}
	for _, f := range files {
		data, err := readBounded(f, maxCRDFileBytes)
		if err != nil {
			return nil, err
		}
		docs, err := parseCRDs(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, d := range docs {
			if _, dup := out[d.Kind]; dup {
				return nil, fmt.Errorf("%s: CRD kind %s defined twice", f, d.Kind)
			}
			out[d.Kind] = d
		}
	}
	return out, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // paths come from the generator's configuration
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return data, nil
}

func parseCRDs(data []byte) ([]CRDSchema, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 64<<10)
	var out []CRDSchema
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
			continue
		}
		var doc crdDocument
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("decode document: %w", err)
		}
		if doc.Kind != "CustomResourceDefinition" {
			continue
		}
		for _, v := range doc.Spec.Versions {
			if !v.Storage {
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(v.Schema.OpenAPIV3Schema, &schema); err != nil {
				return nil, fmt.Errorf("%s: decode openAPIV3Schema: %w", doc.Spec.Names.Kind, err)
			}
			if schema == nil {
				return nil, fmt.Errorf("%s: version %s has no openAPIV3Schema", doc.Spec.Names.Kind, v.Name)
			}
			out = append(out, CRDSchema{Group: doc.Spec.Group, Version: v.Name, Kind: doc.Spec.Names.Kind, Schema: schema})
		}
	}
}

// escapePointer escapes a JSON pointer token (RFC 6901).
func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

var v1alpha1PkgPath = reflect.TypeFor[v1alpha1.Application]().PkgPath()

// crdWalker walks Go types and their CRD schemas in parallel. For every api/v1alpha1
// struct type with a structural (properties) schema node it records the JSON pointer of
// the first such node; API schemas then reference CRD-derived schemas instead of
// re-deriving them from Go types, so they cannot disagree with what the Kubernetes API
// server validates. Later nodes of the same type whose content equals the first one
// (ignoring their description) are replaced by a $ref to it, which removes the many
// copies controller-gen inlines (ApplicationSet embeds the Application template in
// every generator). Roots are walked in a fixed order, so the result is deterministic.
type crdWalker struct {
	refs   map[reflect.Type]string
	canon  map[reflect.Type][]byte
	isLeaf func(reflect.Type) bool
}

func (w *crdWalker) walk(t reflect.Type, node map[string]any, pointer string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if node == nil || w.isLeaf(t) {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		props, _ := node["properties"].(map[string]any)
		if props == nil {
			return
		}
		if t.PkgPath() == v1alpha1PkgPath && t.Name() != "" {
			if first, seen := w.refs[t]; seen {
				if first != pointer && bytes.Equal(canonical(node), w.canon[t]) {
					desc, hasDesc := node["description"]
					clear(node)
					node["$ref"] = first
					if hasDesc {
						node["description"] = desc
					}
					return
				}
			} else {
				w.refs[t] = pointer
				w.canon[t] = canonical(node)
			}
		}
		w.fields(t, props, pointer)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return
		}
		items, _ := node["items"].(map[string]any)
		w.walk(t.Elem(), items, pointer+"/items")
	case reflect.Map:
		ap, _ := node["additionalProperties"].(map[string]any)
		w.walk(t.Elem(), ap, pointer+"/additionalProperties")
	}
}

// canonical is the JSON encoding of node without its description (map keys sorted).
func canonical(node map[string]any) []byte {
	c := make(map[string]any, len(node))
	for k, v := range node {
		if k != "description" {
			c[k] = v
		}
	}
	b, _ := json.Marshal(c)
	return b
}

func (w *crdWalker) fields(t reflect.Type, props map[string]any, pointer string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && (name == "" || strings.Contains(opts, "inline")) {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				w.fields(ft, props, pointer)
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		child, _ := props[name].(map[string]any)
		w.walk(f.Type, child, pointer+"/properties/"+escapePointer(name))
	}
}
