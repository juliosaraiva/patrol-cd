package render

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeHelperMain()
	jsonnet.MaybeJsonnetMain()
	os.Exit(m.Run())
}

// repoRoot returns the module root (for bin/kustomize).
func moduleRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// helmBinary returns the helm binary for tests (PATROL_TEST_HELM, /opt/homebrew/bin/helm,
// or helm on PATH) or skips.
func helmBinary(t testing.TB) string {
	t.Helper()
	for _, p := range []string{os.Getenv("PATROL_TEST_HELM"), "/opt/homebrew/bin/helm"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	t.Skip("helm binary not available")
	return ""
}

// kustomizeBinary returns bin/kustomize (or PATROL_TEST_KUSTOMIZE) or skips.
func kustomizeBinaryPath(t testing.TB) string {
	t.Helper()
	for _, p := range []string{os.Getenv("PATROL_TEST_KUSTOMIZE"), filepath.Join(moduleRoot(t), "bin", "kustomize")} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("kustomize"); err == nil {
		return p
	}
	t.Skip("kustomize binary not available")
	return ""
}

func gitBinary(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return p
}

// testToolPATH is a PATH with the directories of the test tools and the system dirs.
func testToolPATH(dirs ...string) string {
	all := append(slices.Clone(dirs), "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	var out []string
	for _, d := range all {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

type rendererOpt func(*Config)

// newTestRenderer returns a Renderer with real tools where available. Helm is
// configured lazily by withHelm.
func newTestRenderer(t testing.TB, opts ...rendererOpt) *Renderer {
	t.Helper()
	cfg := Config{
		WorkDir:          t.TempDir(),
		PluginSocketDir:  filepath.Join(t.TempDir(), "no-plugins"),
		CMPRetryBackoff:  10 * time.Millisecond,
		CMPRetryAttempts: 2,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Jsonnet:          jsonnet.Runner{Limits: sandbox.Limits{Timeout: 60 * time.Second}},
	}
	if p, err := exec.LookPath("git"); err == nil {
		cfg.GitPath = p
		cfg.ToolPATH = testToolPATH(filepath.Dir(p))
	} else {
		cfg.ToolPATH = testToolPATH()
	}
	for _, o := range opts {
		o(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func withHelm(t testing.TB) rendererOpt {
	bin := helmBinary(t)
	return func(c *Config) {
		c.Helm = &helm.Client{HelmPath: bin, WorkDir: t.TempDir()}
		c.HelmPath = bin
		c.ToolPATH = testToolPATH(append(filepath.SplitList(c.ToolPATH), filepath.Dir(bin))...)
	}
}

func withKustomize(t testing.TB) rendererOpt {
	bin := kustomizeBinaryPath(t)
	return func(c *Config) {
		c.KustomizePath = bin
		c.ToolPATH = testToolPATH(append(filepath.SplitList(c.ToolPATH), filepath.Dir(bin))...)
	}
}

// writeTree creates files under dir. Keys are slash-separated paths; a value
// starting with "->" creates a symlink to the rest.
func writeTree(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// tempRepo creates a repository tree and returns its absolute path.
func tempRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, files)
	return dir
}

// copyFixture copies testdata/<name> into a fresh repository directory.
func copyFixture(t testing.TB, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := filepath.Join(t.TempDir(), "repo")
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}

func generate(t testing.TB, r *Renderer, in Input) *Result {
	t.Helper()
	res, err := r.Generate(context.Background(), in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return res
}

func generateErr(t testing.TB, r *Renderer, in Input) error {
	t.Helper()
	res, err := r.Generate(context.Background(), in)
	if err == nil {
		t.Fatalf("Generate succeeded with %d manifests, want error", len(res.Manifests))
	}
	return err
}

// names returns "<kind>/<name>" of each object.
func names(objs []*unstructured.Unstructured) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.GetKind() + "/" + o.GetName()
	}
	return out
}

func findObj(t testing.TB, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("%s/%s not found in %v", kind, name, names(objs))
	return nil
}

func nested(t testing.TB, o *unstructured.Unstructured, fields ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(o.Object, fields...)
	if err != nil || !ok {
		t.Fatalf("field %v not found in %s: %v", fields, toJSON(o), err)
	}
	return v
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func cm(name string, data map[string]string) string {
	var b bytes.Buffer
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n")
	if len(data) > 0 {
		b.WriteString("data:\n")
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			b.WriteString("  " + k + ": " + strconvQuote(data[k]) + "\n")
		}
	}
	return b.String()
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func assertContains(t testing.TB, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q does not contain %q", s, sub)
	}
}

func assertEqual[T comparable](t testing.TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func assertSlice(t testing.TB, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
