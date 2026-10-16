//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// drainTimeout bounds how long Run waits for stdout/stderr EOF after the process
	// group is gone. Only processes that escaped the group (setsid) can hold the
	// pipes open that long.
	drainTimeout = 2 * time.Second
	// stdinWaitDelay bounds how long Wait waits for the stdin copier after exit.
	stdinWaitDelay = time.Second
	redactedMarker = "******"
	readChunk      = 32 << 10
)

// Run executes c and returns its captured output.
//
// A non-nil error is one of: an error matching ErrInvalidCommand or a lookup error
// (nothing was started, or the rlimit helper did not acknowledge); a start error;
// an error matching ErrOutputLimit; an error matching ErrTimeout (Limits.Timeout or
// a context deadline); context.Canceled; or an error wrapping *ExitError. Output
// captured before a failure is returned in Result whenever the process was started.
//
// No descriptor above stderr reaches the child: in direct mode Run marks every
// descriptor of the calling process close-on-exec before starting it (Go opens
// its own descriptors that way; this catches inherited ones such as socket
// activation), and in helper mode the helper does the same before exec.
func Run(ctx context.Context, c Cmd) (Result, error) {
	name := filepath.Base(c.Path)
	if err := ctx.Err(); err != nil {
		return Result{}, contextError(name, err)
	}
	lim := c.Limits.normalized()

	target, err := resolveExecutable(c.Path, c.Env)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", name, err)
	}
	execPath := target
	argv := make([]string, 0, len(c.Args)+1)
	argv = append(argv, c.Path)
	argv = append(argv, c.Args...)
	helper := lim.needsHelper()
	var statusR, statusW *os.File
	if helper {
		hp, isDefault, err := helperPath()
		if err != nil {
			return Result{}, fmt.Errorf("sandbox: %s: resolve rlimit helper: %w", name, err)
		}
		if isDefault && !helperMainCalled.Load() {
			// Re-executing a binary whose main does not handle HelperArg would run
			// that program (for a test binary: its whole suite) instead of c.
			return Result{}, fmt.Errorf("sandbox: %s: %w: rlimits need the helper, but sandbox.MaybeHelperMain was not called at the start of main (or TestMain)", name, ErrInvalidCommand)
		}
		statusR, statusW, err = os.Pipe()
		if err != nil {
			return Result{}, fmt.Errorf("sandbox: %s: helper status pipe: %w", name, err)
		}
		defer closeAll(statusR)
		argv = encodeHelperArgs(hp, helperSpec{
			cpu:    lim.CPUSeconds,
			as:     lim.AddressSpaceBytes,
			nofile: lim.MaxOpenFiles,
			path:   target,
			argv:   argv,
		})
		execPath = hp
	}

	env := c.Env
	if env == nil {
		env = []string{} // non-nil: os/exec inherits the parent environment for nil
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(statusW)
		return Result{}, fmt.Errorf("sandbox: %s: stdout pipe: %w", name, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdoutR, stdoutW, statusW)
		return Result{}, fmt.Errorf("sandbox: %s: stderr pipe: %w", name, err)
	}

	cmd := &exec.Cmd{
		Path:        execPath,
		Args:        argv,
		Dir:         c.Dir,
		Env:         env,
		Stdin:       c.Stdin,
		Stdout:      stdoutW,
		Stderr:      stderrW,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
		WaitDelay:   stdinWaitDelay,
	}
	if helper {
		cmd.ExtraFiles = []*os.File{statusW} // descriptor helperStatusFD in the child
	} else {
		markInheritedCloseOnExec()
	}

	start := time.Now()
	startErr := cmd.Start()
	// The child holds its own copies; ours must go so EOF is observable.
	closeAll(stdoutW, stderrW, statusW)
	if startErr != nil {
		closeAll(stdoutR, stderrR)
		return Result{}, fmt.Errorf("sandbox: start %s: %w", name, startErr)
	}
	proc := cmd.Process
	pgid := proc.Pid // Setpgid with Pgid 0: the child leads a new group

	overflow := make(chan struct{})
	var overflowOnce sync.Once
	onOverflow := func() { overflowOnce.Do(func() { close(overflow) }) }
	outCap := &capture{limit: lim.MaxStdout}
	errCap := &capture{limit: lim.MaxStderr}
	var readers sync.WaitGroup
	readers.Go(func() { outCap.readFrom(stdoutR, onOverflow) })
	readers.Go(func() { errCap.readFrom(stderrR, onOverflow) })

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var timeoutC <-chan time.Time
	if lim.Timeout > 0 {
		t := time.NewTimer(lim.Timeout)
		defer t.Stop()
		timeoutC = t.C
	}

	// Helper handshake: statusC delivers the helper's status byte, or
	// helperStatusNone at EOF (the program never acknowledged).
	var statusC <-chan byte
	var ackTimeoutC <-chan time.Time
	var status byte
	gotStatus := !helper
	if helper {
		statusC = readStatus(statusR)
		t := time.NewTimer(time.Duration(helperAckTimeout.Load()))
		defer t.Stop()
		ackTimeoutC = t.C
	}

	var waitErr, cause error
wait:
	for {
		select {
		case waitErr = <-waitCh:
			break wait
		case status = <-statusC:
			statusC, ackTimeoutC, gotStatus = nil, nil, true
			if status == helperStatusReady || status == helperStatusFailed {
				continue
			}
			cause = errHelperNoAck
			waitErr = terminate(proc, pgid, waitCh, 0)
			break wait
		case <-ackTimeoutC:
			cause = errHelperNoAck
			waitErr = terminate(proc, pgid, waitCh, 0)
			break wait
		case <-timeoutC:
			cause = ErrTimeout
			waitErr = terminate(proc, pgid, waitCh, lim.KillGrace)
			break wait
		case <-ctx.Done():
			cause = ctx.Err()
			waitErr = terminate(proc, pgid, waitCh, lim.KillGrace)
			break wait
		case <-overflow:
			cause = ErrOutputLimit
			waitErr = terminate(proc, pgid, waitCh, 0)
			break wait
		}
	}

	// The leader has been reaped. Kill whatever is left of its process group
	// (background grandchildren) so nothing outlives the call. While any member is
	// alive the group ID cannot be reused, so this only reaches our descendants;
	// with no members left the call fails with ESRCH.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	// A helper writes its status before exec, so once the leader is gone the byte
	// (if any) is already in the pipe; EOF follows as soon as the group is dead.
	// Only an exit or an overflow can hide a missing acknowledgment; a timeout or
	// cancellation may simply have come before a slow helper started.
	if !gotStatus && (cause == nil || errors.Is(cause, ErrOutputLimit)) {
		status = awaitStatus(statusC, statusR)
		if status != helperStatusReady && status != helperStatusFailed {
			cause = errHelperNoAck
		}
	}

	drainPipes(&readers, stdoutR, stderrR)
	closeAll(stdoutR, stderrR)

	res := Result{
		Stdout:   outCap.bytes(),
		Stderr:   errCap.bytes(),
		Duration: time.Since(start),
	}
	runErr := classify(name, lim, c.Redact, cause, waitErr, outCap, errCap, res.Stderr)
	logRun(ctx, name, lim, res, runErr)
	return res, runErr
}

// terminate stops the process group and returns the leader's wait error.
func terminate(proc *os.Process, pgid int, waitCh <-chan error, grace time.Duration) error {
	if grace > 0 {
		signalGroup(proc, pgid, syscall.SIGTERM)
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case err := <-waitCh:
			return err
		case <-t.C:
		}
	}
	signalGroup(proc, pgid, syscall.SIGKILL)
	return <-waitCh
}

// signalGroup signals the group and, as a backstop for a leader that moved itself
// to another group, the leader through os.Process (which refuses to signal a
// process it has already reaped).
func signalGroup(proc *os.Process, pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
	_ = proc.Signal(sig)
}

// drainPipes waits for both readers to hit EOF. Writers that escaped the process
// group may keep a pipe open; after drainTimeout reads are forced to fail.
func drainPipes(readers *sync.WaitGroup, files ...*os.File) {
	done := make(chan struct{})
	go func() {
		readers.Wait()
		close(done)
	}()
	t := time.NewTimer(drainTimeout)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	for _, f := range files {
		if err := f.SetReadDeadline(time.Now()); err != nil {
			_ = f.Close()
		}
	}
	t.Reset(drainTimeout)
	select {
	case <-done:
	case <-t.C:
		// Reader goroutines stay blocked until the escaped writer exits; capture
		// snapshots are safe to take concurrently.
		slog.Warn("sandbox: output pipes still open after process group was killed")
	}
}

// errHelperNoAck is the internal cause for a helper that never acknowledged.
var errHelperNoAck = errors.New("rlimit helper did not acknowledge")

// readStatus reads the helper's single status byte (helperStatusNone at EOF or
// on error) and delivers it on the returned channel.
func readStatus(r *os.File) <-chan byte {
	ch := make(chan byte, 1)
	go func() {
		var b [1]byte
		if n, _ := r.Read(b[:]); n == 1 {
			ch <- b[0]
			return
		}
		ch <- helperStatusNone
	}()
	return ch
}

// awaitStatus waits up to drainTimeout for the status reader, then forces it to
// return (a process that escaped the group could hold the pipe open).
func awaitStatus(statusC <-chan byte, r *os.File) byte {
	t := time.NewTimer(drainTimeout)
	defer t.Stop()
	select {
	case st := <-statusC:
		return st
	case <-t.C:
	}
	_ = r.SetReadDeadline(time.Now())
	return <-statusC
}

func classify(name string, lim Limits, redact []string, cause, waitErr error, outCap, errCap *capture, stderr []byte) error {
	switch {
	case errors.Is(cause, errHelperNoAck):
		// Whatever ran was not the helper (MaybeHelperMain missing, or a wrong
		// SetHelperPath), so neither its output nor its status describes c.
		return fmt.Errorf("sandbox: %s: %w: %w; the helper binary must call sandbox.MaybeHelperMain first in main (or TestMain)",
			name, ErrInvalidCommand, errHelperNoAck)
	case outCap.exceeded() || errCap.exceeded() || errors.Is(cause, ErrOutputLimit):
		return fmt.Errorf("%s: %w (stdout cap %d bytes, stderr cap %d bytes)",
			name, ErrOutputLimit, lim.MaxStdout, lim.MaxStderr)
	case errors.Is(cause, ErrTimeout):
		return fmt.Errorf("%s: %w after %s", name, ErrTimeout, lim.Timeout)
	case cause != nil:
		return contextError(name, cause)
	case waitErr == nil:
		return nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		out := &ExitError{Code: ee.ExitCode(), StderrTail: stderrTail(stderr, redact)}
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			out.Code = -1
			out.Signal = ws.Signal()
		}
		return fmt.Errorf("%s: %w", name, out)
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The process succeeded; only the stdin copier was still blocked.
		return nil
	}
	return fmt.Errorf("sandbox: %s: wait: %w", name, waitErr)
}

func contextError(name string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w: %w", name, ErrTimeout, err)
	}
	return fmt.Errorf("%s: %w", name, err)
}

func logRun(ctx context.Context, name string, lim Limits, res Result, err error) {
	level := slog.LevelDebug
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrOutputLimit) {
		level = slog.LevelWarn
	}
	logger := slog.Default()
	if !logger.Enabled(ctx, level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("cmd", name),
		slog.Duration("duration", res.Duration),
		slog.Int("stdoutBytes", len(res.Stdout)),
		slog.Int("stderrBytes", len(res.Stderr)),
		slog.Bool("rlimitHelper", lim.needsHelper()),
	}
	var ee *ExitError
	switch {
	case errors.As(err, &ee):
		attrs = append(attrs, slog.Int("exitCode", ee.Code))
		if ee.Signal != 0 {
			attrs = append(attrs, slog.String("signal", ee.Signal.String()))
		}
	case err != nil:
		// Our own error strings never contain arguments, env or output.
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	logger.LogAttrs(ctx, level, "sandbox command finished", attrs...)
}

// stderrTail redacts all of stderr first (so a secret can never be split by the
// cut), then keeps at most MaxStderrTail bytes from the end on a rune boundary.
func stderrTail(stderr []byte, redact []string) string {
	s := redactString(string(stderr), redact)
	if len(s) > MaxStderrTail {
		cut := len(s) - MaxStderrTail
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = s[cut:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, "�"))
}

func redactString(s string, items []string) string {
	if len(items) == 0 {
		return s
	}
	sorted := make([]string, 0, len(items))
	for _, it := range items {
		if it != "" {
			sorted = append(sorted, it)
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	slices.SortStableFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	for _, it := range sorted {
		s = strings.ReplaceAll(s, it, redactedMarker)
	}
	return s
}

// capture accumulates up to limit bytes (limit < 0: unbounded) and keeps draining
// after overflow so writers never block on a full pipe.
type capture struct {
	limit int64

	mu       sync.Mutex
	buf      []byte
	overflow bool
}

func (c *capture) readFrom(r io.Reader, onOverflow func()) {
	chunk := make([]byte, readChunk)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			c.mu.Lock()
			data := chunk[:n]
			if c.limit >= 0 {
				room := c.limit - int64(len(c.buf))
				if int64(n) > room {
					data = chunk[:max(room, 0)]
					c.overflow = true
				}
			}
			c.buf = append(c.buf, data...)
			over := c.overflow
			c.mu.Unlock()
			if over {
				onOverflow()
			}
		}
		if err != nil {
			return
		}
	}
}

// bytes returns the captured data without copying. The capacity is clipped so a
// reader that is still running (escaped writer) can only append past the returned
// slice, never modify it.
func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf[:len(c.buf):len(c.buf)]
}

func (c *capture) exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.overflow
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
