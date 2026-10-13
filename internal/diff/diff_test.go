package diff

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const deployConfig = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels:
    app.kubernetes.io/instance: shop
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  strategy:
    type: RollingUpdate
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: registry.example/web:1.4.2
        ports:
        - containerPort: 8080
        env:
        - name: MODE
          value: production
        resources:
          requests:
            cpu: 0.25
            memory: 64Mi
`

// deployLive is deployConfig as returned by the API server: defaulted fields, an env var
// injected by a mutating webhook, generated metadata and status.
const deployLive = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  uid: 0f6c1d64-5f0e-4c3e-9f52-3c2b4e8f1a77
  resourceVersion: "481516"
  generation: 3
  creationTimestamp: "2026-01-02T03:04:05Z"
  labels:
    app.kubernetes.io/instance: shop
  annotations:
    deployment.kubernetes.io/revision: "3"
spec:
  progressDeadlineSeconds: 600
  replicas: 2
  revisionHistoryLimit: 10
  selector:
    matchLabels: {app: web}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxSurge: 25%, maxUnavailable: 25%}
  template:
    metadata:
      creationTimestamp: null
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: registry.example/web:1.4.2
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8080
          protocol: TCP
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef: {apiVersion: v1, fieldPath: metadata.name}
        - name: MODE
          value: production
        resources:
          requests:
            cpu: 250m
            memory: 64Mi
        terminationMessagePath: /dev/termination-log
        terminationMessagePolicy: File
      dnsPolicy: ClusterFirst
      restartPolicy: Always
      schedulerName: default-scheduler
      securityContext: {}
      terminationGracePeriodSeconds: 30
status:
  availableReplicas: 2
  observedGeneration: 3
  readyReplicas: 2
  replicas: 2
  updatedReplicas: 2
`

const serviceConfig = `
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  selector: {app: web}
  ports:
  - name: http
    port: 80
    targetPort: 8080
`

const serviceLive = `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  uid: 6a0c5a2e-1b0a-4e0e-8d0d-9b1f2b7d9c11
  resourceVersion: "77"
  creationTimestamp: "2026-01-02T03:04:05Z"
spec:
  clusterIP: 10.96.12.34
  clusterIPs: [10.96.12.34]
  internalTrafficPolicy: Cluster
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  selector: {app: web}
  ports:
  - name: http
    port: 80
    protocol: TCP
    targetPort: 8080
  sessionAffinity: None
  type: ClusterIP
status:
  loadBalancer: {}
`

const widgetConfig = `
apiVersion: example.com/v1
kind: Widget
metadata:
  name: w1
  namespace: shop
spec:
  size: 3
  colors: [red, blue]
  options:
    mode: fast
`

// widgetLive has a field defaulted by the CRD schema (spec.options.retries) and status.
const widgetLive = `
apiVersion: example.com/v1
kind: Widget
metadata:
  name: w1
  namespace: shop
  uid: 1a2b
  resourceVersion: "9"
  generation: 1
  creationTimestamp: "2026-01-02T03:04:05Z"
spec:
  size: 3
  colors: [red, blue]
  options:
    mode: fast
    retries: 5
status:
  phase: Ready
`

func modify(t *testing.T, manifest string, fn func(u *unstructured.Unstructured)) *unstructured.Unstructured {
	t.Helper()
	u := obj(t, manifest)
	fn(u)
	return u
}

func set(t *testing.T, value any, path ...string) func(*unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, value, path...))
	}
}

func TestDiffLegacy(t *testing.T) {
	t.Parallel()
	dc := func() *unstructured.Unstructured { return obj(t, deployConfig) }
	dl := func() *unstructured.Unstructured { return obj(t, deployLive) }
	wc := func() *unstructured.Unstructured { return obj(t, widgetConfig) }
	wl := func() *unstructured.Unstructured { return obj(t, widgetLive) }

	cases := []struct {
		name     string
		config   *unstructured.Unstructured
		live     *unstructured.Unstructured
		opts     Options
		modified bool
	}{
		{name: "identical objects", config: dc(), live: dc(), modified: false},
		{name: "defaults, webhook env and status are not drift (two-way)", config: dc(), live: dl(), modified: false},
		{name: "defaults, webhook env and status are not drift (three-way)", config: dc(), live: withLastApplied(t, dl(), dc()), modified: false},
		{name: "replicas changed", config: modify(t, deployConfig, set(t, int64(3), "spec", "replicas")), live: dl(), modified: true},
		{name: "image changed", config: modify(t, deployConfig, func(u *unstructured.Unstructured) {
			c := u.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			c["image"] = "registry.example/web:1.5.0"
		}), live: dl(), modified: true},
		{name: "replicas as float equals int", config: modify(t, deployConfig, set(t, float64(2), "spec", "replicas")), live: dl(), modified: false},
		{name: "env var removed from config is detected with last-applied", config: modify(t, deployConfig, func(u *unstructured.Unstructured) {
			c := u.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			delete(c, "env")
		}), live: withLastApplied(t, dl(), dc()), modified: true},
		{name: "env var removed from config is invisible without last-applied", config: modify(t, deployConfig, func(u *unstructured.Unstructured) {
			c := u.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			delete(c, "env")
		}), live: dl(), modified: false},
		{name: "label added", config: modify(t, deployConfig, func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{"app.kubernetes.io/instance": "shop", "tier": "frontend"})
		}), live: dl(), modified: true},
		{name: "label removed with last-applied", config: modify(t, deployConfig, func(u *unstructured.Unstructured) { u.SetLabels(nil) }),
			live: withLastApplied(t, dl(), dc()), modified: true},
		{name: "label only in live without last-applied", config: modify(t, deployConfig, func(u *unstructured.Unstructured) { u.SetLabels(nil) }),
			live: dl(), modified: false},
		{name: "strategy defaults restored after retainKeys patch", config: modify(t, deployConfig, set(t, map[string]any{"type": "RollingUpdate"}, "spec", "strategy")),
			live: withLastApplied(t, dl(), modify(t, deployConfig, set(t, map[string]any{"type": "Recreate"}, "spec", "strategy"))), modified: false},
		{name: "service with allocated clusterIP", config: obj(t, serviceConfig), live: obj(t, serviceLive), modified: false},
		{name: "service port changed", config: modify(t, serviceConfig, set(t, []any{map[string]any{"name": "http", "port": int64(81), "targetPort": int64(8080)}}, "spec", "ports")),
			live: obj(t, serviceLive), modified: true},
		{name: "service empty clusterIP in config", config: modify(t, serviceConfig, set(t, "", "spec", "clusterIP")), live: obj(t, serviceLive), modified: false},
		{name: "custom resource with schema default (two-way)", config: wc(), live: wl(), modified: false},
		{name: "custom resource with schema default (three-way)", config: wc(), live: withLastApplied(t, wl(), wc()), modified: false},
		{name: "custom resource field removed with last-applied", config: modify(t, widgetConfig, func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec", "options")
		}), live: withLastApplied(t, wl(), wc()), modified: true},
		{name: "custom resource list reordered", config: modify(t, widgetConfig, set(t, []any{"blue", "red"}, "spec", "colors")), live: wl(), modified: true},
		{name: "custom resource namespace absent in config", config: modify(t, widgetConfig, func(u *unstructured.Unstructured) { u.SetNamespace("") }),
			live: wl(), modified: false},
		{name: "cluster-scoped custom resource with namespace in config", config: obj(t, `
apiVersion: example.com/v1
kind: ClusterWidget
metadata: {name: cw, namespace: shop}
spec: {a: b}`), live: obj(t, `
apiVersion: example.com/v1
kind: ClusterWidget
metadata: {name: cw, uid: x, resourceVersion: "1", creationTimestamp: "2026-01-02T03:04:05Z"}
spec: {a: b}`), modified: false},
		{name: "null creationTimestamp in config", config: modify(t, widgetConfig, set(t, nil, "metadata", "creationTimestamp")), live: wl(), modified: false},
		{name: "empty annotations map in config", config: modify(t, widgetConfig, set(t, map[string]any{}, "metadata", "annotations")),
			live: withLastApplied(t, wl(), wc()), modified: false},
		{name: "null labels in typed config", config: modify(t, serviceConfig, set(t, nil, "metadata", "labels")), live: obj(t, serviceLive), modified: false},
		{name: "empty list dropped by typed round trip", config: obj(t, `
apiVersion: v1
kind: ServiceAccount
metadata: {name: builder}
imagePullSecrets: []`), live: obj(t, `
apiVersion: v1
kind: ServiceAccount
metadata: {name: builder, namespace: ci, uid: u1, resourceVersion: "5"}`), modified: false},
		{name: "unknown field in built-in kind is kept and reported", config: obj(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: b}
invalidKey: x`), live: obj(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm, namespace: ns}
data: {a: b}`), modified: true},
		{name: "configmap data changed", config: obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"},"data":{"a":"c"}}`),
			live: obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"ns"},"data":{"a":"b"}}`), modified: true},
		{name: "secret stringData matches data", config: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
type: Opaque
stringData: {user: admin, pin: "1234"}
data: {extra: ZXh0cmE=}`), live: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds, namespace: ns, uid: s1, resourceVersion: "3"}
type: Opaque
data: {user: YWRtaW4=, pin: MTIzNA==, extra: ZXh0cmE=}`), modified: false},
		{name: "secret stringData differs", config: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData: {user: root}`), live: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds, namespace: ns}
type: Opaque
data: {user: YWRtaW4=}`), modified: true},
		{name: "secret null value equals empty value", config: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: s}
type: Opaque
data: {token: null}`), live: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: s, namespace: ns}
type: Opaque
data: {token: ""}`), modified: false},
		{name: "redacted secret values still differ", config: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: s}
data: {token: "++++++++"}`), live: obj(t, `
apiVersion: v1
kind: Secret
metadata: {name: s, namespace: ns}
data: {token: "++++++++++++"}`), modified: true},
		{name: "quantity encodings are equal", config: obj(t, `
apiVersion: v1
kind: Pod
metadata: {name: p}
spec:
  containers:
  - {name: c, image: nginx:1.27, resources: {requests: {cpu: 0.5, memory: 1024Mi}}}`), live: obj(t, `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  containers:
  - {name: c, image: nginx:1.27, resources: {requests: {cpu: 500m, memory: 1Gi}}}`), modified: false},
		{name: "quantity changed", config: obj(t, `
apiVersion: v1
kind: Pod
metadata: {name: p}
spec:
  containers:
  - {name: c, image: nginx:1.27, resources: {requests: {cpu: 0.2}}}`), live: obj(t, `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  containers:
  - {name: c, image: nginx:1.27, resources: {requests: {cpu: 0.1}}}`), modified: true},
		{name: "aggregated cluster role rules ignored", config: obj(t, aggrRoleConfig), live: obj(t, aggrRoleLive),
			opts: Options{IgnoreAggregatedRoles: true}, modified: false},
		{name: "aggregated cluster role rules compared by default", config: obj(t, aggrRoleConfig), live: obj(t, aggrRoleLive), modified: true},
		{name: "empty rules equal null rules", config: obj(t, `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: r}
rules: []`), live: obj(t, `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: r, uid: u}
rules: null`), modified: false},
		{name: "endpoints in a different order", config: obj(t, endpointsConfig), live: obj(t, endpointsLive), modified: false},
		{name: "statefulset volume claim template defaults", config: obj(t, stsConfig), live: withLastApplied(t, obj(t, stsLive), obj(t, stsConfig)), modified: false},
		{name: "webhook rules replaced keep defaulted scope", config: obj(t, webhookConfig), live: withLastApplied(t, obj(t, webhookLive), obj(t, webhookConfig)), modified: false},
		{name: "namespace metadata label default", config: obj(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-a","labels":{"team":"a"}}}`),
			live: withLastApplied(t, obj(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-a","labels":{"team":"a","kubernetes.io/metadata.name":"team-a"}},"spec":{"finalizers":["kubernetes"]},"status":{"phase":"Active"}}`),
				obj(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-a"}}`)), modified: false},
		{name: "unparsable last-applied falls back to two-way", config: dc(), live: modify(t, deployLive, func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{AnnotationLastAppliedConfig: "{not json"})
		}), modified: false},
		{name: "structured merge selected by sync-options annotation", config: modify(t, deployConfig, func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{DefaultSyncOptionsAnnotation: "Validate=false, ServerSideApply=true"})
		}), live: modify(t, deployLive, func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{DefaultSyncOptionsAnnotation: "Validate=false, ServerSideApply=true"})
		}), opts: Options{GVKParser: testParser(t)}, modified: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configBefore, liveBefore := tc.config.DeepCopy(), tc.live.DeepCopy()
			res := mustDiff(t, tc.config, tc.live, tc.opts)
			if !assert.Equal(t, tc.modified, res.Modified) {
				t.Logf("normalized live: %s\npredicted live: %s", res.NormalizedLive, res.PredictedLive)
			}
			assert.Equal(t, configBefore, tc.config, "config must not be mutated")
			assert.Equal(t, liveBefore, tc.live, "live must not be mutated")
		})
	}
}

func TestDiffUnknownFieldVisibleInPrediction(t *testing.T) {
	t.Parallel()
	config := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"},"invalidKey":"x"}`)
	live := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"ns"}}`)
	res := mustDiff(t, config, live, Options{})
	assert.True(t, res.Modified)
	assert.Contains(t, string(res.PredictedLive), "invalidKey")
	assert.NotContains(t, string(res.NormalizedLive), "invalidKey")
}

// TestDiffThreeWaySequence walks through the life cycle of an annotation: defaults are not
// drift, last-applied makes removals visible, and without it removals are invisible.
func TestDiffThreeWaySequence(t *testing.T) {
	t.Parallel()
	config := obj(t, deployConfig)
	config.SetNamespace("")
	config.SetAnnotations(map[string]string{"owner": "team-a"})
	live := config.DeepCopy()
	live.SetNamespace("shop")

	assert.False(t, mustDiff(t, config, live, Options{}).Modified, "namespace added by the server")

	live = withLastApplied(t, live, config)
	assert.False(t, mustDiff(t, config, live, Options{}).Modified, "with last-applied")

	trimmed := config.DeepCopy()
	trimmed.SetAnnotations(nil)
	assert.True(t, mustDiff(t, trimmed, live, Options{}).Modified, "annotation removed from config")

	ann := live.GetAnnotations()
	delete(ann, AnnotationLastAppliedConfig)
	live.SetAnnotations(ann)
	assert.False(t, mustDiff(t, trimmed, live, Options{}).Modified, "removal invisible without last-applied")
}

func TestDiffCreateAndDelete(t *testing.T) {
	t.Parallel()
	d := obj(t, deployConfig)

	res := mustDiff(t, nil, d, Options{})
	assert.False(t, res.Modified, "extraneous live object is not modified by itself")
	assert.Equal(t, "null", string(res.PredictedLive))

	res = mustDiff(t, d, nil, Options{})
	assert.True(t, res.Modified)
	assert.Equal(t, "null", string(res.NormalizedLive))

	_, err := Diff(context.Background(), nil, nil, Options{})
	require.ErrorIs(t, err, ErrNilObjects)

	res, err = TwoWayDiff(nil, d)
	require.NoError(t, err)
	assert.False(t, res.Modified)
	res, err = TwoWayDiff(d, nil)
	require.NoError(t, err)
	assert.True(t, res.Modified)

	// Server-side diff does not call the API server for creations and deletions.
	o := Options{ServerSide: true, DryRunner: failingDryRunner{}}
	res = mustDiff(t, d, nil, o)
	assert.True(t, res.Modified)
	res = mustDiff(t, nil, d, o)
	assert.False(t, res.Modified)
}

func TestDiffNilSpecInLive(t *testing.T) {
	t.Parallel()
	live := obj(t, deployConfig)
	require.NoError(t, unstructured.SetNestedField(live.Object, nil, "spec"))
	assert.True(t, mustDiff(t, obj(t, deployConfig), live, Options{}).Modified)
}

func TestDiffCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Diff(ctx, obj(t, deployConfig), obj(t, deployLive), Options{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiffArray(t *testing.T) {
	t.Parallel()
	same := []*unstructured.Unstructured{obj(t, deployConfig), obj(t, serviceConfig)}
	lives := []*unstructured.Unstructured{obj(t, deployLive), obj(t, serviceLive)}
	list, err := DiffArray(context.Background(), same, lives, Options{})
	require.NoError(t, err)
	assert.False(t, list.Modified)
	require.Len(t, list.Diffs, 2)

	changed := []*unstructured.Unstructured{obj(t, deployConfig), modify(t, serviceConfig, set(t, "LoadBalancer", "spec", "type"))}
	list, err = DiffArray(context.Background(), changed, lives, Options{})
	require.NoError(t, err)
	assert.True(t, list.Modified)
	assert.False(t, list.Diffs[0].Modified)
	assert.True(t, list.Diffs[1].Modified)

	_, err = DiffArray(context.Background(), same, lives[:1], Options{})
	require.ErrorIs(t, err, ErrLengthMismatch)
}

func TestDiffServiceReorderedPorts(t *testing.T) {
	t.Parallel()
	ports := func(order ...int64) []any {
		out := make([]any, 0, len(order))
		for _, p := range order {
			out = append(out, map[string]any{"name": "p" + strings.Repeat("x", int(p%7)), "port": p, "protocol": "TCP", "targetPort": p})
		}
		return out
	}
	config := modify(t, serviceConfig, set(t, ports(80, 443), "spec", "ports"))
	live := modify(t, serviceLive, set(t, ports(443, 80), "spec", "ports"))
	// kubectl apply would reorder the list to the config order, so this is a difference.
	assert.True(t, mustDiff(t, config, live, Options{}).Modified)
	assert.False(t, mustDiff(t, config, modify(t, serviceLive, set(t, ports(80, 443), "spec", "ports")), Options{}).Modified)
}

type failingDryRunner struct{}

func (failingDryRunner) ServerSideApplyDryRun(context.Context, *unstructured.Unstructured, string) (*unstructured.Unstructured, error) {
	return nil, errors.New("dry run must not be called")
}

const aggrRoleConfig = `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: monitoring-view
aggregationRule:
  clusterRoleSelectors:
  - matchLabels: {rbac.example.com/aggregate-to-monitoring: "true"}
rules: []
`

const aggrRoleLive = `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: monitoring-view
  uid: 3c1e
  resourceVersion: "12"
aggregationRule:
  clusterRoleSelectors:
  - matchLabels: {rbac.example.com/aggregate-to-monitoring: "true"}
rules:
- apiGroups: [""]
  resources: [pods, services]
  verbs: [get, list, watch]
`

const endpointsConfig = `
apiVersion: v1
kind: Endpoints
metadata: {name: legacy-db}
subsets:
- addresses:
  - ip: 10.0.0.2
  - ip: 10.0.0.1
  ports:
  - {name: sql, port: 5432}
  - {name: metrics, port: 9187}
`

const endpointsLive = `
apiVersion: v1
kind: Endpoints
metadata: {name: legacy-db, namespace: data, uid: e1, resourceVersion: "4"}
subsets:
- addresses:
  - ip: 10.0.0.1
  - ip: 10.0.0.2
  ports:
  - {name: metrics, port: 9187, protocol: TCP}
  - {name: sql, port: 5432, protocol: TCP}
`

const stsConfig = `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: data}
spec:
  serviceName: db
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers:
      - {name: db, image: postgres:17}
  volumeClaimTemplates:
  - metadata: {name: data}
    spec:
      accessModes: [ReadWriteOnce]
      resources: {requests: {storage: 1Gi}}
`

const stsLive = `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: data, uid: s1, resourceVersion: "8", generation: 1}
spec:
  serviceName: db
  replicas: 1
  podManagementPolicy: OrderedReady
  revisionHistoryLimit: 10
  persistentVolumeClaimRetentionPolicy: {whenDeleted: Retain, whenScaled: Retain}
  updateStrategy: {type: RollingUpdate, rollingUpdate: {partition: 0}}
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers:
      - name: db
        image: postgres:17
        imagePullPolicy: IfNotPresent
        terminationMessagePath: /dev/termination-log
        terminationMessagePolicy: File
        resources: {}
      dnsPolicy: ClusterFirst
      restartPolicy: Always
      schedulerName: default-scheduler
      securityContext: {}
      terminationGracePeriodSeconds: 30
  volumeClaimTemplates:
  - apiVersion: v1
    kind: PersistentVolumeClaim
    metadata: {name: data}
    spec:
      accessModes: [ReadWriteOnce]
      resources: {requests: {storage: 1Gi}}
      volumeMode: Filesystem
    status: {phase: Pending}
`

const webhookConfig = `
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata: {name: injector}
webhooks:
- name: inject.example.com
  admissionReviewVersions: [v1]
  sideEffects: None
  clientConfig:
    service: {name: injector, namespace: system, path: /mutate}
  rules:
  - apiGroups: [""]
    apiVersions: [v1]
    operations: [CREATE]
    resources: [pods]
`

const webhookLive = `
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata: {name: injector, uid: m1, resourceVersion: "6", generation: 2}
webhooks:
- name: inject.example.com
  admissionReviewVersions: [v1]
  sideEffects: None
  clientConfig:
    caBundle: Y2EtYnVuZGxl
    service: {name: injector, namespace: system, path: /mutate, port: 443}
  failurePolicy: Fail
  matchPolicy: Equivalent
  namespaceSelector: {}
  objectSelector: {}
  reinvocationPolicy: Never
  timeoutSeconds: 10
  rules:
  - apiGroups: [""]
    apiVersions: [v1]
    operations: [CREATE]
    resources: [pods]
    scope: "*"
`
