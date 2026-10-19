package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// These tests render every ci/ profile with the helm binary (skipped when helm is not
// installed) and check static properties of the output: strict decoding of built-in kinds
// (unknown or duplicated fields fail), Pod Security "restricted" fields, resources, probes,
// volumes and env, Service names that are DNS labels, NetworkPolicy except entries inside their
// cidr, and that every flag passed to patrolcd exists in the subcommand's --help golden
// (cmd/patrolcd/testdata/help). They do not replace an API server dry run or a cluster.

const kubeVersion = "1.36.0"

func helmBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	return p
}

func renderProfile(t *testing.T, helm, chart, values string) []byte {
	t.Helper()
	cmd := exec.Command(helm, "template", "patrol", chart, "-n", "patrol", "--kube-version", kubeVersion, "-f", values)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template -f %s: %v\n%s", values, err, stderr.String())
	}
	return out
}

func splitDocuments(data []byte) [][]byte {
	var out [][]byte
	for _, part := range strings.Split(string(data), "\n---") {
		part = strings.TrimPrefix(part, "\n")
		if s := strings.TrimSpace(part); s == "" || s == "---" {
			continue
		}
		out = append(out, []byte(part))
	}
	return out
}

func TestRenderedProfiles(t *testing.T) {
	t.Parallel()
	helm := helmBinary(t)
	root := filepath.Join("..", "..")
	chart := filepath.Join(root, "deploy", "helm", "patrol-cd")
	profiles, err := filepath.Glob(filepath.Join(chart, "ci", "*.yaml"))
	if err != nil || len(profiles) == 0 {
		t.Fatalf("no profiles: %v", err)
	}
	flags, err := goldenFlags(filepath.Join(root, "cmd", "patrolcd", "testdata", "help"))
	if err != nil {
		t.Fatal(err)
	}
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := apiextv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	dec := serializer.NewCodecFactory(sch, serializer.EnableStrict).UniversalDeserializer()
	for _, p := range profiles {
		t.Run(filepath.Base(p), func(t *testing.T) {
			t.Parallel()
			first := renderProfile(t, helm, chart, p)
			if second := renderProfile(t, helm, chart, p); !bytes.Equal(first, second) {
				t.Fatal("two renders differ")
			}
			for _, doc := range splitDocuments(first) {
				var meta struct {
					Kind     string `json:"kind"`
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				}
				if err := yaml.Unmarshal(doc, &meta); err != nil {
					t.Fatalf("invalid YAML: %v\n%s", err, doc)
				}
				if meta.Kind == "" {
					continue
				}
				id := meta.Kind + "/" + meta.Metadata.Name
				obj, _, err := dec.Decode(doc, nil, nil)
				if runtime.IsNotRegisteredError(err) {
					continue // cert-manager, Prometheus Operator, Gateway API kinds
				}
				if err != nil {
					t.Errorf("%s: strict decode: %v", id, err)
					continue
				}
				var spec *corev1.PodSpec
				switch o := obj.(type) {
				case *appsv1.Deployment:
					spec = &o.Spec.Template.Spec
				case *appsv1.StatefulSet:
					spec = &o.Spec.Template.Spec
				case *corev1.Service:
					if len(o.Name) > 63 {
						t.Errorf("%s: name longer than 63 characters (DNS label)", id)
					}
				case *networkingv1.NetworkPolicy:
					for _, problem := range checkNetworkPolicy(o) {
						t.Errorf("%s: %s", id, problem)
					}
				}
				if spec == nil {
					continue
				}
				for _, problem := range checkPodSpec(spec) {
					t.Errorf("%s: %s", id, problem)
				}
				for _, problem := range checkFlags(spec, flags) {
					t.Errorf("%s: %s", id, problem)
				}
			}
		})
	}
}

// checkPodSpec returns violations of the chart's pod invariants.
func checkPodSpec(s *corev1.PodSpec) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	if s.AutomountServiceAccountToken == nil {
		add("automountServiceAccountToken is not explicit")
	}
	if s.HostNetwork || s.HostPID || s.HostIPC {
		add("uses host namespaces")
	}
	psc := s.SecurityContext
	if psc == nil || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		add("pod seccomp profile is not RuntimeDefault")
	}
	podNonRoot := psc != nil && psc.RunAsNonRoot != nil && *psc.RunAsNonRoot
	vols := map[string]bool{}
	for _, v := range s.Volumes {
		if vols[v.Name] {
			add("duplicate volume %s", v.Name)
		}
		vols[v.Name] = true
		if v.HostPath != nil {
			add("hostPath volume %s", v.Name)
		}
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit == nil {
			add("emptyDir %s has no sizeLimit", v.Name)
		}
	}
	all := append(slices.Clone(s.InitContainers), s.Containers...)
	for i, c := range all {
		init := i < len(s.InitContainers)
		env := map[string]bool{}
		for _, e := range c.Env {
			if env[e.Name] {
				add("%s: duplicate env %s", c.Name, e.Name)
			}
			env[e.Name] = true
		}
		for _, m := range c.VolumeMounts {
			if !vols[m.Name] {
				add("%s: mounts unknown volume %s", c.Name, m.Name)
			}
		}
		sc := c.SecurityContext
		if sc == nil {
			add("%s: no securityContext", c.Name)
			continue
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			add("%s: allowPrivilegeEscalation is not false", c.Name)
		}
		if sc.Privileged != nil && *sc.Privileged {
			add("%s: privileged", c.Name)
		}
		if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) > 0 {
			add("%s: capabilities are not exactly drop ALL", c.Name)
		}
		if !podNonRoot && (sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot) {
			add("%s: runAsNonRoot is not set", c.Name)
		}
		if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
			add("%s: runs as uid 0", c.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			add("%s: root filesystem is writable", c.Name)
		}
		r := c.Resources
		if r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() || r.Limits.Memory().IsZero() {
			add("%s: missing cpu/memory requests or memory limit", c.Name)
		}
		if !init && !strings.HasPrefix(c.Name, "cmp-") && (c.ReadinessProbe == nil || c.LivenessProbe == nil) {
			add("%s: missing readiness or liveness probe", c.Name)
		}
	}
	return p
}

var goldenFlagRE = regexp.MustCompile(`^\s+(?:-[a-zA-Z], )?(--[a-z0-9-]+)`)

// goldenFlags returns the flags of each patrolcd subcommand from its --help golden.
func goldenFlags(dir string) (map[string]map[string]bool, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.golden"))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, line := range strings.Split(string(data), "\n") {
			if m := goldenFlagRE.FindStringSubmatch(line); m != nil {
				set[m[1]] = true
			}
		}
		out[strings.TrimSuffix(filepath.Base(f), ".golden")] = set
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no help goldens in %s", dir)
	}
	return out, nil
}

// checkFlags verifies the flags of containers running /usr/local/bin/patrolcd <subcommand>.
func checkFlags(s *corev1.PodSpec, flags map[string]map[string]bool) []string {
	var p []string
	for _, c := range append(slices.Clone(s.InitContainers), s.Containers...) {
		argv := append(slices.Clone(c.Command), c.Args...)
		if len(argv) < 2 || argv[0] != "/usr/local/bin/patrolcd" {
			continue
		}
		known, ok := flags[argv[1]]
		if !ok {
			p = append(p, fmt.Sprintf("%s: no help golden for subcommand %q", c.Name, argv[1]))
			continue
		}
		for _, a := range argv[2:] {
			if !strings.HasPrefix(a, "--") {
				continue
			}
			name, _, _ := strings.Cut(a, "=")
			if !known[name] {
				p = append(p, fmt.Sprintf("%s: patrolcd %s has no flag %s", c.Name, argv[1], name))
			}
		}
	}
	return p
}

func TestCheckFlagsRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	flags := map[string]map[string]bool{"server": {"--insecure": true}}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name: "server", Args: []string{"/usr/local/bin/patrolcd", "server", "--insecure", "--bogus=1"},
	}}}
	got := checkFlags(spec, flags)
	if len(got) != 1 || !strings.Contains(got[0], "--bogus") {
		t.Fatalf("checkFlags = %v, want one problem about --bogus", got)
	}
}

func TestCheckPodSpecFindsViolations(t *testing.T) {
	t.Parallel()
	escalate := true
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		Containers: []corev1.Container{{
			Name:            "c",
			Env:             []corev1.EnvVar{{Name: "A"}, {Name: "A"}},
			VolumeMounts:    []corev1.VolumeMount{{Name: "missing"}},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &escalate},
		}},
	}
	got := strings.Join(checkPodSpec(spec), "\n")
	for _, want := range []string{"automountServiceAccountToken", "seccomp", "sizeLimit", "duplicate env A", "unknown volume missing", "allowPrivilegeEscalation", "drop ALL", "runAsNonRoot", "writable", "requests", "probe"} {
		if !strings.Contains(got, want) {
			t.Errorf("violations lack %q:\n%s", want, got)
		}
	}
}
