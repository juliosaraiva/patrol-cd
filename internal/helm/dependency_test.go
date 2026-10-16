package helm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
)

// parentChart writes a chart depending on mychart 0.1.0 from repository.
func parentChart(t *testing.T, repository string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	chartYAML := "apiVersion: v2\nname: parent\nversion: 1.0.0\ndependencies:\n  - name: mychart\n    version: \"0.1.0\"\n    repository: \"" + repository + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDependencyBuildHTTP(t *testing.T) {
	t.Parallel()
	bin := helmBinary(t)
	repo := newChartRepo(t, true, "admin", "pw")
	repo.addChart("mychart", "0.1.0", helmPackage(t, bin, copyDir(t, fixtureChart), "0.1.0"), "")
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		repository string
		repos      []helm.RepoCreds
	}{
		{name: "url", repository: repo.URL(), repos: []helm.RepoCreds{{URL: repo.URL(), Creds: helm.Creds{Username: "admin", Password: "pw", CAData: repo.caPEM()}}}},
		{name: "named", repository: "@stable", repos: []helm.RepoCreds{{Name: "stable", URL: repo.URL(), Creds: helm.Creds{Username: "admin", Password: "pw", CAData: repo.caPEM()}}}},
		{name: "alias", repository: "alias:stable", repos: []helm.RepoCreds{{Name: "stable", URL: repo.URL(), Creds: helm.Creds{Username: "admin", Password: "pw", InsecureSkipVerify: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
			chart := parentChart(t, tc.repository)
			_, err := c.Template(ctx, chart, helm.TemplateOpts{ReleaseName: "demo"})
			if !helm.IsMissingDependencyErr(err) {
				t.Fatalf("expected a missing dependency error, got %v", err)
			}
			if err := c.DependencyBuild(ctx, chart, tc.repos); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(chart, "charts", "mychart-0.1.0.tgz")); err != nil {
				t.Fatalf("dependency not vendored: %v", err)
			}
			out, err := c.Template(ctx, chart, helm.TemplateOpts{ReleaseName: "demo"})
			if err != nil {
				t.Fatal(err)
			}
			mustContain(t, string(out), "name: demo-cm")
		})
	}

	t.Run("missing credentials", func(t *testing.T) {
		t.Parallel()
		c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
		chart := parentChart(t, repo.URL())
		err := c.DependencyBuild(ctx, chart, []helm.RepoCreds{{URL: repo.URL(), Creds: helm.Creds{CAData: repo.caPEM()}}})
		if err == nil || !strings.HasPrefix(err.Error(), "failed to build helm dependencies: failed running helm: `helm dependency build` failed exit status 1") {
			t.Fatalf("unexpected error %v", err)
		}
	})
}

func TestDependencyBuildOCI(t *testing.T) {
	t.Parallel()
	bin := helmBinary(t)
	reg := ocitest.New(t, ocitest.Options{Username: "robot", Password: "oci-pw"})
	reg.PushHelmChart("charts/mychart", "0.1.0", helmPackage(t, bin, copyDir(t, fixtureChart), "0.1.0"))
	c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
	chart := parentChart(t, "oci://"+reg.Host()+"/charts")
	repos := []helm.RepoCreds{{URL: reg.Host() + "/charts", EnableOCI: true, Creds: helm.Creds{Username: "robot", Password: "oci-pw", PlainHTTP: true}}}
	ctx := context.Background()
	if err := c.EnsureDependencies(ctx, chart, repos, helm.DependencyOpts{}); err != nil {
		t.Fatal(err)
	}
	out, err := c.Template(ctx, chart, helm.TemplateOpts{ReleaseName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "name: demo-cm")
	// The marker makes the second call a no-op.
	before := reg.Requests("manifest")
	if err := c.EnsureDependencies(ctx, chart, repos, helm.DependencyOpts{}); err != nil {
		t.Fatal(err)
	}
	if reg.Requests("manifest") != before {
		t.Fatal("EnsureDependencies rebuilt despite the marker")
	}
	if _, err := os.Stat(filepath.Join(chart, helm.DependencyMarkerFile)); err != nil {
		t.Fatal(err)
	}

	// Without credentials the registry refuses the pull.
	anon := parentChart(t, "oci://"+reg.Host()+"/charts")
	err = c.EnsureDependencies(ctx, anon, []helm.RepoCreds{{URL: reg.Host() + "/charts", EnableOCI: true, Creds: helm.Creds{PlainHTTP: true}}}, helm.DependencyOpts{})
	if err == nil || !strings.HasPrefix(err.Error(), "error building helm chart dependencies: failed to build helm dependencies: ") {
		t.Fatalf("anonymous build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(anon, helm.DependencyMarkerFile)); !os.IsNotExist(err) {
		t.Fatal("marker written after a failed build")
	}
}

func TestDependencyBuildCredentialFiles(t *testing.T) {
	t.Parallel()
	bin, capture := fakeHelm(t, `
printf '%s\n' "$@" > "$CAPTURE/args"
env > "$CAPTURE/env"
cp -p "$HELM_REPOSITORY_CONFIG" "$CAPTURE/repositories.yaml"
cp -p "$HELM_REGISTRY_CONFIG" "$CAPTURE/registry.json"
for f in "$HOME"/ca-* "$HOME"/cert-* "$HOME"/key-*; do cp -p "$f" "$CAPTURE/"; done
`)
	c := &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
	chart := parentChart(t, "https://charts.example.com")
	repos := []helm.RepoCreds{
		{Name: "stable", URL: "https://charts.example.com", Creds: helm.Creds{Username: "http-user", Password: "http-secret", CAData: []byte("CA"), CertData: []byte("CERT"), KeyData: []byte("KEY"), PassCredentials: true}},
		{Name: "stable", URL: "https://other.example.com", Creds: helm.Creds{Username: "u2", Password: "p2"}},
		{URL: "oci://registry.example.com:5000/charts", EnableOCI: true, Creds: helm.Creds{Username: "oci-user", Password: "oci-secret", PlainHTTP: true}},
		{URL: "registry.example.com/half", EnableOCI: true, Creds: helm.Creds{Username: "only-user"}},
	}
	if err := c.DependencyBuildWithOptions(context.Background(), chart, repos, helm.DependencyOpts{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(capture, "args"))
	if got := strings.Fields(string(args)); !reflect.DeepEqual(got, []string{"dependency", "build", "--insecure-skip-tls-verify", "--plain-http"}) {
		t.Fatalf("args = %q", got)
	}
	env, _ := os.ReadFile(filepath.Join(capture, "env"))
	for _, secret := range []string{"http-secret", "oci-secret", "p2"} {
		mustNotContain(t, string(args), secret)
		mustNotContain(t, string(env), secret)
	}
	for _, f := range []string{"repositories.yaml", "registry.json"} {
		fi, err := os.Stat(filepath.Join(capture, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", f, fi.Mode().Perm())
		}
	}
	repoYAML, _ := os.ReadFile(filepath.Join(capture, "repositories.yaml"))
	for _, s := range []string{"name: stable", "url: https://charts.example.com", "username: http-user", "password: http-secret",
		"pass_credentials_all: true", "name: stable-2", "url: https://other.example.com"} {
		mustContain(t, string(repoYAML), s)
	}
	mustNotContain(t, string(repoYAML), "oci-secret")
	regJSON, _ := os.ReadFile(filepath.Join(capture, "registry.json"))
	mustContain(t, string(regJSON), "registry.example.com:5000")
	mustNotContain(t, string(regJSON), "registry.example.com/half")
	mustNotContain(t, string(regJSON), "only-user")
	for _, pattern := range []string{"ca-*", "cert-*", "key-*"} {
		m, _ := filepath.Glob(filepath.Join(capture, pattern))
		if len(m) != 1 {
			t.Fatalf("%s files: %v", pattern, m)
		}
		if fi, _ := os.Stat(m[0]); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", m[0], fi.Mode().Perm())
		}
	}
	// The call home (and every credential file) is gone.
	if entries, _ := os.ReadDir(filepath.Join(c.WorkDir, "work")); len(entries) != 0 {
		t.Fatalf("helm home left behind: %v", entries)
	}
}

func TestDependencyRepos(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	chartYAML := `apiVersion: v2
name: deps
version: 1.0.0
dependencies:
  - name: a
    repository: "@named"
  - name: b
    repository: alias:aliased
  - name: c
    repository: https://charts.example.com/stable
  - name: d
    repository: oci://registry.example.com/charts
  - name: e
    repository: http://insecure.example.com
  - name: f
    repository: file://../local
  - name: g
`
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := helm.DependencyRepos(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []helm.DependencyRepo{
		{Name: "named"},
		{Name: "aliased"},
		{Name: "https:--charts.example.com-stable", URL: "https://charts.example.com/stable"},
		{Name: "oci:--registry.example.com-charts", URL: "registry.example.com/charts", EnableOCI: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DependencyRepos = %+v", got)
	}
	// Chart.yaml escaping the chart directory is refused.
	escape := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "Chart.yaml"), filepath.Join(escape, "Chart.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := helm.DependencyRepos(escape); !errors.Is(err, helm.ErrPathOutsideRoot) {
		t.Fatalf("symlinked Chart.yaml: %v", err)
	}
	if helm.IsMissingDependencyErr(nil) || !helm.IsMissingDependencyErr(errors.New("x: found in Chart.yaml, but missing in charts/ directory: y")) {
		t.Fatal("IsMissingDependencyErr")
	}
}
