package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// helmInput renders testdata/helm/app copied into a fresh repository at path "app".
func helmInput(t testing.TB, h *v1alpha1.ApplicationSourceHelm) Input {
	t.Helper()
	root := copyFixture(t, "helm")
	return Input{
		RootDir:   root,
		AppName:   "test-app",
		Namespace: "test-ns",
		Source:    v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/repo.git", Path: "app", Helm: h},
		Revision:  "0123456789abcdef0123456789abcdef01234567",
	}
}

func cmData(t testing.TB, res *Result, name string) map[string]any {
	t.Helper()
	return nested(t, findObj(t, res.Manifests, "ConfigMap", name), "data").(map[string]any)
}

func TestHelmDefaultCommandAndDetection(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	in := helmInput(t, nil)
	res := generate(t, r, in)
	assertEqual(t, res.SourceType, "Helm")
	assertSlice(t, res.Commands, []string{"helm template . --name-template test-app --namespace test-ns --include-crds"})
	assertSlice(t, names(res.Manifests), []string{"CustomResourceDefinition/widgets.example.com", "ConfigMap/test-app-cm", "Pod/test-app-test"})
	data := cmData(t, res, "test-app-cm")
	assertEqual(t, data["key"].(string), "chart-default")
	if data["kubeVersion"].(string) == "" { // helm's default without a cluster version
		t.Fatal("empty kubeVersion")
	}
	if strings.Contains(res.Commands[0], "--kube-version") {
		t.Fatalf("--kube-version passed without a cluster version: %s", res.Commands[0])
	}
	assertEqual(t, data["hasWidgetAPI"].(string), "false")
	assertEqual(t, nested(t, findObj(t, res.Manifests, "ConfigMap", "test-app-cm"), "metadata", "namespace").(string), "test-ns")

	// The shared checkout is not modified by a render (Chart.lock cleanup, temp files).
	assertTreeUnchanged(t, in.RootDir, func() { generate(t, r, in) })
}

func TestHelmReleaseNameAndNamespace(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))

	in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ReleaseName: "different-release-name", Namespace: "different-namespace"})
	res := generate(t, r, in)
	assertSlice(t, res.Commands, []string{"helm template . --name-template different-release-name --namespace different-namespace --include-crds"})
	findObj(t, res.Manifests, "ConfigMap", "different-release-name-cm")

	// Applications outside the control-plane namespace use "<ns>_<name>" instance names;
	// the release name is the name part.
	in = helmInput(t, nil)
	in.AppName = "team-a_guestbook"
	in.Namespace = ""
	res = generate(t, r, in)
	assertSlice(t, res.Commands, []string{"helm template . --name-template guestbook --include-crds"})
	findObj(t, res.Manifests, "ConfigMap", "guestbook-cm")
}

func TestHelmValueFiles(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	tests := []struct {
		name       string
		files      []string
		ignore     bool
		wantValues []string // --values arguments in the command
		wantEnv    string
		wantKey    string
	}{
		{name: "relative and root-absolute in order", files: []string{"values-a.yaml", "/app/values-b.yaml"},
			wantValues: []string{"./app/values-a.yaml", "./app/values-b.yaml"}, wantEnv: "none", wantKey: "values-b"},
		{name: "order reversed", files: []string{"values-b.yaml", "values-a.yaml"},
			wantValues: []string{"./app/values-b.yaml", "./app/values-a.yaml"}, wantEnv: "none", wantKey: "values-a"},
		{name: "glob lexical", files: []string{"envs/*.yaml"},
			wantValues: []string{"./app/envs/dev.yaml", "./app/envs/prod.yaml"}, wantEnv: "prod", wantKey: "chart-default"},
		{name: "doublestar", files: []string{"envs/**/*.yaml"},
			wantValues: []string{"./app/envs/dev.yaml", "./app/envs/prod.yaml", "./app/envs/sub/deep.yaml"}, wantEnv: "deep", wantKey: "chart-default"},
		{name: "explicit entry keeps its position", files: []string{"envs/*.yaml", "envs/dev.yaml"},
			wantValues: []string{"./app/envs/prod.yaml", "./app/envs/dev.yaml"}, wantEnv: "dev", wantKey: "chart-default"},
		{name: "duplicates across globs", files: []string{"envs/d*.yaml", "envs/*.yaml"},
			wantValues: []string{"./app/envs/dev.yaml", "./app/envs/prod.yaml"}, wantEnv: "prod", wantKey: "chart-default"},
		{name: "env substituted then missing", files: []string{"envs/$ARGOCD_APP_NAMESPACE.yaml"}, ignore: true,
			wantEnv: "none", wantKey: "chart-default"},
		{name: "missing ignored", files: []string{"values-a.yaml", "values-optional-override.yaml", "nomatch/*.yaml"}, ignore: true,
			wantValues: []string{"./app/values-a.yaml"}, wantEnv: "none", wantKey: "values-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: tt.files, IgnoreMissingValueFiles: tt.ignore})
			res := generate(t, r, in)
			if len(res.Commands) != 1 {
				t.Fatalf("commands = %q", res.Commands)
			}
			assertSlice(t, valuesArgs(res.Commands[0]), wantNonNil(tt.wantValues))
			data := cmData(t, res, "test-app-cm")
			assertEqual(t, data["envName"].(string), tt.wantEnv)
			assertEqual(t, data["key"].(string), tt.wantKey)
		})
	}

	t.Run("env substituted file exists", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"envs/${ARGOCD_APP_NAMESPACE}.yaml"}})
		in.Namespace = "dev"
		assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["envName"].(string), "dev")
	})
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func wantNonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// valuesArgs returns the --values arguments of a command line.
func valuesArgs(cmd string) []string {
	f := strings.Fields(cmd)
	out := []string{}
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "--values" {
			out = append(out, f[i+1])
		}
	}
	return out
}

func TestHelmValueFileErrors(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("key: outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, file string
		schemes    []string
		ignore     bool
		toolError  bool // helm's own message, which names the file it could not open
		want       string
		code       string
	}{
		{name: "traversal", file: "../../../../etc/passwd",
			want: "error resolving helm value files: error resolving value file path: file '../../../../etc/passwd' resolved to outside repository root"},
		{name: "traversal ignored flag still errors", file: "../../x.yaml", ignore: true,
			want: "file '../../x.yaml' resolved to outside repository root"},
		{name: "glob without match", file: "nomatch/*.yaml",
			want: `values file glob "nomatch/*.yaml" matched no files`, code: apiclient.CodeNotFound},
		{name: "remote without allowed schemes", file: "https://example.com/values.yaml",
			want: "the URL scheme 'https' is not allowed"},
		{name: "ftp not in defaults", file: "ftp://example.com/values.yaml", schemes: []string{"https", "http"},
			want: "the URL scheme 'ftp' is not allowed"},
		{name: "missing file", file: "values-missing.yaml", toolError: true,
			want: "values-missing.yaml: no such file or directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{tt.file}, IgnoreMissingValueFiles: tt.ignore})
			in.HelmOptions.ValuesFileSchemes = tt.schemes
			err := generateErr(t, r, in)
			assertContains(t, err.Error(), tt.want)
			assertEqual(t, ErrorCode(err), tt.code)
			if tt.toolError {
				assertContains(t, err.Error(), "failed to execute helm template command: failed running helm: `helm template . --name-template test-app --namespace test-ns --values ")
			} else if strings.Contains(err.Error(), in.RootDir) {
				t.Fatalf("error leaks the checkout path: %v", err)
			}
		})
	}

	t.Run("out-of-root symlink", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"link.yaml"}})
		if err := os.Symlink(outside, filepath.Join(in.RootDir, "app", "link.yaml")); err != nil {
			t.Fatal(err)
		}
		assertContains(t, generateErr(t, r, in).Error(), "file 'link.yaml' resolved to outside repository root")
	})
	t.Run("out-of-root symlink in glob match", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"envs/*.yaml"}})
		if err := os.Symlink(outside, filepath.Join(in.RootDir, "app", "envs", "zz.yaml")); err != nil {
			t.Fatal(err)
		}
		assertContains(t, generateErr(t, r, in).Error(), `glob pattern "envs/*.yaml": glob match "app/envs/zz.yaml" resolved to outside repository root`)
	})
	t.Run("in-repo symlink allowed", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"link.yaml"}})
		if err := os.Symlink("values-a.yaml", filepath.Join(in.RootDir, "app", "link.yaml")); err != nil {
			t.Fatal(err)
		}
		assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["key"].(string), "values-a")
	})
}

func TestHelmRemoteValueFile(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("key: remote\n"))
	}))
	t.Cleanup(srv.Close)
	r := newTestRenderer(t, withHelm(t))
	in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{srv.URL + "/values.yaml", "missing-but-remote-not-checked.yaml"}, IgnoreMissingValueFiles: true})
	in.HelmOptions.ValuesFileSchemes = []string{"HTTP"} // case-insensitive
	res := generate(t, r, in)
	assertEqual(t, cmData(t, res, "test-app-cm")["key"].(string), "remote")
	assertSlice(t, valuesArgs(res.Commands[0]), []string{srv.URL + "/values.yaml"})
	if hits.Load() == 0 {
		t.Fatal("remote values file was not fetched")
	}
}

func TestHelmValuePrecedence(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	obj := &runtime.RawExtension{Raw: []byte(`{"key":"values-object","nested":{"inner":"from-object"},"list":["a",{"b":1}]}`)}
	base := v1alpha1.ApplicationSourceHelm{
		ValueFiles:   []string{"values-a.yaml"},
		Values:       "key: inline-values\nfromB: inline-values\n",
		ValuesObject: obj,
		Parameters:   []v1alpha1.HelmParameter{{Name: "key", Value: "param"}},
	}
	h := base
	res := generate(t, r, helmInput(t, &h))
	data := cmData(t, res, "test-app-cm")
	assertEqual(t, data["key"].(string), "param")
	assertEqual(t, data["nested"].(string), "from-object")
	assertEqual(t, data["fromB"].(string), "chart-default") // values ignored when valuesObject is set
	assertEqual(t, data["list"].(string), `["a",{"b":1}]`)
	assertContains(t, res.Commands[0], "--values ./app/values-a.yaml --values <temp file with values from source.helm.values/valuesObject>")

	h = base
	h.Parameters = nil
	assertEqual(t, cmData(t, generate(t, r, helmInput(t, &h)), "test-app-cm")["key"].(string), "values-object")

	h.ValuesObject = nil
	data = cmData(t, generate(t, r, helmInput(t, &h)), "test-app-cm")
	assertEqual(t, data["key"].(string), "inline-values")
	assertEqual(t, data["fromB"].(string), "inline-values")
	assertEqual(t, data["fromA"].(string), "values-a")

	h.Values = ""
	assertEqual(t, cmData(t, generate(t, r, helmInput(t, &h)), "test-app-cm")["key"].(string), "values-a")

	// Values are not env-substituted.
	h.Values = "key: $ARGOCD_APP_NAME\n"
	assertEqual(t, cmData(t, generate(t, r, helmInput(t, &h)), "test-app-cm")["key"].(string), "$ARGOCD_APP_NAME")
}

func TestHelmParameters(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	res := generate(t, r, helmInput(t, &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{
		{Name: "flag", Value: "false", ForceString: true},
		{Name: "key", Value: "a,b"},
		{Name: "list", Value: "{x,y}"},
		{Name: "fromA", Value: "first"},
		{Name: "fromA", Value: "last"},
		{Name: "fromB", Value: "test-value-${ARGOCD_APP_NAME}-$PATROL_APP_REVISION_SHORT"},
		{Name: "nested.inner", Value: "$$literal"},
	}}))
	data := cmData(t, res, "test-app-cm")
	assertEqual(t, data["flag"].(string), "false")
	assertEqual(t, data["flagKind"].(string), "string")
	assertEqual(t, data["key"].(string), "a,b")
	assertEqual(t, data["list"].(string), `["x","y"]`)
	assertEqual(t, data["fromA"].(string), "last")
	assertEqual(t, data["fromB"].(string), "test-value-test-app-0123456")
	assertEqual(t, data["nested"].(string), "$literal")
	assertContains(t, res.Commands[0], `--set key=a\,b`)

	res = generate(t, r, helmInput(t, &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "flag", Value: "false"}}}))
	assertEqual(t, cmData(t, res, "test-app-cm")["flagKind"].(string), "bool")
	assertContains(t, res.Commands[0], "--set flag=false")
}

func TestHelmFileParameters(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	for _, p := range []string{"files/data.txt", "/app/files/data.txt", "files/${ARGOCD_APP_PROJECT_NAME}.txt"} {
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{FileParameters: []v1alpha1.HelmFileParameter{{Name: "fileContent", Path: p}}})
		in.ProjectName = "data"
		res := generate(t, r, in)
		assertEqual(t, cmData(t, res, "test-app-cm")["fileContent"].(string), "file-parameter-content")
		assertContains(t, res.Commands[0], "--set-file fileContent=./app/files/data.txt")
	}
	in := helmInput(t, &v1alpha1.ApplicationSourceHelm{FileParameters: []v1alpha1.HelmFileParameter{{Name: "fileContent", Path: "../../../etc/passwd"}}})
	assertContains(t, generateErr(t, r, in).Error(), "error resolving helm value file path: file '../../../etc/passwd' resolved to outside repository root")
}

func TestHelmFlags(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))

	res := generate(t, r, helmInput(t, &v1alpha1.ApplicationSourceHelm{SkipCrds: true, SkipTests: true}))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/test-app-cm"})
	assertSlice(t, res.Commands, []string{"helm template . --name-template test-app --namespace test-ns --skip-tests"})

	for _, v := range []string{"v3", "v4"} {
		res := generate(t, r, helmInput(t, &v1alpha1.ApplicationSourceHelm{Version: v}))
		assertSlice(t, names(res.Manifests), []string{"CustomResourceDefinition/widgets.example.com", "ConfigMap/test-app-cm", "Pod/test-app-test"})
	}
	err := generateErr(t, r, helmInput(t, &v1alpha1.ApplicationSourceHelm{Version: "v2"}))
	assertEqual(t, err.Error(), "error initializing helm app object: failed to create new helm command: helm version 'v2' is not supported")

	schema := helmInput(t, &v1alpha1.ApplicationSourceHelm{})
	schema.Source.Path = "schema"
	err = generateErr(t, r, schema)
	assertContains(t, err.Error(), "failed to execute helm template command")
	assertContains(t, err.Error(), "replicas")
	schema.Source.Helm.SkipSchemaValidation = true
	res = generate(t, r, schema)
	assertEqual(t, cmData(t, res, "schema-cm")["replicas"].(string), "not-a-number")
	assertContains(t, res.Commands[0], "--skip-schema-validation")
}

func TestHelmKubeVersionAndAPIVersions(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))

	in := helmInput(t, nil)
	in.KubeVersion = "v1.30.11+IKS"
	in.APIVersions = []string{"example.com/v1", "apps/v1/Deployment"}
	res := generate(t, r, in)
	data := cmData(t, res, "test-app-cm")
	assertEqual(t, data["kubeVersion"].(string), "v1.30.11")
	assertEqual(t, data["hasWidgetAPI"].(string), "true")
	assertSlice(t, res.Commands, []string{"helm template . --name-template test-app --namespace test-ns --kube-version 1.30.11 --api-versions example.com/v1 --api-versions apps/v1/Deployment --include-crds"})

	// Source values replace (do not merge with) the cluster values.
	in.Source.Helm = &v1alpha1.ApplicationSourceHelm{KubeVersion: "1.16.0", APIVersions: []string{"v1"}}
	data = cmData(t, generate(t, r, in), "test-app-cm")
	assertEqual(t, data["kubeVersion"].(string), "v1.16.0")
	assertEqual(t, data["hasWidgetAPI"].(string), "false")

	in.Source.Helm = &v1alpha1.ApplicationSourceHelm{KubeVersion: "abc"}
	assertContains(t, generateErr(t, r, in).Error(), "could not parse kubernetes version abc")
}

func TestHelmRefSources(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	refRoot := tempRepo(t, map[string]string{
		"env/values.yaml":    "key: from-ref\n",
		"env/prod/x.yaml":    "envName: ref-glob\n",
		"files/content.txt":  "ref-file-content",
		"nested/values.yaml": "fromA: ref-$ARGOCD_APP_NAME\n",
	})
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("key: outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(refRoot, "env", "escape.yaml")); err != nil {
		t.Fatal(err)
	}

	in := helmInput(t, &v1alpha1.ApplicationSourceHelm{
		ValueFiles:     []string{"values-a.yaml", "$values/env/values.yaml", "$values/env/*/x.yaml"},
		FileParameters: []v1alpha1.HelmFileParameter{{Name: "fileContent", Path: "$values/files/content.txt"}},
	})
	in.HasMultipleSources = true
	in.RefSources = map[string]string{"$values": refRoot}
	res := generate(t, r, in)
	data := cmData(t, res, "test-app-cm")
	assertEqual(t, data["key"].(string), "from-ref")
	assertEqual(t, data["envName"].(string), "ref-glob")
	assertEqual(t, data["fileContent"].(string), "ref-file-content")
	assertEqual(t, data["fromA"].(string), "values-a")
	if strings.Contains(res.Commands[0], refRoot) || strings.Contains(res.Commands[0], in.RootDir) {
		t.Fatalf("command leaks checkout paths: %s", res.Commands[0])
	}
	assertContains(t, res.Commands[0], "--values ./env/values.yaml --values ./env/prod/x.yaml")

	// Ref keys may be given without "$".
	in.RefSources = map[string]string{"values": refRoot}
	assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["key"].(string), "from-ref")

	errCases := []struct {
		name  string
		files []string
		refs  map[string]string
		multi bool
		want  string
	}{
		{name: "no ref sources", files: []string{"$values/env/values.yaml"}, multi: true,
			want: `source referenced "$values", but no source has a 'ref' field defined`},
		{name: "unknown ref", files: []string{"$other/env/values.yaml"}, refs: map[string]string{"$values": refRoot, "b": refRoot}, multi: true,
			want: `source referenced "$other", which is not one of the available sources ($b, $values)`},
		{name: "traversal in ref", files: []string{"$values/../../etc/passwd"}, refs: map[string]string{"$values": refRoot}, multi: true,
			want: "resolved to outside repository root"},
		{name: "symlink escape in ref", files: []string{"$values/env/escape.yaml"}, refs: map[string]string{"$values": refRoot}, multi: true,
			want: "resolved to outside repository root"},
		{name: "missing ref file", files: []string{"$values/env/missing.yaml"}, refs: map[string]string{"$values": refRoot}, multi: true,
			want: "failed to execute helm template command"},
	}
	for _, tt := range errCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: tt.files})
			in.HasMultipleSources = tt.multi
			in.RefSources = tt.refs
			assertContains(t, generateErr(t, r, in).Error(), tt.want)
		})
	}

	t.Run("missing ref file ignored", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/env/missing.yaml"}, IgnoreMissingValueFiles: true})
		in.HasMultipleSources = true
		in.RefSources = map[string]string{"$values": refRoot}
		assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["key"].(string), "chart-default")
	})

	t.Run("single source treats $ref as env reference", func(t *testing.T) {
		t.Parallel()
		// "$values" expands to "" so the path is repository-root relative.
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/app/values-b.yaml"}})
		assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["key"].(string), "values-b")
	})
}

func TestHelmDependencyBuild(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	root := copyFixture(t, "helm")
	in := Input{RootDir: root, AppName: "deps", Namespace: "ns", Source: v1alpha1.ApplicationSource{Path: "deps/parent"}}
	res := generate(t, r, in)
	assertSlice(t, sorted(names(res.Manifests)), []string{"ConfigMap/parent-cm", "ConfigMap/sub-cm"})
	if _, err := os.Stat(filepath.Join(root, "deps", "parent", "charts")); err != nil {
		t.Fatalf("dependencies not vendored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deps", "parent", helm.DependencyMarkerFile)); err != nil {
		t.Fatalf("dependency marker not written: %v", err)
	}
	// The second render reuses the vendored dependencies (the marker short-circuits
	// the build even when the dependency source disappears).
	if err := os.RemoveAll(filepath.Join(root, "deps", "sub")); err != nil {
		t.Fatal(err)
	}
	res = generate(t, r, in)
	assertSlice(t, sorted(names(res.Manifests)), []string{"ConfigMap/parent-cm", "ConfigMap/sub-cm"})
}

func TestHelmDependencyNotPermitted(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	// An HTTPS repository whose certificate helm does not trust: unreachable. (Plain
	// http:// dependency repositories are never registered, as upstream.)
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	depRepo := srv.URL + "/charts"
	root := tempRepo(t, map[string]string{
		"chart/Chart.yaml":        fmt.Sprintf("apiVersion: v2\nname: c\nversion: 0.1.0\ndependencies:\n- name: missing\n  version: 1.0.0\n  repository: %s\n", depRepo),
		"chart/templates/cm.yaml": cm("c", nil),
	})
	in := Input{RootDir: root, AppName: "c", ProjectName: "my-project", Source: v1alpha1.ApplicationSource{Path: "chart"},
		ProjectSourceRepos: []string{"https://git.example.com/*"}}
	err := generateErr(t, r, in)
	assertEqual(t, err.Error(), fmt.Sprintf("helm repos %s are not permitted in project 'my-project'", depRepo))
	assertEqual(t, ErrorCode(err), apiclient.CodePermissionDenied)

	in.ProjectSourceRepos = []string{"*"}
	err = generateErr(t, r, in)
	assertContains(t, err.Error(), "error building helm chart dependencies")
	assertEqual(t, ErrorCode(err), "")
}

// packageChart builds a chart archive (<name>/... entries) from dir.
func packageChart(t testing.TB, dir, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chartRepo serves testdata/helm/app as chart "app" (versions 0.1.0 and 0.2.0-rc.1)
// from an index-based repository.
func chartRepo(t testing.TB) *httptest.Server {
	t.Helper()
	tgz := packageChart(t, filepath.Join("testdata", "helm", "app"), "app")
	sum := sha256.Sum256(tgz)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  app:
  - name: app
    version: 0.2.0-rc.1
    urls: [%[1]s/app-0.2.0-rc.1.tgz]
    digest: %[2]s
  - name: app
    version: 0.1.0
    urls: [%[1]s/app-0.1.0.tgz]
    digest: %[2]s
`, srv.URL, hex.EncodeToString(sum[:]))
		case "/app-0.1.0.tgz", "/app-0.2.0-rc.1.tgz":
			_, _ = w.Write(tgz)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHelmChartFromRepository(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	srv := chartRepo(t)
	in := Input{
		AppName:   "chart-app",
		Namespace: "ns",
		Repo:      apiclient.Repository{URL: srv.URL, Type: v1alpha1.RepositoryTypeHelm},
		Source: v1alpha1.ApplicationSource{RepoURL: srv.URL, Chart: "app", TargetRevision: "0.1.*", Path: "ignored", Helm: &v1alpha1.ApplicationSourceHelm{
			ValueFiles: []string{"values-a.yaml"},
			Parameters: []v1alpha1.HelmParameter{{Name: "envName", Value: "rev-$ARGOCD_APP_REVISION"}},
		}},
	}
	res := generate(t, r, in)
	assertEqual(t, res.SourceType, "Helm")
	data := cmData(t, res, "chart-app-cm")
	assertEqual(t, data["key"].(string), "values-a")
	assertEqual(t, data["envName"].(string), "rev-0.1.0")
	assertSlice(t, res.Commands, []string{"helm template . --name-template chart-app --namespace ns --set envName=rev-0.1.0 --values ./values-a.yaml --include-crds"})

	// A value file outside the extracted chart is rejected.
	in.Source.Helm.ValueFiles = []string{"../../outside.yaml"}
	assertContains(t, generateErr(t, r, in).Error(), "resolved to outside repository root")

	// Chart sources combine with `$ref` value files from a git source.
	refRoot := tempRepo(t, map[string]string{"values.yaml": "key: external\n"})
	in.Source.Helm.ValueFiles = []string{"$values/values.yaml"}
	in.HasMultipleSources = true
	in.RefSources = map[string]string{"$values": refRoot}
	assertEqual(t, cmData(t, generate(t, r, in), "chart-app-cm")["key"].(string), "external")

	// Unknown chart.
	in.Source.Chart = "nope"
	assertContains(t, generateErr(t, r, in).Error(), "chart 'nope' not found in index")
}

func TestHelmChartFromOCIRegistry(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	reg := ocitest.New(t, ocitest.Options{Username: "u", Password: "p"})
	reg.PushHelmChart("charts/app", "0.1.0", packageChart(t, filepath.Join("testdata", "helm", "app"), "app"))
	repoURL := reg.Host() + "/charts"
	in := Input{
		AppName: "oci-app",
		Repo:    apiclient.Repository{URL: repoURL, Type: v1alpha1.RepositoryTypeHelm, EnableOCI: true, InsecureOCIForceHTTP: true, Username: "u", Password: "p"},
		Source:  v1alpha1.ApplicationSource{RepoURL: repoURL, Chart: "app", TargetRevision: "0.1.0"},
	}
	res := generate(t, r, in)
	findObj(t, res.Manifests, "ConfigMap", "oci-app-cm")

	in.Repo.Password = "wrong"
	if err := generateErr(t, r, in); !strings.Contains(err.Error(), "error fetching chart") {
		t.Fatalf("error = %v", err)
	}
}

func TestHelmNotConfigured(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	err := generateErr(t, r, helmInput(t, nil))
	if !errors.Is(err, ErrHelmNotConfigured) {
		t.Fatalf("error = %v", err)
	}
	err = generateErr(t, r, Input{Source: v1alpha1.ApplicationSource{RepoURL: "https://charts.example.com", Chart: "x", TargetRevision: "1.0.0"}})
	if !errors.Is(err, ErrHelmNotConfigured) {
		t.Fatalf("error = %v", err)
	}
}

func TestHelmCanceledContext(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withHelm(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Generate(ctx, helmInput(t, nil)); err == nil {
		t.Fatal("want error for canceled context")
	}
}

// assertTreeUnchanged fails when fn changes the files (names, modes, contents) under root.
func assertTreeUnchanged(t testing.TB, root string, fn func()) {
	t.Helper()
	before := snapshotTree(t, root)
	fn()
	after := snapshotTree(t, root)
	if before != after {
		t.Fatalf("tree changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func snapshotTree(t testing.TB, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&b, "%s %v", rel, info.Mode())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			fmt.Fprintf(&b, " %x", sum[:8])
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestHelmDependencyRepos(t *testing.T) {
	t.Parallel()
	chart := `apiVersion: v2
name: deps
version: 0.1.0
dependencies:
- {name: a, version: 1.0.0, repository: "@stable"}
- {name: b, version: 1.0.0, repository: "alias:incubator"}
- {name: c, version: 1.0.0, repository: "https://charts.example.com/exact"}
- {name: d, version: 1.0.0, repository: "https://templated.example.com/charts"}
- {name: e, version: 1.0.0, repository: "oci://registry.example.com/team/charts"}
- {name: f, version: 1.0.0, repository: "oci://other.example.com/charts"}
- {name: g, version: 1.0.0, repository: "oci://ocitemplate.example.com/charts"}
- {name: h, version: 1.0.0, repository: "https://anonymous.example.com/charts"}
- {name: i, version: 1.0.0, repository: "http://plain.example.com/charts"}
- {name: j, version: 1.0.0, repository: "file://../local"}
`
	root := tempRepo(t, map[string]string{"chart/Chart.yaml": chart})
	r := newTestRenderer(t)
	c := &call{
		root:    root,
		appPath: filepath.Join(root, "chart"),
		in: Input{
			Repos: []apiclient.Repository{
				{Name: "stable", URL: "https://stable.example.com", Username: "s-user", Password: "s-pass"},
				{Name: "incubator", URL: "https://incubator.example.com", Password: "i-pass"},
				{URL: "https://charts.example.com/exact", Username: "exact", Password: "exact-pass", Insecure: true},
				{URL: "oci://registry.example.com/team/charts", Type: v1alpha1.RepositoryTypeOCI, Username: "oci-user", Password: "oci-pass"},
				{URL: "other.example.com", EnableOCI: true, Username: "prefix-user", Password: "prefix-pass", InsecureOCIForceHTTP: true},
			},
			HelmRepoCreds: []apiclient.RepositoryCredentialTemplate{
				{URLPrefix: "https://templated.example.com", Access: apiclient.Repository{Username: "tmpl", Password: "tmpl-pass"}},
				{URLPrefix: "oci://ocitemplate.example.com", Access: apiclient.Repository{Type: v1alpha1.RepositoryTypeOCI, Username: "ot", Password: "ot-pass", InsecureOCIForceHTTP: true}},
			},
		},
	}
	repos, err := r.helmDependencyRepos(c, true)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name, url, user, pass string
		oci, plain, insecure  bool
	}
	var got []row
	for _, rc := range repos {
		got = append(got, row{rc.Name, rc.URL, rc.Creds.Username, rc.Creds.Password, rc.EnableOCI, rc.Creds.PlainHTTP, rc.Creds.InsecureSkipVerify})
		if !rc.Creds.PassCredentials {
			t.Errorf("%s: passCredentials not propagated", rc.Name)
		}
	}
	want := []row{
		{name: "stable", url: "https://stable.example.com", user: "s-user", pass: "s-pass"},
		{name: "incubator", url: "https://incubator.example.com", pass: "i-pass"},
		{url: "https://charts.example.com/exact", user: "exact", pass: "exact-pass", insecure: true},
		{name: "https:--templated.example.com-charts", url: "https://templated.example.com/charts", user: "tmpl", pass: "tmpl-pass"},
		{url: "registry.example.com/team/charts", user: "oci-user", pass: "oci-pass", oci: true},
		{name: "oci:--other.example.com-charts", url: "other.example.com/charts", user: "prefix-user", pass: "prefix-pass", oci: true, plain: true},
		{name: "oci:--ocitemplate.example.com-charts", url: "ocitemplate.example.com/charts", user: "ot", pass: "ot-pass", oci: true, plain: true},
		{name: "https:--anonymous.example.com-charts", url: "https://anonymous.example.com/charts"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d repos: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repo %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}
