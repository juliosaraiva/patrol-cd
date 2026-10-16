package reposerver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

const testToken = "test-internal-token"

func moduleRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func gitBin(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return p
}

// helmBin returns the helm binary or "" when unavailable.
func helmBin() string {
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
	return ""
}

func requireHelm(t testing.TB) string {
	t.Helper()
	p := helmBin()
	if p == "" {
		t.Skip("helm not available")
	}
	return p
}

func kustomizeBin(t testing.TB) string {
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
	return ""
}

func requireKustomize(t testing.TB) string {
	t.Helper()
	p := kustomizeBin(t)
	if p == "" {
		t.Skip("kustomize not available")
	}
	return p
}

func gpgBin(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not available")
	}
	return p
}

// shortTempDir returns a short temporary directory (unix socket and gpg-agent paths
// are length limited).
func shortTempDir(t testing.TB, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func toolPATH(t testing.TB) string {
	var dirs []string
	add := func(p string) {
		if p != "" && !slices.Contains(dirs, filepath.Dir(p)) {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	if p, err := exec.LookPath("git"); err == nil {
		add(p)
	}
	add(helmBin())
	add(kustomizeBin(t))
	if p, err := exec.LookPath("gpg"); err == nil {
		add(p)
	}
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

type svcOpt func(*Config)

// newTestService returns a Service on a private root with real tools where available.
func newTestService(t testing.TB, opts ...svcOpt) *Service {
	t.Helper()
	gitBin(t)
	empty := t.TempDir()
	cfg := Config{
		Token:                  testToken,
		RootDir:                filepath.Join(t.TempDir(), "root"),
		AllowLocalRepositories: true,
		Registerer:             prometheus.NewRegistry(),
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		PluginSocketDir:        filepath.Join(empty, "no-plugins"),
		ToolPATH:               toolPATH(t),
		Jsonnet:                jsonnet.Runner{Limits: sandbox.Limits{Timeout: 60 * time.Second}},
		SSHKnownHostsPath:      filepath.Join(empty, "known_hosts"),
		TLSCertsDir:            filepath.Join(empty, "tls"),
		GPGKeysDir:             filepath.Join(empty, "gpg"),
		GnuPGHomeParent:        shortTempDir(t, "pgh"),
		ExecTimeout:            60 * time.Second,
	}
	if p, err := exec.LookPath("git"); err == nil {
		cfg.GitPath = p
	}
	if p := helmBin(); p != "" {
		cfg.HelmPath = p
	}
	if p := kustomizeBin(t); p != "" {
		cfg.KustomizePath = p
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = s.Close()
		_ = removeTree(s.root)
	})
	return s
}

// startServer serves s over HTTP and returns a real client.
func startServer(t testing.TB, s *Service, opts ...apiclient.HTTPClientOption) *apiclient.HTTPClient {
	t.Helper()
	h, err := s.Handler()
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := apiclient.NewHTTPClient(srv.URL, testToken, nil, opts...)
	require.NoError(t, err)
	return c
}

// remote is a bare repository plus a work tree used to author commits.
type remote struct {
	t    testing.TB
	bare string
	work string
	env  []string
}

func fixtureEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Fixture Author",
		"GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_COMMITTER_NAME=Fixture Committer",
		"GIT_COMMITTER_EMAIL=committer@example.com",
		"LC_ALL=C",
	}
}

func runGit(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func newRemote(t testing.TB) *remote {
	t.Helper()
	dir := t.TempDir()
	r := &remote{t: t, bare: filepath.Join(dir, "remote.git"), work: filepath.Join(dir, "work"), env: fixtureEnv(dir)}
	runGit(t, dir, r.env, "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	runGit(t, dir, r.env, "init", "--quiet", "--initial-branch=main", r.work)
	r.git("remote", "add", "origin", r.bare)
	return r
}

func (r *remote) url() string { return "file://" + r.bare }

func (r *remote) repo() apiclient.Repository { return apiclient.Repository{URL: r.url()} }

func (r *remote) git(args ...string) string {
	r.t.Helper()
	return runGit(r.t, r.work, r.env, args...)
}

// commit writes files (empty content deletes; "->target" makes a symlink), commits and
// pushes the current branch, returning the SHA.
func (r *remote) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for _, rel := range sortedKeys(files) {
		p := filepath.Join(r.work, filepath.FromSlash(rel))
		content := files[rel]
		require.NoError(r.t, os.RemoveAll(p))
		if content == "" {
			continue
		}
		require.NoError(r.t, os.MkdirAll(filepath.Dir(p), 0o755))
		if target, ok := strings.CutPrefix(content, "->"); ok {
			require.NoError(r.t, os.Symlink(target, p))
			continue
		}
		require.NoError(r.t, os.WriteFile(p, []byte(content), 0o644))
	}
	r.git("add", "--all")
	r.git("commit", "--quiet", "--allow-empty", "-m", msg)
	branch := r.git("rev-parse", "--abbrev-ref", "HEAD")
	r.git("push", "--quiet", "--force", "origin", branch)
	return r.git("rev-parse", "HEAD")
}

func configMap(name string, data map[string]string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n")
	if len(data) > 0 {
		b.WriteString("data:\n")
		for _, k := range sortedKeys(data) {
			v, _ := json.Marshal(data[k])
			b.WriteString("  " + k + ": " + string(v) + "\n")
		}
	}
	return b.String()
}

func deployment(name, image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
spec:
  selector:
    matchLabels:
      app: ` + name + `
  template:
    metadata:
      labels:
        app: ` + name + `
    spec:
      initContainers:
      - name: init
        image: busybox:1.36
      containers:
      - name: main
        image: ` + image + `
`
}

// objects decodes response manifests.
func objects(t testing.TB, resp *apiclient.ManifestResponse) []*unstructured.Unstructured {
	t.Helper()
	objs, err := manifestsOf(resp)
	require.NoError(t, err)
	return objs
}

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

// tarGz builds a gzipped tar from entries. Entry values: regular file content,
// "->target" for a symlink, "=>target" for a hard link; names ending with "/" are
// directories.
func tarGz(t testing.TB, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		name, content := e[0], e[1]
		hdr := &tar.Header{Name: name, Mode: 0o644, ModTime: time.Unix(0, 0)}
		switch {
		case strings.HasSuffix(name, "/"):
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		case strings.HasPrefix(content, "->"):
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, content[2:]
		case strings.HasPrefix(content, "=>"):
			hdr.Typeflag, hdr.Linkname = tar.TypeLink, content[2:]
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(content))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(content))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// counter reads a counter vector value.
func counter(c *prometheus.CounterVec, labels ...string) float64 {
	return testutil.ToFloat64(c.WithLabelValues(labels...))
}

func bg() context.Context { return context.Background() }
