package examples_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// defaultObjectNamespace receives namespaced example objects without a namespace (source
// manifests that Patrol CD would deploy to an Application's destination namespace).
const defaultObjectNamespace = "patrol-examples"

// expectedDirectoryFiles pins the include/exclude behavior documented in
// examples/apps/directory-recurse-include-exclude/application.yaml.
var expectedDirectoryFiles = map[string][]string{
	"guestbook-directory": {"base/configmap.yaml", "base/deployment.yaml", "networking/networkpolicy.json", "networking/service.yml"},
}

// report counts what the test exercised; it is logged at the end.
type report struct {
	mu     sync.Mutex
	counts map[string]int
	lines  []string
}

func (r *report) add(key string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[key] += n
}

func (r *report) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// TestExamplesAgainstAPIServer validates the examples against a real kube-apiserver with
// the Patrol CD CRDs:
//
//  1. every Kubernetes object document is dry-run created (strict field validation);
//  2. every Helm chart is linted and templated (default values and each values-*.yaml), and
//     every Kustomization is built; their output is dry-run created;
//  3. every Application is validated against its AppProject (project.ValidateApplication)
//     and each of its sources is rendered as Patrol CD would (Helm, Kustomize, Directory with
//     Jsonnet, Config Management Plugin); rendered objects must be permitted by the project
//     and are dry-run created; Applications rendered by an app of apps are processed the
//     same way;
//  4. every ApplicationSet is expanded with the Patrol CD generators (appset.APIGenerator)
//     against the example Clusters, repositories, a fake GitHub and a plugin service, the
//     generated Applications are compared with expectedApplications and processed as in 3.
//
// 1 needs only envtest. 2-4 also need helm and kustomize; without them those subtests are
// skipped, or fail when PATROL_REQUIRE_EXAMPLE_TOOLS or PATROL_REQUIRE_ENVTEST is set.
func TestExamplesAgainstAPIServer(t *testing.T) {
	cfg := requireAPIServer(t)
	// Only the subtests that render sources need helm and kustomize; the dry-run of every
	// static example object and the seeding run without them.
	tl, missingTools := findTools()
	in := loadInventory(t)
	ctx := context.Background()
	api := newAPIServer(t, cfg)
	rep := &report{}
	if err := api.ensureNamespace(ctx, controlPlaneNamespace); err != nil {
		t.Fatal(err)
	}

	// The Operation admission policy ships with the CRDs (deploy/crds) but is not a CRD.
	t.Run("install operations admission policy", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "crds", "operations-admission-policy.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		docs, err := decodeManifests(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range docs {
			if err := api.apply(ctx, o); err != nil {
				t.Fatalf("%s %s: %v", o.GetKind(), o.GetName(), err)
			}
		}
	})

	t.Run("static objects", func(t *testing.T) {
		n := 0
		for _, d := range in.objects(roleObject) {
			if err := api.dryRun(ctx, d.obj, defaultObjectNamespace); err != nil {
				t.Errorf("%s: %v", d, err)
				continue
			}
			n++
		}
		rep.add("static objects dry-run", n)
	})

	// Objects the generators read (Clusters, SCMProviders, AppProjects, Secrets,
	// ConfigMaps in the control-plane namespace) are created for real.
	t.Run("seed control-plane objects", func(t *testing.T) {
		seed := map[string]bool{"Cluster": true, "SCMProvider": true, "AppProject": true, "Secret": true, "ConfigMap": true}
		created := 0
		for _, d := range in.objects(roleObject) {
			if d.obj.GetNamespace() != controlPlaneNamespace || !seed[d.obj.GetKind()] {
				continue
			}
			ok, err := api.create(ctx, d.obj)
			if err != nil {
				t.Fatalf("%s: %v", d, err)
			}
			if ok {
				created++
			}
		}
		rep.add("control-plane objects created for the generators", created)
	})

	workDir := t.TempDir()
	r, err := newRenderer(tl, in, workDir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("helm charts", func(t *testing.T) {
		requireTools(t, missingTools)
		for _, dir := range in.chartDirs {
			abs := filepath.Join(examplesDir, filepath.FromSlash(dir))
			valueSets := [][]string{nil}
			extra, _ := filepath.Glob(filepath.Join(abs, "values-*.yaml"))
			for _, f := range extra {
				valueSets = append(valueSets, []string{"--values", f})
			}
			for _, vs := range valueSets {
				args := append([]string{"lint", "--strict", abs}, vs...)
				if _, err := r.run(ctx, abs, nil, tl.helm, args...); err != nil {
					t.Errorf("%s: %v", dir, err)
					continue
				}
				out, err := r.run(ctx, abs, nil, tl.helm, append([]string{"template", "release", abs, "--namespace", defaultObjectNamespace, "--include-crds", "--kube-version", envtestK8sVersion}, vs...)...)
				if err != nil {
					t.Errorf("%s: %v", dir, err)
					continue
				}
				objs, err := decodeManifests(out)
				if err != nil {
					t.Errorf("%s: %v", dir, err)
					continue
				}
				for _, o := range objs {
					if err := api.dryRun(ctx, o, defaultObjectNamespace); err != nil {
						t.Errorf("%s (%v): %s %s: %v", dir, vs, o.GetKind(), o.GetName(), err)
					}
				}
				rep.add("helm lint+template runs", 1)
				rep.add("objects rendered by helm template", len(objs))
			}
		}
	})

	t.Run("kustomizations", func(t *testing.T) {
		requireTools(t, missingTools)
		components := map[string]bool{}
		referenced := map[string]bool{}
		for _, dir := range in.kustDirs {
			abs := filepath.Join(examplesDir, filepath.FromSlash(dir))
			k, err := readKustomization(abs)
			if err != nil {
				t.Errorf("%s: %v", dir, err)
				continue
			}
			for _, c := range k.Components {
				referenced[path.Join(dir, c)] = true
			}
			if k.Kind == "Component" {
				components[dir] = true
				continue
			}
			out, err := r.run(ctx, abs, nil, tl.kustomize, kustomizeArgs(tl, abs)...)
			if err != nil {
				t.Errorf("%s: %v", dir, err)
				continue
			}
			objs, err := decodeManifests(out)
			if err != nil {
				t.Errorf("%s: %v", dir, err)
				continue
			}
			for _, o := range objs {
				if err := api.dryRun(ctx, o, defaultObjectNamespace); err != nil {
					t.Errorf("%s: %s %s: %v", dir, o.GetKind(), o.GetName(), err)
				}
			}
			rep.add("kustomize builds", 1)
			rep.add("objects rendered by kustomize build", len(objs))
		}
		for c := range components {
			if !referenced[c] {
				t.Errorf("kustomize component %s is not used by any Kustomization", c)
			}
		}
		rep.add("kustomize components (built through overlays)", len(components))
	})

	runner := &appRunner{api: api, renderer: r, rep: rep, projects: staticProjects(t, in), processed: map[string]bool{}}
	var clusters []v1alpha1.Cluster
	for _, d := range in.byKind(v1alpha1.GroupName, "Cluster") {
		c := v1alpha1.Cluster{}
		if err := decodeInto(d.obj, &c); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		clusters = append(clusters, c)
	}
	runner.clusters = project.NewClusterIndex(clusters, true)

	// Applications share the control-plane namespace: static, generated and app-of-apps
	// Applications must not collide when every example is applied.
	appNames := map[string]string{}
	claimName := func(t *testing.T, name, origin string) {
		t.Helper()
		if prev, ok := appNames[name]; ok {
			t.Errorf("Application name %s is used by %s and %s", name, prev, origin)
			return
		}
		appNames[name] = origin
	}
	runner.claim = claimName

	t.Run("applications", func(t *testing.T) {
		requireTools(t, missingTools)
		for _, d := range in.byKind(v1alpha1.GroupName, "Application") {
			app := &v1alpha1.Application{}
			if err := decodeInto(d.obj, app); err != nil {
				t.Errorf("%s: %v", d, err)
				continue
			}
			claimName(t, app.Name, d.String())
			runner.process(t, ctx, app, d.String(), 0)
		}
	})

	scmFake := &scmRecorder{provider: githubURLs{newFakeSCM()}}
	plugin := &pluginService{}
	gen := appset.NewAPIGenerator(generators.Config{
		Client:    api.client,
		Namespace: controlPlaneNamespace,
		Repos:     localRepos{},
		SCM:       generators.SCMConfig{Enabled: true, NewProvider: scmFake.factory},
		Plugin:    generators.PluginConfig{HTTPClient: &http.Client{Transport: plugin}},
	})
	runner.generate = func(t *testing.T, as *v1alpha1.ApplicationSet, origin string) {
		t.Helper()
		apps, err := gen.Generate(ctx, as)
		if err != nil {
			t.Errorf("%s: %v", origin, err)
			return
		}
		var names []string
		for i := range apps {
			names = append(names, apps[i].Name)
		}
		sort.Strings(names)
		want, ok := expectedApplications[as.Name]
		if !ok {
			t.Errorf("%s: add the expected Applications of ApplicationSet %s to expectedApplications", origin, as.Name)
		} else if !slices.Equal(names, want) {
			t.Errorf("%s: generated %v, want %v", origin, names, want)
		}
		rep.add("ApplicationSets expanded", 1)
		rep.add("Applications generated", len(apps))
		for i := range apps {
			app := &apps[i]
			runner.claim(t, app.Name, origin)
			checkGenerated(t, as.Name, app)
			obj, err := toUnstructured(app)
			if err != nil {
				t.Fatal(err)
			}
			if err := api.dryRun(ctx, obj, controlPlaneNamespace); err != nil {
				t.Errorf("%s: generated Application %s: %v", origin, app.Name, err)
				continue
			}
			runner.process(t, ctx, app, origin+" → "+app.Name, 0)
		}
	}

	t.Run("applicationsets", func(t *testing.T) {
		requireTools(t, missingTools)
		seen := map[string]bool{}
		for _, d := range in.byKind(v1alpha1.GroupName, "ApplicationSet") {
			as := &v1alpha1.ApplicationSet{}
			if err := decodeInto(d.obj, as); err != nil {
				t.Errorf("%s: %v", d, err)
				continue
			}
			seen[as.Name] = true
			runner.generate(t, as, d.String())
		}
		for name := range expectedApplications {
			if !seen[name] {
				t.Errorf("expectedApplications lists %s, which is not in the examples", name)
			}
		}
		if plugin.calls == 0 {
			t.Error("the plugin generator example did not call the plugin service")
		}
		scmFake.mu.Lock()
		defer scmFake.mu.Unlock()
		if len(scmFake.requests) == 0 {
			t.Error("no SCM provider was built by the generators")
		}
		for _, req := range scmFake.requests {
			if req.Config.Token == nil && req.Config.GitHubApp == nil {
				t.Errorf("SCM provider built without credentials: %+v", req.Config)
			}
		}
		rep.add("SCM provider instances built (credentials resolved)", len(scmFake.requests))
	})

	t.Run("coverage", func(t *testing.T) {
		requireTools(t, missingTools)
		for _, f := range in.filesOf(classJsonnet) {
			if strings.HasSuffix(f.rel, ".jsonnet") && r.jsonnetFiles[f.rel] == 0 {
				t.Errorf("%s was not evaluated by any Application", f.rel)
			}
		}
		for _, f := range in.filesOf(classCMPTemplate) {
			if r.cmpDirs[path.Dir(f.rel)] == 0 {
				t.Errorf("%s was not rendered by any Application", f.rel)
			}
		}
		for _, f := range in.filesOf(classShell) {
			used := false
			for _, p := range r.plugins {
				for _, a := range append(slices.Clone(p.cfg.Spec.Generate.Command), p.cfg.Spec.Generate.Args...) {
					if strings.HasPrefix(a, pluginConfigDir) && path.Join(path.Dir(p.rel), strings.TrimPrefix(a, pluginConfigDir)) == f.rel {
						used = true
					}
				}
			}
			if !used {
				t.Errorf("%s is not the generate command of any plugin", f.rel)
			}
		}
		for name, n := range r.jsonnetFiles {
			rep.add("jsonnet evaluations", n)
			rep.note("jsonnet: %s evaluated %d times", name, n)
		}
		for dir, n := range r.cmpDirs {
			rep.add("plugin generate runs", n)
			rep.note("plugin: %s rendered %d times", dir, n)
		}
	})

	// Summary.
	keys := slices.Sorted(maps.Keys(rep.counts))
	for _, k := range keys {
		t.Logf("%-55s %d", k, rep.counts[k])
	}
	t.Logf("API server: %s", api.dryRunSummary())
	for _, l := range rep.lines {
		t.Log(l)
	}
	for _, w := range api.warnings.list() {
		t.Logf("API server warning: %s", w)
	}
	t.Logf("tool invocations: %d (helm: %s, kustomize: %s)", len(r.commands), tl.helm, tl.kustomize)
}

type kustomization struct {
	Kind       string   `json:"kind"`
	Components []string `json:"components"`
}

func readKustomization(dir string) (*kustomization, error) {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		var k kustomization
		if err := yaml.Unmarshal(data, &k); err != nil {
			return nil, err
		}
		return &k, nil
	}
	return nil, fmt.Errorf("no kustomization file in %s", dir)
}

func toUnstructured(obj any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("Application")
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

// checkGenerated asserts details the ApplicationSet examples document.
func checkGenerated(t *testing.T, appSet string, app *v1alpha1.Application) {
	t.Helper()
	param := func(name string) string {
		if app.Spec.Source == nil || app.Spec.Source.Helm == nil {
			return ""
		}
		for _, p := range app.Spec.Source.Helm.Parameters {
			if p.Name == name {
				return p.Value
			}
		}
		return ""
	}
	switch app.Name {
	case "guestbook-merged-prod-us":
		if got := param("replicaCount"); got != "3" {
			t.Errorf("%s: merge override not applied: replicaCount=%q, want 3", app.Name, got)
		}
	case "guestbook-merged-staging-eu":
		if got := param("replicaCount"); got != "1" {
			t.Errorf("%s: replicaCount=%q, want 1", app.Name, got)
		}
	case "guestbook-pr-42", "pr-42-guestbook":
		if app.Spec.Source == nil || app.Spec.Source.TargetRevision != shaPR42 {
			t.Errorf("%s (%s): targetRevision is not the pull request head commit", app.Name, appSet)
		}
		if app.Spec.Destination.Namespace != app.Name {
			t.Errorf("%s (%s): namespace %q, want one namespace per pull request named like the Application", app.Name, appSet, app.Spec.Destination.Namespace)
		}
		if appSet == "guestbook-previews" && !slices.Contains(app.Finalizers, v1alpha1.FinalizerResources) {
			t.Errorf("%s: preview Applications need %s for cleanup", app.Name, v1alpha1.FinalizerResources)
		}
	}
	if appSet == "guestbook-rollout" && app.Labels["env"] == "" {
		t.Errorf("%s: RollingSync steps select on the env label, which is missing", app.Name)
	}
}

// appRunner validates and renders Applications.
type appRunner struct {
	api      *apiServer
	renderer *renderer
	rep      *report
	projects map[string]*v1alpha1.AppProject
	clusters *project.ClusterIndex
	generate func(t *testing.T, as *v1alpha1.ApplicationSet, origin string)
	claim    func(t *testing.T, name, origin string)

	mu        sync.Mutex
	processed map[string]bool
}

const maxAppDepth = 3

func (r *appRunner) process(t *testing.T, ctx context.Context, app *v1alpha1.Application, origin string, depth int) {
	t.Helper()
	key, _ := json.Marshal(struct {
		Name string
		Spec v1alpha1.ApplicationSpec
	}{app.Name, app.Spec})
	r.mu.Lock()
	dup := r.processed[string(key)]
	r.processed[string(key)] = true
	r.mu.Unlock()
	if dup {
		r.rep.add("Applications skipped (identical to one already rendered)", 1)
		return
	}
	r.rep.add(fmt.Sprintf("Applications validated and rendered (depth %d)", depth), 1)

	proj := r.projects[app.Spec.GetProject()]
	for _, issue := range project.ValidateApplication(ctx, app, proj, r.clusters, project.ValidateOptions{ControlPlaneNamespace: controlPlaneNamespace}) {
		t.Errorf("%s: %s (%s): %s", origin, issue.Type, issue.Reason, issue.Message)
	}
	if proj == nil {
		return
	}

	var sources []v1alpha1.ApplicationSource
	switch {
	case app.Spec.SourceHydrator != nil:
		dry := app.Spec.SourceHydrator.DrySource
		sources = []v1alpha1.ApplicationSource{{RepoURL: dry.RepoURL, Path: dry.Path, TargetRevision: dry.TargetRevision}}
	case len(app.Spec.Sources) > 0:
		sources = app.Spec.Sources
	case app.Spec.Source != nil:
		sources = []v1alpha1.ApplicationSource{*app.Spec.Source}
	}
	refs := map[string]string{}
	for _, s := range sources {
		if s.Ref != "" {
			root, ok := exampleRepos[s.RepoURL]
			if !ok {
				t.Errorf("%s: ref source %s is not provided by the examples", origin, s.RepoURL)
				continue
			}
			refs["$"+s.Ref] = root
		}
	}
	for _, src := range sources {
		if src.Ref != "" && src.Path == "" && src.Chart == "" {
			continue // values-only source
		}
		res, err := r.renderer.renderSource(ctx, app, src, refs)
		if err != nil {
			t.Errorf("%s: %v", origin, err)
			continue
		}
		r.rep.add("sources rendered: "+res.tool, 1)
		if want, ok := expectedDirectoryFiles[app.Name]; ok && res.tool == string(v1alpha1.ApplicationSourceTypeDirectory) && !slices.Equal(res.files, want) {
			t.Errorf("%s: directory source selected %v, want %v", origin, res.files, want)
		}
		if len(res.objects) == 0 {
			t.Errorf("%s: %s source %s rendered no objects", origin, res.tool, res.dir)
		}
		// Namespaces first, as the sync does, so namespaced objects can follow.
		objs := slices.Clone(res.objects)
		slices.SortStableFunc(objs, func(a, b *unstructured.Unstructured) int {
			return boolInt(b.GetKind() == "Namespace") - boolInt(a.GetKind() == "Namespace")
		})
		for _, o := range objs {
			r.checkObject(t, ctx, app, proj, o, origin, depth)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r *appRunner) checkObject(t *testing.T, ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, o *unstructured.Unstructured, origin string, depth int) {
	t.Helper()
	where := fmt.Sprintf("%s: rendered %s %s", origin, o.GetKind(), o.GetName())
	gvk := o.GroupVersionKind()
	namespaced, err := r.api.namespaced(gvk)
	if err != nil {
		t.Errorf("%s: %v", where, err)
		return
	}
	if !project.IsGroupKindNamePermitted(proj, gvk.GroupKind(), o.GetName(), namespaced) {
		t.Errorf("%s: not permitted by AppProject %s", where, proj.Name)
	}
	if err := r.api.dryRun(ctx, o, app.Spec.Destination.Namespace); err != nil {
		t.Errorf("%s: %v", where, err)
		return
	}
	r.rep.add("rendered objects dry-run", 1)
	if gvk.Group != v1alpha1.GroupName {
		return
	}
	switch gvk.Kind {
	case "Application":
		if depth+1 > maxAppDepth {
			t.Errorf("%s: app of apps nested deeper than %d", where, maxAppDepth)
			return
		}
		child := &v1alpha1.Application{}
		if err := decodeInto(o, child); err != nil {
			t.Errorf("%s: %v", where, err)
			return
		}
		r.claim(t, child.Name, origin)
		r.process(t, ctx, child, origin+" → "+child.Name, depth+1)
	case "ApplicationSet":
		as := &v1alpha1.ApplicationSet{}
		if err := decodeInto(o, as); err != nil {
			t.Errorf("%s: %v", where, err)
			return
		}
		r.generate(t, as, origin+" → "+as.Name)
	}
}
