package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

type fakeGV struct {
	doc string
	err error
}

func (f fakeGV) Schema(string) ([]byte, error) { return []byte(f.doc), f.err }
func (f fakeGV) ServerRelativeURL() string     { return "" }

type fakeOpenAPI map[string]openapi.GroupVersion

func (f fakeOpenAPI) Paths() (map[string]openapi.GroupVersion, error) { return f, nil }

const appsDoc = `{
  "openapi": "3.0.0",
  "info": {"title": "Kubernetes", "version": "v1.36.0"},
  "paths": {},
  "components": {"schemas": {
    "io.k8s.api.apps.v1.Deployment": {
      "type": "object",
      "properties": {
        "apiVersion": {"type": "string"},
        "kind": {"type": "string"},
        "spec": {"type": "object", "properties": {"replicas": {"type": "integer", "format": "int32"}}}
      },
      "x-kubernetes-group-version-kind": [{"group": "apps", "kind": "Deployment", "version": "v1"}]
    },
    "io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "apps", "kind": "DeleteOptions", "version": "v1"}]
    }
  }}
}`

const dupDoc = `{
  "openapi": "3.0.0",
  "info": {"title": "Kubernetes", "version": "v1.36.0"},
  "paths": {},
  "components": {"schemas": {
    "com.example.v1.Deployment": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "apps", "kind": "Deployment", "version": "v1"}]
    },
    "com.example.v1.Widget": {
      "type": "object",
      "properties": {"spec": {"type": "object", "x-kubernetes-preserve-unknown-fields": true}},
      "x-kubernetes-group-version-kind": [{"group": "example.com", "kind": "Widget", "version": "v1"}]
    }
  }}
}`

func TestLoadOpenAPIV3(t *testing.T) {
	t.Parallel()
	deploy := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	widget := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	schemas, p, err := loadOpenAPIV3(fakeOpenAPI{"apis/apps/v1": fakeGV{doc: appsDoc}})
	require.NoError(t, err)
	assert.Contains(t, schemas, "io.k8s.api.apps.v1.Deployment")
	pt := p.Type(deploy)
	require.NotNil(t, pt)
	tv, err := pt.FromUnstructured(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "spec": map[string]any{"replicas": int64(2)}})
	require.NoError(t, err)
	assert.NotNil(t, tv)
	_, err = pt.FromUnstructured(map[string]any{"spec": map[string]any{"replicas": "two"}})
	assert.Error(t, err, "typed parsing validates field types")
	assert.Nil(t, p.Type(widget))

	// Conflicting schemas for one GVK make it ambiguous rather than picking one silently.
	_, p, err = loadOpenAPIV3(fakeOpenAPI{"apis/apps/v1": fakeGV{doc: appsDoc}, "apis/example.com/v1": fakeGV{doc: dupDoc}})
	require.NoError(t, err)
	assert.Nil(t, p.Type(deploy))
	assert.Equal(t, []schema.GroupVersionKind{deploy}, p.Ambiguous())
	assert.NotNil(t, p.Type(widget))

	_, _, err = loadOpenAPIV3(fakeOpenAPI{"apis/apps/v1": fakeGV{err: errors.New("boom")}})
	assert.ErrorContains(t, err, "boom")

	// A group version deleted between listing paths and fetching it is skipped.
	_, p, err = loadOpenAPIV3(fakeOpenAPI{"apis/apps/v1": fakeGV{doc: appsDoc},
		"apis/gone.example.com/v1": fakeGV{err: apierrors.NewNotFound(schema.GroupResource{}, "")}})
	require.NoError(t, err)
	assert.NotNil(t, p.Type(deploy))

	var nilParser *GVKParser
	assert.Nil(t, nilParser.Type(deploy))
}

func TestOpenAPICacheResetDiscardsRacingLoad(t *testing.T) {
	t.Parallel()
	var o openAPICache
	var loads atomic.Int32
	load := func() (map[string]*spec.Schema, *GVKParser, error) {
		loads.Add(1)
		return map[string]*spec.Schema{}, &GVKParser{}, nil
	}
	_, p1, err := o.get(context.Background(), load)
	require.NoError(t, err)
	_, p2, err := o.get(context.Background(), load)
	require.NoError(t, err)
	assert.Same(t, p1, p2)
	assert.EqualValues(t, 1, loads.Load(), "result is cached")

	o.reset()
	_, _, err = o.get(context.Background(), func() (map[string]*spec.Schema, *GVKParser, error) {
		o.reset() // invalidated while loading
		return load()
	})
	require.NoError(t, err)
	s, p, _ := o.cached()
	assert.Nil(t, s)
	assert.Nil(t, p, "a load racing with reset is not stored")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = o.get(ctx, load)
	assert.ErrorIs(t, err, context.Canceled)
}
