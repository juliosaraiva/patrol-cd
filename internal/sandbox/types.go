package sandbox

import (
	"io"
	"time"
)

// Defaults applied when the corresponding Limits field is zero.
const (
	// DefaultTimeout matches the upstream exec timeout default (ARGOCD_EXEC_TIMEOUT).
	DefaultTimeout = 90 * time.Second
	// DefaultMaxStdout caps captured stdout.
	DefaultMaxStdout int64 = 256 << 20
	// DefaultMaxStderr caps captured stderr.
	DefaultMaxStderr int64 = 16 << 20
)

// Limits bounds one command execution.
type Limits struct {
	// Timeout is the wall-clock limit. Zero means DefaultTimeout; a negative value
	// disables it (the context still applies).
	Timeout time.Duration
	// KillGrace, when positive, makes timeout and cancellation send SIGTERM to the
	// process group first and SIGKILL only after this grace period (upstream's
	// SIGTERM-then-fatal-timeout behavior for git). Zero sends SIGKILL immediately.
	// Output overflow always kills immediately.
	KillGrace time.Duration
	// MaxStdout and MaxStderr cap captured output in bytes. Zero means the package
	// default; a negative value disables the cap.
	MaxStdout, MaxStderr int64
	// CPUSeconds sets RLIMIT_CPU (soft limit; the hard limit is one second higher so
	// SIGXCPU is followed by SIGKILL). Zero leaves it unset.
	CPUSeconds uint64
	// AddressSpaceBytes sets RLIMIT_AS. Enforced on Linux; best effort elsewhere
	// (macOS does not enforce it). Zero leaves it unset.
	AddressSpaceBytes uint64
	// MaxOpenFiles sets RLIMIT_NOFILE (soft and hard). Zero leaves it unset.
	MaxOpenFiles uint64
}

// needsHelper reports whether the re-exec helper is required to apply rlimits.
func (l Limits) needsHelper() bool {
	return l.CPUSeconds > 0 || l.AddressSpaceBytes > 0 || l.MaxOpenFiles > 0
}

func (l Limits) normalized() Limits {
	if l.Timeout == 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxStdout == 0 {
		l.MaxStdout = DefaultMaxStdout
	}
	if l.MaxStderr == 0 {
		l.MaxStderr = DefaultMaxStderr
	}
	if l.KillGrace < 0 {
		l.KillGrace = 0
	}
	return l
}

// Cmd describes one command execution.
type Cmd struct {
	// Path is the program: an absolute path, or a bare name looked up in the PATH
	// variable of Env (never the caller's PATH). Relative paths containing a
	// separator are rejected.
	Path string
	// Args are the arguments, excluding the program name (argv[0] is Path).
	Args []string
	// Dir is the working directory. Empty means the caller's working directory.
	Dir string
	// Env is the exact child environment. Nothing is inherited: nil means an empty
	// environment. Use BaseEnv for a minimal allow-listed one.
	Env []string
	// Stdin is the child's standard input. Nil means /dev/null. A non-*os.File
	// reader is copied by a goroutine; it should be finite or unblock on its own.
	Stdin io.Reader
	// Limits bounds the execution.
	Limits Limits
	// Redact lists substrings (tokens, passwords) replaced by "******" in error
	// messages. Result.Stdout and Result.Stderr are returned verbatim.
	Redact []string
}

// Result is the outcome of a command that was started.
type Result struct {
	// Stdout and Stderr hold the captured output, truncated to the configured caps.
	// They are not redacted.
	Stdout, Stderr []byte
	// Duration is the wall-clock time from start until the output was drained.
	Duration time.Duration
}
