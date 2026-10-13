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
	"syscall"
)

// HelperArg is the os.Args[1] value that switches a binary into rlimit-helper mode.
const HelperArg = "__patrol-sandbox-exec"

// Exit codes of the helper when it cannot exec the target (shell conventions).
const (
	helperExitSetup    = 126
	helperExitNotFound = 127
)

var (
	helperMu       sync.RWMutex
	helperOverride string
	executablePath = sync.OnceValues(os.Executable)
)

// SetHelperPath overrides the binary Run re-executes to apply rlimits. An empty
// path restores the default, os.Executable(). The binary must call MaybeHelperMain
// at the start of main (or TestMain).
func SetHelperPath(path string) {
	helperMu.Lock()
	defer helperMu.Unlock()
	helperOverride = path
}

func helperPath() (string, error) {
	helperMu.RLock()
	p := helperOverride
	helperMu.RUnlock()
	if p != "" {
		return p, nil
	}
	return executablePath()
}

// MaybeHelperMain must be called first thing in main (and in TestMain of packages
// whose tests use rlimits). When os.Args[1] is HelperArg it applies the encoded
// rlimits, marks every inherited descriptor above stderr close-on-exec and execs
// the target in place, keeping the PID and process group Run set up; it never
// returns in that mode (on failure it writes to stderr and exits 126/127).
// Otherwise it returns immediately without side effects.
func MaybeHelperMain() {
	if len(os.Args) < 2 || os.Args[1] != HelperArg {
		return
	}
	os.Exit(helperMain(os.Args[2:]))
}

func helperMain(args []string) int {
	spec, err := decodeHelperArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "patrol-sandbox: %v\n", err)
		return helperExitSetup
	}
	if err := applyRlimits(spec); err != nil {
		fmt.Fprintf(os.Stderr, "patrol-sandbox: %v\n", err)
		return helperExitSetup
	}
	markInheritedCloseOnExec()
	// The helper was started with exactly Cmd.Env, so its environment is the target's.
	err = syscall.Exec(spec.path, spec.argv, os.Environ())
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
// descriptors the server inherited without O_CLOEXEC never reach the target.
func markInheritedCloseOnExec() {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		for fd := 3; fd < 1024; fd++ {
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
