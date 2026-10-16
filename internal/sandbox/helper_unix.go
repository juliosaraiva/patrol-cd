//go:build unix

package sandbox

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// HelperArg is the os.Args[1] value that switches a binary into rlimit-helper mode.
const HelperArg = "__patrol-sandbox-exec"

// Exit codes of the helper when it cannot exec the target (shell conventions).
const (
	helperExitSetup    = 126
	helperExitNotFound = 127
)

// Helper handshake. Run passes the write end of a pipe as descriptor
// helperStatusFD (ExtraFiles[0]). The helper writes helperStatusReady after the
// limits are applied, immediately before exec (the descriptor is close-on-exec by
// then, so the target never sees it), or helperStatusFailed before exiting with a
// setup error. A binary that does not call MaybeHelperMain writes neither, and
// Run fails with ErrInvalidCommand instead of returning that program's output.
const (
	helperStatusFD     = 3
	helperStatusReady  = 'R'
	helperStatusFailed = 'F'
	helperStatusNone   = 0
)

// fallbackFDSweep is the descriptor range swept when the descriptor directory
// cannot be listed.
const fallbackFDSweep = 1 << 16

// defaultHelperAckTimeout bounds how long Run waits for the helper's status byte.
// A real helper writes it within milliseconds of starting.
const defaultHelperAckTimeout = 30 * time.Second

var (
	helperMu       sync.RWMutex
	helperOverride string
	executablePath = sync.OnceValues(os.Executable)

	// helperMainCalled records that this process called MaybeHelperMain, so the
	// default helper (this same binary) will recognize HelperArg when re-executed.
	helperMainCalled atomic.Bool
	// helperAckTimeout is defaultHelperAckTimeout; tests lower it.
	helperAckTimeout atomic.Int64
)

func init() { helperAckTimeout.Store(int64(defaultHelperAckTimeout)) }

// SetHelperPath overrides the binary Run re-executes to apply rlimits. An empty
// path restores the default, os.Executable(). The binary must call MaybeHelperMain
// at the start of main (or TestMain); Run verifies that with a handshake and fails
// with ErrInvalidCommand when the helper does not acknowledge.
func SetHelperPath(path string) {
	helperMu.Lock()
	defer helperMu.Unlock()
	helperOverride = path
}

// helperPath returns the helper binary and whether it is the default (this
// process's own executable).
func helperPath() (string, bool, error) {
	helperMu.RLock()
	p := helperOverride
	helperMu.RUnlock()
	if p != "" {
		return p, false, nil
	}
	exe, err := executablePath()
	return exe, true, err
}

// MaybeHelperMain must be called first thing in main (and in TestMain of packages
// whose tests use rlimits). When os.Args[1] is HelperArg it applies the encoded
// rlimits, marks every inherited descriptor above stderr close-on-exec,
// acknowledges to Run, and execs the target in place, keeping the PID and process
// group Run set up; it never returns in that mode (on failure it writes to stderr
// and exits 126/127). Otherwise it records that the helper is installed (Run
// refuses to use this binary as its default helper before that) and returns.
func MaybeHelperMain() {
	if len(os.Args) < 2 || os.Args[1] != HelperArg {
		helperMainCalled.Store(true)
		return
	}
	os.Exit(helperMain(os.Args[2:], helperStatusFile()))
}

// helperStatusFile returns the handshake pipe Run passed, or nil when descriptor
// helperStatusFD is not a pipe (the helper was not started by Run; it then fails).
func helperStatusFile() *os.File {
	var st syscall.Stat_t
	if err := syscall.Fstat(helperStatusFD, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return nil
	}
	return os.NewFile(helperStatusFD, "patrol-sandbox-status")
}

// helperMain runs the helper. status is the handshake pipe (nil: none, which is
// a setup failure).
func helperMain(args []string, status *os.File) int {
	fail := func(code int, format string, a ...any) int {
		if status != nil {
			_, _ = status.Write([]byte{helperStatusFailed})
		}
		fmt.Fprintf(os.Stderr, "patrol-sandbox: "+format+"\n", a...)
		return code
	}
	spec, err := decodeHelperArgs(args)
	if err != nil {
		return fail(helperExitSetup, "%v", err)
	}
	if status == nil {
		return fail(helperExitSetup, "%v: no status descriptor; the helper is started by sandbox.Run", ErrInvalidCommand)
	}
	if err := applyRlimits(spec); err != nil {
		return fail(helperExitSetup, "%v", err)
	}
	markInheritedCloseOnExec() // includes the status descriptor
	if _, err := status.Write([]byte{helperStatusReady}); err != nil {
		fmt.Fprintf(os.Stderr, "patrol-sandbox: acknowledge: %v\n", err)
		return helperExitSetup
	}
	// The helper was started with exactly Cmd.Env, so its environment is the target's.
	err = syscall.Exec(spec.path, spec.argv, os.Environ())
	// The status byte was already sent; the exit status reports the failure.
	fmt.Fprintf(os.Stderr, "patrol-sandbox: exec %s: %v\n", filepath.Base(spec.path), err)
	if errors.Is(err, syscall.ENOENT) {
		return helperExitNotFound
	}
	return helperExitSetup
}

// helperSpec is what Run passes to the helper.
type helperSpec struct {
	cpu, as, nofile uint64
	path            string   // absolute target
	argv            []string // target argv, argv[0] included
}

// encodeHelperArgs returns the helper argv:
//
//	<helper> __patrol-sandbox-exec cpu=N as=N nofile=N -- <path> <argv0> [args...]
func encodeHelperArgs(helper string, s helperSpec) []string {
	out := make([]string, 0, 7+len(s.argv))
	out = append(out, helper, HelperArg,
		"cpu="+strconv.FormatUint(s.cpu, 10),
		"as="+strconv.FormatUint(s.as, 10),
		"nofile="+strconv.FormatUint(s.nofile, 10),
		"--", s.path)
	return append(out, s.argv...)
}

// decodeHelperArgs parses the arguments after HelperArg. It is strict: every
// limit key must appear exactly once, the target must be absolute and argv0 present.
func decodeHelperArgs(args []string) (helperSpec, error) {
	var s helperSpec
	seen := map[string]bool{}
	i := 0
	for ; i < len(args) && args[i] != "--"; i++ {
		k, v, ok := strings.Cut(args[i], "=")
		if !ok || seen[k] {
			return helperSpec{}, fmt.Errorf("%w: helper argument %q", ErrInvalidCommand, args[i])
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return helperSpec{}, fmt.Errorf("%w: helper limit %q: %w", ErrInvalidCommand, args[i], err)
		}
		switch k {
		case "cpu":
			s.cpu = n
		case "as":
			s.as = n
		case "nofile":
			s.nofile = n
		default:
			return helperSpec{}, fmt.Errorf("%w: unknown helper limit %q", ErrInvalidCommand, k)
		}
		seen[k] = true
	}
	if len(seen) != 3 {
		return helperSpec{}, fmt.Errorf("%w: helper limits incomplete", ErrInvalidCommand)
	}
	if i >= len(args) || len(args)-i < 3 {
		return helperSpec{}, fmt.Errorf("%w: helper target missing", ErrInvalidCommand)
	}
	s.path = args[i+1]
	if !filepath.IsAbs(s.path) {
		return helperSpec{}, fmt.Errorf("%w: helper target %q is not absolute", ErrInvalidCommand, s.path)
	}
	s.argv = args[i+2:]
	return s, nil
}

func applyRlimits(s helperSpec) error {
	if s.cpu > 0 {
		// Soft limit raises SIGXCPU; the hard limit one second later is SIGKILL.
		hard := s.cpu
		if hard < math.MaxUint64 {
			hard++
		}
		if err := lowerRlimit(syscall.RLIMIT_CPU, s.cpu, hard); err != nil {
			return fmt.Errorf("set RLIMIT_CPU: %w", err)
		}
	}
	if s.nofile > 0 {
		if err := lowerRlimit(syscall.RLIMIT_NOFILE, s.nofile, s.nofile); err != nil {
			return fmt.Errorf("set RLIMIT_NOFILE: %w", err)
		}
	}
	if s.as > 0 {
		if err := setAddressSpaceLimit(s.as); err != nil {
			return fmt.Errorf("set RLIMIT_AS: %w", err)
		}
	}
	return nil
}

// lowerRlimit sets the limit, clamped to the current hard limit, since an
// unprivileged process can only lower it.
func lowerRlimit(resource int, soft, hard uint64) error {
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(resource, &cur); err != nil {
		return err
	}
	hard = min(hard, cur.Max)
	soft = min(soft, hard)
	return syscall.Setrlimit(resource, &syscall.Rlimit{Cur: soft, Max: hard})
}

// markInheritedCloseOnExec marks every descriptor above stderr close-on-exec, so
// descriptors the process inherited (or created) without O_CLOEXEC never reach a
// child. Setting the flag does not affect the descriptor's use in this process,
// and os/exec clears it on the copies it passes as ExtraFiles.
func markInheritedCloseOnExec() {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		// Listing failed (for example EMFILE): sweep a fixed range that covers
		// typical RLIMIT_NOFILE settings.
		for fd := 3; fd < fallbackFDSweep; fd++ {
			syscall.CloseOnExec(fd)
		}
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}
