package cmdutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/sso"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Special server names stored in contexts (upstream).
const (
	// PortForwardServer is the server name of contexts created with --port-forward.
	PortForwardServer = "port-forward"
	// CoreServer is the server name of contexts created with --core.
	CoreServer = "kubernetes"
)

// maxPEMFileBytes bounds certificate files read from disk.
const maxPEMFileBytes = 1 << 20

// TokenSource tells where the bearer token of a Target comes from.
type TokenSource string

// Token sources.
const (
	TokenNone   TokenSource = ""
	TokenFlag   TokenSource = "flag"
	TokenEnv    TokenSource = "env"
	TokenConfig TokenSource = "config"
)

// Target is a resolved server and credentials.
type Target struct {
	// ContextName is the local configuration context the target comes from ("" when none).
	ContextName string
	// Server holds the address and connection settings.
	Server config.Server
	// Token is the bearer token; RefreshToken the IdP refresh token of an SSO context.
	Token        string
	RefreshToken string
	TokenSource  TokenSource
	// CAPEM, ClientCertPEM and ClientKeyPEM are the TLS material (from --server-crt /
	// --client-crt / --client-crt-key or the context).
	CAPEM         []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
	// PortForward and Core select the transport.
	PortForward bool
	Core        bool
}

// String never prints credentials.
func (t *Target) String() string {
	return fmt.Sprintf("Target{context:%q server:%q tokenSource:%q}", t.ContextName, t.Server.Server, t.TokenSource)
}

// ResolveTarget resolves the server and credentials from the flags, the environment
// and the local configuration (API-051/052): --server > PATROL_SERVER > context;
// --auth-token > PATROL_AUTH_TOKEN > context. Credentials and TLS material of the
// context are only used when the effective server is the context's server, so a token
// is never sent to another server than the one it was obtained from. A requested
// port-forward (--port-forward, --port-forward-namespace) or core mode (--core) is
// itself the effective server ("port-forward" / "kubernetes"): it only reuses a context
// created for that mode, never the credentials of a remote server's context.
func (f *Factory) ResolveTarget() (*Target, error) {
	cfg, err := f.ReadConfig()
	if err != nil {
		return nil, err
	}
	t := &Target{}
	var cctx *config.Context
	if cfg != nil && (f.Opts.Context != "" || cfg.CurrentContext != "") {
		if cctx, err = cfg.ResolveContext(f.Opts.Context); err != nil {
			return nil, err
		}
	} else if f.Opts.Context != "" {
		return nil, fmt.Errorf("Context '%s' undefined", f.Opts.Context) //nolint:staticcheck // upstream wording
	}
	pf := f.Opts.PortForward || f.Opts.PortForwardNS != ""
	useCtx := false
	if cctx != nil {
		t.ContextName = cctx.Name
		if f.contextMatches(cctx, pf) {
			useCtx = true
			t.Server = cctx.Server
			t.Token, t.RefreshToken = cctx.User.AuthToken, cctx.User.RefreshToken
			if t.Token != "" {
				t.TokenSource = TokenConfig
			}
			if t.Server.CACertificateAuthorityData != "" {
				if t.CAPEM, err = base64.StdEncoding.DecodeString(t.Server.CACertificateAuthorityData); err != nil {
					return nil, fmt.Errorf("decode certificate-authority-data of context %s: %w", cctx.Name, err)
				}
			}
			switch {
			case t.Server.ClientCertificateData != "" && t.Server.ClientCertificateKeyData != "":
				if t.ClientCertPEM, err = base64.StdEncoding.DecodeString(t.Server.ClientCertificateData); err != nil {
					return nil, fmt.Errorf("decode client-certificate-data of context %s: %w", cctx.Name, err)
				}
				if t.ClientKeyPEM, err = base64.StdEncoding.DecodeString(t.Server.ClientCertificateKeyData); err != nil {
					return nil, fmt.Errorf("decode client-certificate-key-data of context %s: %w", cctx.Name, err)
				}
			case t.Server.ClientCertificateData != "" || t.Server.ClientCertificateKeyData != "":
				return nil, errors.New("ClientCertificateData and ClientCertificateKeyData must always be specified together")
			}
		} else {
			t.ContextName = ""
			f.Logger().Debug("the server differs from the context's server; the context's credentials are not used", slog.String("context", cctx.Name))
		}
	}
	switch {
	case useCtx:
		if f.Opts.Server != "" && !pf && !f.Opts.Core {
			t.Server.Server = f.Opts.Server
		}
	case f.Opts.Core:
		t.Server.Server = CoreServer
	case pf:
		t.Server.Server = PortForwardServer
	default:
		t.Server.Server = f.Opts.Server
	}
	if tok := strings.TrimSpace(f.Opts.AuthToken); tok != "" {
		t.Token, t.RefreshToken = tok, ""
		t.TokenSource = TokenFlag
		if !f.Changed(FlagAuthToken) {
			t.TokenSource = TokenEnv
		}
	} else if f.argoToken != "" && f.argoServer != "" && sameServer(t.Server.Server, f.argoServer) {
		t.Token, t.RefreshToken, t.TokenSource = f.argoToken, "", TokenEnv
		f.notes = append(f.notes, fmt.Sprintf("%s is honored as a migration alias of %s; set %s instead", EnvArgoAuthToken, EnvAuthToken, EnvAuthToken))
		f.Logger().Warn(f.notes[len(f.notes)-1])
	}
	if err := f.applyConnectionFlags(t); err != nil {
		return nil, err
	}
	t.PortForward = pf || (useCtx && t.Server.Server == PortForwardServer)
	t.Core = f.Opts.Core || (useCtx && t.Server.Core)
	if t.Core {
		if err := f.coreConflicts(t, pf); err != nil {
			return nil, err
		}
	}
	if t.Server.Server == "" && !t.PortForward && !t.Core {
		return nil, errors.New("Patrol CD server address unspecified") //nolint:staticcheck // upstream wording
	}
	return t, nil
}

// coreConflicts refuses core mode combined with a Patrol CD server or token: core mode
// never contacts a server and never sends a Patrol CD token (API-055).
func (f *Factory) coreConflicts(t *Target, portForward bool) error {
	switch {
	case f.Opts.Core && f.Opts.Server != "":
		return Usagef("--%s cannot be combined with --%s (or %s): core mode talks to the Kubernetes API of your kubeconfig context, not to a Patrol CD server", FlagCore, FlagServer, EnvServer)
	case f.Opts.Core && portForward:
		return Usagef("--%s cannot be combined with --%s or --%s: core mode does not use patrol-server", FlagCore, FlagPortForward, FlagPortForwardNS)
	case t.TokenSource == TokenFlag || t.TokenSource == TokenEnv:
		return Usagef("core mode cannot be combined with --%s (or %s): it authenticates with your kubeconfig credentials and never sends a Patrol CD token", FlagAuthToken, EnvAuthToken)
	}
	return nil
}

// contextMatches reports whether the effective server requested by the flags and the
// environment is the server of cctx: a requested core mode matches only a core context,
// a requested port-forward only a "port-forward" context, --server/PATROL_SERVER only
// the same address, and no request matches any context.
func (f *Factory) contextMatches(cctx *config.Context, portForward bool) bool {
	switch {
	case f.Opts.Core:
		return cctx.Server.Core
	case portForward:
		return !cctx.Server.Core && cctx.Server.Server == PortForwardServer
	case f.Opts.Server != "":
		return sameServer(f.Opts.Server, cctx.Server.Server)
	default:
		return true
	}
}

// applyConnectionFlags overlays the TLS and transport flags on t.
func (f *Factory) applyConnectionFlags(t *Target) error {
	if f.Opts.PlainText {
		t.Server.PlainText = true
	}
	if f.Opts.Insecure {
		t.Server.Insecure = true
	}
	if f.Opts.RootPath != "" {
		t.Server.GRPCWebRootPath = f.Opts.RootPath
	}
	if f.Opts.ServerCrt != "" {
		b, err := readBounded(f.Opts.ServerCrt)
		if err != nil {
			return fmt.Errorf("read server certificate: %w", err)
		}
		t.CAPEM = b
	}
	switch {
	case f.Opts.ClientCrt != "" && f.Opts.ClientCrtKey != "":
		c, err := readBounded(f.Opts.ClientCrt)
		if err != nil {
			return fmt.Errorf("read client certificate: %w", err)
		}
		k, err := readBounded(f.Opts.ClientCrtKey)
		if err != nil {
			return fmt.Errorf("read client certificate key: %w", err)
		}
		t.ClientCertPEM, t.ClientKeyPEM = c, k
	case f.Opts.ClientCrt != "" || f.Opts.ClientCrtKey != "":
		return errors.New("--client-crt and --client-crt-key must always be specified together")
	}
	for _, h := range f.Opts.Headers {
		if name, _, ok := strings.Cut(h, ":"); !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("additional headers must be colon(:)-separated: %s", h)
		}
	}
	return nil
}

// TargetFromFlags builds a target for server from the connection flags only (login).
func (f *Factory) TargetFromFlags(server string) (*Target, error) {
	t := &Target{Server: config.Server{Server: server}}
	if err := f.applyConnectionFlags(t); err != nil {
		return nil, err
	}
	t.PortForward = f.Opts.PortForward || f.Opts.PortForwardNS != ""
	t.Core = f.Opts.Core
	return t, nil
}

func readBounded(path string) ([]byte, error) {
	fh, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(io.LimitReader(fh, maxPEMFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPEMFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxPEMFileBytes)
	}
	return b, nil
}

// sameServer compares two server addresses ignoring scheme defaults, case of the host
// and trailing slashes.
func sameServer(a, b string) bool {
	return normalizeServer(a) == normalizeServer(b)
}

func normalizeServer(s string) string {
	s = strings.TrimSpace(strings.TrimRight(s, "/"))
	if !strings.Contains(s, "://") {
		return strings.ToLower(s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return strings.ToLower(s)
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) || (u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return host + strings.TrimRight(u.Path, "/")
}

// Connection returns the transport connection for t (tracked; closed by Close).
func (f *Factory) Connection(ctx context.Context, t *Target) (*transport.Connection, error) {
	req := transport.Request{
		ServerAddr: t.Server.Server, PlainText: t.Server.PlainText, Insecure: t.Server.Insecure,
		Kube: f.KubeOptions(), Logger: f.Logger(),
	}
	p := transport.Direct
	switch {
	case t.Core:
		p = f.Core
		if p == nil {
			p = transport.Unavailable(transport.ErrCoreModeUnavailable)
		}
		req.ServerAddr = ""
	case t.PortForward:
		p = f.PortForward
		if p == nil {
			p = &transport.PortForward{}
		}
		req.ServerAddr = ""
	}
	c, err := p.Connect(ctx, req)
	if err != nil {
		return nil, err
	}
	f.track(c)
	return c, nil
}

// KubeOptions returns the Kubernetes access options (port-forward and core mode).
func (f *Factory) KubeOptions() transport.KubeOptions {
	o := transport.KubeOptions{
		Overrides:      f.KubeOverrides,
		Namespace:      f.Opts.PortForwardNS,
		ServerName:     f.Opts.ServerName,
		RepoServerName: f.Opts.RepoServerName,
		ControllerName: f.Opts.ControllerName,
	}
	if f.Opts.KubeContext != "" {
		o.Overrides.CurrentContext = f.Opts.KubeContext
	}
	o.KubeconfigPaths = f.KubeconfigPaths(f.Kubeconfig)
	return o
}

// HTTPClient builds the HTTP client for t: TLS 1.2+, the context or --server-crt CA (the
// system roots otherwise), the client certificate, verification skipped only with
// --insecure; proxies from the environment.
func HTTPClient(t *Target, insecure bool) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default HTTP transport")
	}
	tr := base.Clone()
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tc.InsecureSkipVerify = true //nolint:gosec // explicit --insecure
	}
	if len(t.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(t.CAPEM) {
			return nil, errors.New("the server certificate data contains no PEM certificate")
		}
		tc.RootCAs = pool
	}
	if len(t.ClientCertPEM) > 0 {
		cert, err := tls.X509KeyPair(t.ClientCertPEM, t.ClientKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	tr.TLSClientConfig = tc
	return &http.Client{Transport: tr}, nil
}

// ClientFor connects to t and returns an API client using t's token.
func (f *Factory) ClientFor(ctx context.Context, t *Target) (*apiclient.Client, error) {
	conn, err := f.Connection(ctx, t)
	if err != nil {
		return nil, err
	}
	hc := conn.HTTPClient
	if hc == nil {
		if hc, err = HTTPClient(t, conn.Insecure); err != nil {
			return nil, err
		}
	}
	if l := f.Logger(); l.Enabled(ctx, slog.LevelDebug) {
		hc = withDebugLog(hc, l)
	}
	token := t.Token
	if conn.AuthDisabled {
		token = ""
	}
	rootPath := t.Server.GRPCWebRootPath
	if t.Core {
		rootPath = "" // the in-process server is not mounted under a root path
	}
	c, err := apiclient.NewClient(&apiclient.ClientOptions{
		ServerAddr:   conn.ServerAddr,
		PlainText:    conn.PlainText,
		Insecure:     conn.Insecure,
		RootPath:     rootPath,
		Headers:      f.Opts.Headers,
		UserAgent:    "patrol-cli/" + f.Version.Version,
		HTTPRetryMax: f.Opts.HTTPRetryMax,
		AuthToken:    token,
		HTTPClient:   hc,
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// NewClient returns the API client of the current target: an expired SSO session with
// a refresh token is renewed first, and renewed session tokens sent by the server are
// stored in the local configuration (when the token came from it).
func (f *Factory) NewClient(ctx context.Context) (*apiclient.Client, error) {
	t, err := f.ResolveTarget()
	if err != nil {
		return nil, err
	}
	if t.TokenSource == TokenConfig && t.RefreshToken != "" && TokenExpired(t.Token, f.now()) {
		if err := f.RefreshSSO(ctx, t); err != nil {
			return nil, err
		}
	}
	c, err := f.ClientFor(ctx, t)
	if err != nil {
		return nil, err
	}
	if t.TokenSource == TokenConfig && t.ContextName != "" {
		name := t.ContextName
		c.OnTokenRenewed(func(tok string) {
			if err := f.storeToken(name, tok, ""); err != nil {
				f.Logger().Warn("cannot store the renewed session token", slog.Any("error", err))
			}
		})
	}
	return c, nil
}

func (f *Factory) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}
	return f.Now()
}

// storeToken replaces the tokens of a context user (refresh unchanged when empty).
func (f *Factory) storeToken(contextName, token, refresh string) error {
	cfg, err := f.ReadConfig()
	if err != nil || cfg == nil {
		return err
	}
	c, err := cfg.ResolveContext(contextName)
	if err != nil {
		return err
	}
	u := c.User
	u.AuthToken = token
	if refresh != "" {
		u.RefreshToken = refresh
	}
	cfg.UpsertUser(u)
	return f.WriteConfig(cfg)
}

// RefreshSSO renews the session of an SSO context with its refresh token: the identity
// provider issues a new ID token, which is exchanged for a Patrol session; both tokens
// are stored.
func (f *Factory) RefreshSSO(ctx context.Context, t *Target) error {
	anon := *t
	anon.Token = ""
	c, err := f.ClientFor(ctx, &anon)
	if err != nil {
		return err
	}
	set, err := c.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("get server settings: %w", err)
	}
	p, err := sso.ProviderFromSettings(c.BaseURL(), set)
	if err != nil {
		return err
	}
	hc, err := f.IdPHTTPClient(c, t, p.Issuer)
	if err != nil {
		return err
	}
	res, err := sso.Refresh(ctx, p, hc, t.RefreshToken)
	if err != nil {
		return fmt.Errorf("%w; run 'patrol relogin' to re-authenticate", err)
	}
	sess, err := c.ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: res.IDToken, AccessToken: res.AccessToken})
	if err != nil {
		return fmt.Errorf("exchange the refreshed ID token: %w", err)
	}
	t.Token, t.RefreshToken = sess.Token, res.RefreshToken
	if t.ContextName != "" {
		if err := f.storeToken(t.ContextName, t.Token, t.RefreshToken); err != nil {
			return fmt.Errorf("store the refreshed session: %w", err)
		}
	}
	return nil
}

// IdPHTTPClient returns the HTTP client used to talk to the identity provider: the
// server's client (its TLS settings) when the issuer is served by the Patrol server (the
// bundled Dex), otherwise a client verifying the provider with the system roots.
func (f *Factory) IdPHTTPClient(c *apiclient.Client, t *Target, issuer string) (*http.Client, error) {
	if u, err := url.Parse(c.BaseURL()); err == nil && sso.SameHost(issuer, u.Host) {
		return HTTPClient(t, t.Server.Insecure)
	}
	return HTTPClient(&Target{}, false)
}

// TokenClaims parses the claims of a JWT without verifying it (display and expiry).
func TokenClaims(token string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser(jwt.WithoutClaimsValidation()).ParseUnverified(token, claims); err != nil {
		return nil, fmt.Errorf("invalid token format: %w", err)
	}
	return claims, nil
}

// TokenExpired reports whether the token has an expiry in the past (or is unparsable).
func TokenExpired(token string, now time.Time) bool {
	claims, err := TokenClaims(token)
	if err != nil {
		return true
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return false
	}
	return !exp.After(now)
}

// UserDisplayName returns the email, name or subject of a token (upstream
// userDisplayName).
func UserDisplayName(claims jwt.MapClaims) string {
	for _, k := range []string{"email", "name", "sub"} {
		if v, ok := claims[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ClaimString returns a string claim.
func ClaimString(claims jwt.MapClaims, key string) string {
	v, _ := claims[key].(string)
	return v
}
