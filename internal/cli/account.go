package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// printOpFmt is the label/value layout of detail views (upstream printOpFmtStr).
const printOpFmt = "%-20s%s\n"

// NewAccountCommand returns `patrol account` (API-073, TEN-111).
func NewAccountCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "account",
		Short: "Manage account settings",
		Example: `  # List accounts
  patrol account list

  # Update the current user's password
  patrol account update-password

  # Can I sync any app?
  patrol account can-i sync applications '*'

  # Get User information
  patrol account get-user-info`,
		RunE: func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	c.AddCommand(
		newAccountUpdatePasswordCommand(f),
		newAccountGetUserInfoCommand(f),
		newAccountCanICommand(f),
		newAccountListCommand(f),
		newAccountGenerateTokenCommand(f),
		newAccountGetCommand(f),
		newAccountDeleteTokenCommand(f),
		newAccountSessionTokenCommand(f),
		newBcryptCommand(f),
	)
	return c
}

func currentAccount(c *cobra.Command, client *apiclient.Client) (*apiclient.UserInfo, error) {
	ui, err := client.UserInfo(c.Context())
	if err != nil {
		return nil, err
	}
	return ui, nil
}

func newAccountUpdatePasswordCommand(f *cmdutil.Factory) *cobra.Command {
	var account, currentPassword, newPassword string
	c := &cobra.Command{
		Use:   "update-password",
		Short: "Update an account's password",
		Long: `
This command can be used to update the password of the currently logged on
user, or an arbitrary local user account when the currently logged on user
has appropriate RBAC permissions to change other accounts.
`,
		Example: `
	# Update the current user's password
	patrol account update-password

	# Update the password for user foobar
	patrol account update-password --account foobar
`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			ui, err := currentAccount(c, client)
			if err != nil {
				return err
			}
			claims, _ := cmdutil.TokenClaims(client.Token())
			local := cmdutil.ClaimString(claims, "kind") == "login" || cmdutil.ClaimString(claims, "kind") == "apiKey"
			if local && currentPassword == "" {
				if currentPassword, err = f.Prompter().Password(fmt.Sprintf("*** Enter password of currently logged in user (%s)", ui.Username)); err != nil {
					return err
				}
			}
			if account == "" {
				account = ui.Username
			}
			if newPassword == "" {
				if newPassword, err = f.Prompter().NewPassword(account); err != nil {
					return err
				}
			}
			if err := client.UpdatePassword(c.Context(), &apiclient.UpdatePasswordRequest{Name: account, CurrentPassword: currentPassword, NewPassword: newPassword}); err != nil {
				return err
			}
			output.Println(f.Out(), "Password updated")
			if account != ui.Username || cmdutil.ClaimString(claims, "kind") != "login" {
				return nil
			}
			// The session of the old password is revoked: log in again and store it.
			t, err := f.ResolveTarget()
			if err != nil || t.TokenSource != cmdutil.TokenConfig || t.ContextName == "" {
				return err
			}
			res, err := loginAfterPasswordChange(c.Context(), client, account, newPassword)
			if err != nil {
				return err
			}
			cfg, err := f.ReadConfig()
			if err != nil || cfg == nil {
				return err
			}
			cctx, err := cfg.ResolveContext(t.ContextName)
			if err != nil {
				return err
			}
			cfg.UpsertUser(config.User{Name: cctx.User.Name, AuthToken: res.Token})
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			output.Printf(f.Out(), "Context '%s' updated\n", t.ContextName)
			return nil
		},
	}
	c.Flags().StringVar(&currentPassword, "current-password", "", "Password of the currently logged on user")
	c.Flags().StringVar(&newPassword, "new-password", "", "New password you want to update to")
	c.Flags().StringVar(&account, "account", "", "An account name that should be updated. Defaults to current user account")
	return c
}

func newAccountGetUserInfoCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:     "get-user-info",
		Short:   "Get user info",
		Aliases: []string{"whoami"},
		Example: `  # Get User information for the currently logged-in user (see 'patrol login')
  patrol account get-user-info

  # Get User information in yaml format
  patrol account get-user-info -o yaml`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if format != "" && format != output.YAML && format != output.JSON {
				return cmdutil.Fatalf("Unknown output format: %s", format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			ui, err := client.UserInfo(c.Context())
			if err != nil {
				return err
			}
			switch format {
			case output.YAML, output.JSON:
				return output.PrintResource(f.Out(), ui, format)
			}
			output.Printf(f.Out(), "Logged In: %v\n", ui.LoggedIn)
			if ui.LoggedIn {
				output.Printf(f.Out(), "Username: %s\n", ui.Username)
				output.Printf(f.Out(), "Issuer: %s\n", ui.Iss)
				output.Printf(f.Out(), "Groups: %v\n", strings.Join(ui.Groups, ","))
			}
			return nil
		},
	}
	c.Flags().StringVarP(&format, "output", "o", "", "Output format. One of: yaml, json")
	return c
}

func newAccountCanICommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "can-i ACTION RESOURCE SUBRESOURCE",
		Short: "Can I",
		Example: fmt.Sprintf(`
# Can I sync any app?
patrol account can-i sync applications '*'

# Can I update a project?
patrol account can-i update projects 'default'

# Can I create a cluster?
patrol account can-i create clusters '*'

Actions: %v
Resources: %v
`, rbac.Actions(), rbac.Resources()),
		Args: cmdutil.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			ok, err := client.CanI(c.Context(), args[1], args[0], args[2])
			if err != nil {
				return err
			}
			output.Println(f.Out(), map[bool]string{true: "yes", false: "no"}[ok])
			return nil
		},
	}
}

func newAccountListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:     "list",
		Short:   "List accounts",
		Example: "patrol account list",
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Name, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListAccounts(c.Context())
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			case output.Name:
				for _, a := range l.Items {
					output.Println(f.Out(), a.Name)
				}
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("NAME", "ENABLED", "CAPABILITIES")
			for _, a := range l.Items {
				t.Row(a.Name, a.Enabled, strings.Join(a.Capabilities, ", "))
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|name")
	return c
}

func newAccountGetCommand(f *cmdutil.Factory) *cobra.Command {
	var format, account string
	c := &cobra.Command{
		Use:   "get",
		Short: "Get account details",
		Example: `# Get the currently logged in account details
patrol account get

# Get details for an account by name
patrol account get --account <account-name>`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Name, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if account == "" {
				ui, err := currentAccount(c, client)
				if err != nil {
					return err
				}
				account = ui.Username
			}
			acc, err := client.GetAccount(c.Context(), account)
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResource(f.Out(), acc, format)
			case output.Name:
				output.Println(f.Out(), acc.Name)
				return nil
			}
			return printAccountDetails(f, acc)
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|name")
	c.Flags().StringVarP(&account, "account", "a", "", "Account name. Defaults to the current account.")
	return c
}

func printAccountDetails(f *cmdutil.Factory, acc *apiclient.Account) error {
	w := f.Out()
	output.Printf(w, printOpFmt, "Name:", acc.Name)
	output.Printf(w, printOpFmt, "Enabled:", strconv.FormatBool(acc.Enabled))
	output.Printf(w, printOpFmt, "Capabilities:", strings.Join(acc.Capabilities, ", "))
	_, _ = fmt.Fprintln(w, "\nTokens:")
	if len(acc.Tokens) == 0 {
		_, _ = fmt.Fprintln(w, "NONE")
		return nil
	}
	t := output.NewTable(w)
	t.Row("ID", "ISSUED AT", "EXPIRING AT")
	for _, tok := range acc.Tokens {
		t.Row(tok.ID, formatUnix(tok.IssuedAt), formatExpiry(tok.ExpiresAt, time.Now()))
	}
	return t.Flush()
}

var durationRE = regexp.MustCompile(`^(\d+)([smhd])$`)

// parseDuration accepts "<n><unit>" with s, m, h or d (upstream argoproj/pkg time) and Go
// durations ("1h30m").
func parseDuration(s string) (time.Duration, error) {
	if m := durationRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, err
		}
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		if n > int64((1<<63-1)/unit) {
			return 0, fmt.Errorf("duration %s is too large", s)
		}
		return time.Duration(n) * unit, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("Invalid since format '%s'. Expected format <duration><unit> (e.g. 3h)", s) //nolint:staticcheck // upstream wording
	}
	return d, nil
}

func newAccountGenerateTokenCommand(f *cmdutil.Factory) *cobra.Command {
	var account, expiresIn, id string
	c := &cobra.Command{
		Use:   "generate-token",
		Short: "Generate account token",
		Example: `# Generate token for the currently logged in account
patrol account generate-token

# Generate token for the account with the specified name
patrol account generate-token --account <account-name>`,
		RunE: func(c *cobra.Command, _ []string) error {
			d, err := parseDuration(expiresIn)
			if err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if account == "" {
				ui, err := currentAccount(c, client)
				if err != nil {
					return err
				}
				account = ui.Username
			}
			res, err := client.CreateAccountToken(c.Context(), &apiclient.CreateAccountTokenRequest{Name: account, ID: id, ExpiresIn: int64(d.Seconds())})
			if err != nil {
				return err
			}
			// Emitting the token is the purpose of this command.
			_, _ = fmt.Fprintln(f.Out(), res.Token)
			return nil
		},
	}
	c.Flags().StringVarP(&account, "account", "a", "", "Account name. Defaults to the current account.")
	c.Flags().StringVarP(&expiresIn, "expires-in", "e", "0s", "Duration before the token will expire. (Default: No expiration)")
	c.Flags().StringVar(&id, "id", "", "Optional token id. Fall back to uuid if not value specified.")
	return c
}

func newAccountDeleteTokenCommand(f *cmdutil.Factory) *cobra.Command {
	var account string
	c := &cobra.Command{
		Use:   "delete-token",
		Short: "Deletes account token",
		Example: `# Delete token of the currently logged in account
patrol account delete-token ID

# Delete token of the account with the specified name
patrol account delete-token --account <account-name> ID`,
		Args: cmdutil.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id := args[0]
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if account == "" {
				ui, err := currentAccount(c, client)
				if err != nil {
					return err
				}
				account = ui.Username
			}
			ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to delete '%s' token? [y/n]", id))
			if err != nil {
				return err
			}
			if !ok {
				output.Printf(f.Out(), "The command to delete '%s' was cancelled.\n", id)
				return nil
			}
			return client.DeleteAccountToken(c.Context(), account, id)
		},
	}
	c.Flags().StringVarP(&account, "account", "a", "", "Account name. Defaults to the current account.")
	return c
}

func newAccountSessionTokenCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "session-token",
		Short: "Display current session token",
		Long: `Display the current session token for authentication.

Automatically refreshes expired tokens using refresh token (SSO users).
For local users: Shows current token (manual relogin needed if expired)`,
		Example: `# Display current session token (automatically refreshes if needed)
patrol account session-token

# Show detailed token information
patrol account session-token -o json

# Use in scripts
export PATROL_AUTH_TOKEN=$(patrol account session-token)
curl -H "Authorization: Bearer $PATROL_AUTH_TOKEN" $PATROL_SERVER/api/v1/applications`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if format != "" && format != output.JSON {
				return output.UnknownFormat(format)
			}
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				return cmdutil.Fatalf("No configuration found. Please login first with 'patrol login'")
			}
			cctx, err := cfg.ResolveContext(f.Opts.Context)
			if err != nil {
				return cmdutil.Fatalf("No context found. Please login first with 'patrol login'")
			}
			if cctx.User.AuthToken == "" {
				return cmdutil.Fatalf("No authentication token found. Please login first with 'patrol login'")
			}
			now := time.Now
			if f.Now != nil {
				now = f.Now
			}
			if cctx.User.RefreshToken != "" && cmdutil.TokenExpired(cctx.User.AuthToken, now()) {
				t := &cmdutil.Target{ContextName: cctx.Name, Server: cctx.Server, Token: cctx.User.AuthToken, RefreshToken: cctx.User.RefreshToken, TokenSource: cmdutil.TokenConfig}
				if err := mergeConnectionFlags(f, t); err != nil {
					return err
				}
				if err := f.RefreshSSO(c.Context(), t); err != nil {
					return cmdutil.Fatalf("Refresh token is invalid or expired. Please run 'patrol relogin' to re-authenticate")
				}
				cctx.User.AuthToken = t.Token
				cctx.User.RefreshToken = t.RefreshToken
			}
			claims, err := cmdutil.TokenClaims(cctx.User.AuthToken)
			if err != nil {
				return cmdutil.Fatalf("Invalid token format. Please run 'patrol relogin'")
			}
			if exp, err := claims.GetExpirationTime(); err != nil || (exp != nil && !exp.After(now())) {
				return cmdutil.Fatalf("Token is invalid or expired. Please run 'patrol relogin'")
			}
			if format != output.JSON {
				// Emitting the token is the purpose of this command.
				_, _ = fmt.Fprintln(f.Out(), cctx.User.AuthToken)
				return nil
			}
			info := map[string]any{
				"type":              "local",
				"issuer":            cmdutil.ClaimString(claims, "iss"),
				"username":          strings.SplitN(cmdutil.ClaimString(claims, "sub"), ":", 2)[0],
				"token":             cctx.User.AuthToken,
				"has_refresh_token": cctx.User.RefreshToken != "",
			}
			if k := cmdutil.ClaimString(claims, "kind"); k == "sso" || k == "external" {
				info["type"] = "sso"
			}
			if iat, err := claims.GetIssuedAt(); err == nil && iat != nil {
				info["issued_at"] = iat.UTC().Format(time.RFC3339)
			}
			if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
				info["expires_at"] = exp.UTC().Format(time.RFC3339)
			}
			return output.PrintJSON(f.Out(), info)
		},
	}
	c.Flags().StringVarP(&format, "output", "o", "", "Output format (json)")
	return c
}

func newBcryptCommand(f *cmdutil.Factory) *cobra.Command {
	var password string
	c := &cobra.Command{
		Use:   "bcrypt",
		Short: "Generate bcrypt hash for any password",
		Example: `# Generate bcrypt hash for any password
patrol account bcrypt --password YOUR_PASSWORD

# Prompt for password input
patrol account bcrypt

# Read password from stdin
echo -e "password" | patrol account bcrypt`,
		Args: cmdutil.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var err error
			if password == "" {
				if password, err = f.Prompter().Password("Password"); err != nil {
					return err
				}
			}
			if password == "" {
				return errors.New("blank passwords are not allowed")
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				return cmdutil.Fatalf("Failed to generate bcrypt hash: %v", err)
			}
			_, _ = f.Out().Write(hash)
			return nil
		},
	}
	c.Flags().StringVar(&password, "password", "", "Password for which bcrypt hash is generated")
	return c
}

// loginAfterPasswordChange logs in with a password that was just changed. The server
// applies the change through its settings watch, so a login racing it is retried a few
// times with backoff (about 3 s in total) while it is rejected as unauthenticated.
func loginAfterPasswordChange(ctx context.Context, client *apiclient.Client, user, password string) (*apiclient.SessionResponse, error) {
	backoff := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		res, err := client.Login(ctx, user, password)
		if err == nil || !apiclient.IsUnauthenticated(err) || attempt == 5 {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}
