package cmp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// upstreamExample is the plugin.yaml of the upstream operator manual.
const upstreamExample = `
apiVersion: argoproj.io/v1alpha1
kind: ConfigManagementPlugin
metadata:
  name: my-plugin
spec:
  version: v1.0
  init:
    command: [sh]
    args: [-c, 'echo "Initializing..."']
  generate:
    command: [sh, -c]
    args:
      - |
        echo "{\"kind\": \"ConfigMap\", \"apiVersion\": \"v1\", \"metadata\": { \"name\": \"$ARGOCD_APP_NAME\"}}"
  discover:
    fileName: "./subdir/s*.yaml"
    find:
      glob: "**/Chart.yaml"
      command: [sh, -c, find . -name env.yaml]
  parameters:
    static:
      - name: string-param
        title: Description of the string param
        tooltip: Tooltip shown when the user hovers the
        required: true
        itemType: "number"
        collectionType: "string"
        string: default-string-value
      - name: array-param
        array: [default, items]
        collectionType: array
      - name: map-param
        map:
          some: value
        collectionType: map
    dynamic:
      command: [echo, '[{"name": "example-param", "string": "default-string-value"}]']
  preserveFileMode: false
  provideGitCreds: false
`

func TestParsePluginConfigUpstreamExample(t *testing.T) {
	t.Parallel()
	cfg := mustParse(t, upstreamExample)
	if w := cfg.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %q", w)
	}
	if got := cfg.PluginName(); got != "my-plugin-v1.0" {
		t.Fatalf("PluginName = %q", got)
	}
	if got := cfg.SocketName(); got != "my-plugin-v1.0.sock" {
		t.Fatalf("SocketName = %q", got)
	}
	if !cfg.IsDiscoveryConfigured() {
		t.Fatal("discovery not configured")
	}
	spec := cfg.Spec
	if !reflect.DeepEqual(spec.Init, Command{Command: []string{"sh"}, Args: []string{"-c", `echo "Initializing..."`}}) {
		t.Fatalf("init = %#v", spec.Init)
	}
	if got := spec.Generate.argv(); len(got) != 3 || got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("generate argv = %q", got)
	}
	if spec.Discover.FileName != "./subdir/s*.yaml" || spec.Discover.Find.Glob != "**/Chart.yaml" ||
		!reflect.DeepEqual(spec.Discover.Find.Command.Command, []string{"sh", "-c", "find . -name env.yaml"}) {
		t.Fatalf("discover = %#v", spec.Discover)
	}
	wantStatic := []apiclient.ParameterAnnouncement{
		{Name: "string-param", Title: "Description of the string param", Tooltip: "Tooltip shown when the user hovers the", Required: true, ItemType: "number", CollectionType: "string", String: "default-string-value"},
		{Name: "array-param", Array: []string{"default", "items"}, CollectionType: "array"},
		{Name: "map-param", Map: map[string]string{"some": "value"}, CollectionType: "map"},
	}
	if !reflect.DeepEqual(spec.Parameters.Static, wantStatic) {
		t.Fatalf("static = %#v", spec.Parameters.Static)
	}
	if !reflect.DeepEqual(spec.Parameters.Dynamic.Command, []string{"echo", `[{"name": "example-param", "string": "default-string-value"}]`}) {
		t.Fatalf("dynamic = %#v", spec.Parameters.Dynamic)
	}
}

func TestParsePluginConfigValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, yaml, want string
	}{
		{"missing name", "kind: ConfigManagementPlugin\nspec: {generate: {command: [sh]}}",
			"invalid plugin configuration file. metadata.name should be non-empty"},
		{"wrong kind", "kind: Plugin\nmetadata: {name: p}\nspec: {generate: {command: [sh]}}",
			"invalid plugin configuration file. kind should be ConfigManagementPlugin, found Plugin"},
		{"missing kind", "metadata: {name: p}\nspec: {generate: {command: [sh]}}",
			"invalid plugin configuration file. kind should be ConfigManagementPlugin, found "},
		{"empty generate", "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec: {generate: {args: [x]}}",
			"invalid plugin configuration file. spec.generate command should be non-empty"},
		{"name with slash", "kind: ConfigManagementPlugin\nmetadata: {name: ../p}\nspec: {generate: {command: [sh]}}",
			`metadata.name "../p" must be a valid file name component`},
		{"dot-dot version", "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec: {version: '..', generate: {command: [sh]}}",
			`spec.version ".." must be a valid file name component`},
		{"empty executable", "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec: {generate: {command: [sh]}, init: {command: ['']}}",
			"spec.init command[0] should be non-empty"},
		{"not yaml", "kind: [", "error unmarshalling plugin configuration file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParsePluginConfig([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
			assertContains(t, err.Error(), tt.want)
		})
	}
}

func TestParsePluginConfigMinimalAndWarnings(t *testing.T) {
	t.Parallel()
	minimal := mustParse(t, "apiVersion: cd.patrol.example/v1alpha1\nkind: ConfigManagementPlugin\nmetadata: {name: p}\nspec: {generate: {command: [cat]}}")
	if w := minimal.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %q", w)
	}
	if minimal.SocketName() != "p.sock" || minimal.IsDiscoveryConfigured() {
		t.Fatalf("socket %q discovery %v", minimal.SocketName(), minimal.IsDiscoveryConfigured())
	}

	cfg := mustParse(t, `
apiVersion: example.com/v9
kind: ConfigManagementPlugin
metadata: {name: p, namespace: ignored}
spec:
  generate: {command: [cat]}
  lockRepo: true
  provideGitCreds: true
  unknownField: 1
`)
	w := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{`unexpected apiVersion "example.com/v9"`, "spec.lockRepo is ignored", "never forwards repository credentials", "fields Patrol ignores", "unknownField"} {
		assertContains(t, w, want)
	}
	if !cfg.Spec.ProvideGitCreds || !cfg.Spec.LockRepo {
		t.Fatalf("spec = %#v", cfg.Spec)
	}
}

func TestReadPluginConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := ReadPluginConfig(dir); err == nil {
		t.Fatal("expected error for a missing file")
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(upstreamExample), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadPluginConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metadata.Name != "my-plugin" {
		t.Fatalf("name = %q", cfg.Metadata.Name)
	}

	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, ConfigFileName), []byte(strings.Repeat("#", MaxConfigBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ReadPluginConfig(big)
	if err == nil {
		t.Fatal("expected size error")
	}
	assertContains(t, err.Error(), "exceeds")
}
