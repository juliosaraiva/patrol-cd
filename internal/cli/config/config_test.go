package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sample() *LocalConfig {
	return &LocalConfig{
		CurrentContext: "prod",
		Contexts: []ContextRef{
			{Name: "prod", Server: "cd.example.com", User: "prod"},
			{Name: "dev", Server: "dev.example.com:8080", User: "dev"},
		},
		Servers: []Server{{Server: "cd.example.com", CACertificateAuthorityData: "Y2E="}, {Server: "dev.example.com:8080", PlainText: true}},
		Users:   []User{{Name: "prod", AuthToken: "tok-prod", RefreshToken: "ref-prod"}, {Name: "dev", AuthToken: "tok-dev"}},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "patrol", "config")
	if err := Write(sample(), path); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "prod" || len(got.Contexts) != 2 || got.Users[0].RefreshToken != "ref-prod" || !got.Servers[1].PlainText {
		t.Fatalf("round trip: %+v", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", di.Mode().Perm())
		}
	}
	// Atomic replacement leaves no temporary files.
	if err := Write(sample(), path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("stray files: %v", entries)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"current-context: prod", "auth-token: tok-prod", "refresh-token: ref-prod", "plain-text: true", "certificate-authority-data: Y2E="} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("file lacks %q:\n%s", k, raw)
		}
	}
}

func TestReadErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if c, err := Read(filepath.Join(dir, "missing")); c != nil || err != nil {
		t.Fatalf("missing file: %v %v", c, err)
	}
	write := func(name, data string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, data string
		mode       os.FileMode
		want       string
	}{
		{"garbage", "contexts: [", 0o600, "failed to parse config file"},
		{"badctx", "current-context: nope\n", 0o600, "Context 'nope' undefined"},
		{"badserver", "current-context: a\ncontexts:\n- name: a\n  server: s\n  user: a\nusers:\n- name: a\n", 0o600, "Server 's' undefined"},
		{"big", strings.Repeat("#", MaxFileBytes+1), 0o600, "exceeds"},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name, data string
			mode       os.FileMode
			want       string
		}{"loose", "current-context: \"\"\n", 0o644, "config file has incorrect permission flags -rw-r--r--, change the file permission either to 0400 or 0600"})
	}
	for _, tc := range cases {
		_, err := Read(write(tc.name, tc.data, tc.mode))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if tc.name == "loose" && !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("loose: not ErrInsecurePermissions: %v", err)
		}
	}
	// Read-only files are fine.
	if runtime.GOOS != "windows" {
		if _, err := Read(write("ro", "", 0o400)); err != nil {
			t.Fatal(err)
		}
	}
	// Empty files are an empty configuration.
	c, err := Read(write("empty", "", 0o600))
	if err != nil || c == nil || !c.IsEmpty() {
		t.Fatalf("empty: %v %v", c, err)
	}
}

func TestContextOperations(t *testing.T) {
	t.Parallel()
	c := sample()
	ctx, err := c.ResolveContext("")
	if err != nil || ctx.Name != "prod" || ctx.Server.Server != "cd.example.com" || ctx.User.AuthToken != "tok-prod" {
		t.Fatalf("resolve current: %+v %v", ctx, err)
	}
	if _, err := c.ResolveContext("nope"); err == nil || err.Error() != "Context 'nope' undefined" {
		t.Fatalf("resolve missing: %v", err)
	}
	if _, err := (&LocalConfig{}).ResolveContext(""); err == nil {
		t.Fatal("empty current context resolved")
	}
	c.UpsertServer(Server{Server: "cd.example.com", Insecure: true})
	if s, _ := c.GetServer("cd.example.com"); !s.Insecure || len(c.Servers) != 2 {
		t.Fatalf("upsert server: %+v", c.Servers)
	}
	c.UpsertUser(User{Name: "prod", AuthToken: "new"})
	if c.GetToken("prod") != "new" || len(c.Users) != 2 {
		t.Fatalf("upsert user: %+v", c.Users)
	}
	c.UpsertContext(ContextRef{Name: "dev", Server: "cd.example.com", User: "dev"})
	if !c.ServerInUse("cd.example.com", "prod") || c.ServerInUse("dev.example.com:8080", "") {
		t.Fatal("ServerInUse")
	}
	if !c.RemoveToken("prod") || c.GetToken("prod") != "" || c.RemoveToken("nope") {
		t.Fatal("RemoveToken")
	}
	if srv, ok := c.RemoveContext("dev"); !ok || srv != "cd.example.com" {
		t.Fatalf("RemoveContext: %q %v", srv, ok)
	}
	if _, ok := c.RemoveContext("dev"); ok {
		t.Fatal("removed twice")
	}
	if !c.RemoveServer("dev.example.com:8080") || c.RemoveServer("dev.example.com:8080") {
		t.Fatal("RemoveServer")
	}
	if !c.RemoveUser("dev") || c.RemoveUser("dev") {
		t.Fatal("RemoveUser")
	}
}

func TestUserRedaction(t *testing.T) {
	t.Parallel()
	u := User{Name: "prod", AuthToken: "secret-token", RefreshToken: "secret-refresh"}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "user", u)
	for _, s := range []string{fmt.Sprint(u), fmt.Sprintf("%v %+v %#v %s", u, u, u, u), buf.String()} {
		if strings.Contains(s, "secret") {
			t.Fatalf("token leaked: %s", s)
		}
	}
	if !strings.Contains(fmt.Sprint(u), "[redacted]") || !strings.Contains(buf.String(), `"hasAuthToken":true`) {
		t.Fatalf("unexpected rendering: %s / %s", fmt.Sprint(u), buf.String())
	}
}

func TestDefaultPath(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) LookupEnv {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HOME": "/home/u"}, "/home/u/.config/patrol/config"},
		{map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/xdg"}, "/xdg/patrol/config"},
		{map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/xdg", EnvConfigDir: "/cfgdir"}, "/cfgdir/config"},
		{map[string]string{"HOME": "/home/u", EnvConfigDir: "/cfgdir", EnvConfig: "/explicit/file"}, "/explicit/file"},
		{map[string]string{"HOME": "/home/u", EnvConfig: "  "}, "/home/u/.config/patrol/config"},
	}
	for _, tc := range cases {
		got, err := DefaultPath(env(tc.env))
		if err != nil || filepath.ToSlash(got) != tc.want {
			t.Fatalf("%v: %q %v, want %q", tc.env, got, err, tc.want)
		}
	}
}
