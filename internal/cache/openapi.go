package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/openapi"
	"k8s.io/kube-openapi/pkg/schemaconv"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
	smdschema "sigs.k8s.io/structured-merge-diff/v6/schema"
	"sigs.k8s.io/structured-merge-diff/v6/typed"
)

// GVKParser resolves structured-merge-diff types for group/version/kinds, built from the
// cluster's OpenAPI v3 schemas. It is immutable and safe for concurrent use.
type GVKParser struct {
	types     map[schema.GroupVersionKind]*typed.ParseableType
	ambiguous []schema.GroupVersionKind
}

// Type returns the parseable type of gvk, or nil when the cluster publishes no (or more than
// one conflicting) schema for it. Callers fall back to a deduced type for nil.
func (p *GVKParser) Type(gvk schema.GroupVersionKind) *typed.ParseableType {
	if p == nil {
		return nil
	}
	return p.types[gvk]
}

// Ambiguous lists GVKs published by more than one schema; they resolve to nil.
func (p *GVKParser) Ambiguous() []schema.GroupVersionKind {
	if p == nil {
		return nil
	}
	return slices.Clone(p.ambiguous)
}

// openAPICache lazily loads OpenAPI data. reset discards it; a load racing with reset does
// not store its (possibly stale) result.
type openAPICache struct {
	mu      sync.Mutex
	loadMu  sync.Mutex
	gen     uint64
	schemas map[string]*spec.Schema
	parser  *GVKParser
}

func (o *openAPICache) cached() (map[string]*spec.Schema, *GVKParser, uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.schemas, o.parser, o.gen
}

func (o *openAPICache) get(ctx context.Context, load func() (map[string]*spec.Schema, *GVKParser, error)) (map[string]*spec.Schema, *GVKParser, error) {
	if s, p, _ := o.cached(); p != nil {
		return s, p, nil
	}
	o.loadMu.Lock()
	defer o.loadMu.Unlock()
	s, p, gen := o.cached()
	if p != nil {
		return s, p, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s, p, err := load()
	if err != nil {
		return nil, nil, err
	}
	o.mu.Lock()
	if o.gen == gen {
		o.schemas, o.parser = s, p
	}
	o.mu.Unlock()
	return s, p, nil
}

func (o *openAPICache) reset() {
	o.mu.Lock()
	o.gen++
	o.schemas, o.parser = nil, nil
	o.mu.Unlock()
}

// loadOpenAPIV3 downloads every OpenAPI v3 group-version document and merges their
// component schemas.
func loadOpenAPIV3(client openapi.Client) (map[string]*spec.Schema, *GVKParser, error) {
	paths, err := client.Paths()
	if err != nil {
		return nil, nil, fmt.Errorf("list OpenAPI v3 paths: %w", err)
	}
	schemas := map[string]*spec.Schema{}
	for _, name := range slices.Sorted(maps.Keys(paths)) {
		raw, err := paths[name].Schema("application/json")
		if apierrors.IsNotFound(err) {
			continue // the group version disappeared after Paths (for example a deleted CRD)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("fetch OpenAPI v3 document %q: %w", name, err)
		}
		var doc spec3.OpenAPI
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, nil, fmt.Errorf("decode OpenAPI v3 document %q: %w", name, err)
		}
		if doc.Components != nil {
			maps.Copy(schemas, doc.Components.Schemas)
		}
	}
	p, err := newGVKParser(schemas)
	if err != nil {
		return nil, nil, err
	}
	return schemas, p, nil
}

// newGVKParser converts OpenAPI schemas to structured-merge-diff types and indexes them by
// the x-kubernetes-group-version-kind extension.
func newGVKParser(schemas map[string]*spec.Schema) (*GVKParser, error) {
	s, err := schemaconv.ToSchemaFromOpenAPI(schemas, false)
	if err != nil {
		return nil, fmt.Errorf("convert OpenAPI schemas: %w", err)
	}
	parser := &typed.Parser{Schema: smdschema.Schema{Types: s.Types}}
	owner := map[schema.GroupVersionKind]string{}
	p := &GVKParser{types: map[schema.GroupVersionKind]*typed.ParseableType{}}
	ambiguous := map[schema.GroupVersionKind]struct{}{}
	for _, name := range slices.Sorted(maps.Keys(schemas)) {
		sch := schemas[name]
		if sch == nil {
			continue
		}
		for _, gvk := range gvksOf(sch.Extensions) {
			if prev, ok := owner[gvk]; ok && prev != name {
				ambiguous[gvk] = struct{}{}
				continue
			}
			pt := parser.Type(name)
			if !pt.IsValid() {
				continue
			}
			owner[gvk] = name
			p.types[gvk] = &pt
		}
	}
	for gvk := range ambiguous {
		delete(p.types, gvk)
		p.ambiguous = append(p.ambiguous, gvk)
	}
	slices.SortFunc(p.ambiguous, func(a, b schema.GroupVersionKind) int {
		switch {
		case a.String() < b.String():
			return -1
		case a.String() > b.String():
			return 1
		}
		return 0
	})
	return p, nil
}

// gvksOf parses the x-kubernetes-group-version-kind extension.
func gvksOf(ext spec.Extensions) []schema.GroupVersionKind {
	raw, ok := ext["x-kubernetes-group-version-kind"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []schema.GroupVersionKind
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		g, _ := m["group"].(string)
		v, _ := m["version"].(string)
		k, _ := m["kind"].(string)
		if v != "" && k != "" {
			out = append(out, schema.GroupVersionKind{Group: g, Version: v, Kind: k})
		}
	}
	return out
}
