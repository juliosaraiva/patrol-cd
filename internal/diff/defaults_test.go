package diff

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
)

// defaulted decodes manifest into its typed form, applies defaults and returns the result
// as unstructured JSON values.
func defaulted(t *testing.T, manifest string) map[string]any {
	t.Helper()
	u := obj(t, manifest)
	typed, err := scheme.Scheme.New(u.GroupVersionKind())
	require.NoError(t, err)
	data, err := json.Marshal(u.Object)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, typed))
	applyDefaults(typed)
	out, err := json.Marshal(typed)
	require.NoError(t, err)
	m, err := decodeObject(out)
	require.NoError(t, err)
	return m
}

func field(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(m, path...)
	require.NoError(t, err)
	require.True(t, ok, "missing %v", path)
	return v
}

func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	d := defaulted(t, deployConfig)
	assert.EqualValues(t, 600, field(t, d, "spec", "progressDeadlineSeconds"))
	assert.EqualValues(t, 10, field(t, d, "spec", "revisionHistoryLimit"))
	assert.Equal(t, "25%", field(t, d, "spec", "strategy", "rollingUpdate", "maxSurge"))
	c := field(t, d, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	assert.Equal(t, "IfNotPresent", c["imagePullPolicy"])
	assert.Equal(t, "/dev/termination-log", c["terminationMessagePath"])
	assert.Equal(t, "TCP", c["ports"].([]any)[0].(map[string]any)["protocol"])
	assert.Equal(t, "250m", c["resources"].(map[string]any)["requests"].(map[string]any)["cpu"])
	assert.Equal(t, "ClusterFirst", field(t, d, "spec", "template", "spec", "dnsPolicy"))

	s := defaulted(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"},"spec":{"type":"LoadBalancer","ports":[{"port":80}]}}`)
	assert.Equal(t, "Cluster", field(t, s, "spec", "externalTrafficPolicy"))
	assert.Equal(t, "Cluster", field(t, s, "spec", "internalTrafficPolicy"))
	assert.Equal(t, true, field(t, s, "spec", "allocateLoadBalancerNodePorts"))
	assert.EqualValues(t, 80, field(t, s, "spec", "ports").([]any)[0].(map[string]any)["targetPort"])
	ext := defaulted(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"},"spec":{"type":"ExternalName","externalName":"x.example"}}`)
	_, ok, _ := unstructured.NestedFieldNoCopy(ext, "spec", "internalTrafficPolicy")
	assert.False(t, ok, "ExternalName services get no internalTrafficPolicy")

	pod := defaulted(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{"hostNetwork":true,"containers":[
		{"name":"c","image":"nginx","ports":[{"containerPort":8080}],"resources":{"limits":{"cpu":"0.0001"}},
		 "livenessProbe":{"httpGet":{"port":8080}}}],
		"volumes":[{"name":"v"},{"name":"s","secret":{"secretName":"x"}}]}}`)
	pc := field(t, pod, "spec", "containers").([]any)[0].(map[string]any)
	assert.Equal(t, "Always", pc["imagePullPolicy"], "untagged images default to Always")
	assert.EqualValues(t, 8080, pc["ports"].([]any)[0].(map[string]any)["hostPort"])
	assert.Equal(t, "1m", pc["resources"].(map[string]any)["limits"].(map[string]any)["cpu"], "quantities round up to milli")
	assert.Equal(t, "1m", pc["resources"].(map[string]any)["requests"].(map[string]any)["cpu"], "requests default to limits on Pods")
	probe := pc["livenessProbe"].(map[string]any)
	assert.EqualValues(t, 3, probe["failureThreshold"])
	assert.Equal(t, "/", probe["httpGet"].(map[string]any)["path"])
	assert.Equal(t, "HTTP", probe["httpGet"].(map[string]any)["scheme"])
	vols := field(t, pod, "spec", "volumes").([]any)
	assert.Contains(t, vols[0].(map[string]any), "emptyDir")
	assert.EqualValues(t, 420, vols[1].(map[string]any)["secret"].(map[string]any)["defaultMode"])
	assert.Equal(t, true, field(t, pod, "spec", "enableServiceLinks"))

	job := defaulted(t, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"j"},"spec":{"template":{"spec":{"containers":[{"name":"c","image":"i:1"}],"restartPolicy":"Never"}}}}`)
	assert.EqualValues(t, 6, field(t, job, "spec", "backoffLimit"))
	assert.EqualValues(t, 1, field(t, job, "spec", "completions"))
	assert.Equal(t, "NonIndexed", field(t, job, "spec", "completionMode"))
	assert.Equal(t, "TerminatingOrFailed", field(t, job, "spec", "podReplacementPolicy"))

	cj := defaulted(t, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"c"},"spec":{"schedule":"* * * * *","jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"c","image":"i:1"}]}}}}}}`)
	assert.Equal(t, "Allow", field(t, cj, "spec", "concurrencyPolicy"))
	assert.EqualValues(t, 3, field(t, cj, "spec", "successfulJobsHistoryLimit"))

	ds := defaulted(t, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"d"},"spec":{"template":{"spec":{"containers":[{"name":"c","image":"i:1"}]}}}}`)
	assert.EqualValues(t, 1, field(t, ds, "spec", "updateStrategy", "rollingUpdate", "maxUnavailable"))

	np := defaulted(t, `{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"n"},"spec":{"podSelector":{},"egress":[{"ports":[{"port":53}]}]}}`)
	assert.Equal(t, []any{"Ingress", "Egress"}, field(t, np, "spec", "policyTypes"))
	assert.Equal(t, "TCP", field(t, np, "spec", "egress").([]any)[0].(map[string]any)["ports"].([]any)[0].(map[string]any)["protocol"])

	hpa := defaulted(t, `{"apiVersion":"autoscaling/v2","kind":"HorizontalPodAutoscaler","metadata":{"name":"h"},"spec":{"maxReplicas":3,"scaleTargetRef":{"kind":"Deployment","name":"d"},"behavior":{"scaleDown":{"stabilizationWindowSeconds":60}}}}`)
	assert.EqualValues(t, 1, field(t, hpa, "spec", "minReplicas"))
	assert.EqualValues(t, 80, field(t, hpa, "spec", "metrics").([]any)[0].(map[string]any)["resource"].(map[string]any)["target"].(map[string]any)["averageUtilization"])
	assert.EqualValues(t, 60, field(t, hpa, "spec", "behavior", "scaleDown", "stabilizationWindowSeconds"))
	assert.Len(t, field(t, hpa, "spec", "behavior", "scaleUp", "policies"), 2)

	sc := defaulted(t, `{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass","metadata":{"name":"s"},"provisioner":"x"}`)
	assert.Equal(t, "Delete", field(t, sc, "reclaimPolicy"))

	cm := defaulted(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"a":"b"}}`)
	assert.Equal(t, map[string]any{"a": "b"}, cm["data"], "kinds without defaults are unchanged")
}

func TestImageTag(t *testing.T) {
	t.Parallel()
	for image, want := range map[string]string{
		"nginx":                               "latest",
		"nginx:latest":                        "latest",
		"nginx:1.27":                          "1.27",
		"registry:5000/team/app":              "latest",
		"registry:5000/team/app:v2":           "v2",
		"app@sha256:abcd":                     "",
		"app:v1@sha256:abcd":                  "v1",
		"registry.example/a/b/c:2026.10.12-1": "2026.10.12-1",
	} {
		assert.Equal(t, want, imageTag(image), image)
	}
}
