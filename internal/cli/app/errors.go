package app

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Exit codes (upstream util/errors: generic errors exit 20; `app diff` documents 2 for
// general errors and 1 for a diff).
const (
	exitUsage     = cmdutil.ExitUsage
	exitDiffError = 2
	exitDiffFound = 1
)

// usage prints the help of c and returns a silent exit 1 (upstream: help and os.Exit(1)
// when required arguments are missing).
func usage(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) }

// fatal returns an error exiting with status 1 (upstream log.Fatal).
func fatal(format string, a ...any) error { return cmdutil.Fatalf(format, a...) }

// fatalErr wraps err to exit with status 1 (upstream log.Fatalf with an error).
func fatalErr(err error) error { return &cmdutil.ExitError{Code: cmdutil.ExitFatal, Err: err} }

// fatalGeneric returns an error exiting with status 20 (upstream errors.Fatal(ErrorGeneric)).
func fatalGeneric(format string, a ...any) error {
	return &cmdutil.ExitError{Code: cmdutil.ExitGeneric, Err: fmt.Errorf(format, a...)}
}

// silentExit returns an error that exits with code without a message.
func silentExit(code int) error { return cmdutil.Silent(code) }

// withExitCode wraps err (unless it already carries an exit code).
func withExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return err
	}
	return &cmdutil.ExitError{Code: code, Err: err}
}

// asAPIError reports whether err is (or wraps) an *apiclient.APIError.
func asAPIError(err error, target **apiclient.APIError) bool {
	return errors.As(err, target)
}
