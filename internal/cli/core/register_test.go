package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/admin"
	"github.com/juliosaraiva/patrol-cd/internal/cli/app"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/core"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
)

// failingProvider records whether core mode tried to connect, and the last request.
type failingProvider struct {
	called bool
	req    transport.Request
}

func (p *failingProvider) Connect(_ context.Context, req transport.Request) (*transport.Connection, error) {
	p.called, p.req = true, req
	return nil, errors.New("test provider: no cluster")
}

// runner returns a CLI runner with the command groups of cmd/patrol and core mode whose
// provider never reaches a cluster.
func runner(t *testing.T) (*clitest.Runner, *failingProvider) {
	t.Helper()
	p := &failingProvider{}
	adminReg := func(root *cobra.Command, f *cmdutil.Factory) {
		admin.Register(root, &admin.Factory{IOStreams: admin.IOStreams{In: f.IO.In, Out: f.IO.Out, ErrOut: f.IO.ErrOut}})
	}
	setProvider := func(_ *cobra.Command, f *cmdutil.Factory) { f.Core = p }
	return clitest.NewRunner(t, app.Register, adminReg, setProvider, core.Register), p
}

func TestConflictingFlags(t *testing.T) {
	t.Parallel()
	r, p := runner(t)
	r.Run("--core", "--server", "cd.example.com", "app", "list").Failed(t, 1, "--core cannot be combined with --server (or PATROL_SERVER)")
	r.Run("--core", "--auth-token", "tok", "app", "list").Failed(t, 1, "core mode cannot be combined with --auth-token (or PATROL_AUTH_TOKEN)")
	r.Run("--core", "--port-forward", "app", "list").Failed(t, 1, "--core cannot be combined with --port-forward")
	r.RunWith(clitest.RunOptions{Env: map[string]string{cmdutil.EnvCore: "true", cmdutil.EnvServer: "cd.example.com"}}, "proj", "list").
		Failed(t, 1, "--core cannot be combined with --server")
	r.RunWith(clitest.RunOptions{Env: map[string]string{cmdutil.EnvCore: "true", cmdutil.EnvAuthToken: "tok"}}, "proj", "list").
		Failed(t, 1, "cannot be combined with --auth-token")
	r.RunWith(clitest.RunOptions{Env: map[string]string{cmdutil.EnvCore: "maybe"}}, "proj", "list").
		Failed(t, 1, "PATROL_CORE: invalid boolean value")
	assert.False(t, p.called, "conflicting flags must be refused before connecting")

	// PATROL_CORE=true selects core mode; PATROL_OPTS wins over it.
	r.RunWith(clitest.RunOptions{Env: map[string]string{cmdutil.EnvCore: "true"}}, "proj", "list").Failed(t, 20, "test provider: no cluster")
	assert.True(t, p.called)
	p.called = false
	r.RunWith(clitest.RunOptions{Env: map[string]string{cmdutil.EnvCore: "true", cmdutil.EnvOpts: "--core=false --server cd.invalid:1 --plaintext"}}, "proj", "list").Failed(t, 20)
	assert.False(t, p.called)

	// A persisted core context with a token on the command line is refused too.
	r.Run("login", "--core").Succeeded(t).Contains(t, "Context 'kubernetes' updated")
	r.Run("--auth-token", "tok", "proj", "list").Failed(t, 1, "core mode cannot be combined with --auth-token")
}

func TestUnsupportedCommands(t *testing.T) {
	t.Parallel()
	r, p := runner(t)
	for _, args := range [][]string{
		{"account", "update-password", "--current-password", "a", "--new-password", "b"},
		{"account", "generate-token"},
		{"account", "delete-token", "id"},
		{"account", "session-token"},
	} {
		r.Run(append([]string{"--core"}, args...)...).
			Failed(t, 20, args[0]+" "+args[1]+" is not available in core mode", "there are no Patrol CD sessions", "kubeconfig credentials")
	}
	r.Run("--core", "admin", "dashboard").Failed(t, 20, "admin dashboard is not available in core mode")
	assert.False(t, p.called)

	// login --core stores a core context without a token; session commands then refuse it.
	r.Run("login", "--core").Succeeded(t)
	r.Run("relogin").Failed(t, 20, "relogin is not available in core mode")
	r.Run("account", "session-token").Failed(t, 20, "account session-token is not available in core mode")
	r.Run("account", "generate-token").Failed(t, 20, "account generate-token is not available in core mode")
	c, err := config.Read(r.ConfigPath())
	require.NoError(t, err)
	require.NotNil(t, c)
	cctx, err := c.ResolveContext("")
	require.NoError(t, err)
	assert.True(t, cctx.Server.Core)
	assert.Empty(t, cctx.User.AuthToken)
	assert.Empty(t, cctx.User.RefreshToken)

	// Another server selected explicitly is not core mode; the dashboard only refuses an
	// explicit --core.
	r.Run("--server", "127.0.0.1:1", "--plaintext", "account", "generate-token").Failed(t, 20).
		NotContains(t, "not available in core mode")
	missing := filepath.Join(t.TempDir(), "no-kubeconfig")
	assert.NotContains(t, r.Run("admin", "dashboard", "--kubeconfig", missing).Stderr, "not available in core mode")
	r.Run("logout", "kubernetes").Succeeded(t).Contains(t, "Logged out from 'kubernetes'")
}

func TestRegistrarKeepsAnExistingProvider(t *testing.T) {
	t.Parallel()
	f := cmdutil.NewFactory(cmdutil.IOStreams{}, func(string) (string, bool) { return "", false })
	p := &failingProvider{}
	f.Core = p
	core.Register(cli.NewRootCommand(f), f)
	assert.Same(t, p, f.Core)
	f2 := cmdutil.NewFactory(cmdutil.IOStreams{}, func(string) (string, bool) { return "", false })
	core.Register(cli.NewRootCommand(f2), f2)
	_, ok := f2.Core.(*core.Provider)
	assert.True(t, ok)
}

// TestKubectlFlagsOnSubcommands: the kubectl client flags of the command groups are
// accepted by every subcommand (before or after the group name), reach the core-mode
// provider, and never clash with a subcommand's own flags.
func TestKubectlFlagsOnSubcommands(t *testing.T) {
	t.Parallel()
	r, p := runner(t)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	for _, args := range [][]string{
		{"app", "list", "-n", "patrol-staging"},
		{"-n", "patrol-staging", "app", "get", "web"},
		{"proj", "list", "--namespace", "patrol-staging"},
		{"repo", "list", "-n", "patrol-staging", "--context", "staging", "--kubeconfig", kc},
		{"appset", "list", "-n", "patrol-staging", "--user", "dev", "--cluster", "c", "--request-timeout", "5s"},
		{"cluster", "list", "-n", "patrol-staging"},
		{"account", "get-user-info", "-n", "patrol-staging"},
		{"cert", "list", "-n", "patrol-staging"},
		{"gpg", "list", "-n", "patrol-staging"},
	} {
		p.called, p.req = false, transport.Request{}
		r.Run(append([]string{"--core"}, args...)...).Failed(t, 20, "test provider: no cluster")
		require.True(t, p.called, "%v", args)
		assert.Equal(t, "patrol-staging", p.req.Kube.Overrides.Context.Namespace, "%v", args)
	}
	r.Run("--core", "repo", "list", "-n", "x", "--context", "staging", "--kubeconfig", kc).Failed(t, 20)
	assert.Equal(t, "staging", p.req.Kube.Overrides.CurrentContext)
	assert.Equal(t, []string{kc}, p.req.Kube.KubeconfigPaths)
	r.Run("--core", "appset", "list", "--user", "dev", "--cluster", "c").Failed(t, 20)
	assert.Equal(t, "dev", p.req.Kube.Overrides.Context.AuthInfo)
	assert.Equal(t, "c", p.req.Kube.Overrides.Context.Cluster)

	// Every command merges its flags without a shorthand clash; -n is the kubectl
	// namespace wherever it is defined, and a subcommand's own --namespace (cluster
	// add/set) takes precedence there.
	f := cmdutil.NewFactory(cmdutil.IOStreams{}, func(string) (string, bool) { return "", false })
	root := cli.NewRootCommand(f, app.Register, core.Register)
	var walk func(c *cobra.Command)
	checked := 0
	walk = func(c *cobra.Command) {
		require.NotPanics(t, func() { _ = c.InheritedFlags(); _ = c.LocalFlags() }, c.CommandPath())
		if fl := c.Flags().ShorthandLookup("n"); fl != nil {
			assert.Equal(t, "namespace", fl.Name, c.CommandPath())
			assert.Contains(t, fl.Usage, "namespace scope for this CLI request", c.CommandPath())
			checked++
		}
		for _, sc := range c.Commands() {
			walk(sc)
		}
	}
	walk(root)
	assert.Greater(t, checked, 50, "subcommands with the kubectl -n flag")
	for _, path := range [][]string{{"cluster", "add"}, {"cluster", "set"}} {
		c, _, err := root.Find(path)
		require.NoError(t, err)
		fl := c.Flags().Lookup("namespace")
		require.NotNil(t, fl)
		assert.Contains(t, fl.Usage, "List of namespaces", "%v keeps its own --namespace", path)
	}
}
