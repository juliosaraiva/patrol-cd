package commitserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func TestMain(m *testing.M) {
	// The test binary doubles as the GIT_ASKPASS helper and the sandbox rlimit helper.
	git.MaybeAskpassMain()
	sandbox.MaybeHelperMain()
	os.Exit(m.Run())
}

func gitBin(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found in PATH")
	}
	return p
}

// fixtureEnv isolates fixture git commands from user and system configuration.
func fixtureEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Dry Author",
		"GIT_AUTHOR_EMAIL=dry@example.com",
		"GIT_COMMITTER_NAME=Dry Author",
		"GIT_COMMITTER_EMAIL=dry@example.com",
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

// remote is a bare repository used as the hydrated (destination) repository.
type remote struct {
	t    testing.TB
	dir  string
	bare string
	env  []string
}

func newRemote(t testing.TB) *remote {
	t.Helper()
	gitBin(t)
	// Resolve symlinks (macOS /var -> /private/var) so file:// URLs are canonical.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	r := &remote{t: t, dir: dir, bare: filepath.Join(dir, "hydrated.git"), env: fixtureEnv(dir)}
	runGit(t, dir, r.env, "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	return r
}

func (r *remote) url() string { return "file://" + r.bare }

func (r *remote) git(args ...string) string {
	r.t.Helper()
	return runGit(r.t, r.bare, r.env, args...)
}

// tryGit runs git in the bare repository and reports failure instead of failing the test.
func (r *remote) tryGit(args ...string) (string, error) {
	cmd := exec.Command(gitBin(r.t), args...)
	cmd.Dir = r.bare
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *remote) commitCount(branch string) int {
	r.t.Helper()
	var n int
	_, err := fmt.Sscanf(r.git("rev-list", "--count", "refs/heads/"+branch), "%d", &n)
	require.NoError(r.t, err)
	return n
}

func (r *remote) file(branch, path string) string {
	r.t.Helper()
	return r.git("show", "refs/heads/"+branch+":"+path)
}

func (r *remote) rawFile(branch, path string) string {
	r.t.Helper()
	cmd := exec.Command(gitBin(r.t), "show", "refs/heads/"+branch+":"+path)
	cmd.Dir = r.bare
	cmd.Env = r.env
	out, err := cmd.Output()
	require.NoError(r.t, err)
	return string(out)
}

func (r *remote) hasFile(branch, path string) bool {
	_, err := r.tryGit("cat-file", "-e", "refs/heads/"+branch+":"+path)
	return err == nil
}

func (r *remote) head(branch string) string {
	r.t.Helper()
	return r.git("rev-parse", "refs/heads/"+branch)
}

func (r *remote) note(sha string) commitNote {
	r.t.Helper()
	var n commitNote
	require.NoError(r.t, json.Unmarshal([]byte(r.git("notes", "--ref="+NoteNamespace, "show", sha)), &n))
	return n
}

// seed commits files (path -> content; a "symlink:" prefix makes a symbolic link) on branch
// through a scratch clone and pushes it.
func (r *remote) seed(branch string, files map[string]string) string {
	r.t.Helper()
	work := filepath.Join(r.dir, fmt.Sprintf("seed-%d", time.Now().UnixNano()))
	runGit(r.t, r.dir, r.env, "clone", "--quiet", r.bare, work)
	if out, err := exec.Command(gitBin(r.t), "-C", work, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch).Output(); err == nil && len(out) > 0 {
		runGit(r.t, work, r.env, "checkout", "--quiet", "-B", branch, "origin/"+branch)
	} else {
		runGit(r.t, work, r.env, "checkout", "--quiet", "--orphan", branch)
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p := filepath.Join(work, k)
		require.NoError(r.t, os.MkdirAll(filepath.Dir(p), 0o755))
		_ = os.RemoveAll(p)
		if target, ok := strings.CutPrefix(files[k], "symlink:"); ok {
			require.NoError(r.t, os.Symlink(target, p))
			continue
		}
		require.NoError(r.t, os.WriteFile(p, []byte(files[k]), 0o644))
	}
	runGit(r.t, work, r.env, "add", "--all")
	runGit(r.t, work, r.env, "commit", "--quiet", "--allow-empty", "-m", "seed "+branch)
	runGit(r.t, work, r.env, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return runGit(r.t, work, r.env, "rev-parse", "HEAD")
}

func newTestService(t testing.TB, opts ServiceOptions) *Service {
	t.Helper()
	opts.AllowLocalRepositories = true
	if opts.WorkDir == "" {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		require.NoError(t, err)
		opts.WorkDir = dir
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return NewService(opts)
}

func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

const (
	drySHA1 = "1111111111111111111111111111111111111111"
	drySHA2 = "2222222222222222222222222222222222222222"
	drySHA3 = "3333333333333333333333333333333333333333"
)

func configMap(name, value string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"default"},"data":{"key":%q}}`, name, value))
}

func newRequest(url, branch, drySHA string, paths ...PathDetails) *CommitHydratedManifestsRequest {
	return &CommitHydratedManifestsRequest{
		Repo:           apiclient.Repository{URL: url},
		SyncBranch:     branch,
		TargetBranch:   branch,
		DrySHA:         drySHA,
		CommitMessage:  "hydrate " + drySHA[:7],
		ReadmeTemplate: settings.DefaultReadmeMessageTemplate,
		Paths:          paths,
	}
}

func pathOf(p string, manifests ...json.RawMessage) PathDetails {
	return PathDetails{Path: p, Manifests: manifests, Commands: []string{"kustomize build ."}}
}
