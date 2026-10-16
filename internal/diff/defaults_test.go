package diff

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestApplyDefaultsVerifiedAgainstAPIServer(t *testing.T) {
	t.Parallel()
	// Values below were confirmed against kube-apiserver 1.36 (TestEnvtestDefaultsConformance).
	d := defaulted(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d"},"spec":{"template":{"spec":{
		"containers":[{"name":"c","image":"a:1","startupProbe":{"grpc":{"port":81}}}],
		"volumes":[{"name":"i","iscsi":{"targetPortal":"t","iqn":"q","lun":0}},{"name":"r","rbd":{"monitors":["m"],"image":"x"}},
		{"name":"az","azureDisk":{"diskName":"d","diskURI":"u"}},{"name":"s","scaleIO":{"gateway":"g","system":"s","volumeName":"v","secretRef":{"name":"x"}}}]}}}}`)
	c := field(t, d, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	assert.Equal(t, "", c["startupProbe"].(map[string]any)["grpc"].(map[string]any)["service"])
	vols := field(t, d, "spec", "template", "spec", "volumes").([]any)
	assert.Equal(t, "default", vols[0].(map[string]any)["iscsi"].(map[string]any)["iscsiInterface"])
	assert.Equal(t, map[string]any{"monitors": []any{"m"}, "image": "x", "pool": "rbd", "user": "admin", "keyring": "/etc/ceph/keyring"}, vols[1].(map[string]any)["rbd"])
	assert.Equal(t, map[string]any{"diskName": "d", "diskURI": "u", "cachingMode": "ReadWrite", "fsType": "ext4", "kind": "Shared", "readOnly": false}, vols[2].(map[string]any)["azureDisk"])
	assert.Equal(t, "ThinProvisioned", vols[3].(map[string]any)["scaleIO"].(map[string]any)["storageMode"])

	svc := defaulted(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"},"spec":{"externalIPs":["192.0.2.1"],"ports":[{"port":80}]}}`)
	assert.Equal(t, "Cluster", field(t, svc, "spec", "externalTrafficPolicy"), "ClusterIP with external IPs is externally accessible")

	lr := defaulted(t, `{"apiVersion":"v1","kind":"LimitRange","metadata":{"name":"l"},"spec":{"limits":[{"type":"Container","max":{"cpu":"2"},"min":{"cpu":"0.1","memory":"0.5Mi"}}]}}`)
	item := field(t, lr, "spec", "limits").([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"cpu": "2"}, item["default"])
	assert.Equal(t, map[string]any{"cpu": "2", "memory": "512Ki"}, item["defaultRequest"], "default request falls back to default limit, then min")

	pod := defaulted(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{"resources":{"limits":{"cpu":"2","memory":"256Mi","hugepages-2Mi":"4Mi"}},
		"initContainers":[{"name":"side","image":"s:1","restartPolicy":"Always","resources":{"requests":{"cpu":"100m"}}},{"name":"init","image":"i:1","resources":{"requests":{"cpu":"1500m","memory":"10Mi"}}}],
		"containers":[{"name":"c","image":"n:1","resources":{"requests":{"cpu":"250m","memory":"64Mi","hugepages-2Mi":"2Mi"},"limits":{"hugepages-2Mi":"2Mi"}}},{"name":"d","image":"n:1","resources":{"limits":{"cpu":"300m"}}}]}}`)
	assert.Equal(t, map[string]any{"cpu": "1600m", "memory": "64Mi", "hugepages-2Mi": "4Mi"}, field(t, pod, "spec", "resources", "requests"),
		"pod-level CPU/memory requests default to effective container requests, hugepages to the pod limit")

	pv := defaulted(t, `{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv"},"spec":{"capacity":{"storage":"1Gi"},"hostPath":{"path":"/d"}}}`)
	assert.Equal(t, "Retain", field(t, pv, "spec", "persistentVolumeReclaimPolicy"))
	assert.Equal(t, "Filesystem", field(t, pv, "spec", "volumeMode"))
	assert.Equal(t, "", field(t, pv, "spec", "hostPath", "type"))

	crb := defaulted(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"b"},"roleRef":{"kind":"ClusterRole","name":"view"},
		"subjects":[{"kind":"User","name":"u"},{"kind":"ServiceAccount","name":"sa","namespace":"n"}]}`)
	assert.Equal(t, "rbac.authorization.k8s.io", field(t, crb, "roleRef", "apiGroup"))
	subjects := field(t, crb, "subjects").([]any)
	assert.Equal(t, "rbac.authorization.k8s.io", subjects[0].(map[string]any)["apiGroup"])
	assert.NotContains(t, subjects[1].(map[string]any), "apiGroup", "service account subjects stay in the core group")

	ic := defaulted(t, `{"apiVersion":"networking.k8s.io/v1","kind":"IngressClass","metadata":{"name":"i"},"spec":{"controller":"c","parameters":{"kind":"P","name":"p"}}}`)
	assert.Equal(t, "Cluster", field(t, ic, "spec", "parameters", "scope"))

	csi := defaulted(t, `{"apiVersion":"storage.k8s.io/v1","kind":"CSIDriver","metadata":{"name":"c"},"spec":{}}`)
	assert.Equal(t, map[string]any{"attachRequired": true, "fsGroupPolicy": "ReadWriteOnceWithFSType", "podInfoOnMount": false, "requiresRepublish": false,
		"seLinuxMount": false, "storageCapacity": false, "volumeLifecycleModes": []any{"Persistent"}}, csi["spec"])

	es := defaulted(t, `{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"name":"e"},"addressType":"IPv4","endpoints":[],"ports":[{"port":80}]}`)
	assert.Equal(t, []any{map[string]any{"name": "", "port": int64(80), "protocol": "TCP"}}, es["ports"])

	vap := defaulted(t, `{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicy","metadata":{"name":"v"},"spec":{"matchConstraints":{"resourceRules":[{"apiGroups":["apps"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["deployments"]}]}}}`)
	assert.Equal(t, "Fail", field(t, vap, "spec", "failurePolicy"))
	assert.Equal(t, "Equivalent", field(t, vap, "spec", "matchConstraints", "matchPolicy"))
	assert.Equal(t, map[string]any{}, field(t, vap, "spec", "matchConstraints", "objectSelector"))
	assert.Equal(t, "*", field(t, vap, "spec", "matchConstraints", "resourceRules").([]any)[0].(map[string]any)["scope"])
}

// TestTypedDefaultsDoNotLeakPatchDirectives: a defaults patch that introduces a whole
// structure (here the Deployment strategy) must not leave strategic-merge directives behind.
func TestTypedDefaultsDoNotLeakPatchDirectives(t *testing.T) {
	t.Parallel()
	u := obj(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"spec":{"containers":[{"name":"c","image":"a:1"}]}}}}`)
	data := mustJSON(t, u)
	out, err := withTypedDefaults(data, func() (runtime.Object, error) { return scheme.Scheme.New(u.GroupVersionKind()) })
	require.NoError(t, err)
	assert.NotContains(t, string(out), "$retainKeys")
	strategy, ok := nested(t, out, "spec", "strategy")
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"}}, strategy)
}

// TestDefaultingIdempotent: applying the defaults pipeline (typed re-encoding and defaults
// patch) to an already defaulted object changes nothing, for every corpus manifest.
func TestDefaultingIdempotent(t *testing.T) {
	t.Parallel()
	for _, manifest := range defaultsCorpus {
		u := obj(t, manifest)
		t.Run(u.GetKind()+"/"+u.GetName(), func(t *testing.T) {
			t.Parallel()
			once := locallyDefaulted(t, u)
			twice := locallyDefaulted(t, &unstructured.Unstructured{Object: once})
			assert.Equal(t, once, twice)
			assert.NotContains(t, string(mustJSON(t, &unstructured.Unstructured{Object: once})), `"$`, "no patch directives")
		})
	}
}
