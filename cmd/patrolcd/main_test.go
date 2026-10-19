package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden help files")

// binary is the patrolcd binary built by TestMain with `go build`.
var binary string

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if testing.Short() {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "patrolcd-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binary = filepath.Join(dir, "patrolcd")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go build ./cmd/patrolcd: %v\n", err)
		return 1
	}
	return m.Run()
}

func requireBinary(t *testing.T) {
	t.Helper()
	if binary == "" {
		t.Skip("binary not built in -short mode")
	}
}

// fixedEnv is a minimal, deterministic environment: help output must not depend on the
// developer's variables (TMPDIR is a cmp-server default, KUBERNETES_* select log formats).
func fixedEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "TMPDIR=/tmp"}, extra...)
}

type result struct {
	stdout, stderr string
	code           int
}

func runBinary(t *testing.T, path string, env []string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		require.True(t, errors.As(err, &ee), "run %s: %v", path, err)
		code = ee.ExitCode()
	}
	return result{stdout: out.String(), stderr: errOut.String(), code: code}
}

// TestHelpGolden pins `patrolcd --help` and the help of every subcommand (flag names,
// types, defaults, environment variables and patrol-cmd-params-cm keys).
func TestHelpGolden(t *testing.T) {
	requireBinary(t)
	cases := map[string][]string{
		"root":                     {"--help"},
		"server":                   {"server", "--help"},
		"controller":               {"controller", "--help"},
		"repo-server":              {"repo-server", "--help"},
		"appset-controller":        {"appset-controller", "--help"},
		"notifications-controller": {"notifications-controller", "--help"},
		"cmp-server":               {"cmp-server", "--help"},
		"commit-server":            {"commit-server", "--help"},
		"k8s-auth":                 {"k8s-auth", "--help"},
		"bootstrap":                {"bootstrap", "--help"},
		"version":                  {"version", "--help"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := runBinary(t, binary, fixedEnv(), "", args...)
			require.Equal(t, 0, r.code, r.stderr)
			golden := filepath.Join("testdata", "help", name+".golden")
			if *update {
				require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
				require.NoError(t, os.WriteFile(golden, []byte(r.stdout), 0o644))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "run `go test ./cmd/patrolcd -run TestHelpGolden -update` to create the golden files")
			assert.Equal(t, string(want), r.stdout)
		})
	}
}

// TestBootstrapRegistered checks that `patrolcd --help` lists the first-run bootstrap step
// the install manifests run as an init container.
func TestBootstrapRegistered(t *testing.T) {
	requireBinary(t)
	r := runBinary(t, binary, fixedEnv(), "", "--help")
	require.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, r.stdout, "\n  bootstrap ")
	r = runBinary(t, binary, fixedEnv(), "", "bootstrap", "--help")
	require.Equal(t, 0, r.code, r.stderr)
	for _, flag := range []string{"--namespace", "--skip-admin", "--skip-internal-token", "--timeout", "--kubeconfig", "--context"} {
		assert.Contains(t, r.stdout, flag)
	}
}

func TestVersion(t *testing.T) {
	requireBinary(t)
	r := runBinary(t, binary, fixedEnv(), "", "version", "--short")
	require.Equal(t, 0, r.code, r.stderr)
	assert.True(t, strings.HasPrefix(r.stdout, "patrolcd: "), r.stdout)
	r = runBinary(t, binary, fixedEnv(), "", "version", "-o", "json")
	require.Equal(t, 0, r.code, r.stderr)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &doc))
	assert.Equal(t, runtime.GOOS+"/"+runtime.GOARCH, doc["platform"])
	assert.NotEmpty(t, doc["version"])
}

func TestUnknownCommandFails(t *testing.T) {
	requireBinary(t)
	r := runBinary(t, binary, fixedEnv(), "", "no-such-component")
	assert.Equal(t, 1, r.code)
	assert.Contains(t, r.stderr, "unknown command")
}

// TestHelperModesAreDispatched runs the binary in each hidden helper mode: the modes must be
// recognized before the command tree parses arguments (cobra would reject them as unknown
// commands with exit status 1).
func TestHelperModesAreDispatched(t *testing.T) {
	requireBinary(t)
	t.Run("jsonnet evaluator", func(t *testing.T) {
		dir := t.TempDir()
		main := filepath.Join(dir, "main.jsonnet")
		require.NoError(t, os.WriteFile(main, []byte(`{ kind: "ConfigMap", apiVersion: "v1", metadata: { name: "x" } }`), 0o644))
		req, err := json.Marshal(map[string]any{"repoRoot": dir, "files": []string{main}})
		require.NoError(t, err)
		r := runBinary(t, binary, fixedEnv(), string(req), "jsonnet-eval")
		require.Equal(t, 0, r.code, r.stderr)
		var resp struct {
			Outputs []json.RawMessage `json:"outputs"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &resp), r.stdout)
		require.Len(t, resp.Outputs, 1)
		assert.Contains(t, string(resp.Outputs[0]), `"ConfigMap"`)
	})
	t.Run("sandbox rlimit helper", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the sandbox helper is Unix only")
		}
		r := runBinary(t, binary, fixedEnv(), "", "__patrol-sandbox-exec")
		assert.Equal(t, 126, r.code, r.stderr)
		assert.True(t, strings.HasPrefix(r.stderr, "patrol-sandbox: "), r.stderr)
	})
	t.Run("git askpass", func(t *testing.T) {
		creds := filepath.Join(t.TempDir(), "creds.json")
		require.NoError(t, os.WriteFile(creds, []byte(`{"username":"robot","password":"not-a-real-password"}`), 0o600))
		env := fixedEnv("PATROL_GIT_ASKPASS_FILE="+creds, "PATROL_GIT_ASKPASS_ORIGIN=https://git.example.com")
		r := runBinary(t, binary, env, "", "Username for 'https://git.example.com': ")
		require.Equal(t, 0, r.code, r.stderr)
		assert.Equal(t, "robot\n", r.stdout)
		r = runBinary(t, binary, env, "", "Password for 'https://other.example.com': ")
		assert.Equal(t, 1, r.code)
		assert.Empty(t, r.stdout, "credentials are never offered to another origin")
	})
}

// TestBinaryNameDispatch runs the binary through component names (symbolic links in the
// image) and PATROL_BINARY_NAME.
func TestBinaryNameDispatch(t *testing.T) {
	requireBinary(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "patrol-repo-server")
	require.NoError(t, os.Symlink(binary, link))
	r := runBinary(t, link, fixedEnv(), "", "--help")
	require.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, r.stdout, "patrolcd repo-server [flags]")

	r = runBinary(t, binary, fixedEnv("PATROL_BINARY_NAME=patrol-commit-server"), "", "--help")
	require.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, r.stdout, "patrolcd commit-server [flags]")
}
