package cmp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeHelperMain()
	os.Exit(m.Run())
}

const testRevision = "0123456789abcdef0123456789abcdef01234567"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// socketDir returns a short temporary directory for unix sockets (macOS limits socket
// paths to 104 bytes, which t.TempDir() can exceed).
func socketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mustParse(t testing.TB, yamlText string) *PluginConfig {
	t.Helper()
	cfg, err := ParsePluginConfig([]byte(yamlText))
	if err != nil {
		t.Fatalf("ParsePluginConfig: %v", err)
	}
	return cfg
}

// testServer is a Server listening on a real unix socket.
type testServer struct {
	*Server
	dir string // socket directory
}

// startServer starts a server for pluginYAML in dir (a new socket directory when
// empty) and stops it at cleanup.
func startServer(t testing.TB, dir, pluginYAML string, mutate ...func(*Config)) *testServer {
	t.Helper()
	if dir == "" {
		dir = socketDir(t)
	}
	cfg := Config{
		Plugin:    mustParse(t, pluginYAML),
		SocketDir: dir,
		WorkDir:   t.TempDir(),
		Environ:   []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()},
		Logger:    discardLogger(),
		KillGrace: -1,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ListenAndServe: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not stop")
		}
	})
	waitForSocket(t, s.SocketPath(), done)
	return &testServer{Server: s, dir: dir}
}

func waitForSocket(t testing.TB, p string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server stopped before listening: %v", err)
		default:
		}
		if c, err := net.Dial("unix", p); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never became ready", p)
}

// client returns an HTTP client of the server socket.
func (s *testServer) client() *http.Client {
	p := s.SocketPath()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p)
		},
		DisableCompression: true,
	}}
}

// post sends body to endpoint with the stream content type and returns status and
// decoded body (cmpwire.Error for failures).
func (s *testServer) post(t testing.TB, endpoint string, body []byte, header http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://cmp"+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", cmpwire.ContentTypeStream)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := s.client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// tryPost is post for goroutines other than the test's.
func (s *testServer) tryPost(endpoint string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, "http://cmp"+endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", cmpwire.ContentTypeStream)
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func decodeError(t testing.TB, data []byte) cmpwire.Error {
	t.Helper()
	var e cmpwire.Error
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("decode error body %q: %v", data, err)
	}
	return e
}

// writeTree writes files (slash paths; a value prefixed with "#!" is executable)
// under a new directory and returns it.
func writeTree(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// tarGzTree archives files as the repo-server does.
func tarGzTree(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := sandbox.Tar(writeTree(t, files), &buf, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	body     []byte
	size     int64 // when body is nil: a zero-filled body of this size
	mode     int64
}

// rawTarGz builds an arbitrary (possibly malicious) archive.
func rawTarGz(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.linkname, Mode: e.mode, Format: tar.FormatPAX}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
			if e.body == nil {
				h.Size = e.size
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			var r io.Reader = bytes.NewReader(e.body)
			if e.body == nil {
				r = io.LimitReader(zeroReader{}, e.size)
			}
			if _, err := io.Copy(tw, r); err != nil {
				t.Fatal(err)
			}
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

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// streamBody frames archive with metadata for appRelPath and env.
func streamBody(t testing.TB, archive []byte, appRelPath string, env ...string) []byte {
	t.Helper()
	frame, err := cmpwire.EncodeMetadata(cmpwire.Metadata{
		AppName:    path.Base(appRelPath),
		AppRelPath: appRelPath,
		Checksum:   sha(archive),
		Size:       int64(len(archive)),
		Env:        cmpwire.EnvEntries(env),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(frame, archive...)
}

// newRenderer returns the repo-server's CMP client for the socket directory.
func newRenderer(t testing.TB, dir string) *render.Renderer {
	t.Helper()
	r, err := render.New(render.Config{
		WorkDir:          t.TempDir(),
		PluginSocketDir:  dir,
		CMPRetryAttempts: 1,
		CMPRetryBackoff:  10 * time.Millisecond,
		Logger:           discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// renderInput is a plugin-source render input for a repository of files with the
// application in "app".
func renderInput(t testing.TB, files map[string]string, plugin *v1alpha1.ApplicationSourcePlugin) render.Input {
	t.Helper()
	return render.Input{
		RootDir:     writeTree(t, files),
		AppName:     "guestbook",
		Namespace:   "team-ns",
		ProjectName: "proj",
		Revision:    testRevision,
		KubeVersion: "v1.30.4",
		APIVersions: []string{"v1", "apps/v1"},
		Repo:        apiclient.Repository{URL: "https://git.example.com/repo.git", Username: "user", Password: "secret-password"},
		Source:      v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/repo.git", Path: "app", TargetRevision: "main", Plugin: plugin},
	}
}

func assertContains(t testing.TB, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%q does not contain %q", got, want)
	}
}

// workDirEntries lists the per-request directories left in the work directory.
func workDirEntries(t testing.TB, s *Server) []string {
	t.Helper()
	entries, err := os.ReadDir(s.WorkDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// waitNoEntries waits until the work directory is empty (cleanup runs after the
// response is written).
func waitNoEntries(t testing.TB, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := workDirEntries(t, s)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("work directory not cleaned: %v", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
