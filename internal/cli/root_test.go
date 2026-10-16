package cli_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// helpEnv makes the --config default deterministic.
var helpEnv = clitest.RunOptions{Env: map[string]string{"HOME": "/home/patrol"}, Unset: []string{"PATROL_CONFIG"}}

func TestHelpGolden(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"root":                {"--help"},
		"login":               {"login", "--help"},
		"logout":              {"logout", "--help"},
		"context":             {"context", "--help"},
		"version":             {"version", "--help"},
		"account":             {"account", "--help"},
		"account-can-i":       {"account", "can-i", "--help"},
		"cert":                {"cert", "--help"},
		"gpg":                 {"gpg", "--help"},
		"repo":                {"repo", "--help"},
		"repo-add":            {"repo", "add", "--help"},
		"repocreds-add":       {"repocreds", "add", "--help"},
		"cluster":             {"cluster", "--help"},
		"cluster-add":         {"cluster", "add", "--help"},
		"proj":                {"proj", "--help"},
		"proj-create":         {"proj", "create", "--help"},
		"proj-role":           {"proj", "role", "--help"},
		"proj-windows":        {"proj", "windows", "--help"},
		"proj-windows-add":    {"proj", "windows", "add", "--help"},
		"proj-source-integ":   {"proj", "source-integrity", "git", "policies", "add", "--help"},
		"completion":          {"completion", "--help"},
		"configure":           {"configure", "--help"},
		"account-gen-token":   {"account", "generate-token", "--help"},
		"proj-role-new-token": {"proj", "role", "create-token", "--help"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := clitest.NewRunner(t).RunWith(helpEnv, args...).Succeeded(t)
			golden := filepath.Join("testdata", "help", name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(r.Stdout), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if string(want) != r.Stdout {
				t.Fatalf("help of %v differs from %s (run with -update):\n%s", args, golden, r.Stdout)
			}
		})
	}
}

func TestInvalidInvocations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		env  map[string]string
		code int
		want []string
	}{
		{"unknown command", []string{"nosuch"}, nil, 1, []string{`unknown command "nosuch" for "patrol"`, "Run 'patrol --help' for usage."}},
		{"unknown flag", []string{"proj", "list", "--nosuch"}, nil, 1, []string{"unknown flag: --nosuch", "Run 'patrol proj list --help' for usage."}},
		{"bad flag value", []string{"--http-retry-max", "x", "proj", "list"}, nil, 1, []string{"invalid argument"}},
		{"missing args prints help", []string{"proj", "get"}, nil, 1, nil},
		{"missing server", []string{"proj", "list"}, nil, 20, []string{"Patrol CD server address unspecified"}},
		{"core mode", []string{"--core", "proj", "list"}, nil, 20, []string{"core mode not available in this build"}},
		{"client cert without key", []string{"--server", "127.0.0.1:1", "--client-crt", "x.crt", "proj", "list"}, nil, 20, []string{"--client-crt and --client-crt-key must always be specified together"}},
		{"bad header", []string{"--server", "127.0.0.1:1", "-H", "nocolon", "proj", "list"}, nil, 20, []string{"additional headers must be colon(:)-separated: nocolon"}},
		{"bad log level", []string{"--loglevel", "loud", "version", "--client"}, nil, 1, []string{"loud"}},
		{"bad log format", []string{"--logformat", "xml", "version", "--client"}, nil, 1, []string{"unknown log format"}},
		{"PATROL_OPTS unbalanced quote", []string{"version", "--client"}, map[string]string{"PATROL_OPTS": `--server "x`}, 1, []string{"PATROL_OPTS"}},
		{"PATROL_OPTS not a flag", []string{"version", "--client"}, map[string]string{"PATROL_OPTS": `server x`}, 1, []string{"PATROL_OPTS invalid at 'server'"}},
		{"undefined context", []string{"--patrol-context", "nope", "proj", "list"}, nil, 20, []string{"Context 'nope' undefined"}},
		{"bad version output", []string{"version", "--client", "-o", "toml"}, nil, 1, []string{"toml"}},
		{"completion shell", []string{"completion", "tcsh"}, nil, 1, []string{"Invalid shell 'tcsh'. The supported shells are bash, zsh, fish and powershell."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := clitest.NewRunner(t)
			if tc.name == "undefined context" {
				// A configuration must exist for the context lookup.
				if err := config.Write(&config.LocalConfig{CurrentContext: "a", Contexts: []config.ContextRef{{Name: "a", Server: "a:1", User: "a:1"}}, Servers: []config.Server{{Server: "a:1"}}, Users: []config.User{{Name: "a:1"}}}, r.ConfigPath()); err != nil {
					t.Fatal(err)
				}
			}
			res := r.RunWith(clitest.RunOptions{Env: tc.env}, tc.args...)
			res.Failed(t, tc.code, tc.want...)
			if tc.name == "missing args prints help" && !strings.Contains(res.Stdout+res.Stderr, "Usage:") {
				t.Fatalf("help not printed: %+v", res)
			}
		})
	}
}

func TestVersionClient(t *testing.T) {
	t.Parallel()
	r := clitest.NewRunner(t)
	out := r.Run("version", "--client").Succeeded(t)
	if !strings.HasPrefix(out.Stdout, "patrol: ") || !strings.Contains(out.Stdout, "GoVersion: "+runtime.Version()) {
		t.Fatalf("unexpected output:\n%s", out.Stdout)
	}
	short := r.Run("version", "--client", "--short").Succeeded(t)
	if lines := strings.Split(strings.TrimSpace(short.Stdout), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "patrol: ") {
		t.Fatalf("short: %q", short.Stdout)
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(r.Run("version", "--client", "-o", "json").Succeeded(t).Stdout), &j); err != nil {
		t.Fatal(err)
	}
	if _, ok := j["client"]; !ok {
		t.Fatalf("json: %v", j)
	}
	var y map[string]any
	if err := yaml.Unmarshal([]byte(r.Run("version", "--client", "-o", "yaml").Succeeded(t).Stdout), &y); err != nil {
		t.Fatal(err)
	}
	if _, ok := y["client"]; !ok {
		t.Fatalf("yaml: %v", y)
	}
}

func TestCompletionScripts(t *testing.T) {
	t.Parallel()
	for shell, marker := range map[string]string{
		"bash":       "__start_patrol",
		"zsh":        "#compdef patrol",
		"fish":       "complete -c patrol",
		"powershell": "Register-ArgumentCompleter",
	} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			clitest.NewRunner(t).Run("completion", shell).Succeeded(t).Contains(t, marker)
		})
	}
}

func writePlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "patrol-"+name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
}

func TestPluginDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugins")
	}
	t.Parallel()
	r := clitest.NewRunner(t)
	bin := filepath.Join(r.Dir, "bin")
	writePlugin(t, bin, "hello", `echo "name=$PATROL_PLUGIN_NAME args=$* config=$PATROL_CONFIG"; echo oops >&2; exit 3`)
	r.Env["PATH"] = bin
	res := r.Run("hello", "a", "--flag", "b")
	if res.Code != 3 {
		t.Fatalf("exit %d, want 3: %+v", res.Code, res)
	}
	want := "name=hello args=a --flag b config=" + r.ConfigPath()
	if strings.TrimSpace(res.Stdout) != want || strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("stdout %q stderr %q, want %q", res.Stdout, res.Stderr, want)
	}
	// Built-in commands win over plugins.
	writePlugin(t, bin, "version", "echo plugin; exit 0")
	r.Run("version", "--client").Succeeded(t).NotContains(t, "plugin")
	// Unknown commands without a plugin are usage errors.
	r.Run("nosuch").Failed(t, 1, "unknown command")
	// Plugins appear in completions.
	r.Run("__complete", "hel").Succeeded(t).Contains(t, "hello")
}

func TestPluginRelativePathRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell plugins")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	writePlugin(t, filepath.Join(dir, "rel"), "x", "echo ran")
	r := clitest.NewRunner(t)
	r.Env["PATH"] = "rel"
	r.Run("x").Failed(t, 1, "plugin 'patrol-x' found in a relative PATH entry; refusing to run it").NotContains(t, "ran")
}
