//go:build unix

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sh(script string, args ...string) Cmd {
	return Cmd{
		Path: "/bin/sh",
		Args: append([]string{"-c", script, "sh"}, args...),
		Env:  BaseEnv(),
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil && len(bytes.TrimSpace(data)) > 0
	}, slowBound, 10*time.Millisecond, "pid file %s never written", path)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Positive(t, pid)
	return pid
}

// requireGone waits until pid no longer exists (zombies are reaped by init).
func requireGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, slowBound, 20*time.Millisecond, "process %d still alive", pid)
}

func TestRunCapturesOutput(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), sh(`printf 'out:%s' "$1"; printf 'err' >&2`, "arg with space"))
	require.NoError(t, err)
	assert.Equal(t, "out:arg with space", string(res.Stdout))
	assert.Equal(t, "err", string(res.Stderr))
	assert.Positive(t, res.Duration)
}

func TestRunStdinAndBareNameLookup(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), Cmd{Path: "cat", Env: BaseEnv(), Stdin: strings.NewReader("from stdin")})
	require.NoError(t, err)
	assert.Equal(t, "from stdin", string(res.Stdout))
}

func TestRunDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := sh(`pwd -P`)
	c.Dir = dir
	res, err := Run(t.Context(), c)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, want, strings.TrimSpace(string(res.Stdout)))
}

func TestRunInvalidCommand(t *testing.T) {
	t.Parallel()
	nonExec := filepath.Join(t.TempDir(), "script")
	require.NoError(t, os.WriteFile(nonExec, []byte("#!/bin/sh\n"), 0o644))

	tests := []struct {
		name string
		cmd  Cmd
		want error
	}{
		{"empty path", Cmd{Env: BaseEnv()}, ErrInvalidCommand},
		{"relative path with separator", Cmd{Path: "bin/sh", Env: BaseEnv()}, ErrInvalidCommand},
		{"dot relative path", Cmd{Path: "./sh", Env: BaseEnv()}, ErrInvalidCommand},
		{"bare name not found", Cmd{Path: "patrol-no-such-tool", Env: BaseEnv()}, fs.ErrNotExist},
		{"bare name without PATH in env", Cmd{Path: "sh"}, fs.ErrNotExist},
		{"directory", Cmd{Path: os.TempDir(), Env: BaseEnv()}, ErrInvalidCommand},
		{"not executable", Cmd{Path: nonExec, Env: BaseEnv()}, fs.ErrPermission},
		{"missing absolute", Cmd{Path: "/nonexistent/patrol/tool", Env: BaseEnv()}, fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Run(t.Context(), tt.cmd)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestRunRelativePATHEntriesIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "evil-tool"), []byte("#!/bin/sh\necho pwned\n"), 0o755))
	_, err := Run(t.Context(), Cmd{Path: "evil-tool", Dir: dir, Env: []string{"PATH=.:" + "relative/bin"}})
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestRunEnvNotInherited(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("PATROL_SANDBOX_CANARY", "must-not-leak")

	res, err := Run(t.Context(), Cmd{Path: "/usr/bin/env", Env: BaseEnv("EXTRA=1", "LANG=C")})
	require.NoError(t, err)
	out := string(res.Stdout)
	assert.NotContains(t, out, "PATROL_SANDBOX_CANARY")
	assert.NotContains(t, out, "must-not-leak")
	assert.Contains(t, out, "EXTRA=1\n")
	assert.Contains(t, out, "LANG=C\n")
	assert.NotContains(t, out, "LANG=C.UTF-8")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.Len(t, lines, 5, "PATH, HOME, TMPDIR, LANG, EXTRA only: %q", out)

	// nil Env means an empty environment, not the parent's.
	res, err = Run(t.Context(), Cmd{Path: "/usr/bin/env"})
	require.NoError(t, err)
	assert.Empty(t, string(res.Stdout))

	// The helper path must not leak either.
	res, err = Run(t.Context(), Cmd{Path: "/usr/bin/env", Limits: Limits{MaxOpenFiles: 64}})
	require.NoError(t, err)
	assert.Empty(t, string(res.Stdout))
}

func TestRunExitError(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-T0KEN"
	long := strings.Repeat("x", 10_000)
	tests := []struct {
		name       string
		cmd        Cmd
		wantCode   int
		wantSignal syscall.Signal
		check      func(t *testing.T, ee *ExitError, err error)
	}{
		{
			name:     "exit code and stderr tail",
			cmd:      sh(`echo partial; echo "fatal: bad thing" >&2; exit 3`),
			wantCode: 3,
			check: func(t *testing.T, ee *ExitError, err error) {
				assert.Equal(t, "fatal: bad thing", ee.StderrTail)
				assert.Contains(t, err.Error(), "exit status 3: fatal: bad thing")
			},
		},
		{
			name: "redaction in tail and message",
			cmd: func() Cmd {
				c := sh(`echo "auth failed for https://user:$1@example.com" >&2; exit 128`, secret)
				c.Redact = []string{secret, "", "user"}
				return c
			}(),
			wantCode: 128,
			check: func(t *testing.T, ee *ExitError, err error) {
				assert.NotContains(t, ee.StderrTail, secret)
				assert.NotContains(t, err.Error(), secret)
				assert.Contains(t, ee.StderrTail, "https://******:******@example.com")
			},
		},
		{
			name: "tail is bounded and secret at the cut is fully redacted",
			cmd: func() Cmd {
				// The secret straddles the 4KiB cut point counted from the end.
				c := sh(`printf '%s%s' "$1" "$2" >&2; exit 1`, long+secret, strings.Repeat("y", MaxStderrTail-4))
				c.Redact = []string{secret}
				return c
			}(),
			wantCode: 1,
			check: func(t *testing.T, ee *ExitError, err error) {
				assert.LessOrEqual(t, len(ee.StderrTail), MaxStderrTail)
				assert.NotContains(t, ee.StderrTail, secret[len(secret)-4:])
				assert.True(t, strings.HasSuffix(ee.StderrTail, "yyyy"))
			},
		},
		{
			name:       "killed by signal",
			cmd:        sh(`kill -TERM $$; sleep 5`),
			wantCode:   -1,
			wantSignal: syscall.SIGTERM,
			check: func(t *testing.T, ee *ExitError, err error) {
				assert.Contains(t, err.Error(), "terminated by signal")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Run(t.Context(), tt.cmd)
			require.Error(t, err)
			var ee *ExitError
			require.ErrorAs(t, err, &ee)
			assert.Equal(t, tt.wantCode, ee.Code)
			assert.Equal(t, tt.wantSignal, ee.Signal)
			tt.check(t, ee, err)
		})
	}
}

func TestStderrTailRuneBoundary(t *testing.T) {
	t.Parallel()
	// "é" is two bytes; a cut in the middle must not produce invalid UTF-8.
	in := []byte(strings.Repeat("é", MaxStderrTail))
	got := stderrTail(in, nil)
	assert.LessOrEqual(t, len(got), MaxStderrTail)
	assert.True(t, strings.HasPrefix(got, "é"))
	assert.NotContains(t, got, "�")
}

// slowBound is a generous upper bound for "returned promptly" assertions. It only
// distinguishes "did not hang on a 300s child" and tolerates heavily loaded hosts.
const slowBound = 60 * time.Second

func TestRunTimeoutKillsGrandchildren(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := sh(`sleep 300 & echo $! > "$1"; wait`, pidFile)
	c.Limits.Timeout = 3 * time.Second
	start := time.Now()
	res, err := Run(t.Context(), c)
	require.ErrorIs(t, err, ErrTimeout)
	assert.Less(t, time.Since(start), slowBound)
	assert.Less(t, res.Duration, slowBound)
	requireGone(t, readPID(t, pidFile))
}

func TestRunContextCancelKillsGroup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Run(ctx, sh(`sleep 300 & echo $! > "$1"; wait`, pidFile))
		done <- outcome{res, err}
	}()
	pid := readPID(t, pidFile) // cancel only once the grandchild exists
	cancel()
	var out outcome
	select {
	case out = <-done:
	case <-time.After(slowBound):
		t.Fatal("Run did not return after cancellation")
	}
	require.ErrorIs(t, out.err, context.Canceled)
	require.NotErrorIs(t, out.err, ErrTimeout)
	requireGone(t, pid)
}

func TestRunContextDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, sh(`sleep 300`))
	require.ErrorIs(t, err, ErrTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = Run(ctx, sh(`true`)) // already expired: nothing is started
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunLeaderExitKillsBackgroundChildren(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	// The background sleep inherits stdout; Run must not wait for it.
	res, err := Run(t.Context(), sh(`sleep 300 & echo $! > "$1"; echo done`, pidFile))
	require.NoError(t, err)
	assert.Equal(t, "done\n", string(res.Stdout))
	assert.Less(t, time.Since(start), slowBound)
	requireGone(t, readPID(t, pidFile))
}

func TestRunEscapedWriterDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	require.NoError(t, err)
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	// The shell waits for the child to leave the group before exiting, otherwise
	// the post-exit group kill would race with the child's setsid.
	res, err := Run(t.Context(), sh(`"$1" `+testChildArg+` setsid-sleep "$2" & while [ ! -s "$2" ]; do sleep 0.05; done; echo started`, self, pidFile))
	require.NoError(t, err)
	assert.Equal(t, "started\n", string(res.Stdout))
	assert.Less(t, time.Since(start), slowBound)
	assert.GreaterOrEqual(t, res.Duration, drainTimeout, "the drain deadline is what unblocked Run")

	pid := readPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// It left the group, so the group kill could not reach it: that is why the
	// pipe drain is bounded.
	require.NoError(t, syscall.Kill(pid, 0))
}

func TestRunKillGrace(t *testing.T) {
	t.Parallel()
	t.Run("SIGTERM first", func(t *testing.T) {
		t.Parallel()
		c := sh(`trap 'echo got-term; exit 7' TERM; echo ready; while :; do sleep 0.05; done`)
		c.Limits = Limits{Timeout: 2 * time.Second, KillGrace: 5 * time.Minute}
		start := time.Now()
		res, err := Run(t.Context(), c)
		require.ErrorIs(t, err, ErrTimeout)
		require.Contains(t, string(res.Stdout), "ready", "trap was not installed before the timeout")
		assert.Contains(t, string(res.Stdout), "got-term")
		assert.Less(t, time.Since(start), slowBound, "SIGTERM must end the run long before the grace period")
	})
	t.Run("escalates to SIGKILL", func(t *testing.T) {
		t.Parallel()
		c := sh(`trap '' TERM; echo ready; while :; do sleep 0.05; done`)
		c.Limits = Limits{Timeout: 2 * time.Second, KillGrace: time.Second}
		start := time.Now()
		res, err := Run(t.Context(), c)
		require.ErrorIs(t, err, ErrTimeout)
		require.Contains(t, string(res.Stdout), "ready", "trap was not installed before the timeout")
		elapsed := time.Since(start)
		assert.GreaterOrEqual(t, elapsed, 3*time.Second)
		assert.Less(t, elapsed, slowBound)
	})
}

func TestRunOutputLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		script             string
		limits             Limits
		wantErr            bool
		wantStdout, wantSE int
	}{
		{"stdout flood", `while :; do echo 0123456789; done`, Limits{MaxStdout: 1000}, true, 1000, 0},
		{"stderr flood", `while :; do echo 0123456789 >&2; done`, Limits{MaxStderr: 100}, true, 0, 100},
		{"exactly at limit", `printf 0123456789`, Limits{MaxStdout: 10}, false, 10, 0},
		{"one byte over", `printf 0123456789X`, Limits{MaxStdout: 10}, true, 10, 0},
		{"unlimited", `printf 0123456789X`, Limits{MaxStdout: -1}, false, 11, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := sh(tt.script)
			c.Limits = tt.limits
			c.Limits.Timeout = 30 * time.Second
			start := time.Now()
			res, err := Run(t.Context(), c)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrOutputLimit)
			} else {
				require.NoError(t, err)
			}
			assert.Len(t, res.Stdout, tt.wantStdout)
			assert.Len(t, res.Stderr, tt.wantSE)
			assert.Less(t, time.Since(start), slowBound)
		})
	}
}

func TestRunNoDescriptorInheritance(t *testing.T) {
	t.Parallel()
	// Hold a descriptor numbered above anything ls itself opens (it uses up to two
	// after 0-2), so its absence in the child is meaningful.
	var held *os.File
	for range 64 {
		f, err := os.Open(os.DevNull)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		if f.Fd() > 9 {
			held = f
			break
		}
	}
	require.NotNil(t, held, "could not obtain a descriptor above 9")
	heldFD := int(held.Fd())

	for _, helper := range []bool{false, true} {
		t.Run(fmt.Sprintf("helper=%v", helper), func(t *testing.T) {
			c := sh(`exec ls "$1"`, fdDir)
			if helper {
				c.Limits.MaxOpenFiles = 128
			}
			res, err := Run(t.Context(), c)
			require.NoError(t, err, "stderr: %s", res.Stderr)
			var open []int
			for _, f := range strings.Fields(string(res.Stdout)) {
				fd, err := strconv.Atoi(f)
				require.NoError(t, err)
				open = append(open, fd)
			}
			assert.NotContains(t, open, heldFD, "descriptor opened by the parent leaked")
			// Run (direct mode) and the helper mark every descriptor above stderr
			// close-on-exec, so only 0-2 and the ones ls opened itself remain.
			for _, fd := range open {
				assert.LessOrEqual(t, fd, 4, "unexpected inherited descriptor %d in %v", fd, open)
			}
		})
	}
}

func TestRunRlimitsViaHelper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		script string
		limits Limits
		want   string
	}{
		{"nofile soft", `ulimit -n`, Limits{MaxOpenFiles: 48}, "48"},
		{"nofile hard", `ulimit -Hn`, Limits{MaxOpenFiles: 48}, "48"},
		{"cpu soft", `ulimit -t`, Limits{CPUSeconds: 7}, "7"},
		{"cpu hard is one second higher", `ulimit -Ht`, Limits{CPUSeconds: 7}, "8"},
	}
	if runtime.GOOS == "linux" {
		tests = append(tests, struct {
			name   string
			script string
			limits Limits
			want   string
		}{"address space", `ulimit -v`, Limits{AddressSpaceBytes: 1 << 30}, strconv.Itoa((1 << 30) / 1024)})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := sh(tt.script)
			c.Limits = tt.limits
			res, err := Run(t.Context(), c)
			require.NoError(t, err, "stderr: %s", res.Stderr)
			assert.Equal(t, tt.want, strings.TrimSpace(string(res.Stdout)))
		})
	}
}

func TestRunRlimitCPUKillsBusyLoop(t *testing.T) {
	t.Parallel()
	c := sh(`while :; do :; done`)
	// CPU time, not wall time: on a loaded host one CPU second can take a while.
	c.Limits = Limits{CPUSeconds: 1, Timeout: 5 * time.Minute}
	_, err := Run(t.Context(), c)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTimeout)
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Contains(t, []syscall.Signal{syscall.SIGXCPU, syscall.SIGKILL}, ee.Signal)
}

func TestRunHelperPreservesArgv0AndArgs(t *testing.T) {
	t.Parallel()
	c := sh(`printf '%s|' "$0" "$@"`, "a b", "", "--", "c")
	c.Limits.CPUSeconds = 30
	res, err := Run(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, "sh|a b||--|c|", string(res.Stdout))
}

func TestRunConcurrent(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			c := sh(`printf '%s' "$1"; printf e >&2`, strconv.Itoa(i))
			if i%4 == 0 {
				c.Limits.MaxOpenFiles = 256
			}
			res, err := Run(t.Context(), c)
			if err == nil && (string(res.Stdout) != strconv.Itoa(i) || string(res.Stderr) != "e") {
				err = fmt.Errorf("run %d: got %q/%q", i, res.Stdout, res.Stderr)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestSetHelperPath(t *testing.T) {
	// Not parallel: mutates package state.
	missing := filepath.Join(t.TempDir(), "no-helper")
	SetHelperPath(missing)
	t.Cleanup(func() { SetHelperPath("") })

	_, err := Run(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", "true"}, Limits: Limits{CPUSeconds: 5}})
	require.ErrorIs(t, err, fs.ErrNotExist)

	// Without rlimits the helper is not involved.
	_, err = Run(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", "true"}})
	require.NoError(t, err)

	SetHelperPath("")
	_, err = Run(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", "true"}, Limits: Limits{CPUSeconds: 5}})
	require.NoError(t, err)
}

// withHelperState runs fn with the helper path override and the
// MaybeHelperMain flag set as given, restoring both afterwards. Callers must not
// be parallel: the state is package-global.
func withHelperState(t *testing.T, path string, mainCalled bool, ackTimeout time.Duration) {
	t.Helper()
	prevCalled := helperMainCalled.Load()
	prevAck := helperAckTimeout.Load()
	SetHelperPath(path)
	helperMainCalled.Store(mainCalled)
	if ackTimeout > 0 {
		helperAckTimeout.Store(int64(ackTimeout))
	}
	t.Cleanup(func() {
		SetHelperPath("")
		helperMainCalled.Store(prevCalled)
		helperAckTimeout.Store(prevAck)
	})
}

func TestRunHelperHandshake(t *testing.T) {
	// Not parallel: mutates package state.
	t.Run("program ignoring the helper protocol is not mistaken for the target", func(t *testing.T) {
		// /bin/echo stands in for a binary whose main never calls MaybeHelperMain:
		// before the fix Run returned its output ("__patrol-sandbox-exec cpu=5 ...")
		// with a nil error and never ran the target.
		withHelperState(t, "/bin/echo", true, 0)
		res, err := Run(t.Context(), Cmd{Path: "/bin/echo", Args: []string{"hello"}, Limits: Limits{CPUSeconds: 5}})
		require.ErrorIs(t, err, ErrInvalidCommand)
		assert.Contains(t, err.Error(), "did not acknowledge")
		assert.NotEqual(t, "hello\n", string(res.Stdout))
	})

	t.Run("long-running non-helper is killed after the acknowledgment timeout", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "pid")
		script := filepath.Join(dir, "not-a-helper")
		// Holds the inherited status descriptor open and never writes to it.
		require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho $$ > '"+pidFile+"'\nexec /bin/sleep 300\n"), 0o755))
		// Long enough for a new script to record its pid, so that the kill can be checked.
		withHelperState(t, script, true, 5*time.Second)
		start := time.Now()
		_, err := Run(t.Context(), Cmd{Path: "/bin/echo", Args: []string{"hello"}, Limits: Limits{CPUSeconds: 5, Timeout: 5 * time.Minute}})
		require.ErrorIs(t, err, ErrInvalidCommand)
		require.NotErrorIs(t, err, ErrTimeout)
		assert.Less(t, time.Since(start), slowBound)
		// Run reaps what it kills, so nothing writes the file after this: on a loaded machine
		// the timeout can fire before the first line ran, and then there is no process to find.
		if data, err := os.ReadFile(pidFile); err == nil && len(bytes.TrimSpace(data)) > 0 {
			requireGone(t, readPID(t, pidFile))
		}
	})

	t.Run("default helper requires MaybeHelperMain in this process", func(t *testing.T) {
		withHelperState(t, "", false, 0)
		marker := filepath.Join(t.TempDir(), "ran")
		c := sh(`: > "$1"`, marker)
		c.Limits.CPUSeconds = 5
		_, err := Run(t.Context(), c)
		require.ErrorIs(t, err, ErrInvalidCommand)
		assert.Contains(t, err.Error(), "MaybeHelperMain")
		assert.NoFileExists(t, marker, "nothing may be started")
		// Direct mode does not need the helper.
		c.Limits.CPUSeconds = 0
		_, err = Run(t.Context(), c)
		require.NoError(t, err)
		assert.FileExists(t, marker)
	})

	t.Run("real helper still works and reports exec failures as exit errors", func(t *testing.T) {
		withHelperState(t, "", true, 0)
		c := sh(`printf ok`)
		c.Limits.CPUSeconds = 5
		res, err := Run(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, "ok", string(res.Stdout))
	})
}

func TestRunDirectModeDoesNotLeakRawDescriptors(t *testing.T) {
	// Not parallel: raw descriptors without O_CLOEXEC would leak into the children
	// of concurrently running tests.
	var raw []int
	t.Cleanup(func() {
		for _, fd := range raw {
			_ = syscall.Close(fd)
		}
	})
	// Open without O_CLOEXEC, as an inherited (socket-activation style) or
	// cgo-opened descriptor would be, until one lands above anything ls opens.
	for range 64 {
		fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
		require.NoError(t, err)
		raw = append(raw, fd)
		if fd > 9 {
			break
		}
	}
	require.Greater(t, raw[len(raw)-1], 9, "could not obtain a descriptor above 9")

	for _, helper := range []bool{false, true} {
		t.Run(fmt.Sprintf("helper=%v", helper), func(t *testing.T) {
			c := sh(`exec ls "$1"`, fdDir)
			if helper {
				c.Limits.MaxOpenFiles = 128
			}
			res, err := Run(t.Context(), c)
			require.NoError(t, err, "stderr: %s", res.Stderr)
			var open []int
			for _, f := range strings.Fields(string(res.Stdout)) {
				fd, err := strconv.Atoi(f)
				require.NoError(t, err)
				open = append(open, fd)
			}
			// ls itself uses 3 and 4, so a raw descriptor at those numbers is
			// indistinguishable; everything above must be absent.
			assert.NotContains(t, open, raw[len(raw)-1], "raw descriptor leaked into the child")
			for _, fd := range open {
				assert.LessOrEqual(t, fd, 4, "unexpected inherited descriptor %d in %v", fd, open)
			}
		})
	}
	// Marking close-on-exec does not close them in this process.
	for _, fd := range raw {
		var st syscall.Stat_t
		require.NoError(t, syscall.Fstat(fd, &st), "fd %d", fd)
	}
}
