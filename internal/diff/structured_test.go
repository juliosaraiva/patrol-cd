package diff

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const smdServiceLive = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  labels: {app: web, old: "yes", team: payments}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"apiVersion":"v1","kind":"Service"}'
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:app: {}
          f:old: {}
      f:spec:
        f:ports:
          k:{"port":80,"protocol":"TCP"}:
            .: {}
            f:name: {}
            f:port: {}
            f:targetPort: {}
          k:{"port":443,"protocol":"TCP"}:
            .: {}
            f:name: {}
            f:port: {}
            f:targetPort: {}
        f:selector: {}
    manager: patrol-controller
    operation: Apply
    time: "2026-01-02T03:04:05Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:team: {}
    manager: kubectl-edit
    operation: Update
    time: "2026-01-03T03:04:05Z"
spec:
  clusterIP: 10.96.0.10
  clusterIPs: [10.96.0.10]
  internalTrafficPolicy: Cluster
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  ports:
  - {name: http, port: 80, protocol: TCP, targetPort: 8080}
  - {name: https, port: 443, protocol: TCP, targetPort: 8443}
  selector: {app: web}
  sessionAffinity: None
  type: ClusterIP
`

const smdServiceConfig = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  labels: {app: web, old: "yes"}
spec:
  selector: {app: web}
  ports:
  - {name: http, port: 80, targetPort: 8080}
  - {name: https, port: 443, targetPort: 8443}
`

const smdHPALive = `
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: api
  namespace: shop
  managedFields:
  - apiVersion: autoscaling/v1
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:maxReplicas: {}
        f:minReplicas: {}
        f:scaleTargetRef: {}
    manager: helm
    operation: Apply
  - apiVersion: autoscaling/v2
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:metrics: {}
    manager: patrol-controller
    operation: Apply
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: api}
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: Resource
    resource: {name: cpu, target: {type: Utilization, averageUtilization: 50}}
`

const smdHPAConfig = `
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: api, namespace: shop}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: api}
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: Resource
    resource: {name: cpu, target: {type: Utilization, averageUtilization: 50}}
`

func TestStructuredMergeDiff(t *testing.T) {
	t.Parallel()
	p := testParser(t)
	svcConfig := func(fn func(u *unstructured.Unstructured)) *unstructured.Unstructured {
		u := obj(t, smdServiceConfig)
		if fn != nil {
			fn(u)
		}
		return u
	}

	t.Run("no change keeps fields of other managers", func(t *testing.T) {
		t.Parallel()
		res, err := StructuredMergeDiff(svcConfig(nil), obj(t, smdServiceLive), p, DefaultManager)
		require.NoError(t, err)
		assert.False(t, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)
	})
	t.Run("removed port owned by the manager is pruned", func(t *testing.T) {
		t.Parallel()
		cfg := svcConfig(func(u *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedSlice(u.Object, []any{map[string]any{"name": "http", "port": int64(80), "targetPort": int64(8080)}}, "spec", "ports"))
		})
		res, err := StructuredMergeDiff(cfg, obj(t, smdServiceLive), p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
		ports, _ := nested(t, res.PredictedLive, "spec", "ports")
		assert.Len(t, ports, 1)
		livePorts, _ := nested(t, res.NormalizedLive, "spec", "ports")
		assert.Len(t, livePorts, 2)
	})
	t.Run("label applied before and dropped from config is pruned", func(t *testing.T) {
		t.Parallel()
		cfg := svcConfig(func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"app": "web"}) })
		res, err := StructuredMergeDiff(cfg, obj(t, smdServiceLive), p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
		labels, _ := nested(t, res.PredictedLive, "metadata", "labels")
		assert.Equal(t, map[string]any{"app": "web", "team": "payments"}, labels, "team is owned by kubectl-edit and survives")
	})
	t.Run("a different manager does not prune", func(t *testing.T) {
		t.Parallel()
		cfg := svcConfig(func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"app": "web"}) })
		res, err := StructuredMergeDiff(cfg, obj(t, smdServiceLive), p, "someone-else")
		require.NoError(t, err)
		assert.False(t, res.Modified)
	})
	t.Run("defaults and annotation handling", func(t *testing.T) {
		t.Parallel()
		cfg := svcConfig(func(u *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(u.Object, "ClientIP", "spec", "sessionAffinity"))
		})
		res, err := StructuredMergeDiff(cfg, obj(t, smdServiceLive), p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
		v, ok := nested(t, res.PredictedLive, "spec", "sessionAffinityConfig", "clientIP", "timeoutSeconds")
		require.True(t, ok, "ClientIP affinity defaults are applied to the prediction")
		assert.EqualValues(t, 10800, v)
		assert.NotContains(t, string(res.PredictedLive), AnnotationLastAppliedConfig)
		assert.NotContains(t, string(res.NormalizedLive), AnnotationLastAppliedConfig)
	})
	t.Run("type applied before is reset to its default", func(t *testing.T) {
		t.Parallel()
		live := obj(t, smdServiceLive)
		require.NoError(t, unstructured.SetNestedField(live.Object, "NodePort", "spec", "type"))
		mf := live.GetManagedFields()
		mf[0].FieldsV1.SetRawBytes([]byte(`{"f:metadata":{"f:labels":{"f:app":{},"f:old":{}}},"f:spec":{"f:selector":{},"f:type":{},"f:ports":{"k:{\"port\":80,\"protocol\":\"TCP\"}":{".":{},"f:name":{},"f:port":{},"f:targetPort":{}},"k:{\"port\":443,\"protocol\":\"TCP\"}":{".":{},"f:name":{},"f:port":{},"f:targetPort":{}}}}}`))
		live.SetManagedFields(mf)
		res, err := StructuredMergeDiff(svcConfig(nil), live, p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
		typ, _ := nested(t, res.PredictedLive, "spec", "type")
		assert.Equal(t, "ClusterIP", typ)
	})
	t.Run("cross-version managers: unchanged", func(t *testing.T) {
		t.Parallel()
		res, err := StructuredMergeDiff(obj(t, smdHPAConfig), obj(t, smdHPALive), p, DefaultManager)
		require.NoError(t, err)
		assert.False(t, res.Modified)
	})
	t.Run("cross-version managers: changed", func(t *testing.T) {
		t.Parallel()
		cfg := obj(t, smdHPAConfig)
		require.NoError(t, unstructured.SetNestedField(cfg.Object, int64(20), "spec", "maxReplicas"))
		res, err := StructuredMergeDiff(cfg, obj(t, smdHPALive), p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
	})
	t.Run("deployment with defaults and webhook env is not drift", func(t *testing.T) {
		t.Parallel()
		// Through Diff, so that the config is re-encoded (cpu: 0.25 becomes 250m) first.
		res, err := Diff(context.Background(), obj(t, deployConfig), obj(t, deployLive), Options{StructuredMerge: true, GVKParser: p})
		require.NoError(t, err)
		assert.False(t, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)
	})
	t.Run("without a parser lists are atomic", func(t *testing.T) {
		t.Parallel()
		res, err := StructuredMergeDiff(obj(t, deployConfig), obj(t, deployLive), nil, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified, "the webhook-injected env var is replaced with the config list")
	})
	t.Run("unknown kind with a parser", func(t *testing.T) {
		t.Parallel()
		_, err := StructuredMergeDiff(obj(t, widgetConfig), obj(t, widgetLive), p, DefaultManager)
		require.ErrorIs(t, err, ErrNoSchema)
	})
	t.Run("invalid managed fields", func(t *testing.T) {
		t.Parallel()
		live := obj(t, smdServiceLive)
		mf := live.GetManagedFields()
		mf[1].Operation = "Patch"
		live.SetManagedFields(mf)
		_, err := StructuredMergeDiff(svcConfig(nil), live, p, DefaultManager)
		require.Error(t, err)
	})
	t.Run("create and delete", func(t *testing.T) {
		t.Parallel()
		res, err := StructuredMergeDiff(svcConfig(nil), nil, p, DefaultManager)
		require.NoError(t, err)
		assert.True(t, res.Modified)
		res, err = StructuredMergeDiff(nil, obj(t, smdServiceLive), p, DefaultManager)
		require.NoError(t, err)
		assert.False(t, res.Modified)
	})
	t.Run("selected through Diff options", func(t *testing.T) {
		t.Parallel()
		cfg := svcConfig(func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"app": "web"}) })
		res, err := Diff(context.Background(), cfg, obj(t, smdServiceLive), Options{StructuredMerge: true, GVKParser: p})
		require.NoError(t, err)
		assert.True(t, res.Modified)
	})
}

func TestManagerIdentifier(t *testing.T) {
	t.Parallel()
	live := obj(t, smdServiceLive)
	managed, err := decodeManagedFields(live.GetManagedFields())
	require.NoError(t, err)
	require.Len(t, managed, 2)
	key, err := managerIdentifier(live.GetManagedFields()[0])
	require.NoError(t, err)
	assert.JSONEq(t, `{"manager":"patrol-controller","operation":"Apply"}`, key, "appliers are identified without apiVersion and time")
	key, err = managerIdentifier(live.GetManagedFields()[1])
	require.NoError(t, err)
	assert.JSONEq(t, `{"manager":"kubectl-edit","operation":"Update","apiVersion":"v1"}`, key)
}
