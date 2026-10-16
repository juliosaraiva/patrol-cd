package cmp

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// envDumpPlugin renders one ConfigMap whose data holds selected variables, the names
// of all variables, and the content of init.txt (written by init).
const envDumpPlugin = `
apiVersion: argoproj.io/v1alpha1
kind: ConfigManagementPlugin
metadata:
  name: envdump
spec:
  version: v1.0
  init:
    command: [sh, -c]
    args: ['echo "init saw $ARGOCD_APP_NAME" > init.txt; echo "init log" >&2']
  generate:
    command: [sh, -c]
    args:
      - |
        names=$(env | cut -d= -f1 | sort | tr '\n' ' ')
        cat <<EOF
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: "$ARGOCD_APP_NAME"
          namespace: "$ARGOCD_APP_NAMESPACE"
        data:
          names: "$names"
          init: "$(cat init.txt)"
          foo: "$ARGOCD_ENV_FOO"
          rev: "$ARGOCD_ENV_REV"
          kubeVersion: "$KUBE_VERSION"
          kubeAPIVersions: "$KUBE_API_VERSIONS"
          paramTag: "$PARAM_HELM_PARAMETERS_IMAGE_TAG"
          paramFile: "$PARAM_VALUES_FILES_0"
          envPath: "$ARGOCD_ENV_PATH"
          path: "$PATH"
          pwd: "$(basename "$(pwd)")"
          files: "$(find . -type f | sort | tr '\n' ' ')"
        ---
        EOF
`

func configMapData(t testing.TB, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	data, _, err := unstructured.NestedStringMap(objs[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGenerateEndToEnd drives the server with the repo-server's CMP client (internal/render)
// over a real unix socket: plugin selection by name, init then generate in the app
// directory, the request environment and YAML output parsing.
func TestGenerateEndToEnd(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", envDumpPlugin)
	r := newRenderer(t, srv.dir)
	plugin := &v1alpha1.ApplicationSourcePlugin{
		Name: "envdump-v1.0",
		Env: v1alpha1.Env{
			{Name: "FOO", Value: "bar"},
			{Name: "REV", Value: "test-$ARGOCD_APP_REVISION_SHORT"},
			{Name: "PATH", Value: "/evil"},
		},
		Parameters: v1alpha1.ApplicationSourcePluginParameters{
			{Name: "values-files", OptionalArray: &v1alpha1.OptionalArray{Array: []string{"values-dev.yaml"}}},
			{Name: "helm-parameters", OptionalMap: &v1alpha1.OptionalMap{Map: map[string]string{"image.tag": "v1.2.3"}}},
		},
	}
	files := map[string]string{"app/main.cue": "x: 1", "app/sub/values.yaml": "a: b", "other/readme.md": "x"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Generate(ctx, renderInput(t, files, plugin))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.SourceType != "Plugin" {
		t.Fatalf("source type = %q", res.SourceType)
	}
	if res.Manifests[0].GetName() != "guestbook" || res.Manifests[0].GetNamespace() != "team-ns" {
		t.Fatalf("object = %s/%s", res.Manifests[0].GetNamespace(), res.Manifests[0].GetName())
	}
	data := configMapData(t, res.Manifests)
	want := map[string]string{
		"init":            "init saw guestbook",
		"foo":             "bar",
		"rev":             "test-0123456",
		"kubeVersion":     "1.30.4",
		"kubeAPIVersions": "v1,apps/v1",
		"paramTag":        "v1.2.3",
		"paramFile":       "values-dev.yaml",
		"envPath":         "/evil",
		"path":            os.Getenv("PATH"),
		"pwd":             "app",
		"files":           "./init.txt ./main.cue ./sub/values.yaml ",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%s] = %q, want %q", k, data[k], v)
		}
	}
	waitNoEntries(t, srv.Server)
}

// TestEnvironmentIsolation checks that plugins see only PATH, HOME, TMPDIR, LANG, the
// pass-through variables and the request environment: nothing else of the server
// process (tokens, Kubernetes service variables, server settings).
func TestEnvironmentIsolation(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", envDumpPlugin, func(c *Config) {
		c.Environ = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=/home/patrol",
			"PATROL_INTERNAL_TOKEN=s3cr3t", "KUBERNETES_SERVICE_HOST=10.0.0.1", "AWS_SECRET_ACCESS_KEY=x",
			"PATROL_EXEC_TIMEOUT=90s", "AVP_TYPE=vault", "AVP_AUTH_TYPE=k8s",
		}
		c.PassEnv = []string{"AVP_*"}
	})
	r := newRenderer(t, srv.dir)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Generate(ctx, renderInput(t, map[string]string{"app/a.txt": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "envdump-v1.0"}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	names := strings.Fields(configMapData(t, res.Manifests)["names"])
	allowed := func(n string) bool {
		switch n {
		case "PATH", "HOME", "TMPDIR", "LANG", "AVP_TYPE", "AVP_AUTH_TYPE", "KUBE_VERSION", "KUBE_API_VERSIONS",
			// set by sh itself
			"PWD", "SHLVL", "OLDPWD", "_":
			return true
		}
		return strings.HasPrefix(n, "ARGOCD_APP_") || strings.HasPrefix(n, "PATROL_APP_")
	}
	for _, n := range names {
		if !allowed(n) {
			t.Errorf("unexpected variable %s in plugin environment", n)
		}
	}
	for _, n := range []string{"AVP_TYPE", "AVP_AUTH_TYPE", "ARGOCD_APP_NAME", "HOME"} {
		if !slices.Contains(names, n) {
			t.Errorf("missing %s in %q", n, names)
		}
	}
}

// TestEnvironmentIsolationProcessEnv uses the real process environment (Environ nil).
func TestEnvironmentIsolationProcessEnv(t *testing.T) {
	t.Setenv("PATROL_CMP_TEST_LEAK", "leaked-value")
	srv := startServer(t, "", `
kind: ConfigManagementPlugin
metadata: {name: leak}
spec:
  generate:
    command: [sh, -c, 'printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"x\"},\"data\":{\"leak\":\"%s\"}}" "$PATROL_CMP_TEST_LEAK"']
`, func(c *Config) { c.Environ = nil })
	r := newRenderer(t, srv.dir)
	res, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/a": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "leak"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := configMapData(t, res.Manifests)["leak"]; got != "" {
		t.Fatalf("server environment leaked into plugin: %q", got)
	}
}

// TestEnvironmentPassAll checks the upstream-compatible setting PATROL_CMP_PASS_ENV=*:
// commands see the whole sidecar environment (e.g. argocd-vault-plugin's VAULT_ADDR
// and AVP_* variables, proxy settings), followed by the request environment, which
// wins on duplicate names.
func TestEnvironmentPassAll(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", `
kind: ConfigManagementPlugin
metadata: {name: passall}
spec:
  generate:
    command: [sh, -c]
    args:
      - |
        cat <<EOF
        {"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"},"data":{
          "vault":"$VAULT_ADDR","avp":"$AVP_TYPE","proxy":"$HTTPS_PROXY",
          "app":"$ARGOCD_APP_NAME","kube":"$KUBE_VERSION","path":"$PATH"}}
        EOF
`, func(c *Config) {
		c.Environ = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=/home/patrol",
			"VAULT_ADDR=https://vault.example:8200", "AVP_TYPE=vault", "HTTPS_PROXY=http://proxy.example:3128",
			"ARGOCD_APP_NAME=from-sidecar", "KUBE_VERSION=from-sidecar",
		}
		c.PassEnv = []string{"*"}
	})
	r := newRenderer(t, srv.dir)
	res, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/a": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "passall"}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := configMapData(t, res.Manifests)
	want := map[string]string{
		"vault": "https://vault.example:8200", "avp": "vault", "proxy": "http://proxy.example:3128",
		"app": "guestbook", "kube": "1.30.4", "path": os.Getenv("PATH"),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func matchRequest(t *testing.T, srv *testServer, files map[string]string, appRelPath string) (cmpwire.MatchResponse, int, cmpwire.Error) {
	t.Helper()
	status, data := srv.post(t, cmpwire.PathMatch, streamBody(t, tarGzTree(t, files), appRelPath), nil)
	if status != http.StatusOK {
		return cmpwire.MatchResponse{}, status, decodeError(t, data)
	}
	var resp cmpwire.MatchResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp, status, cmpwire.Error{}
}

func discoveryPlugin(discover string) string {
	return "kind: ConfigManagementPlugin\nmetadata: {name: disc}\nspec:\n  generate: {command: [cat]}\n  discover:\n" + discover
}

func TestDiscovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		discover  string
		files     map[string]string
		supported bool
		errSubstr string
	}{
		{"fileName matches", "    fileName: '*.cue'\n", map[string]string{"app/main.cue": "x"}, true, ""},
		{"fileName no match", "    fileName: '*.cue'\n", map[string]string{"app/main.yaml": "x", "other/x.cue": "x"}, false, ""},
		{"fileName subdir", "    fileName: './subdir/s*.yaml'\n", map[string]string{"app/subdir/svc.yaml": "x"}, true, ""},
		{"fileName has no doublestar", "    fileName: '**/x.yaml'\n", map[string]string{"app/a/b/x.yaml": "x"}, false, ""},
		{"fileName bad pattern", "    fileName: '['\n", map[string]string{"app/a": "x"}, false, `match repository error: error finding filename match for pattern "["`},
		{"glob nested", "    find: {glob: '**/Chart.yaml'}\n", map[string]string{"app/charts/sub/Chart.yaml": "x"}, true, ""},
		{"glob at root", "    find: {glob: '**/Chart.yaml'}\n", map[string]string{"app/Chart.yaml": "x"}, true, ""},
		{"glob no match", "    find: {glob: '**/Chart.yaml'}\n", map[string]string{"app/values.yaml": "x", "Chart.yaml": "outside app"}, false, ""},
		{"glob bad pattern", "    find: {glob: 'a/[b'}\n", map[string]string{"app/a": "x"}, false, `error finding glob match for pattern "a/[b"`},
		{"glob leaving app", "    find: {glob: '../*'}\n", map[string]string{"app/a": "x"}, false, "pattern leaves the application directory"},
		{"fileName wins over glob", "    fileName: '*.cue'\n    find: {glob: '**'}\n", map[string]string{"app/a.yaml": "x"}, false, ""},
		{"command match", "    find: {command: [sh, -c, find . -name env.yaml]}\n", map[string]string{"app/x/env.yaml": "x"}, true, ""},
		{"command empty output", "    find: {command: [sh, -c, find . -name env.yaml]}\n", map[string]string{"app/x/other.yaml": "x"}, false, ""},
		{"command fails", "    find: {command: [sh, -c, 'echo bad >&2; exit 1']}\n", map[string]string{"app/a": "x"}, false,
			"match repository error: error running find command: `sh -c \"echo bad >&2; exit 1\"` failed exit status 1: bad"},
		{"command sees request env", "    find: {command: [sh, -c, 'test \"$ARGOCD_APP_NAME\" = app && echo yes']}\n", map[string]string{"app/a": "x"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := startServer(t, "", discoveryPlugin(tt.discover))
			files := tt.files
			body := streamBody(t, tarGzTree(t, files), "app", "ARGOCD_APP_NAME=app")
			status, data := srv.post(t, cmpwire.PathMatch, body, nil)
			if tt.errSubstr != "" {
				if status == http.StatusOK {
					t.Fatalf("expected error, got %s", data)
				}
				assertContains(t, decodeError(t, data).Message, tt.errSubstr)
				return
			}
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, data)
			}
			var resp cmpwire.MatchResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.IsSupported != tt.supported || !resp.IsDiscoveryEnabled {
				t.Fatalf("response = %+v, want supported=%v", resp, tt.supported)
			}
			waitNoEntries(t, srv.Server)
		})
	}

	t.Run("no discovery", func(t *testing.T) {
		t.Parallel()
		srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: none}\nspec: {generate: {command: [cat]}}")
		resp, status, perr := matchRequest(t, srv, map[string]string{"app/a.cue": "x"}, "app")
		if status != http.StatusOK || resp.IsSupported || resp.IsDiscoveryEnabled {
			t.Fatalf("status %d resp %+v err %+v", status, resp, perr)
		}
	})
}

// TestAutoDiscoveryEndToEnd runs the repo-server's auto-discovery over several real
// sidecars sharing a socket directory.
func TestAutoDiscoveryEndToEnd(t *testing.T) {
	t.Parallel()
	dir := socketDir(t)
	gen := func(name string) string {
		return fmt.Sprintf(`  generate: {command: [sh, -c, 'printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"%s\"}}"']}`, name)
	}
	startServer(t, dir, "kind: ConfigManagementPlugin\nmetadata: {name: a-nodiscovery}\nspec:\n"+gen("a"))
	startServer(t, dir, "kind: ConfigManagementPlugin\nmetadata: {name: b-cue}\nspec:\n"+gen("b")+"\n  discover: {fileName: '*.cue'}")
	startServer(t, dir, "kind: ConfigManagementPlugin\nmetadata: {name: c-yaml}\nspec:\n"+gen("c")+"\n  discover: {find: {glob: '**/*.yaml'}}")
	r := newRenderer(t, dir)
	plugins, err := r.ListPlugins()
	if err != nil || !reflect.DeepEqual(plugins, []string{"a-nodiscovery", "b-cue", "c-yaml"}) {
		t.Fatalf("ListPlugins = %q, %v", plugins, err)
	}
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"lexically first match wins", map[string]string{"app/main.cue": "x", "app/a/v.yaml": "x"}, "b"},
		{"second plugin", map[string]string{"app/a/v.yaml": "x"}, "c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := r.Generate(context.Background(), renderInput(t, tt.files, &v1alpha1.ApplicationSourcePlugin{}))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Manifests[0].GetName(); got != tt.want {
				t.Fatalf("plugin = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		_, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/x.txt": "x"}, &v1alpha1.ApplicationSourcePlugin{}))
		if err == nil {
			t.Fatal("expected error")
		}
		assertContains(t, err.Error(), "could not find plugin supporting the given repository")
	})
	t.Run("named plugin whose discovery does not match", func(t *testing.T) {
		t.Parallel()
		_, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/x.txt": "x"}, &v1alpha1.ApplicationSourcePlugin{Name: "b-cue"}))
		if err == nil {
			t.Fatal("expected error")
		}
		assertContains(t, err.Error(), `could not find cmp-server plugin with name "b-cue" supporting the given repository`)
	})
}

const paramsPlugin = `
kind: ConfigManagementPlugin
metadata: {name: params}
spec:
  generate: {command: [cat]}
  parameters:
    static:
      - name: string-param
        title: Title
        tooltip: Tip
        required: true
        itemType: number
        collectionType: string
        string: default
      - name: map-param
        collectionType: map
        map: {a: b}
    dynamic:
      command: [sh, -c]
      args: ['printf "[{\"name\":\"dyn-%s\",\"array\":[\"x\",\"y\"],\"collectionType\":\"array\"}]" "$ARGOCD_APP_NAME"']
`

func TestParametersEndToEnd(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", paramsPlugin)
	r := newRenderer(t, srv.dir)
	got, err := r.PluginParameters(context.Background(), renderInput(t, map[string]string{"app/a": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "params"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []apiclient.ParameterAnnouncement{
		{Name: "dyn-guestbook", Array: []string{"x", "y"}, CollectionType: "array"},
		{Name: "string-param", Title: "Title", Tooltip: "Tip", Required: true, ItemType: "number", CollectionType: "string", String: "default"},
		{Name: "map-param", CollectionType: "map", Map: map[string]string{"a": "b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("announcements = %#v\nwant %#v", got, want)
	}
}

func TestParametersErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, dynamic, want string
	}{
		{"invalid json", "[sh, -c, 'echo not-json']", "get parameters announcement error: error unmarshaling dynamic parameter output into ParametersAnnouncementResponse: invalid character"},
		{"command fails", "[sh, -c, 'exit 2']", "get parameters announcement error: error executing dynamic parameter output command: `sh -c \"exit 2\"` failed exit status 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec:\n  generate: {command: [cat]}\n  parameters: {dynamic: {command: "+tt.dynamic+"}}")
			status, data := srv.post(t, cmpwire.PathParameters, streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app"), nil)
			if status != http.StatusInternalServerError {
				t.Fatalf("status = %d: %s", status, data)
			}
			assertContains(t, decodeError(t, data).Message, tt.want)
		})
	}

	t.Run("static only", func(t *testing.T) {
		t.Parallel()
		srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: p}\nspec:\n  generate: {command: [cat]}")
		status, data := srv.post(t, cmpwire.PathParameters, streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app"), nil)
		if status != http.StatusOK || strings.TrimSpace(string(data)) != `{"parameterAnnouncements":[]}` {
			t.Fatalf("status %d body %s", status, data)
		}
	})
}

func TestGenerateOutputHandling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, generate string
		init           string
		manifests      int
		status         int
		errSubstr      string
	}{
		{name: "empty output", generate: "[sh, -c, 'true']", manifests: 0, status: http.StatusOK},
		{name: "json stream", generate: `[sh, -c, 'echo "{\"apiVersion\":\"v1\",\"kind\":\"A\"}{\"apiVersion\":\"v1\",\"kind\":\"B\"}"']`, manifests: 2, status: http.StatusOK},
		{name: "yaml with empty docs", generate: `[sh, -c, 'printf "apiVersion: v1\nkind: A\n---\n---\napiVersion: v1\nkind: B\n---\n"']`, manifests: 2, status: http.StatusOK},
		{name: "invalid output", generate: "[sh, -c, 'echo \"a: [\"']", status: http.StatusInternalServerError,
			errSubstr: "error generating manifests: failed to unmarshal manifest"},
		{name: "generate fails", generate: "[sh, -c, 'echo broken >&2; exit 4']", status: http.StatusInternalServerError,
			errSubstr: "error generating manifests: `sh -c \"echo broken >&2; exit 4\"` failed exit status 4: broken"},
		{name: "init fails", init: "[sh, -c, 'echo no-deps >&2; exit 1']", generate: "[cat, missing]", status: http.StatusInternalServerError,
			errSubstr: "error generating manifests: `sh -c \"echo no-deps >&2; exit 1\"` failed exit status 1: no-deps"},
		{name: "init output ignored", init: "[sh, -c, 'echo not-a-manifest: [']", generate: "[sh, -c, 'true']", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := "kind: ConfigManagementPlugin\nmetadata: {name: out}\nspec:\n  generate: {command: " + tt.generate + "}\n"
			if tt.init != "" {
				spec += "  init: {command: " + tt.init + "}\n"
			}
			srv := startServer(t, "", spec)
			status, data := srv.post(t, cmpwire.PathGenerate, streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app"), nil)
			if status != tt.status {
				t.Fatalf("status = %d, want %d: %s", status, tt.status, data)
			}
			if tt.errSubstr != "" {
				e := decodeError(t, data)
				assertContains(t, e.Message, tt.errSubstr)
				if e.Code != apiclient.CodeInternal {
					t.Fatalf("code = %q", e.Code)
				}
				waitNoEntries(t, srv.Server)
				return
			}
			var resp cmpwire.ManifestResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Manifests) != tt.manifests {
				t.Fatalf("manifests = %q", resp.Manifests)
			}
		})
	}
}

// TestGenerateErrorThroughClient checks the error chain the repo-server reports.
func TestGenerateErrorThroughClient(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: bad}\nspec:\n  generate: {command: [sh, -c, 'echo \"a: [\"']}")
	r := newRenderer(t, srv.dir)
	_, err := r.Generate(context.Background(), renderInput(t, map[string]string{"app/a": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "bad"}))
	if err == nil {
		t.Fatal("expected error")
	}
	assertContains(t, err.Error(), `CMP processing failed for application "guestbook": error generating manifests in cmp: error generating manifests: failed to unmarshal manifest`)
}

func TestPreserveFileMode(t *testing.T) {
	t.Parallel()
	files := map[string]string{"app/gen.sh": "#!/bin/sh\necho '{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"script\"}}'\n"}
	for _, preserve := range []bool{false, true} {
		t.Run(strconv.FormatBool(preserve), func(t *testing.T) {
			t.Parallel()
			srv := startServer(t, "", fmt.Sprintf("kind: ConfigManagementPlugin\nmetadata: {name: mode}\nspec:\n  preserveFileMode: %v\n  generate: {command: [./gen.sh]}", preserve))
			r := newRenderer(t, srv.dir)
			res, err := r.Generate(context.Background(), renderInput(t, files, &v1alpha1.ApplicationSourcePlugin{Name: "mode"}))
			if !preserve {
				if err == nil {
					t.Fatal("script ran without preserveFileMode")
				}
				assertContains(t, err.Error(), "`./gen.sh` failed gen.sh: permission denied: app/gen.sh is not executable")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Manifests[0].GetName() != "script" {
				t.Fatalf("manifests = %v", res.Manifests)
			}
		})
	}
}

// processGone waits until pid no longer exists.
func processGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d survived the command", pid)
}

func readPID(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// sleeperPlugin starts a background sleep (recording its PID in $PID_DIR/pid) and
// waits for it.
const sleeperPlugin = `
kind: ConfigManagementPlugin
metadata: {name: sleeper}
spec:
  generate:
    command: [sh, -c]
    args: ['sleep 60 & echo $! > "$PID_DIR/pid"; wait']
`

func TestExecTimeout(t *testing.T) {
	t.Parallel()
	pidDir := t.TempDir()
	srv := startServer(t, "", sleeperPlugin, func(c *Config) {
		c.Exec.Timeout = time.Second
		c.KillGrace = 200 * time.Millisecond
		c.Environ = append(c.Environ, "PID_DIR="+pidDir)
		c.PassEnv = []string{"PID_DIR"}
	})
	start := time.Now()
	status, data := srv.post(t, cmpwire.PathGenerate, streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app"), nil)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d: %s", status, data)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("took %s", d)
	}
	e := decodeError(t, data)
	if e.Code != apiclient.CodeDeadlineExceeded {
		t.Fatalf("code = %q", e.Code)
	}
	assertContains(t, e.Message, "error generating manifests: `sh -c \"sleep 60 & echo $! > \\\"$PID_DIR/pid\\\"; wait\"` failed exec timeout exceeded")
	processGone(t, readPID(t, filepath.Join(pidDir, "pid")))
	waitNoEntries(t, srv.Server)
}

// TestClientDeadline checks that the client deadline (minus 100 ms) bounds commands
// and that the whole process group is killed.
func TestClientDeadline(t *testing.T) {
	t.Parallel()
	pidDir := t.TempDir()
	srv := startServer(t, "", sleeperPlugin, func(c *Config) {
		c.Environ = append(c.Environ, "PID_DIR="+pidDir)
		c.PassEnv = []string{"PID_DIR"}
	})
	r := newRenderer(t, srv.dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Generate(ctx, renderInput(t, map[string]string{"app/a": "a"}, &v1alpha1.ApplicationSourcePlugin{Name: "sleeper"}))
	if err == nil {
		t.Fatal("expected error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
	if !strings.Contains(err.Error(), "failed request deadline exceeded") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	processGone(t, readPID(t, filepath.Join(pidDir, "pid")))
	waitNoEntries(t, srv.Server)

	// A deadline already in the past fails without running anything.
	hdr := http.Header{cmpwire.HeaderDeadline: {time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}}
	status, data := srv.post(t, cmpwire.PathGenerate, streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app"), hdr)
	if status != http.StatusGatewayTimeout || decodeError(t, data).Code != apiclient.CodeDeadlineExceeded {
		t.Fatalf("status %d: %s", status, data)
	}
}

// TestRejectedStreams covers malformed, oversized and malicious requests.
func TestRejectedStreams(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: guard}\nspec:\n  generate: {command: [sh, -c, 'echo should-not-run > /dev/null']}", func(c *Config) {
		c.MaxArchiveBytes = 256 << 10
		c.Archive = sandbox.ArchiveLimits{MaxTotalBytes: 1 << 20, MaxFiles: 50}
	})
	good := tarGzTree(t, map[string]string{"app/a": "a"})
	bomb := rawTarGz(t, tarEntry{name: "app/", typeflag: tar.TypeDir, mode: 0o755}, tarEntry{name: "app/zeros", typeflag: tar.TypeReg, size: 64 << 20})
	if len(bomb) > 256<<10 {
		t.Fatalf("bomb is %d bytes compressed", len(bomb))
	}
	manyFiles := []tarEntry{{name: "app/", typeflag: tar.TypeDir, mode: 0o755}}
	for i := range 60 {
		manyFiles = append(manyFiles, tarEntry{name: fmt.Sprintf("app/f%d", i), typeflag: tar.TypeReg, body: []byte("x")})
	}
	tampered := streamBody(t, good, "app")
	tampered[len(tampered)-5] ^= 0xff
	oversizedMeta, err := cmpwire.EncodeMetadata(cmpwire.Metadata{AppName: "app", AppRelPath: "app", Checksum: sha(nil), Size: 512 << 10})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		endpoint    string
		body        []byte
		contentType string
		header      http.Header
		method      string
		status      int
		code        string
		msg         string
	}{
		{name: "tar bomb", body: streamBody(t, bomb, "app"), status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted,
			msg: "generate manifest error receiving stream: error decompressing tgz file:"},
		{name: "too many files", body: streamBody(t, rawTarGz(t, manyFiles...), "app"), status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted},
		{name: "archive above limit", body: oversizedMeta, status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted,
			msg: "source archive exceeds the size limit: 524288 bytes, limit 262144"},
		{name: "dot-dot entry", body: streamBody(t, rawTarGz(t, tarEntry{name: "app/../../evil", typeflag: tar.TypeReg, body: []byte("x")}), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "absolute entry", body: streamBody(t, rawTarGz(t, tarEntry{name: "/tmp/patrol-cmp-evil", typeflag: tar.TypeReg, body: []byte("x")}), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "symlink out of tree", body: streamBody(t, rawTarGz(t, tarEntry{name: "app/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "relative symlink escape", body: streamBody(t, rawTarGz(t, tarEntry{name: "app/link", typeflag: tar.TypeSymlink, linkname: "../../.."}), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "device file", body: streamBody(t, rawTarGz(t, tarEntry{name: "app/dev", typeflag: tar.TypeChar}), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "checksum mismatch", body: tampered, status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument,
			msg: "generate manifest error receiving stream: file checksum validation error"},
		{name: "truncated payload", body: streamBody(t, good, "app")[:len(streamBody(t, good, "app"))-3], status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "no metadata", body: nil, status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument,
			msg: "generate manifest error receiving stream: error getting stream metadata: metadata is nil"},
		{name: "app path missing", body: streamBody(t, good, "nope"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument,
			msg: `error generating manifests: application path "nope" is not a directory of the received source`},
		{name: "not gzip", body: streamBody(t, []byte("plain text"), "app"), status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
		{name: "content type", body: streamBody(t, good, "app"), contentType: "application/json", status: http.StatusUnsupportedMediaType, code: apiclient.CodeInvalidArgument},
		{name: "method", method: http.MethodGet, status: http.StatusMethodNotAllowed, code: apiclient.CodeInvalidArgument},
		{name: "unknown endpoint", endpoint: "/plugin/v1/nope", body: streamBody(t, good, "app"), status: http.StatusNotFound, code: apiclient.CodeNotFound},
		{name: "bad deadline header", body: streamBody(t, good, "app"), header: http.Header{cmpwire.HeaderDeadline: {"tomorrow"}}, status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument},
	}
	t.Run("requests", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				endpoint := tt.endpoint
				if endpoint == "" {
					endpoint = cmpwire.PathGenerate
				}
				method := tt.method
				if method == "" {
					method = http.MethodPost
				}
				req, err := http.NewRequest(method, "http://cmp"+endpoint, strings.NewReader(string(tt.body)))
				if err != nil {
					t.Fatal(err)
				}
				ct := tt.contentType
				if ct == "" {
					ct = cmpwire.ContentTypeStream
				}
				req.Header.Set("Content-Type", ct)
				for k, v := range tt.header {
					req.Header[k] = v
				}
				resp, err := srv.client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				var e cmpwire.Error
				if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != tt.status || e.Code != tt.code {
					t.Fatalf("status %d code %q (%s), want %d %q", resp.StatusCode, e.Code, e.Message, tt.status, tt.code)
				}
				if ct := resp.Header.Get("Content-Type"); ct != cmpwire.ContentTypeProblem {
					t.Fatalf("content type %q", ct)
				}
				if tt.msg != "" {
					assertContains(t, e.Message, tt.msg)
				}
			})
		}
	})
	// The parallel subtests above have finished.
	waitNoEntries(t, srv.Server)
	if _, err := os.Stat("/tmp/patrol-cmp-evil"); err == nil {
		t.Fatal("absolute archive entry was written")
	}
}

// TestConcurrentRequestsIsolated runs generates in parallel: each sees only its own
// tree, and every per-request directory is removed afterwards (also on failure and
// when the plugin leaves an unwritable directory behind).
func TestConcurrentRequestsIsolated(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", `
kind: ConfigManagementPlugin
metadata: {name: iso}
spec:
  generate:
    command: [sh, -c]
    args:
      - |
        echo "$ARGOCD_APP_NAME" > marker
        mkdir -p locked/inner && chmod 000 locked
        sleep 0.3
        if [ "$ARGOCD_APP_NAME" = fail ]; then exit 1; fi
        printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"%s"},"data":{"marker":"%s","id":"%s"}}' "$ARGOCD_APP_NAME" "$(cat marker)" "$(cat id.txt)"
`)
	const n = 8
	names := make([]string, n)
	bodies := make([][]byte, n)
	for i := range n {
		names[i] = fmt.Sprintf("app%d", i)
		if i == n-1 {
			names[i] = "fail"
		}
		bodies[i] = streamBody(t, tarGzTree(t, map[string]string{"app/id.txt": names[i]}), "app", "ARGOCD_APP_NAME="+names[i])
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			name := names[i]
			status, data, err := srv.tryPost(cmpwire.PathGenerate, bodies[i])
			if err != nil {
				errs <- err
				return
			}
			if name == "fail" {
				if status == http.StatusOK {
					errs <- fmt.Errorf("failing plugin succeeded")
				}
				return
			}
			var resp cmpwire.ManifestResponse
			if status != http.StatusOK || json.Unmarshal(data, &resp) != nil || len(resp.Manifests) != 1 {
				errs <- fmt.Errorf("%s: status %d: %s", name, status, data)
				return
			}
			var obj struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal([]byte(resp.Manifests[0]), &obj); err != nil {
				errs <- err
				return
			}
			if obj.Data["marker"] != name || obj.Data["id"] != name {
				errs <- fmt.Errorf("%s saw %v", name, obj.Data)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	waitNoEntries(t, srv.Server)
}

func TestSocketLifecycle(t *testing.T) {
	t.Parallel()
	dir := socketDir(t)
	pluginYAML := "kind: ConfigManagementPlugin\nmetadata: {name: my-plugin}\nspec:\n  version: v1.0\n  provideGitCreds: true\n  discover: {fileName: '*.x'}\n  generate: {command: [cat]}"

	// A stale socket (no listener) is replaced.
	stale := filepath.Join(dir, "my-plugin-v1.0.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()

	cfg := Config{Plugin: mustParse(t, pluginYAML), SocketDir: dir, WorkDir: t.TempDir(), Logger: discardLogger()}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	waitForSocket(t, s.SocketPath(), done)
	if s.SocketPath() != stale {
		t.Fatalf("socket = %s", s.SocketPath())
	}
	fi, err := os.Lstat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != DefaultSocketMode {
		t.Fatalf("socket mode = %v", fi.Mode().Perm())
	}

	// CheckPluginConfiguration.
	ts := &testServer{Server: s, dir: dir}
	resp, err := ts.client().Get("http://cmp" + cmpwire.PathConfig)
	if err != nil {
		t.Fatal(err)
	}
	var cr cmpwire.ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !cr.IsDiscoveryConfigured || !cr.ProvideGitCreds {
		t.Fatalf("config = %+v", cr)
	}

	// A second server for the same plugin refuses to steal the live socket.
	s2, err := New(Config{Plugin: mustParse(t, pluginYAML), SocketDir: dir, WorkDir: t.TempDir(), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.ListenAndServe(context.Background()); err == nil || !strings.Contains(err.Error(), "another server is listening") {
		t.Fatalf("second server: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed: %v", err)
	}

	// A regular file in the way is not removed.
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s3, err := New(Config{Plugin: mustParse(t, pluginYAML), SocketDir: dir, WorkDir: t.TempDir(), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s3.ListenAndServe(context.Background()); err == nil || !strings.Contains(err.Error(), "is not a socket") {
		t.Fatalf("server over regular file: %v", err)
	}
}

func TestNewInitializesWorkDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	junk := filepath.Join(parent, WorkDirName, "old", "locked")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(junk), 0o500); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Plugin: mustParse(t, discoveryPlugin("    fileName: x\n")), WorkDir: parent, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if left := workDirEntries(t, s); len(left) != 0 {
		t.Fatalf("work dir not wiped: %v", left)
	}
	fi, err := os.Stat(s.WorkDir())
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("work dir mode %v, %v", fi.Mode().Perm(), err)
	}

	for _, bad := range []Config{
		{},
		{Plugin: &PluginConfig{}},
		{Plugin: mustParse(t, discoveryPlugin("    fileName: x\n")), WorkDir: "relative"},
		{Plugin: mustParse(t, discoveryPlugin("    fileName: x\n")), WorkDir: parent, PassEnv: []string{"A=B"}},
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%+v) succeeded", bad)
		}
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewPedanticRegistry()
	srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: m}\nspec:\n  generate: {command: [sh, -c, 'true']}", func(c *Config) { c.Registerer = reg })
	body := streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app")
	if status, data := srv.post(t, cmpwire.PathGenerate, body, nil); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, data)
	}
	if status, _ := srv.post(t, cmpwire.PathGenerate, nil, nil); status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	if got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues("generate", "success")); got != 1 {
		t.Fatalf("success = %v", got)
	}
	if got := testutil.ToFloat64(srv.metrics.requests.WithLabelValues("generate", apiclient.CodeInvalidArgument)); got != 1 {
		t.Fatalf("invalid_argument = %v", got)
	}
	if got := testutil.ToFloat64(srv.metrics.inFlight); got != 0 {
		t.Fatalf("in flight = %v", got)
	}
	if n, err := testutil.GatherAndCount(reg, "patrol_cmp_request_duration_seconds"); err != nil || n != 1 {
		t.Fatalf("duration series = %d, %v", n, err)
	}
	if _, err := newMetrics(reg, "m"); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}

func TestShutdown(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T, generate string, timeout time.Duration) (*testServer, context.CancelFunc, <-chan error) {
		t.Helper()
		s, err := New(Config{
			Plugin:          mustParse(t, "kind: ConfigManagementPlugin\nmetadata: {name: slow}\nspec:\n  generate: {command: "+generate+"}"),
			SocketDir:       socketDir(t),
			WorkDir:         t.TempDir(),
			Environ:         []string{"PATH=" + os.Getenv("PATH")},
			Logger:          discardLogger(),
			KillGrace:       -1,
			ShutdownTimeout: timeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.ListenAndServe(ctx) }()
		waitForSocket(t, s.SocketPath(), done)
		return &testServer{Server: s}, cancel, done
	}
	// waitInFlight waits until a request directory exists.
	waitInFlight := func(t *testing.T, s *Server) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for len(workDirEntries(t, s)) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("request never started")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	type result struct {
		status int
		data   []byte
		err    error
	}
	body := streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app")

	t.Run("in-flight request completes", func(t *testing.T) {
		t.Parallel()
		srv, cancel, done := start(t, `[sh, -c, 'sleep 0.5; echo "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\"}"']`, 20*time.Second)
		res := make(chan result, 1)
		go func() {
			status, data, err := srv.tryPost(cmpwire.PathGenerate, body)
			res <- result{status, data, err}
		}()
		waitInFlight(t, srv.Server)
		cancel()
		r := <-res
		if r.err != nil || r.status != http.StatusOK {
			t.Fatalf("request: %d %s %v", r.status, r.data, r.err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(srv.SocketPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("socket left: %v", err)
		}
	})

	t.Run("slow request cancelled after timeout", func(t *testing.T) {
		t.Parallel()
		srv, cancel, done := start(t, `[sleep, "60"]`, 200*time.Millisecond)
		res := make(chan result, 1)
		go func() {
			status, data, err := srv.tryPost(cmpwire.PathGenerate, body)
			res <- result{status, data, err}
		}()
		waitInFlight(t, srv.Server)
		begin := time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("server did not stop")
		}
		if d := time.Since(begin); d > 10*time.Second {
			t.Fatalf("shutdown took %s", d)
		}
		if r := <-res; r.err == nil && r.status == http.StatusOK {
			t.Fatal("cancelled request succeeded")
		}
		if left := workDirEntries(t, srv.Server); len(left) != 0 {
			t.Fatalf("work directory not cleaned: %v", left)
		}
	})
}
