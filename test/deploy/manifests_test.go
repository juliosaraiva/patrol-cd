package deploy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// manifestVariant is a single-file static manifest and the chart values it is generated from
// (hack/genmanifests; deploy/manifests/profiles).
type manifestVariant struct {
	file     string   // relative to deploy/manifests
	base     string   // kustomize base directory with the same objects
	profiles []string // value files in deploy/manifests/profiles
	cluster  bool     // includes the namespace, CRDs and the admission policy
}

var manifestVariants = []manifestVariant{
	{"install.yaml", "cluster-install", []string{"install.yaml"}, true},
	{"install-with-hydrator.yaml", "cluster-install-with-hydrator", []string{"install.yaml", "hydrator.yaml"}, true},
	{"namespace-install.yaml", "namespace-install", []string{"namespace-install.yaml"}, false},
	{"namespace-install-with-hydrator.yaml", "namespace-install-with-hydrator", []string{"namespace-install.yaml", "hydrator.yaml"}, false},
	{"core-install.yaml", "core-install", []string{"install.yaml", "core-install.yaml"}, true},
	{"core-install-with-hydrator.yaml", "core-install-with-hydrator", []string{"install.yaml", "core-install.yaml", "hydrator.yaml"}, true},
	{"ha/install.yaml", "ha/cluster-install", []string{"install.yaml", "ha.yaml"}, true},
	{"ha/install-with-hydrator.yaml", "ha/cluster-install-with-hydrator", []string{"install.yaml", "ha.yaml", "hydrator.yaml"}, true},
	{"ha/namespace-install.yaml", "ha/namespace-install", []string{"namespace-install.yaml", "ha.yaml"}, false},
	{"ha/namespace-install-with-hydrator.yaml", "ha/namespace-install-with-hydrator", []string{"namespace-install.yaml", "ha.yaml", "hydrator.yaml"}, false},
}

func manifestsDir() string { return filepath.Join(repoRoot(), "deploy", "manifests") }

// requireManifests skips (with a reason) until deploy/manifests exists.
func requireManifests(t testing.TB) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manifestsDir(), "install.yaml")); err != nil {
		t.Skipf("SKIPPED: deploy/manifests/install.yaml does not exist yet: %v", err)
	}
}

// stripHelm removes the Helm-only metadata the static manifests do not carry.
func stripHelm(u *unstructured.Unstructured) *unstructured.Unstructured {
	u = u.DeepCopy()
	var walk func(v any)
	clean := func(meta map[string]any) {
		if l, ok := meta["labels"].(map[string]any); ok {
			delete(l, "helm.sh/chart")
			if l["app.kubernetes.io/managed-by"] == "Helm" {
				delete(l, "app.kubernetes.io/managed-by")
			}
			if len(l) == 0 {
				delete(meta, "labels")
			}
		}
		if a, ok := meta["annotations"].(map[string]any); ok {
			for k := range a {
				if strings.HasPrefix(k, "helm.sh/") || strings.HasPrefix(k, "meta.helm.sh/") {
					delete(a, k)
				}
			}
			if len(a) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "metadata" {
					if m, ok := child.(map[string]any); ok {
						clean(m)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(u.Object)
	return u
}

// TestStaticManifestsMatchChart checks that every static manifest holds exactly the objects
// the chart renders with the variant's values (Helm metadata removed), plus only the
// Namespace in cluster-wide variants, that its kustomize base builds to the same objects, and
// that crds.yaml equals deploy/crds.
func TestStaticManifestsMatchChart(t *testing.T) {
	t.Parallel()
	requireManifests(t)
	kustomize := kustomizeBin(t)
	for _, v := range manifestVariants {
		t.Run(v.file, func(t *testing.T) {
			t.Parallel()
			static, err := readObjects(filepath.Join(manifestsDir(), v.file))
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, p := range v.profiles {
				files = append(files, filepath.Join(manifestsDir(), "profiles", p))
			}
			rendered, err := parseObjects(render(t, releaseName, releaseNS, files))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]*unstructured.Unstructured{}
			for _, o := range rendered {
				want[o.id()] = stripHelm(o.u)
			}
			got := map[string]*unstructured.Unstructured{}
			workloads := 0
			for _, o := range static {
				if o.u.GetKind() == "Namespace" {
					if !v.cluster || o.u.GetName() != releaseNS {
						t.Errorf("unexpected Namespace %s", o.u.GetName())
					}
					if o.u.GetLabels()["pod-security.kubernetes.io/enforce"] != "restricted" {
						t.Errorf("Namespace %s does not enforce Pod Security restricted", o.u.GetName())
					}
					continue
				}
				got[o.id()] = o.u
				if o.u.GetKind() == "Deployment" || o.u.GetKind() == "StatefulSet" {
					workloads++
				}
			}
			var missing, extra, differ []string
			for id, w := range want {
				g, ok := got[id]
				switch {
				case !ok:
					missing = append(missing, id)
				case !reflect.DeepEqual(w.Object, g.Object):
					differ = append(differ, id)
				}
			}
			for id := range got {
				if _, ok := want[id]; !ok {
					extra = append(extra, id)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			sort.Strings(differ)
			if len(missing)+len(extra)+len(differ) > 0 {
				t.Errorf("static manifest differs from the chart render (%v):\nmissing:\n%s\nextra:\n%s\ndifferent:\n%s", v.profiles, joinLines(missing), joinLines(extra), joinLines(differ))
			}
			// The kustomize base builds to the same objects.
			r := runCmd(t, 2*time.Minute, nil, kustomize, "build", filepath.Join(manifestsDir(), v.base))
			if r.err != nil {
				t.Fatalf("kustomize build %s: %v\n%s", v.base, r.err, r.stderr)
			}
			built, err := parseObjects(r.stdout)
			if err != nil {
				t.Fatal(err)
			}
			if len(built) != len(static) {
				t.Errorf("kustomize build %s: %d objects, %s has %d", v.base, len(built), v.file, len(static))
			}
			for i := range built {
				if i < len(static) && !reflect.DeepEqual(built[i].u.Object, static[i].u.Object) {
					t.Errorf("kustomize build %s: object %d (%s) differs from %s", v.base, i, built[i].id(), v.file)
				}
			}
			t.Logf("%s: %d objects (%d workloads) equal to the chart render with %v; kustomize base %s identical", v.file, len(got), workloads, v.profiles, v.base)
		})
	}
	t.Run("crds.yaml", func(t *testing.T) {
		t.Parallel()
		static, err := readObjects(filepath.Join(manifestsDir(), "crds.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(crdDir(), "*.yaml"))
		n := 0
		for _, f := range files {
			src, err := readObjects(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range src {
				n++
				g := find(static, s.u.GetKind(), s.u.GetName())
				if g == nil {
					t.Errorf("%s missing from crds.yaml", s.id())
					continue
				}
				if !reflect.DeepEqual(s.u.Object["spec"], g.u.Object["spec"]) {
					t.Errorf("%s: spec differs from %s", s.id(), filepath.Base(f))
				}
			}
		}
		if len(static) != n {
			t.Errorf("crds.yaml has %d objects, deploy/crds %d", len(static), n)
		}
	})
}

// TestStaticManifestsApply server-side applies install.yaml object by object (what `kubectl
// apply --server-side --force-conflicts -f install.yaml` does) to a fresh envtest API server
// with strict field validation, re-applies it (no object may change), and deletes it. Every
// other variant is validated with a strict server-side dry run.
func TestStaticManifestsApply(t *testing.T) {
	t.Parallel()
	requireManifests(t)
	k := freshEnv(t, envOptions{name: "manifests"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	objs, err := readObjects(filepath.Join(manifestsDir(), "install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	kube, warns := k.warningClient(t)
	rc := kube.Discovery().RESTClient()
	apply := func(pass string) map[string]string {
		rv := map[string]string{}
		for _, o := range objs {
			warns.take()
			if err := k.apply(ctx, rc, o, "", applyOptions{fieldManager: "kubectl", force: true, strict: true}); err != nil {
				t.Fatalf("%s: %v", pass, err)
			}
			for _, w := range warns.take() {
				t.Errorf("%s: %s: warning: %s", pass, o.id(), w)
			}
			got, err := k.get(ctx, o, "")
			if err != nil || got == nil {
				t.Fatalf("%s: %s not readable after apply: %v", pass, o.id(), err)
			}
			// Objects with a generation (spec changes bump it) are compared by generation:
			// their status is written by API server controllers (CRD conditions) in between.
			if g := got.GetGeneration(); g > 0 {
				rv[o.id()] = "generation " + strconv.FormatInt(g, 10)
			} else {
				rv[o.id()] = "resourceVersion " + got.GetResourceVersion()
			}
		}
		return rv
	}
	first := apply("apply")
	second := apply("re-apply")
	changed := 0
	for id, v := range first {
		if second[id] != v {
			changed++
			t.Errorf("re-apply changed %s (%s → %s)", id, v, second[id])
		}
	}
	t.Logf("install.yaml: %d objects applied, re-applied with %d changes", len(objs), changed)

	// Delete in reverse order (the namespace last; envtest has no namespace controller, so
	// the namespace stays Terminating, which is checked instead of its removal).
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		m := mustMapping(t, k, o.u.GroupVersionKind())
		var err error
		if namespaced, _ := k.isNamespaced(o); namespaced {
			err = k.dyn.Resource(m.Resource).Namespace(o.u.GetNamespace()).Delete(ctx, o.u.GetName(), metav1.DeleteOptions{})
		} else {
			err = k.dyn.Resource(m.Resource).Delete(ctx, o.u.GetName(), metav1.DeleteOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete %s: %v", o.id(), err)
		}
	}
	gone, terminating := 0, 0
	for _, o := range objs {
		timeout := 2 * time.Minute
		if o.u.GetKind() == "CustomResourceDefinition" {
			// A CRD is removed only after the API server's customresourcecleanup finalizer
			// has deleted its instances and removed itself. Under full-suite load (about 20
			// envtest control planes and -race component processes) one CRD once stayed
			// present for more than 2 minutes, while this test passes in 21-25 s alone; the
			// CRD wait is therefore 5 minutes, and a timeout reports the CRD's state.
			timeout = 5 * time.Minute
		}
		got := waitDeleted(ctx, t, k, o, timeout)
		if got == nil {
			gone++
		} else {
			terminating++
			t.Logf("terminating (finalizers %v): %s", got.GetFinalizers(), o.id())
		}
	}
	t.Logf("install.yaml deleted: %d objects gone, %d terminating (namespace without a namespace controller)", gone, terminating)

	// Strict dry run of every variant (and crds.yaml) against the shared control plane.
	sk := sharedEnv(t)
	if err := sk.ensureNamespace(ctx, releaseNS, nil); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, v := range append([]string{"crds.yaml"}, func() []string {
		var fs []string
		for _, mv := range manifestVariants {
			fs = append(fs, mv.file)
		}
		return fs
	}()...) {
		vobjs, err := readObjects(filepath.Join(manifestsDir(), v))
		if err != nil {
			t.Fatal(err)
		}
		r := dryRunAll(ctx, t, sk, vobjs)
		for _, p := range r.problems {
			t.Errorf("%s: %s", v, p)
		}
		total += r.validated
	}
	t.Logf("static manifests: %d objects validated by strict dry run across %d files", total, len(manifestVariants)+1)
}

// waitDeleted polls until o is gone (a Namespace: until it is Terminating; envtest runs no
// namespace controller) and returns the last object read (nil when gone). On timeout the
// failure carries what the API server reports about the object (deletionFacts).
func waitDeleted(ctx context.Context, t *testing.T, k *kubeEnv, o object, timeout time.Duration) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastErr := "none"
	for {
		got, err := k.get(ctx, o, "")
		switch {
		case err != nil:
			lastErr = err.Error()
		case got == nil:
			return nil
		case o.u.GetKind() == "Namespace" && got.GetDeletionTimestamp() != nil:
			return got
		}
		if time.Now().After(deadline) {
			// A fresh context: the test's own may be the one that expired.
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			facts := deletionFacts(dctx, k, o)
			cancel()
			t.Fatalf("timed out after %s waiting for %s deleted (last read error: %s): %s", timeout, o.id(), lastErr, facts)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// deletionFacts describes an object that is still present from what the API server returns
// (no audit log needed): deletionTimestamp and its age, finalizers and resourceVersion; for a
// CustomResourceDefinition also its status conditions and the number of instances left.
func deletionFacts(ctx context.Context, k *kubeEnv, o object) string {
	got, err := k.get(ctx, o, "")
	switch {
	case err != nil:
		return "final read failed: " + err.Error()
	case got == nil:
		return "gone at the final read"
	}
	var b strings.Builder
	if dt := got.GetDeletionTimestamp(); dt != nil {
		fmt.Fprintf(&b, "deletionTimestamp %s (%s ago)", dt.UTC().Format(time.RFC3339), time.Since(dt.Time).Round(time.Second))
	} else {
		b.WriteString("no deletionTimestamp (the delete was not accepted)")
	}
	fmt.Fprintf(&b, ", finalizers %v, resourceVersion %s", got.GetFinalizers(), got.GetResourceVersion())
	if got.GetKind() != "CustomResourceDefinition" {
		return b.String()
	}
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		fmt.Fprintf(&b, ", condition %v=%v (reason %v: %v; since %v)", m["type"], m["status"], m["reason"], m["message"], m["lastTransitionTime"])
	}
	if len(conds) == 0 {
		b.WriteString(", no status conditions")
	}
	group, _, _ := unstructured.NestedString(got.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(got.Object, "spec", "names", "plural")
	versions, _, _ := unstructured.NestedSlice(got.Object, "spec", "versions")
	for _, v := range versions {
		m, _ := v.(map[string]any)
		if storage, _ := m["storage"].(bool); !storage {
			continue
		}
		name, _ := m["name"].(string)
		gvr := schema.GroupVersionResource{Group: group, Version: name, Resource: plural}
		res := plural + "." + group + "/" + name
		list, err := k.dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 100})
		if err != nil {
			fmt.Fprintf(&b, ", instances of %s: list failed: %v", res, err)
			continue
		}
		n := int64(len(list.Items))
		if r := list.GetRemainingItemCount(); r != nil {
			n += *r
		}
		fmt.Fprintf(&b, ", %d instances of %s left", n, res)
	}
	return b.String()
}
