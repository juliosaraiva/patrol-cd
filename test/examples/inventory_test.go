package examples_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render"
)

// fileClass says how a file under examples/ is validated.
type fileClass string

const (
	classYAML         fileClass = "yaml"          // parsed; each document is dispatched by kind
	classJSON         fileClass = "json"          // parsed
	classHelmTemplate fileClass = "helm-template" // rendered by helm lint/template
	classJsonnet      fileClass = "jsonnet"       // evaluated through the Applications using it
	classLua          fileClass = "lua"           // executed by the Lua customization test
	classShell        fileClass = "shell"         // sh -n, and executed as plugin generate command
	classCMPTemplate  fileClass = "cmp-template"  // rendered by the Config Management Plugin
	classTypeScript   fileClass = "typescript"    // UI extension; not compiled by this Go test
	classJavaScript   fileClass = "javascript"    // UI extension smoke script; not run by this Go test
	classMarkdown     fileClass = "markdown"      // documentation
	classGitIgnore    fileClass = "gitignore"
)

// exampleFile is one file under examples/.
type exampleFile struct {
	rel   string // slash-separated path relative to examples/
	abs   string
	class fileClass
	// chartDir and kustDir are the enclosing Helm chart / Kustomization directory (rel),
	// "" when none.
	chartDir string
	kustDir  string
}

// exampleDoc is one YAML document of a classYAML file.
type exampleDoc struct {
	file  *exampleFile
	index int
	// obj is set for documents with apiVersion and kind.
	obj *unstructured.Unstructured
}

func (d exampleDoc) String() string {
	if d.obj == nil {
		return fmt.Sprintf("%s#%d", d.file.rel, d.index)
	}
	name := d.obj.GetName()
	if name == "" {
		name = d.obj.GetGenerateName() + "*"
	}
	return fmt.Sprintf("%s#%d (%s %s)", d.file.rel, d.index, d.obj.GetKind(), name)
}

// docRole says how a Kubernetes object document is validated.
type docRole string

const (
	roleObject         docRole = "object"          // dry-run created as is
	roleCMPConfig      docRole = "cmp-config"      // plugin.yaml, parsed by internal/cmp
	roleKustomization  docRole = "kustomization"   // kustomize.config.k8s.io file
	roleKustomizeInput docRole = "kustomize-input" // resource or patch of a Kustomization (validated through kustomize build)
)

func (d exampleDoc) role() docRole {
	gv := d.obj.GroupVersionKind()
	switch {
	case gv.Group == v1alpha1.GroupName && gv.Kind == "ConfigManagementPlugin":
		return roleCMPConfig
	case gv.Group == "kustomize.config.k8s.io":
		return roleKustomization
	case gv.Group == v1alpha1.GroupName:
		// Patrol objects next to a Kustomization (an Application deploying it) are complete
		// objects, not kustomize resources or patches.
		return roleObject
	case d.file.kustDir != "":
		return roleKustomizeInput
	}
	return roleObject
}

// inventory is the classified and parsed content of examples/.
type inventory struct {
	files     []*exampleFile
	docs      []exampleDoc // every non-empty YAML document
	chartDirs []string     // rel paths of directories with Chart.yaml
	kustDirs  []string     // rel paths of directories with a kustomization file
	errs      []error
}

var (
	invOnce sync.Once
	inv     *inventory
)

// loadInventory walks examples/ once per test binary.
func loadInventory(t *testing.T) *inventory {
	t.Helper()
	invOnce.Do(func() { inv = buildInventory() })
	if len(inv.errs) > 0 {
		for _, err := range inv.errs {
			t.Error(err)
		}
		t.FailNow()
	}
	return inv
}

func buildInventory() *inventory {
	in := &inventory{}
	var all []string
	err := filepath.WalkDir(examplesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(examplesDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			in.errs = append(in.errs, fmt.Errorf("%s: only regular files are allowed in examples/", rel))
			return nil
		}
		all = append(all, rel)
		switch {
		case d.Name() == "Chart.yaml":
			in.chartDirs = append(in.chartDirs, path.Dir(rel))
		case render.IsKustomization(d.Name()):
			in.kustDirs = append(in.kustDirs, path.Dir(rel))
		}
		return nil
	})
	if err != nil {
		in.errs = append(in.errs, err)
		return in
	}
	sort.Strings(all)
	for _, rel := range all {
		f := &exampleFile{rel: rel, abs: filepath.Join(examplesDir, filepath.FromSlash(rel))}
		f.chartDir = enclosing(rel, in.chartDirs)
		f.kustDir = enclosing(rel, in.kustDirs)
		class, err := classify(f)
		if err != nil {
			in.errs = append(in.errs, err)
			continue
		}
		f.class = class
		in.files = append(in.files, f)
		switch class {
		case classYAML:
			docs, err := parseYAMLFile(f)
			if err != nil {
				in.errs = append(in.errs, err)
				continue
			}
			in.docs = append(in.docs, docs...)
		case classJSON:
			data, err := os.ReadFile(f.abs)
			if err == nil && !json.Valid(data) {
				err = errors.New("invalid JSON")
			}
			if err != nil {
				in.errs = append(in.errs, fmt.Errorf("%s: %w", rel, err))
			}
		}
	}
	return in
}

// enclosing returns the longest dir of dirs that contains rel ("" when none).
func enclosing(rel string, dirs []string) string {
	best := ""
	for _, d := range dirs {
		if (d == "." || strings.HasPrefix(rel, d+"/")) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

func classify(f *exampleFile) (fileClass, error) {
	base := path.Base(f.rel)
	ext := path.Ext(base)
	if f.chartDir != "" {
		inChart := strings.TrimPrefix(f.rel, f.chartDir+"/")
		switch {
		case strings.HasPrefix(inChart, "templates/"), base == ".helmignore":
			return classHelmTemplate, nil
		}
	}
	switch {
	case ext == ".yaml" || ext == ".yml":
		return classYAML, nil
	case ext == ".json":
		return classJSON, nil
	case ext == ".jsonnet" || ext == ".libsonnet":
		return classJsonnet, nil
	case ext == ".lua":
		return classLua, nil
	case ext == ".sh":
		return classShell, nil
	case ext == ".tmpl":
		return classCMPTemplate, nil
	case ext == ".ts" || ext == ".tsx":
		return classTypeScript, nil
	case ext == ".mjs":
		return classJavaScript, nil
	case ext == ".md":
		return classMarkdown, nil
	case base == ".gitignore":
		return classGitIgnore, nil
	}
	return "", fmt.Errorf("%s: no validation is defined for this file type; extend classify in test/examples", f.rel)
}

// parseYAMLFile splits a YAML file into documents and decodes each one strictly (duplicate
// keys are errors).
func parseYAMLFile(f *exampleFile) ([]exampleDoc, error) {
	data, err := os.ReadFile(f.abs)
	if err != nil {
		return nil, err
	}
	var out []exampleDoc
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for i := 0; ; i++ {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", f.rel, i, err)
		}
		var m map[string]any
		if err := yaml.UnmarshalStrict(raw, &m); err != nil {
			// Non-object documents (lists, scalars) are not used in examples.
			return nil, fmt.Errorf("%s: document %d: %w", f.rel, i, err)
		}
		if len(m) == 0 {
			continue
		}
		d := exampleDoc{file: f, index: i}
		if _, ok := m["apiVersion"]; ok {
			if _, ok := m["kind"]; ok {
				d.obj = &unstructured.Unstructured{Object: m}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// objects returns the documents with the given role.
func (in *inventory) objects(role docRole) []exampleDoc {
	var out []exampleDoc
	for _, d := range in.docs {
		if d.obj != nil && d.role() == role {
			out = append(out, d)
		}
	}
	return out
}

// byKind returns the roleObject documents of a group/kind.
func (in *inventory) byKind(group, kind string) []exampleDoc {
	var out []exampleDoc
	for _, d := range in.objects(roleObject) {
		gvk := d.obj.GroupVersionKind()
		if gvk.Group == group && gvk.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func (in *inventory) filesOf(class fileClass) []*exampleFile {
	var out []*exampleFile
	for _, f := range in.files {
		if f.class == class {
			out = append(out, f)
		}
	}
	return out
}

// decodeInto converts an unstructured object to a typed one, rejecting unknown fields.
func decodeInto(obj *unstructured.Unstructured, into any) error {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// TestInventory checks that every file under examples/ has a validation, that YAML and
// JSON files parse, that shell scripts are syntactically valid and that Secret templates
// carry only REPLACE_ME placeholders.
func TestInventory(t *testing.T) {
	in := loadInventory(t)

	counts := map[fileClass]int{}
	for _, f := range in.files {
		counts[f.class]++
	}
	roles := map[docRole]int{}
	data := 0
	for _, d := range in.docs {
		if d.obj == nil {
			data++
			continue
		}
		roles[d.role()]++
	}
	classes := make([]string, 0, len(counts))
	for c, n := range counts {
		classes = append(classes, fmt.Sprintf("%s=%d", c, n))
	}
	sort.Strings(classes)
	t.Logf("files under examples/: %d (%s)", len(in.files), strings.Join(classes, " "))
	t.Logf("YAML documents: %d (objects=%d cmp-config=%d kustomization=%d kustomize-input=%d data=%d)",
		len(in.docs), roles[roleObject], roles[roleCMPConfig], roles[roleKustomization], roles[roleKustomizeInput], data)
	t.Logf("Helm charts: %d %v; Kustomization directories: %d %v", len(in.chartDirs), in.chartDirs, len(in.kustDirs), in.kustDirs)
	t.Logf("not validated by this Go test: %d TypeScript and %d JavaScript files (UI extension: pnpm typecheck/build/smoke, see docs/guides/examples.md), %d markdown, %d .gitignore",
		counts[classTypeScript], counts[classJavaScript], counts[classMarkdown], counts[classGitIgnore])

	t.Run("shell syntax", func(t *testing.T) {
		for _, f := range in.filesOf(classShell) {
			out, err := exec.Command("sh", "-n", f.abs).CombinedOutput()
			if err != nil {
				t.Errorf("%s: sh -n: %v\n%s", f.rel, err, out)
			}
		}
	})

	t.Run("secret placeholders", func(t *testing.T) {
		n := 0
		for _, d := range in.byKind("", "Secret") {
			n++
			if _, ok := d.obj.Object["data"]; ok {
				t.Errorf("%s: use stringData with REPLACE_ME placeholders, not data", d)
			}
			sd, _, _ := unstructured.NestedStringMap(d.obj.Object, "stringData")
			if len(sd) == 0 {
				t.Errorf("%s: Secret template without stringData", d)
			}
			for k, v := range sd {
				if v != "REPLACE_ME" {
					t.Errorf("%s: stringData[%q] must be the placeholder REPLACE_ME", d, k)
				}
			}
		}
		t.Logf("Secret templates checked: %d", n)
	})

	t.Run("control-plane object names", func(t *testing.T) {
		// Two different control-plane objects (namespace patrol) with the same kind and name
		// would overwrite each other when the examples are applied together. Fragments meant
		// to be merged into another object are listed. (Application source manifests are
		// deployed to their Application's destination namespace and may share names.)
		mergeFragments := map[string]bool{"github/04-notifications.yaml": true}
		seen := map[string]string{}
		for _, d := range in.objects(roleObject) {
			if d.obj.GetName() == "" || d.obj.GetNamespace() != controlPlaneNamespace || mergeFragments[d.file.rel] {
				continue
			}
			key := d.obj.GroupVersionKind().GroupKind().String() + "/" + d.obj.GetNamespace() + "/" + d.obj.GetName()
			if prev, ok := seen[key]; ok {
				t.Errorf("%s and %s define the same object %s", prev, d, key)
			}
			seen[key] = d.String()
		}
	})

	if len(in.filesOf(classTypeScript)) > 0 && !slices.ContainsFunc(in.files, func(f *exampleFile) bool { return path.Base(f.rel) == "package.json" }) {
		t.Error("TypeScript files without package.json")
	}
}
