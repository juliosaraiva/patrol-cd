package deploy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// thirdPartyKinds are rendered kinds whose CRDs belong to other projects (cert-manager,
// Prometheus Operator, Gateway API). envtest does not have them, so their objects cannot be
// validated by the API server here; they are counted and reported as not validated.
var thirdPartyKinds = map[schema.GroupVersionKind]bool{
	{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}:                   true,
	{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"}:                        true,
	{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}:          true,
	{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}:          true,
	{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}:           true,
	{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant"}: true,
}

// dryRunReport is the outcome of validating one set of objects.
type dryRunReport struct {
	validated    int
	notValidated []string // third-party objects (no CRD in envtest)
	problems     []string
}

// dryRunAll server-side applies every object with dryRun=All and fieldValidation=Strict and
// records errors and warnings. Namespaces referenced by namespaced objects must exist.
func dryRunAll(ctx context.Context, t testing.TB, k *kubeEnv, objs []object) dryRunReport {
	t.Helper()
	kube, warns := k.warningClient(t)
	rc := kube.Discovery().RESTClient()
	var r dryRunReport
	for _, o := range objs {
		gvk := o.u.GroupVersionKind()
		if thirdPartyKinds[gvk] {
			r.notValidated = append(r.notValidated, o.id())
			continue
		}
		namespaced, err := k.isNamespaced(o)
		if err != nil {
			r.problems = append(r.problems, fmt.Sprintf("%s: no such kind in the API server: %v", o.id(), err))
			continue
		}
		if namespaced && o.u.GetNamespace() == "" {
			r.problems = append(r.problems, fmt.Sprintf("%s: namespaced object without metadata.namespace", o.id()))
		}
		if !namespaced && o.u.GetNamespace() != "" {
			r.problems = append(r.problems, fmt.Sprintf("%s: cluster-scoped object with metadata.namespace", o.id()))
		}
		warns.take()
		err = k.apply(ctx, rc, o, "", applyOptions{fieldManager: "deploy-test", dryRun: true, force: true, strict: true})
		if err != nil {
			r.problems = append(r.problems, err.Error())
		}
		for _, w := range warns.take() {
			r.problems = append(r.problems, fmt.Sprintf("%s: warning: %s", o.id(), w))
		}
		r.validated++
	}
	return r
}

// namespacesOf returns the namespaces of the objects, sorted.
func namespacesOf(objs []object) []string {
	set := map[string]bool{}
	for _, o := range objs {
		if ns := o.u.GetNamespace(); ns != "" {
			set[ns] = true
		}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// TestAPIServerDryRun validates every rendered object of every profile with a server-side
// dry run (strict field validation, admission included) against envtest with the Patrol CRDs
// and the admission policy installed. Any error or warning fails.
func TestAPIServerDryRun(t *testing.T) {
	t.Parallel()
	k := sharedEnv(t)
	var mu sync.Mutex
	totalValidated, totalSkipped := 0, 0
	t.Cleanup(func() {
		t.Logf("API dry run: %d objects validated, %d third-party objects not validated (no CRD in envtest)", totalValidated, totalSkipped)
	})
	for _, p := range profiles(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			objs := renderProfile(t, p)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			for _, ns := range namespacesOf(objs) {
				if err := k.ensureNamespace(ctx, ns, nil); err != nil {
					t.Fatal(err)
				}
			}
			r := dryRunAll(ctx, t, k, objs)
			for _, p := range r.problems {
				t.Error(p)
			}
			if len(r.notValidated) > 0 {
				t.Logf("not validated (third-party CRDs absent): %s", strings.Join(r.notValidated, ", "))
			}
			t.Logf("%s: %d objects validated by dry run, %d not validated", p.name, r.validated, len(r.notValidated))
			mu.Lock()
			totalValidated += r.validated
			totalSkipped += len(r.notValidated)
			mu.Unlock()
		})
	}
}

// TestRenderedCRDsMatchDeployCRDs compares the chart's CRD templates and admission policy
// (rendered by the default profile) with deploy/crds.
func TestRenderedCRDsMatchDeployCRDs(t *testing.T) {
	t.Parallel()
	objs := renderProfile(t, profile{name: "default", path: filepath.Join(chartDir(), "ci", "default.yaml")})
	files, err := filepath.Glob(filepath.Join(crdDir(), "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, f := range files {
		srcs, err := readObjects(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range srcs {
			want++
			got := find(objs, s.u.GetKind(), s.u.GetName())
			if got == nil {
				t.Errorf("%s from %s is not rendered by the chart", s.id(), filepath.Base(f))
				continue
			}
			if !reflect.DeepEqual(s.u.Object["spec"], got.u.Object["spec"]) {
				t.Errorf("%s: rendered spec differs from %s", s.id(), filepath.Base(f))
			}
			// The chart adds Helm labels and the keep annotation; the source labels must remain.
			for key, v := range s.u.GetLabels() {
				if got.u.GetLabels()[key] != v {
					t.Errorf("%s: label %s=%q missing (got %q)", s.id(), key, v, got.u.GetLabels()[key])
				}
			}
			if got.u.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
				t.Errorf("%s: crds.keep (default true) did not set helm.sh/resource-policy: keep", s.id())
			}
		}
	}
	if n := len(byKind(objs, "CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding")); n != want {
		t.Errorf("chart renders %d CRDs/policy objects, deploy/crds has %d", n, want)
	}
}

// TestAPIServerDryRunNegativeControls proves the checks of TestAPIServerDryRun are live:
// an unknown field (a typo inside a free-form values fragment that values.schema.json cannot
// see), a duplicated YAML key and a warning are each reported.
func TestAPIServerDryRunNegativeControls(t *testing.T) {
	t.Parallel()
	k := sharedEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := k.ensureNamespace(ctx, releaseNS, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("unknown-field-from-values-typo", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "typo.yaml")
		if err := os.WriteFile(f, []byte("global:\n  containerSecurityContext:\n    readOnlyRootFileSystem: false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		objs, err := parseObjects(render(t, releaseName, releaseNS, []string{f}))
		if err != nil {
			t.Fatal(err)
		}
		d := find(objs, "Deployment", "patrol-server")
		if d == nil {
			t.Fatal("no patrol-server Deployment")
		}
		r := dryRunAll(ctx, t, k, []object{*d})
		if len(r.problems) != 1 || !strings.Contains(r.problems[0], `containers[name="server"].securityContext.readOnlyRootFileSystem: field not declared in schema`) {
			t.Fatalf("want one strict-validation error for the unknown field, got:\n%s", joinLines(r.problems))
		}
	})

	t.Run("duplicate-key", func(t *testing.T) {
		raw := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dup\n  namespace: " + releaseNS + "\ndata:\n  a: \"1\"\ndata:\n  b: \"2\"\n")
		objs, err := parseObjects(raw)
		if err != nil {
			t.Fatal(err)
		}
		r := dryRunAll(ctx, t, k, objs)
		if len(r.problems) != 1 || !strings.Contains(r.problems[0], `key "data" already set in map`) {
			t.Fatalf("want one duplicate-field error, got:\n%s", joinLines(r.problems))
		}
	})

	t.Run("warning-captured", func(t *testing.T) {
		// Kubernetes 1.36 accepts finalizer names without a "/" on custom resources with a
		// warning (core kinds reject them); Patrol's own finalizer names have this shape (see
		// TASKS.md, finalizer rename).
		raw := fmt.Appendf(nil, "apiVersion: cd.patrol.example/v1alpha1\nkind: AppProject\nmetadata:\n  name: warn\n  namespace: %s\n  finalizers: [resources-finalizer.cd.patrol.example]\nspec: {}\n", releaseNS)
		objs, err := parseObjects(raw)
		if err != nil {
			t.Fatal(err)
		}
		r := dryRunAll(ctx, t, k, objs)
		found := slices.ContainsFunc(r.problems, func(p string) bool { return strings.Contains(p, "warning:") && strings.Contains(p, "finalizer") })
		if !found {
			t.Fatalf("want the finalizer-name warning to be reported, got:\n%s", joinLines(r.problems))
		}
	})

	t.Run("unknown-kind-reported", func(t *testing.T) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1", "kind": "Widget", "metadata": map[string]any{"name": "w", "namespace": releaseNS},
		}}
		r := dryRunAll(ctx, t, k, []object{{raw: []byte("apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n"), u: u}})
		if len(r.problems) != 1 || !strings.Contains(r.problems[0], "no such kind") {
			t.Fatalf("want an unknown-kind problem, got:\n%s", joinLines(r.problems))
		}
	})
}

// mustMapping is a helper for tests that need a REST mapping of a built-in kind.
func mustMapping(t testing.TB, k *kubeEnv, gvk schema.GroupVersionKind) *meta.RESTMapping {
	t.Helper()
	m, err := k.mapping(gvk)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
