package compare

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// obj decodes a YAML or JSON manifest.
func obj(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	data, err := yaml.YAMLToJSON([]byte(manifest))
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, u.UnmarshalJSON(data))
	return u
}

// toJSON encodes an object as a manifest string, as the repo-server returns it.
func toJSON(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	b, err := json.Marshal(u.Object)
	require.NoError(t, err)
	return string(b)
}

func configMap(name, namespace string, data map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
	}}
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	if data != nil {
		d := map[string]any{}
		for k, v := range data {
			d[k] = v
		}
		u.Object["data"] = d
	}
	return u
}
