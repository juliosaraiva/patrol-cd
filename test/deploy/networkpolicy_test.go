package deploy_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// networkPolicy converts a rendered NetworkPolicy.
func networkPolicy(t *testing.T, objs []object, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	o := find(objs, "NetworkPolicy", name)
	if o == nil {
		t.Fatalf("NetworkPolicy %s not rendered", name)
	}
	var np networkingv1.NetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.u.Object, &np); err != nil {
		t.Fatal(err)
	}
	return &np
}

// egressRulesToPort returns the egress rules of np that allow TCP port.
func egressRulesToPort(np *networkingv1.NetworkPolicy, port int32) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	for _, r := range np.Spec.Egress {
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntVal == port && (p.Protocol == nil || *p.Protocol == "TCP") {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// TestTracingEgress: with an OTLP collector configured (tracing.otlpAddress, or otlp.address
// in configs.params), the NetworkPolicies of the components that export traces (server,
// controller, repo-server) allow egress to the collector port, to tracing.networkPolicy.to
// when set; the other components get no such rule.
func TestTracingEgress(t *testing.T) {
	t.Parallel()
	defaults := filepath.Join(chartDir(), "ci", "default.yaml")
	exporters := []string{"patrol-server", "patrol-controller", "patrol-repo-server"}
	others := []string{"patrol-applicationset-controller", "patrol-notifications-controller"}
	peer := `{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"observability"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"otel-collector"}}}`
	for _, tc := range []struct {
		name   string
		args   []string
		port   int32
		wantTo bool // the rule's peers are tracing.networkPolicy.to
	}{
		{name: "none", port: 4317},
		{name: "tracing.otlpAddress", args: []string{"--set", "tracing.otlpAddress=otel-collector.observability.svc:4317"}, port: 4317},
		{name: "configs.params otlp.address", args: []string{"--set-string", `configs.params.otlp\.address=collector.example.test:55680`}, port: 55680},
		{name: "address without port", args: []string{"--set", "tracing.otlpAddress=otel-collector.observability.svc"}, port: 4317},
		{name: "explicit port and peers", args: []string{"--set", "tracing.otlpAddress=otel-collector.observability.svc:4317",
			"--set", "tracing.networkPolicy.port=4318", "--set-json", "tracing.networkPolicy.to=[" + peer + "]"}, port: 4318, wantTo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs, err := parseObjects(render(t, releaseName, releaseNS, []string{defaults}, tc.args...))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range exporters {
				rules := egressRulesToPort(networkPolicy(t, objs, name), tc.port)
				switch {
				case tc.name == "none":
					if len(rules) != 0 {
						t.Errorf("%s: egress to %d without a collector: %+v", name, tc.port, rules)
					}
				case len(rules) != 1:
					t.Errorf("%s: %d egress rules allow TCP %d, want 1 (traces would be dropped)", name, len(rules), tc.port)
				case tc.wantTo:
					if got := fmt.Sprintf("%+v", rules[0].To); len(rules[0].To) != 1 || rules[0].To[0].NamespaceSelector == nil || rules[0].To[0].PodSelector == nil ||
						!reflect.DeepEqual(rules[0].To[0].NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": "observability"}) {
						t.Errorf("%s: collector rule peers = %s, want tracing.networkPolicy.to", name, got)
					}
				default:
					if len(rules[0].To) != 0 {
						t.Errorf("%s: collector rule restricted to %+v without tracing.networkPolicy.to", name, rules[0].To)
					}
				}
			}
			for _, name := range others {
				if rules := egressRulesToPort(networkPolicy(t, objs, name), tc.port); len(rules) != 0 && tc.port != 443 {
					t.Errorf("%s exports no traces but allows egress to %d: %+v", name, tc.port, rules)
				}
			}
		})
	}
}
