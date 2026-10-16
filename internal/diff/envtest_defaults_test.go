package diff

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

// dryRunCreate creates u with dryRun=All and returns what the API server would persist
// (after decoding defaults, registry strategy and admission).
func (k *kubeDryRunner) dryRunCreate(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := k.resource(u)
	if err != nil {
		return nil, err
	}
	return ri.Create(ctx, u, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
}

// defaultsCorpus are minimal manifests exercising the defaulting functions re-implemented in
// defaults.go. Names are unique per entry. Namespaced objects are created in "default".
var defaultsCorpus = []string{
	`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d1"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"nginx:1.27","ports":[{"containerPort":80}],"resources":{"requests":{"cpu":"0.25","memory":"64Mi"},"limits":{"cpu":"1"}},"env":[{"name":"N","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}}],"livenessProbe":{"httpGet":{"port":80}},"readinessProbe":{"tcpSocket":{"port":80}},"startupProbe":{"grpc":{"port":81}},"lifecycle":{"preStop":{"httpGet":{"port":80}}}}],"initContainers":[{"name":"init","image":"busybox"}],"volumes":[{"name":"e"},{"name":"s","secret":{"secretName":"x"}},{"name":"cm","configMap":{"name":"y"}},{"name":"dapi","downwardAPI":{"items":[{"path":"n","fieldRef":{"fieldPath":"metadata.name"}}]}},{"name":"proj","projected":{"sources":[{"serviceAccountToken":{"path":"t"}},{"downwardAPI":{"items":[{"path":"l","fieldRef":{"fieldPath":"metadata.labels"}}]}}]}},{"name":"hp","hostPath":{"path":"/tmp"}},{"name":"eph","ephemeral":{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}}}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d2"},"spec":{"replicas":3,"strategy":{"type":"Recreate"},"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"hostNetwork":true,"containers":[{"name":"c","image":"registry.example:5000/app","ports":[{"containerPort":8080}]}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d3"},"spec":{"strategy":{"rollingUpdate":{"maxSurge":1}},"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"s1"},"spec":{"serviceName":"s","selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"postgres:17"}]}},"volumeClaimTemplates":[{"metadata":{"name":"data"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}]}}`,
	`{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"s2"},"spec":{"updateStrategy":{"type":"OnDelete"},"podManagementPolicy":"Parallel","selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"postgres:17"}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"s3"},"spec":{"updateStrategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"postgres:17"}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"ds1"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"agent:2"}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"ds2"},"spec":{"updateStrategy":{"type":"OnDelete"},"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"agent:2"}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"rs1"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app:1"}]}}}}`,
	`{"apiVersion":"v1","kind":"ReplicationController","metadata":{"name":"rc1"},"spec":{"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app:1"}]}}}}`,
	`{"apiVersion":"v1","kind":"PodTemplate","metadata":{"name":"pt1"},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app:1"}]}}}`,
	`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p1"},"spec":{"containers":[{"name":"c","image":"nginx","ports":[{"containerPort":8080}],"resources":{"limits":{"cpu":"0.0001","memory":"1Gi"}}}],"initContainers":[{"name":"i","image":"busybox:1","resources":{"limits":{"cpu":"100m"},"requests":{"memory":"1Mi"}}}]}}`,
	`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p2"},"spec":{"hostNetwork":true,"restartPolicy":"Never","containers":[{"name":"c","image":"nginx:1","ports":[{"containerPort":8080},{"containerPort":9090,"hostPort":9090}]}]}}`,
	`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"j1"},"spec":{"template":{"metadata":{"labels":{"a":"b"}},"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"job:1"}]}}}}`,
	`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"j2"},"spec":{"parallelism":3,"podFailurePolicy":{"rules":[{"action":"Ignore","onPodConditions":[{"type":"DisruptionTarget"}]}]},"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"job:1"}]}}}}`,
	`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"j3"},"spec":{"completions":4,"completionMode":"Indexed","backoffLimitPerIndex":1,"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"job:1"}]}}}}`,
	`{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"cj1"},"spec":{"schedule":"*/5 * * * *","jobTemplate":{"spec":{"template":{"spec":{"restartPolicy":"OnFailure","containers":[{"name":"c","image":"job:1"}]}}}}}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc1"},"spec":{"selector":{"a":"b"},"ports":[{"name":"http","port":80},{"name":"named","port":81,"targetPort":"web"}]}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc2"},"spec":{"type":"NodePort","selector":{"a":"b"},"ports":[{"port":80,"protocol":"UDP"}]}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc3"},"spec":{"type":"LoadBalancer","selector":{"a":"b"},"ports":[{"port":443,"targetPort":8443}]}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc4"},"spec":{"type":"ExternalName","externalName":"db.example.com"}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc5"},"spec":{"sessionAffinity":"ClientIP","selector":{"a":"b"},"ports":[{"port":80}]}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc6"},"spec":{"externalIPs":["192.0.2.10"],"selector":{"a":"b"},"ports":[{"port":80}]}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc7"},"spec":{"clusterIP":"None","selector":{"a":"b"},"ports":[{"port":80}]}}`,
	`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"sec1"},"stringData":{"a":"b"}}`,
	`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1"},"data":{"a":"b"}}`,
	`{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"pvc1"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1.5Gi"}}}}`,
	`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-defaults"}}`,
	`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"sa1"}}`,
	`{"apiVersion":"v1","kind":"LimitRange","metadata":{"name":"lr1"},"spec":{"limits":[{"type":"Container","max":{"cpu":"2"},"min":{"cpu":"0.1"}}]}}`,
	`{"apiVersion":"v1","kind":"ResourceQuota","metadata":{"name":"rq1"},"spec":{"hard":{"cpu":"1.5","pods":"10"}}}`,
	`{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"np1"},"spec":{"podSelector":{},"ingress":[{"ports":[{"port":80}]}]}}`,
	`{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"np2"},"spec":{"podSelector":{},"egress":[{"ports":[{"port":53,"protocol":"UDP"}]}]}}`,
	`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"ing1"},"spec":{"rules":[{"host":"a.example","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"s","port":{"number":80}}}}]}}]}}`,
	`{"apiVersion":"autoscaling/v1","kind":"HorizontalPodAutoscaler","metadata":{"name":"hpa1"},"spec":{"maxReplicas":3,"scaleTargetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"d"}}}`,
	`{"apiVersion":"autoscaling/v2","kind":"HorizontalPodAutoscaler","metadata":{"name":"hpa2"},"spec":{"maxReplicas":3,"scaleTargetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"d"}}}`,
	`{"apiVersion":"autoscaling/v2","kind":"HorizontalPodAutoscaler","metadata":{"name":"hpa3"},"spec":{"maxReplicas":3,"scaleTargetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"d"},"behavior":{"scaleDown":{"stabilizationWindowSeconds":60}}}}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"MutatingWebhookConfiguration","metadata":{"name":"mwc-defaults"},"webhooks":[{"name":"m.example.com","admissionReviewVersions":["v1"],"sideEffects":"None","clientConfig":{"service":{"name":"s","namespace":"default"}},"rules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["widgets"]}]}]}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingWebhookConfiguration","metadata":{"name":"vwc-defaults"},"webhooks":[{"name":"v.example.com","admissionReviewVersions":["v1"],"sideEffects":"None","clientConfig":{"url":"https://hook.example.com/validate"},"rules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["*"],"resources":["widgets"]}]}]}`,
	`{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass","metadata":{"name":"sc-defaults"},"provisioner":"example.com/disk"}`,
	`{"apiVersion":"scheduling.k8s.io/v1","kind":"PriorityClass","metadata":{"name":"pc-defaults"},"value":1000}`,
	`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"cr-defaults"},"rules":[{"apiGroups":[""],"resources":["pods"],"verbs":["get"]}]}`,
	`{"apiVersion":"policy/v1","kind":"PodDisruptionBudget","metadata":{"name":"pdb1"},"spec":{"minAvailable":1,"selector":{"matchLabels":{"a":"b"}}}}`,
	`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p3"},"spec":{"resources":{"limits":{"cpu":"1","memory":"128Mi"}},"containers":[{"name":"c","image":"nginx:1"}]}}`,
	`{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-defaults"},"spec":{"capacity":{"storage":"0.5Gi"},"accessModes":["ReadWriteOnce"],"hostPath":{"path":"/data"}}}`,
	`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"crb-defaults"},"roleRef":{"kind":"ClusterRole","name":"view"},"subjects":[{"kind":"User","name":"alice"},{"kind":"Group","name":"devs"},{"kind":"ServiceAccount","name":"sa","namespace":"default"}]}`,
	`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"rb1"},"roleRef":{"kind":"ClusterRole","name":"view"},"subjects":[{"kind":"User","name":"bob"}]}`,
	`{"apiVersion":"networking.k8s.io/v1","kind":"IngressClass","metadata":{"name":"ic-defaults"},"spec":{"controller":"example.com/ingress","parameters":{"apiGroup":"example.com","kind":"Params","name":"p"}}}`,
	`{"apiVersion":"storage.k8s.io/v1","kind":"CSIDriver","metadata":{"name":"csi.example.com"},"spec":{}}`,
	`{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"name":"es1"},"addressType":"IPv4","endpoints":[{"addresses":["10.0.0.1"]}],"ports":[{"port":80}]}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicy","metadata":{"name":"vap-defaults"},"spec":{"matchConstraints":{"resourceRules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["widgets"]}]},"validations":[{"expression":"object.spec.replicas <= 5"}]}}`,
	`{"apiVersion":"v1","kind":"Endpoints","metadata":{"name":"ep1"},"subsets":[{"addresses":[{"ip":"10.0.0.1"}],"ports":[{"port":80}]}]}`,
	`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p4"},"spec":{"resources":{"limits":{"cpu":"2","memory":"256Mi","hugepages-2Mi":"4Mi"}},"initContainers":[{"name":"side","image":"s:1","restartPolicy":"Always","resources":{"requests":{"cpu":"100m"}}},{"name":"init","image":"i:1","resources":{"requests":{"cpu":"1500m","memory":"10Mi"}}}],"containers":[{"name":"c","image":"nginx:1","resources":{"requests":{"cpu":"250m","memory":"64Mi","hugepages-2Mi":"2Mi"},"limits":{"hugepages-2Mi":"2Mi"}}},{"name":"d","image":"nginx:1","resources":{"limits":{"cpu":"300m"}}}]}}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicyBinding","metadata":{"name":"vapb-defaults"},"spec":{"policyName":"vap-defaults","validationActions":["Deny"],"matchResources":{"resourceRules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["widgets"]}]}}}`,
	`{"apiVersion":"v1","kind":"Service","metadata":{"name":"svc8"},"spec":{"type":"LoadBalancer","externalTrafficPolicy":"Local","selector":{"a":"b"},"ports":[{"port":80}]}}`,
	`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d4"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app:1","readinessProbe":{"exec":{"command":["true"]},"periodSeconds":5}}],"volumes":[{"name":"pr","projected":{"sources":[{"configMap":{"name":"x"}}]}},{"name":"iscsi","iscsi":{"targetPortal":"10.0.0.1:3260","iqn":"iqn.2001-04.com.example:storage","lun":0}},{"name":"rbd","rbd":{"monitors":["10.0.0.2:6789"],"image":"img"}},{"name":"az","azureDisk":{"diskName":"d","diskURI":"https://example/d"}}]}}}}`,
	`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d5"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"app:1"}],"volumes":[{"name":"sio","scaleIO":{"gateway":"https://gw","system":"s","volumeName":"v","secretRef":{"name":"x"}}},{"name":"sos","storageos":{"volumeName":"v"}},{"name":"fc","fc":{"wwids":["w"]}},{"name":"nfs","nfs":{"server":"s","path":"/p"}}]}}}}`,
	`{"apiVersion":"storage.k8s.io/v1","kind":"CSIDriver","metadata":{"name":"csi2.example.com"},"spec":{"tokenRequests":[{"audience":"a"}]}}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"MutatingAdmissionPolicy","metadata":{"name":"map-defaults"},"spec":{"matchConstraints":{"resourceRules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["widgets"]}]},"reinvocationPolicy":"Never","mutations":[{"patchType":"ApplyConfiguration","applyConfiguration":{"expression":"Object{metadata: Object.metadata{labels: {\"a\": \"b\"}}}"}}]}}`,
	`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"MutatingAdmissionPolicyBinding","metadata":{"name":"mapb-defaults"},"spec":{"policyName":"map-defaults","matchResources":{"resourceRules":[{"apiGroups":["example.com"],"apiVersions":["v1"],"operations":["CREATE"],"resources":["widgets"]}]}}}`,
}

// serverOnlyFields lists fields the API server adds on create that are not produced by the
// defaulting functions (registry strategies, allocators, admission plugins, conversion
// round trips) and that the baseline does not reproduce locally either. Patterns match the
// "<kind>:<flattened path>" form.
var serverOnlyFields = []*regexp.Regexp{
	// Allocations by the Service registry.
	regexp.MustCompile(`^Service:spec\.(clusterIP|clusterIPs\.\d+|ipFamilies\.\d+|ipFamilyPolicy|healthCheckNodePort)$`),
	regexp.MustCompile(`^Service:spec\.ports\.\d+\.nodePort$`),
	// The Job registry generates the selector and controller labels.
	regexp.MustCompile(`^Job:spec\.selector\.matchLabels\.`),
	regexp.MustCompile(`^Job:spec\.template\.metadata\.labels\.(batch\.kubernetes\.io/)?(controller-uid|job-name)$`),
	// Namespace registry.
	regexp.MustCompile(`^Namespace:spec\.finalizers\.\d+$`),
	// Priority admission plugin.
	regexp.MustCompile(`^Pod:spec\.(priority|preemptionPolicy)$`),
	// DefaultTolerationSeconds admission plugin.
	regexp.MustCompile(`^Pod:spec\.tolerations\.`),
	// The StatefulSet registry adds apiVersion/kind to claim templates (the legacy diff trims
	// live templates to the manifest's shape instead, see trimVolumeClaimTemplates).
	regexp.MustCompile(`^StatefulSet:spec\.volumeClaimTemplates\.\d+\.(apiVersion|kind)$`),
	// DaemonSet registry bookkeeping.
	regexp.MustCompile(`^DaemonSet:metadata\.annotations\.deprecated\.daemonset\.template\.generation$`),
	// The Job registry labels the Job itself and records that the selector was generated.
	regexp.MustCompile(`^Job:metadata\.labels\.(batch\.kubernetes\.io/)?(controller-uid|job-name)$`),
	regexp.MustCompile(`^Job:spec\.manualSelector$`),
	// The Secret registry folds stringData into data (the diff does this in NormalizeSecret).
	regexp.MustCompile(`^Secret:data\.`),
	// autoscaling/v1 defaults the CPU target during conversion, not in its defaulting
	// function; the baseline's local defaulting does not reproduce it either.
	regexp.MustCompile(`^HorizontalPodAutoscaler:spec\.targetCPUUtilizationPercentage$`),
}

// serverEncodingArtifacts are zero values the server serializes for non-omitempty fields of
// its Go types. They are not defaults: a manifest without them matches the live object
// because the merge keeps live fields the manifest does not mention.
var serverEncodingArtifacts = []*regexp.Regexp{
	regexp.MustCompile(`(^|\.)(containers|initContainers)\.\d+\.resources$`),
	regexp.MustCompile(`(^|\.)(metadata)$`),
	regexp.MustCompile(`^EndpointSlice:endpoints\.\d+\.conditions$`),
	regexp.MustCompile(`^StatefulSet:spec\.serviceName$`),
}

func isZeroJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// flatten turns a JSON value into leaf path -> value. Empty maps and lists are leaves.
func flatten(prefix string, v any, out map[string]any) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[prefix] = t
		}
		for k, child := range t {
			flatten(join(k), child, out)
		}
	case []any:
		if len(t) == 0 {
			out[prefix] = t
		}
		for i, child := range t {
			flatten(join(strconv.Itoa(i)), child, out)
		}
	default:
		out[prefix] = v
	}
}

// comparable reports whether a path takes part in the conformance comparison: status and
// server-owned metadata are excluded, labels and annotations are compared.
func comparablePath(p string) bool {
	if strings.HasPrefix(p, "status") {
		return false
	}
	if strings.HasPrefix(p, "metadata") {
		return strings.HasPrefix(p, "metadata.labels") || strings.HasPrefix(p, "metadata.annotations")
	}
	return true
}

// locallyDefaulted runs the steps the legacy diff applies to a manifest: the typed re-encoding
// (remarshal) and the defaults patch (withTypedDefaults).
func locallyDefaulted(t *testing.T, u *unstructured.Unstructured) map[string]any {
	t.Helper()
	gvk := u.GroupVersionKind()
	_, err := scheme.Scheme.New(gvk)
	require.NoError(t, err)
	re, err := remarshal(u, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	data, err := json.Marshal(re.Object)
	require.NoError(t, err)
	out, err := withTypedDefaults(data, func() (runtime.Object, error) { return scheme.Scheme.New(gvk) })
	require.NoError(t, err)
	m, err := decodeObject(out)
	require.NoError(t, err)
	return m
}

// TestEnvtestDefaultsConformance checks the re-implemented API defaults (defaults.go) against
// a real kube-apiserver of the baseline's Kubernetes minor version: every field the local
// defaulting sets must be set by the server to the same value, and every field the server
// adds must be either produced locally or a known registry/admission field.
func TestEnvtestDefaultsConformance(t *testing.T) {
	k, _ := envtestClients(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, manifest := range defaultsCorpus {
		input := obj(t, manifest)
		t.Run(input.GetKind()+"/"+input.GetName(), func(t *testing.T) {
			m, err := k.mapper.RESTMapping(input.GroupVersionKind().GroupKind(), input.GroupVersionKind().Version)
			require.NoError(t, err)
			if m.Scope.Name() == "namespace" {
				input.SetNamespace("default")
			}
			server, err := k.dryRunCreate(ctx, input)
			require.NoError(t, err)
			local := locallyDefaulted(t, input)

			in, srv, loc := map[string]any{}, map[string]any{}, map[string]any{}
			flatten("", input.Object, in)
			flatten("", server.Object, srv)
			flatten("", local, loc)

			kind := input.GetKind()
			var wrong, missing []string
			for p, lv := range loc {
				if !comparablePath(p) {
					continue
				}
				if iv, ok := in[p]; ok && reflect.DeepEqual(iv, lv) {
					continue
				}
				if sv, ok := srv[p]; !ok || !reflect.DeepEqual(sv, lv) {
					wrong = append(wrong, fmt.Sprintf("%s: local %v, server %v (present=%t)", p, lv, sv, ok))
				}
			}
			for p, sv := range srv {
				if !comparablePath(p) {
					continue
				}
				if _, ok := in[p]; ok {
					continue
				}
				if _, ok := loc[p]; ok {
					continue
				}
				if slices.ContainsFunc(serverOnlyFields, func(re *regexp.Regexp) bool { return re.MatchString(kind + ":" + p) }) {
					continue
				}
				if isZeroJSON(sv) && slices.ContainsFunc(serverEncodingArtifacts, func(re *regexp.Regexp) bool { return re.MatchString(kind + ":" + p) }) {
					continue
				}
				missing = append(missing, fmt.Sprintf("%s = %v", p, sv))
			}
			// Values the server rewrote (canonical quantities, rounding) must be rewritten
			// identically by the local pipeline.
			var rewritten []string
			for p, iv := range in {
				if !comparablePath(p) || kind == "Secret" {
					continue
				}
				sv, ok := srv[p]
				if !ok || reflect.DeepEqual(sv, iv) {
					continue
				}
				if lv := loc[p]; !reflect.DeepEqual(lv, sv) {
					rewritten = append(rewritten, fmt.Sprintf("%s: input %v, server %v, local %v", p, iv, sv, lv))
				}
			}
			slices.Sort(wrong)
			slices.Sort(missing)
			slices.Sort(rewritten)
			assert.Empty(t, wrong, "fields defaulted locally with a value the server does not produce")
			assert.Empty(t, missing, "fields defaulted by the server but not locally")
			assert.Empty(t, rewritten, "input values the server rewrote differently")
		})
	}
}

// TestEnvtestCorpusInSync stores every corpus manifest in a real API server, once with
// client-side apply semantics (last-applied-configuration) and once with server-side apply,
// and checks that no strategy reports drift for the unchanged manifest while a label change
// is detected.
func TestEnvtestCorpusInSync(t *testing.T) {
	k, p := envtestClients(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for i, manifest := range defaultsCorpus {
		base := obj(t, manifest)
		t.Run(base.GetKind()+"/"+base.GetName(), func(t *testing.T) {
			m, err := k.mapper.RESTMapping(base.GroupVersionKind().GroupKind(), base.GroupVersionKind().Version)
			require.NoError(t, err)
			if m.Scope.Name() == "namespace" {
				// One namespace per object: LimitRanges and ResourceQuotas in the corpus
				// must not constrain the Pods of other entries.
				ns := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "v1", "kind": "Namespace",
					"metadata": map[string]any{"name": fmt.Sprintf("insync-%d", i)},
				}}
				_, err := k.create(ctx, ns)
				require.NoError(t, err)
				base.SetNamespace(ns.GetName())
			}
			// Managed resources always carry tracking metadata, so the field manager owns at
			// least one field (a dry run of a manifest without any owned field returns no
			// managedFields entry for the manager, which server-side diff rejects like the
			// baseline does).
			labels := base.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["app.kubernetes.io/part-of"] = "diff-envtest"
			base.SetLabels(labels)
			// Baseline quirk: EndpointSlice endpoints is an atomic list and the server
			// serializes the non-omitempty conditions struct, so the legacy strategy reports
			// drift for endpoints declared without conditions.
			legacyDrift := base.GetKind() == "EndpointSlice"

			// Client-side apply: the manifest is stored with its last-applied annotation.
			csaConfig := base.DeepCopy()
			csaConfig.SetName(base.GetName() + "-csa")
			created, err := k.create(ctx, withLastApplied(t, csaConfig, csaConfig))
			require.NoError(t, err)
			three := mustDiff(t, csaConfig, created, Options{})
			assert.Equal(t, legacyDrift, three.Modified, "three-way\nlive: %s\npredicted: %s", three.NormalizedLive, three.PredictedLive)
			two := created.DeepCopy()
			unstructured.RemoveNestedField(two.Object, "metadata", "annotations", AnnotationLastAppliedConfig)
			res := mustDiff(t, csaConfig, two, Options{})
			assert.Equal(t, legacyDrift, res.Modified, "two-way\nlive: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)

			changed := csaConfig.DeepCopy()
			labels = changed.GetLabels()
			labels["changed"] = "true"
			changed.SetLabels(labels)
			assert.True(t, mustDiff(t, changed, created, Options{}).Modified, "label change (three-way)")

			// Server-side apply with the Patrol field manager.
			ssaConfig := base.DeepCopy()
			ssaConfig.SetName(base.GetName() + "-ssa")
			applied, err := k.apply(ctx, ssaConfig, DefaultManager, false)
			require.NoError(t, err)
			o := DefaultOptions()
			o.ServerSide, o.GVKParser, o.DryRunner = true, p, k
			ssd := mustDiff(t, ssaConfig, applied, o)
			assert.False(t, ssd.Modified, "server-side\nlive: %s\npredicted: %s", ssd.NormalizedLive, ssd.PredictedLive)
			legacy := mustDiff(t, ssaConfig, applied, Options{})
			assert.Equal(t, legacyDrift, legacy.Modified, "legacy on SSA object\nlive: %s\npredicted: %s", legacy.NormalizedLive, legacy.PredictedLive)

			changed = ssaConfig.DeepCopy()
			labels = changed.GetLabels()
			labels["changed"] = "true"
			changed.SetLabels(labels)
			assert.True(t, mustDiff(t, changed, applied, o).Modified, "label change (server-side)")
		})
	}
}
