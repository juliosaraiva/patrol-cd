package diff

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"
)

var (
	mask1 = strings.Repeat("+", 8)
	mask2 = strings.Repeat("+", 12)
	mask3 = strings.Repeat("+", 16)
)

// secret builds a Secret whose data holds the base64 encoding of the given plaintexts.
func secret(data map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"}}}
	if data != nil {
		d := map[string]any{}
		for k, v := range data {
			d[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		u.Object["data"] = d
	}
	return u
}

func dataOf(u *unstructured.Unstructured) map[string]any {
	d, _, _ := unstructured.NestedMap(u.Object, "data")
	return d
}

func lastAppliedOf(t testingT, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	la, err := LastAppliedConfig(u)
	require.NoError(t, err)
	require.NotNil(t, la)
	return la
}

func TestHideSecretData(t *testing.T) {
	t.Parallel()
	t.Run("same keys, different values", func(t *testing.T) {
		t.Parallel()
		target, live, err := HideSecretData(secret(map[string]string{"k1": "a", "k2": "a"}), secret(map[string]string{"k1": "b", "k2": "b"}), nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"k1": mask1, "k2": mask1}, dataOf(target))
		assert.Equal(t, map[string]any{"k1": mask2, "k2": mask2}, dataOf(live))
	})
	t.Run("same keys, same values", func(t *testing.T) {
		t.Parallel()
		target, live, err := HideSecretData(secret(map[string]string{"k1": "a"}), secret(map[string]string{"k1": "a"}), nil)
		require.NoError(t, err)
		assert.Equal(t, dataOf(target), dataOf(live))
	})
	t.Run("different keys", func(t *testing.T) {
		t.Parallel()
		target, live, err := HideSecretData(secret(map[string]string{"k1": "a", "k2": "a"}), secret(map[string]string{"k2": "b", "k3": "b"}), nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"k1": mask1, "k2": mask1}, dataOf(target))
		assert.Equal(t, map[string]any{"k2": mask2, "k3": mask1}, dataOf(live))
	})
	t.Run("nil sides", func(t *testing.T) {
		t.Parallel()
		target, live, err := HideSecretData(nil, secret(map[string]string{"k": "v"}), nil)
		require.NoError(t, err)
		assert.Nil(t, target)
		assert.Equal(t, map[string]any{"k": mask1}, dataOf(live))
		target, live, err = HideSecretData(secret(map[string]string{"k": "v"}), nil, nil)
		require.NoError(t, err)
		assert.Nil(t, live)
		assert.Equal(t, map[string]any{"k": mask1}, dataOf(target))
	})
	t.Run("empty and null data", func(t *testing.T) {
		t.Parallel()
		target := secret(map[string]string{})
		live := secret(map[string]string{"token": "t", "namespace": "default"})
		target.Object["data"] = nil
		mt, ml, err := HideSecretData(target, live, nil)
		require.NoError(t, err)
		assert.Nil(t, mt.Object["data"])
		assert.Equal(t, map[string]any{"token": mask1, "namespace": mask1}, dataOf(ml))
	})
	t.Run("stringData is folded in", func(t *testing.T) {
		t.Parallel()
		target := secret(nil)
		target.Object["stringData"] = map[string]any{"user": "admin"}
		mt, ml, err := HideSecretData(target, secret(map[string]string{"user": "admin"}), nil)
		require.NoError(t, err)
		assert.NotContains(t, mt.Object, "stringData")
		assert.Equal(t, dataOf(mt), dataOf(ml))
	})
	t.Run("invalid values are masked too", func(t *testing.T) {
		t.Parallel()
		target := secret(nil)
		target.Object["data"] = map[string]any{"k1": "a2V5MQ==", "k3": false}
		target.Object["stringData"] = map[string]any{"k4": "v", "k5": int64(5)}
		mt, _, err := HideSecretData(target, secret(nil), nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"k1": mask1, "k3": mask1, "k4": mask1, "k5": mask1}, dataOf(mt))
	})
	t.Run("live last-applied", func(t *testing.T) {
		t.Parallel()
		live := withLastApplied(t, secret(map[string]string{"k": "live"}), secret(map[string]string{"k": "applied"}))
		mt, ml, err := HideSecretData(secret(map[string]string{"k": "target"}), live, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"k": mask1}, dataOf(mt))
		assert.Equal(t, map[string]any{"k": mask2}, dataOf(ml))
		assert.Equal(t, map[string]any{"k": mask3}, dataOf(lastAppliedOf(t, ml)))
	})
	t.Run("target and live last-applied", func(t *testing.T) {
		t.Parallel()
		target := withLastApplied(t, secret(map[string]string{"k1": "t1", "k2": "t2"}), secret(map[string]string{"k1": "t1", "k2": "t2"}))
		live := withLastApplied(t, secret(map[string]string{"k1": "l1"}), secret(map[string]string{"k1": "l1"}))
		mt, ml, err := HideSecretData(target, live, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"k1": mask1, "k2": mask1}, dataOf(mt))
		assert.Equal(t, map[string]any{"k1": mask2}, dataOf(ml))
		assert.Equal(t, map[string]any{"k1": mask1, "k2": mask1}, dataOf(lastAppliedOf(t, mt)))
		assert.Equal(t, map[string]any{"k1": mask2}, dataOf(lastAppliedOf(t, ml)))
	})
	t.Run("malformed last-applied fails closed", func(t *testing.T) {
		t.Parallel()
		// Truncated JSON, and valid JSON without kind (rejected by the baseline's decoder).
		for _, raw := range []string{`{"data":{"password":"U0VDUkVUVkFM"`, `{"data":{"password":"U0VDUkVUVkFM"}}`} {
			for _, side := range []string{"target", "live"} {
				target, live := secret(map[string]string{"k": "v"}), secret(map[string]string{"k": "v"})
				bad := live
				if side == "target" {
					bad = target
				}
				bad.SetAnnotations(map[string]string{AnnotationLastAppliedConfig: raw})
				mt, ml, err := HideSecretData(target, live, nil)
				require.NoError(t, err)
				got := ml
				if side == "target" {
					got = mt
				}
				assert.Equal(t, mask1, got.GetAnnotations()[AnnotationLastAppliedConfig], side)
			}
		}
	})
	t.Run("input is not modified", func(t *testing.T) {
		t.Parallel()
		target, live := secret(map[string]string{"k": "a"}), secret(map[string]string{"k": "b"})
		tb, lb := target.DeepCopy(), live.DeepCopy()
		_, _, err := HideSecretData(target, live, nil)
		require.NoError(t, err)
		assert.Equal(t, tb, target)
		assert.Equal(t, lb, live)
	})
}

func TestHideSecretDataAnnotations(t *testing.T) {
	t.Parallel()
	la := `{"apiVersion":"v1","kind":"Secret","metadata":{"annotations":{"app":"test","token/value":"secret","key":"secret-key"},"name":"my-secret"},"type":"Opaque"}`
	cases := []struct {
		name      string
		hide      map[string]bool
		annots    map[string]any
		want      map[string]any
		targetNil bool
	}{
		{name: "nothing hidden", annots: map[string]any{"token/value": "secret", "app": "test"}, want: map[string]any{"token/value": "secret", "app": "test"}},
		{name: "hidden annotations", hide: map[string]bool{"token/value": true, "key": true},
			annots: map[string]any{"token/value": "secret", "key": "secret-key", "app": "test"},
			want:   map[string]any{"token/value": mask1, "key": mask1, "app": "test"}},
		{name: "hidden inside last-applied", hide: map[string]bool{"token/value": true, "key": true}, targetNil: true,
			annots: map[string]any{"token/value": "secret", "app": "test", AnnotationLastAppliedConfig: la},
			want: map[string]any{"token/value": mask1, "app": "test",
				AnnotationLastAppliedConfig: `{"apiVersion":"v1","kind":"Secret","metadata":{"annotations":{"app":"test","key":"++++++++","token/value":"++++++++"},"name":"my-secret"},"type":"Opaque"}`}},
		{name: "hide the last-applied annotation itself", hide: map[string]bool{AnnotationLastAppliedConfig: true}, targetNil: true,
			annots: map[string]any{"app": "test", AnnotationLastAppliedConfig: la},
			want:   map[string]any{"app": "test", AnnotationLastAppliedConfig: mask1}},
		{name: "non-string annotation values", hide: map[string]bool{"token/value": true, "key": true},
			annots: map[string]any{"token/value": int64(0), "key": "secret", "app": true},
			want:   map[string]any{"token/value": mask1, "key": mask1, "app": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": "s", "annotations": tc.annots}, "type": "Opaque"}}
			var target *unstructured.Unstructured
			if !tc.targetNil {
				target = s.DeepCopy()
			}
			mt, ml, err := HideSecretData(target, s.DeepCopy(), tc.hide)
			require.NoError(t, err)
			for _, o := range []*unstructured.Unstructured{mt, ml} {
				if o == nil {
					continue
				}
				annots, _, _ := unstructured.NestedMap(o.Object, "metadata", "annotations")
				for k, v := range tc.want {
					assert.Equal(t, v, annots[k], k)
				}
			}
		})
	}
	t.Run("differences stay visible", func(t *testing.T) {
		t.Parallel()
		mk := func(v string) *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": "s", "annotations": map[string]any{"token/value": v}}}}
		}
		mt, ml, err := HideSecretData(mk("new"), mk("old"), map[string]bool{"token/value": true})
		require.NoError(t, err)
		assert.Equal(t, mask1, mt.GetAnnotations()["token/value"])
		assert.Equal(t, mask2, ml.GetAnnotations()["token/value"])
	})
}

func TestMaskSecretResult(t *testing.T) {
	t.Parallel()
	pred, live := secret(map[string]string{"pw": "new"}), secret(map[string]string{"pw": "old"})
	res := &Result{Modified: true, PredictedLive: mustJSON(t, pred), NormalizedLive: mustJSON(t, live)}
	masked, err := MaskSecretResult(res, nil)
	require.NoError(t, err)
	assert.True(t, masked.Modified)
	assert.NotContains(t, string(masked.PredictedLive), dataOf(pred)["pw"])
	assert.NotContains(t, string(masked.NormalizedLive), dataOf(live)["pw"])
	assert.NotEqual(t, string(masked.PredictedLive), string(masked.NormalizedLive))

	created := &Result{Modified: true, PredictedLive: mustJSON(t, pred), NormalizedLive: []byte("null")}
	masked, err = MaskSecretResult(created, nil)
	require.NoError(t, err)
	assert.Equal(t, "null", string(masked.NormalizedLive))
	assert.NotContains(t, string(masked.PredictedLive), dataOf(pred)["pw"])

	cm := &Result{PredictedLive: []byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{"a":"b"}}`), NormalizedLive: []byte("null")}
	same, err := MaskSecretResult(cm, nil)
	require.NoError(t, err)
	assert.Same(t, cm, same)
}

// plaintext draws a value that cannot be confused with a mask or occur by accident inside
// another document (long enough, no "+").
func plaintext(t *rapid.T, label string) string {
	return rapid.StringMatching(`[a-zA-Z0-9]{10,24}`).Draw(t, label)
}

// TestHideSecretDataProperties checks, over random Secrets (with last-applied annotations),
// that masking never reveals a value (plaintext or base64) and that, for every key, masks
// are equal exactly when the underlying values are equal.
func TestHideSecretDataProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		pool := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-zA-Z0-9]{10,24}`), 1, 4, rapid.ID[string]).Draw(rt, "pool")
		keys := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{1,6}`), 1, 5, rapid.ID[string]).Draw(rt, "keys")
		gen := func(label string) map[string]string {
			m := map[string]string{}
			for _, k := range keys {
				if rapid.Bool().Draw(rt, label+"-has-"+k) {
					m[k] = rapid.SampledFrom(pool).Draw(rt, label+"-"+k)
				}
			}
			return m
		}
		targetData, liveData, laData := gen("target"), gen("live"), gen("lastApplied")
		target := secret(targetData)
		live := secret(liveData)
		if rapid.Bool().Draw(rt, "withLastApplied") {
			live = withLastApplied(rt, live, secret(laData))
		} else {
			laData = nil
		}
		hideAnnotation := plaintext(rt, "annotation")
		live.SetAnnotations(mergeMaps(live.GetAnnotations(), map[string]string{"token": hideAnnotation}))

		mt, ml, err := HideSecretData(target, live, map[string]bool{"token": true})
		require.NoError(rt, err)
		out := string(mustJSON(rt, mt)) + string(mustJSON(rt, ml))
		for _, v := range append(pool, hideAnnotation) {
			assert.NotContains(rt, out, v)
			assert.NotContains(rt, out, base64.StdEncoding.EncodeToString([]byte(v)))
		}

		sides := []struct {
			plain  map[string]string
			masked map[string]any
		}{{targetData, dataOf(mt)}, {liveData, dataOf(ml)}}
		if laData != nil {
			sides = append(sides, struct {
				plain  map[string]string
				masked map[string]any
			}{laData, dataOf(lastAppliedOf(rt, ml))})
		}
		for _, k := range keys {
			for i := range sides {
				for j := range sides {
					pi, iok := sides[i].plain[k]
					pj, jok := sides[j].plain[k]
					if !iok || !jok {
						continue
					}
					mi, mj := sides[i].masked[k].(string), sides[j].masked[k].(string)
					assert.Equal(rt, pi == pj, mi == mj, "key %q sides %d/%d", k, i, j)
					assert.Equal(rt, "", strings.Trim(mi, "+"), "masks only contain '+'")
					assert.GreaterOrEqual(rt, len(mi), 8)
					assert.Zero(rt, (len(mi)-8)%4)
				}
			}
		}
	})
}

func mergeMaps(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
