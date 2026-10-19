package deploy_test

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitServer serves bare repositories over git smart HTTP (git http-backend) with basic
// authentication; the repo-server under test clones from it like from any remote.
type gitServer struct {
	url, user, password string
	root                string
}

func newGitServer(t testing.TB) *gitServer {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		skipOrFail(t, envRequireTools, "git is not available: %v", err)
	}
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		skipOrFail(t, envRequireTools, "git-http-backend not available: %v", err)
	}
	g := &gitServer{user: "patrol-git", password: "git-" + randomHex(t, 12), root: t.TempDir()}
	home := t.TempDir()
	h := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + g.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + home,
		},
		InheritEnv: []string{"PATH"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != g.user || p != g.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.url = srv.URL
	return g
}

// repo creates <name>.git with one commit on main holding files and returns its clone URL.
func (g *gitServer) repo(t testing.TB, name string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(g.root, name+".git")
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_AUTHOR_NAME=deploy-test", "GIT_AUTHOR_EMAIL=deploy-test@example.com",
			"GIT_COMMITTER_NAME=deploy-test", "GIT_COMMITTER_EMAIL=deploy-test@example.com",
			"GIT_AUTHOR_DATE=2026-10-17T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-17T00:00:00Z",
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(g.root, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	run(work, "init", "--quiet", "--initial-branch=main", ".")
	for p, content := range files {
		full := filepath.Join(work, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(work, "add", "-A")
	run(work, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "initial")
	run(work, "push", "--quiet", bare, "main")
	return g.url + "/" + name + ".git"
}

// waitFor polls cond until it reports true or the timeout ends; the last detail is reported.
func waitFor(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, detail := cond()
		if ok {
			return
		}
		if detail != "" {
			last = detail
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last: %s)", timeout, what, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
