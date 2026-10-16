package core

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/server"
)

// Register installs core mode with default options (a cli.Registrar). Add it after the
// other registrars so that the command guards see every command.
func Register(root *cobra.Command, f *cmdutil.Factory) { Registrar(Options{})(root, f) }

// Registrar returns a cli.Registrar that installs a Provider with o on the factory (unless
// one is set) and refuses, in core mode, the commands that need Patrol CD sessions:
// relogin, account update-password / generate-token / delete-token / session-token, and
// admin dashboard (which never serves the UI from an unauthenticated in-process server).
func Registrar(o Options) func(root *cobra.Command, f *cmdutil.Factory) {
	return func(root *cobra.Command, f *cmdutil.Factory) {
		if f.Core == nil {
			opts := o
			if opts.LookupEnv == nil {
				opts.LookupEnv = func(k string) (string, bool) {
					v := f.Getenv(k)
					return v, v != ""
				}
			}
			if opts.Version == "" {
				opts.Version = f.Version.Version
			}
			f.Core = NewProvider(opts)
		}
		for _, g := range guards {
			if c := findCommand(root, g.path...); c != nil {
				guardCommand(c, f, g)
			}
		}
	}
}

// guard describes a command refused in core mode.
type guard struct {
	path []string
	what string
	// explicit: refused only when core mode is requested (--core, PATROL_CORE,
	// PATROL_OPTS), not merely because the selected context is a core context.
	explicit bool
	// current: the command acts on the current context (relogin), not on --patrol-context.
	current bool
	// message overrides the standard message.
	message string
}

var guards = []guard{
	{path: []string{"relogin"}, what: "relogin", current: true},
	{path: []string{"account", "update-password"}, what: "account update-password"},
	{path: []string{"account", "generate-token"}, what: "account generate-token"},
	{path: []string{"account", "delete-token"}, what: "account delete-token"},
	{path: []string{"account", "session-token"}, what: "account session-token"},
	{path: []string{"admin", "dashboard"}, what: "admin dashboard", explicit: true,
		message: "admin dashboard is not available in core mode: Patrol CD never serves the web UI from an unauthenticated in-process server, which any local process could use with your Kubernetes credentials; run it without --core against an installation with patrol-server"},
}

func findCommand(root *cobra.Command, path ...string) *cobra.Command {
	c := root
	for _, name := range path {
		var next *cobra.Command
		for _, sc := range c.Commands() {
			if sc.Name() == name {
				next = sc
				break
			}
		}
		if next == nil {
			return nil
		}
		c = next
	}
	return c
}

func guardCommand(c *cobra.Command, f *cmdutil.Factory, g guard) {
	msg := g.message
	if msg == "" {
		msg = server.CoreUnsupportedMessage(g.what)
	}
	check := func() error {
		if active(f, g) {
			return errors.New(msg)
		}
		return nil
	}
	switch {
	case c.RunE != nil:
		run := c.RunE
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if err := check(); err != nil {
				return err
			}
			return run(cmd, args)
		}
	case c.Run != nil:
		run := c.Run
		c.Run = nil
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if err := check(); err != nil {
				return err
			}
			run(cmd, args)
			return nil
		}
	}
}

// active reports whether core mode applies to a guarded command: requested explicitly, or
// (unless the guard is explicit-only) the context the command uses is a core context and
// no server or port-forward is requested instead.
func active(f *cmdutil.Factory, g guard) bool {
	if f.Opts.Core {
		return true
	}
	if g.explicit || f.Opts.Server != "" || f.Opts.PortForward || f.Opts.PortForwardNS != "" {
		return false
	}
	cfg, err := f.ReadConfig()
	if err != nil || cfg == nil {
		return false
	}
	name := f.Opts.Context
	if g.current {
		name = ""
	}
	cctx, err := cfg.ResolveContext(name)
	return err == nil && cctx.Server.Core
}
