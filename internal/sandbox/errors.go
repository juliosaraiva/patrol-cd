package sandbox

import (
	"errors"
	"fmt"
	"syscall"
)

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrTimeout reports that the command exceeded Limits.Timeout or that the
	// context deadline expired. The process group was killed.
	ErrTimeout = errors.New("sandbox: command timed out")
	// ErrOutputLimit reports that stdout or stderr exceeded its cap. The process
	// group was killed and the captured output is truncated to the cap.
	ErrOutputLimit = errors.New("sandbox: command output limit exceeded")
	// ErrInvalidCommand reports a Cmd that cannot be executed safely (empty or
	// relative path, malformed helper arguments).
	ErrInvalidCommand = errors.New("sandbox: invalid command")

	// ErrPathEscape reports a path, symlink or archive entry that resolves outside
	// its root. *OutOfBoundsSymlinkError unwraps to it.
	ErrPathEscape = errors.New("sandbox: path escapes root")
	// ErrAbsolutePath reports an absolute path where a root-relative one is required.
	ErrAbsolutePath = errors.New("sandbox: absolute path not allowed")
	// ErrTooManySymlinks reports a symlink chain that is too long (or a loop).
	ErrTooManySymlinks = errors.New("sandbox: too many levels of symbolic links")

	// ErrUnsafeArchiveEntry reports an archive entry that is rejected for safety
	// reasons (device or fifo, unsupported type, conflicting duplicate, symlinked
	// parent, '..' component).
	ErrUnsafeArchiveEntry = errors.New("sandbox: unsafe archive entry")
	// ErrArchiveLimit reports that an archive exceeded ArchiveLimits.
	ErrArchiveLimit = errors.New("sandbox: archive limit exceeded")
	// ErrInvalidArchive reports a corrupt or unreadable archive stream.
	ErrInvalidArchive = errors.New("sandbox: invalid archive")
)

// MaxStderrTail is the maximum size in bytes of ExitError.StderrTail.
const MaxStderrTail = 4 << 10

// ExitError reports a command that ran and exited unsuccessfully.
type ExitError struct {
	// Code is the exit status, or -1 when the process was terminated by a signal.
	Code int
	// Signal is the terminating signal, or 0 when the process exited normally.
	Signal syscall.Signal
	// StderrTail holds at most MaxStderrTail bytes from the end of stderr, with every
	// Cmd.Redact substring replaced by "******". It is valid UTF-8.
	StderrTail string
}

func (e *ExitError) Error() string {
	status := fmt.Sprintf("exit status %d", e.Code)
	if e.Signal != 0 {
		status = "terminated by signal " + e.Signal.String()
	}
	if e.StderrTail == "" {
		return status
	}
	return status + ": " + e.StderrTail
}

// OutOfBoundsSymlinkError reports a symlink whose target is absolute or resolves
// outside the checked root. It unwraps to ErrPathEscape.
type OutOfBoundsSymlinkError struct {
	// Path is the symlink location relative to the root.
	Path string
	// Target is the raw link target.
	Target string
}

func (e *OutOfBoundsSymlinkError) Error() string {
	return fmt.Sprintf("out of bounds symlink found: %s", e.Path)
}

// Unwrap returns ErrPathEscape so errors.Is(err, ErrPathEscape) matches.
func (e *OutOfBoundsSymlinkError) Unwrap() error { return ErrPathEscape }
