package stack

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/app"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

// CLIResult is the outcome of one in-process patrol CLI run.
type CLIResult struct {
	Args   []string
	Stdout string
	Stderr string
	Code   int
}

// Succeeded fails the test unless the run exited 0.
func (r CLIResult) Succeeded(t testing.TB) CLIResult {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("patrol %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// CLI runs the patrol CLI in-process as admin against this Stack (--server, --plaintext,
// --auth-token of the admin session; private HOME and configuration file).
func (s *Stack) CLI(t testing.TB, args ...string) CLIResult {
	t.Helper()
	return s.CLIWithToken(t, s.API(t).Token(), args...)
}

// CLIWithToken runs the patrol CLI in-process with a bearer token (empty: none).
func (s *Stack) CLIWithToken(t testing.TB, token string, args ...string) CLIResult {
	t.Helper()
	full := []string{"--server", s.serverAddr, "--plaintext"}
	if token != "" {
		full = append(full, "--auth-token", token)
	}
	return runCLI(t, t.TempDir(), "", append(full, args...))
}

// CLISession is a patrol CLI user with a persistent HOME and configuration file, so state
// written by one command (login contexts, tokens) is read by the next.
type CLISession struct {
	s    *Stack
	home string
	// Stdin is the standard input of the next Run (consumed).
	Stdin string
}

// NewCLISession returns a CLI user without credentials.
func (s *Stack) NewCLISession(t testing.TB) *CLISession {
	t.Helper()
	return &CLISession{s: s, home: t.TempDir()}
}

// Home is the session's HOME directory.
func (c *CLISession) Home() string { return c.home }

// Run runs the patrol CLI with this session's configuration. No server flags are added:
// commands after `patrol login` use the stored context.
func (c *CLISession) Run(t testing.TB, args ...string) CLIResult {
	t.Helper()
	in := c.Stdin
	c.Stdin = ""
	return runCLI(t, c.home, in, args)
}

func runCLI(t testing.TB, home, stdin string, args []string) CLIResult {
	t.Helper()
	env := map[string]string{"HOME": home, "PATH": "", config.EnvConfig: filepath.Join(home, "patrol", "config")}
	var stdout, stderr bytes.Buffer
	f := cmdutil.NewFactory(cmdutil.IOStreams{In: strings.NewReader(stdin), Out: &stdout, ErrOut: &stderr}, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	f.Environ = func() []string {
		out := make([]string, 0, len(env))
		for k, v := range env {
			out = append(out, k+"="+v)
		}
		return out
	}
	f.OpenBrowser = func(string) error { return nil }
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(2*time.Minute))
	defer cancel()
	code := cli.Execute(ctx, f, args, app.Register)
	return CLIResult{Args: args, Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}
