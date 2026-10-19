package main

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// These tests render the chart with specific values (helm binary, skipped when it is not
// installed) and check the rendered objects: names that must be DNS labels, NetworkPolicy
// except entries, the ServiceAccount fallback, PDBs, HPAs, self-service notification RBAC, the
// Ingress backend port and the bootstrap Role.

// chartDir is the chart, from this package's directory.
func chartDir() string { return filepath.Join("..", "..", "deploy", "helm", "patrol-cd") }

// renderValues runs helm template with a values document and extra arguments. It returns the
// output and helm's error output; err is set when helm fails.
func renderValues(t *testing.T, values string, args ...string) ([]byte, string, error) {
	t.Helper()
	helm := helmBinary(t)
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helm, append([]string{"template", "patrol", chartDir(), "-n", "patrol", "--kube-version", kubeVersion, "-f", f}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.String(), err
}

// mustRender renders and fails the test when helm fails.
func mustRender(t *testing.T, values string, args ...string) []byte {
	t.Helper()
	out, stderr, err := renderValues(t, values, args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr)
	}
	return out
}

// rendered is one rendered object: its kind, name and YAML document.
type rendered struct {
	kind, name string
	doc        []byte
}

func objects(t *testing.T, out []byte) []rendered {
	t.Helper()
	var objs []rendered
	for _, doc := range splitDocuments(out) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("invalid YAML: %v\n%s", err, doc)
		}
		if meta.Kind != "" {
			objs = append(objs, rendered{kind: meta.Kind, name: meta.Metadata.Name, doc: doc})
		}
	}
	return objs
}

// decodeObject decodes the object of a kind and name into into; it fails the test when it is absent.
func decodeObject(t *testing.T, objs []rendered, kind, name string, into any) {
	t.Helper()
	for _, o := range objs {
		if o.kind == kind && o.name == name {
			if err := yaml.UnmarshalStrict(o.doc, into); err != nil {
				t.Fatalf("%s/%s: %v", kind, name, err)
			}
			return
		}
	}
	t.Fatalf("%s/%s not rendered", kind, name)
}

// strictSubset reports whether inner is a strict subset of outer (the API server's rule for
// ipBlock.except: same family, longer prefix, inside the cidr).
func strictSubset(outer, inner string) (bool, error) {
	o, err := netip.ParsePrefix(outer)
	if err != nil {
		return false, err
	}
	i, err := netip.ParsePrefix(inner)
	if err != nil {
		return false, err
	}
	return o.Addr().Is4() == i.Addr().Is4() && i.Bits() > o.Bits() && o.Masked().Contains(i.Masked().Addr()), nil
}

// checkNetworkPolicy returns ipBlocks whose except entries the API server would reject.
func checkNetworkPolicy(np *networkingv1.NetworkPolicy) []string {
	var p []string
	for i, rule := range np.Spec.Egress {
		for j, peer := range rule.To {
			if peer.IPBlock == nil {
				continue
			}
			for _, e := range peer.IPBlock.Except {
				ok, err := strictSubset(peer.IPBlock.CIDR, e)
				if err != nil || !ok {
					p = append(p, fmt.Sprintf("egress[%d].to[%d]: except %s is not a strict subset of %s (%v)", i, j, e, peer.IPBlock.CIDR, err))
				}
			}
		}
	}
	return p
}

func TestStrictSubset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		outer, inner string
		want         bool
	}{
		{"0.0.0.0/0", "169.254.169.254/32", true},
		{"10.20.0.0/16", "10.0.0.0/8", false},
		{"10.0.0.0/8", "10.0.0.0/8", false},
		{"::/0", "fd00:ec2::254/128", true},
		{"0.0.0.0/0", "fd00:ec2::254/128", false},
		{"2001:db8::/32", "2001:db9::/48", false},
	} {
		if got, err := strictSubset(tc.outer, tc.inner); err != nil || got != tc.want {
			t.Errorf("strictSubset(%s, %s) = %v, %v; want %v", tc.outer, tc.inner, got, err, tc.want)
		}
	}
}

// TestServiceNamesAreDNSLabels: the longest prefixes the schema accepts (40 characters) keep
// every Service name within 63 characters, and the metrics Services keep their suffix.
func TestServiceNamesAreDNSLabels(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"patrol-cd-production-eu-west-12", "abcdefghij-abcdefghij-abcdefghij-abcdefg"} {
		t.Run(fmt.Sprint(len(prefix)), func(t *testing.T) {
			t.Parallel()
			out := mustRender(t, "fullnameOverride: "+prefix+"\nhydrator:\n  enabled: true\n")
			metrics := 0
			for _, o := range objects(t, out) {
				if o.kind != "Service" {
					continue
				}
				if len(o.name) > 63 {
					t.Errorf("Service %s: %d characters, more than 63", o.name, len(o.name))
				}
				if strings.HasSuffix(o.name, "-metrics") {
					metrics++
				}
			}
			if metrics != 3 {
				t.Errorf("%d Services end in -metrics, want 3 (server, controller, notifications controller)", metrics)
			}
		})
	}
}

// TestNetworkPolicyExcept: except entries are attached only to the CIDRs that strictly contain
// them, and the cloud metadata endpoints are removed from the repo-server and commit-server
// egress by default.
func TestNetworkPolicyExcept(t *testing.T) {
	t.Parallel()
	type block struct {
		CIDR   string
		Except []string
	}
	for _, tc := range []struct {
		name, values, policy string
		want                 []block
	}{
		{
			name: "default-repo-server", policy: "patrol-repo-server",
			want: []block{{"0.0.0.0/0", []string{"169.254.169.254/32"}}, {"::/0", []string{"fd00:ec2::254/128"}}},
		},
		{
			name: "default-commit-server", policy: "patrol-commit-server", values: "hydrator:\n  enabled: true\n",
			want: []block{{"0.0.0.0/0", []string{"169.254.169.254/32"}}, {"::/0", []string{"fd00:ec2::254/128"}}},
		},
		{
			name: "default-controller-unchanged", policy: "patrol-controller",
			want: []block{{"0.0.0.0/0", nil}, {"::/0", nil}},
		},
		{
			name: "metadata-opt-out", policy: "patrol-repo-server", values: "networkPolicy:\n  excludeMetadataEndpoints: []\n",
			want: []block{{"0.0.0.0/0", nil}, {"::/0", nil}},
		},
		{
			name: "add-back", policy: "patrol-repo-server",
			values: "networkPolicy:\n  repoServerEgress:\n    cidrs: [0.0.0.0/0, 10.20.0.0/16]\n    except: [10.0.0.0/8, 169.254.0.0/16]\n    ports: [22, 443]\n",
			want:   []block{{"0.0.0.0/0", []string{"10.0.0.0/8", "169.254.0.0/16", "169.254.169.254/32"}}, {"10.20.0.0/16", nil}},
		},
		{
			name: "narrowed", policy: "patrol-repo-server",
			values: "networkPolicy:\n  repoServerEgress:\n    cidrs: [10.20.0.0/16]\n    except: [169.254.0.0/16, 10.20.5.0/24]\n    ports: [443]\n",
			want:   []block{{"10.20.0.0/16", []string{"10.20.5.0/24"}}},
		},
		{
			name: "ipv6-partial-prefix", policy: "patrol-controller",
			values: "networkPolicy:\n  controllerEgress:\n    cidrs: [\"2001:db8::/31\", \"2001:db8:8000::/33\", \"::/0\"]\n    except: [\"2001:db9:1::/48\", \"2001:db8:ffff::/48\", \"fd00::/8\", \"2001:db8::/31\"]\n    ports: [443]\n",
			want: []block{
				{"2001:db8::/31", []string{"2001:db9:1::/48", "2001:db8:ffff::/48"}},
				{"2001:db8:8000::/33", []string{"2001:db8:ffff::/48"}},
				{"::/0", []string{"2001:db9:1::/48", "2001:db8:ffff::/48", "fd00::/8", "2001:db8::/31"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var np networkingv1.NetworkPolicy
			decodeObject(t, objects(t, mustRender(t, tc.values)), "NetworkPolicy", tc.policy, &np)
			for _, p := range checkNetworkPolicy(&np) {
				t.Error(p)
			}
			// The external egress rule (<component>Egress) is the last rule with ipBlocks; the
			// API server rule (kubeAPIServer.cidrs) precedes it.
			var got []block
			for _, rule := range np.Spec.Egress {
				var blocks []block
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						blocks = append(blocks, block{peer.IPBlock.CIDR, peer.IPBlock.Except})
					}
				}
				if blocks != nil {
					got = blocks
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("ipBlocks = %v\nwant       %v", got, tc.want)
			}
		})
	}
}

// componentSA lists the components with a ServiceAccount, the values that deploy them and the
// name of the workload.
var componentSA = []struct {
	key, values, kind, workload string
}{
	{"server", "", "Deployment", "patrol-server"},
	{"controller", "", "StatefulSet", "patrol-controller"},
	{"repoServer", "", "Deployment", "patrol-repo-server"},
	{"appsetController", "", "Deployment", "patrol-applicationset-controller"},
	{"notificationsController", "", "Deployment", "patrol-notifications-controller"},
	{"commitServer", "hydrator:\n  enabled: true\n", "Deployment", "patrol-commit-server"},
	{"dex", "dex:\n  enabled: true\n  clientSecretRef:\n    name: dex-client\nconfigs:\n  cm:\n    url: https://patrol.example.com\n", "Deployment", "patrol-dex-server"},
}

// TestServiceAccountNameRequired: with serviceAccount.create false every component needs a name;
// the chart never binds RBAC to, or runs pods as, the namespace's "default" ServiceAccount.
func TestServiceAccountNameRequired(t *testing.T) {
	t.Parallel()
	for _, c := range componentSA {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			off := c.values + c.key + ":\n  serviceAccount:\n    create: false\n"
			if c.key == "dex" {
				off = strings.Replace(c.values, "dex:\n  enabled: true\n", "dex:\n  enabled: true\n  serviceAccount:\n    create: false\n", 1)
			}
			_, stderr, err := renderValues(t, off)
			want := c.key + ".serviceAccount.name is required when " + c.key + ".serviceAccount.create is false"
			if err == nil || !strings.Contains(stderr, want) {
				t.Fatalf("create=false without a name: err=%v, want an error containing %q:\n%s", err, want, stderr)
			}
			named := c.values + c.key + ":\n  serviceAccount:\n    create: false\n    name: existing-sa\n"
			if c.key == "dex" {
				named = strings.Replace(c.values, "dex:\n  enabled: true\n", "dex:\n  enabled: true\n  serviceAccount:\n    create: false\n    name: existing-sa\n", 1)
			}
			objs := objects(t, mustRender(t, named))
			var spec corev1.PodSpec
			switch c.kind {
			case "StatefulSet":
				var s appsv1.StatefulSet
				decodeObject(t, objs, c.kind, c.workload, &s)
				spec = s.Spec.Template.Spec
			default:
				var d appsv1.Deployment
				decodeObject(t, objs, c.kind, c.workload, &d)
				spec = d.Spec.Template.Spec
			}
			if spec.ServiceAccountName != "existing-sa" {
				t.Errorf("%s runs as %q, want existing-sa", c.workload, spec.ServiceAccountName)
			}
			for _, o := range objs {
				if o.kind != "RoleBinding" && o.kind != "ClusterRoleBinding" {
					continue
				}
				var b rbacv1.RoleBinding // the subjects of both kinds decode the same way
				if err := yaml.Unmarshal(o.doc, &b); err != nil {
					t.Fatal(err)
				}
				for _, s := range b.Subjects {
					if s.Name == "default" {
						t.Errorf("%s/%s binds ServiceAccount default", o.kind, o.name)
					}
				}
			}
		})
	}
}

// TestPDBMaxUnavailable: an explicit maxUnavailable 0 is rendered (it was replaced by 1).
func TestPDBMaxUnavailable(t *testing.T) {
	t.Parallel()
	objs := objects(t, mustRender(t, "server:\n  pdb:\n    maxUnavailable: 0\ncontroller:\n  pdb:\n    minAvailable: 1\n"))
	for name, want := range map[string]string{"patrol-server": "maxUnavailable=0", "patrol-controller": "minAvailable=1", "patrol-repo-server": "maxUnavailable=1"} {
		var pdb policyv1.PodDisruptionBudget
		decodeObject(t, objs, "PodDisruptionBudget", name, &pdb)
		got := ""
		switch {
		case pdb.Spec.MinAvailable != nil && pdb.Spec.MaxUnavailable == nil:
			got = "minAvailable=" + pdb.Spec.MinAvailable.String()
		case pdb.Spec.MaxUnavailable != nil && pdb.Spec.MinAvailable == nil:
			got = "maxUnavailable=" + pdb.Spec.MaxUnavailable.String()
		}
		if got != want {
			t.Errorf("PodDisruptionBudget %s: %s, want %s", name, got, want)
		}
	}
}

// TestHPAMetrics: a single non-zero target renders exactly that metric (the invalid
// combinations are in ci/invalid).
func TestHPAMetrics(t *testing.T) {
	t.Parallel()
	objs := objects(t, mustRender(t, "server:\n  autoscaling:\n    enabled: true\n    targetCPUUtilizationPercentage: 0\n    targetMemoryUtilizationPercentage: 75\n"))
	var hpa autoscalingv2.HorizontalPodAutoscaler
	decodeObject(t, objs, "HorizontalPodAutoscaler", "patrol-server", &hpa)
	if len(hpa.Spec.Metrics) != 1 || hpa.Spec.Metrics[0].Resource == nil || hpa.Spec.Metrics[0].Resource.Name != corev1.ResourceMemory {
		t.Errorf("metrics = %+v, want one memory metric", hpa.Spec.Metrics)
	}
}

// TestNotificationsSelfServiceRBAC: the Application-namespace Role of the notifications
// controller gets the self-service rules whenever the binary would enable self-service.
func TestNotificationsSelfServiceRBAC(t *testing.T) {
	t.Parallel()
	const ns = "applicationNamespaces: [team-a]\n"
	for _, tc := range []struct {
		name, values string
		want         bool
	}{
		{"off-by-default", ns, false},
		{"param-string", ns + "configs:\n  params:\n    notificationscontroller.selfservice.enabled: \"true\"\n", true},
		{"param-mixed-case", ns + "configs:\n  params:\n    notificationscontroller.selfservice.enabled: \"True\"\n", true},
		{"param-bool", ns + "configs:\n  params:\n    notificationscontroller.selfservice.enabled: true\n", true},
		{"param-false", ns + "configs:\n  params:\n    notificationscontroller.selfservice.enabled: \"FALSE\"\n", false},
		{"flag", ns + "notificationsController:\n  extraArgs: [--self-service-notification-enabled]\n", true},
		{"flag-false-wins", ns + "configs:\n  params:\n    notificationscontroller.selfservice.enabled: true\nnotificationsController:\n  extraArgs: [--self-service-notification-enabled=false]\n", false},
		{"env", ns + "notificationsController:\n  env:\n    - name: PATROL_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED\n      value: \"TRUE\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var role rbacv1.Role
			decodeObject(t, objects(t, mustRender(t, tc.values)), "Role", "patrol-notifications-controller-applications", &role)
			got := false
			for _, r := range role.Rules {
				if slices.Contains(r.Resources, "secrets") && slices.Equal(r.ResourceNames, []string{"patrol-notifications-secret"}) {
					got = true
				}
			}
			if got != tc.want {
				t.Errorf("self-service Secret rule present = %v, want %v (rules %+v)", got, tc.want, role.Rules)
			}
		})
	}
}

// TestIngressBackendPort: the Ingress targets the https Service port unless server.insecure
// (OPS-113, OPS-114), checked on the profiles that render an Ingress.
func TestIngressBackendPort(t *testing.T) {
	t.Parallel()
	for profile, want := range map[string]string{"all-features.yaml": "https", "ingress-nginx.yaml": "http"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(chartDir(), "ci", profile))
			if err != nil {
				t.Fatal(err)
			}
			objs := objects(t, mustRender(t, string(data)))
			found := 0
			for _, o := range objs {
				if o.kind != "Ingress" {
					continue
				}
				found++
				var ing networkingv1.Ingress
				if err := yaml.UnmarshalStrict(o.doc, &ing); err != nil {
					t.Fatal(err)
				}
				for _, rule := range ing.Spec.Rules {
					for _, p := range rule.HTTP.Paths {
						if got := p.Backend.Service.Port.Name; got != want {
							t.Errorf("Ingress %s backend port %q, want %q", o.name, got, want)
						}
					}
				}
			}
			if found != 1 {
				t.Errorf("%d Ingresses rendered, want 1", found)
			}
		})
	}
}

// TestBootstrapRoleDelete: the bootstrap Role may delete patrol-initial-admin-secret and
// nothing else (finding DV-6), with the server or the controller as the primary workload.
func TestBootstrapRoleDelete(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]string{"server": "", "core": "server:\n  enabled: false\n"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var role rbacv1.Role
			decodeObject(t, objects(t, mustRender(t, values)), "Role", "patrol-bootstrap", &role)
			var deletes []rbacv1.PolicyRule
			for _, r := range role.Rules {
				if slices.Contains(r.Verbs, "delete") || slices.Contains(r.Verbs, "*") {
					deletes = append(deletes, r)
				}
			}
			want := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"patrol-initial-admin-secret"}, Verbs: []string{"delete"}}
			if len(deletes) != 1 || fmt.Sprint(deletes[0]) != fmt.Sprint(want) {
				t.Errorf("delete rules = %+v, want exactly %+v", deletes, want)
			}
		})
	}
}
