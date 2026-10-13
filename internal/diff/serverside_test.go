package diff

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// stubDryRunner returns a canned dry-run result and records what it was asked to apply.
type stubDryRunner struct {
	mu       sync.Mutex
	result   string
	err      error
	got      *unstructured.Unstructured
	gotOwner string
	t        testing.TB
}

func (s *stubDryRunner) ServerSideApplyDryRun(_ context.Context, o *unstructured.Unstructured, manager string) (*unstructured.Unstructured, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got, s.gotOwner = o.DeepCopy(), manager
	if s.err != nil {
		return nil, s.err
	}
	return obj(s.t, s.result), nil
}

const ssdLiveService = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  resourceVersion: "100"
  labels: {app: web, stale: "true"}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"apiVersion":"v1","kind":"Service","metadata":{"name":"web"},"spec":{"type":"ClusterIP"}}'
  managedFields:
  - manager: patrol-controller
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1: {"f:metadata":{"f:labels":{"f:app":{},"f:stale":{}}},"f:spec":{"f:ports":{"k:{\"port\":80,\"protocol\":\"TCP\"}":{".":{},"f:name":{},"f:port":{},"f:targetPort":{}}},"f:selector":{}}}
spec:
  clusterIP: 10.96.0.20
  clusterIPs: [10.96.0.20]
  internalTrafficPolicy: Cluster
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  ports: [{name: http, port: 80, protocol: TCP, targetPort: 8080}]
  selector: {app: web}
  sessionAffinity: None
  type: ClusterIP
`

const ssdConfigService = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  labels: {app: web, stale: "true"}
spec:
  selector: {app: web}
  sessionAffinity: ClientIP
  ports: [{name: http, port: 80, targetPort: 8081}]
`

// ssdPredictedService is what the API server returns for ssdConfigService: the new target
// port and session affinity, a label added by a mutating webhook, a new resourceVersion.
const ssdPredictedService = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  resourceVersion: "101"
  labels: {app: web, stale: "true", mutated-by: webhook}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"apiVersion":"v1","kind":"Service","metadata":{"name":"web"},"spec":{"type":"ClusterIP"}}'
  managedFields:
  - manager: patrol-controller
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1: {"f:metadata":{"f:labels":{"f:app":{},"f:stale":{}}},"f:spec":{"f:ports":{"k:{\"port\":80,\"protocol\":\"TCP\"}":{".":{},"f:name":{},"f:port":{},"f:targetPort":{}}},"f:selector":{},"f:sessionAffinity":{}}}
spec:
  clusterIP: 10.96.0.20
  clusterIPs: [10.96.0.20]
  internalTrafficPolicy: Cluster
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  ports: [{name: http, port: 80, protocol: TCP, targetPort: 8081}]
  selector: {app: web}
  sessionAffinity: ClientIP
  sessionAffinityConfig: {clientIP: {timeoutSeconds: 10800}}
  type: ClusterIP
`

func ssdOptions(t *testing.T, dr DryRunner) Options {
	o := DefaultOptions()
	o.ServerSide = true
	o.GVKParser = testParser(t)
	o.DryRunner = dr
	return o
}

func TestServerSideDiff(t *testing.T) {
	t.Parallel()

	t.Run("mutation webhook changes are ignored by default", func(t *testing.T) {
		t.Parallel()
		dr := &stubDryRunner{t: t, result: ssdPredictedService}
		res := mustDiff(t, obj(t, ssdConfigService), obj(t, ssdLiveService), ssdOptions(t, dr))
		assert.True(t, res.Modified)
		labels, _ := nested(t, res.PredictedLive, "metadata", "labels")
		assert.NotContains(t, labels, "mutated-by")
		tp, _ := nested(t, res.PredictedLive, "spec", "ports")
		assert.EqualValues(t, 8081, tp.([]any)[0].(map[string]any)["targetPort"])
		assert.Equal(t, DefaultManager, dr.gotOwner)
		// Neither side carries server bookkeeping.
		for _, side := range [][]byte{res.PredictedLive, res.NormalizedLive} {
			assert.NotContains(t, string(side), "managedFields")
			assert.NotContains(t, string(side), "resourceVersion")
			assert.NotContains(t, string(side), AnnotationLastAppliedConfig)
		}
	})
	t.Run("mutation webhook changes are included on request", func(t *testing.T) {
		t.Parallel()
		o := ssdOptions(t, &stubDryRunner{t: t, result: ssdPredictedService})
		o.IgnoreMutationWebhook = false
		res := mustDiff(t, obj(t, ssdConfigService), obj(t, ssdLiveService), o)
		assert.True(t, res.Modified)
		labels, _ := nested(t, res.PredictedLive, "metadata", "labels")
		assert.Contains(t, labels, "mutated-by")
	})
	t.Run("only a webhook mutation is not drift", func(t *testing.T) {
		t.Parallel()
		predicted := obj(t, ssdLiveService)
		predicted.SetLabels(map[string]string{"app": "web", "stale": "true", "mutated-by": "webhook"})
		predicted.SetResourceVersion("101")
		cfg := obj(t, ssdConfigService)
		unstructured.RemoveNestedField(cfg.Object, "spec", "sessionAffinity")
		require.NoError(t, unstructured.SetNestedSlice(cfg.Object, []any{map[string]any{"name": "http", "port": int64(80), "targetPort": int64(8080)}}, "spec", "ports"))
		dr := &stubDryRunner{t: t, result: string(mustJSON(t, predicted))}
		res := mustDiff(t, cfg, obj(t, ssdLiveService), ssdOptions(t, dr))
		assert.False(t, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)

		o := ssdOptions(t, dr)
		o.IgnoreMutationWebhook = false
		assert.True(t, mustDiff(t, cfg, obj(t, ssdLiveService), o).Modified)
	})
	t.Run("removal of an owned label is reported", func(t *testing.T) {
		t.Parallel()
		predicted := obj(t, ssdLiveService)
		predicted.SetLabels(map[string]string{"app": "web"})
		mf := predicted.GetManagedFields()
		mf[0].FieldsV1.SetRawBytes([]byte(`{"f:metadata":{"f:labels":{"f:app":{}}},"f:spec":{"f:ports":{"k:{\"port\":80,\"protocol\":\"TCP\"}":{".":{},"f:name":{},"f:port":{},"f:targetPort":{}}},"f:selector":{}}}`))
		predicted.SetManagedFields(mf)
		cfg := obj(t, ssdConfigService)
		cfg.SetLabels(map[string]string{"app": "web"})
		res := mustDiff(t, cfg, obj(t, ssdLiveService), ssdOptions(t, &stubDryRunner{t: t, result: string(mustJSON(t, predicted))}))
		assert.True(t, res.Modified)
		pl, _ := nested(t, res.PredictedLive, "metadata", "labels")
		assert.Equal(t, map[string]any{"app": "web"}, pl)
		ll, _ := nested(t, res.NormalizedLive, "metadata", "labels")
		assert.Contains(t, ll, "stale")
	})
	t.Run("different resourceVersion alone is not drift", func(t *testing.T) {
		t.Parallel()
		predicted := obj(t, ssdLiveService)
		predicted.SetResourceVersion("999")
		cfg := obj(t, ssdConfigService)
		unstructured.RemoveNestedField(cfg.Object, "spec", "sessionAffinity")
		require.NoError(t, unstructured.SetNestedSlice(cfg.Object, []any{map[string]any{"name": "http", "port": int64(80), "targetPort": int64(8080)}}, "spec", "ports"))
		o := ssdOptions(t, &stubDryRunner{t: t, result: string(mustJSON(t, predicted))})
		o.IgnoreMutationWebhook = false
		assert.False(t, mustDiff(t, cfg, obj(t, ssdLiveService), o).Modified)
	})
	t.Run("ignore rules apply after the dry run", func(t *testing.T) {
		t.Parallel()
		n, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "", Kind: "Service", JSONPointers: []string{"/spec/sessionAffinity", "/spec/sessionAffinityConfig"}}}, IgnoreNormalizerOptions{})
		require.NoError(t, err)
		dr := &stubDryRunner{t: t, result: ssdPredictedService}
		o := ssdOptions(t, dr)
		o.Normalizer = n
		res := mustDiff(t, obj(t, ssdConfigService), obj(t, ssdLiveService), o)
		assert.True(t, res.Modified, "the port change is still visible")
		assert.NotContains(t, string(res.PredictedLive), "sessionAffinity")
		assert.NotContains(t, string(res.NormalizedLive), "sessionAffinity")
		v, ok, _ := unstructured.NestedString(dr.got.Object, "spec", "sessionAffinity")
		assert.True(t, ok, "the dry run receives the complete config")
		assert.Equal(t, "ClientIP", v)
	})
	t.Run("secret values only present in last-applied are stripped", func(t *testing.T) {
		t.Parallel()
		const hidden = "T05MWUlOQU5OT1RBVElPTg=="
		la := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"password":"` + hidden + `"}}`
		live := obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s","namespace":"ns"},"type":"Opaque","data":{"password":"cGFzcw=="}}`)
		live.SetAnnotations(map[string]string{AnnotationLastAppliedConfig: la})
		predicted := live.DeepCopy()
		cfg := obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s","namespace":"ns"},"type":"Opaque","data":{"password":"cGFzcw=="}}`)
		o := ssdOptions(t, &stubDryRunner{t: t, result: string(mustJSON(t, predicted))})
		o.IgnoreMutationWebhook = false
		res := mustDiff(t, cfg, live, o)
		assert.False(t, res.Modified)
		assert.NotContains(t, string(res.PredictedLive), hidden)
		assert.NotContains(t, string(res.NormalizedLive), hidden)
	})
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		cfg, live := obj(t, ssdConfigService), obj(t, ssdLiveService)

		_, err := Diff(context.Background(), cfg, live, ssdOptions(t, nil))
		require.ErrorIs(t, err, ErrNoDryRunner)

		boom := errors.New("admission webhook denied the request")
		_, err = Diff(context.Background(), cfg, live, ssdOptions(t, &stubDryRunner{t: t, err: boom}))
		require.ErrorIs(t, err, ErrDryRun)
		require.ErrorIs(t, err, boom)

		noMF := obj(t, ssdPredictedService)
		noMF.SetManagedFields(nil)
		_, err = Diff(context.Background(), cfg, live, ssdOptions(t, &stubDryRunner{t: t, result: string(mustJSON(t, noMF))}))
		require.ErrorIs(t, err, ErrNoManagedFields)

		o := ssdOptions(t, &stubDryRunner{t: t, result: ssdPredictedService})
		o.Manager = "someone-else"
		_, err = Diff(context.Background(), cfg, live, o)
		require.ErrorIs(t, err, ErrManagerNotFound)

		o = ssdOptions(t, &stubDryRunner{t: t, result: ssdPredictedService})
		o.GVKParser = nil
		_, err = Diff(context.Background(), cfg, live, o)
		require.ErrorIs(t, err, ErrNoGVKParser)
	})
}

// TestServerSideDiffCompositeKeys covers associative lists keyed by several fields
// (container ports: containerPort+protocol). The key field defaulted by the server
// (protocol) is not owned by the applier, yet must survive webhook-mutation removal.
func TestServerSideDiffCompositeKeys(t *testing.T) {
	t.Parallel()
	live := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  managedFields:
  - manager: patrol-controller
    operation: Apply
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1: {"f:spec":{"f:selector":{},"f:template":{"f:metadata":{"f:labels":{"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{".":{},"f:image":{},"f:name":{},"f:ports":{"k:{\"containerPort\":80,\"protocol\":\"TCP\"}":{".":{},"f:containerPort":{}}}}}}}}}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: web
        image: nginx:1.27
        ports: [{containerPort: 80, protocol: TCP}]
`)
	config := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: web
        image: nginx:1.27
        ports: [{containerPort: 80}, {containerPort: 9090, name: metrics}]
`)
	predicted := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels: {injected: "true"}
  managedFields:
  - manager: patrol-controller
    operation: Apply
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1: {"f:spec":{"f:selector":{},"f:template":{"f:metadata":{"f:labels":{"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{".":{},"f:image":{},"f:name":{},"f:ports":{"k:{\"containerPort\":80,\"protocol\":\"TCP\"}":{".":{},"f:containerPort":{}},"k:{\"containerPort\":9090,\"protocol\":\"TCP\"}":{".":{},"f:containerPort":{},"f:name":{}}}}}}}}}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: web
        image: nginx:1.27
        ports: [{containerPort: 80, protocol: TCP}, {containerPort: 9090, name: metrics, protocol: TCP}]
`
	res := mustDiff(t, config, live, ssdOptions(t, &stubDryRunner{t: t, result: predicted}))
	assert.True(t, res.Modified)
	assert.NotContains(t, string(res.PredictedLive), "injected")
	cs, _ := nested(t, res.PredictedLive, "spec", "template", "spec", "containers")
	ports := cs.([]any)[0].(map[string]any)["ports"].([]any)
	require.Len(t, ports, 2)
	for _, p := range ports {
		assert.Equal(t, "TCP", p.(map[string]any)["protocol"], "composite key field must be kept: %v", p)
	}
}

const widgetSwagger = `{
  "swagger": "2.0",
  "info": {"title": "widgets", "version": "v1"},
  "paths": {},
  "definitions": {
    "com.example.v1.Widget": {
      "type": "object",
      "x-kubernetes-group-version-kind": [{"group": "example.com", "version": "v1", "kind": "Widget"}],
      "properties": {
        "apiVersion": {"type": "string"},
        "kind": {"type": "string"},
        "metadata": {"type": "object", "x-kubernetes-preserve-unknown-fields": true},
        "spec": {"type": "object", "x-kubernetes-preserve-unknown-fields": true}
      }
    }
  }
}`

// TestServerSideDiffPreserveUnknownFields: under x-kubernetes-preserve-unknown-fields the
// server records only owned leaves; the containers above them must not be stripped.
func TestServerSideDiffPreserveUnknownFields(t *testing.T) {
	t.Parallel()
	p := parserFromSwagger(t, widgetSwagger)
	live := obj(t, `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"},"spec":{"configuration":{"value":"old"}}}`)
	config := obj(t, `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"},"spec":{"configuration":{"value":"new"}}}`)
	predicted := `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w","managedFields":[
	  {"manager":"patrol-controller","operation":"Apply","apiVersion":"example.com/v1","fieldsType":"FieldsV1",
	   "fieldsV1":{"f:spec":{"f:configuration":{"f:value":{}}}}}]},
	  "spec":{"configuration":{"value":"new"},"unrelated":"injected-by-webhook"}}`
	o := ssdOptions(t, &stubDryRunner{t: t, result: predicted})
	o.GVKParser = p
	res := mustDiff(t, config, live, o)
	assert.True(t, res.Modified)
	v, ok := nested(t, res.PredictedLive, "spec", "configuration", "value")
	require.True(t, ok)
	assert.Equal(t, "new", v)
	_, ok = nested(t, res.PredictedLive, "spec", "unrelated")
	assert.False(t, ok)
}

func TestWithoutOwnedAncestors(t *testing.T) {
	t.Parallel()
	mk := fieldpath.MakePathOrDie
	owned := fieldpath.NewSet(mk("spec", "configuration", "value"))
	got := withoutOwnedAncestors(fieldpath.NewSet(mk("spec"), mk("spec", "configuration"), mk("spec", "unrelated"), mk("spec", "configuration", "other")), owned)
	assert.False(t, got.Has(mk("spec")))
	assert.False(t, got.Has(mk("spec", "configuration")))
	assert.True(t, got.Has(mk("spec", "unrelated")))
	assert.True(t, got.Has(mk("spec", "configuration", "other")))
}

func TestIsCompositeKeyField(t *testing.T) {
	t.Parallel()
	composite := fieldpath.MakePathOrDie("spec", "ports", fieldpath.KeyByFields("containerPort", 80, "protocol", "TCP"), "protocol")
	single := fieldpath.MakePathOrDie("spec", "containers", fieldpath.KeyByFields("name", "web"), "name")
	other := fieldpath.MakePathOrDie("spec", "ports", fieldpath.KeyByFields("containerPort", 80, "protocol", "TCP"), "name")
	assert.True(t, isCompositeKeyField(composite))
	assert.False(t, isCompositeKeyField(single))
	assert.False(t, isCompositeKeyField(other))
	assert.False(t, isCompositeKeyField(fieldpath.Path{}))
}
