package stack

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// GitServer serves bare repositories over git smart HTTP (git http-backend) with basic
// authentication. Tests write to the repositories directly on disk (Repo); Patrol reads them
// through HTTP like any remote.
type GitServer struct {
	// URL is the base URL (http://127.0.0.1:<port>); repositories are <URL>/<name>.git.
	URL string
	// Username and Password are the credentials the server requires.
	Username, Password string

	root     string
	home     string
	srv      *httptest.Server
	requests atomic.Int64
	down     atomic.Bool
	mu       sync.Mutex
	repos    map[string]*Repo
}

// gitBinary is the git executable (PATH).
func gitBinary(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return p
}

func newGitServer(t testing.TB, password string) *GitServer {
	t.Helper()
	git := gitBinary(t)
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	g := &GitServer{Username: "patrol-git", Password: password, root: t.TempDir(), home: t.TempDir(), repos: map[string]*Repo{}}
	h := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + g.root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"HOME=" + g.home,
		},
		InheritEnv: []string{"PATH"},
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		if g.down.Load() {
			http.Error(w, "git server unavailable", http.StatusServiceUnavailable)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != g.Username || p != g.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	g.URL = g.srv.URL
	return g
}

// Requests is the number of HTTP requests the server received.
func (g *GitServer) Requests() int64 { return g.requests.Load() }

// SetDown makes the server answer 503 (true) or serve again (false).
func (g *GitServer) SetDown(down bool) { g.down.Store(down) }

// Repo is one repository: a bare repository served by the GitServer and a working clone the
// test commits in.
type Repo struct {
	// Name is the repository name; URL its clone URL.
	Name, URL string

	g    *GitServer
	bare string
	work string
	// branch is the checked-out branch of the working clone.
	branch string
	mu     sync.Mutex
}

// NewRepo creates <name>.git with an initial commit on main.
func (g *GitServer) NewRepo(t testing.TB, name string) *Repo {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.repos[name]; ok {
		t.Fatalf("repository %s already exists", name)
	}
	r := &Repo{Name: name, URL: g.URL + "/" + name + ".git", g: g, bare: filepath.Join(g.root, name+".git"), work: filepath.Join(t.TempDir(), name), branch: "main"}
	r.git(t, "", "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	r.git(t, "", "init", "--quiet", "--initial-branch=main", r.work)
	r.git(t, r.work, "remote", "add", "origin", r.bare)
	if err := os.WriteFile(filepath.Join(r.work, "README.md"), []byte("# "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Commit(t, "initial commit")
	g.repos[name] = r
	return r
}

// git runs git in dir with an isolated configuration (no user or system config, no commit
// signing) and returns its trimmed standard output.
func (r *Repo) git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command(gitBinary(t), full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.g.home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Patrol Test", "GIT_AUTHOR_EMAIL=test@patrol.example",
		"GIT_COMMITTER_NAME=Patrol Test", "GIT_COMMITTER_EMAIL=test@patrol.example",
		"GIT_TERMINAL_PROMPT=0",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// Write writes files (path relative to the repository root → content) into the working
// clone without committing.
func (r *Repo) Write(t testing.TB, files map[string]string) *Repo {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, content := range files {
		full := filepath.Join(r.work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// Remove deletes paths (files or directories) from the working clone without committing.
func (r *Repo) Remove(t testing.TB, paths ...string) *Repo {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if err := os.RemoveAll(filepath.Join(r.work, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// Commit commits every change of the working clone and pushes the current branch; it
// returns the new commit SHA.
func (r *Repo) Commit(t testing.TB, message string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.git(t, r.work, "add", "--all")
	r.git(t, r.work, "commit", "--quiet", "--allow-empty", "-m", message)
	r.git(t, r.work, "push", "--quiet", "origin", "HEAD:refs/heads/"+r.branch)
	return r.git(t, r.work, "rev-parse", "HEAD")
}

// CommitFiles writes files and commits them.
func (r *Repo) CommitFiles(t testing.TB, message string, files map[string]string) string {
	t.Helper()
	r.Write(t, files)
	return r.Commit(t, message)
}

// Tag creates (or moves, with force) a lightweight tag at rev and pushes it.
func (r *Repo) Tag(t testing.TB, name, rev string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.git(t, r.work, "tag", "--force", name, rev)
	r.git(t, r.work, "push", "--quiet", "--force", "origin", "refs/tags/"+name)
}

// Branch creates a branch at rev, pushes it and checks it out in the working clone.
func (r *Repo) Branch(t testing.TB, name, rev string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.git(t, r.work, "checkout", "--quiet", "-B", name, rev)
	r.git(t, r.work, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+name)
	r.branch = name
}

// Checkout switches the working clone to an existing branch (fetched from the server).
func (r *Repo) Checkout(t testing.TB, branch string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.git(t, r.work, "fetch", "--quiet", "origin")
	r.git(t, r.work, "checkout", "--quiet", "-B", branch, "origin/"+branch)
	r.branch = branch
}

// ForceUpdate moves branch on the server to rev (a force push, e.g. a rewritten history)
// and resets the working clone to it when the branch is checked out.
func (r *Repo) ForceUpdate(t testing.TB, branch, rev string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := r.git(t, r.work, "rev-parse", rev+"^{commit}")
	r.git(t, r.work, "push", "--quiet", "--force", "origin", sha+":refs/heads/"+branch)
	if r.branch == branch {
		r.git(t, r.work, "reset", "--quiet", "--hard", sha)
	}
}

// Head returns the SHA a ref (branch, tag) points to on the server.
func (r *Repo) Head(t testing.TB, ref string) string {
	t.Helper()
	return r.git(t, r.bare, "rev-parse", ref+"^{commit}")
}

// String is the clone URL.
func (r *Repo) String() string { return r.URL }

// GoString hides nothing secret (the URL carries no credentials).
func (r *Repo) GoString() string { return fmt.Sprintf("Repo(%s)", r.URL) }
