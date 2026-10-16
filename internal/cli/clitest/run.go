package clitest

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

// Result is the outcome of one CLI run.
type Result struct {
	Args   []string
	Stdout string
	Stderr string
	Code   int
}

// Succeeded fails the test unless the run exited 0.
func (r Result) Succeeded(t testing.TB) Result {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("patrol %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// Failed fails the test unless the run exited with code and stderr contains every
// fragment of want.
func (r Result) Failed(t testing.TB, code int, want ...string) Result {
	t.Helper()
	if r.Code != code {
		t.Fatalf("patrol %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), r.Code, code, r.Stdout, r.Stderr)
	}
	for _, w := range want {
		if !strings.Contains(r.Stderr, w) {
			t.Fatalf("patrol %s: stderr does not contain %q:\n%s", strings.Join(r.Args, " "), w, r.Stderr)
		}
	}
	return r
}

// Contains fails the test unless stdout contains every fragment.
func (r Result) Contains(t testing.TB, want ...string) Result {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(r.Stdout, w) {
			t.Fatalf("patrol %s: stdout does not contain %q:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), w, r.Stdout, r.Stderr)
		}
	}
	return r
}

// NotContains fails the test if stdout or stderr contains any fragment.
func (r Result) NotContains(t testing.TB, unwanted ...string) Result {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(r.Stdout, w) || strings.Contains(r.Stderr, w) {
			t.Fatalf("patrol %s: output contains %q:\nstdout:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), w, r.Stdout, r.Stderr)
		}
	}
	return r
}

// RunOptions customize one run.
type RunOptions struct {
	// Stdin is the standard input (empty by default).
	Stdin string
	// Env overrides environment variables for this run; Unset removes some.
	Env   map[string]string
	Unset []string
	// NoServerFlags documents that the arguments carry their own server flags. RunWith
	// never adds flags (only RunAs adds --server, --plaintext/--server-crt and
	// --auth-token), so it has no effect there.
	NoServerFlags bool
	// Factory adjusts the factory before the run (e.g. a custom OpenBrowser).
	Factory func(*cmdutil.Factory)
	// Timeout bounds the run (default 2 minutes).
	Timeout time.Duration
}

// BaseEnv is the environment of every run: HOME is Env.Dir, PATROL_CONFIG points at
// Env.ConfigPath, PATH is empty and nothing else is inherited from the test process.
func (e *Env) BaseEnv() map[string]string {
	env := map[string]string{
		"HOME":           e.Dir,
		"PATH":           "",
		config.EnvConfig: e.ConfigPath(),
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	for k, v := range e.runEnv {
		env[k] = v
	}
	return env
}

// SetEnv sets an environment variable for subsequent runs of this Env.
func (e *Env) SetEnv(key, value string) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.runEnv[key] = value
}

// ConfigPath is the CLI configuration file used by runs.
func (e *Env) ConfigPath() string { return filepath.Join(e.Dir, "patrol", "config") }

// ReadConfig reads the CLI configuration file.
func (e *Env) ReadConfig() *config.LocalConfig {
	e.T.Helper()
	c, err := config.Read(e.ConfigPath())
	e.must(err)
	if c == nil {
		e.T.Fatal("no CLI configuration")
	}
	return c
}

// ServerAddr is the --server value (host:port).
func (e *Env) ServerAddr() string { return e.Addr }

// CAFile writes the server CA to a file (TLS Envs) and returns its path.
func (e *Env) CAFile() string {
	e.T.Helper()
	p := filepath.Join(e.Dir, "server-ca.crt")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	e.must(os.WriteFile(p, e.CAPEM, 0o600))
	return p
}

// ServerFlags are the global flags that reach the server without a stored context.
func (e *Env) ServerFlags() []string {
	f := []string{"--server", e.Addr}
	if e.Options.TLS {
		f = append(f, "--server-crt", e.CAFile())
	} else {
		f = append(f, "--plaintext")
	}
	if p := strings.Trim(e.Options.RootPath, "/"); p != "" {
		f = append(f, "--grpc-web-root-path", "/"+p)
	}
	return f
}

// Run runs the CLI with args and the base environment.
func (e *Env) Run(args ...string) Result { return e.RunWith(RunOptions{}, args...) }

// RunAs runs the CLI with --server flags and --auth-token token (no stored context).
func (e *Env) RunAs(token string, args ...string) Result {
	full := append(append(e.ServerFlags(), "--auth-token", token), args...)
	return e.RunWith(RunOptions{NoServerFlags: true}, full...)
}

// RunWith runs the CLI in-process through the real root command.
func (e *Env) RunWith(o RunOptions, args ...string) Result {
	e.T.Helper()
	return execute(e.ctx, e.BaseEnv(), e.Options.Registrars, o, args)
}

func execute(ctx context.Context, env map[string]string, regs []cli.Registrar, o RunOptions, args []string) Result {
	for k, v := range o.Env {
		env[k] = v
	}
	for _, k := range o.Unset {
		delete(env, k)
	}
	var stdout, stderr bytes.Buffer
	f := cmdutil.NewFactory(cmdutil.IOStreams{In: strings.NewReader(o.Stdin), Out: &stdout, ErrOut: &stderr}, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	f.Environ = func() []string { return Environ(env) }
	f.OpenBrowser = Browse
	f.SSOTimeout = 30 * time.Second
	if o.Factory != nil {
		o.Factory(f)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code := cli.Execute(ctx, f, args, regs...)
	return Result{Args: args, Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

// Runner runs the CLI without a server: help, local configuration, plugins, completion.
// It needs no envtest.
type Runner struct {
	T *testing.T
	// Dir is HOME; the configuration file is Dir/patrol/config.
	Dir string
	// Env is the base environment (HOME, PATH="" and PATROL_CONFIG by default).
	Env map[string]string
	// Registrars add command groups.
	Registrars []cli.Registrar
}

// NewRunner returns a Runner with a private HOME.
func NewRunner(t *testing.T, registrars ...cli.Registrar) *Runner {
	dir := t.TempDir()
	return &Runner{T: t, Dir: dir, Registrars: registrars, Env: map[string]string{
		"HOME": dir, "PATH": "", config.EnvConfig: filepath.Join(dir, "patrol", "config"),
	}}
}

// ConfigPath is the configuration file of the Runner.
func (r *Runner) ConfigPath() string { return r.Env[config.EnvConfig] }

// Run runs the CLI.
func (r *Runner) Run(args ...string) Result { return r.RunWith(RunOptions{}, args...) }

// RunWith runs the CLI with per-run options.
func (r *Runner) RunWith(o RunOptions, args ...string) Result {
	r.T.Helper()
	env := make(map[string]string, len(r.Env))
	for k, v := range r.Env {
		env[k] = v
	}
	return execute(r.T.Context(), env, r.Registrars, o, args)
}

// Environ renders an environment map as KEY=VALUE pairs (sorted).
func Environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Browse is the test "browser" of SSO logins: it follows the authorization URL and its
// redirects (the fake IdP redirects straight to the CLI's loopback callback) in the
// background.
func Browse(url string) error {
	go func() {
		hc := &http.Client{Timeout: 30 * time.Second}
		resp, err := hc.Get(url) //nolint:gosec // test browser following the login URL
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	return nil
}

// Login runs `patrol login` for a local account and fails the test on error.
func (e *Env) Login(user string, extra ...string) Result {
	e.T.Helper()
	args := append([]string{"login", e.Addr, "--username", user, "--password", Password}, e.loginFlags()...)
	return e.Run(append(args, extra...)...).Succeeded(e.T)
}

func (e *Env) loginFlags() []string {
	var f []string
	if e.Options.TLS {
		f = append(f, "--server-crt", e.CAFile())
	} else {
		f = append(f, "--plaintext")
	}
	if p := strings.Trim(e.Options.RootPath, "/"); p != "" {
		f = append(f, "--grpc-web-root-path", "/"+p)
	}
	return f
}

// LoginAs stores a context for user with a freshly issued session token (no login
// round trip) and makes it current; it returns the token.
func (e *Env) LoginAs(user string) string {
	e.T.Helper()
	tok := e.LoginToken(user)
	e.StoreContext(user, tok)
	return tok
}

// StoreContext writes a context named after the server for user/token and makes it
// current.
func (e *Env) StoreContext(user, token string) {
	e.T.Helper()
	cfg, err := config.Read(e.ConfigPath())
	e.must(err)
	if cfg == nil {
		cfg = &config.LocalConfig{}
	}
	srv := config.Server{Server: e.Addr, PlainText: !e.Options.TLS, GRPCWebRootPath: strings.Trim(e.Options.RootPath, "/")}
	if e.Options.TLS {
		srv.CACertificateAuthorityData = base64.StdEncoding.EncodeToString(e.CAPEM)
	}
	if srv.GRPCWebRootPath != "" {
		srv.GRPCWebRootPath = "/" + srv.GRPCWebRootPath
	}
	cfg.UpsertServer(srv)
	cfg.UpsertUser(config.User{Name: e.Addr, AuthToken: token})
	cfg.UpsertContext(config.ContextRef{Name: e.Addr, Server: e.Addr, User: e.Addr})
	cfg.CurrentContext = e.Addr
	e.must(config.Write(cfg, e.ConfigPath()))
}
