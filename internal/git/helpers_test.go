package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// gitBin returns the git executable or skips the test.
func gitBin(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found in PATH")
	}
	return p
}

// fixtureEnv is the environment of fixture-building git commands: no user or
// system configuration, a fixed identity.
func fixtureEnv(home string, extra ...string) []string {
	env := []string{
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
	return append(env, extra...)
}

// runGit runs a fixture git command and returns trimmed stdout.
func runGit(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// remoteFixture is a bare "remote" repository plus a working clone used to
// author commits.
type remoteFixture struct {
	t    testing.TB
	dir  string
	bare string
	work string
	env  []string
}

func newRemoteFixture(t testing.TB) *remoteFixture {
	t.Helper()
	gitBin(t)
	dir := t.TempDir()
	f := &remoteFixture{t: t, dir: dir, bare: filepath.Join(dir, "remote.git"), work: filepath.Join(dir, "work")}
	f.env = fixtureEnv(dir, "GIT_ALLOW_PROTOCOL=file:http:https:ssh")
	runGit(t, dir, f.env, "init", "--quiet", "--bare", "--initial-branch=main", f.bare)
	runGit(t, dir, f.env, "init", "--quiet", "--initial-branch=main", f.work)
	f.git("remote", "add", "origin", f.bare)
	return f
}

func (f *remoteFixture) url() string { return "file://" + f.bare }

func (f *remoteFixture) git(args ...string) string {
	f.t.Helper()
	return runGit(f.t, f.work, f.env, args...)
}

func (f *remoteFixture) bareGit(args ...string) string {
	f.t.Helper()
	return runGit(f.t, f.bare, f.env, args...)
}

// commit writes files (relative path -> content; empty content deletes) and
// commits them, returning the SHA.
func (f *remoteFixture) commit(msg string, files map[string]string) string {
	f.t.Helper()
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, rel := range keys {
		p := filepath.Join(f.work, rel)
		if files[rel] == "" {
			require.NoError(f.t, os.RemoveAll(p))
			continue
		}
		require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(f.t, os.WriteFile(p, []byte(files[rel]), 0o644))
	}
	f.git("add", "--all")
	f.git("commit", "--quiet", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

func (f *remoteFixture) push(refspecs ...string) {
	f.t.Helper()
	f.git(append([]string{"push", "--quiet", "origin"}, refspecs...)...)
}

// cmdRecorder records every command a client runs.
type cmdRecorder struct {
	mu   sync.Mutex
	cmds []sandbox.Cmd
}

func (r *cmdRecorder) hook(c sandbox.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, c)
}

func (r *cmdRecorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.cmds {
		if len(c.Args) > 0 && c.Args[0] == sub {
			n++
		}
	}
	return n
}

func (r *cmdRecorder) all() []sandbox.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cmds)
}

func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func newTestClient(t testing.TB, repoURL string, creds Creds, opts ...Option) *Client {
	t.Helper()
	c, err := New(repoURL, filepath.Join(t.TempDir(), "wc"), creds, append([]Option{AllowLocalRepositories(true)}, opts...)...)
	require.NoError(t, err)
	return c
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
