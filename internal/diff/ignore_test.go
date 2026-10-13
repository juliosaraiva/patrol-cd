package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
)

// logBuffer is a concurrency-safe slog sink for asserting on log output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newTestLogger() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

const ignoreDeployment = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels: {app: web, "node-role.kubernetes.io/worker": "true", "tilde~key": x}
spec:
  replicas: 3
  template:
    spec:
      initContainers:
      - {name: init-0, image: busybox}
      - {name: injected-init, image: istio/proxy}
      - {name: init-1, image: busybox}
      containers:
      - {name: web, image: nginx}
`

func normalized(t *testing.T, rules []IgnoreRule, manifest string) *unstructured.Unstructured {
	t.Helper()
	n, err := NewIgnoreNormalizer(rules, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	u := obj(t, manifest)
	require.NoError(t, n.Normalize(context.Background(), u))
	return u
}

func TestIgnoreNormalizerJSONPointers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pointer string
		check   func(t *testing.T, u *unstructured.Unstructured)
	}{
		{"map key", "/spec/replicas", func(t *testing.T, u *unstructured.Unstructured) {
			_, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas")
			assert.False(t, ok)
		}},
		{"array index", "/spec/template/spec/initContainers/1", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Equal(t, []string{"init-0", "init-1"}, initNames(t, u))
		}},
		{"negative array index", "/spec/template/spec/initContainers/-1", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Equal(t, []string{"init-0", "injected-init"}, initNames(t, u))
		}},
		{"escaped slash", "/metadata/labels/node-role.kubernetes.io~1worker", func(t *testing.T, u *unstructured.Unstructured) {
			assert.NotContains(t, u.GetLabels(), "node-role.kubernetes.io/worker")
		}},
		{"escaped tilde", "/metadata/labels/tilde~0key", func(t *testing.T, u *unstructured.Unstructured) {
			assert.NotContains(t, u.GetLabels(), "tilde~key")
		}},
		{"whole subtree", "/spec/template/spec/initContainers", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Empty(t, initNames(t, u))
		}},
		{"missing path is a no-op", "/spec/does/not/exist", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Equal(t, obj(t, ignoreDeployment), u)
		}},
		{"out of range index is a no-op", "/spec/template/spec/initContainers/7", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Len(t, initNames(t, u), 3)
		}},
		{"pointer into a scalar is a no-op", "/spec/replicas/x", func(t *testing.T, u *unstructured.Unstructured) {
			assert.Equal(t, obj(t, ignoreDeployment), u)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.check(t, normalized(t, []IgnoreRule{{Group: "apps", Kind: "Deployment", JSONPointers: []string{tc.pointer}}}, ignoreDeployment))
		})
	}
}

func initNames(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	items, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "initContainers")
	require.NoError(t, err)
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	return names
}

func TestIgnoreNormalizerMatching(t *testing.T) {
	t.Parallel()
	replicasRemoved := func(t *testing.T, r IgnoreRule) bool {
		t.Helper()
		r.JSONPointers = []string{"/spec/replicas"}
		u := normalized(t, []IgnoreRule{r}, ignoreDeployment)
		_, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas")
		return !ok
	}
	cases := []struct {
		name string
		rule IgnoreRule
		want bool
	}{
		{"exact group and kind", IgnoreRule{Group: "apps", Kind: "Deployment"}, true},
		{"wildcards", IgnoreRule{Group: "*", Kind: "*"}, true},
		{"glob kind", IgnoreRule{Group: "apps", Kind: "Deploy*"}, true},
		{"glob group", IgnoreRule{Group: "app?", Kind: "Deployment"}, true},
		{"other kind", IgnoreRule{Group: "apps", Kind: "StatefulSet"}, false},
		{"core group does not match apps", IgnoreRule{Group: "", Kind: "Deployment"}, false},
		{"name matches", IgnoreRule{Group: "*", Kind: "*", Name: "web"}, true},
		{"name differs", IgnoreRule{Group: "*", Kind: "*", Name: "api"}, false},
		{"namespace matches", IgnoreRule{Group: "*", Kind: "*", Namespace: "shop"}, true},
		{"namespace differs", IgnoreRule{Group: "*", Kind: "*", Namespace: "other"}, false},
		{"invalid glob never matches", IgnoreRule{Group: "[", Kind: "*"}, false},
		{"empty kind never matches", IgnoreRule{Group: "*"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, replicasRemoved(t, tc.rule))
		})
	}
}

func TestIgnoreNormalizerJQ(t *testing.T) {
	t.Parallel()

	t.Run("select list items by content", func(t *testing.T) {
		t.Parallel()
		u := normalized(t, []IgnoreRule{{Group: "apps", Kind: "Deployment",
			JQPathExpressions: []string{`.spec.template.spec.initContainers[] | select(.name == "injected-init")`}}}, ignoreDeployment)
		assert.Equal(t, []string{"init-0", "init-1"}, initNames(t, u))
	})
	t.Run("plain path", func(t *testing.T) {
		t.Parallel()
		u := normalized(t, []IgnoreRule{{Group: "*", Kind: "*", JQPathExpressions: []string{".spec.replicas"}}}, ignoreDeployment)
		_, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas")
		assert.False(t, ok)
		assert.Equal(t, "Deployment", u.GetKind(), "numbers and the rest of the object survive the jq round trip")
	})
	t.Run("optional iteration on missing field", func(t *testing.T) {
		t.Parallel()
		u := normalized(t, []IgnoreRule{{Group: "*", Kind: "*", JQPathExpressions: []string{".webhooks[]?.clientConfig.caBundle"}}}, ignoreDeployment)
		assert.Equal(t, obj(t, ignoreDeployment), u)
	})
	t.Run("runtime error leaves the object unchanged", func(t *testing.T) {
		t.Parallel()
		log, buf := newTestLogger()
		n, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "apps", Kind: "Deployment", JQPathExpressions: []string{".spec.fakeField.foo[]"}}}, IgnoreNormalizerOptions{Log: log})
		require.NoError(t, err)
		u := obj(t, ignoreDeployment)
		require.NoError(t, n.Normalize(context.Background(), u))
		assert.Equal(t, obj(t, ignoreDeployment), u)
		assert.Contains(t, buf.String(), "failed to apply normalization patch")
		assert.Contains(t, buf.String(), "kind=Deployment")
		assert.Contains(t, buf.String(), "name=web")
	})
	t.Run("invalid expression is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "apps", Kind: "Deployment",
			JQPathExpressions: []string{`.spec.template.spec.containers[] | select(.name == "missing-quote)`}}}, IgnoreNormalizerOptions{})
		require.ErrorIs(t, err, ErrInvalidIgnoreRule)
	})
	t.Run("expression is embedded verbatim like the baseline", func(t *testing.T) {
		t.Parallel()
		log, buf := newTestLogger()
		n, err := NewIgnoreNormalizer([]IgnoreRule{{Kind: "ConfigMap",
			JQPathExpressions: []string{`.nothing) | .data["config.yaml"] |= (fromjson | del(.auth) | tojson`}}}, IgnoreNormalizerOptions{Log: log})
		require.NoError(t, err)
		cm := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-configmap"},"data":{"config.yaml":"not: json"}}`)
		require.NoError(t, n.Normalize(context.Background(), cm))
		assert.Contains(t, buf.String(), "fromjson")
		assert.Contains(t, buf.String(), "name=my-configmap")

		good := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"config.yaml":"{\"auth\":\"x\",\"keep\":1}"}}`)
		require.NoError(t, n.Normalize(context.Background(), good))
		v, _, _ := unstructured.NestedString(good.Object, "data", "config.yaml")
		assert.JSONEq(t, `{"keep":1}`, v)
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		jp, err := compileJQPatch(`until(true==false; [.] + [1])`, 50*time.Millisecond)
		require.NoError(t, err)
		start := time.Now()
		_, err = jp.apply(context.Background(), obj(t, ignoreDeployment).Object)
		require.ErrorContains(t, err, "jq patch execution timed out")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("multiple results are rejected", func(t *testing.T) {
		t.Parallel()
		jp, err := compileJQPatch(`.a), (.b`, time.Second)
		require.NoError(t, err)
		_, err = jp.apply(context.Background(), map[string]any{"a": int64(1), "b": int64(2)})
		require.ErrorContains(t, err, "multiple")
	})
	t.Run("non-object result is rejected", func(t *testing.T) {
		t.Parallel()
		jp, err := compileJQPatch(`.a) | 1 | (.`, time.Second)
		require.NoError(t, err)
		_, err = jp.apply(context.Background(), map[string]any{"a": int64(1)})
		require.Error(t, err)
	})
	t.Run("large integers keep their precision", func(t *testing.T) {
		t.Parallel()
		u := normalized(t, []IgnoreRule{{Group: "*", Kind: "*", JQPathExpressions: []string{".spec.drop"}}},
			`{"apiVersion":"example.com/v1","kind":"Big","metadata":{"name":"b"},"spec":{"drop":1,"n":9007199254740993}}`)
		v, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "n")
		assert.Equal(t, int64(9007199254740993), v)
	})
}

// TestIgnoreNormalizerJQEnvironment is not parallel because it sets an environment variable.
func TestIgnoreNormalizerJQEnvironment(t *testing.T) {
	t.Setenv("PATROL_DIFF_TEST_SECRET", "s3cr3t")
	u := normalized(t, []IgnoreRule{{Group: "*", Kind: "*",
		JQPathExpressions: []string{`.metadata.labels[(env.PATROL_DIFF_TEST_SECRET // "unset")]`}}},
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","labels":{"unset":"1","s3cr3t":"1"}}}`)
	assert.Equal(t, map[string]string{"s3cr3t": "1"}, u.GetLabels(), "env must be empty inside jq expressions")
}

func TestIgnoreNormalizerSecretErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	log, buf := newTestLogger()
	n, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "", Kind: "Secret", JQPathExpressions: []string{".data.password[]"}}}, IgnoreNormalizerOptions{Log: log})
	require.NoError(t, err)
	s := obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"password":"aHVudGVyMg=="}}`)
	require.NoError(t, n.Normalize(context.Background(), s))
	out := buf.String()
	assert.Contains(t, out, "failed to apply normalization patch")
	assert.NotContains(t, out, "aHVudGVyMg==")
	assert.Contains(t, out, "redacted")
}

func TestIgnoreNormalizerOrderAndNil(t *testing.T) {
	t.Parallel()
	n, err := NewIgnoreNormalizer([]IgnoreRule{
		{Group: "*", Kind: "*", JSONPointers: []string{"/spec/template/spec/initContainers/0"}},
		{Group: "*", Kind: "*", JQPathExpressions: []string{".spec.template.spec.initContainers[0]"}},
	}, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	u := obj(t, ignoreDeployment)
	require.NoError(t, n.Normalize(context.Background(), u))
	assert.Equal(t, []string{"init-1"}, initNames(t, u), "rules apply in order")
	require.Error(t, n.Normalize(context.Background(), nil))
}

func TestTrustedManagers(t *testing.T) {
	t.Parallel()
	n, err := NewIgnoreNormalizer([]IgnoreRule{
		{Group: "*", Kind: "*", ManagedFieldsManagers: []string{"kube-controller-manager"}},
		{Group: "apps", Kind: "Deployment", ManagedFieldsManagers: []string{"hpa", "kube-controller-manager"}},
		{Group: "apps", Kind: "Deployment", Name: "other", ManagedFieldsManagers: []string{"never"}},
	}, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"kube-controller-manager", "hpa"}, n.TrustedManagers(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "web", "shop"))
	assert.Equal(t, []string{"kube-controller-manager"}, n.TrustedManagers(schema.GroupKind{Kind: "Service"}, "web", "shop"))

	chained := Chain(nil, n, NormalizerFunc(func(context.Context, *unstructured.Unstructured) error { return nil }))
	p, ok := chained.(TrustedManagersProvider)
	require.True(t, ok)
	assert.Equal(t, []string{"kube-controller-manager", "hpa"}, p.TrustedManagers(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "web", "shop"))

	failing := Chain(NormalizerFunc(func(context.Context, *unstructured.Unstructured) error { return errors.New("boom") }), n)
	require.Error(t, failing.Normalize(context.Background(), obj(t, ignoreDeployment)))
}

// TestPointerPatchMatchesJSONPatch checks pointerPatch against the JSON patch implementation
// whose semantics the baseline uses for jsonPointers (evanphx/json-patch, v4 API): same
// result document, a failure exactly when the library fails, and "missing path" failures
// (which are silent) classified the same way.
func TestPointerPatchMatchesJSONPatch(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		doc := genDoc(rt, 3)
		pointer := genPointer(rt, doc)
		docJSON, err := json.Marshal(doc)
		require.NoError(rt, err)

		patchJSON, err := json.Marshal([]map[string]string{{"op": "remove", "path": pointer}})
		require.NoError(rt, err)
		patch, err := jsonpatch.DecodePatch(patchJSON)
		require.NoError(rt, err)
		want, wantErr := patch.Apply(docJSON)

		cp, err := decodeObject(docJSON)
		require.NoError(rt, err)
		got, gotErr := pointerPatch{pointer: pointer}.apply(context.Background(), cp)

		if wantErr != nil {
			if gotErr == nil {
				rt.Fatalf("pointer %q: library failed (%v) but pointerPatch succeeded", pointer, wantErr)
			}
			assert.Equal(rt, errors.Is(wantErr, jsonpatch.ErrMissing), errors.Is(gotErr, errPointerMissing),
				"pointer %q: library error %v, ours %v", pointer, wantErr, gotErr)
			return
		}
		require.NoError(rt, gotErr, "pointer %q", pointer)
		gotJSON, err := json.Marshal(got)
		require.NoError(rt, err)
		assert.JSONEq(rt, string(want), string(gotJSON), "pointer %q", pointer)
	})
}

func genDoc(t *rapid.T, depth int) map[string]any {
	n := rapid.IntRange(0, 4).Draw(t, "keys")
	m := make(map[string]any, n)
	for range n {
		k := rapid.SampledFrom([]string{"a", "b", "c/d", "e~f", "0", "1", ""}).Draw(t, "key")
		m[k] = genValue(t, depth-1)
	}
	return m
}

func genValue(t *rapid.T, depth int) any {
	kinds := []string{"string", "int", "bool", "null"}
	if depth > 0 {
		kinds = append(kinds, "object", "array", "array")
	}
	switch rapid.SampledFrom(kinds).Draw(t, "kind") {
	case "object":
		return genDoc(t, depth)
	case "array":
		n := rapid.IntRange(0, 4).Draw(t, "len")
		a := make([]any, n)
		for i := range a {
			a[i] = genValue(t, depth-1)
		}
		return a
	case "int":
		return rapid.Int64Range(-5, 5).Draw(t, "int")
	case "bool":
		return rapid.Bool().Draw(t, "bool")
	case "null":
		return nil
	default:
		return rapid.StringMatching(`[a-z]{0,3}`).Draw(t, "s")
	}
}

// genPointer draws a pointer that usually walks existing structure and sometimes does not.
func genPointer(t *rapid.T, doc map[string]any) string {
	var cur any = doc
	steps := rapid.IntRange(1, 4).Draw(t, "steps")
	parts := make([]string, 0, steps)
	for range steps {
		var tok string
		switch c := cur.(type) {
		case map[string]any:
			keys := make([]string, 0, len(c)+1)
			keys = append(keys, "zz")
			for k := range c {
				keys = append(keys, k)
			}
			tok = rapid.SampledFrom(sortedStrings(keys)).Draw(t, "mapkey")
			cur = c[tok]
		case []any:
			idx := rapid.IntRange(-len(c)-1, len(c)+1).Draw(t, "idx")
			tok = strconv.Itoa(idx)
			if rapid.IntRange(0, 9).Draw(t, "weird") == 0 {
				tok = rapid.SampledFrom([]string{"-", "x", "01", "+1"}).Draw(t, "badidx")
			}
			if idx >= 0 && idx < len(c) {
				cur = c[idx]
			} else {
				cur = nil
			}
		default:
			tok = "k"
			cur = nil
		}
		parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1"))
	}
	p := "/" + strings.Join(parts, "/")
	switch rapid.IntRange(0, 19).Draw(t, "prefix") {
	case 0:
		return strings.TrimPrefix(p, "/")
	case 1:
		return ""
	case 2:
		return "x" + p
	}
	return p
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestIgnoreNormalizerJQNonJSONTypes: objects built in Go (not decoded from JSON) may carry
// other value types; they must be handled without panicking.
func TestIgnoreNormalizerJQNonJSONTypes(t *testing.T) {
	t.Parallel()
	n, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "*", Kind: "*", JQPathExpressions: []string{".spec.drop"}}}, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "W", "metadata": map[string]any{"name": "w"},
		"spec": map[string]any{"drop": true, "tags": []string{"a", "b"}, "small": int8(3), "big": uint64(7), "ratio": float32(0.5)}}}
	require.NoError(t, n.Normalize(context.Background(), u))
	spec := u.Object["spec"].(map[string]any)
	assert.NotContains(t, spec, "drop")
	assert.Equal(t, []any{"a", "b"}, spec["tags"])
	assert.Equal(t, int64(3), spec["small"])
	assert.Equal(t, int64(7), spec["big"])
	assert.InDelta(t, 0.5, spec["ratio"], 1e-9)
}
