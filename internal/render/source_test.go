package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func TestExplicitType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  v1alpha1.ApplicationSource
		want string
		err  string
	}{
		{name: "none", src: v1alpha1.ApplicationSource{}},
		{name: "empty directory block", src: v1alpha1.ApplicationSource{Directory: &v1alpha1.ApplicationSourceDirectory{}}, want: "Directory"},
		{name: "empty plugin block", src: v1alpha1.ApplicationSource{Plugin: &v1alpha1.ApplicationSourcePlugin{}}, want: "Plugin"},
		{name: "helm", src: v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{}}, want: "Helm"},
		{name: "kustomize", src: v1alpha1.ApplicationSource{Kustomize: &v1alpha1.ApplicationSourceKustomize{}}, want: "Kustomize"},
		{name: "two", src: v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{}, Kustomize: &v1alpha1.ApplicationSourceKustomize{}},
			err: "multiple application sources defined: Kustomize,Helm"},
		{name: "all", src: v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{}, Kustomize: &v1alpha1.ApplicationSourceKustomize{},
			Directory: &v1alpha1.ApplicationSourceDirectory{}, Plugin: &v1alpha1.ApplicationSourcePlugin{}},
			err: "multiple application sources defined: Kustomize,Helm,Directory,Plugin"},
	}
	for _, tt := range tests {
		got, err := ExplicitType(&tt.src)
		switch {
		case tt.err != "":
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: error = %v, want %q", tt.name, err, tt.err)
			}
		case err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want == "" && got != nil:
			t.Errorf("%s: got %v, want nil", tt.name, *got)
		case tt.want != "" && (got == nil || string(*got) != tt.want):
			t.Errorf("%s: got %v, want %s", tt.name, got, tt.want)
		}
	}
}

func TestDetectBuiltinType(t *testing.T) {
	t.Parallel()
	chart := "apiVersion: v2\nname: c\nversion: 0.1.0\n"
	tests := []struct {
		name    string
		files   map[string]string
		enabled map[string]bool
		want    string
	}{
		{"chart", map[string]string{"Chart.yaml": chart}, nil, "Helm"},
		{"suffix match", map[string]string{"MyChart.yaml": chart}, nil, "Helm"},
		{"kustomization.yaml", map[string]string{"kustomization.yaml": ""}, nil, "Kustomize"},
		{"kustomization.yml", map[string]string{"kustomization.yml": ""}, nil, "Kustomize"},
		{"Kustomization", map[string]string{"Kustomization": ""}, nil, "Kustomize"},
		{"both: kustomize wins", map[string]string{"Chart.yaml": chart, "kustomization.yaml": ""}, nil, "Kustomize"},
		{"wrong case", map[string]string{"KUSTOMIZATION.yaml": ""}, nil, "Directory"},
		{"chart only in subdirectory", map[string]string{"sub/Chart.yaml": chart, "a.yaml": ""}, nil, "Directory"},
		{"directory named Chart.yaml", map[string]string{"Chart.yaml/x": ""}, nil, "Directory"},
		{"plain", map[string]string{"a.yaml": ""}, nil, "Directory"},
		{"helm disabled", map[string]string{"Chart.yaml": chart}, map[string]bool{"Helm": false}, "Directory"},
		{"helm disabled, kustomize present", map[string]string{"Chart.yaml": chart, "kustomization.yaml": ""}, map[string]bool{"Helm": false}, "Kustomize"},
		{"kustomize disabled, chart present", map[string]string{"Chart.yaml": chart, "kustomization.yaml": ""}, map[string]bool{"Kustomize": false}, "Helm"},
		{"explicitly enabled", map[string]string{"Chart.yaml": chart}, map[string]bool{"Helm": true}, "Helm"},
	}
	for _, tt := range tests {
		root := tempRepo(t, tt.files)
		got, err := detectBuiltinType(root, tt.enabled)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if string(got) != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestGenerateExplicitTypes(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t) // no Helm client: proves Helm is not used
	chartRepo := map[string]string{
		"Chart.yaml":           "apiVersion: v2\nname: c\nversion: 0.1.0\n",
		"values.yaml":          "a: b\n",
		"templates/cm.yaml":    cm("{{ .Release.Name }}", nil),
		"kustomization.yaml":   "resources: []\n",
		"plain-manifests.yaml": cm("plain", nil),
	}
	root := tempRepo(t, chartRepo)

	// directory: {} on a chart+kustomize directory renders it as plain YAML.
	res := generate(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Directory: &v1alpha1.ApplicationSourceDirectory{}}})
	assertEqual(t, res.SourceType, "Directory")
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/plain"})

	// An explicit Helm source with Helm disabled is rendered as Directory.
	res = generate(t, r, Input{RootDir: root, EnabledSourceTypes: map[string]bool{"Helm": false},
		Source: v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{ReleaseName: "x"}}})
	assertEqual(t, res.SourceType, "Directory")

	// Implicit detection with both tools disabled: Directory.
	res = generate(t, r, Input{RootDir: root, EnabledSourceTypes: map[string]bool{"Helm": false, "Kustomize": false}})
	assertEqual(t, res.SourceType, "Directory")

	// Plugin is never downgraded, even with every built-in disabled.
	err := generateErr(t, r, Input{RootDir: root, AppName: "app", EnabledSourceTypes: map[string]bool{"Helm": false, "Kustomize": false, "Directory": false, "Plugin": false},
		Source: v1alpha1.ApplicationSource{Plugin: &v1alpha1.ApplicationSourcePlugin{}}})
	assertContains(t, err.Error(), `CMP processing failed for application "app"`)

	err = generateErr(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{
		Helm: &v1alpha1.ApplicationSourceHelm{}, Kustomize: &v1alpha1.ApplicationSourceKustomize{}}})
	assertEqual(t, err.Error(), "error getting app source type: multiple application sources defined: Kustomize,Helm")
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	chart := "apiVersion: v2\nname: c\nversion: 0.1.0\n"
	root := tempRepo(t, map[string]string{
		"apps/chart/Chart.yaml":            chart,
		"apps/kust/kustomization.yaml":     "",
		"apps/both/Chart.yaml":             chart,
		"apps/both/kustomization.yml":      "",
		"apps/plain/a.yaml":                "",
		"apps/chart/charts/sub/Chart.yaml": chart,
		"apps/linked":                      "->kust",
		"Kustomization":                    "",
	})
	r := newTestRenderer(t)
	apps, err := r.Discover(context.Background(), root, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		".":                     "Kustomize",
		"apps/chart":            "Helm",
		"apps/chart/charts/sub": "Helm",
		"apps/kust":             "Kustomize",
		"apps/both":             "Kustomize",
	}
	if len(apps) != len(want) {
		t.Fatalf("apps = %v, want %v", apps, want)
	}
	for k, v := range want {
		if apps[k] != v {
			t.Errorf("apps[%q] = %q, want %q", k, apps[k], v)
		}
	}
	apps, err = r.Discover(context.Background(), root, root, map[string]bool{"Kustomize": false})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := apps["apps/kust"]; ok || apps["apps/both"] != "Helm" {
		t.Fatalf("kustomize disabled: %v", apps)
	}
}

func TestAppPathResolution(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	root := tempRepo(t, map[string]string{
		"app/a.yaml": cm("a", nil),
		"root.yaml":  cm("root", nil),
		"file.yaml":  cm("file", nil),
		"escape":     "->" + outside,
		"inside":     "->app",
	})
	r := newTestRenderer(t)
	errs := map[string]string{
		"/abs":      "/abs: app path is absolute",
		"../x":      "../x: app path outside root",
		"a/../..":   "a/../..: app path outside root",
		"missing":   "missing: app path does not exist",
		"file.yaml": "file.yaml: app path is not a directory",
		"escape":    "escape: app path outside root",
	}
	for p, want := range errs {
		err := generateErr(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: p}})
		assertEqual(t, err.Error(), want)
	}
	for _, p := range []string{"", "."} {
		res := generate(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: p}})
		assertSlice(t, names(res.Manifests), []string{"ConfigMap/file", "ConfigMap/root"})
	}
	res := generate(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: "inside"}})
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a"})
	// AppPath overrides Source.Path.
	res = generate(t, r, Input{RootDir: root, AppPath: "app", Source: v1alpha1.ApplicationSource{Path: "missing"}})
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a"})

	err := generateErr(t, r, Input{RootDir: "relative/root"})
	assertContains(t, err.Error(), "must be absolute")
}

func TestOverrideFiles(t *testing.T) {
	t.Parallel()
	assertSlice(t, OverrideFiles(""), []string{".argocd-source.yaml", ".patrol-source.yaml"})
	assertSlice(t, OverrideFiles("ns_app"), []string{".argocd-source.yaml", ".patrol-source.yaml", ".argocd-source-ns_app.yaml", ".patrol-source-ns_app.yaml"})

	r := newTestRenderer(t)
	base := map[string]string{
		"app/a.yaml":       cm("a", nil),
		"app/b.yml":        cm("b", nil),
		"app/sub/c.yaml":   cm("c", nil),
		"elsewhere/x.yaml": cm("x", nil),
	}
	steps := []struct {
		file, content string
		want          []string
	}{
		{"", "", []string{"ConfigMap/a", "ConfigMap/b"}},
		{".argocd-source.yaml", "directory:\n  recurse: true\n", []string{"ConfigMap/a", "ConfigMap/b", "ConfigMap/c"}},
		{".patrol-source.yaml", "directory:\n  include: '*.yaml'\n", []string{"ConfigMap/a", "ConfigMap/c"}},
		{".argocd-source-team-a_test-app.yaml", "directory:\n  exclude: 'sub/*'\n", []string{"ConfigMap/a"}},
		{".patrol-source-team-a_test-app.yaml", "directory:\n  exclude: null\n  include: '{*.yaml,*.yml}'\n", []string{"ConfigMap/a", "ConfigMap/b", "ConfigMap/c"}},
	}
	root := tempRepo(t, base)
	for _, s := range steps {
		if s.file != "" {
			if err := os.WriteFile(filepath.Join(root, "app", s.file), []byte(s.content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		in := Input{RootDir: root, AppName: "team-a_test-app", Source: v1alpha1.ApplicationSource{Path: "app"}}
		assertSlice(t, names(generate(t, r, in).Manifests), s.want)
	}

	// path, repoURL, chart and targetRevision cannot be overridden.
	root = tempRepo(t, base)
	writeTree(t, root, map[string]string{"app/.argocd-source.yaml": "path: elsewhere\nrepoURL: https://evil.example.com\ntargetRevision: x\nchart: c\n"})
	in := Input{RootDir: root, AppName: "a", Source: v1alpha1.ApplicationSource{Path: "app", RepoURL: "https://git.example.com/r.git"}}
	res := generate(t, r, in)
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a", "ConfigMap/b"})
	if in.Source.Directory != nil {
		t.Fatal("Generate modified the caller's source")
	}

	// null removes a field set in the Application.
	root = tempRepo(t, base)
	writeTree(t, root, map[string]string{"app/.argocd-source.yaml": "directory:\n  recurse: null\n"})
	res = generate(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: "app", Directory: &v1alpha1.ApplicationSourceDirectory{Recurse: true}}})
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a", "ConfigMap/b"})

	// An empty file is an empty patch; a directory with the name is ignored.
	root = tempRepo(t, base)
	writeTree(t, root, map[string]string{"app/.argocd-source.yaml": "", "app/.patrol-source.yaml/x": ""})
	assertSlice(t, names(generate(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: "app"}}).Manifests), []string{"ConfigMap/a", "ConfigMap/b"})
}

func TestOverrideFileErrors(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	outside := filepath.Join(t.TempDir(), "o.yaml")
	if err := os.WriteFile(outside, []byte("directory: {recurse: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		files map[string]string
		src   v1alpha1.ApplicationSource
		lim   int64
		want  string
	}{
		{name: "malformed", files: map[string]string{"app/.argocd-source.yaml": "directory: [unterminated\n"},
			want: "error getting app source type: error while parsing source parameters: .argocd-source.yaml: "},
		{name: "wrong type", files: map[string]string{"app/.patrol-source.yaml": "directory: 5\n"},
			want: "error while parsing source parameters: .patrol-source.yaml: "},
		{name: "conflicting tool", files: map[string]string{"app/.argocd-source.yaml": "helm:\n  releaseName: x\n"},
			src:  v1alpha1.ApplicationSource{Directory: &v1alpha1.ApplicationSourceDirectory{}},
			want: "error getting app source type: multiple application sources defined: Helm,Directory"},
		{name: "symlink outside", files: map[string]string{"app/.argocd-source.yaml": "->" + outside},
			want: ".argocd-source.yaml: override file resolves outside the repository"},
		{name: "too large", files: map[string]string{"app/.argocd-source.yaml": "directory:\n  recurse: true\n"}, lim: 10,
			want: ".argocd-source.yaml: file exceeds 10 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"app/a.yaml": cm("a", nil)}
			for k, v := range tt.files {
				files[k] = v
			}
			in := Input{RootDir: tempRepo(t, files), Source: tt.src}
			in.Source.Path = "app"
			in.Limits.MaxOverrideFileBytes = tt.lim
			assertContains(t, generateErr(t, r, in).Error(), tt.want)
		})
	}
}

func TestBuildEnv(t *testing.T) {
	t.Parallel()
	in := Input{
		AppName: "ns_app", Namespace: "dest", ProjectName: "proj", Revision: testRevision,
		Repo:   apiclient.Repository{URL: "https://git.example.com/r.git"},
		Source: v1alpha1.ApplicationSource{Path: "apps/x", TargetRevision: "HEAD"},
	}
	env := newBuildEnv(in).Environ()
	want := []string{
		"ARGOCD_APP_NAME=ns_app",
		"ARGOCD_APP_NAMESPACE=dest",
		"ARGOCD_APP_PROJECT_NAME=proj",
		"ARGOCD_APP_REVISION=" + testRevision,
		"ARGOCD_APP_REVISION_SHORT=0123456",
		"ARGOCD_APP_REVISION_SHORT_8=01234567",
		"ARGOCD_APP_SOURCE_REPO_URL=https://git.example.com/r.git",
		"ARGOCD_APP_SOURCE_PATH=apps/x",
		"ARGOCD_APP_SOURCE_TARGET_REVISION=HEAD",
	}
	for i, w := range want {
		assertEqual(t, env[i], w)
		assertEqual(t, env[len(want)+i], "PATROL_"+strings.TrimPrefix(w, "ARGOCD_"))
	}
	assertEqual(t, len(env), 2*len(want))

	// Short revisions are not padded; an OCI digest keeps its upstream short form.
	short := newBuildEnv(Input{Revision: "abc"}).Environ()
	assertContains(t, strings.Join(short, "\n"), "ARGOCD_APP_REVISION_SHORT=abc\n")
	digest := newBuildEnv(Input{Revision: "sha256:0123456789"}).Environ()
	assertContains(t, strings.Join(digest, "\n"), "ARGOCD_APP_REVISION_SHORT=sha256:\n")
	// Empty values keep their names (upstream Env.Environ).
	assertContains(t, strings.Join(short, "\n"), "ARGOCD_APP_NAME=\n")

	e := newBuildEnv(in)
	tests := map[string]string{
		"${ARGOCD_APP_NAME}-$PATROL_APP_NAMESPACE": "ns_app-dest",
		"$$ARGOCD_APP_NAME":                        "$ARGOCD_APP_NAME",
		"${UNKNOWN}x":                              "x",
		"no vars":                                  "no vars",
		"$":                                        "$", // os.Expand keeps a lone trailing "$"
		"cost: $$5":                                "cost: $5",
	}
	for s, want := range tests {
		assertEqual(t, e.Envsubst(s), want)
	}

	sub := e.subset("APP_NAME", "APP_SOURCE_PATH").Environ()
	assertSlice(t, sub, []string{"ARGOCD_APP_NAME=ns_app", "ARGOCD_APP_SOURCE_PATH=apps/x", "PATROL_APP_NAME=ns_app", "PATROL_APP_SOURCE_PATH=apps/x"})
	assertSlice(t, newEnvFromList([]string{"A=1", "B", "=x", "C=a=b"}).Environ(), []string{"A=1", "C=a=b"})
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	assertEqual(t, redactURL("https://user:pass@example.com/r.git"), "https://user:xxxxx@example.com/r.git")
	assertEqual(t, redactURL("git@github.com:org/repo.git"), "git@github.com:org/repo.git")
}
