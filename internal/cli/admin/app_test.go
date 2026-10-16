package admin

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
)

func TestAppGenerateSpecGolden(t *testing.T) {
	t.Parallel()
	out, _, err := offline(t, "admin", "app", "generate-spec", "guestbook", "--repo", "https://github.com/example/apps.git", "--path", "guestbook",
		"--dest-namespace", "default", "--dest-server", "https://kubernetes.default.svc", "--directory-recurse", "--sync-policy", "automated",
		"--auto-prune", "--self-heal", "--label", "team=a", "--annotations", "note=x", "--set-finalizer", "--sync-option", "CreateNamespace=true", "--project", "team-a")
	require.NoError(t, err)
	golden, err := os.ReadFile("testdata/app/guestbook.golden.yaml")
	require.NoError(t, err)
	assert.Equal(t, string(golden), out)
}

func TestAppGenerateSpecVariants(t *testing.T) {
	t.Parallel()
	// Helm chart from a Helm repository, JSON output.
	out, _, err := offline(t, "admin", "app", "generate-spec", "nginx", "--repo", "https://charts.example.com", "--helm-chart", "nginx", "--revision", "1.2.3",
		"--dest-name", "prod", "--dest-namespace", "web", "--helm-set", "replicaCount=2", "--values", "values-prod.yaml", "--release-name", "web", "-o", "json")
	require.NoError(t, err)
	app := &v1alpha1.Application{}
	require.NoError(t, yaml.Unmarshal([]byte(out), app))
	assert.Equal(t, "nginx", app.Name)
	require.NotNil(t, app.Spec.Source)
	assert.Equal(t, "nginx", app.Spec.Source.Chart)
	assert.Equal(t, "1.2.3", app.Spec.Source.TargetRevision)
	require.NotNil(t, app.Spec.Source.Helm)
	assert.Equal(t, "web", app.Spec.Source.Helm.ReleaseName)
	assert.Equal(t, []string{"values-prod.yaml"}, app.Spec.Source.Helm.ValueFiles)
	assert.Equal(t, "replicaCount", app.Spec.Source.Helm.Parameters[0].Name)
	assert.Equal(t, "prod", app.Spec.Destination.Name)
	assert.True(t, strings.HasPrefix(out, "{\n"))

	// From a file, with flag overrides merged.
	out, _, err = offline(t, "admin", "app", "generate-spec", "-f", "testdata/app/app.yaml", "--revision", "v2", "--label", "team=b")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal([]byte(out), app))
	assert.Equal(t, "from-file", app.Name)
	assert.Equal(t, "v2", app.Spec.Source.TargetRevision)
	assert.Equal(t, map[string]string{"origin": "file", "team": "b"}, app.Labels)

	// From a URL.
	data, err := os.ReadFile("testdata/app/app.yaml")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	t.Cleanup(srv.Close)
	out, _, err = offline(t, "admin", "app", "generate-spec", "-f", srv.URL+"/app.yaml")
	require.NoError(t, err)
	assert.Contains(t, out, "name: from-file")

	// Name mismatch, missing name.
	_, _, err = offline(t, "admin", "app", "generate-spec", "other", "-f", "testdata/app/app.yaml")
	require.ErrorContains(t, err, "app name 'other' does not match app spec metadata.name 'from-file'")
	out, _, err = offline(t, "admin", "app", "generate-spec")
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "Usage:")
}

func TestAppGenerateSpecInline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	orig, err := os.ReadFile("testdata/app/app.yaml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, orig, 0o640))

	out, _, err := offline(t, "admin", "app", "generate-spec", "-f", path, "-i", "--revision", "v3")
	require.NoError(t, err)
	assert.Empty(t, out)
	back, err := os.ReadFile(path + ".back")
	require.NoError(t, err)
	assert.Equal(t, orig, back)
	updated, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "targetRevision: v3")
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), st.Mode().Perm(), "the rewritten file keeps the original mode")

	_, _, err = offline(t, "admin", "app", "generate-spec", "guestbook", "-i")
	require.ErrorContains(t, err, "the file path must be specified using flag '--file'")
}

func TestDiffReconcileResults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.yaml")
	second := filepath.Join(dir, "second.yaml")
	require.NoError(t, os.WriteFile(first, []byte(`applications:
- name: guestbook
  health: Healthy
  sync:
    status: Synced
- name: removed
  health: Healthy
  sync:
    status: Synced
`), 0o600))
	require.NoError(t, os.WriteFile(second, []byte(`applications:
- name: guestbook
  health: Degraded
  sync:
    status: OutOfSync
- name: added
  health: Progressing
  sync:
    status: Synced
`), 0o600))
	out, _, err := offline(t, "admin", "app", "diff-reconcile-results", first, second)
	require.NoError(t, err)
	// Applications are printed sorted by name, each followed by its diff.
	iAdded, iGuestbook, iRemoved := strings.Index(out, "added\n"), strings.Index(out, "guestbook\n"), strings.Index(out, "removed\n")
	require.True(t, iAdded >= 0 && iGuestbook > iAdded && iRemoved > iGuestbook, out)
	assert.Contains(t, out, "-health: Healthy\n+health: Degraded")
	assert.Contains(t, out, "-  status: Synced\n+  status: OutOfSync")
	assert.Contains(t, out, "+health: Progressing")
	assert.Contains(t, out, "-name: removed")

	same, _, err := offline(t, "admin", "app", "diff-reconcile-results", first, first)
	require.NoError(t, err)
	assert.Equal(t, "guestbook\nremoved\n", same)

	_, _, err = offline(t, "admin", "app", "diff-reconcile-results", first, filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
}

// localGitRepo returns a file:// URL of a bare repository whose HEAD has the given files.
func localGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "repo.git")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com"}
	run := func(dir string, args ...string) {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.MkdirAll(work, 0o755))
	run(work, "init", "-q", "-b", "main")
	for name, content := range files {
		p := filepath.Join(work, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	run(work, "add", ".")
	run(work, "commit", "-q", "-m", "initial")
	run(root, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

// startRepoServer serves a real repo-server over plain HTTP on loopback and returns its
// address and a token file.
func startRepoServer(t *testing.T) (string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	empty := t.TempDir()
	// The repo-server makes its working copies read-only: remove them with permissions
	// restored, outside t.TempDir.
	rootParent, err := os.MkdirTemp("", "patrol-admin-reposerver-")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = filepath.WalkDir(rootParent, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		_ = os.RemoveAll(rootParent)
	})
	const token = "admin-test-internal-token"
	svc, err := reposerver.New(reposerver.Config{
		Token: token, RootDir: filepath.Join(rootParent, "root"), AllowLocalRepositories: true, GitPath: git,
		ToolPATH: filepath.Dir(git) + string(os.PathListSeparator) + "/usr/bin:/bin", Registerer: prometheus.NewRegistry(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PluginSocketDir: filepath.Join(empty, "no-plugins"),
		SSHKnownHostsPath: reposerver.TrustMaterialDisabled, TLSCertsDir: reposerver.TrustMaterialDisabled,
		GPGKeysDir: reposerver.TrustMaterialDisabled, DisableGPG: true, ExecTimeout: time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	h, err := svc.Handler()
	require.NoError(t, err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	srv.Start()
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(token+"\n"), 0o600))
	return l.Addr().String(), tokenFile
}

func TestGetReconcileResults(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "reconcile")
	appGVR := kindApplication.gvr(currentGroup)
	repoURL := localGitRepo(t, map[string]string{"manifests/cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: rendered-by-test
data:
  key: value
`})
	createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AppProject", "metadata": map[string]any{"name": "default"},
		"spec": map[string]any{
			"sourceRepos":              []any{"*"},
			"destinations":             []any{map[string]any{"server": "*", "namespace": "*"}},
			"clusterResourceWhitelist": []any{map[string]any{"group": "*", "kind": "*"}},
		},
	}})
	newApp := func(name, team string) {
		app := createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Application",
			"metadata": map[string]any{"name": name, "labels": map[string]any{"team": team}},
			"spec": map[string]any{
				"project":     "default",
				"source":      map[string]any{"repoURL": repoURL, "path": "manifests", "targetRevision": "HEAD"},
				"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": ns},
			},
		}})
		app.Object["status"] = map[string]any{
			"sync":   map[string]any{"status": "Synced", "revision": "abc"},
			"health": map[string]any{"status": "Healthy"},
			"conditions": []any{map[string]any{
				"type": "ComparisonError", "status": "True", "reason": "Test", "message": "stored condition",
				"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
			}},
		}
		_, err := envDyn.Resource(appGVR).Namespace(ns).UpdateStatus(ctx, app, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	newApp("app-a", "a")
	newApp("app-b", "b")

	tio := newTestIO(ns)
	path := filepath.Join(t.TempDir(), "results.yaml")
	require.NoError(t, tio.run(ctx, "admin", "app", "get-reconcile-results", path))
	var stored reconcileResults
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, &stored))
	require.Len(t, stored.Applications, 2)
	assert.Equal(t, "app-a", stored.Applications[0].Name)
	assert.Equal(t, v1alpha1.HealthStatusCode("Healthy"), stored.Applications[0].Health)
	assert.Equal(t, v1alpha1.SyncStatusCode("Synced"), stored.Applications[0].Sync.Status)
	assert.Equal(t, "stored condition", stored.Applications[0].Conditions[0].Message)

	// Label selector and JSON output.
	require.NoError(t, tio.run(ctx, "admin", "app", "get-reconcile-results", path, "--l", "team=b", "--o", "json"))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "{"))
	require.NoError(t, yaml.Unmarshal(data, &stored))
	require.Len(t, stored.Applications, 1)
	assert.Equal(t, "app-b", stored.Applications[0].Name)
	require.ErrorContains(t, tio.run(ctx, "admin", "app", "get-reconcile-results", path, "--o", "xml"), "format xml is not supported")
	require.ErrorContains(t, tio.run(ctx, "admin", "app", "get-reconcile-results", path, "--refresh"), "--refresh requires --repo-server")

	// --refresh compares again with a real repo-server: the rendered ConfigMap does not
	// exist, so the Application is OutOfSync whatever its stored status says.
	addr, tokenFile := startRepoServer(t)
	refreshed := filepath.Join(t.TempDir(), "refreshed.yaml")
	err = tio.run(ctx, "admin", "app", "get-reconcile-results", refreshed, "--refresh", "--l", "team=a",
		"--repo-server", addr, "--repo-server-plaintext", "--repo-server-token-file", tokenFile)
	require.NoError(t, err, tio.errOut.String())
	assert.Contains(t, tio.out.String(), "Reconciling apps of https://kubernetes.default.svc\napp-a\n")
	data, err = os.ReadFile(refreshed)
	require.NoError(t, err)
	var fresh reconcileResults
	require.NoError(t, yaml.Unmarshal(data, &fresh))
	require.Len(t, fresh.Applications, 1)
	assert.Equal(t, v1alpha1.SyncStatusCode("OutOfSync"), fresh.Applications[0].Sync.Status, string(data))
	assert.Equal(t, v1alpha1.HealthStatusCode("Missing"), fresh.Applications[0].Health, string(data))
	assert.NotEmpty(t, fresh.Applications[0].Sync.Revision, "the revision resolved by the repo-server is reported")
	assert.Empty(t, fresh.Applications[0].Conditions, "the stored ComparisonError is cleared by the successful comparison")

	// A comparison failure (the repo-server rejects a wrong token) is reported as the
	// upstream ComparisonError condition with an Unknown sync status.
	bad := filepath.Join(t.TempDir(), "bad-token")
	require.NoError(t, os.WriteFile(bad, []byte("wrong-token-value"), 0o600))
	require.NoError(t, tio.run(ctx, "admin", "app", "get-reconcile-results", refreshed, "--refresh", "--l", "team=a",
		"--repo-server", addr, "--repo-server-plaintext", "--repo-server-token-file", bad))
	data, err = os.ReadFile(refreshed)
	require.NoError(t, err)
	assert.NotContains(t, string(data)+tio.out.String()+tio.errOut.String(), "wrong-token-value")
	fresh = reconcileResults{}
	require.NoError(t, yaml.Unmarshal(data, &fresh))
	require.Len(t, fresh.Applications, 1)
	assert.Equal(t, v1alpha1.SyncStatusCode("Unknown"), fresh.Applications[0].Sync.Status, string(data))
	require.NotEmpty(t, fresh.Applications[0].Conditions, string(data))
	assert.Equal(t, "ComparisonError", fresh.Applications[0].Conditions[0].Type)
}
