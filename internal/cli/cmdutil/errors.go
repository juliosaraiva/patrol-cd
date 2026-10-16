package cmdutil

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// Exit codes (upstream util/errors: generic failures exit with 20; usage errors and
// explicit fatal messages with 1).
const (
	ExitOK      = 0
	ExitFatal   = 1
	ExitUsage   = 1
	ExitGeneric = 20
)

// ExitError carries a process exit code. A Silent error prints nothing (the command
// already reported, e.g. printed help). Any error implementing ExitCode() int (and
// optionally IsSilent() bool) is honored the same way, so command packages can define
// their own type.
type ExitError struct {
	Code   int
	Err    error
	Silent bool
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap returns the cause.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit status.
func (e *ExitError) ExitCode() int { return e.Code }

// IsSilent reports whether the message must not be printed.
func (e *ExitError) IsSilent() bool { return e.Silent }

// Fatalf returns an error that exits with status 1 (upstream log.Fatal).
func Fatalf(format string, a ...any) error {
	return &ExitError{Code: ExitFatal, Err: fmt.Errorf(format, a...)}
}

// Silent returns an error that exits with code without printing anything.
func Silent(code int) error { return &ExitError{Code: code, Silent: true} }

// UsageError marks command-line usage errors (exit status 1).
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }

// Unwrap returns the cause.
func (e *UsageError) Unwrap() error { return e.Err }

// ExitCode returns ExitUsage.
func (e *UsageError) ExitCode() int { return ExitUsage }

// Usagef returns a usage error.
func Usagef(format string, a ...any) error {
	return &UsageError{Err: fmt.Errorf(format, a...)}
}

// HelpExit prints the help of c to its output and returns a silent exit 1 (upstream:
// c.HelpFunc()(c, args); os.Exit(1) when arguments are missing).
func HelpExit(c *cobra.Command, args []string) error {
	c.HelpFunc()(c, args)
	return Silent(ExitUsage)
}

// ExactArgs returns a validator printing help and exiting 1 unless n arguments are given.
func ExactArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) != n {
			return HelpExit(c, args)
		}
		return nil
	}
}

// MinimumArgs returns a validator printing help and exiting 1 with fewer than n arguments.
func MinimumArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < n {
			return HelpExit(c, args)
		}
		return nil
	}
}

// RangeArgs returns a validator printing help and exiting 1 outside [lo, hi] arguments.
func RangeArgs(lo, hi int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < lo || len(args) > hi {
			return HelpExit(c, args)
		}
		return nil
	}
}

// NoArgs returns a validator printing help and exiting 1 when arguments are given.
func NoArgs(c *cobra.Command, args []string) error {
	if len(args) != 0 {
		return HelpExit(c, args)
	}
	return nil
}

// ExitCodeOf returns the exit status for err and whether its message is printed.
func ExitCodeOf(err error) (code int, printMessage bool) {
	if err == nil {
		return ExitOK, false
	}
	var coded interface{ ExitCode() int }
	code = ExitGeneric
	if errors.As(err, &coded) {
		code = coded.ExitCode()
	}
	var silent interface{ IsSilent() bool }
	if errors.As(err, &silent) && silent.IsSilent() {
		return code, false
	}
	return code, true
}
