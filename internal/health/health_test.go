package health

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func loadFixture(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &obj))
	return &unstructured.Unstructured{Object: obj}
}

func mustYAML(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
	return &unstructured.Unstructured{Object: obj}
}

// TestAssessFixtures runs every upstream gitops-engine health fixture with the expectation
// asserted by upstream's health_test.go.
func TestAssessFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file string
		want Status // "" means no health (nil result)
		msg  *string
	}{
		{file: "deployment-nginx.yaml", want: StatusHealthy},
		{file: "deployment-progressing.yaml", want: StatusProgressing},
		{file: "deployment-suspended.yaml", want: StatusSuspended, msg: ptr("Deployment is paused")},
		{file: "deployment-degraded.yaml", want: StatusDegraded},
		{file: "statefulset.yaml", want: StatusHealthy},
		{file: "statefulset-ondelete.yaml", want: StatusHealthy},
		{file: "daemonset-ondelete.yaml", want: StatusHealthy},
		{file: "pvc-bound.yaml", want: StatusHealthy},
		{file: "pvc-pending.yaml", want: StatusProgressing},
		{file: "svc-clusterip.yaml", want: StatusHealthy},
		{file: "svc-loadbalancer.yaml", want: StatusHealthy},
		{file: "svc-loadbalancer-unassigned.yaml", want: StatusProgressing},
		{file: "svc-loadbalancer-nonemptylist.yaml", want: StatusHealthy},
		{file: "ingress.yaml", want: StatusHealthy},
		{file: "ingress-unassigned.yaml", want: StatusProgressing},
		{file: "ingress-nonemptylist.yaml", want: StatusHealthy},
		{file: "knative-service.yaml", want: ""},
		{file: "job-running.yaml", want: StatusProgressing},
		{file: "job-failed.yaml", want: StatusDegraded},
		{file: "job-succeeded.yaml", want: StatusHealthy},
		{file: "job-suspended.yaml", want: StatusSuspended},
		{file: "hpa-v2-healthy.yaml", want: StatusHealthy},
		{file: "hpa-v2-degraded.yaml", want: StatusDegraded},
		{file: "hpa-v2-progressing.yaml", want: StatusProgressing},
		{file: "hpa-v1-degraded.yaml", want: StatusDegraded},
		{file: "hpa-v1-degraded-failedgetobjectmetric.yaml", want: StatusDegraded},
		{file: "hpa-v1-healthy.yaml", want: StatusHealthy},
		{file: "hpa-v1-healthy-toofew.yaml", want: StatusHealthy},
		{file: "hpa-v1-progressing.yaml", want: StatusProgressing},
		{file: "hpa-v1-progressing-with-no-annotations.yaml", want: StatusProgressing, msg: ptr("Waiting to Autoscale")},
		{file: "pod-pending.yaml", want: StatusProgressing},
		{file: "pod-running-not-ready.yaml", want: StatusProgressing},
		{file: "pod-crashloop.yaml", want: StatusDegraded},
		{file: "pod-imagepullbackoff.yaml", want: StatusDegraded},
		{file: "pod-error.yaml", want: StatusDegraded},
		{file: "pod-running-restart-always.yaml", want: StatusHealthy},
		{file: "pod-running-restart-never.yaml", want: StatusProgressing},
		{file: "pod-running-restart-onfailure.yaml", want: StatusProgressing},
		{file: "pod-failed.yaml", want: StatusDegraded},
		{file: "pod-succeeded.yaml", want: StatusHealthy},
		{file: "pod-deletion.yaml", want: StatusProgressing, msg: ptr("Pending deletion")},
		// Application health is not built in (upstream removed it in v1.8).
		{file: "application-healthy.yaml", want: ""},
		{file: "application-degraded.yaml", want: ""},
		{file: "patrol-application-healthy.yaml", want: ""},
		{file: "patrol-application-degraded.yaml", want: ""},
		{file: "apiservice-v1-true.yaml", want: StatusHealthy, msg: ptr("Passed: all checks passed")},
		{file: "apiservice-v1-false.yaml", want: StatusProgressing},
		{file: "apiservice-v1beta1-true.yaml", want: StatusHealthy},
		{file: "apiservice-v1beta1-false.yaml", want: StatusProgressing},
	}
	seen := map[string]bool{}
	for _, tc := range tests {
		seen[tc.file] = true
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(loadFixture(t, tc.file), nil)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, hs)
				return
			}
			require.NotNil(t, hs)
			assert.Equal(t, tc.want, hs.Status)
			if tc.msg != nil {
				assert.Equal(t, *tc.msg, hs.Message)
			}
		})
	}
	// Every YAML fixture except the Patrol "nohealth" one must be covered above.
	entries, err := os.ReadDir("testdata")
	require.NoError(t, err)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" || e.Name() == "patrol-application-nohealth.yaml" {
			continue
		}
		assert.True(t, seen[e.Name()], "fixture %s has no expectation", e.Name())
	}
}

func ptr[T any](v T) *T { return &v }

func TestArgoWorkflowHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase   string
		message string
		want    Status
	}{
		{"", "", StatusProgressing},
		{"Pending", "waiting", StatusProgressing},
		{"Running", "This node is running", StatusProgressing},
		{"Succeeded", "This node is has succeeded", StatusHealthy},
		{"Failed", "boom", StatusDegraded},
		{"Error", "err", StatusDegraded},
		{"Omitted", "x", StatusUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "argoproj.io/v1alpha1",
				"kind":       "Workflow",
				"spec":       map[string]any{"entrypoint": "main", "extraneousKey": "ignored"},
			}}
			if tc.phase != "" {
				obj.Object["status"] = map[string]any{"phase": tc.phase, "message": tc.message}
			}
			hs, err := Assess(obj, nil)
			require.NoError(t, err)
			assert.Equal(t, &HealthStatus{Status: tc.want, Message: tc.message}, hs)
		})
	}
}

func TestDeploymentHealthMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want HealthStatus
	}{
		{
			name: "generation not observed",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 3}
status: {observedGeneration: 2}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: observed deployment generation less than desired generation"},
		},
		{
			name: "progress deadline exceeded wins",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 1}
spec: {replicas: 3}
status:
  observedGeneration: 1
  updatedReplicas: 1
  conditions: [{type: Progressing, status: "False", reason: ProgressDeadlineExceeded}]`,
			want: HealthStatus{StatusDegraded, `Deployment "d" exceeded its progress deadline`},
		},
		{
			name: "updated replicas behind spec",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 1}
spec: {replicas: 3}
status: {observedGeneration: 1, updatedReplicas: 1, replicas: 3}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: 1 out of 3 new replicas have been updated..."},
		},
		{
			name: "old replicas pending termination",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 1}
spec: {replicas: 2}
status: {observedGeneration: 1, updatedReplicas: 2, replicas: 3}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: 1 old replicas are pending termination..."},
		},
		{
			name: "updated replicas not available",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 1}
spec: {replicas: 2}
status: {observedGeneration: 1, updatedReplicas: 2, replicas: 2, availableReplicas: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: 1 of 2 updated replicas are available..."},
		},
		{
			name: "paused beats everything",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 5}
spec: {paused: true}
status: {observedGeneration: 1}`,
			want: HealthStatus{StatusSuspended, "Deployment is paused"},
		},
		{
			name: "healthy",
			doc: `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, generation: 1}
spec: {replicas: 2}
status: {observedGeneration: 1, updatedReplicas: 2, replicas: 2, availableReplicas: 2}`,
			want: HealthStatus{Status: StatusHealthy},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, tc.doc), nil)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, hs)
		})
	}
}

func TestStatefulSetDaemonSetReplicaSetMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want HealthStatus
	}{
		{
			name: "sts generation zero",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s}
status: {observedGeneration: 0}`,
			want: HealthStatus{StatusProgressing, "Waiting for statefulset spec update to be observed..."},
		},
		{
			name: "sts pods not ready",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s, generation: 1}
spec: {replicas: 3}
status: {observedGeneration: 1, readyReplicas: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for 2 pods to be ready..."},
		},
		{
			name: "sts partitioned in progress",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s, generation: 1}
spec:
  replicas: 3
  updateStrategy: {type: RollingUpdate, rollingUpdate: {partition: 1}}
status: {observedGeneration: 1, readyReplicas: 3, updatedReplicas: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for partitioned roll out to finish: 1 out of 2 new pods have been updated..."},
		},
		{
			name: "sts partitioned complete",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s, generation: 1}
spec:
  replicas: 3
  updateStrategy: {type: RollingUpdate, rollingUpdate: {partition: 1}}
status: {observedGeneration: 1, readyReplicas: 3, updatedReplicas: 2}`,
			want: HealthStatus{StatusHealthy, "partitioned roll out complete: 2 new pods have been updated..."},
		},
		{
			name: "sts revision mismatch",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s, generation: 1}
spec: {replicas: 1}
status: {observedGeneration: 1, readyReplicas: 1, updatedReplicas: 0, currentRevision: a, updateRevision: b}`,
			want: HealthStatus{StatusProgressing, "waiting for statefulset rolling update to complete 0 pods at revision b..."},
		},
		{
			name: "sts rolling complete",
			doc: `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: s, generation: 1}
spec: {replicas: 1}
status: {observedGeneration: 1, readyReplicas: 1, currentReplicas: 1, currentRevision: a, updateRevision: a}`,
			want: HealthStatus{StatusHealthy, "statefulset rolling update complete 1 pods at revision a..."},
		},
		{
			name: "ds updating",
			doc: `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds, generation: 1}
status: {observedGeneration: 1, desiredNumberScheduled: 3, updatedNumberScheduled: 1}`,
			want: HealthStatus{StatusProgressing, `Waiting for daemon set "ds" rollout to finish: 1 out of 3 new pods have been updated...`},
		},
		{
			name: "ds not available",
			doc: `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds, generation: 1}
status: {observedGeneration: 1, desiredNumberScheduled: 3, updatedNumberScheduled: 3, numberAvailable: 2}`,
			want: HealthStatus{StatusProgressing, `Waiting for daemon set "ds" rollout to finish: 2 of 3 updated pods are available...`},
		},
		{
			name: "ds generation",
			doc: `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds, generation: 2}
status: {observedGeneration: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: observed daemon set generation less than desired generation"},
		},
		{
			name: "ds healthy",
			doc: `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds, generation: 1}
status: {observedGeneration: 1, desiredNumberScheduled: 3, updatedNumberScheduled: 3, numberAvailable: 3}`,
			want: HealthStatus{Status: StatusHealthy},
		},
		{
			name: "rs replica failure",
			doc: `apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, generation: 1}
spec: {replicas: 2}
status:
  observedGeneration: 1
  conditions: [{type: ReplicaFailure, status: "True", message: quota exceeded}]`,
			want: HealthStatus{StatusDegraded, "quota exceeded"},
		},
		{
			name: "rs waiting",
			doc: `apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, generation: 1}
spec: {replicas: 2}
status: {observedGeneration: 1, availableReplicas: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: 1 out of 2 new replicas are available..."},
		},
		{
			name: "rs generation",
			doc: `apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, generation: 2}
status: {observedGeneration: 1}`,
			want: HealthStatus{StatusProgressing, "Waiting for rollout to finish: observed replica set generation less than desired generation"},
		},
		{
			name: "rs healthy",
			doc: `apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, generation: 1}
spec: {replicas: 2}
status: {observedGeneration: 1, availableReplicas: 2}`,
			want: HealthStatus{Status: StatusHealthy},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, tc.doc), nil)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, hs)
		})
	}
}

func TestPodHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want HealthStatus
	}{
		{
			name: "always: waiting error degrades and joins messages",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Always}
status:
  phase: Running
  containerStatuses:
  - {name: a, state: {waiting: {reason: CrashLoopBackOff, message: back-off a}}}
  - {name: b, state: {waiting: {reason: ContainerCreating}}}
  - {name: c, state: {waiting: {reason: CreateContainerConfigError, message: bad config}}}
  - {name: d, state: {waiting: {reason: ErrImagePull, message: pull failed}}}`,
			want: HealthStatus{StatusDegraded, "back-off a, bad config, pull failed"},
		},
		{
			name: "never: waiting error ignored (hook safety)",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Pending
  message: waiting
  containerStatuses:
  - {name: a, state: {waiting: {reason: ImagePullBackOff, message: back-off}}}`,
			want: HealthStatus{StatusProgressing, "waiting"},
		},
		{
			name: "onfailure running is progressing",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: OnFailure}
status: {phase: Running, message: m}`,
			want: HealthStatus{StatusProgressing, "m"},
		},
		{
			name: "always running ready is healthy",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Always}
status:
  phase: Running
  conditions: [{type: Ready, status: "True"}]`,
			want: HealthStatus{Status: StatusHealthy},
		},
		{
			name: "always running not ready with previous termination is degraded",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Always}
status:
  phase: Running
  message: restarting
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: a, lastState: {terminated: {exitCode: 1}}, state: {running: {}}}`,
			want: HealthStatus{StatusDegraded, "restarting"},
		},
		{
			name: "always running not ready is progressing",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Always}
status:
  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses: [{name: a, state: {running: {}}}]`,
			want: HealthStatus{Status: StatusProgressing},
		},
		{
			name: "failed uses pod message first",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Failed
  message: evicted
  containerStatuses: [{name: a, state: {terminated: {exitCode: 2}}}]`,
			want: HealthStatus{StatusDegraded, "evicted"},
		},
		{
			name: "failed prefers init container failure",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Failed
  initContainerStatuses: [{name: init, state: {terminated: {exitCode: 3}}}]
  containerStatuses: [{name: main, state: {terminated: {exitCode: 1, message: main failed}}}]`,
			want: HealthStatus{StatusDegraded, `container "init" failed with exit code 3`},
		},
		{
			name: "failed termination message",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Failed
  containerStatuses: [{name: main, state: {terminated: {exitCode: 1, message: boom}}}]`,
			want: HealthStatus{StatusDegraded, "boom"},
		},
		{
			name: "failed oomkilled",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Failed
  containerStatuses: [{name: main, state: {terminated: {exitCode: 137, reason: OOMKilled}}}]`,
			want: HealthStatus{StatusDegraded, "OOMKilled"},
		},
		{
			name: "failed without detail",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status:
  phase: Failed
  containerStatuses: [{name: main, state: {terminated: {exitCode: 0}}}]`,
			want: HealthStatus{Status: StatusDegraded},
		},
		{
			name: "succeeded",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}
status: {phase: Succeeded, message: done}`,
			want: HealthStatus{StatusHealthy, "done"},
		},
		{
			name: "unknown phase",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Always}
status: {phase: Unknown, message: node lost}`,
			want: HealthStatus{StatusUnknown, "node lost"},
		},
		{
			name: "no phase",
			doc: `apiVersion: v1
kind: Pod
metadata: {name: p}
spec: {restartPolicy: Never}`,
			want: HealthStatus{Status: StatusUnknown},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, tc.doc), nil)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, hs)
		})
	}
}

func TestJobHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		conds string
		want  HealthStatus
	}{
		{"no conditions", `[]`, HealthStatus{Status: StatusProgressing}},
		{"complete", `[{type: Complete, status: "True", message: ok}]`, HealthStatus{StatusHealthy, "ok"}},
		{"failed", `[{type: Failed, status: "True", message: BackoffLimitExceeded}]`, HealthStatus{StatusDegraded, "BackoffLimitExceeded"}},
		{"failed wins over suspended", `[{type: Suspended, status: "True", message: s}, {type: Failed, status: "True", message: f}]`, HealthStatus{StatusDegraded, "f"}},
		{"suspended reports failure message (empty)", `[{type: Suspended, status: "True", message: Job suspended}]`, HealthStatus{Status: StatusSuspended}},
		{"suspended false then complete", `[{type: Suspended, status: "False", message: resumed}]`, HealthStatus{StatusHealthy, "resumed"}},
		{"failure target alone is still running", `[{type: FailureTarget, status: "True", message: x}]`, HealthStatus{Status: StatusProgressing}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, "apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\nstatus:\n  conditions: "+tc.conds), nil)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, hs)
		})
	}
}

func TestHPAHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want HealthStatus
	}{
		{
			name: "v2 degraded wins over healthy",
			doc: `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: h}
status:
  conditions:
  - {type: AbleToScale, status: "True", reason: SucceededGetScale, message: ok}
  - {type: ScalingActive, status: "False", reason: FailedGetExternalMetric, message: no metric}`,
			want: HealthStatus{StatusDegraded, "no metric"},
		},
		{
			name: "v2 scaling limited healthy",
			doc: `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: h}
status:
  conditions: [{type: ScalingLimited, status: "True", reason: TooFewReplicas, message: limited}]`,
			want: HealthStatus{StatusHealthy, "limited"},
		},
		{
			name: "v1 empty annotation list",
			doc: `apiVersion: autoscaling/v1
kind: HorizontalPodAutoscaler
metadata:
  name: h
  annotations: {autoscaling.alpha.kubernetes.io/conditions: "[]"}`,
			want: HealthStatus{StatusProgressing, "Waiting to Autoscale"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, tc.doc), nil)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, hs)
		})
	}
}

func TestAPIServiceWithoutConditions(t *testing.T) {
	t.Parallel()
	hs, err := Assess(mustYAML(t, "apiVersion: apiregistration.k8s.io/v1\nkind: APIService\nmetadata: {name: a}"), nil)
	require.NoError(t, err)
	assert.Equal(t, &HealthStatus{StatusProgressing, "Waiting to be processed"}, hs)
}

func TestAssessErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		doc     string
		wantErr error
		wantMsg string
	}{
		{
			name:    "unsupported deployment version",
			doc:     "apiVersion: apps/v1beta2\nkind: Deployment\nmetadata: {name: d}",
			wantErr: ErrUnsupportedVersion,
			wantMsg: "unsupported Deployment GVK: apps/v1beta2, Kind=Deployment",
		},
		{
			name:    "unsupported hpa version",
			doc:     "apiVersion: autoscaling/v2beta2\nkind: HorizontalPodAutoscaler\nmetadata: {name: h}",
			wantErr: ErrUnsupportedVersion,
			wantMsg: "unsupported HPA GVK: autoscaling/v2beta2, Kind=HorizontalPodAutoscaler",
		},
		{
			name:    "malformed deployment",
			doc:     "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {replicas: three}",
			wantErr: ErrMalformedObject,
		},
		{
			name:    "malformed hpa annotation",
			doc:     "apiVersion: autoscaling/v1\nkind: HorizontalPodAutoscaler\nmetadata: {name: h, annotations: {autoscaling.alpha.kubernetes.io/conditions: 'not json'}}",
			wantErr: ErrMalformedObject,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, err := Assess(mustYAML(t, tc.doc), nil)
			require.ErrorIs(t, err, tc.wantErr)
			require.NotNil(t, hs)
			assert.Equal(t, StatusUnknown, hs.Status)
			assert.Equal(t, err.Error(), hs.Message)
			if tc.wantMsg != "" {
				assert.Equal(t, tc.wantMsg, hs.Message)
			}
		})
	}

	_, err := Assess(nil, nil)
	require.ErrorIs(t, err, ErrNilObject)
}

type fakeOverrides struct {
	hs  *HealthStatus
	err error
}

func (f fakeOverrides) GetResourceHealth(*unstructured.Unstructured) (*HealthStatus, error) {
	return f.hs, f.err
}

func TestAssessOverrides(t *testing.T) {
	t.Parallel()
	pod := loadFixture(t, "pod-running-restart-always.yaml")

	t.Run("override wins over builtin", func(t *testing.T) {
		t.Parallel()
		hs, err := Assess(pod, fakeOverrides{hs: &HealthStatus{StatusDegraded, "custom"}})
		require.NoError(t, err)
		assert.Equal(t, &HealthStatus{StatusDegraded, "custom"}, hs)
	})
	t.Run("nil override falls back to builtin", func(t *testing.T) {
		t.Parallel()
		hs, err := Assess(pod, fakeOverrides{})
		require.NoError(t, err)
		assert.Equal(t, StatusHealthy, hs.Status)
	})
	t.Run("override error is Unknown", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("lua boom")
		hs, err := Assess(pod, fakeOverrides{err: boom})
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, err, ErrOverride)
		assert.Equal(t, &HealthStatus{StatusUnknown, "lua boom"}, hs)
	})
	t.Run("deletion beats override", func(t *testing.T) {
		t.Parallel()
		hs, err := Assess(loadFixture(t, "pod-deletion.yaml"), fakeOverrides{hs: &HealthStatus{Status: StatusHealthy}})
		require.NoError(t, err)
		assert.Equal(t, &HealthStatus{StatusProgressing, "Pending deletion"}, hs)
	})
	t.Run("hook finalizer keeps assessing while deleting", func(t *testing.T) {
		t.Parallel()
		obj := loadFixture(t, "pod-deletion.yaml")
		obj.SetFinalizers(append(obj.GetFinalizers(), HookFinalizer))
		hs, err := Assess(obj, fakeOverrides{hs: &HealthStatus{Status: StatusHealthy}})
		require.NoError(t, err)
		assert.Equal(t, StatusHealthy, hs.Status)
	})
}

func TestApplicationHealthOptIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file string
		want *HealthStatus
	}{
		{"patrol-application-healthy.yaml", &HealthStatus{Status: StatusHealthy}},
		{"patrol-application-degraded.yaml", &HealthStatus{StatusDegraded, "one or more resources are degraded"}},
		{"patrol-application-nohealth.yaml", &HealthStatus{Status: StatusProgressing}},
		// Argo CD Applications are a different group and are not covered.
		{"application-healthy.yaml", nil},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			obj := loadFixture(t, tc.file)
			off, err := Assess(obj, nil)
			require.NoError(t, err)
			assert.Nil(t, off, "application health must be disabled by default")

			on, err := Assess(obj, WithApplicationHealth(nil))
			require.NoError(t, err)
			assert.Equal(t, tc.want, on)
		})
	}

	t.Run("invalid status is Unknown", func(t *testing.T) {
		t.Parallel()
		obj := mustYAML(t, "apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {health: {status: Bogus}}")
		assert.Equal(t, &HealthStatus{StatusUnknown, "Lua returned an invalid health status"}, ApplicationHealth(obj))
	})
	t.Run("configured override takes precedence", func(t *testing.T) {
		t.Parallel()
		obj := loadFixture(t, "patrol-application-healthy.yaml")
		hs, err := Assess(obj, WithApplicationHealth(fakeOverrides{hs: &HealthStatus{Status: StatusSuspended}}))
		require.NoError(t, err)
		assert.Equal(t, StatusSuspended, hs.Status)
	})
}

func TestHasBuiltinCheck(t *testing.T) {
	t.Parallel()
	for _, gk := range []struct{ g, k string }{
		{"", "Pod"}, {"", "Service"}, {"", "PersistentVolumeClaim"},
		{"apps", "Deployment"}, {"apps", "StatefulSet"}, {"apps", "ReplicaSet"}, {"apps", "DaemonSet"},
		{"extensions", "Ingress"}, {"networking.k8s.io", "Ingress"}, {"argoproj.io", "Workflow"},
		{"apiregistration.k8s.io", "APIService"}, {"batch", "Job"}, {"autoscaling", "HorizontalPodAutoscaler"},
	} {
		assert.True(t, HasBuiltinCheck(schemaGK(gk.g, gk.k)), "%s/%s", gk.g, gk.k)
	}
	for _, gk := range []struct{ g, k string }{
		{"batch", "CronJob"}, {"", "Namespace"}, {"argoproj.io", "Application"}, {ApplicationGroup, ApplicationKind},
	} {
		assert.False(t, HasBuiltinCheck(schemaGK(gk.g, gk.k)), "%s/%s", gk.g, gk.k)
	}
}
