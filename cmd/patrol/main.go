// Command patrol is the Patrol CD command-line interface (docs/architecture/components/cli.md).
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/admin"
	"github.com/juliosaraiva/patrol-cd/internal/cli/app"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/core"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func main() {
	// Re-exec hooks first: the sandbox helper and the Jsonnet evaluator child used by
	// client-side `app ... --local` rendering run this binary with a reserved argument.
	sandbox.MaybeHelperMain()
	jsonnet.MaybeJsonnetMain()
	os.Exit(cli.Main(os.Args[1:], registrars()...))
}

// registrars are the command groups implemented outside internal/cli, then core mode
// (--core transport and the guards of commands needing Patrol CD sessions), which must
// come last.
func registrars() []cli.Registrar {
	return []cli.Registrar{app.Register, registerAdmin, core.Register}
}

// registerAdmin adds `patrol admin` (direct Kubernetes access) with the root's streams and
// prompt default, and maps its errors to the root's exit-code contract.
func registerAdmin(root *cobra.Command, f *cmdutil.Factory) {
	admin.Register(root, &admin.Factory{
		IOStreams:      admin.IOStreams{In: f.IO.In, Out: f.IO.Out, ErrOut: f.IO.ErrOut},
		PromptsEnabled: f.PromptsEnabled(),
		Now:            f.Now,
	})
	for _, c := range root.Commands() {
		if c.Name() == "admin" {
			adaptAdminErrors(c)
		}
	}
}

// adaptAdminErrors converts the errors of the admin tree: admin.ExitCode gives the status
// (an *admin.ExitError's code, else 1) and an error without message is silent.
func adaptAdminErrors(c *cobra.Command) {
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			err := run(cmd, args)
			code := admin.ExitCode(err)
			if code == 0 {
				return nil
			}
			return &cmdutil.ExitError{Code: code, Err: err, Silent: err.Error() == ""}
		}
	}
	for _, sc := range c.Commands() {
		adaptAdminErrors(sc)
	}
}
