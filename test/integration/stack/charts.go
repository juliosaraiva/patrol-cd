package stack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
)

// helmCommand runs the real helm binary with an isolated home (no repositories, no plugins).
func helmCommand(t testing.TB, dir string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	home := t.TempDir()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"HELM_CACHE_HOME=" + filepath.Join(home, "cache"), "HELM_CONFIG_HOME=" + filepath.Join(home, "config"),
		"HELM_DATA_HOME=" + filepath.Join(home, "data"), "HELM_PLUGINS=" + filepath.Join(home, "plugins"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// PackageChart writes a chart (paths relative to the chart directory → content; Chart.yaml
// must name the chart <name> at <version>) and packages it with `helm package`. It returns
// the .tgz bytes.
func PackageChart(t testing.TB, name string, files map[string]string) []byte {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	for p, content := range files {
		full := filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	helmCommand(t, out, "package", src, "--destination", out)
	matches, err := filepath.Glob(filepath.Join(out, "*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("helm package produced %v (%v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ChartRepo is a Helm HTTP chart repository (index.yaml and chart archives) served by
// httptest; the index is generated with `helm repo index`.
type ChartRepo struct {
	// URL is the repository URL (http://127.0.0.1:<port>).
	URL      string
	dir      string
	requests atomic.Int64
}

// NewChartRepo starts an empty chart repository.
func NewChartRepo(t testing.TB) *ChartRepo {
	t.Helper()
	r := &ChartRepo{dir: t.TempDir()}
	fs := http.FileServer(http.Dir(r.dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests.Add(1)
		fs.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.URL = srv.URL
	helmCommand(t, r.dir, "repo", "index", ".", "--url", r.URL)
	return r
}

// AddChart adds a packaged chart and regenerates the index.
func (r *ChartRepo) AddChart(t testing.TB, name, version string, tgz []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name+"-"+version+".tgz"), tgz, 0o600); err != nil {
		t.Fatal(err)
	}
	helmCommand(t, r.dir, "repo", "index", ".", "--url", r.URL)
}

// Requests is the number of HTTP requests served.
func (r *ChartRepo) Requests() int64 { return r.requests.Load() }

// NewOCIRegistry starts an in-process OCI registry (internal/oci/ocitest, plain HTTP).
func NewOCIRegistry(t testing.TB) *ocitest.Registry {
	t.Helper()
	return ocitest.New(t, ocitest.Options{})
}

// TarGz builds a deterministic gzipped tar of files (path → content), the content layer of
// an OCI artifact source.
func TarGz(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	mod := time.Unix(1700000000, 0)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), ModTime: mod, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
