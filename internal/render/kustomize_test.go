package render

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

// kustomizeInput renders testdata/kustomize/<path> copied into a fresh repository.
func kustomizeInput(t testing.TB, path string, k *v1alpha1.ApplicationSourceKustomize) Input {
	t.Helper()
	return Input{
		RootDir:     copyFixture(t, "kustomize"),
		AppName:     "test-app",
		Namespace:   "test-ns",
		ProjectName: "proj",
		Revision:    testRevision,
		Source:      v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/repo.git", Path: path, TargetRevision: "main", Kustomize: k},
	}
}

func deployment(t testing.TB, res *Result, name string) *unstructured.Unstructured {
	t.Helper()
	return findObj(t, res.Manifests, "Deployment", name)
}

func TestKustomizeBuild(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "overlay", nil)
	var res *Result
	assertTreeUnchanged(t, in.RootDir, func() { res = generate(t, r, in) })
	assertEqual(t, res.SourceType, "Kustomize")
	assertSlice(t, res.Commands, []string{"kustomize build ./overlay"})
	assertSlice(t, names(res.Manifests), []string{"ClusterRoleBinding/guestbook-binding", "Deployment/guestbook-ui"})
	d := deployment(t, res, "guestbook-ui")
	assertEqual(t, d.GetAnnotations()["patched-by"], "overlay")

	// kustomization.yml is detected too.
	in = kustomizeInput(t, "variants", nil)
	res = generate(t, r, in)
	assertEqual(t, res.SourceType, "Kustomize")
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/variant"})
}

func TestKustomizeAllOverrides(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "overlay", &v1alpha1.ApplicationSourceKustomize{
		NamePrefix:                "test-",
		NameSuffix:                "-sfx",
		Images:                    []v1alpha1.KustomizeImage{"gcr.io/heptio-images/ks-guestbook-demo=nginx:${ARGOCD_APP_REVISION_SHORT}"},
		Replicas:                  []v1alpha1.KustomizeReplica{{Name: "guestbook-ui", Count: intstr.FromString("3")}},
		CommonLabels:              map[string]string{"app.example/instance": "${ARGOCD_APP_NAME}"},
		CommonAnnotations:         map[string]string{"note": "${PATROL_APP_NAME}"},
		ForceCommonAnnotations:    true,
		CommonAnnotationsEnvsubst: true,
		Namespace:                 "ns1",
		Patches: v1alpha1.KustomizePatches{{
			Target: &v1alpha1.KustomizeSelector{KustomizeResID: v1alpha1.KustomizeResID{KustomizeGvk: v1alpha1.KustomizeGvk{Kind: "Deployment"}}},
			Patch:  `[{"op": "replace", "path": "/spec/template/spec/containers/0/ports/0/containerPort", "value": 443}]`,
		}},
		Components: []string{"../components/extra"},
	})
	var res *Result
	assertTreeUnchanged(t, in.RootDir, func() { res = generate(t, r, in) })
	assertSlice(t, res.Commands, []string{
		"kustomize edit set nameprefix -- test-",
		"kustomize edit set namesuffix -- -sfx",
		"kustomize edit set image gcr.io/heptio-images/ks-guestbook-demo=nginx:0123456",
		"kustomize edit set replicas guestbook-ui=3",
		"kustomize edit add label app.example/instance:test-app",
		"kustomize edit add annotation --force note:test-app",
		"kustomize edit set namespace -- ns1",
		PatchesCommandComment,
		"kustomize edit add component ../components/extra",
		"kustomize build ./overlay",
	})
	d := deployment(t, res, "test-guestbook-ui-sfx")
	assertEqual(t, d.GetNamespace(), "ns1")
	assertEqual(t, d.GetAnnotations()["note"], "test-app")
	assertEqual(t, d.GetAnnotations()["patched-by"], "overlay") // existing patches kept
	assertEqual(t, d.GetLabels()["app.example/instance"], "test-app")
	assertEqual(t, d.GetLabels()["component"], "extra")
	assertEqual(t, nested(t, d, "spec", "replicas").(int64), int64(3))
	containers := nested(t, d, "spec", "template", "spec", "containers").([]any)
	c0 := containers[0].(map[string]any)
	assertEqual(t, c0["image"].(string), "nginx:0123456")
	assertEqual(t, c0["ports"].([]any)[0].(map[string]any)["containerPort"].(int64), int64(443))
	selector := nested(t, d, "spec", "selector", "matchLabels").(map[string]any)
	assertEqual(t, selector["app.example/instance"].(string), "test-app") // labels reach selectors by default

	crb := findObj(t, res.Manifests, "ClusterRoleBinding", "test-guestbook-binding-sfx")
	subjects := nested(t, crb, "subjects").([]any)
	assertEqual(t, subjects[0].(map[string]any)["namespace"].(string), "ns1")
}

func TestKustomizeReplicas(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{Replicas: []v1alpha1.KustomizeReplica{{Name: "guestbook-ui", Count: intstr.FromInt32(1337)}}})
	res := generate(t, r, in)
	assertEqual(t, nested(t, deployment(t, res, "guestbook-ui"), "spec", "replicas").(int64), int64(1337))
	assertEqual(t, res.Commands[0], "kustomize edit set replicas guestbook-ui=1337")

	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{Replicas: []v1alpha1.KustomizeReplica{{Name: "guestbook-ui", Count: intstr.FromString("abc")}}})
	assertEqual(t, generateErr(t, r, in).Error(), "expected integer value for count. Received: abc")
}

func TestKustomizeLabels(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{
		CommonLabels:          map[string]string{"test": "label"},
		ForceCommonLabels:     true,
		LabelWithoutSelector:  true,
		LabelIncludeTemplates: true,
	})
	res := generate(t, r, in)
	assertEqual(t, res.Commands[0], "kustomize edit add label --force --without-selector --include-templates test:label")
	d := deployment(t, res, "guestbook-ui")
	assertEqual(t, d.GetLabels()["test"], "label")
	if _, ok := nested(t, d, "spec", "selector", "matchLabels").(map[string]any)["test"]; ok {
		t.Fatal("label applied to selector with labelWithoutSelector")
	}
	assertEqual(t, nested(t, d, "spec", "template", "metadata", "labels").(map[string]any)["test"].(string), "label")

	// labelWithoutSelector alone: not on templates either.
	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{CommonLabels: map[string]string{"test": "label"}, LabelWithoutSelector: true})
	d = deployment(t, generate(t, r, in), "guestbook-ui")
	if _, ok := nested(t, d, "spec", "template", "metadata", "labels").(map[string]any)["test"]; ok {
		t.Fatal("label applied to template without labelIncludeTemplates")
	}
}

func TestKustomizeAnnotations(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))

	// The overlay already sets "note": without force kustomize refuses.
	in := kustomizeInput(t, "overlay", &v1alpha1.ApplicationSourceKustomize{CommonAnnotations: map[string]string{"note": "x"}})
	err := generateErr(t, r, in)
	assertContains(t, err.Error(), "`kustomize edit add annotation note:x` failed exit status 1")
	if strings.Contains(err.Error(), "patrol-kustomize-") {
		t.Fatalf("error names the scratch copy: %v", err)
	}

	// Without envsubst the value is literal.
	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{CommonAnnotations: map[string]string{"app-source": "${ARGOCD_APP_NAME}"}})
	res := generate(t, r, in)
	assertEqual(t, deployment(t, res, "guestbook-ui").GetAnnotations()["app-source"], "${ARGOCD_APP_NAME}")

	in.Source.Kustomize.CommonAnnotationsEnvsubst = true
	in.AppName = "annotation-test-app"
	res = generate(t, r, in)
	assertEqual(t, deployment(t, res, "guestbook-ui").GetAnnotations()["app-source"], "annotation-test-app")
}

func TestKustomizePatchesWithoutKustomizationFile(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "restricted", &v1alpha1.ApplicationSourceKustomize{Patches: v1alpha1.KustomizePatches{{Patch: "[]"}}})
	assertEqual(t, generateErr(t, r, in).Error(), "kustomization file not found in the path")
}

func TestKustomizeComponents(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))

	in := kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{Components: []string{"../components/missing"}})
	assertContains(t, generateErr(t, r, in).Error(), "`kustomize edit add component ../components/missing` failed")

	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{
		Components:              []string{"../components/extra", "../components/missing", "../../../../outside"},
		IgnoreMissingComponents: true,
	})
	res := generate(t, r, in)
	assertSlice(t, res.Commands, []string{"kustomize edit add component ../components/extra", "kustomize build ./base"})
	assertEqual(t, deployment(t, res, "guestbook-ui").GetLabels()["component"], "extra")

	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{Components: []string{"../components/missing"}, IgnoreMissingComponents: true})
	assertSlice(t, generate(t, r, in).Commands, []string{"kustomize build ./base"})
}

func TestKustomizeVersionSelection(t *testing.T) {
	t.Parallel()
	bin := kustomizeBinaryPath(t)
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{Version: "v5.8.1"})
	in.KustomizeOptions.Versions = map[string]string{"v5.8.1": bin}
	res := generate(t, r, in)
	assertSlice(t, res.Commands, []string{bin + " build ./base"})

	in.Source.Kustomize.Version = "5.8.1" // compared literally
	assertEqual(t, generateErr(t, r, in).Error(), "error getting kustomize binary path: kustomize version 5.8.1 is not registered")
}

func TestKustomizeBuildOptions(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "restricted/app", nil)
	assertContains(t, generateErr(t, r, in).Error(), "`kustomize build")

	in.KustomizeOptions.BuildOptions = "--load-restrictor LoadRestrictionsNone"
	res := generate(t, r, in)
	assertSlice(t, res.Commands, []string{"kustomize build ./restricted/app --load-restrictor LoadRestrictionsNone"})
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/outside"})

	// Per-version build options are not applied (upstream behavior at the baseline).
	in.KustomizeOptions.BuildOptions = ""
	in.KustomizeOptions.VersionBuildOptions = map[string]string{"v5.8.1": "--load-restrictor LoadRestrictionsNone"}
	assertContains(t, generateErr(t, r, in).Error(), "`kustomize build")
}

func TestKustomizeEnableHelm(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t), withHelm(t))
	in := kustomizeInput(t, "helm", nil)
	in.KubeVersion = "v1.30.0+k3s1"
	in.APIVersions = []string{"example.com/v1", "v1"}
	in.KustomizeOptions.BuildOptions = "--enable-helm"
	var res *Result
	assertTreeUnchanged(t, in.RootDir, func() { res = generate(t, r, in) })
	assertSlice(t, res.Commands, []string{"kustomize build ./helm --enable-helm --helm-kube-version 1.30.0 --helm-api-versions example.com/v1 --helm-api-versions v1"})
	data := cmData(t, res, "rel-inflated")
	assertEqual(t, data["greeting"].(string), "hello")
	assertEqual(t, data["kubeVersion"].(string), "v1.30.0")
	assertEqual(t, data["hasWidgetAPI"].(string), "true")

	// Source values replace the cluster's.
	in.Source.Kustomize = &v1alpha1.ApplicationSourceKustomize{KubeVersion: "5.6.7", APIVersions: []string{"v1", "v2"}}
	res = generate(t, r, in)
	assertSlice(t, res.Commands, []string{"kustomize build ./helm --enable-helm --helm-kube-version 5.6.7 --helm-api-versions v1 --helm-api-versions v2"})

	// Without --enable-helm the fields are ignored.
	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{KubeVersion: "5.6.7", APIVersions: []string{"v1"}})
	in.KustomizeOptions.BuildOptions = "--load-restrictor LoadRestrictionsRootOnly"
	assertSlice(t, generate(t, r, in).Commands, []string{"kustomize build ./base --load-restrictor LoadRestrictionsRootOnly"})

	// An unparsable kubeVersion fails, as upstream.
	in.Source.Kustomize.KubeVersion = "abc"
	assertContains(t, generateErr(t, r, in).Error(), "could not parse kubernetes version abc")
}

func TestKustomizeBuildEnvironment(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "env", nil)
	in.KustomizeOptions.BuildOptions = "--enable-alpha-plugins --enable-exec"
	res := generate(t, r, in)
	data := cmData(t, res, "build-env")
	assertEqual(t, data["ARGOCD_APP_NAME"].(string), "test-app")
	assertEqual(t, data["ARGOCD_APP_NAMESPACE"].(string), "test-ns")
	assertEqual(t, data["ARGOCD_APP_REVISION_SHORT"].(string), "0123456")
	assertEqual(t, data["PATROL_APP_NAME"].(string), "test-app")
	assertEqual(t, data["PATROL_APP_SOURCE_PATH"].(string), "env")
	assertEqual(t, data["GIT_SSL_CAINFO_CONTENT"].(string), "")

	// HTTPS repositories with a configured CA bundle export GIT_SSL_CAINFO.
	withCA := newTestRenderer(t, withKustomize(t), func(c *Config) {
		c.CAData = func(host string) []byte {
			if host == "git.example.com" {
				return []byte("test-ca-bundle")
			}
			return nil
		}
	})
	in.Repo.URL = "https://git.example.com/repo.git"
	assertEqual(t, cmData(t, generate(t, withCA, in), "build-env")["GIT_SSL_CAINFO_CONTENT"].(string), "test-ca-bundle")
	in.Repo.URL = "git@git.example.com:repo.git"
	assertEqual(t, cmData(t, generate(t, withCA, in), "build-env")["GIT_SSL_CAINFO_CONTENT"].(string), "")
}

// TestKustomizeProxyEnvironment checks the proxy variables kustomize (and the git and
// helm processes it starts) sees: the repo-server's own without a repository proxy,
// else the repository proxy with no_proxy always set (upstream os.Environ() +
// proxy.UpsertEnv). Not parallel: it sets process environment variables.
func TestKustomizeProxyEnvironment(t *testing.T) {
	r := newTestRenderer(t, withKustomize(t))
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	const pod = "http://pod-proxy.example:3128"
	t.Setenv("HTTPS_PROXY", pod)
	t.Setenv("http_proxy", pod)
	t.Setenv("NO_PROXY", "pod.local")
	const repo = "http://repo-proxy.example:8080"
	tests := []struct {
		name, proxy, noProxy string
		want                 []string
	}{
		{"repo-server variables without a repository proxy", "", "", []string{
			"HTTPS_PROXY=" + pod, "NO_PROXY=pod.local", "http_proxy=" + pod,
		}},
		{"repository proxy with empty noProxy clears the pod NO_PROXY", repo, "", []string{
			"HTTPS_PROXY=" + repo, "HTTP_PROXY=" + repo, "NO_PROXY=", "http_proxy=" + repo, "https_proxy=" + repo, "no_proxy=",
		}},
		{"repository proxy and noProxy", repo, "internal.example", []string{
			"HTTPS_PROXY=" + repo, "HTTP_PROXY=" + repo, "NO_PROXY=internal.example", "http_proxy=" + repo, "https_proxy=" + repo, "no_proxy=internal.example",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := kustomizeInput(t, "env", nil)
			in.KustomizeOptions.BuildOptions = "--enable-alpha-plugins --enable-exec"
			in.Repo.Proxy, in.Repo.NoProxy = tt.proxy, tt.noProxy
			got := cmData(t, generate(t, r, in), "build-env")["PROXY_ENV"].(string)
			assertSlice(t, strings.Fields(got), tt.want)
		})
	}
}

func TestKustomizeInvalid(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "invalid", nil)
	err := generateErr(t, r, in)
	assertContains(t, err.Error(), "`kustomize build "+filepath.Join(in.RootDir, "invalid")+"` failed exit status 1")
	assertContains(t, err.Error(), "does-not-exist.yaml")

	// An explicit kustomize block on a directory without a kustomization fails in
	// kustomize.
	in = kustomizeInput(t, "restricted", &v1alpha1.ApplicationSourceKustomize{})
	assertContains(t, generateErr(t, r, in).Error(), "`kustomize build")
}

// gitRemoteBase creates a local git repository with base/ (a ConfigMap) tagged
// v1.0.0 and returns its file:// URL.
func gitRemoteBase(t *testing.T) string {
	t.Helper()
	gitPath := gitBinary(t)
	dir := tempRepo(t, map[string]string{
		"base/kustomization.yaml": "resources:\n- cm.yaml\n",
		"base/cm.yaml":            cm("remote-cm", nil),
	})
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "-m", "init"},
		{"tag", "v1.0.0"},
	} {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return "file://" + dir
}

func TestKustomizeRemoteBaseGitGuard(t *testing.T) {
	t.Parallel()
	remote := gitRemoteBase(t)
	r := newTestRenderer(t, withKustomize(t))
	marker := filepath.Join(t.TempDir(), "pwned")

	root := tempRepo(t, map[string]string{
		"ok/kustomization.yaml":   "resources:\n- " + remote + "//base?ref=v1.0.0\n",
		"evil/kustomization.yaml": "resources:\n- " + remote + "//base?ref=--upload-pack=touch%20" + marker + "\n",
	})
	res := generate(t, r, Input{RootDir: root, AppName: "a", Source: v1alpha1.ApplicationSource{Path: "ok"}})
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/remote-cm"})

	err := generateErr(t, r, Input{RootDir: root, AppName: "a", Source: v1alpha1.ApplicationSource{Path: "evil"}})
	assertContains(t, err.Error(), "`kustomize build")
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("injected git option was executed (marker stat: %v)", statErr)
	}
}

func TestKustomizeWrappers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A fake helm that prints its Helm home environment.
	fakeHelm := filepath.Join(dir, "helm")
	if err := os.WriteFile(fakeHelm, []byte("#!/bin/sh\necho \"$HELM_DATA_HOME|$HELM_CONFIG_HOME|$HELM_PLUGINS|$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake git that prints its arguments.
	fakeGit := filepath.Join(dir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestRenderer(t, func(c *Config) { c.HelmPath = fakeHelm; c.GitPath = fakeGit })
	scratch := t.TempDir()
	c := &call{log: r.log}
	bin, err := r.installWrappers(c, scratch, "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string, env []string, args ...string) (string, error) {
		res, err := sandbox.Run(context.Background(), sandbox.Cmd{Path: filepath.Join(bin, name), Args: args, Env: sandbox.BaseEnv(env...)})
		return strings.TrimSpace(string(res.Stdout)), err
	}
	// Kustomize (helmGlobals.configHome) points Helm at repository content; the
	// wrapper pins the Helm home to an empty per-call directory.
	out, err := run("helm", []string{"HELM_DATA_HOME=/repo/evil", "HELM_CONFIG_HOME=/repo/evil", "HELM_PLUGINS=/repo/evil/plugins"}, "template", "x")
	if err != nil {
		t.Fatal(err)
	}
	safe := filepath.Join(scratch, "helm-safe")
	assertEqual(t, out, safe+"|"+safe+"|"+safe+"/plugins|template x")
	entries, err := os.ReadDir(filepath.Join(safe, "plugins"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("plugins dir: %v %v", entries, err)
	}

	out, err = run("git", nil, "fetch", "--depth=1", "https://example.com/repo", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, out, "fetch --depth=1 --end-of-options https://example.com/repo v1")
	out, err = run("git", nil, "init")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, out, "init")
	_, err = run("git", nil, "fetch", "--upload-pack=x", "origin")
	var ee *sandbox.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.StderrTail, "git wrapper: unrecognized 'git fetch' invocation, refusing (see GHSA-9v9p-x54c-58gc)") {
		t.Fatalf("unexpected fetch shape not refused: %v", err)
	}

	// Paths that cannot be embedded in the scripts are refused.
	odd := filepath.Join(t.TempDir(), "it's")
	if err := os.Mkdir(odd, 0o755); err != nil {
		t.Fatal(err)
	}
	oddGit := filepath.Join(odd, "git")
	if err := os.WriteFile(oddGit, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r2 := newTestRenderer(t, func(c *Config) { c.GitPath = oddGit })
	if _, err := r2.installWrappers(c, t.TempDir(), "/usr/bin"); err == nil || !strings.Contains(err.Error(), "failed to install git wrapper for kustomize build") {
		t.Fatalf("error = %v", err)
	}
}

func TestEditAddArgs(t *testing.T) {
	t.Parallel()
	m := map[string]string{"b": "2", "a": "1"}
	assertSlice(t, editAddArgs(m, semver.MustParse("v5.0.0")), []string{"a:1", "b:2"})
	assertSlice(t, editAddArgs(m, semver.MustParse("v3.8.4")), []string{"a:1,b:2"})
}

func TestKustomizeScratchCopyLimits(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t, withKustomize(t))
	in := kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{NamePrefix: "p-"})
	in.Limits.MaxScratchCopyFiles = 3
	err := generateErr(t, r, in)
	if !errors.Is(err, ErrScratchCopyLimit) {
		t.Fatalf("error = %v, want ErrScratchCopyLimit", err)
	}
	in.Limits.MaxScratchCopyFiles = 0
	in.Limits.MaxScratchCopyBytes = 100
	if err := generateErr(t, r, in); !errors.Is(err, ErrScratchCopyLimit) {
		t.Fatalf("error = %v, want ErrScratchCopyLimit", err)
	}

	// An out-of-bounds symlink fails the copy (the checkout is otherwise checked by
	// the repository layer).
	in = kustomizeInput(t, "base", &v1alpha1.ApplicationSourceKustomize{NamePrefix: "p-"})
	if err := os.Symlink("/etc/passwd", filepath.Join(in.RootDir, "base", "link")); err != nil {
		t.Fatal(err)
	}
	if err := generateErr(t, r, in); !errors.Is(err, sandbox.ErrPathEscape) {
		t.Fatalf("error = %v, want sandbox.ErrPathEscape", err)
	}
}
