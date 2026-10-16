package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/cli/sso"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// tlsProbeTimeout bounds the TLS probe of login (upstream 30 s).
const tlsProbeTimeout = 30 * time.Second

type ssoFlags struct {
	callback      string
	port          int
	launchBrowser bool
}

func (s *ssoFlags) bind(c *cobra.Command) {
	c.Flags().IntVar(&s.port, "sso-port", sso.DefaultPort, "Port to run local OAuth2 login application")
	c.Flags().StringVar(&s.callback, "callback", "", "Scheme, Host and Port for the callback URL")
	c.Flags().BoolVar(&s.launchBrowser, "sso-launch-browser", true, "Automatically launch the system default browser when performing SSO login")
}

// NewLoginCommand returns `patrol login` (API-058, TEN-110, TEN-130).
func NewLoginCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		ctxName     string
		username    string
		password    string
		useSSO      bool
		skipTestTLS bool
		sf          ssoFlags
	)
	c := &cobra.Command{
		Use:   "login SERVER",
		Short: "Log in to Patrol CD",
		Long:  "Log in to Patrol CD",
		Example: `# Login to Patrol CD using a username and password
patrol login cd.example.com

# Login to Patrol CD using SSO
patrol login cd.example.com --sso

# Configure direct access using Kubernetes API server
patrol login cd.example.com --core`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			pf := f.Opts.PortForward || f.Opts.PortForwardNS != ""
			if len(args) != 1 && !pf && !f.Opts.Core {
				return cmdutil.HelpExit(c, args)
			}
			var server string
			switch {
			case pf:
				server = cmdutil.PortForwardServer
			case f.Opts.Core:
				server = cmdutil.CoreServer
			default:
				server = args[0]
			}
			t, err := f.TargetFromFlags(server)
			if err != nil {
				return err
			}
			if !pf && !f.Opts.Core {
				if err := checkServerScheme(t); err != nil {
					return err
				}
				if !skipTestTLS && !t.Server.PlainText {
					if err := probeAndConfirm(ctx, f, t); err != nil {
						return err
					}
				}
			}
			if ctxName == "" {
				ctxName = server
				if rp := strings.Trim(t.Server.GRPCWebRootPath, "/"); rp != "" {
					ctxName = server + "/" + rp
				}
			}
			var token, refresh string
			if !f.Opts.Core {
				client, err := f.ClientFor(ctx, t)
				if err != nil {
					return err
				}
				if useSSO {
					token, refresh, err = ssoLogin(ctx, f, client, t, sf)
				} else {
					token, err = passwordLogin(ctx, f, client, username, password)
				}
				if err != nil {
					return err
				}
				claims, err := cmdutil.TokenClaims(token)
				if err != nil {
					return err
				}
				output.Printf(f.Out(), "'%s' logged in successfully\n", cmdutil.UserDisplayName(claims))
			}
			cfg, err := f.ReadConfig()
			if err != nil {
				return err
			}
			if cfg == nil {
				cfg = &config.LocalConfig{}
			}
			s := config.Server{
				Server: server, PlainText: t.Server.PlainText, Insecure: t.Server.Insecure,
				GRPCWeb: f.Opts.GRPCWeb, GRPCWebRootPath: t.Server.GRPCWebRootPath, Core: f.Opts.Core,
			}
			if len(t.CAPEM) > 0 {
				s.CACertificateAuthorityData = base64.StdEncoding.EncodeToString(t.CAPEM)
			}
			cfg.UpsertServer(s)
			cfg.UpsertUser(config.User{Name: ctxName, AuthToken: token, RefreshToken: refresh})
			cfg.CurrentContext = ctxName
			cfg.UpsertContext(config.ContextRef{Name: ctxName, User: ctxName, Server: server})
			if err := f.WriteConfig(cfg); err != nil {
				return err
			}
			output.Printf(f.Out(), "Context '%s' updated\n", ctxName)
			return nil
		},
	}
	c.Flags().StringVar(&ctxName, "name", "", "Name to use for the context")
	c.Flags().StringVar(&username, "username", "", "The username of an account to authenticate")
	c.Flags().StringVar(&password, "password", "", "The password of an account to authenticate")
	c.Flags().BoolVar(&useSSO, "sso", false, "Perform SSO login")
	c.Flags().BoolVar(&skipTestTLS, "skip-test-tls", false, "Skip testing whether the server is configured with TLS (this can help when the command hangs for no apparent reason)")
	sf.bind(c)
	return c
}

// checkServerScheme applies an explicit http:// or https:// scheme of the address.
func checkServerScheme(t *cmdutil.Target) error {
	if !strings.Contains(t.Server.Server, "://") {
		return nil
	}
	u, err := url.Parse(t.Server.Server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid server address %q", t.Server.Server)
	}
	switch u.Scheme {
	case "http":
		t.Server.PlainText = true
	case "https":
	default:
		return fmt.Errorf("unsupported scheme %q in server address", u.Scheme)
	}
	return nil
}

// tlsProbe reports whether addr speaks TLS and, if so, the certificate verification
// error (nil when the certificate verifies).
func tlsProbe(ctx context.Context, server string, roots []byte) (bool, error, error) {
	hostport := server
	if strings.Contains(server, "://") {
		u, err := url.Parse(server)
		if err != nil {
			return false, nil, err
		}
		hostport = u.Host
	} else if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(strings.Trim(hostport, "[]"), "443")
	}
	ctx, cancel := context.WithTimeout(ctx, tlsProbeTimeout)
	defer cancel()
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // probe only; verification is tested next
	conn, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		var rh tls.RecordHeaderError
		if errors.As(err, &rh) {
			return false, nil, nil
		}
		return false, nil, err
	}
	_ = conn.Close()
	host, _, _ := net.SplitHostPort(hostport)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if len(roots) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(roots)
		cfg.RootCAs = pool
	}
	conn, err = (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", hostport)
	if err != nil {
		return true, err, nil
	}
	_ = conn.Close()
	return true, nil, nil
}

// probeAndConfirm asks before talking plain HTTP or skipping certificate verification
// (mandatory prompts, as upstream).
func probeAndConfirm(ctx context.Context, f *cmdutil.Factory, t *cmdutil.Target) error {
	isTLS, certErr, err := tlsProbe(ctx, t.Server.Server, t.CAPEM)
	if err != nil {
		return fmt.Errorf("test TLS of %s: %w", t.Server.Server, err)
	}
	if !isTLS {
		ok, err := f.Prompter().Confirm("WARNING: server is not configured with TLS. Proceed (y/n)? ")
		if err != nil || !ok {
			return cmdutil.Silent(cmdutil.ExitFatal)
		}
		t.Server.PlainText = true
		return nil
	}
	if certErr != nil && !t.Server.Insecure {
		ok, err := f.Prompter().Confirm(fmt.Sprintf("WARNING: server certificate had error: %s. Proceed insecurely (y/n)? ", output.Sanitize(certErr.Error())))
		if err != nil || !ok {
			return cmdutil.Silent(cmdutil.ExitFatal)
		}
		t.Server.Insecure = true
	}
	return nil
}

// passwordLogin prompts for missing credentials and creates a local session.
func passwordLogin(ctx context.Context, f *cmdutil.Factory, c *apiclient.Client, username, password string) (string, error) {
	var err error
	if username == "" {
		if username, err = f.Prompter().Input("Username"); err != nil {
			return "", err
		}
	}
	if password == "" {
		if password, err = f.Prompter().Password("Password"); err != nil {
			return "", err
		}
	}
	res, err := c.Login(ctx, username, password)
	if err != nil {
		return "", err
	}
	if res.Token == "" {
		return "", errors.New("the server returned no session token")
	}
	return res.Token, nil
}

// ssoLogin runs the browser flow against the server's identity provider and exchanges the
// ID token for a Patrol session.
func ssoLogin(ctx context.Context, f *cmdutil.Factory, c *apiclient.Client, t *cmdutil.Target, sf ssoFlags) (string, string, error) {
	set, err := c.GetSettings(ctx)
	if err != nil {
		return "", "", fmt.Errorf("get server settings: %w", err)
	}
	p, err := sso.ProviderFromSettings(t.Server.Server, set)
	if err != nil {
		return "", "", err
	}
	hc, err := f.IdPHTTPClient(c, t, p.Issuer)
	if err != nil {
		return "", "", err
	}
	res, err := sso.Login(ctx, p, sso.LoginOptions{
		HTTPClient: hc, Port: sf.port, Callback: sf.callback, LaunchBrowser: sf.launchBrowser,
		OpenBrowser: f.OpenBrowser, Out: f.ErrOut(), Timeout: f.SSOTimeout, Logger: f.Logger(),
	})
	if err != nil {
		return "", "", err
	}
	sess, err := c.ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: res.IDToken, AccessToken: res.AccessToken})
	if err != nil {
		return "", "", fmt.Errorf("exchange the ID token for a Patrol CD session: %w", err)
	}
	return sess.Token, res.RefreshToken, nil
}
