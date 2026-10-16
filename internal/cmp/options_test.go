package cmp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestOptionsFromEnv(t *testing.T) {
	t.Parallel()
	def, err := OptionsFromEnv(lookupMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def, DefaultOptions()) {
		t.Fatalf("defaults = %+v", def)
	}
	if def.SocketDir != cmpwire.DefaultSocketDir || def.ConfigDir != "/home/patrol/cmp-server/config" || def.ExecTimeout != 90*time.Second ||
		def.ReceiveTimeout != 5*time.Minute {
		t.Fatalf("defaults = %+v", def)
	}

	o, err := OptionsFromEnv(lookupMap(map[string]string{
		EnvSocketDirArgo:    "/argo/plugins",
		EnvWorkDirArgo:      "/argo/work",
		EnvWorkDir:          "/patrol/work",
		EnvExecTimeoutArgo:  "2m",
		EnvPassEnv:          "AVP_*, HELM_CACHE_HOME;FOO",
		EnvReceiveTimeout:   "0",
		EnvMaxArchiveSize:   "512Mi",
		EnvMaxExtractedSize: "0",
		EnvMaxFiles:         "1000",
		EnvLogLevelArgo:     "debug",
		EnvLogFormat:        "text",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Options{
		ConfigDir: DefaultConfigDir, SocketDir: "/argo/plugins", WorkDir: "/patrol/work", ExecTimeout: 2 * time.Minute,
		PassEnv: []string{"AVP_*", "HELM_CACHE_HOME", "FOO"}, ReceiveTimeout: 0, MaxArchiveBytes: 512 << 20, MaxExtractedBytes: -1, MaxFiles: 1000,
		LogLevel: "debug", LogFormat: "text",
	}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("options = %+v\nwant %+v", o, want)
	}

	for name, env := range map[string]map[string]string{
		"bad duration":   {EnvExecTimeout: "90"},
		"negative":       {EnvExecTimeout: "-1s"},
		"bad receive":    {EnvReceiveTimeout: "soon"},
		"neg receive":    {EnvReceiveTimeout: "-1m"},
		"bad size":       {EnvMaxArchiveSize: "lots"},
		"bad files":      {EnvMaxFiles: "x"},
		"bad level":      {EnvLogLevel: "loud"},
		"bad format":     {EnvLogFormat: "xml"},
		"bad pass env":   {EnvPassEnv: "A=B"},
		"relative work":  {EnvWorkDir: "work"},
		"patrol timeout": {EnvExecTimeout: "soon", EnvExecTimeoutArgo: "1s"},
	} {
		if _, err := OptionsFromEnv(lookupMap(env)); err == nil {
			t.Errorf("%s: accepted %v", name, env)
		}
	}
}

func TestOptionsFlagsAndLogger(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	fs := pflag.NewFlagSet("cmp-server", pflag.ContinueOnError)
	o.AddFlags(fs)
	if err := fs.Parse([]string{"--config-dir-path=/cfg", "--exec-timeout=5s", "--receive-timeout=30s", "--pass-env=A,B*", "--loglevel=warn", "--logformat=text"}); err != nil {
		t.Fatal(err)
	}
	if o.ConfigDir != "/cfg" || o.ExecTimeout != 5*time.Second || !reflect.DeepEqual(o.PassEnv, []string{"A", "B*"}) {
		t.Fatalf("options = %+v", o)
	}
	var buf bytes.Buffer
	l, err := o.NewLogger(&buf)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "level=WARN msg=shown") {
		t.Fatalf("log = %q", buf.String())
	}
	cfg := o.Config(mustParse(t, discoveryPlugin("    fileName: x\n")), l, nil)
	if cfg.Exec.Timeout != 5*time.Second || cfg.ReceiveTimeout != 30*time.Second || cfg.SocketDir != o.SocketDir || cfg.Archive.MaxFiles != o.MaxFiles {
		t.Fatalf("config = %+v", cfg)
	}
	// A disabled receive timeout must not turn into Config's default.
	o.ReceiveTimeout = 0
	if cfg := o.Config(mustParse(t, discoveryPlugin("    fileName: x\n")), l, nil); cfg.ReceiveTimeout >= 0 {
		t.Fatalf("disabled receive timeout = %s", cfg.ReceiveTimeout)
	}
}

func writePluginYAML(t testing.TB, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunEndToEnd starts the cmp-server entry point from plugin.yaml, uses it through
// the repo-server client and stops it (the socket is removed).
func TestRunEndToEnd(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.ConfigDir = writePluginYAML(t, `
apiVersion: argoproj.io/v1alpha1
kind: ConfigManagementPlugin
metadata:
  name: my-plugin
spec:
  version: v1.0
  generate:
    command: [sh, -c]
    args: ['echo "{\"kind\": \"ConfigMap\", \"apiVersion\": \"v1\", \"metadata\": { \"name\": \"$ARGOCD_APP_NAME\", \"annotations\": {\"Foo\": \"$ARGOCD_ENV_FOO\"}}}"']
`)
	o.SocketDir = socketDir(t)
	o.WorkDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var logs bytes.Buffer // read only after Run returned
	go func() { done <- Run(ctx, o, slog.New(slog.NewTextHandler(&logs, nil)), nil) }()
	sock := filepath.Join(o.SocketDir, "my-plugin-v1.0.sock")
	waitForSocket(t, sock, done)

	r := newRenderer(t, o.SocketDir)
	plugins, err := r.ListPlugins()
	if err != nil || !reflect.DeepEqual(plugins, []string{"my-plugin-v1.0"}) {
		t.Fatalf("ListPlugins = %q, %v", plugins, err)
	}
	res, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/a": "a"},
		&v1alpha1.ApplicationSourcePlugin{Name: "my-plugin-v1.0", Env: v1alpha1.Env{{Name: "FOO", Value: "bar"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifests[0].GetName() != "guestbook" || res.Manifests[0].GetAnnotations()["Foo"] != "bar" {
		t.Fatalf("manifest = %v", res.Manifests[0].Object)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
	// Operators migrating plugins see that the sidecar environment is not passed.
	assertContains(t, logs.String(), "not the sidecar environment (upstream passes all of it). Set PATROL_CMP_PASS_ENV")
}

// TestRunRejectsInvalidConfig checks that each upstream validation error aborts
// startup before a socket is created.
func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"invalid plugin configuration file. metadata.name should be non-empty":                  "kind: ConfigManagementPlugin\nspec: {generate: {command: [sh]}}",
		"invalid plugin configuration file. kind should be ConfigManagementPlugin, found Other": "kind: Other\nmetadata: {name: p}\nspec: {generate: {command: [sh]}}",
		"invalid plugin configuration file. spec.generate command should be non-empty":          "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec: {}",
		"error reading plugin configuration file":                                               "",
	}
	for want, content := range tests {
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			o := DefaultOptions()
			o.ConfigDir = t.TempDir()
			if content != "" {
				o.ConfigDir = writePluginYAML(t, content)
			}
			o.SocketDir = socketDir(t)
			o.WorkDir = t.TempDir()
			err := Run(context.Background(), o, discardLogger(), nil)
			if err == nil {
				t.Fatal("Run succeeded")
			}
			assertContains(t, err.Error(), want)
			entries, _ := os.ReadDir(o.SocketDir)
			if len(entries) != 0 {
				t.Fatalf("socket dir not empty: %v", entries)
			}
		})
	}
}
