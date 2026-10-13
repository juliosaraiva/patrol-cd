//go:build !unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// HelperArg is the os.Args[1] value that switches a binary into rlimit-helper mode.
const HelperArg = "__patrol-sandbox-exec"

// Run is not supported on non-Unix platforms.
func Run(_ context.Context, c Cmd) (Result, error) {
	return Result{}, fmt.Errorf("sandbox: %s: %w", c.Path, errors.ErrUnsupported)
}

// MaybeHelperMain is a no-op on non-Unix platforms.
func MaybeHelperMain() {}

// SetHelperPath is a no-op on non-Unix platforms.
func SetHelperPath(string) {}
