package examples_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	"github.com/gobwas/glob"
	"github.com/google/go-jsonnet"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// The examples refer to placeholder repositories; their content is a directory of examples/.
// docs/guides/examples.md documents the same mapping for users who push the examples to
// their own Git server.
var (
	// exampleRepos maps Git repository URLs to the directory (relative to examples/) that
	// is the repository root.
	exampleRepos = map[string]string{
		"https://git.example.com/platform/patrol-examples.git":        ".",
		"https://git.example.com/platform/patrol-examples-values.git": "apps/multi-source-helm-values/values-repo",
		// The application repository of the pull request and SCM provider examples.
		"https://github.com/example-org/guestbook.git": "apps/helm-chart",
	}
	// exampleCharts maps Helm chart sources ("<repoURL> <chart>" for Helm repositories,
	// the artifact URL for oci:// sources) to the chart directory that was pushed there
	// (apps/helm-oci-notes/README.md).
	exampleCharts = map[string]string{
		"registry.example.com/charts guestbook":       "apps/helm-chart/chart",
		"oci://registry.example.com/charts/guestbook": "apps/helm-chart/chart",
	}
)

// pluginConfigDir is where the cmp-server sidecar mounts plugin.yaml and its scripts
// (internal/cmp DefaultConfigDir). Command arguments below it are mapped to the directory of
// the example's plugin.yaml.
const pluginConfigDir = "/home/patrol/cmp-server/config/"

// pseudoRevision stands for the resolved commit of every Git source.
const pseudoRevision = "0123456789abcdef0123456789abcdef01234567"

// renderer renders Application sources from the examples with the real tools.
type renderer struct {
	tools   tools
	plugins map[string]pluginDef // by PluginName ("<name>[-<version>]")
	workDir string

	mu sync.Mutex
	// jsonnetFiles, cmpDirs and pluginRuns record what was exercised (coverage).
	jsonnetFiles map[string]int // rel path → evaluations
	cmpDirs      map[string]int // rel app dir → plugin runs
	commands     []string
}

type pluginDef struct {
	cfg *cmp.PluginConfig
	dir string // absolute directory of plugin.yaml
	rel string
}

func newRenderer(tl tools, in *inventory, workDir string) (*renderer, error) {
	r := &renderer{tools: tl, plugins: map[string]pluginDef{}, workDir: workDir, jsonnetFiles: map[string]int{}, cmpDirs: map[string]int{}}
	for _, d := range in.objects(roleCMPConfig) {
		data, err := os.ReadFile(d.file.abs)
		if err != nil {
			return nil, err
		}
		cfg, err := cmp.ParsePluginConfig(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
		r.plugins[cfg.PluginName()] = pluginDef{cfg: cfg, dir: filepath.Dir(d.file.abs), rel: d.file.rel}
	}
	return r, nil
}

// renderResult is the output of one source.
type renderResult struct {
	tool    string // Helm, Kustomize, Directory, Plugin
	dir     string // rendered directory, relative to examples/
	objects []*unstructured.Unstructured
	// files are the manifest files a Directory source selected (relative to dir).
	files []string
}

// locate resolves a source to a directory of examples/ (relative path) and whether it is a
// Helm chart source (chart or oci:// artifact).
func locate(src v1alpha1.ApplicationSource) (string, bool, error) {
	if src.Chart != "" {
		dir, ok := exampleCharts[src.RepoURL+" "+src.Chart]
		if !ok {
			return "", false, fmt.Errorf("chart %s of %s is not provided by the examples", src.Chart, src.RepoURL)
		}
		return dir, true, nil
	}
	if dir, ok := exampleCharts[src.RepoURL]; ok {
		if p := path.Clean("/" + src.Path); p != "/" {
			return "", false, fmt.Errorf("%s is a Helm chart artifact: path must be \".\", got %q", src.RepoURL, src.Path)
		}
		return dir, true, nil
	}
	root, ok := exampleRepos[src.RepoURL]
	if !ok {
		return "", false, fmt.Errorf("repository %s is not provided by the examples", src.RepoURL)
	}
	p := path.Join(root, src.Path)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false, fmt.Errorf("path %q escapes the repository", src.Path)
	}
	return p, false, nil
}

// repoRootOf returns the repository root directory (relative to examples/) of a Git source.
func repoRootOf(src v1alpha1.ApplicationSource) string {
	if root, ok := exampleRepos[src.RepoURL]; ok {
		return root
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// renderSource renders one source of app. refs maps "$<ref>" to the root directory
// (relative to examples/) of the source with that ref.
func (r *renderer) renderSource(ctx context.Context, app *v1alpha1.Application, src v1alpha1.ApplicationSource, refs map[string]string) (*renderResult, error) {
	rel, isChart, err := locate(src)
	if err != nil {
		return nil, err
	}
	abs := filepath.Join(examplesDir, filepath.FromSlash(rel))
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("source directory %s does not exist", rel)
	}
	if isChart && src.TargetRevision != "" {
		if err := checkChartVersion(abs, src.TargetRevision); err != nil {
			return nil, err
		}
	}
	explicit, err := render.ExplicitType(&src)
	if err != nil {
		return nil, err
	}
	var tool v1alpha1.ApplicationSourceType
	switch {
	case explicit != nil:
		tool = *explicit
	case isChart || exists(filepath.Join(abs, "Chart.yaml")):
		tool = v1alpha1.ApplicationSourceTypeHelm
	case slices.ContainsFunc(render.KustomizationNames, func(n string) bool { return exists(filepath.Join(abs, n)) }):
		tool = v1alpha1.ApplicationSourceTypeKustomize
	default:
		tool = v1alpha1.ApplicationSourceTypeDirectory
	}
	res := &renderResult{tool: string(tool), dir: rel}
	switch tool {
	case v1alpha1.ApplicationSourceTypeHelm:
		res.objects, err = r.helmTemplate(ctx, app, src, abs, refs)
	case v1alpha1.ApplicationSourceTypeKustomize:
		res.objects, err = r.kustomizeBuild(ctx, src, abs)
	case v1alpha1.ApplicationSourceTypeDirectory:
		res.objects, res.files, err = r.directory(src, filepath.Join(examplesDir, filepath.FromSlash(repoRootOf(src))), abs)
	case v1alpha1.ApplicationSourceTypePlugin:
		res.objects, err = r.plugin(ctx, app, src, abs, rel)
	default:
		err = fmt.Errorf("unsupported source type %s", tool)
	}
	if err != nil {
		return nil, fmt.Errorf("%s source %s: %w", tool, rel, err)
	}
	return res, nil
}

func checkChartVersion(chartDir, targetRevision string) error {
	data, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return err
	}
	var chart struct {
		Version string `json:"version"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		return err
	}
	c, err := semver.NewConstraint(targetRevision)
	if err != nil {
		return fmt.Errorf("targetRevision %q is not a version constraint: %w", targetRevision, err)
	}
	v, err := semver.NewVersion(chart.Version)
	if err != nil {
		return err
	}
	if !c.Check(v) {
		return fmt.Errorf("chart version %s does not satisfy targetRevision %q", chart.Version, targetRevision)
	}
	return nil
}

// run executes a tool and returns its standard output.
func (r *renderer) run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	line := filepath.Base(name) + " " + strings.ReplaceAll(strings.Join(args, " "), examplesDir+string(filepath.Separator), "examples/")
	r.mu.Lock()
	r.commands = append(r.commands, line)
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", line, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// decodeManifests splits tool output into objects, flattening List kinds.
func decodeManifests(data []byte) ([]*unstructured.Unstructured, error) {
	objs, err := render.SplitYAML(data)
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if o.IsList() {
			if err := o.EachListItem(func(item k8sruntime.Object) error {
				u, ok := item.(*unstructured.Unstructured)
				if !ok {
					return errors.New("unexpected list item type")
				}
				out = append(out, u)
				return nil
			}); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// resolveValueFile resolves a Helm value file or file parameter path: "$<ref>/<path>" is
// relative to the root of the source with that ref, other paths to the chart directory.
func resolveValueFile(chartDir, p string, refs map[string]string) (string, error) {
	if strings.HasPrefix(p, "$") {
		ref, rest, _ := strings.Cut(p, "/")
		root, ok := refs[ref]
		if !ok {
			return "", fmt.Errorf("value file %s: no source has ref %s", p, strings.TrimPrefix(ref, "$"))
		}
		return filepath.Join(examplesDir, filepath.FromSlash(root), filepath.FromSlash(rest)), nil
	}
	return filepath.Join(chartDir, filepath.FromSlash(p)), nil
}

// helmTemplate renders a Helm source as Patrol CD does: value files, then values /
// valuesObject, then parameters, release name defaulting to the Application name.
func (r *renderer) helmTemplate(ctx context.Context, app *v1alpha1.Application, src v1alpha1.ApplicationSource, chartDir string, refs map[string]string) ([]*unstructured.Unstructured, error) {
	h := src.Helm
	if h == nil {
		h = &v1alpha1.ApplicationSourceHelm{}
	}
	release := app.Name
	if h.ReleaseName != "" {
		release = h.ReleaseName
	}
	ns := app.Spec.Destination.Namespace
	if h.Namespace != "" {
		ns = h.Namespace
	}
	kubeVersion := envtestK8sVersion
	if h.KubeVersion != "" {
		kubeVersion = h.KubeVersion
	}
	args := []string{"template", release, chartDir, "--namespace", ns, "--kube-version", kubeVersion}
	if h.SkipCrds {
		args = append(args, "--skip-crds")
	} else {
		args = append(args, "--include-crds")
	}
	for _, vf := range h.ValueFiles {
		p, err := resolveValueFile(chartDir, vf, refs)
		if err != nil {
			return nil, err
		}
		if !exists(p) {
			if h.IgnoreMissingValueFiles {
				continue
			}
			return nil, fmt.Errorf("value file %s does not exist", vf)
		}
		args = append(args, "--values", p)
	}
	inline := []byte(h.Values)
	if h.ValuesObject != nil && len(h.ValuesObject.Raw) > 0 {
		inline = h.ValuesObject.Raw
	}
	if len(bytes.TrimSpace(inline)) > 0 {
		f, err := os.CreateTemp(r.workDir, "values-*.yaml")
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(inline); err != nil {
			return nil, err
		}
		_ = f.Close()
		args = append(args, "--values", f.Name())
	}
	for _, p := range h.Parameters {
		flag := "--set"
		if p.ForceString {
			flag = "--set-string"
		}
		args = append(args, flag, p.Name+"="+p.Value)
	}
	for _, p := range h.FileParameters {
		fp, err := resolveValueFile(chartDir, p.Path, refs)
		if err != nil {
			return nil, err
		}
		args = append(args, "--set-file", p.Name+"="+fp)
	}
	if h.SkipTests {
		args = append(args, "--skip-tests")
	}
	if h.SkipSchemaValidation {
		args = append(args, "--skip-schema-validation")
	}
	for _, v := range h.APIVersions {
		args = append(args, "--api-versions", v)
	}
	out, err := r.run(ctx, chartDir, nil, r.tools.helm, args...)
	if err != nil {
		return nil, err
	}
	return decodeManifests(out)
}

// kustomizeBuild runs kustomize build with the build options of examples/platform/settings/
// patrol-cm.yaml (kustomize.buildOptions: --enable-helm). Application-level Kustomize
// overrides are not emulated, so the examples must not use them.
func (r *renderer) kustomizeBuild(ctx context.Context, src v1alpha1.ApplicationSource, dir string) ([]*unstructured.Unstructured, error) {
	if src.Kustomize != nil {
		data, _ := json.Marshal(src.Kustomize)
		if string(data) != "{}" {
			return nil, fmt.Errorf("spec.source.kustomize overrides are not emulated by test/examples: %s", data)
		}
	}
	out, err := r.run(ctx, dir, nil, r.tools.kustomize, kustomizeArgs(r.tools, dir)...)
	if err != nil {
		return nil, err
	}
	return decodeManifests(out)
}

func kustomizeArgs(tl tools, dir string) []string {
	return []string{"build", "--enable-helm", "--helm-command", tl.helm, dir}
}

// manifestFileRe selects Directory-source candidates (upstream manifestFile).
var manifestFileRe = regexp.MustCompile(`^.*\.(yaml|yml|json|jsonnet)$`)

// directory renders a Directory source: recurse, include/exclude (gobwas/glob patterns
// without separators, matched against the path relative to the source path, exclude
// first) and Jsonnet with extVars, TLAs and library paths relative to the repository root.
func (r *renderer) directory(src v1alpha1.ApplicationSource, repoRootAbs, dir string) ([]*unstructured.Unstructured, []string, error) {
	d := v1alpha1.ApplicationSourceDirectory{}
	if src.Directory != nil {
		d = *src.Directory
	}
	var include, exclude *glob.Pattern
	var err error
	if d.Include != "" {
		if include, err = glob.Compile(d.Include); err != nil {
			return nil, nil, fmt.Errorf("include %q: %w", d.Include, err)
		}
	}
	if d.Exclude != "" {
		if exclude, err = glob.Compile(d.Exclude); err != nil {
			return nil, nil, fmt.Errorf("exclude %q: %w", d.Exclude, err)
		}
	}
	var files []string
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != dir && !d.Recurse {
				return fs.SkipDir
			}
			return nil
		}
		if !manifestFileRe.MatchString(e.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if exclude != nil && exclude.Match(rel) {
			return nil
		}
		if include != nil && !include.Match(rel) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var objs []*unstructured.Unstructured
	for _, rel := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		var out []*unstructured.Unstructured
		if strings.HasSuffix(rel, ".jsonnet") {
			out, err = r.jsonnet(abs, repoRootAbs, d.Jsonnet)
		} else {
			var data []byte
			if data, err = os.ReadFile(abs); err == nil {
				out, err = decodeManifests(data)
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", rel, err)
		}
		objs = append(objs, out...)
	}
	return objs, files, nil
}

func (r *renderer) jsonnet(file, repoRootAbs string, opts v1alpha1.ApplicationSourceJsonnet) ([]*unstructured.Unstructured, error) {
	vm := jsonnet.MakeVM()
	var jpaths []string
	for _, lib := range opts.Libs {
		jpaths = append(jpaths, filepath.Join(repoRootAbs, filepath.FromSlash(lib)))
	}
	vm.Importer(&jsonnet.FileImporter{JPaths: jpaths})
	for _, v := range opts.ExtVars {
		if v.Code {
			vm.ExtCode(v.Name, v.Value)
		} else {
			vm.ExtVar(v.Name, v.Value)
		}
	}
	for _, v := range opts.TLAs {
		if v.Code {
			vm.TLACode(v.Name, v.Value)
		} else {
			vm.TLAVar(v.Name, v.Value)
		}
	}
	out, err := vm.EvaluateFile(file)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(examplesDir, file)
	r.mu.Lock()
	r.jsonnetFiles[filepath.ToSlash(rel)]++
	r.mu.Unlock()
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, err
	}
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case map[string]any:
		items = []any{t}
	default:
		return nil, fmt.Errorf("jsonnet output is %T, want an object or an array", v)
	}
	var objs []*unstructured.Unstructured
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jsonnet array element is %T, want an object", it)
		}
		u := &unstructured.Unstructured{Object: m}
		if u.IsList() {
			data, _ := json.Marshal(m)
			list, err := decodeManifests(data)
			if err != nil {
				return nil, err
			}
			objs = append(objs, list...)
			continue
		}
		objs = append(objs, u)
	}
	return objs, nil
}

// escapeParamName is upstream's parameter name escaping for PARAM_<NAME> variables.
func escapeParamName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, strings.ToUpper(name))
}

// pluginEnv is the generate environment Patrol CD gives a plugin: the sandbox base
// environment, the build variables (ARGOCD_APP_* and PATROL_APP_*), KUBE_VERSION,
// KUBE_API_VERSIONS, ARGOCD_ENV_<name> and the parameter variables.
func pluginEnv(app *v1alpha1.Application, src v1alpha1.ApplicationSource) ([]string, error) {
	build := map[string]string{
		"APP_NAME":                   app.Name,
		"APP_NAMESPACE":              app.Spec.Destination.Namespace,
		"APP_PROJECT_NAME":           app.Spec.GetProject(),
		"APP_REVISION":               pseudoRevision,
		"APP_REVISION_SHORT":         pseudoRevision[:7],
		"APP_REVISION_SHORT_8":       pseudoRevision[:8],
		"APP_SOURCE_REPO_URL":        src.RepoURL,
		"APP_SOURCE_PATH":            src.Path,
		"APP_SOURCE_TARGET_REVISION": src.TargetRevision,
	}
	var buildEnv []string
	lookup := map[string]string{}
	for _, prefix := range []string{"ARGOCD_", "PATROL_"} {
		for _, k := range slices.Sorted(maps.Keys(build)) {
			buildEnv = append(buildEnv, prefix+k+"="+build[k])
			lookup[prefix+k] = build[k]
		}
	}
	env := sandbox.BaseEnv()
	env = append(env, buildEnv...)
	env = append(env, "KUBE_VERSION="+envtestK8sVersion, "KUBE_API_VERSIONS=")
	p := src.Plugin
	for _, e := range p.Env {
		env = append(env, "ARGOCD_ENV_"+e.Name+"="+os.Expand(e.Value, func(k string) string { return lookup[k] }))
	}
	params, err := json.Marshal(p.Parameters)
	if err != nil {
		return nil, err
	}
	env = append(env, "ARGOCD_APP_PARAMETERS="+string(params))
	for _, param := range p.Parameters {
		base := "PARAM_" + escapeParamName(param.Name)
		if param.String != nil {
			env = append(env, base+"="+*param.String)
		}
		if param.OptionalMap != nil {
			for _, k := range slices.Sorted(maps.Keys(param.Map)) {
				env = append(env, base+"_"+escapeParamName(k)+"="+param.Map[k])
			}
		}
		if param.OptionalArray != nil {
			for i, v := range param.Array {
				env = append(env, base+"_"+strconv.Itoa(i)+"="+v)
			}
		}
	}
	return env, nil
}

// plugin runs the generate command of the Config Management Plugin selected by
// spec.source.plugin.name (or by discovery) in a copy of the application directory, as
// the cmp-server sidecar does with the streamed source.
func (r *renderer) plugin(ctx context.Context, app *v1alpha1.Application, src v1alpha1.ApplicationSource, dir, rel string) ([]*unstructured.Unstructured, error) {
	var def pluginDef
	if name := src.Plugin.Name; name != "" {
		d, ok := r.plugins[name]
		if !ok {
			return nil, fmt.Errorf("no ConfigManagementPlugin named %s in the examples", name)
		}
		def = d
	} else {
		for _, name := range slices.Sorted(maps.Keys(r.plugins)) {
			d := r.plugins[name]
			if m, _ := filepath.Glob(filepath.Join(dir, d.cfg.Spec.Discover.FileName)); d.cfg.Spec.Discover.FileName != "" && len(m) > 0 {
				def = d
				break
			}
		}
		if def.cfg == nil {
			return nil, errors.New("no plugin discovers the application directory")
		}
	}
	// Discovery must also select the directory, so the plugin works without an explicit name.
	if pattern := def.cfg.Spec.Discover.FileName; pattern != "" {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) == 0 {
			return nil, fmt.Errorf("discover.fileName %q of %s does not match the application directory", pattern, def.rel)
		}
	}
	work, err := os.MkdirTemp(r.workDir, "cmp-")
	if err != nil {
		return nil, err
	}
	if err := os.CopyFS(work, os.DirFS(dir)); err != nil {
		return nil, err
	}
	env, err := pluginEnv(app, src)
	if err != nil {
		return nil, err
	}
	run := func(c cmp.Command) ([]byte, error) {
		argv := append(slices.Clone(c.Command), c.Args...)
		for i, a := range argv {
			if rest, ok := strings.CutPrefix(a, pluginConfigDir); ok {
				argv[i] = filepath.Join(def.dir, filepath.FromSlash(rest))
			}
		}
		return r.run(ctx, work, env, argv[0], argv[1:]...)
	}
	if def.cfg.Spec.Init.IsSet() {
		if _, err := run(def.cfg.Spec.Init); err != nil {
			return nil, err
		}
	}
	out, err := run(def.cfg.Spec.Generate)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cmpDirs[rel]++
	r.mu.Unlock()
	return decodeManifests(out)
}
