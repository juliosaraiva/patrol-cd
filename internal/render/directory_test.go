package render

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func dirInput(root string, dir *v1alpha1.ApplicationSourceDirectory) Input {
	return Input{RootDir: root, AppName: "test-app", Namespace: "default", Source: v1alpha1.ApplicationSource{Path: ".", Directory: dir}}
}

func TestDirectoryPlainManifests(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"a.yaml":     cm("a", nil) + "---\n---\n" + cm("a2", nil) + "\n---\n",
		"b.yml":      cm("b", nil),
		"c.json":     `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`,
		"d.txt":      cm("d", nil),
		"e.YAML":     cm("e", nil),
		"values.yml": "foo: bar\n",
	})
	r := newTestRenderer(t)
	res := generate(t, r, dirInput(root, nil))
	assertEqual(t, res.SourceType, "Directory")
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a", "ConfigMap/a2", "ConfigMap/b", "ConfigMap/c"})
	if len(res.Commands) != 0 {
		t.Fatalf("commands = %v, want none", res.Commands)
	}
}

func utf16File(s string, order binary.ByteOrder) string {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(units))
	if order == binary.LittleEndian {
		b[0], b[1] = 0xFF, 0xFE
	} else {
		b[0], b[1] = 0xFE, 0xFF
	}
	for i, u := range units {
		order.PutUint16(b[2+2*i:], u)
	}
	return string(b)
}

func TestDirectoryUTF16AndBOM(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"le.yaml":  utf16File(cm("le", map[string]string{"k": "ünïcode"}), binary.LittleEndian),
		"be.yaml":  utf16File(cm("be", nil), binary.BigEndian),
		"bom.yaml": "\xEF\xBB\xBF" + cm("bom", nil),
	})
	res := generate(t, newTestRenderer(t), dirInput(root, nil))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/be", "ConfigMap/bom", "ConfigMap/le"})
	assertEqual(t, nested(t, findObj(t, res.Manifests, "ConfigMap", "le"), "data", "k").(string), "ünïcode")
}

func TestDirectoryParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, file, content, want string
	}{
		{"json multiple objects", "x.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}} {"kind":"x"}`, `Found multiple objects in "x.json". Only single objects are allowed in JSON files.`},
		{"json invalid", "x.json", `{"apiVersion": `, `Failed to unmarshal "x.json"`},
		{"malformed manifest", "x.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: [a\n", `Failed to unmarshal "x.yaml"`},
		{"valid then malformed", "x.yaml", cm("ok", nil) + "---\n{{ bad\n", `Failed to unmarshal "x.yaml"`},
		{"json array", "x.json", `[1,2]`, `Failed to unmarshal "x.json"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t, map[string]string{tt.file: tt.content})
			err := generateErr(t, newTestRenderer(t), dirInput(root, nil))
			assertContains(t, err.Error(), tt.want)
			assertEqual(t, ErrorCode(err), apiclient.CodeInvalidArgument)
		})
	}
}

func TestDirectoryIgnoresUnrelatedFiles(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"values.yaml":   "foo: bar\nlist: [1, 2]\n",
		"broken.yaml":   "kind: Thing\nfoo: [unterminated\n", // no apiVersion/metadata: ignored
		"package.json":  `{"name":"x","version":"1.0.0"}`,
		"template.yaml": "{{ .Values.x }}\n",
		"cm.yaml":       cm("only", nil),
	})
	res := generate(t, newTestRenderer(t), dirInput(root, nil))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/only"})
}

func TestDirectoryRecurse(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"a.yaml":              cm("a", nil),
		"b.yaml":              cm("b", nil),
		"sub/c.yaml":          cm("c", nil),
		".hidden/d.yaml":      cm("d", nil),
		"elsewhere/e.yaml":    cm("e", nil),
		"app/x.yaml":          cm("x", nil),
		"app/linked":          "->../elsewhere",
		"app/nested/yy.yaml":  cm("yy", nil),
		"app/nested/z.notyml": cm("z", nil),
	})
	r := newTestRenderer(t)
	res := generate(t, r, dirInput(root, &v1alpha1.ApplicationSourceDirectory{}))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/a", "ConfigMap/b"})
	res = generate(t, r, dirInput(root, &v1alpha1.ApplicationSourceDirectory{Recurse: true}))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/d", "ConfigMap/a", "ConfigMap/yy", "ConfigMap/x", "ConfigMap/b", "ConfigMap/e", "ConfigMap/c"})

	// A symlinked directory is not traversed.
	in := dirInput(root, &v1alpha1.ApplicationSourceDirectory{Recurse: true})
	in.Source.Path = "app"
	res = generate(t, r, in)
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/yy", "ConfigMap/x"})
}

func TestDirectoryIncludeExclude(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"config.json":             `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"config-json"}}`,
		"deployment.yaml":         cm("deployment", nil),
		"env-use2/configmap.yaml": cm("use2", nil),
		"env-usw2/configmap.yaml": cm("usw2", nil),
		"other.yml":               cm("other", nil),
		"some-directory/s.yaml":   cm("s", nil),
	}
	root := tempRepo(t, files)
	r := newTestRenderer(t)
	tests := []struct {
		name string
		dir  v1alpha1.ApplicationSourceDirectory
		want []string
	}{
		{"docs example", v1alpha1.ApplicationSourceDirectory{Recurse: true, Include: "*.yaml", Exclude: "{config.json,env-usw2/*}"},
			[]string{"ConfigMap/deployment", "ConfigMap/use2", "ConfigMap/s"}},
		{"brace include", v1alpha1.ApplicationSourceDirectory{Include: "{*.yml,*.yaml}"},
			[]string{"ConfigMap/deployment", "ConfigMap/other"}},
		{"subdirectory include", v1alpha1.ApplicationSourceDirectory{Recurse: true, Include: "some-directory/*"},
			[]string{"ConfigMap/s"}},
		{"exclude nothing", v1alpha1.ApplicationSourceDirectory{Exclude: "nothing-matches-*"},
			[]string{"ConfigMap/config-json", "ConfigMap/deployment", "ConfigMap/other"}},
		{"exclude wins over include", v1alpha1.ApplicationSourceDirectory{Include: "*.yaml", Exclude: "deploy*"},
			nil},
		{"malformed include matches nothing", v1alpha1.ApplicationSourceDirectory{Include: "["},
			nil},
		{"malformed exclude excludes nothing", v1alpha1.ApplicationSourceDirectory{Exclude: "["},
			[]string{"ConfigMap/config-json", "ConfigMap/deployment", "ConfigMap/other"}},
		{"comma list without braces is literal", v1alpha1.ApplicationSourceDirectory{Include: "deployment.yaml,other.yml"},
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := generate(t, r, dirInput(root, &tt.dir))
			if len(tt.want) == 0 && len(res.Manifests) == 0 {
				return
			}
			assertSlice(t, names(res.Manifests), tt.want)
		})
	}
}

func TestDirectorySkipMarker(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"a.yaml":    "# +argocd:skip-file-rendering\n" + cm("skipped", nil),
		"b.yaml":    cm("kept", nil),
		"c.yaml":    cm("patrol-skipped", map[string]string{"x": "+patrol:skip-file-rendering"}),
		"d.jsonnet": "// +argocd:skip-file-rendering\n{ syntax error",
		"huge.yaml": "# +argocd:skip-file-rendering\n" + strings.Repeat("#", 4096),
	})
	in := dirInput(root, nil)
	in.Limits.MaxCombinedDirectoryManifestsSize = 1024 // skipped files do not count
	res := generate(t, newTestRenderer(t), in)
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/kept"})
}

func TestDirectoryListsAndNullLists(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"list.yaml": `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: one
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: two
`,
		"null.yaml": "apiVersion: v1\nkind: ConfigMapList\nitems: null\n",
	})
	res := generate(t, newTestRenderer(t), dirInput(root, nil))
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/one", "ConfigMap/two"})
}

func TestDirectorySymlinks(t *testing.T) {
	t.Parallel()
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte(cm("outside", nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newTestRenderer(t)

	t.Run("in-repo link renders", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"real/a.yaml": cm("a", nil), "app/link.yaml": "->../real/a.yaml"})
		in := dirInput(root, nil)
		in.Source.Path = "app"
		assertSlice(t, names(generate(t, r, in).Manifests), []string{"ConfigMap/a"})
	})
	t.Run("out-of-root link rejected", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"link.yaml": "->" + outside})
		err := generateErr(t, r, dirInput(root, nil))
		assertContains(t, err.Error(), `illegal filepath in symlink at "link.yaml"`)
		assertContains(t, err.Error(), "failed to get potentially valid manifests: invalid manifest file")
	})
	t.Run("relative escape rejected", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"app/link.yaml": "->../../outside.yaml"})
		in := dirInput(root, nil)
		in.Source.Path = "app"
		assertContains(t, generateErr(t, r, in).Error(), "illegal filepath in symlink")
	})
	t.Run("dangling link skipped", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"dangling.yaml": "->missing.yaml", "a.yaml": cm("a", nil)})
		assertSlice(t, names(generate(t, r, dirInput(root, nil)).Manifests), []string{"ConfigMap/a"})
	})
	t.Run("link to directory skipped", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"d/x.txt": "x", "dir.yaml": "->d", "a.yaml": cm("a", nil)})
		assertSlice(t, names(generate(t, r, dirInput(root, nil)).Manifests), []string{"ConfigMap/a"})
	})
	t.Run("circular link fails", func(t *testing.T) {
		root := tempRepo(t, map[string]string{"a.yaml": "->b.yaml", "b.yaml": "->a.yaml"})
		assertContains(t, generateErr(t, r, dirInput(root, nil)).Error(), `failed to evaluate symlink at "a.yaml"`)
	})
}

func TestDirectoryMaxCombinedSize(t *testing.T) {
	t.Parallel()
	big := cm("big", map[string]string{"x": strings.Repeat("a", 6000)})
	root := tempRepo(t, map[string]string{"a.yaml": big, "b.yaml": strings.Replace(big, "name: big", "name: big2", 1), "lib.jsonnet": "{apiVersion: 'v1', kind: 'ConfigMap', metadata: {name: 'j'}}"})
	r := newTestRenderer(t)
	in := dirInput(root, nil)
	in.Limits.MaxCombinedDirectoryManifestsSize = 10000
	err := generateErr(t, r, in)
	assertEqual(t, err.Error(), "failed to get potentially valid manifests: exceeded max combined manifest file size")
	if !errors.Is(err, ErrExceededMaxCombinedManifestFileSize) {
		t.Fatalf("error %v does not match ErrExceededMaxCombinedManifestFileSize", err)
	}

	in.Limits.MaxCombinedDirectoryManifestsSize = -1 // unlimited (upstream 0)
	assertSlice(t, names(generate(t, r, in).Manifests), []string{"ConfigMap/big", "ConfigMap/big2", "ConfigMap/j"})

	// Excluded files and Jsonnet files do not count.
	in.Limits.MaxCombinedDirectoryManifestsSize = 10000
	in.Source.Directory = &v1alpha1.ApplicationSourceDirectory{Exclude: "b.yaml"}
	assertSlice(t, names(generate(t, r, in).Manifests), []string{"ConfigMap/big", "ConfigMap/j"})
}

func TestDirectoryRejectsObjectsWithoutAPIVersion(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{"a.yaml": "kind: ConfigMap\nmetadata:\n  name: x\n"})
	err := generateErr(t, newTestRenderer(t), dirInput(root, nil))
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("error %v is not ErrInvalidManifest", err)
	}
}

func TestDirectoryFixture(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	root := copyFixture(t, "directory")
	tests := []struct {
		name string
		dir  *v1alpha1.ApplicationSourceDirectory
		want []string
	}{
		{"defaults", nil, []string{"ConfigMap/config-json", "Deployment/guestbook-ui", "Service/guestbook-ui", "ConfigMap/list-item-1", "ConfigMap/list-item-2"}},
		{"recurse", &v1alpha1.ApplicationSourceDirectory{Recurse: true},
			[]string{"ConfigMap/config-json", "Deployment/guestbook-ui", "Service/guestbook-ui", "ConfigMap/use2", "ConfigMap/usw2", "ConfigMap/list-item-1", "ConfigMap/list-item-2"}},
		{"docs include/exclude example", &v1alpha1.ApplicationSourceDirectory{Recurse: true, Include: "*.yaml", Exclude: "{config.json,env-usw2/*}"},
			[]string{"Deployment/guestbook-ui", "Service/guestbook-ui", "ConfigMap/use2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := generate(t, r, dirInput(root, tt.dir))
			assertEqual(t, res.SourceType, "Directory")
			assertSlice(t, names(res.Manifests), tt.want)
		})
	}
}
