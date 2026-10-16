package cmdutil

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

// TestResolveTargetScopesContextCredentials checks that a context's credentials are only
// used for the server they were issued by: a requested port-forward or core mode is its
// own effective server and never reuses a remote context's token (refresh token, CA).
func TestResolveTargetScopesContextCredentials(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config")
	cfg := &config.LocalConfig{CurrentContext: "cd.example.com"}
	for _, c := range []struct {
		srv        config.Server
		tok, renew string
	}{
		{config.Server{Server: "cd.example.com"}, "remote-tok", "remote-refresh"},
		{config.Server{Server: PortForwardServer, Insecure: true}, "pf-tok", ""},
		{config.Server{Server: CoreServer, Core: true}, "", ""},
	} {
		cfg.UpsertServer(c.srv)
		cfg.UpsertUser(config.User{Name: c.srv.Server, AuthToken: c.tok, RefreshToken: c.renew})
		cfg.UpsertContext(config.ContextRef{Name: c.srv.Server, Server: c.srv.Server, User: c.srv.Server})
	}
	if err := config.Write(cfg, path); err != nil {
		t.Fatal(err)
	}
	type want struct {
		ctx, server, token, refresh string
		src                         TokenSource
		pf, core                    bool
	}
	remote := want{ctx: "cd.example.com", server: "cd.example.com", token: "remote-tok", refresh: "remote-refresh", src: TokenConfig}
	cases := []struct {
		name string
		opts GlobalOptions
		want want
	}{
		{"current context", GlobalOptions{}, remote},
		{"same server", GlobalOptions{Server: "https://cd.example.com:443/"}, func() want { w := remote; w.server = "https://cd.example.com:443/"; return w }()},
		{"other server", GlobalOptions{Server: "evil.example.com"}, want{server: "evil.example.com"}},
		{"--port-forward with a remote context", GlobalOptions{PortForward: true}, want{server: PortForwardServer, pf: true}},
		{"--port-forward-namespace with a remote context", GlobalOptions{PortForwardNS: "patrol"}, want{server: PortForwardServer, pf: true}},
		{"--port-forward and --server of the remote context", GlobalOptions{PortForward: true, Server: "cd.example.com"}, want{server: PortForwardServer, pf: true}},
		{"--port-forward with the port-forward context", GlobalOptions{PortForward: true, Context: PortForwardServer}, want{ctx: PortForwardServer, server: PortForwardServer, token: "pf-tok", src: TokenConfig, pf: true}},
		{"port-forward context without flags", GlobalOptions{Context: PortForwardServer}, want{ctx: PortForwardServer, server: PortForwardServer, token: "pf-tok", src: TokenConfig, pf: true}},
		{"--server with the port-forward context", GlobalOptions{Context: PortForwardServer, Server: "cd.example.com"}, want{server: "cd.example.com"}},
		{"--core with a remote context", GlobalOptions{Core: true}, want{server: CoreServer, core: true}},
		{"--core with the port-forward context", GlobalOptions{Core: true, Context: PortForwardServer}, want{server: CoreServer, core: true}},
		{"--core with the core context", GlobalOptions{Core: true, Context: CoreServer}, want{ctx: CoreServer, server: CoreServer, core: true}},
		{"--port-forward with the core context", GlobalOptions{PortForward: true, Context: CoreServer}, want{server: PortForwardServer, pf: true}},
		{"explicit token with --port-forward", GlobalOptions{PortForward: true, AuthToken: "explicit"}, want{server: PortForwardServer, token: "explicit", src: TokenEnv, pf: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFactory(IOStreams{Out: io.Discard, ErrOut: io.Discard}, func(string) (string, bool) { return "", false })
			f.Opts = tc.opts
			f.Opts.ConfigPath = path
			got, err := f.ResolveTarget()
			if err != nil {
				t.Fatal(err)
			}
			g := want{ctx: got.ContextName, server: got.Server.Server, token: got.Token, refresh: got.RefreshToken, src: got.TokenSource, pf: got.PortForward, core: got.Core}
			if g != tc.want {
				t.Fatalf("got %+v, want %+v", g, tc.want)
			}
		})
	}
}
