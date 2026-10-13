package diff

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/managedfields"
	openapiproto "k8s.io/kube-openapi/pkg/util/proto"
	"sigs.k8s.io/yaml"
)

// testingT is satisfied by *testing.T, *testing.B and *rapid.T.
type testingT interface {
	require.TestingT
	Helper()
}

// obj parses a YAML/JSON manifest into an unstructured object with the value types the
// Kubernetes client produces (int64 for integers).
func obj(t testingT, manifest string) *unstructured.Unstructured {
	t.Helper()
	data, err := yaml.YAMLToJSON([]byte(manifest))
	require.NoError(t, err)
	m, err := decodeObject(data)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: m}
}

// withLastApplied returns a copy of live annotated with config as last-applied-configuration.
func withLastApplied(t testingT, live, config *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	out := live.DeepCopy()
	data, err := json.Marshal(config.Object)
	require.NoError(t, err)
	ann := out.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationLastAppliedConfig] = string(data)
	out.SetAnnotations(ann)
	return out
}

func mustDiff(t testingT, config, live *unstructured.Unstructured, o Options) *Result {
	t.Helper()
	res, err := Diff(context.Background(), config, live, o)
	require.NoError(t, err)
	return res
}

func decoded(t testingT, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func nested(t testingT, data []byte, path ...string) (any, bool) {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(decoded(t, data), path...)
	require.NoError(t, err)
	return v, ok
}

var (
	parserOnce sync.Once
	parser     *managedfields.GvkParser
	parserErr  error
)

// testParser builds a GvkParser from testdata/openapi-v2.json (generated from the envtest
// kube-apiserver by internal/gen/openapifixture).
func testParser(t testing.TB) *managedfields.GvkParser {
	t.Helper()
	parserOnce.Do(func() {
		data, err := os.ReadFile("testdata/openapi-v2.json")
		if err != nil {
			parserErr = err
			return
		}
		doc, err := openapi_v2.ParseDocument(data)
		if err != nil {
			parserErr = err
			return
		}
		models, err := openapiproto.NewOpenAPIData(doc)
		if err != nil {
			parserErr = err
			return
		}
		parser, parserErr = managedfields.NewGVKParser(models, false)
	})
	require.NoError(t, parserErr)
	return parser
}

// parserFromSwagger builds a GvkParser from an inline swagger document.
func parserFromSwagger(t testing.TB, swagger string) *managedfields.GvkParser {
	t.Helper()
	doc, err := openapi_v2.ParseDocument([]byte(swagger))
	require.NoError(t, err)
	models, err := openapiproto.NewOpenAPIData(doc)
	require.NoError(t, err)
	p, err := managedfields.NewGVKParser(models, false)
	require.NoError(t, err)
	return p
}

func mustJSON(t testingT, u *unstructured.Unstructured) []byte {
	t.Helper()
	data, err := json.Marshal(u.Object)
	require.NoError(t, err)
	return data
}
