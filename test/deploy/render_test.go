package deploy_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// object is one rendered Kubernetes object.
type object struct {
	// raw is the YAML document exactly as rendered (duplicate keys included).
	raw []byte
	// source is helm's "# Source:" comment ("" for other inputs).
	source string
	u      *unstructured.Unstructured
}

// id is kind/namespace/name (namespace empty for cluster-scoped objects).
func (o object) id() string {
	return o.u.GetKind() + "/" + o.u.GetNamespace() + "/" + o.u.GetName()
}

// profile is one values file of deploy/helm/patrol-cd/ci.
type profile struct {
	name string // base name without .yaml
	path string
}

// profiles returns the chart's CI profiles (ci/*.yaml, ci/invalid excluded), sorted.
func profiles(t testing.TB) []profile {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(chartDir(), "ci", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no profiles in %s", filepath.Join(chartDir(), "ci"))
	}
	sort.Strings(files)
	out := make([]profile, 0, len(files))
	for _, f := range files {
		out = append(out, profile{name: strings.TrimSuffix(filepath.Base(f), ".yaml"), path: f})
	}
	return out
}

// templateArgs are the helm template arguments of a static render.
func templateArgs(release, namespace string, valueFiles []string, extra ...string) []string {
	args := []string{"template", release, chartDir(), "-n", namespace, "--kube-version", kubeVersion}
	for _, f := range valueFiles {
		args = append(args, "-f", f)
	}
	return append(args, extra...)
}

var renderCache sync.Map // key string → []byte

// render runs helm template (cached per argument list within the test binary).
func render(t testing.TB, release, namespace string, valueFiles []string, extra ...string) []byte {
	t.Helper()
	helm := helmBin(t)
	args := templateArgs(release, namespace, valueFiles, extra...)
	key := strings.Join(args, "\x00")
	if v, ok := renderCache.Load(key); ok {
		return v.([]byte)
	}
	r := runCmd(t, 2*time.Minute, helmEnv(t), helm, args...)
	if r.err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), r.err, r.stderr)
	}
	renderCache.Store(key, r.stdout)
	return r.stdout
}

// renderProfile renders a CI profile with the fixed release name and namespace.
func renderProfile(t testing.TB, p profile) []object {
	t.Helper()
	objs, err := parseObjects(render(t, releaseName, releaseNS, []string{p.path}))
	if err != nil {
		t.Fatalf("profile %s: %v", p.name, err)
	}
	return objs
}

// parseObjects splits a multi-document YAML stream into objects; empty documents are skipped.
func parseObjects(data []byte) ([]object, error) {
	rd := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []object
	for {
		doc, err := rd.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return nil, fmt.Errorf("invalid YAML document: %w\n%s", err, doc)
		}
		if len(m) == 0 {
			continue
		}
		o := object{raw: doc, u: &unstructured.Unstructured{Object: m}}
		for _, line := range strings.Split(string(doc), "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(line), "# Source: "); ok {
				o.source = s
				break
			}
		}
		out = append(out, o)
	}
}

// byKind returns the objects of the given kinds.
func byKind(objs []object, kinds ...string) []object {
	var out []object
	for _, o := range objs {
		if slices.Contains(kinds, o.u.GetKind()) {
			out = append(out, o)
		}
	}
	return out
}

// find returns the object with that kind and name (any namespace), or nil.
func find(objs []object, kind, name string) *object {
	for i := range objs {
		if objs[i].u.GetKind() == kind && objs[i].u.GetName() == name {
			return &objs[i]
		}
	}
	return nil
}

// TestChartLint runs `helm lint --strict` on every profile.
func TestChartLint(t *testing.T) {
	t.Parallel()
	helm := helmBin(t)
	for _, p := range profiles(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			r := runCmd(t, 2*time.Minute, helmEnv(t), helm, "lint", "--strict", chartDir(), "--kube-version", kubeVersion, "-f", p.path)
			if r.err != nil {
				t.Fatalf("helm lint --strict -f %s: %v\n%s", p.path, r.err, r.combined())
			}
			if !strings.Contains(r.combined(), "1 chart(s) linted, 0 chart(s) failed") {
				t.Fatalf("unexpected helm lint output:\n%s", r.combined())
			}
			for _, line := range strings.Split(r.combined(), "\n") {
				if strings.Contains(line, "[WARNING]") || strings.Contains(line, "[ERROR]") {
					t.Errorf("helm lint: %s", line)
				}
			}
		})
	}
}

// TestChartRenderDeterministic renders every profile twice (two helm processes) and requires
// identical output that parses into uniquely identified objects.
func TestChartRenderDeterministic(t *testing.T) {
	t.Parallel()
	helm := helmBin(t)
	total := 0
	var mu sync.Mutex
	t.Cleanup(func() { t.Logf("render matrix: %d profiles, %d objects in total", len(profiles(t)), total) })
	for _, p := range profiles(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			args := templateArgs(releaseName, releaseNS, []string{p.path})
			var outs [2][]byte
			for i := range outs {
				r := runCmd(t, 2*time.Minute, helmEnv(t), helm, args...)
				if r.err != nil {
					t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), r.err, r.stderr)
				}
				outs[i] = r.stdout
			}
			if !bytes.Equal(outs[0], outs[1]) {
				t.Fatalf("two renders of %s differ", p.name)
			}
			objs, err := parseObjects(outs[0])
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, o := range objs {
				if o.u.GetAPIVersion() == "" || o.u.GetKind() == "" || o.u.GetName() == "" {
					t.Errorf("object without apiVersion, kind or name (%s):\n%s", o.source, o.raw)
					continue
				}
				if seen[o.id()] {
					t.Errorf("duplicate object %s", o.id())
				}
				seen[o.id()] = true
			}
			mu.Lock()
			total += len(objs)
			mu.Unlock()
			t.Logf("%s: %d objects, render identical twice", p.name, len(objs))
		})
	}
}

// TestValuesSchema checks that values.schema.json (plus the chart's own validations)
// accepts every profile and rejects deliberately bad values: key typos at every level, wrong
// types and invalid enumerations, plus every ci/invalid file with its "# expect:" message.
func TestValuesSchema(t *testing.T) {
	t.Parallel()
	helm := helmBin(t)
	for _, p := range profiles(t) {
		t.Run("accepts/"+p.name, func(t *testing.T) {
			t.Parallel()
			r := runCmd(t, 2*time.Minute, helmEnv(t), helm, templateArgs(releaseName, releaseNS, []string{p.path})...)
			if r.err != nil {
				t.Fatalf("profile rejected: %v\n%s", r.err, r.stderr)
			}
		})
	}
	type badCase struct {
		name, values string
		// want are substrings that must all appear in helm's error output.
		want []string
	}
	cases := []badCase{
		{"typo-top-level", "servr:\n  replicas: 2\n", []string{"at ''", "additional properties 'servr' not allowed"}},
		{"typo-component", "server:\n  replica: 2\n", []string{"at '/server'", "'replica' not allowed"}},
		{"typo-nested", "server:\n  service:\n    typ: NodePort\n", []string{"at '/server/service'", "'typ' not allowed"}},
		{"typo-global", "global:\n  imageRegistery: r.example\n", []string{"at '/global'", "'imageRegistery' not allowed"}},
		// Kubernetes fragments (securityContext, affinity, ...) are free-form in the schema: a typo
		// there is caught by the API server's strict field validation instead
		// (TestAPIServerDryRunNegativeControls/unknown-field-from-values-typo).
		{"type-replicas-string", "server:\n  replicas: two\n", []string{"at '/server/replicas'", "got string, want integer"}},
		{"type-bool-string", "crds:\n  keep: \"yes\"\n", []string{"at '/crds/keep'", "got string, want boolean"}},
		{"type-list-for-map", "global:\n  commonLabels: [a, b]\n", []string{"at '/global/commonLabels'", "got array, want object"}},
		{"type-map-for-list", "applicationNamespaces:\n  team-a: true\n", []string{"at '/applicationNamespaces'", "got object, want array"}},
		{"type-negative-replicas", "controller:\n  replicas: -1\n", []string{"at '/controller/replicas'"}},
		{"enum-internal-tls", "internalTLS:\n  mode: bogus\n", []string{"at '/internalTLS/mode'", "value must be one of"}},
		{"enum-anti-affinity", "global:\n  podAntiAffinity: sometimes\n", []string{"at '/global/podAntiAffinity'", "value must be one of"}},
		{"enum-rbac-scope", "rbac:\n  scope: world\n", []string{"at '/rbac/scope'", "value must be one of"}},
		{"enum-pull-policy", "image:\n  pullPolicy: Sometimes\n", []string{"at '/image/pullPolicy'"}},
		{"enum-service-type", "server:\n  service:\n    type: Magic\n", []string{"at '/server/service/type'"}},
	}
	invalid, err := filepath.Glob(filepath.Join(chartDir(), "ci", "invalid", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(invalid)
	for _, f := range invalid {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		for _, line := range strings.Split(string(data), "\n") {
			if s, ok := strings.CutPrefix(line, "# expect: "); ok {
				want = strings.TrimSpace(s)
				break
			}
		}
		if want == "" {
			t.Errorf("%s: no '# expect: <message>' line", f)
			continue
		}
		cases = append(cases, badCase{name: "ci-invalid/" + strings.TrimSuffix(filepath.Base(f), ".yaml"), values: string(data), want: []string{want}})
	}
	for _, c := range cases {
		t.Run("rejects/"+c.name, func(t *testing.T) {
			t.Parallel()
			f := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(f, []byte(c.values), 0o600); err != nil {
				t.Fatal(err)
			}
			r := runCmd(t, 2*time.Minute, helmEnv(t), helm, templateArgs(releaseName, releaseNS, []string{f})...)
			if r.err == nil {
				t.Fatalf("values accepted, want an error containing %q:\n%s", c.want, c.values)
			}
			for _, w := range c.want {
				if !strings.Contains(string(r.stderr), w) {
					t.Errorf("error lacks %q:\n%s", w, r.stderr)
				}
			}
		})
	}
	t.Logf("schema: %d profiles accepted, %d bad value sets rejected (%d from ci/invalid)", len(profiles(t)), len(cases), len(invalid))
}
