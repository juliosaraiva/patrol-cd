package render

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func jsonnetInput(t testing.TB, opts v1alpha1.ApplicationSourceJsonnet) Input {
	t.Helper()
	return Input{
		RootDir:   copyFixture(t, "jsonnet"),
		AppName:   "test-app",
		Namespace: "test-ns",
		Source:    v1alpha1.ApplicationSource{Path: "app", Directory: &v1alpha1.ApplicationSourceDirectory{Jsonnet: opts}},
	}
}

var jsonnetOpts = v1alpha1.ApplicationSourceJsonnet{
	ExtVars: []v1alpha1.JsonnetVar{{Name: "app", Value: "${ARGOCD_APP_NAME}"}, {Name: "code", Value: "{a: 1}", Code: true}},
	TLAs:    []v1alpha1.JsonnetVar{{Name: "ns", Value: "$ARGOCD_APP_NAMESPACE"}, {Name: "replicas", Value: "3", Code: true}},
	Libs:    []string{"vendor"},
}

func TestJsonnetDirectory(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	res := generate(t, r, jsonnetInput(t, jsonnetOpts))
	assertEqual(t, res.SourceType, "Directory")
	assertSlice(t, names(res.Manifests), []string{
		"ConfigMap/arr-test-app", "ConfigMap/arr2", // array output, in order
		"ConfigMap/l1", "ConfigMap/l2", // List flattened
		"ConfigMap/obj", // single object
		"ConfigMap/plain",
	})
	obj := findObj(t, res.Manifests, "ConfigMap", "obj")
	assertEqual(t, obj.GetNamespace(), "test-ns")
	data := nested(t, obj, "data").(map[string]any)
	assertEqual(t, data["replicas"].(string), "3") // code TLA typed as a number
	assertEqual(t, data["code"].(string), "1")
	assertEqual(t, data["color"].(string), "blue")

	// A leading "/" in libs is the repository root as well.
	opts := jsonnetOpts
	opts.Libs = []string{"/vendor"}
	assertEqual(t, len(generate(t, r, jsonnetInput(t, opts)).Manifests), 6)
}

func TestJsonnetDisabled(t *testing.T) {
	t.Parallel()
	in := jsonnetInput(t, v1alpha1.ApplicationSourceJsonnet{})
	in.EnabledSourceTypes = map[string]bool{"Directory": false}
	res := generate(t, newTestRenderer(t), in)
	assertEqual(t, res.SourceType, "Directory")
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/plain"})
}

func TestJsonnetErrors(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	tests := []struct {
		name  string
		file  string
		code  string
		opts  v1alpha1.ApplicationSourceJsonnet
		want  string
		ecode string
	}{
		{name: "syntax error", file: "bad.jsonnet", code: "{", opts: jsonnetOpts,
			want: `Failed to evaluate jsonnet "bad.jsonnet": `, ecode: apiclient.CodeInvalidArgument},
		{name: "scalar output", file: "scalar.jsonnet", code: "'just a string'", opts: jsonnetOpts,
			want: `Failed to unmarshal generated json "scalar.jsonnet": `, ecode: apiclient.CodeInvalidArgument},
		{name: "missing ext var", file: "zz.jsonnet", code: "[]", opts: v1alpha1.ApplicationSourceJsonnet{Libs: []string{"vendor"}},
			want: `Failed to evaluate jsonnet "array.jsonnet": RUNTIME ERROR: Undefined external variable: app`, ecode: apiclient.CodeInvalidArgument},
		{name: "lib outside repository", file: "zz.jsonnet", code: "[]", opts: v1alpha1.ApplicationSourceJsonnet{Libs: []string{"../../etc"}},
			want: "file '../../etc' resolved to outside repository root"},
		{name: "import outside repository", file: "evil.jsonnet", code: "importstr '/etc/hosts'", opts: jsonnetOpts,
			want: `Failed to evaluate jsonnet "evil.jsonnet": RUNTIME ERROR: jsonnet import "/etc/hosts" resolves outside repository root`, ecode: apiclient.CodeInvalidArgument},
		{name: "object without kind", file: "nokind.jsonnet", code: "{apiVersion: 'v1', metadata: {name: 'x'}}", opts: jsonnetOpts,
			want: `Failed to unmarshal generated json "nokind.jsonnet"`, ecode: apiclient.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := jsonnetInput(t, tt.opts)
			if err := os.WriteFile(filepath.Join(in.RootDir, "app", tt.file), []byte(tt.code), 0o644); err != nil {
				t.Fatal(err)
			}
			err := generateErr(t, r, in)
			assertContains(t, err.Error(), tt.want)
			assertEqual(t, ErrorCode(err), tt.ecode)
		})
	}
}

func TestJsonnetSkipMarkerAndInclude(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	in := jsonnetInput(t, jsonnetOpts)
	if err := os.WriteFile(filepath.Join(in.RootDir, "app", "skipped.jsonnet"), []byte("// +argocd:skip-file-rendering\n{"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(generate(t, r, in).Manifests), 6)

	// include/exclude apply to Jsonnet files.
	in.Source.Directory.Include = "*.jsonnet"
	in.Source.Directory.Exclude = "list.jsonnet"
	assertSlice(t, names(generate(t, r, in).Manifests), []string{"ConfigMap/arr-test-app", "ConfigMap/arr2", "ConfigMap/obj"})
}
