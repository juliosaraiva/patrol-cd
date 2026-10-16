package app

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
)

// Register adds the `app` and `appset` command groups to root (a cli.Registrar). The CLI
// core (internal/cli/cmdutil.Factory) owns the global flags, the local configuration, the
// authenticated API client, prompts and the interactive editor.
func Register(root *cobra.Command, f *cmdutil.Factory) {
	e := newEnv(f)
	root.AddCommand(
		cmdutil.AddKubectlFlags(newAppCommand(e), f),
		cmdutil.AddKubectlFlags(newAppSetCommand(e), f),
	)
}

// env carries the collaborators of every command. Tests replace individual fields.
type env struct {
	// client returns the API client.
	client func(ctx context.Context) (Client, error)
	// prompter asks mandatory questions (sync --preview-changes).
	prompter func() *cmdutil.Prompter
	// promptsEnabled reports the optional-prompt preference (--prompts-enabled).
	promptsEnabled func() bool
	// stdoutIsTerminal decides whether deletion confirmations are asked.
	stdoutIsTerminal func(out io.Writer) bool
	// getenv reads environment variables (PATROL_DIFF_PROGRAM, KUBECTL_EXTERNAL_DIFF).
	getenv func(string) string
	// edit runs the interactive editor loop (`app edit`).
	edit func(ctx context.Context, pattern string, data []byte, save func([]byte) error) error
	// now is the clock used for timestamps.
	now func() time.Time
	// pollInterval is the polling period used when the watch stream is unavailable.
	pollInterval time.Duration
	// watchRetryDelay is the delay before a broken stream is re-opened.
	watchRetryDelay time.Duration
	// httpGet fetches http(s) URLs given to -f / --values-literal-file (bounded).
	httpGet func(ctx context.Context, url string) ([]byte, error)
	// renderer renders local sources client side (--local without server-side generation).
	renderer localRenderer
}

var errNoFactory = errors.New("no CLI factory configured")

func newEnv(f *cmdutil.Factory) *env {
	e := &env{
		stdoutIsTerminal: isTerminalWriter,
		getenv:           os.Getenv,
		now:              time.Now,
		pollInterval:     3 * time.Second,
		watchRetryDelay:  time.Second,
		httpGet:          httpGetBounded,
		renderer:         toolRenderer{},
		client:           func(context.Context) (Client, error) { return nil, errNoFactory },
		promptsEnabled:   func() bool { return false },
		prompter:         func() *cmdutil.Prompter { return cmdutil.NewPrompter(os.Stdin, os.Stderr) },
		edit: func(context.Context, string, []byte, func([]byte) error) error {
			return errNoFactory
		},
	}
	if f == nil {
		return e
	}
	e.client = func(ctx context.Context) (Client, error) {
		c, err := f.NewClient(ctx)
		if err != nil {
			return nil, err
		}
		return NewAPIClient(c), nil
	}
	e.promptsEnabled = f.PromptsEnabled
	e.prompter = f.Prompter
	e.getenv = f.Getenv
	e.edit = f.InteractiveEdit
	if f.Now != nil {
		e.now = f.Now
	}
	return e
}

// optionalPrompt returns the prompter of optional confirmations.
func (e *env) optionalPrompt() cmdutil.OptionalPrompt {
	return cmdutil.OptionalPrompt{Enabled: e.promptsEnabled(), P: e.prompter()}
}

// isTerminalWriter reports whether w is a character device (an interactive terminal).
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
