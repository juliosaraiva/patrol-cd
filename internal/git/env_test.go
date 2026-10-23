package git

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func configEntries(env []string) map[string]string {
	_, cfg := splitGitConfig(env)
	m := map[string]string{}
	for _, c := range cfg {
		m[c.key] = c.value
	}
	return m
}

// Not parallel: uses t.Setenv.
func TestCommandEnvironmentIsExact(t *testing.T) {
	t.Setenv("PATROL_CANARY_SECRET", "canary")
	t.Setenv("GIT_DIR", "/should/not/leak")
	t.Setenv("https_proxy", "http://env-proxy.example:3128")
	f := newRevisionFixture(t)
	rec := &cmdRecorder{}
	c := newTestClient(t, f.url(), nil, withRunHook(rec.hook))
	_, err := c.LsRemote(testCtx(t))
	require.NoError(t, err)
	env := rec.all()[0].Env
	m := envMap(env)
	assert.NotContains(t, m, "PATROL_CANARY_SECRET")
	assert.NotContains(t, m, "GIT_DIR")
	assert.Equal(t, "/dev/null", m["HOME"])
	assert.Equal(t, "0", m["GIT_TERMINAL_PROMPT"])
	assert.Equal(t, "1", m["GIT_LFS_SKIP_SMUDGE"])
	assert.Equal(t, "/dev/null", m["GIT_CONFIG_GLOBAL"])
	assert.Equal(t, "http://env-proxy.example:3128", m["https_proxy"], "process proxy passes through without a Proxy option")
	cfg := configEntries(env)
	assert.Equal(t, "false", cfg["maintenance.autoDetach"])
	assert.Equal(t, "false", cfg["gc.autoDetach"])
	assert.Equal(t, "", cfg["credential.helper"])
	assert.Equal(t, "never", cfg["protocol.ext.allow"])
	assert.Contains(t, cfg, "credential.helper")

	rec2 := &cmdRecorder{}
	c2 := newTestClient(t, "https://example.invalid/x", nil, BuiltinGitConfig(false), Proxy("http://p.example:1"), NoProxy("a,b"), withRunHook(rec2.hook))
	_, _ = c2.LsRemote(testCtx(t))
	m2 := envMap(rec2.all()[0].Env)
	assert.Equal(t, "http://p.example:1", m2["http_proxy"])
	assert.Equal(t, "http://p.example:1", m2["https_proxy"])
	assert.Equal(t, "a,b", m2["no_proxy"])
	cfg2 := configEntries(rec2.all()[0].Env)
	assert.NotContains(t, cfg2, "maintenance.autoDetach")
	assert.NotContains(t, cfg2, "gc.autoDetach")
}

func TestTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	// A server that accepts connections and never answers.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	repoURL := "http://" + l.Addr().String() + "/repo.git"

	c := newTestClient(t, repoURL, nil, Timeout(time.Second), KillGrace(100*time.Millisecond))
	start := time.Now()
	_, err = c.LsRemote(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, sandbox.ErrTimeout)
	assert.Less(t, time.Since(start), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	c2 := newTestClient(t, repoURL, nil, KillGrace(100*time.Millisecond))
	_, err = c2.LsRemote(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWithRetry(t *testing.T) {
	t.Parallel()
	transient := &CommandError{Op: "fetch", Message: "fatal: the remote end hung up unexpectedly (exit status 128)"}
	permanent := &CommandError{Op: "fetch", Message: "fatal: repository not found (exit status 128)"}
	auth := &CommandError{Op: "fetch", Message: "rpc failed; authentication failed", Kind: ErrAuthentication}

	c := newTestClient(t, "https://example.com/x", nil, Retry(3, time.Millisecond, 2*time.Millisecond))
	ctx := testCtx(t)
	calls := 0
	err := c.withRetry(ctx, func() error {
		calls++
		if calls < 3 {
			return transient
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)

	calls = 0
	err = c.withRetry(ctx, func() error { calls++; return transient })
	assert.ErrorIs(t, err, transient)
	assert.Equal(t, 3, calls, "attempts are bounded")

	for _, e := range []error{permanent, auth} {
		calls = 0
		err = c.withRetry(ctx, func() error { calls++; return e })
		assert.ErrorIs(t, err, e)
		assert.Equal(t, 1, calls, "non-transient errors are not retried")
	}

	noRetry := newTestClient(t, "https://example.com/x", nil)
	calls = 0
	_ = noRetry.withRetry(ctx, func() error { calls++; return transient })
	assert.Equal(t, 1, calls, "default is a single attempt")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	slow := newTestClient(t, "https://example.com/x", nil, Retry(5, time.Hour, time.Hour))
	err = slow.withRetry(cancelled, func() error { return transient })
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClassifyStderr(t *testing.T) {
	t.Parallel()
	tests := map[string]error{
		"fatal: Authentication failed for 'https://x/'":                             ErrAuthentication,
		"fatal: could not read Username for 'https://x': terminal prompts disabled": ErrAuthentication,
		"git@host: Permission denied (publickey).":                                  ErrAuthentication,
		"Host key verification failed.":                                             ErrAuthentication,
		"fatal: couldn't find remote ref refs/heads/nope":                           ErrRevisionNotFound,
		"fatal: invalid reference: nope":                                            ErrRevisionNotFound,
		"fatal: unable to access 'https://x/': Could not resolve host: x":           nil,
	}
	for msg, want := range tests {
		got := classifyStderr(strings.ToLower(msg))
		if want == nil {
			assert.NoError(t, got, msg)
		} else {
			assert.True(t, errors.Is(got, want), msg)
		}
	}
}

func TestCommandErrorRedaction(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, "https://user:hunter2-secret@example.com/x", HTTPSCreds{Password: "tok-secret-1"})
	err := c.commandError("fetch", sandbox.Result{Stderr: []byte("fatal: tok-secret-1 hunter2-secret")},
		&sandbox.ExitError{Code: 128, StderrTail: "fatal: tok-secret-1 and hunter2-secret"})
	assert.NotContains(t, err.Error(), "tok-secret-1")
	assert.Equal(t, "git fetch: fatal: ****** and ****** (exit status 128)", err.Error(),
		"credential secrets and the URL password are both redacted")
	var exitErr *sandbox.ExitError
	assert.ErrorAs(t, err, &exitErr)

	c2 := newTestClient(t, "https://user:hunter2-secret@example.com/x", nil)
	err = c2.commandError("fetch", sandbox.Result{}, &sandbox.ExitError{Code: 128, StderrTail: "x hunter2-secret y"})
	assert.Equal(t, "git fetch: x ****** y (exit status 128)", err.Error())
}

// With LFS enabled the client configures the LFS filter itself, whatever the host's system
// gitconfig holds (git-lfs skips the checkout of large files without it); without LFS it does not.
func TestLFSFilterConfig(t *testing.T) {
	t.Parallel()
	filter := func(c *Client) map[string]string {
		got := map[string]string{}
		for _, e := range c.baseConfig() {
			if strings.HasPrefix(e.key, "filter.lfs.") {
				got[e.key] = e.value
			}
		}
		return got
	}
	assert.Equal(t, map[string]string{
		"filter.lfs.clean":    "git-lfs clean -- %f",
		"filter.lfs.smudge":   "git-lfs smudge -- %f",
		"filter.lfs.process":  "git-lfs filter-process",
		"filter.lfs.required": "true",
	}, filter(newTestClient(t, "https://example.com/x", nil, EnableLFS(true))))
	assert.Empty(t, filter(newTestClient(t, "https://example.com/x", nil)))
}

func TestLFS(t *testing.T) {
	t.Parallel()
	gitBin(t)
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not installed")
	}
	f := newRemoteFixture(t)
	f.git("lfs", "install", "--local")
	f.git("lfs", "track", "*.bin")
	f.commit("lfs", map[string]string{"blob.bin": "binary-content\n", ".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n"})
	f.push("main")
	ctx := testCtx(t)
	c := newTestClient(t, f.url(), nil, EnableLFS(true))
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))
	assert.Equal(t, "binary-content\n", readFile(t, filepath.Join(c.Root(), "blob.bin")))
}
