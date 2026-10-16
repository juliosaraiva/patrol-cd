package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
)

// NewLogoutCommand returns `patrol logout CONTEXT` (API-058): the session is revoked on
// the server (DELETE /api/v1/session) and the token removed from the local configuration,
// even when the server cannot be reached.
func NewLogoutCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "logout CONTEXT",
		Short: "Log out from Patrol CD",
		Long:  "Log out from Patrol CD",
		Example: `# Logout from the active Patrol CD context
# This can be helpful for security reasons or when you want to switch between different Patrol CD contexts or accounts.
patrol logout CONTEXT

# Logout from a specific context named 'cd.example.com'
patrol logout cd.example.com
`,
		ValidArgsFunction: completeContexts(f),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmdutil.HelpExit(c, args)
			}
			name := args[0]
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				return cmdutil.Fatalf("Nothing to logout from")
			}
			ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to log out from '%s'?", name))
			if err != nil {
				return err
			}
			if !ok {
				f.Logger().Info(fmt.Sprintf("Logout from '%s' cancelled", name))
				return nil
			}
			cctx, err := cfg.ResolveContext(name)
			if err != nil {
				return cmdutil.Fatalf("Context %s does not exist", name)
			}
			if cctx.User.AuthToken == "" {
				f.Logger().Warn("failed to invalidate token on server: error getting token from local context file")
			} else if !cctx.Server.Core {
				revokeSession(c, f, cctx)
			}
			if !cfg.RemoveToken(cctx.User.Name) {
				return cmdutil.Fatalf("Context %s does not exist", name)
			}
			if err := cfg.Validate(); err != nil {
				return cmdutil.Fatalf("Error in logging out: %s", err)
			}
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			output.Printf(f.Out(), "Logged out from '%s'\n", name)
			return nil
		},
	}
}

func revokeSession(c *cobra.Command, f *cmdutil.Factory, cctx *config.Context) {
	t := &cmdutil.Target{ContextName: cctx.Name, Server: cctx.Server, Token: cctx.User.AuthToken, TokenSource: cmdutil.TokenConfig,
		PortForward: cctx.Server.Server == cmdutil.PortForwardServer}
	client, err := f.ClientFor(c.Context(), t)
	if err == nil {
		_, err = client.Logout(c.Context())
	}
	if err != nil {
		f.Logger().Warn("failed to invalidate token on server", slog.String("error", output.Sanitize(err.Error())))
		return
	}
	f.Logger().Info("token successfully invalidated on server")
}

// NewReloginCommand returns `patrol relogin` (API-058): local sessions log in again
// with the account of the stored token, SSO sessions run the SSO flow again.
func NewReloginCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		password string
		sf       ssoFlags
	)
	c := &cobra.Command{
		Use:   "relogin",
		Short: "Refresh an expired authenticate token",
		Long:  "Refresh an expired authenticate token",
		Example: `
# Reinitiates the login with previous contexts
patrol relogin

# Reinitiates the login with password
patrol relogin --password YOUR_PASSWORD

# Configure direct access using Kubernetes API server
patrol login cd.example.com --core

# If user logged in with - "patrol login cd.example.com" with sso login
# The command - "patrol relogin" will Reinitiates SSO login and updates the server context`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				return cmdutil.HelpExit(c, args)
			}
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				return cmdutil.Fatalf("No context found. Login using `patrol login`")
			}
			cctx, err := cfg.ResolveContext(cfg.CurrentContext)
			if err != nil {
				return err
			}
			if cctx.User.AuthToken == "" {
				return cmdutil.Fatalf("No authentication token found for context '%s'. Login using `patrol login`", cctx.Name)
			}
			claims, err := cmdutil.TokenClaims(cctx.User.AuthToken)
			if err != nil {
				return err
			}
			t := &cmdutil.Target{ContextName: cctx.Name, Server: cctx.Server, PortForward: cctx.Server.Server == cmdutil.PortForwardServer}
			if err := mergeConnectionFlags(f, t); err != nil {
				return err
			}
			client, err := f.ClientFor(c.Context(), t)
			if err != nil {
				return err
			}
			var token, refresh string
			if cmdutil.ClaimString(claims, "kind") == "login" || (cmdutil.ClaimString(claims, "kind") == "" && strings.HasSuffix(cmdutil.ClaimString(claims, "sub"), ":login")) {
				output.Printf(f.Out(), "Relogging in as '%s'\n", cmdutil.UserDisplayName(claims))
				user, _, _ := strings.Cut(cmdutil.ClaimString(claims, "sub"), ":")
				token, err = passwordLogin(c.Context(), f, client, user, password)
			} else {
				output.Println(f.Out(), "Reinitiating SSO login")
				token, refresh, err = ssoLogin(c.Context(), f, client, t, sf)
			}
			if err != nil {
				return err
			}
			cfg.UpsertUser(config.User{Name: cctx.User.Name, AuthToken: token, RefreshToken: refresh})
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			output.Printf(f.Out(), "Context '%s' updated\n", cfg.CurrentContext)
			return nil
		},
	}
	c.Flags().StringVar(&password, "password", "", "The password of an account to authenticate")
	sf.bind(c)
	return c
}

// mergeConnectionFlags overlays the client certificate, header and TLS flags on t.
func mergeConnectionFlags(f *cmdutil.Factory, t *cmdutil.Target) error {
	ft, err := f.TargetFromFlags(t.Server.Server)
	if err != nil {
		return err
	}
	t.Server.PlainText = t.Server.PlainText || ft.Server.PlainText
	t.Server.Insecure = t.Server.Insecure || ft.Server.Insecure
	if ft.Server.GRPCWebRootPath != "" {
		t.Server.GRPCWebRootPath = ft.Server.GRPCWebRootPath
	}
	if len(ft.CAPEM) > 0 {
		t.CAPEM = ft.CAPEM
	} else if t.Server.CACertificateAuthorityData != "" {
		t.CAPEM, _ = decodeB64(t.Server.CACertificateAuthorityData)
	}
	if len(ft.ClientCertPEM) > 0 {
		t.ClientCertPEM, t.ClientKeyPEM = ft.ClientCertPEM, ft.ClientKeyPEM
	}
	return nil
}

// NewContextCommand returns `patrol context [CONTEXT]` (API-059).
func NewContextCommand(f *cmdutil.Factory) *cobra.Command {
	var deletion bool
	c := &cobra.Command{
		Use:     "context [CONTEXT]",
		Aliases: []string{"ctx"},
		Short:   "Switch between contexts",
		Example: `# List Patrol CD Contexts
patrol context

# Switch Patrol CD context
patrol context cd.example.com

# Delete Patrol CD context
patrol context cd.example.com --delete`,
		ValidArgsFunction: completeContexts(f),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				output.Println(f.Out(), "No local configuration found")
				return cmdutil.Silent(cmdutil.ExitFatal)
			}
			if deletion {
				if len(args) == 0 {
					return cmdutil.HelpExit(c, args)
				}
				return deleteContext(f, cfg, args[0])
			}
			if len(args) == 0 {
				return printContexts(f, cfg)
			}
			name := args[0]
			prevFile := filepath.Join(filepath.Dir(f.ConfigPath()), config.PrevContextFile)
			if name == "-" {
				b, err := os.ReadFile(prevFile) //nolint:gosec // file in the user's configuration directory
				if err != nil {
					return fmt.Errorf("read the previous context: %w", err)
				}
				name = strings.TrimSpace(string(b))
			}
			if cfg.CurrentContext == name {
				output.Printf(f.Out(), "Already at context '%s'\n", name)
				return nil
			}
			if _, err := cfg.ResolveContext(name); err != nil {
				return cmdutil.Fatalf("%v", err)
			}
			prev := cfg.CurrentContext
			cfg.CurrentContext = name
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			if err := os.WriteFile(prevFile, []byte(prev), 0o600); err != nil {
				return fmt.Errorf("record the previous context: %w", err)
			}
			output.Printf(f.Out(), "Switched to context '%s'\n", name)
			return nil
		},
	}
	c.Flags().BoolVar(&deletion, "delete", false, "Delete the context instead of switching to it")
	return c
}

func deleteContext(f *cmdutil.Factory, cfg *config.LocalConfig, name string) error {
	server, ok := cfg.RemoveContext(name)
	if !ok {
		return fmt.Errorf("context %s does not exist", name)
	}
	_ = cfg.RemoveUser(name)
	if !cfg.ServerInUse(server, "") {
		_ = cfg.RemoveServer(server)
	}
	if cfg.IsEmpty() && len(cfg.Contexts) == 0 {
		if err := config.Delete(f.ConfigPath()); err != nil {
			return err
		}
	} else {
		if cfg.CurrentContext == name {
			cfg.CurrentContext = ""
		}
		if err := cfg.Validate(); err != nil {
			return errors.New("error in logging out")
		}
		if err := f.WriteConfig(cfg); err != nil {
			return err
		}
	}
	output.Printf(f.Out(), "Context '%s' deleted\n", name)
	return nil
}

func printContexts(f *cmdutil.Factory, cfg *config.LocalConfig) error {
	t := output.NewTable(f.Out())
	t.Row("CURRENT", "NAME", "SERVER")
	for _, ref := range cfg.Contexts {
		cur := " "
		if cfg.CurrentContext == ref.Name {
			cur = "*"
		}
		server := ref.Server
		if ctx, err := cfg.ResolveContext(ref.Name); err != nil {
			f.Logger().Warn(fmt.Sprintf("Context '%s' had error: %v", ref.Name, err))
		} else {
			server = ctx.Server.Server
		}
		t.Row(cur, ref.Name, server)
	}
	return t.Flush()
}

// NewConfigureCommand returns `patrol configure` (API-060).
func NewConfigureCommand(f *cmdutil.Factory) *cobra.Command {
	var promptsEnabled bool
	c := &cobra.Command{
		Use:   "configure",
		Short: "Manage local configuration",
		Example: `# Enable optional interactive prompts
patrol configure --prompts-enabled
patrol configure --prompts-enabled=true

# Disable optional interactive prompts
patrol configure --prompts-enabled=false`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				output.Println(f.Out(), "No local configuration found")
				return cmdutil.Silent(cmdutil.ExitFatal)
			}
			if c.Flags().Changed("prompts-enabled") {
				cfg.PromptsEnabled = promptsEnabled
			}
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			output.Println(f.Out(), "Successfully updated the following configuration settings:")
			output.Printf(f.Out(), "prompts-enabled: %v\n", cfg.PromptsEnabled)
			return nil
		},
	}
	c.Flags().BoolVar(&promptsEnabled, "prompts-enabled", false, "Enable (or disable) optional interactive prompts")
	return c
}
