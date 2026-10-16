package helm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeHelperMain()
	os.Exit(m.Run())
}

const fixtureChart = "testdata/charts/mychart"

// helmBinary returns the helm 4 binary used by tests that run helm, or skips.
func helmBinary(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("PATROL_TEST_HELM"); p != "" {
		return p
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	if _, err := os.Stat("/opt/homebrew/bin/helm"); err == nil {
		return "/opt/homebrew/bin/helm"
	}
	t.Skip("helm binary not available")
	return ""
}

func newClient(t testing.TB) *helm.Client {
	t.Helper()
	return &helm.Client{HelmPath: helmBinaryOrEmpty(), WorkDir: t.TempDir()}
}

func helmBinaryOrEmpty() string {
	if p := os.Getenv("PATROL_TEST_HELM"); p != "" {
		return p
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	if _, err := os.Stat("/opt/homebrew/bin/helm"); err == nil {
		return "/opt/homebrew/bin/helm"
	}
	return "helm"
}

var chartVersionLine = regexp.MustCompile(`(?m)^version:.*$`)

// packageChart builds a chart archive (<name>/... entries) from srcDir in Go,
// rewriting the Chart.yaml version. It does not need the helm binary.
func packageChart(t testing.TB, srcDir, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	var files []string
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, p := range files {
		rel, _ := filepath.Rel(srcDir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if rel == "Chart.yaml" {
			data = chartVersionLine.ReplaceAll(data, []byte("version: "+version))
		}
		hdr := &tar.Header{Name: name + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// helmPackage runs `helm package` on srcDir and returns the archive.
func helmPackage(t testing.TB, helmBin, srcDir, version string) []byte {
	t.Helper()
	out := t.TempDir()
	cmd := exec.Command(helmBin, "package", srcDir, "--version", version, "--destination", out)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm package: %v: %s", err, b)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 {
		t.Fatalf("helm package produced %v (%v)", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(out, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// copyDir copies a fixture tree into a fresh temp directory.
func copyDir(t testing.TB, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// chartRepo is an httptest Helm chart repository.
type chartRepo struct {
	srv        *httptest.Server
	user, pass string

	mu         sync.Mutex
	files      map[string][]byte
	entries    map[string][]helm.ChartVersion
	requests   map[string]int
	userAgents []string
	authSeen   map[string]bool // path -> request carried credentials
	noETag     bool
}

func newChartRepo(t testing.TB, useTLS bool, user, pass string) *chartRepo {
	t.Helper()
	r := &chartRepo{user: user, pass: pass, files: map[string][]byte{}, entries: map[string][]helm.ChartVersion{},
		requests: map[string]int{}, authSeen: map[string]bool{}}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	r.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if useTLS {
		r.srv.StartTLS()
	} else {
		r.srv.Start()
	}
	t.Cleanup(r.srv.Close)
	r.writeIndex()
	return r
}

func (r *chartRepo) URL() string { return r.srv.URL }

func (r *chartRepo) caPEM() []byte {
	if r.srv.Certificate() == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw})
}

// addChart publishes archive at /<name>-<version>.tgz (or urlOverride) and
// lists it in the index.
func (r *chartRepo) addChart(name, version string, archive []byte, urlOverride string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file := fmt.Sprintf("%s-%s.tgz", name, version)
	r.files["/"+file] = archive
	u := file
	if urlOverride != "" {
		u = urlOverride
	}
	r.entries[name] = append(r.entries[name], helm.ChartVersion{Name: name, Version: version, URLs: []string{u}})
	r.writeIndexLocked()
}

func (r *chartRepo) setFile(path string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = data
}

func (r *chartRepo) writeIndex() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeIndexLocked()
}

func (r *chartRepo) writeIndexLocked() {
	data, _ := yaml.Marshal(helm.Index{APIVersion: "v1", Entries: r.entries})
	r.files["/index.yaml"] = data
}

func (r *chartRepo) sawAuth(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authSeen[path]
}

func (r *chartRepo) agents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.userAgents...)
}

func (r *chartRepo) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[path]
}

func (r *chartRepo) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests[req.URL.Path]++
	r.userAgents = append(r.userAgents, req.Header.Get("User-Agent"))
	_, _, hasAuth := req.BasicAuth()
	r.authSeen[req.URL.Path] = hasAuth
	data, ok := r.files[req.URL.Path]
	noETag := r.noETag
	r.mu.Unlock()
	if r.user != "" {
		if u, p, ok := req.BasicAuth(); !ok || u != r.user || p != r.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="charts"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if !ok {
		http.NotFound(w, req)
		return
	}
	if !noETag {
		sum := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if req.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	_, _ = w.Write(data)
}

func treeFiles(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func mustContain(t testing.TB, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q in:\n%s", needle, haystack)
	}
}

func mustNotContain(t testing.TB, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("did not expect %q in:\n%s", needle, haystack)
	}
}
