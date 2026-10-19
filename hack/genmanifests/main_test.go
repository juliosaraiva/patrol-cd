package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) []object {
	t.Helper()
	objs, err := parseObjects([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func encode(t *testing.T, objs []object) string {
	t.Helper()
	b, err := encodeObjects(objs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const helmDeployment = `---
# Source: patrol-cd/templates/server/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: patrol-server
  namespace: patrol
  labels:
    app.kubernetes.io/name: patrol-server
    app.kubernetes.io/instance: patrol
    app.kubernetes.io/managed-by: Helm
    helm.sh/chart: patrol-cd-0.1.0
  annotations:
    meta.helm.sh/release-name: patrol
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: patrol-server
      app.kubernetes.io/instance: patrol
  template:
    metadata:
      labels:
        app.kubernetes.io/name: patrol-server
        app.kubernetes.io/instance: patrol
        app.kubernetes.io/managed-by: Helm
        helm.sh/chart: patrol-cd-0.1.0
      annotations:
        checksum/cmd-params: abc
    spec:
      containers:
        - name: server # a comment
          image: patrol-cd/patrol-cd:0.1.0
---
# Source: patrol-cd/templates/crds/x.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: things.cd.patrol.example
  labels:
    helm.sh/chart: patrol-cd-0.1.0
    app.kubernetes.io/managed-by: Helm
  annotations:
    helm.sh/resource-policy: keep
spec:
  group: cd.patrol.example
`

func TestSanitizeStripsHelmMetadata(t *testing.T) {
	t.Parallel()
	objs := mustParse(t, helmDeployment)
	if err := sanitizeAll(objs); err != nil {
		t.Fatal(err)
	}
	out := encode(t, objs)
	for _, bad := range []string{"helm.sh", "managed-by", "meta.helm.sh", "# a comment", "# Source"} {
		if strings.Contains(out, bad) {
			t.Errorf("sanitized output still contains %q:\n%s", bad, out)
		}
	}
	for _, keep := range []string{"checksum/cmd-params: abc", "app.kubernetes.io/instance: patrol", "image: patrol-cd/patrol-cd:0.1.0"} {
		if !strings.Contains(out, keep) {
			t.Errorf("sanitized output lost %q:\n%s", keep, out)
		}
	}
	// The CRD's labels and annotations were all Helm metadata: both maps disappear.
	if strings.Contains(out, "keep") || strings.Count(out, "annotations:") != 1 {
		t.Errorf("empty metadata maps not removed:\n%s", out)
	}
}

func TestSanitizeRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ doc, want string }{
		"hook": {`apiVersion: v1
kind: Pod
metadata:
  name: test
  namespace: patrol
  annotations:
    helm.sh/hook: test
`, "Helm hook"},
		"other manager": {`apiVersion: v1
kind: ConfigMap
metadata:
  name: c
  namespace: patrol
  labels:
    app.kubernetes.io/managed-by: Flux
`, "unexpected app.kubernetes.io/managed-by"},
		"selector": {`apiVersion: v1
kind: Service
metadata:
  name: s
  namespace: patrol
spec:
  selector:
    helm.sh/chart: patrol-cd-0.1.0
`, "outside metadata.labels/annotations"},
		"secret data": {`apiVersion: v1
kind: Secret
metadata:
  name: s
  namespace: patrol
data:
  key: dmFsdWU=
`, "secret with data"},
		"secret stringData": {`apiVersion: v1
kind: Secret
metadata:
  name: s
  namespace: patrol
stringData:
  key: value
`, "secret with stringData"},
		"placeholder": {`apiVersion: v1
kind: ConfigMap
metadata:
  name: c
  namespace: patrol
data:
  url: http://RELEASE-NAME-server
`, "placeholder"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := sanitizeAll(mustParse(t, tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// An empty Secret (patrol-secret, filled at runtime) is fine.
	if err := sanitizeAll(mustParse(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: s, namespace: patrol}\ntype: Opaque\n")); err != nil {
		t.Fatal(err)
	}
}

func TestParseObjects(t *testing.T) {
	t.Parallel()
	objs := mustParse(t, "---\n---\n# only a comment\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: patrol\n")
	if len(objs) != 1 || objs[0].id() != "v1/ConfigMap/patrol/a" {
		t.Fatalf("objects = %+v", objs)
	}
	for name, doc := range map[string]string{
		"duplicate key":    "apiVersion: v1\nkind: ConfigMap\nkind: Secret\nmetadata: {name: a}\n",
		"duplicate object": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n",
		"not a mapping":    "- a\n- b\n",
		"no name":          "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
	} {
		if _, err := parseObjects([]byte(doc)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestCheckScope(t *testing.T) {
	t.Parallel()
	crds := mustParse(t, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: a.cd.patrol.example}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: p}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: p}
`)
	role := mustParse(t, "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: r}\n")
	cm := mustParse(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: patrol}\n")
	elsewhere := mustParse(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: team-a}\n")
	ns := mustParse(t, "apiVersion: v1\nkind: Namespace\nmetadata: {name: patrol}\n")
	cluster := variant{file: "install.yaml", cluster: true}
	namespaced := variant{file: "namespace-install.yaml"}

	if err := checkScope(cluster, crds, append(role, cm...)); err != nil {
		t.Errorf("valid cluster-wide variant: %v", err)
	}
	if err := checkScope(namespaced, nil, cm); err != nil {
		t.Errorf("valid namespaced variant: %v", err)
	}
	for name, tc := range map[string]struct {
		v          variant
		crds, rest []object
		want       string
	}{
		"cluster without policy":  {cluster, crds[:1], cm, "without a ValidatingAdmissionPolicy"},
		"namespaced with CRDs":    {namespaced, crds, cm, "namespace-scoped variant renders"},
		"namespaced ClusterRole":  {namespaced, nil, role, "cluster-scoped"},
		"other namespace":         {cluster, crds, elsewhere, `not in namespace "patrol"`},
		"chart renders namespace": {cluster, crds, ns, "the generator adds its own"},
	} {
		if err := checkScope(tc.v, tc.crds, tc.rest); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestSortObjects(t *testing.T) {
	t.Parallel()
	objs := mustParse(t, `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: patrol-controller, namespace: patrol}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: b, namespace: patrol}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: h, namespace: patrol}
---
apiVersion: example.com/v1
kind: Zebra
metadata: {name: z, namespace: patrol}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: a, namespace: patrol}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: n, namespace: patrol}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: patrol-server, namespace: patrol}
---
apiVersion: v1
kind: Namespace
metadata: {name: patrol}
`)
	sortObjects(objs)
	var got []string
	for _, o := range objs {
		got = append(got, o.kind+"/"+o.name)
	}
	want := "Namespace/patrol NetworkPolicy/n ConfigMap/a ConfigMap/b Deployment/patrol-server StatefulSet/patrol-controller HorizontalPodAutoscaler/h Zebra/z"
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v\nwant    %s", got, want)
	}
}

func TestVerifyVariant(t *testing.T) {
	t.Parallel()
	raw := []byte(helmDeployment + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: patrol-cm
  namespace: patrol
  labels:
    helm.sh/chart: patrol-cd-0.1.0
data:
  mode: "0440"
`)
	objs := mustParse(t, string(raw))
	if err := sanitizeAll(objs); err != nil {
		t.Fatal(err)
	}
	ns, err := namespaceObject("0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	crds, rest := splitCRDs(objs)
	sortObjects(rest)
	good := encode(t, append(append([]object{ns}, crds...), rest...))
	cluster := variant{cluster: true}
	if err := verifyVariant(cluster, []byte(good), raw, "0.1.0"); err != nil {
		t.Fatalf("faithful manifest rejected: %v", err)
	}
	for name, tc := range map[string]struct{ built, want string }{
		"changed field":     {strings.Replace(good, `mode: "0440"`, `mode: "0644"`, 1), "differs from the chart render: v1/ConfigMap/patrol/patrol-cm"},
		"unquoted octal":    {strings.Replace(good, `mode: "0440"`, `mode: 0440`, 1), "differs from the chart render"},
		"missing object":    {good[:strings.LastIndex(good, "---")], "missing apps/v1/Deployment/patrol/patrol-server"},
		"extra object":      {good + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: patrol}\n", "not in the chart render"},
		"helm label kept":   {strings.Replace(good, "name: patrol-cm\n", "name: patrol-cm\n  labels:\n    helm.sh/chart: x\n", 1), "differs"},
		"namespace not 1st": {encode(t, append(append(append([]object{}, crds...), ns), rest...)), "namespace is not the first object"},
		"kind order":        {encode(t, append(append([]object{ns}, crds...), rest[1], rest[0])), "apply order"},
	} {
		if err := verifyVariant(cluster, []byte(tc.built), raw, "0.1.0"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// A namespaced variant must not contain the namespace.
	if err := verifyVariant(variant{}, []byte(good), raw, "0.1.0"); err == nil || !strings.Contains(err.Error(), "not in the chart render: v1/Namespace//patrol") {
		t.Errorf("namespaced variant with a namespace: err = %v", err)
	}
}

func TestCommentWraps(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 40)
	out := comment("Title.", "", long, "  kubectl apply --server-side --force-conflicts -f "+strings.Repeat("x", 120))
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			t.Fatalf("not a comment line: %q", line)
		}
		if len(line) > commentWidth && !strings.HasPrefix(line, "#   kubectl") {
			t.Errorf("line longer than %d: %q", commentWidth, line)
		}
	}
	if !strings.Contains(out, "#\n") {
		t.Errorf("blank line not kept:\n%s", out)
	}
}

func TestVariantsAreConsistent(t *testing.T) {
	t.Parallel()
	dirs, files := map[string]bool{}, map[string]bool{}
	for _, v := range variants {
		if dirs[v.dir] || files[v.file] {
			t.Errorf("duplicate variant %s / %s", v.dir, v.file)
		}
		dirs[v.dir], files[v.file] = true, true
		for _, p := range v.profiles {
			if _, err := os.Stat(filepath.Join("..", "..", "deploy", "manifests", "profiles", p)); err != nil {
				t.Errorf("%s: %v", v.file, err)
			}
		}
		if strings.HasPrefix(v.dir, "ha/") != strings.HasPrefix(v.file, "ha/") {
			t.Errorf("%s and %s disagree on the HA directory", v.dir, v.file)
		}
		if strings.Contains(v.file, "hydrator") != (v.profiles[len(v.profiles)-1] == "hydrator.yaml") {
			t.Errorf("%s: hydrator profile mismatch %v", v.file, v.profiles)
		}
	}
}

// repoOptions are the generator options of the repository, from this package's directory.
func repoOptions(t *testing.T) options {
	t.Helper()
	root := filepath.Join("..", "..")
	o := options{
		chart:       filepath.Join(root, "deploy", "helm", "patrol-cd"),
		out:         filepath.Join(root, "deploy", "manifests"),
		helm:        envOr("HELM", "helm"),
		kustomize:   envOr("KUSTOMIZE", filepath.Join(root, "bin", "kustomize")),
		kubeVersion: "1.36.0",
	}
	for _, tool := range []string{o.helm, o.kustomize} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("PATROL_REQUIRE_DEPLOY_TOOLS") != "" {
				t.Fatalf("PATROL_REQUIRE_DEPLOY_TOOLS is set but %s is missing: %v", tool, err)
			}
			t.Skipf("SKIPPED: %s is not available (%v); set PATROL_REQUIRE_DEPLOY_TOOLS=1 to make this a failure", tool, err)
		}
	}
	return o
}

// TestManifestsAreCurrent renders the chart twice: both runs must produce identical bytes, equal
// to the committed deploy/manifests (the same check as `make verify-manifests`).
func TestManifestsAreCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("SKIPPED: renders the chart (helm, kustomize) in -short mode")
	}
	o := repoOptions(t)
	first, err := generate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("runs generated %d and %d files", len(first), len(second))
	}
	for rel, b := range first {
		if !bytes.Equal(b, second[rel]) {
			t.Errorf("non-deterministic output: %s", rel)
		}
	}
	if err := checkFiles(o.out, first); err != nil {
		t.Fatal(err)
	}
}
