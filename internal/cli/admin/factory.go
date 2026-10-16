package admin

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"k8s.io/client-go/rest"
)

// IOStreams are the standard streams of a command.
type IOStreams struct {
	// In is read for interactive prompts and "-" (stdin) inputs. Nil means os.Stdin.
	In io.Reader
	// Out receives command output. Nil means os.Stdout.
	Out io.Writer
	// ErrOut receives warnings, progress and diagnostics. Nil means os.Stderr.
	ErrOut io.Writer
}

// Factory is the narrow contract between the CLI root (internal/cli) and the "patrol admin"
// command tree. The admin commands talk to Kubernetes directly (never to the Patrol API
// server), so the factory supplies only streams, logging and, optionally, the Kubernetes
// connection.
//
// The admin command registers the kubectl client flags (--kubeconfig, --context, --cluster,
// --user, -n/--namespace, --server, --token, --as, ...) as persistent flags of "admin", as
// upstream registers them on each admin subcommand; they shadow root flags of the same name
// (for example --server) inside the admin tree only.
//
// Contract for the root command:
//   - call Register(root, f) once;
//   - run the tree with ExecuteContext so commands honor cancellation (SIGINT);
//   - map returned errors with ExitCode: an *ExitError carries the exit status (1 for
//     "rbac can" answering "No", 2 for blocking incompatibilities of argocd-import, ...) and
//     may carry no message (the command already printed its result); any other error is
//     printed and exits with status 1.
type Factory struct {
	IOStreams

	// Logger receives diagnostics. Nil logs warnings and errors as text to ErrOut.
	Logger *slog.Logger

	// RESTConfig, when set, replaces kubeconfig loading: it returns the REST configuration
	// of the control-plane cluster and its default namespace. -n/--namespace still selects
	// the namespace when given. Used by tests and by in-process wiring.
	RESTConfig func() (*rest.Config, string, error)

	// PromptsEnabled is the default of --prompts-enabled (the local CLI configuration value,
	// false when unset).
	PromptsEnabled bool

	// Now returns the current time (nil: time.Now).
	Now func() time.Time
}

func (f *Factory) in() io.Reader {
	if f == nil || f.In == nil {
		return os.Stdin
	}
	return f.In
}

func (f *Factory) out() io.Writer {
	if f == nil || f.Out == nil {
		return os.Stdout
	}
	return f.Out
}

func (f *Factory) errOut() io.Writer {
	if f == nil || f.ErrOut == nil {
		return os.Stderr
	}
	return f.ErrOut
}

func (f *Factory) logger() *slog.Logger {
	if f != nil && f.Logger != nil {
		return f.Logger
	}
	return slog.New(slog.NewTextHandler(f.errOut(), &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (f *Factory) now() time.Time {
	if f != nil && f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// ExitError ends a command with a specific exit status. Err may be nil when the command
// already printed its outcome (for example "rbac can" printing "No").
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit status for an error returned by an admin command: 0 for
// nil, the code of an *ExitError, else 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 1
}

// exitf returns an *ExitError with code and a formatted message.
func exitWith(code int, err error) error { return &ExitError{Code: code, Err: err} }
