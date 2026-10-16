// Package cli implements the patrol command-line interface (docs/architecture/components/cli.md):
// the root command with its global flags, plugin dispatch, shell completion and the
// platform command groups (login, logout, relogin, context, configure, version, account,
// cert, gpg, repo, repocreds, cluster, proj). Other command groups (app, appset, admin)
// are added through Registrars.
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
)

// CommandName is the CLI binary name.
const CommandName = "patrol"

// Registrar adds commands to the root command (e.g. app.Register).
type Registrar func(root *cobra.Command, f *cmdutil.Factory)

func init() {
	// Run every PersistentPreRun in the command chain (the root's sets up logging and
	// validates PATROL_OPTS even when a group defines its own hook).
	cobra.EnableTraverseRunHooks = true
}

// NewRootCommand returns the patrol root command bound to f.
func NewRootCommand(f *cmdutil.Factory, registrars ...Registrar) *cobra.Command {
	root := &cobra.Command{
		Use:   CommandName,
		Short: "patrol controls a Patrol CD server",
		Run: func(c *cobra.Command, args []string) {
			c.HelpFunc()(c, args)
		},
		DisableAutoGenTag: true,
		SilenceUsage:      true,
		SilenceErrors:     true,
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return listPlugins(f), cobra.ShellCompDirectiveNoFileComp
		},
		PersistentPreRunE: func(*cobra.Command, []string) error { return f.Setup() },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetIn(f.IO.In)
	root.SetOut(f.Out())
	root.SetErr(f.ErrOut())
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return &cmdutil.UsageError{Err: fmt.Errorf("%w\nRun '%s --help' for usage.", err, c.CommandPath())} //nolint:staticcheck // cobra's usage hint
	})
	f.BindFlags(root.PersistentFlags())

	root.AddCommand(
		NewCompletionCommand(f),
		cmdutil.AddKubectlFlags(NewVersionCommand(f), f),
		cmdutil.AddKubectlFlags(NewClusterCommand(f), f),
		NewLoginCommand(f),
		NewReloginCommand(f),
		cmdutil.AddKubectlFlags(NewRepoCommand(f), f),
		cmdutil.AddKubectlFlags(NewRepoCredsCommand(f), f),
		NewContextCommand(f),
		cmdutil.AddKubectlFlags(NewProjectCommand(f), f),
		cmdutil.AddKubectlFlags(NewAccountCommand(f), f),
		NewLogoutCommand(f),
		cmdutil.AddKubectlFlags(NewCertCommand(f), f),
		cmdutil.AddKubectlFlags(NewGPGCommand(f), f),
		cmdutil.AddKubectlFlags(NewConfigureCommand(f), f),
	)
	for _, r := range registrars {
		if r != nil {
			r(root, f)
		}
	}
	return root
}

// Execute runs the CLI with args (without the program name) and returns the exit code:
// 0 on success, the code of an error carrying one (ExitCode() int), 1 for usage errors
// and 20 for other failures (upstream). Unknown commands are dispatched to
// patrol-<name> plugins on PATH.
func Execute(ctx context.Context, f *cmdutil.Factory, args []string, registrars ...Registrar) int {
	root := NewRootCommand(f, registrars...)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if cerr := f.Close(); cerr != nil {
		f.Logger().Debug("closing connections failed", "error", cerr)
	}
	if err == nil {
		return cmdutil.ExitOK
	}
	if isUnknownCommand(err) && len(args) > 0 {
		code, handled := runPlugin(ctx, f, args)
		if handled {
			return code
		}
		_, _ = fmt.Fprintf(f.ErrOut(), "Error: %s\nRun '%s --help' for usage.\n", output.Sanitize(err.Error()), CommandName)
		return cmdutil.ExitUsage
	}
	code, printMsg := cmdutil.ExitCodeOf(err)
	if printMsg {
		_, _ = fmt.Fprintf(f.ErrOut(), "Error: %s\n", output.SanitizeMultiline(err.Error()))
	}
	return code
}

// Main runs the CLI with the process environment and returns the exit code.
func Main(args []string, registrars ...Registrar) int {
	f := cmdutil.NewFactory(cmdutil.SystemIO(), os.LookupEnv)
	return Execute(context.Background(), f, args, registrars...)
}

func isUnknownCommand(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown command")
}
