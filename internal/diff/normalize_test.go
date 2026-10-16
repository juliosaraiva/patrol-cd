package diff

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
)

func TestRemarshal(t *testing.T) {
	t.Parallel()
	log := Options{}.withDefaults().Log

	t.Run("drops empty lists and generated nulls", func(t *testing.T) {
		t.Parallel()
		out, err := remarshal(obj(t, `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"sa"},"imagePullSecrets":[]}`), log)
		require.NoError(t, err)
		assert.NotContains(t, out.Object, "imagePullSecrets")
		assert.NotContains(t, out.Object["metadata"], "creationTimestamp")
	})
	t.Run("nested creationTimestamp null is removed", func(t *testing.T) {
		t.Parallel()
		out, err := remarshal(obj(t, stsLive), log)
		require.NoError(t, err)
		_, ok, _ := unstructured.NestedFieldNoCopy(out.Object, "spec", "template", "metadata", "creationTimestamp")
		assert.False(t, ok)
	})
	t.Run("quantities", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			in   any
			want any
		}{{0.2, "200m"}, {"0.2", "200m"}, {int64(2), "2"}, {"2000m", "2"}, {"invalid", "invalid"}} {
			pod := obj(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"i","resources":{"requests":{}}}]}}`)
			c := pod.Object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			c["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = tc.in
			out, err := remarshal(pod, log)
			require.NoError(t, err)
			oc := out.Object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			assert.Equal(t, tc.want, oc["resources"].(map[string]any)["requests"].(map[string]any)["cpu"], "%v", tc.in)
		}
	})
	t.Run("unknown kinds only lose Go type information", func(t *testing.T) {
		t.Parallel()
		in := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "Widget", "spec": map[string]any{"n": 3, "f": float32(1.5)}}}
		out, err := remarshal(in, log)
		require.NoError(t, err)
		assert.Equal(t, int64(3), out.Object["spec"].(map[string]any)["n"])
		assert.Equal(t, 1.5, out.Object["spec"].(map[string]any)["f"])
	})
	t.Run("unencodable object is an error", func(t *testing.T) {
		t.Parallel()
		_, err := remarshal(&unstructured.Unstructured{Object: map[string]any{"kind": "X", "bad": make(chan int)}}, log)
		require.Error(t, err)
		_, err = Diff(context.Background(), &unstructured.Unstructured{Object: map[string]any{"kind": "X", "bad": make(chan int)}}, nil, Options{})
		require.Error(t, err)
	})
}

func TestNormalizeSecretVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		data, stringData map[string]any
	}{
		{"valid", map[string]any{"k1": "azE="}, map[string]any{"k2": "v2"}},
		{"invalid data values", map[string]any{"k1": "azE=", "k2": int64(2)}, map[string]any{"k3": "v3", "k4": nil}},
		{"only stringData", nil, map[string]any{"k3": "v3", "k4": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"},
				"type": "Opaque", "data": tc.data, "stringData": tc.stringData}}
			if tc.data == nil {
				delete(u.Object, "data")
			}
			NormalizeSecret(u)
			assert.NotContains(t, u.Object, "stringData")
			data, ok, _ := unstructured.NestedMap(u.Object, "data")
			require.True(t, ok)
			for _, src := range []map[string]any{tc.data, tc.stringData} {
				for k := range src {
					assert.Contains(t, data, k)
				}
			}
		})
	}
	cm := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"stringData":{"a":"b"}}`)
	NormalizeSecret(cm)
	assert.Contains(t, cm.Object, "stringData", "only Secrets are touched")
	NormalizeSecret(nil)
}

func TestNormalizeCanonicalizesSecretData(t *testing.T) {
	t.Parallel()
	u := obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"a":"YWJj\n","b":null}}`)
	require.NoError(t, Normalize(context.Background(), u, Options{}))
	assert.Equal(t, map[string]any{"a": "YWJj", "b": ""}, u.Object["data"])
}

func TestNormalizeRoles(t *testing.T) {
	t.Parallel()
	r := obj(t, aggrRoleLive)
	require.NoError(t, Normalize(context.Background(), r, Options{IgnoreAggregatedRoles: true}))
	assert.Nil(t, r.Object["rules"])

	r = obj(t, aggrRoleLive)
	require.NoError(t, Normalize(context.Background(), r, Options{}))
	assert.NotNil(t, r.Object["rules"])

	malformed := obj(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"r"},"aggregationRule":"oops","rules":[{"verbs":["get"]}]}`)
	require.NoError(t, Normalize(context.Background(), malformed, Options{IgnoreAggregatedRoles: true}))
	assert.NotNil(t, malformed.Object["rules"])
}

// TestNormalizeEndpointsOrderInsensitive: any permutation of subsets, addresses and ports
// normalizes to the same object.
func TestNormalizeEndpointsOrderInsensitive(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		nSubsets := rapid.IntRange(1, 3).Draw(rt, "subsets")
		subsets := make([]any, nSubsets)
		for i := range subsets {
			addrs := rapid.SliceOfNDistinct(rapid.IntRange(1, 250), 1, 5, rapid.ID[int]).Draw(rt, fmt.Sprintf("addrs%d", i))
			ports := rapid.SliceOfNDistinct(rapid.IntRange(1, 65535), 1, 4, rapid.ID[int]).Draw(rt, fmt.Sprintf("ports%d", i))
			a := make([]any, len(addrs))
			for j, ip := range addrs {
				a[j] = map[string]any{"ip": fmt.Sprintf("10.%d.0.%d", i, ip)}
			}
			p := make([]any, len(ports))
			for j, port := range ports {
				p[j] = map[string]any{"name": fmt.Sprintf("p%d", port), "port": int64(port)}
			}
			subsets[i] = map[string]any{"addresses": a, "ports": p}
		}
		mk := func(ss []any) *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Endpoints", "metadata": map[string]any{"name": "e"}, "subsets": ss}}
		}
		original := mk(deepCopyValue(subsets).([]any))
		shuffled := deepCopyValue(subsets).([]any)
		shuffled = rapid.Permutation(shuffled).Draw(rt, "subsetOrder")
		for i := range shuffled {
			s := shuffled[i].(map[string]any)
			s["addresses"] = rapid.Permutation(s["addresses"].([]any)).Draw(rt, "addrOrder")
			s["ports"] = rapid.Permutation(s["ports"].([]any)).Draw(rt, "portOrder")
		}
		other := mk(shuffled)
		require.NoError(rt, Normalize(context.Background(), original, Options{}))
		require.NoError(rt, Normalize(context.Background(), other, Options{}))
		assert.Equal(rt, original.Object, other.Object)
	})
}

// genObject draws a random object of one of the kinds with special normalization rules.
func genObject(t *rapid.T) *unstructured.Unstructured {
	name := rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "name")
	md := map[string]any{"name": name, "namespace": "ns"}
	if rapid.Bool().Draw(t, "labels") {
		md["labels"] = map[string]any{"app": name}
	}
	if rapid.Bool().Draw(t, "ts") {
		md["creationTimestamp"] = nil
	}
	quantities := []any{"100m", 0.5, "1Gi", "512Mi", int64(1), "1.5", "2000m"}
	switch rapid.SampledFrom([]string{"configmap", "secret", "deployment", "widget", "role", "endpoints"}).Draw(t, "kind") {
	case "configmap":
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": md,
			"data": map[string]any{"a": rapid.String().Draw(t, "a")}}}
	case "secret":
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": md, "type": "Opaque",
			"data": map[string]any{"a": base64.StdEncoding.EncodeToString([]byte(rapid.String().Draw(t, "a")))}}}
		if rapid.Bool().Draw(t, "stringData") {
			u.Object["stringData"] = map[string]any{"b": rapid.String().Draw(t, "b")}
		}
		return u
	case "deployment":
		n := rapid.IntRange(1, 3).Draw(t, "containers")
		cs := make([]any, n)
		for i := range cs {
			cs[i] = map[string]any{"name": fmt.Sprintf("c%d", i), "image": "img:" + rapid.StringMatching(`[0-9]{1,2}`).Draw(t, "tag"),
				"resources": map[string]any{"requests": map[string]any{"cpu": rapid.SampledFrom(quantities).Draw(t, "cpu")}}}
		}
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": md,
			"spec": map[string]any{"replicas": rapid.Int64Range(0, 5).Draw(t, "replicas"),
				"template": map[string]any{"spec": map[string]any{"containers": cs}}},
			"status": map[string]any{"replicas": int64(1)}}}
	case "role":
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": md,
			"rules": []any{}}}
		if rapid.Bool().Draw(t, "aggregated") {
			u.Object["aggregationRule"] = map[string]any{"clusterRoleSelectors": []any{}}
			u.Object["rules"] = []any{map[string]any{"verbs": []any{"get"}}}
		}
		return u
	case "endpoints":
		return obj(t, endpointsConfig)
	default:
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": md,
			"spec": genDoc(t, 3), "status": map[string]any{"phase": "x"}}}
	}
}

func propertyNormalizer(t testingT) Normalizer {
	n, err := NewNormalizer(NormalizerConfig{
		Rules: append(SystemIgnoreRules(nil, DefaultCompareOptions()),
			IgnoreRule{Group: "*", Kind: "*", JSONPointers: []string{"/metadata/labels/app"}},
			IgnoreRule{Group: "apps", Kind: "*", JQPathExpressions: []string{".spec.template.spec.containers[]? | select(.name == \"c1\")"}}),
		KnownTypeFields: map[schema.GroupKind][]KnownTypeField{{Group: "example.com", Kind: "Widget"}: {{Field: "spec.a", Type: "core/Quantity"}}},
	})
	require.NoError(t, err)
	return n
}

// TestNormalizeIdempotent: normalizing twice is the same as normalizing once.
func TestNormalizeIdempotent(t *testing.T) {
	t.Parallel()
	n := propertyNormalizer(t)
	rapid.Check(t, func(rt *rapid.T) {
		o := Options{Normalizer: n, IgnoreAggregatedRoles: rapid.Bool().Draw(rt, "aggr")}
		u, err := remarshal(genObject(rt), Options{}.withDefaults().Log)
		require.NoError(rt, err)
		require.NoError(rt, Normalize(context.Background(), u, o))
		once := u.DeepCopy()
		require.NoError(rt, Normalize(context.Background(), u, o))
		assert.Equal(rt, once.Object, u.Object)

		again, err := remarshal(once, Options{}.withDefaults().Log)
		require.NoError(rt, err)
		twice, err := remarshal(again, Options{}.withDefaults().Log)
		require.NoError(rt, err)
		assert.Equal(rt, again.Object, twice.Object, "remarshal is idempotent")
	})
}

// TestDiffReflexive: an object is never different from itself, with any strategy that
// does not need the API server, and the inputs are never mutated.
func TestDiffReflexive(t *testing.T) {
	t.Parallel()
	n := propertyNormalizer(t)
	p := testParser(t)
	rapid.Check(t, func(rt *rapid.T) {
		u := genObject(rt)
		before := u.DeepCopy()
		o := Options{Normalizer: n}
		if rapid.Bool().Draw(rt, "smd") && u.GetKind() != "Widget" && u.GetKind() != "Endpoints" {
			o.StructuredMerge, o.GVKParser = true, p
		}
		if rapid.Bool().Draw(rt, "lastApplied") {
			u = withLastApplied(rt, u, u)
			before = u.DeepCopy()
		}
		res, err := Diff(context.Background(), u, u, o)
		require.NoError(rt, err)
		assert.False(rt, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)
		assert.Equal(rt, before.Object, u.Object, "input mutated")
	})
}
