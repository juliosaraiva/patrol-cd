package helm_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// renderedConfigMap returns the data of the fixture ConfigMap in out.
func renderedConfigMap(t *testing.T, out []byte) map[string]string {
	t.Helper()
	for _, doc := range strings.Split(string(out), "\n---") {
		var obj struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse rendered doc: %v\n%s", err, doc)
		}
		if obj.Kind == "ConfigMap" {
			return obj.Data
		}
	}
	t.Fatalf("no ConfigMap rendered:\n%s", out)
	return nil
}

func templateClient(t *testing.T) *helm.Client {
	t.Helper()
	return &helm.Client{HelmPath: helmBinary(t), WorkDir: t.TempDir()}
}

func TestTemplateDefaults(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	out, cmd, err := c.TemplateWithCommand(context.Background(), chart, helm.TemplateOpts{ReleaseName: "demo", Namespace: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "helm template . --name-template demo --namespace ns1 --include-crds" {
		t.Fatalf("command = %q", cmd)
	}
	s := string(out)
	mustContain(t, s, "kind: CustomResourceDefinition")
	mustContain(t, s, "name: demo-cm")
	mustContain(t, s, "namespace: ns1")
	mustContain(t, s, "helm.sh/hook: test")
	data := renderedConfigMap(t, out)
	if data["message"] != "hello" || data["replicaCount"] != "1" || data["hasExample"] != "false" {
		t.Fatalf("data = %v", data)
	}
	if _, err := os.Stat(filepath.Join(chart, "Chart.lock")); !os.IsNotExist(err) {
		t.Fatal("Chart.lock left behind")
	}

	// Empty namespace: flag omitted, helm's default applies.
	_, cmd, err = c.TemplateWithCommand(context.Background(), chart, helm.TemplateOpts{ReleaseName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, cmd, "--namespace")
}

func TestTemplateFlags(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	ctx := context.Background()
	tests := []struct {
		name    string
		opts    helm.TemplateOpts
		has     []string
		hasNot  []string
		cmdHas  []string
		cmdNot  []string
		wantErr string
	}{
		{name: "skip crds", opts: helm.TemplateOpts{SkipCRDs: true}, hasNot: []string{"CustomResourceDefinition"}, cmdNot: []string{"--include-crds"}},
		{name: "include crds wins", opts: helm.TemplateOpts{SkipCRDs: true, IncludeCRDs: true}, has: []string{"CustomResourceDefinition"}},
		{name: "skip tests", opts: helm.TemplateOpts{SkipTests: true}, hasNot: []string{"helm.sh/hook: test"}, cmdHas: []string{"--skip-tests"}},
		{name: "kube version normalized", opts: helm.TemplateOpts{KubeVersion: "v1.30.11+IKS"}, has: []string{`kubeVersion: "v1.30.11"`}, cmdHas: []string{"--kube-version 1.30.11"}},
		{name: "api versions", opts: helm.TemplateOpts{APIVersions: []string{"example.com/v1", "apps/v1/Deployment"}}, has: []string{`hasExample: "true"`},
			cmdHas: []string{"--api-versions example.com/v1 --api-versions apps/v1/Deployment"}},
		{name: "version v3", opts: helm.TemplateOpts{Version: "v3"}, has: []string{"ConfigMap"}},
		{name: "version v4", opts: helm.TemplateOpts{Version: "v4"}, has: []string{"ConfigMap"}},
		{name: "version v2", opts: helm.TemplateOpts{Version: "v2"}, wantErr: "helm version 'v2' is not supported"},
		{name: "bad kube version", opts: helm.TemplateOpts{KubeVersion: "abc"}, wantErr: "could not parse kubernetes version abc"},
		{name: "schema violation", opts: helm.TemplateOpts{SetStringParams: []helm.KV{{Name: "replicaCount", Value: "2"}}}, wantErr: "values don't meet the specifications of the schema"},
		{name: "schema skipped", opts: helm.TemplateOpts{SetStringParams: []helm.KV{{Name: "replicaCount", Value: "2"}}, SkipSchemaValidation: true},
			has: []string{`replicaCount: "2"`}, cmdHas: []string{"--set-string replicaCount=2", "--skip-schema-validation"}},
		{name: "relative value file rejected", opts: helm.TemplateOpts{ValueFiles: []string{"values.yaml"}}, wantErr: "is not resolved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.opts.ReleaseName = "demo"
			out, cmd, err := c.TemplateWithCommand(ctx, chart, tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.has {
				mustContain(t, string(out), s)
			}
			for _, s := range tt.hasNot {
				mustNotContain(t, string(out), s)
			}
			for _, s := range tt.cmdHas {
				mustContain(t, cmd, s)
			}
			for _, s := range tt.cmdNot {
				mustNotContain(t, cmd, s)
			}
		})
	}
	if err := helm.CheckVersion("v2"); !errors.Is(err, helm.ErrUnsupportedVersion) {
		t.Fatalf("CheckVersion: %v", err)
	}
}

func TestTemplateValuePrecedence(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f1 := write("one.yaml", "message: file1\nimage:\n  tag: from-file1\n")
	f2 := write("two.yaml", "message: file2\n")
	cfg := write("config.txt", "line1,line2")
	base := helm.TemplateOpts{ReleaseName: "demo", ValueFiles: []string{f1, f2}}
	render := func(o helm.TemplateOpts) (map[string]string, string) {
		t.Helper()
		out, cmd, err := c.TemplateWithCommand(context.Background(), chart, o)
		if err != nil {
			t.Fatal(err)
		}
		return renderedConfigMap(t, out), cmd
	}

	data, _ := render(base)
	if data["message"] != "file2" || data["image"] != "nginx:from-file1" {
		t.Fatalf("value files: %v", data)
	}
	withValues := base
	withValues.Values = []byte("message: inline\n")
	data, cmd := render(withValues)
	if data["message"] != "inline" {
		t.Fatalf("inline values: %v", data)
	}
	mustContain(t, cmd, "--values "+f1+" --values "+f2+" --values "+helm.ExtraValuesPlaceholder)
	mustNotContain(t, cmd, c.WorkDir)

	withParams := withValues
	withParams.SetParams = []helm.KV{{Name: "message", Value: "first"}, {Name: "extra", Value: "a,b"}, {Name: "list", Value: "{x,y}"}, {Name: "message", Value: "param"}}
	withParams.SetFileParams = []helm.KV{{Name: "config", Value: cfg}}
	data, cmd = render(withParams)
	if data["message"] != "param" || data["extra"] != "a,b" || data["list"] != "x|y" || data["config"] != "line1,line2" {
		t.Fatalf("parameters: %v", data)
	}
	mustContain(t, cmd, `--set message=param --set extra=a\,b --set list={x,y} --set-file config=`+cfg)

	// --set makes "false" a bool, --set-string keeps the string.
	typed := helm.TemplateOpts{ReleaseName: "demo", SetParams: []helm.KV{{Name: "message", Value: "false"}}}
	data, _ = render(typed)
	if data["message"] != "false" {
		t.Fatalf("set bool: %v", data)
	}
}

func TestTemplateErrors(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	if err := os.WriteFile(filepath.Join(chart, "templates", "broken.yaml"), []byte("{{ fail \"boom\" }}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := c.Template(context.Background(), chart, helm.TemplateOpts{ReleaseName: "demo", APIVersions: []string{"a/v1", "b/v1"}})
	if err == nil {
		t.Fatal("broken chart rendered")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "failed to execute helm template command: failed running helm: `helm template . --name-template demo <api versions removed> --include-crds` failed exit status 1: ") {
		t.Fatalf("error = %q", msg)
	}
	mustContain(t, msg, "boom")
	mustNotContain(t, msg, "--api-versions")
	var ee *sandbox.ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("error does not unwrap to ExitError: %v", err)
	}
	if _, err := c.Template(context.Background(), "relative/path", helm.TemplateOpts{}); !errors.Is(err, helm.ErrInvalidArgument) {
		t.Fatalf("relative chart dir: %v", err)
	}
	if _, err := c.Template(context.Background(), filepath.Join(chart, "Chart.yaml"), helm.TemplateOpts{}); !errors.Is(err, helm.ErrInvalidArgument) {
		t.Fatalf("file as chart dir: %v", err)
	}
}

// fakeHelm writes an executable shell script used as HelmPath. The script body
// may reference $CAPTURE, a directory baked into the script (the helm
// environment is sanitized, so it cannot be passed through the environment).
func fakeHelm(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture")
	if err := os.MkdirAll(capture, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nCAPTURE=%q\n%s\n", capture, body)
	p := filepath.Join(dir, "helm")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, capture
}

func TestTemplateIsolationAndChartLock(t *testing.T) {
	t.Parallel()
	bin, capture := fakeHelm(t, `
n=$(date +%s%N 2>/dev/null)$$
env > "$CAPTURE/env.$n"
printf '%s\n' "$@" > "$CAPTURE/args.$n"
touch Chart.lock
echo "kind: ConfigMap"
`)
	c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
	chart := copyDir(t, fixtureChart)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Template(context.Background(), chart, helm.TemplateOpts{ReleaseName: "demo", Values: []byte("secretish: value\n")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := os.Stat(filepath.Join(chart, "Chart.lock")); !os.IsNotExist(err) {
		t.Fatal("Chart.lock created by helm template was not removed")
	}
	envs, _ := filepath.Glob(filepath.Join(capture, "env.*"))
	if len(envs) != 4 {
		t.Fatalf("captured %d environments", len(envs))
	}
	homes := map[string]bool{}
	for _, f := range envs {
		data, _ := os.ReadFile(f)
		env := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			env[k] = v
		}
		home := env["HOME"]
		homes[home] = true
		if !strings.HasPrefix(home, filepath.Join(c.WorkDir, "work")+string(filepath.Separator)) {
			t.Fatalf("HOME %q not under the work dir", home)
		}
		for _, k := range []string{"HELM_CONFIG_HOME", "HELM_CACHE_HOME", "HELM_DATA_HOME", "HELM_REGISTRY_CONFIG",
			"HELM_REPOSITORY_CONFIG", "HELM_REPOSITORY_CACHE", "HELM_CONTENT_CACHE", "HELM_PLUGINS", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
			if !strings.HasPrefix(env[k], home+string(filepath.Separator)) {
				t.Fatalf("%s=%q not inside the call home %q", k, env[k], home)
			}
		}
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("helm home %q not removed", home)
		}
	}
	if len(homes) != 4 {
		t.Fatalf("concurrent renders shared helm homes: %v", homes)
	}

	// A pre-existing Chart.lock is kept.
	if err := os.WriteFile(filepath.Join(chart, "Chart.lock"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Template(context.Background(), chart, helm.TemplateOpts{ReleaseName: "demo"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(chart, "Chart.lock")); err != nil || string(b) != "keep" {
		t.Fatalf("pre-existing Chart.lock: %q, %v", b, err)
	}
}

func TestTemplateTimeout(t *testing.T) {
	t.Parallel()
	bin, _ := fakeHelm(t, "exec sleep 30")
	c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir(), Exec: sandbox.Limits{Timeout: 200 * time.Millisecond}}
	start := time.Now()
	_, err := c.Template(context.Background(), copyDir(t, fixtureChart), helm.TemplateOpts{ReleaseName: "demo"})
	if !errors.Is(err, sandbox.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestTemplateEndToEndFromRepository(t *testing.T) {
	t.Parallel()
	bin := helmBinary(t)
	repo := newChartRepo(t, true, "admin", "pw")
	repo.addChart("mychart", "0.5.0", helmPackage(t, bin, copyDir(t, fixtureChart), "0.5.0"), "")
	c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
	creds := helm.Creds{Username: "admin", Password: "pw", CAData: repo.caPEM()}
	ctx := context.Background()
	v, err := c.ResolveChartVersion(ctx, repo.URL(), "mychart", "0.*", creds, false)
	if err != nil || v != "0.5.0" {
		t.Fatalf("resolve: %q, %v", v, err)
	}
	dir, err := c.PullChart(ctx, repo.URL(), "mychart", v, creds, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	vf, _, err := helm.ResolveValueFilePathOrURL(dir, dir, "values.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Template(ctx, dir, helm.TemplateOpts{ReleaseName: "e2e", ValueFiles: []string{vf}, SetParams: []helm.KV{{Name: "message", Value: "from-repo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if data := renderedConfigMap(t, out); data["message"] != "from-repo" {
		t.Fatalf("data = %v", data)
	}
}

func TestTemplateRemoteValuesAndRlimits(t *testing.T) {
	t.Parallel()
	remote := newChartRepo(t, false, "", "")
	remote.setFile("/values.yaml", []byte("message: remote-values\n"))
	c := &helm.Client{
		HelmPath: helmBinary(t),
		WorkDir:  t.TempDir(),
		// Rlimits run helm through the sandbox re-exec helper (TestMain).
		Exec: sandbox.Limits{CPUSeconds: 120, MaxOpenFiles: 1024},
	}
	vf, remoteOK, err := helm.ResolveValueFilePathOrURL("/unused", "/", remote.URL()+"/values.yaml", helm.DefaultValuesFileSchemes)
	if err != nil || !remoteOK {
		t.Fatalf("resolve remote: %v", err)
	}
	out, err := c.Template(context.Background(), copyDir(t, fixtureChart), helm.TemplateOpts{ReleaseName: "demo", ValueFiles: []string{vf}})
	if err != nil {
		t.Fatal(err)
	}
	if data := renderedConfigMap(t, out); data["message"] != "remote-values" {
		t.Fatalf("data = %v", data)
	}
}
