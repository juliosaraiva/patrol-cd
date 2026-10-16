package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// pluginRequest is what the fake sidecar received in one streamed call.
type pluginRequest struct {
	path        string
	meta        cmpwire.Metadata
	files       []string // regular files and symlinks in the archive, slash-separated
	env         map[string]string
	hasDeadline bool
}

// fakePlugin is an in-test CMP sidecar speaking cmpwire on a unix socket.
type fakePlugin struct {
	// discoverGlob, when non-empty, configures discovery: a filepath.Glob relative
	// to the application directory (upstream discover.fileName semantics).
	discoverGlob    string
	provideGitCreds bool
	// generate returns the generate response; nil echoes the request as a ConfigMap.
	generate func(req pluginRequest) (cmpwire.ManifestResponse, *cmpwire.Error)
	params   []apiclient.ParameterAnnouncement
	// matchErr makes match calls fail.
	matchErr *cmpwire.Error

	mu       sync.Mutex
	requests []pluginRequest
	configs  atomic.Int32
}

func (f *fakePlugin) record(req pluginRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
}

func (f *fakePlugin) calls(path string) []pluginRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pluginRequest
	for _, r := range f.requests {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// pluginSocketDir returns a short temporary directory for unix sockets (macOS limits
// socket paths to 104 bytes, which t.TempDir() can exceed).
func pluginSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serve starts f on <dir>/<name>.sock.
func (f *fakePlugin) serve(t *testing.T, dir, name string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, name+cmpwire.SocketSuffix))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+cmpwire.PathConfig, func(w http.ResponseWriter, _ *http.Request) {
		f.configs.Add(1)
		writeJSON(w, http.StatusOK, cmpwire.ConfigResponse{IsDiscoveryConfigured: f.discoverGlob != "", ProvideGitCreds: f.provideGitCreds})
	})
	for _, p := range []string{cmpwire.PathMatch, cmpwire.PathGenerate, cmpwire.PathParameters} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, r *http.Request) { f.handleStream(t, w, r) })
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	ct := cmpwire.ContentTypeJSON
	if status != http.StatusOK {
		ct = cmpwire.ContentTypeProblem
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePlugin) handleStream(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != cmpwire.ContentTypeStream {
		writeJSON(w, http.StatusUnsupportedMediaType, cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: "bad content type"})
		return
	}
	meta, err := cmpwire.ReadMetadata(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: err.Error()})
		return
	}
	work, err := os.MkdirTemp("", "pcmp-work")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, cmpwire.Error{Code: apiclient.CodeInternal, Message: err.Error()})
		return
	}
	defer func() { _ = os.RemoveAll(work) }()
	payload := cmpwire.PayloadReader(r.Body, meta)
	if err := sandbox.ExtractTarGz(payload, work, sandbox.ArchiveLimits{}); err != nil {
		writeJSON(w, http.StatusBadRequest, cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: err.Error()})
		return
	}
	if _, err := io.Copy(io.Discard, payload); err != nil {
		writeJSON(w, http.StatusBadRequest, cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: err.Error()})
		return
	}
	req := pluginRequest{path: r.URL.Path, meta: meta, env: map[string]string{}, hasDeadline: r.Header.Get(cmpwire.HeaderDeadline) != ""}
	for _, e := range meta.Env {
		req.env[e.Name] = e.Value
	}
	_ = filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(work, p)
		req.files = append(req.files, filepath.ToSlash(rel))
		return nil
	})
	f.record(req)
	appDir := filepath.Join(work, filepath.FromSlash(meta.AppRelPath))

	switch r.URL.Path {
	case cmpwire.PathMatch:
		if f.matchErr != nil {
			writeJSON(w, http.StatusBadRequest, f.matchErr)
			return
		}
		matches, _ := filepath.Glob(filepath.Join(appDir, f.discoverGlob))
		writeJSON(w, http.StatusOK, cmpwire.MatchResponse{IsSupported: len(matches) > 0, IsDiscoveryEnabled: f.discoverGlob != ""})
	case cmpwire.PathGenerate:
		if f.generate != nil {
			resp, perr := f.generate(req)
			if perr != nil {
				writeJSON(w, http.StatusBadRequest, perr)
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		cmData := map[string]string{"files": strings.Join(req.files, ",")}
		for k, v := range req.env {
			cmData[strings.ReplaceAll(k, "_", "-")] = v
		}
		obj := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "plugin-output"}, "data": cmData}
		doc, _ := yaml.Marshal(obj)
		writeJSON(w, http.StatusOK, cmpwire.ManifestResponse{Manifests: []string{string(doc)}})
	case cmpwire.PathParameters:
		writeJSON(w, http.StatusOK, cmpwire.ParametersResponse{ParameterAnnouncements: f.params})
	}
}

func withPlugins(dir string) rendererOpt {
	return func(c *Config) { c.PluginSocketDir = dir }
}

func pluginInput(t testing.TB, files map[string]string, plugin *v1alpha1.ApplicationSourcePlugin) Input {
	t.Helper()
	return Input{
		RootDir:     tempRepo(t, files),
		AppName:     "test-app",
		Namespace:   "test-ns",
		ProjectName: "proj",
		Revision:    testRevision,
		KubeVersion: "v1.30.11+IKS",
		APIVersions: []string{"v1", "apps/v1", "apps/v1/Deployment"},
		Repo:        apiclient.Repository{URL: "https://git.example.com/repo.git", Username: "user", Password: "secret-password"},
		Source:      v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/repo.git", Path: "app", TargetRevision: "main", Plugin: plugin},
	}
}

var pluginRepoFiles = map[string]string{
	"app/main.cue":      "x: 1",
	"app/values.yaml":   "a: b",
	"app/run.sh":        "#!/bin/sh\n",
	"other/readme.md":   "other",
	".git/config":       "[core]",
	".git/objects/x":    "obj",
	"top.secret":        "s",
	"app/nested/x.yaml": "c: d",
}

func TestPluginNamedWithoutDiscovery(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	p := &fakePlugin{provideGitCreds: true}
	p.serve(t, dir, "named-v1.0")
	r := newTestRenderer(t, withPlugins(dir), func(c *Config) { c.PluginTarExclusions = []string{"*.secret"} })
	plugin := &v1alpha1.ApplicationSourcePlugin{
		Name: "named-v1.0",
		Env: v1alpha1.Env{
			{Name: "FOO", Value: "bar"},
			{Name: "REV", Value: "test-$ARGOCD_APP_REVISION_SHORT_8-${KUBE_VERSION}"},
			{Name: "PATH", Value: "/evil"},
			{Name: "ESC", Value: "$$FOO"},
		},
		Parameters: v1alpha1.ApplicationSourcePluginParameters{
			{Name: "values-files", OptionalArray: &v1alpha1.OptionalArray{Array: []string{"values-dev.yaml"}}},
			{Name: "helm-parameters", OptionalMap: &v1alpha1.OptionalMap{Map: map[string]string{"image.tag": "v1.2.3"}}},
			{Name: "single", String: ptrTo("one")},
			{Name: "empty", OptionalArray: &v1alpha1.OptionalArray{}},
		},
	}
	in := pluginInput(t, pluginRepoFiles, plugin)
	archived := 0
	in.OnSourceArchived = func() { archived++ }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Generate(ctx, in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertEqual(t, res.SourceType, "Plugin")
	assertEqual(t, archived, 1)
	if len(res.Commands) != 0 {
		t.Fatalf("commands = %q", res.Commands)
	}
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/plugin-output"})
	if n := len(p.calls(cmpwire.PathMatch)); n != 0 {
		t.Fatalf("named plugin without discovery was probed %d times", n)
	}

	gen := p.calls(cmpwire.PathGenerate)
	if len(gen) != 1 {
		t.Fatalf("generate calls = %d", len(gen))
	}
	req := gen[0]
	if !req.hasDeadline {
		t.Fatal("deadline header not sent")
	}
	assertEqual(t, req.meta.AppName, "app")
	assertEqual(t, req.meta.AppRelPath, "app")
	slices.Sort(req.files)
	assertSlice(t, req.files, []string{"app/main.cue", "app/nested/x.yaml", "app/run.sh", "app/values.yaml", "other/readme.md"})

	wantEnv := map[string]string{
		"ARGOCD_APP_NAME":                   "test-app",
		"ARGOCD_APP_NAMESPACE":              "test-ns",
		"ARGOCD_APP_PROJECT_NAME":           "proj",
		"ARGOCD_APP_REVISION":               testRevision,
		"ARGOCD_APP_REVISION_SHORT":         "0123456",
		"ARGOCD_APP_REVISION_SHORT_8":       "01234567",
		"ARGOCD_APP_SOURCE_REPO_URL":        "https://git.example.com/repo.git",
		"ARGOCD_APP_SOURCE_PATH":            "app",
		"ARGOCD_APP_SOURCE_TARGET_REVISION": "main",
		"PATROL_APP_NAME":                   "test-app",
		"PATROL_APP_REVISION_SHORT_8":       "01234567",
		"KUBE_VERSION":                      "1.30.11",
		"KUBE_API_VERSIONS":                 "v1,apps/v1,apps/v1/Deployment",
		"ARGOCD_ENV_FOO":                    "bar",
		"ARGOCD_ENV_REV":                    "test-01234567-1.30.11",
		"ARGOCD_ENV_PATH":                   "/evil",
		"ARGOCD_ENV_ESC":                    "$FOO",
		// Keys are sorted: upstream marshals each parameter through a map.
		"ARGOCD_APP_PARAMETERS":           `[{"array":["values-dev.yaml"],"name":"values-files"},{"map":{"image.tag":"v1.2.3"},"name":"helm-parameters"},{"name":"single","string":"one"},{"array":[],"name":"empty"}]`,
		"PARAM_VALUES_FILES_0":            "values-dev.yaml",
		"PARAM_HELM_PARAMETERS_IMAGE_TAG": "v1.2.3",
		"PARAM_SINGLE":                    "one",
	}
	for k, v := range wantEnv {
		got, ok := req.env[k]
		if !ok || got != v {
			t.Errorf("env %s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for k, v := range req.env {
		if k == "PATH" || strings.HasPrefix(k, "GIT_") || strings.Contains(v, "secret-password") {
			t.Errorf("unexpected env entry %s=%q", k, v)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestPluginSelectionErrors(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	(&fakePlugin{discoverGlob: "*.never"}).serve(t, dir, "disc")
	(&fakePlugin{discoverGlob: "*.cue", matchErr: &cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: "match repository error: boom"}}).serve(t, dir, "broken")
	r := newTestRenderer(t, withPlugins(dir))

	tests := []struct {
		name, plugin, want string
	}{
		{"discovery does not match", "disc", `CMP processing failed for application "test-app": could not find cmp-server plugin with name "disc" supporting the given repository`},
		{"unknown name", "nope", `could not find cmp-server plugin with name "nope" supporting the given repository: error dialing to cmp-server for plugin nope.sock:`},
		{"traversal", "../escape", `invalid socket file path, "../escape.sock" is outside plugin socket dir`},
		{"match error", "broken", `could not find cmp-server plugin with name "broken" supporting the given repository: error checking repository support for plugin broken.sock: match repository error: boom`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{Name: tt.plugin})
			err := generateErr(t, r, in)
			assertContains(t, err.Error(), tt.want)
		})
	}

	t.Run("explicit plugin block without a match", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{"app/a.txt": "x"}
		err := generateErr(t, r, pluginInput(t, files, &v1alpha1.ApplicationSourcePlugin{}))
		assertEqual(t, err.Error(), `CMP processing failed for application "test-app": could not find plugin supporting the given repository`)
	})
	t.Run("missing socket directory", func(t *testing.T) {
		t.Parallel()
		r := newTestRenderer(t, withPlugins(filepath.Join(dir, "missing")))
		err := generateErr(t, r, pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{}))
		assertContains(t, err.Error(), `CMP processing failed for application "test-app": failed to list all plugins in dir: `)
	})
}

func TestPluginAutoDiscovery(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	noDisc := &fakePlugin{}
	noDisc.serve(t, dir, "0-no-discovery")
	b := &fakePlugin{discoverGlob: "*.cue"}
	b.serve(t, dir, "b-plugin")
	a := &fakePlugin{discoverGlob: "*.cue", generate: func(pluginRequest) (cmpwire.ManifestResponse, *cmpwire.Error) {
		return cmpwire.ManifestResponse{Manifests: []string{cm("from-a", nil), cm("from-a-2", nil) + "---\n" + cm("from-a-3", nil)}}, nil
	}}
	a.serve(t, dir, "a-plugin")
	// A regular file with a .sock name is not a plugin.
	if err := os.WriteFile(filepath.Join(dir, "0-file.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRenderer(t, withPlugins(dir), withHelm(t))

	// Implicit detection: the matching plugin wins over Chart.yaml.
	files := map[string]string{"app/main.cue": "x: 1", "app/Chart.yaml": "apiVersion: v2\nname: c\nversion: 0.1.0\n"}
	in := pluginInput(t, files, nil)
	res := generate(t, r, in)
	assertEqual(t, res.SourceType, "Plugin")
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/from-a", "ConfigMap/from-a-2", "ConfigMap/from-a-3"})
	if len(noDisc.calls(cmpwire.PathMatch)) != 0 || len(noDisc.calls(cmpwire.PathGenerate)) != 0 {
		t.Fatal("plugin without discovery was used by auto-discovery")
	}
	if len(b.calls(cmpwire.PathGenerate)) != 0 {
		t.Fatal("lexically later plugin was used")
	}
	// Implicit detection probes send only the build environment.
	probe := a.calls(cmpwire.PathMatch)[0]
	if _, ok := probe.env["KUBE_VERSION"]; ok {
		t.Fatal("detection probe received KUBE_VERSION")
	}
	assertEqual(t, probe.env["ARGOCD_APP_NAME"], "test-app")

	// The explicit `plugin: {}` block selects by discovery too.
	in.Source.Plugin = &v1alpha1.ApplicationSourcePlugin{}
	assertEqual(t, generate(t, r, in).SourceType, "Plugin")

	// Without a match, implicit detection falls back to the built-in tools.
	in = pluginInput(t, map[string]string{"app/Chart.yaml": "apiVersion: v2\nname: c\nversion: 0.1.0\n", "app/templates/cm.yaml": cm("c", nil)}, nil)
	res = generate(t, r, in)
	assertEqual(t, res.SourceType, "Helm")

	// Discover (ListApps) reports a plugin-claimed path as {".": "Plugin"}.
	discoverRoot := tempRepo(t, files)
	apps, err := r.Discover(context.Background(), discoverRoot, filepath.Join(discoverRoot, "app"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps["."] != "Plugin" {
		t.Fatalf("Discover = %v", apps)
	}
}

func TestPluginGenerateOutput(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	var mode atomic.Value
	mode.Store("")
	p := &fakePlugin{generate: func(pluginRequest) (cmpwire.ManifestResponse, *cmpwire.Error) {
		switch mode.Load().(string) {
		case "error":
			return cmpwire.ManifestResponse{}, &cmpwire.Error{Code: apiclient.CodeInvalidArgument, Message: "`sh -c exit 1` failed exit status 1: oops"}
		case "invalid":
			return cmpwire.ManifestResponse{Manifests: []string{"{not yaml"}}, nil
		case "huge":
			return cmpwire.ManifestResponse{Manifests: []string{cm("big", map[string]string{"x": strings.Repeat("a", 8192)})}}, nil
		case "nokind":
			return cmpwire.ManifestResponse{Manifests: []string{"apiVersion: v1\nmetadata:\n  name: x\n"}}, nil
		case "list":
			return cmpwire.ManifestResponse{Manifests: []string{"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: l1\n"}}, nil
		}
		return cmpwire.ManifestResponse{}, nil
	}}
	p.serve(t, dir, "p")
	r := newTestRenderer(t, withPlugins(dir), func(c *Config) { c.Limits.MaxOutputBytes = 4096 })
	run := func(m string) (*Result, error) {
		mode.Store(m)
		return r.Generate(context.Background(), pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{Name: "p"}))
	}
	res, err := run("")
	if err != nil || len(res.Manifests) != 0 {
		t.Fatalf("empty output: %v %v", res, err)
	}
	_, err = run("error")
	assertEqual(t, err.Error(), "CMP processing failed for application \"test-app\": error generating manifests in cmp: `sh -c exit 1` failed exit status 1: oops")
	_, err = run("invalid")
	assertContains(t, err.Error(), "failed to convert CMP manifests to unstructured objects: ")
	_, err = run("huge")
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("huge output: %v", err)
	}
	_, err = run("nokind")
	assertContains(t, err.Error(), "failed to convert CMP manifests to unstructured objects")
	res, err = run("list")
	if err != nil {
		t.Fatal(err)
	}
	assertSlice(t, names(res.Manifests), []string{"ConfigMap/l1"})
}

func TestPluginManifestGeneratePaths(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	p := &fakePlugin{}
	p.serve(t, dir, "p")
	files := map[string]string{
		"apps/a/main.cue":     "x",
		"apps/shared/lib.cue": "y",
		"apps/b/other.cue":    "z",
		"root.txt":            "r",
	}
	on := newTestRenderer(t, withPlugins(dir), func(c *Config) { c.PluginUseManifestGeneratePaths = true })
	off := newTestRenderer(t, withPlugins(dir))

	in := pluginInput(t, files, &v1alpha1.ApplicationSourcePlugin{Name: "p"})
	in.Source.Path = "apps/a"
	in.ManifestGeneratePaths = ".;../shared"
	generate(t, on, in)
	req := p.calls(cmpwire.PathGenerate)[0]
	assertEqual(t, req.meta.AppRelPath, "a")
	slices.Sort(req.files)
	assertSlice(t, req.files, []string{"a/main.cue", "b/other.cue", "shared/lib.cue"})

	generate(t, off, in)
	req = p.calls(cmpwire.PathGenerate)[1]
	assertEqual(t, req.meta.AppRelPath, "apps/a")
	slices.Sort(req.files)
	assertSlice(t, req.files, []string{"apps/a/main.cue", "apps/b/other.cue", "apps/shared/lib.cue", "root.txt"})

	// Annotation entries relative to the repository root and app-only.
	in.ManifestGeneratePaths = "."
	generate(t, on, in)
	req = p.calls(cmpwire.PathGenerate)[2]
	assertEqual(t, req.meta.AppRelPath, ".")
	assertSlice(t, req.files, []string{"main.cue"})
}

// TestPluginManifestGeneratePathsSymlinkOutsideSubtree pins the behavior for a
// repository symlink that leaves the streamed subtree: with
// PluginUseManifestGeneratePaths the archive root is the common root, and a link
// that escapes it fails the call before anything is sent. Upstream writes such a
// link into the archive, and its cmp-server's untar then rejects it (os.Root.Stat of
// the joined target: "path escapes from parent", util/io/files/tar.go untar), so the
// generation fails there too; Patrol's sidecar extraction rejects it the same way
// (sandbox.ExtractTarGz, used by internal/cmp). Streaming the whole repository
// (flag off) keeps the link inside the archive root and succeeds.
func TestPluginManifestGeneratePathsSymlinkOutsideSubtree(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	p := &fakePlugin{}
	p.serve(t, dir, "p")
	files := map[string]string{
		"apps/a/main.cue":     "x",
		"apps/a/shared":       "->../shared",
		"apps/shared/lib.cue": "y",
	}
	in := pluginInput(t, files, &v1alpha1.ApplicationSourcePlugin{Name: "p"})
	in.Source.Path = "apps/a"
	in.ManifestGeneratePaths = "."

	on := newTestRenderer(t, withPlugins(dir), func(c *Config) { c.PluginUseManifestGeneratePaths = true })
	err := generateErr(t, on, in)
	var oob *sandbox.OutOfBoundsSymlinkError
	if !errors.As(err, &oob) {
		t.Fatalf("error = %v, want *sandbox.OutOfBoundsSymlinkError", err)
	}
	assertContains(t, err.Error(), `CMP processing failed for application "test-app": error compressing repo files: out of bounds symlink found: shared`)
	assertEqual(t, len(p.calls(cmpwire.PathGenerate)), 0)

	off := newTestRenderer(t, withPlugins(dir))
	generate(t, off, in)
	req := p.calls(cmpwire.PathGenerate)[0]
	slices.Sort(req.files)
	assertSlice(t, req.files, []string{"apps/a/main.cue", "apps/a/shared", "apps/shared/lib.cue"})

	// The receiving side rejects the archive upstream would have sent.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range []*tar.Header{
		{Name: "main.cue", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		{Name: "shared", Typeflag: tar.TypeSymlink, Linkname: "../shared", Mode: 0o777},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	err = sandbox.ExtractTarGz(&buf, t.TempDir(), sandbox.ArchiveLimits{})
	if !errors.As(err, &oob) {
		t.Fatalf("extract error = %v, want *sandbox.OutOfBoundsSymlinkError", err)
	}
}

func TestApplicationRootPath(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"apps/a/x":      "x",
		"apps/shared/y": "y",
		"other/z":       "z",
		"apps/a/link":   "->../../other",
	})
	app := filepath.Join(root, "apps", "a")
	log := newTestRenderer(t).log
	tests := []struct {
		annotation, want string
	}{
		{"", root},
		{".", app},
		{".;../shared", filepath.Join(root, "apps")},
		{"/other", root},
		{"../../../../../etc", root}, // clamped at the root
		{";;", root},
		{"link", root}, // resolves to other/
		{"/apps/a/sub", app},
	}
	for _, tt := range tests {
		if got := applicationRootPath(root, app, tt.annotation, log); got != tt.want {
			t.Errorf("annotation %q: got %s, want %s", tt.annotation, got, tt.want)
		}
	}
}

func TestPluginParametersAndList(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	params := []apiclient.ParameterAnnouncement{{Name: "dynamic", Title: "Dynamic"}, {Name: "static", Required: true, ItemType: "string", CollectionType: "map", Map: map[string]string{"k": "v"}}}
	p := &fakePlugin{discoverGlob: "*.cue", params: params}
	p.serve(t, dir, "my-plugin-v1.0")
	(&fakePlugin{}).serve(t, dir, "other")
	if err := os.WriteFile(filepath.Join(dir, "not-a-socket.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRenderer(t, withPlugins(dir))

	got, err := r.ListPlugins()
	if err != nil {
		t.Fatal(err)
	}
	assertSlice(t, got, []string{"my-plugin-v1.0", "other"})
	if _, err := newTestRenderer(t, withPlugins(filepath.Join(dir, "missing"))).ListPlugins(); err == nil || !strings.Contains(err.Error(), "failed to get plugins from dir") {
		t.Fatalf("missing dir: %v", err)
	}

	in := pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{
		Env:        v1alpha1.Env{{Name: "FOO", Value: "x-$ARGOCD_APP_NAME"}},
		Parameters: v1alpha1.ApplicationSourcePluginParameters{{Name: "p", String: ptrTo("v")}},
	})
	ann, err := r.PluginParameters(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ann) != 2 || ann[0].Name != "dynamic" || ann[1].Map["k"] != "v" {
		t.Fatalf("announcements = %+v", ann)
	}
	req := p.calls(cmpwire.PathParameters)[0]
	for _, k := range []string{"ARGOCD_APP_REVISION", "ARGOCD_APP_NAMESPACE", "KUBE_VERSION", "ARGOCD_APP_PROJECT_NAME"} {
		if _, ok := req.env[k]; ok {
			t.Errorf("parameters env has %s", k)
		}
	}
	assertEqual(t, req.env["ARGOCD_APP_NAME"], "test-app")
	assertEqual(t, req.env["ARGOCD_APP_SOURCE_TARGET_REVISION"], "main")
	assertEqual(t, req.env["ARGOCD_ENV_FOO"], "x-test-app")
	assertEqual(t, req.env["PARAM_P"], "v")
}

func TestPluginDialRetry(t *testing.T) {
	t.Parallel()
	dir := pluginSocketDir(t)
	// A socket file whose listener is gone: dialing fails every time.
	l, err := net.Listen("unix", filepath.Join(dir, "dead.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close()
	r := newTestRenderer(t, withPlugins(dir), func(c *Config) { c.CMPRetryAttempts = 3; c.CMPRetryBackoff = 5 * time.Millisecond })
	start := time.Now()
	err = generateErr(t, r, pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{Name: "dead"}))
	assertContains(t, err.Error(), "error dialing to cmp-server for plugin dead.sock")
	if time.Since(start) < 15*time.Millisecond {
		t.Fatalf("no backoff between attempts (%s)", time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Generate(ctx, pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{Name: "dead"})); err == nil {
		t.Fatal("canceled context: want error")
	}
}

func TestPluginParamEnvEscaping(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"helm-parameters": "HELM_PARAMETERS",
		"image.tag":       "IMAGE_TAG",
		"a_b":             "A_B",
		"ünï":             "ÜNÏ",
		"straße":          "STRA_E", // unicode.ToUpper keeps ß, which is then replaced
		"x y/z":           "X_Y_Z",
	}
	for in, want := range tests {
		got := escapeParamName(in)
		// Non-ASCII letters survive ToUpper and are then replaced.
		want = strings.Map(func(r rune) rune {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
				return r
			}
			return '_'
		}, want)
		assertEqual(t, got, want)
	}
	env, err := pluginParametersEnv(v1alpha1.ApplicationSourcePluginParameters{
		{Name: "m", OptionalMap: &v1alpha1.OptionalMap{Map: map[string]string{"b": "2", "a": "1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSlice(t, env, []string{`ARGOCD_APP_PARAMETERS=[{"map":{"a":"1","b":"2"},"name":"m"}]`, "PARAM_M_A=1", "PARAM_M_B=2"})
	assertEqual(t, fmt.Sprint(len(env)), "3")
}
