package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestNormalDiff(t *testing.T) {
	t.Parallel()
	assert.Empty(t, normalDiff("x\n", "x\n"))
	tests := []struct {
		name, a, b, want string
	}{
		{"change", "l1\nl2\nl3\n", "l1\nX\nl3\n", "2c2\n< l2\n---\n> X\n"},
		{"range change", "a\nb\nc\nd\n", "a\nX\nY\nZ\nd\n", "2,3c2,4\n< b\n< c\n---\n> X\n> Y\n> Z\n"},
		{"append", "a\nb\n", "a\nb\nc\n", "2a3\n> c\n"},
		{"delete", "a\nb\nc\n", "a\nc\n", "2d1\n< b\n"},
		{"creation (empty live)", "", "x: 1\ny: 2\n", "0a1,2\n> x: 1\n> y: 2\n"},
		{"deletion (empty target)", "x: 1\n", "", "1d0\n< x: 1\n"},
		{"two hunks", "1\n2\n3\n4\n5\n", "1\nX\n3\nY\n5\n", "2c2\n< 2\n---\n> X\n4c4\n< 4\n---\n> Y\n"},
		{"no final newline", "x", "y", "1c1\n< x\n\\ No newline at end of file\n---\n> y\n\\ No newline at end of file\n"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, normalDiff(tc.a, tc.b), tc.name)
	}
}

func TestSplitCommandLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
		err  bool
	}{
		{in: "diff -u -N", want: []string{"diff", "-u", "-N"}},
		{in: `colordiff "--label x" 'a b'`, want: []string{"colordiff", "--label x", "a b"}},
		{in: `tool a\ b`, want: []string{"tool", "a b"}},
		{in: `  spaced   words  `, want: []string{"spaced", "words"}},
		{in: `"unterminated`, err: true},
	}
	for _, tc := range tests {
		got, err := splitCommandLine(tc.in)
		if tc.err {
			assert.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestPrintResourceDiffExternalProgram(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell script diff program")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "mydiff")
	// Prints both file names (base) and their contents, then exits 1 like diff tools do.
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" | sed 's#.*/##'; cat \"$1\"; echo \"$2\" | sed 's#.*/##'; cat \"$2\"; exit 1\n"), 0o700))
	e := defaultTestEnv()
	e.getenv = func(k string) string {
		if k == envKubectlExtDiff {
			return script
		}
		return ""
	}
	live := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}, "data": map[string]any{"k": "old"}}}
	target := live.DeepCopy()
	target.Object["data"] = map[string]any{"k": "new"}
	var out, errOut bytes.Buffer
	key := resourceKey{Kind: "ConfigMap", Namespace: "ns", Name: "cm"}
	require.NoError(t, e.printResourceDiff(context.Background(), &out, &errOut, key, live, target))
	assert.Equal(t, "\n===== /ConfigMap ns/cm ======\ncm-live.yaml\napiVersion: v1\ndata:\n  k: old\nkind: ConfigMap\nmetadata:\n  name: cm\ncm\napiVersion: v1\ndata:\n  k: new\nkind: ConfigMap\nmetadata:\n  name: cm\n", out.String())

	// PATROL_DIFF_PROGRAM wins over KUBECTL_EXTERNAL_DIFF.
	e.getenv = func(k string) string {
		switch k {
		case envDiffProgram:
			return "false"
		case envKubectlExtDiff:
			return script
		}
		return ""
	}
	out.Reset()
	require.NoError(t, e.printResourceDiff(context.Background(), &out, &errOut, key, live, target), "exit status 1 is a difference, not an error")
	assert.Equal(t, "\n===== /ConfigMap ns/cm ======\n", out.String())

	e.getenv = func(k string) string {
		if k == envDiffProgram {
			return filepath.Join(dir, "missing-tool")
		}
		return ""
	}
	require.Error(t, e.printResourceDiff(context.Background(), &out, &errOut, key, live, target))
}

func manifest(t *testing.T, obj map[string]any) string {
	t.Helper()
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(b)
}

func cm(name, ns, value string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": ns}, "data": map[string]any{"k": value}}
}

func TestComputeDiffClientSide(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"},
		Spec: v1alpha1.ApplicationSpec{
			Destination: v1alpha1.ApplicationDestination{Namespace: "default"},
			IgnoreDifferences: v1alpha1.IgnoreDifferences{
				{Kind: "ConfigMap", Name: "ignored", JSONPointers: []string{"/data"}},
			},
		},
	}
	hook := cm("hook", "default", "x")
	hook["metadata"].(map[string]any)["annotations"] = map[string]any{v1alpha1.AnnotationKeyHook: "PreSync"}
	managed := []apiclient.ResourceDiff{
		{Kind: "ConfigMap", Namespace: "default", Name: "same", TargetState: manifest(t, cm("same", "default", "v")), NormalizedLiveState: manifest(t, cm("same", "default", "v"))},
		{Kind: "ConfigMap", Namespace: "default", Name: "changed", TargetState: manifest(t, cm("changed", "default", "new")), NormalizedLiveState: manifest(t, cm("changed", "default", "old"))},
		{Kind: "ConfigMap", Namespace: "default", Name: "missing", TargetState: manifest(t, cm("missing", "default", "v")), NormalizedLiveState: "null"},
		{Kind: "ConfigMap", Namespace: "default", Name: "extra", TargetState: "null", NormalizedLiveState: manifest(t, cm("extra", "default", "v"))},
		{Kind: "ConfigMap", Namespace: "default", Name: "ignored", TargetState: manifest(t, cm("ignored", "default", "a")), NormalizedLiveState: manifest(t, cm("ignored", "default", "b"))},
		{Kind: "ConfigMap", Namespace: "default", Name: "hook", TargetState: manifest(t, hook), NormalizedLiveState: "null"},
	}
	e := defaultTestEnv()
	changed, err := e.computeDiff(context.Background(), nil, &diffRequest{app: app, managed: managed, settings: &apiclient.Settings{TrackingMethod: "label"}, concurrency: -1, maxBatchKB: 250})
	require.NoError(t, err)
	var names []string
	for _, c := range changed {
		names = append(names, c.key.Name)
	}
	// Sorted by key; equal, ignored and hook resources are left out; resources missing on
	// one side are reported (upstream compareManifests).
	assert.Equal(t, []string{"changed", "extra", "missing"}, names)
	assert.Nil(t, changed[1].target, "extraneous resource: would be pruned")
	assert.Nil(t, changed[2].live, "missing resource: would be created")
	require.NotNil(t, changed[0].target)
	assert.Equal(t, map[string]any{"k": "new"}, changed[0].target.Object["data"])
}

func TestComputeDiffLocalTargetsNormalization(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"},
		Spec:       v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Namespace: "default"}},
	}
	live := cm("cfg", "default", "v")
	live["metadata"].(map[string]any)["labels"] = map[string]any{"app.kubernetes.io/instance": "guestbook"}
	managed := []apiclient.ResourceDiff{
		{Kind: "ConfigMap", Namespace: "default", Name: "cfg", NormalizedLiveState: manifest(t, live)},
		{Kind: "Secret", Namespace: "default", Name: "s", NormalizedLiveState: manifest(t, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "default"}, "data": map[string]any{"a": "KysrKw=="}})},
	}
	// Local manifests without namespace (namespaced kind known from the live state) and a
	// duplicate; Secrets are excluded on both sides.
	noNS := cm("cfg", "", "v")
	targets, err := manifestsToObjects([]string{
		manifest(t, cm("cfg", "", "old")), manifest(t, noNS),
		manifest(t, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"}, "stringData": map[string]any{"a": "plain"}}),
	})
	require.NoError(t, err)
	var warn bytes.Buffer
	e := defaultTestEnv()
	changed, err := e.computeDiff(context.Background(), nil, &diffRequest{
		app: app, managed: managed, settings: &apiclient.Settings{TrackingMethod: "label", ControllerNamespace: "patrol"}, targets: targets, excludeSecrets: true,
		concurrency: -1, maxBatchKB: 250, warn: &warn,
	})
	require.NoError(t, err)
	assert.Empty(t, changed, "namespace defaulted, tracking label applied, last duplicate wins")
	assert.Contains(t, warn.String(), "RepeatedResourceWarning: Resource /ConfigMap/default/cfg appeared 2 times")
	assert.Contains(t, warn.String(), "Secret default/s is not supported in local diff")
	assert.NotContains(t, warn.String(), "plain")
}

func TestComputeDiffServerSideBatches(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "patrol"}, Spec: v1alpha1.ApplicationSpec{Project: "p"}}
	var managed []apiclient.ResourceDiff
	for _, n := range []string{"a", "b", "c", "d"} {
		managed = append(managed, apiclient.ResourceDiff{Kind: "ConfigMap", Namespace: "ns", Name: n,
			TargetState: manifest(t, cm(n, "ns", strings.Repeat("x", 600))), NormalizedLiveState: manifest(t, cm(n, "ns", "y"))})
	}
	fc := newFakeClient()
	var batches []int
	fc.ssd = func(req *apiclient.ApplicationServerSideDiffRequest) (*apiclient.ServerSideDiffResponse, error) {
		assert.Equal(t, "p", req.Project)
		require.Len(t, req.TargetManifests, len(req.LiveResources))
		batches = append(batches, len(req.LiveResources))
		var out apiclient.ServerSideDiffResponse
		for i, l := range req.LiveResources {
			out.Items = append(out.Items, apiclient.ResourceDiff{Kind: l.Kind, Name: l.Name, Modified: true, LiveState: l.LiveState, TargetState: req.TargetManifests[i]})
		}
		return &out, nil
	}
	e := defaultTestEnv()
	// 1 KB batches: every item (~700 bytes) gets its own batch; sequential for a stable order.
	changed, err := e.computeDiff(context.Background(), fc, &diffRequest{app: app, managed: managed, serverSide: true, concurrency: 1, maxBatchKB: 1})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 1, 1, 1}, batches)
	assert.Len(t, changed, 4)

	batches = nil
	_, err = e.computeDiff(context.Background(), fc, &diffRequest{app: app, managed: managed, serverSide: true, concurrency: -1, maxBatchKB: 250})
	require.NoError(t, err)
	assert.Equal(t, []int{4}, batches)
}

func TestDiffCommandExitCodes(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"}}
	same := []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: "d", Name: "a", TargetState: manifest(t, cm("a", "d", "v")), NormalizedLiveState: manifest(t, cm("a", "d", "v"))}}
	changed := []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: "d", Name: "a", TargetState: manifest(t, cm("a", "d", "new")), NormalizedLiveState: manifest(t, cm("a", "d", "old"))}}

	fc := newFakeClient(app)
	fc.managed = same
	out, _, err := runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook")
	require.NoError(t, err)
	assert.Empty(t, out)

	fc.managed = changed
	out, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook")
	requireExitCode(t, err, 1)
	assert.Contains(t, out, "===== /ConfigMap d/a ======")
	assert.Contains(t, out, "3c3\n<   k: old\n---\n>   k: new\n")

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--diff-exit-code", "5")
	requireExitCode(t, err, 5)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--exit-code=false")
	require.NoError(t, err)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "missing")
	requireExitCode(t, err, 2)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff")
	requireExitCode(t, err, 2)

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--source-names", "a", "--source-positions", "1", "--revisions", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Only one of source-positions and source-names can be specified.")

	_, _, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--server-side-diff-concurrency", "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0 is not allowed")

	_, stderr, err := runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--server-side-diff", "--local", "dir")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--server-side-diff with --local requires --server-side-generate.")
	assert.Contains(t, stderr, "Warning: Application does not have ServerSideDiff=true annotation.")
}

func TestDiffCommandRevisionExcludesSecrets(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"},
		Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Namespace: "d"}}}
	fc := newFakeClient(app)
	fc.managed = []apiclient.ResourceDiff{
		{Kind: "ConfigMap", Namespace: "d", Name: "a", NormalizedLiveState: manifest(t, cm("a", "d", "v"))},
		{Kind: "Secret", Namespace: "d", Name: "s", NormalizedLiveState: manifest(t, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "d"}, "data": map[string]any{"k": "KysrKw=="}})},
	}
	fc.manifests = []string{manifest(t, cm("a", "d", "v2")), manifest(t, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "d"}, "data": map[string]any{"k": "KysrKysrKys="}})}
	out, _, err := runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--revision", "v2")
	requireExitCode(t, err, 1)
	assert.Contains(t, out, "/ConfigMap d/a")
	assert.NotContains(t, out, "Secret")
	require.Len(t, fc.manifestQs, 1)
	assert.Equal(t, "v2", fc.manifestQs[0].Revision)
}

// TestDiffOutputSanitizesRenderedNames checks that names, kinds and namespaces of rendered
// (not yet validated) resources cannot inject terminal escapes into the diff header or the
// warnings, and that bidirectional overrides in the YAML body are neutralized.
func TestDiffOutputSanitizesRenderedNames(t *testing.T) {
	t.Parallel()
	const (
		evilName = "x\x1b]0;pwned\x07\x1b[2J"
		evilNS   = "d\x1b[1A"
		evilKind = "Config\x1b[31mMap"
	)
	evil := func(name string) map[string]any {
		obj := cm(name, evilNS, "\u202eevil")
		obj["kind"] = evilKind
		return obj
	}
	unsafe := []string{"\x1b", "\x07", "\u202e"}
	assertClean := func(t *testing.T, label, s string) {
		t.Helper()
		for _, u := range unsafe {
			assert.NotContains(t, s, u, "%s contains %q", label, u)
		}
	}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"},
		Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Namespace: "d"}}}

	// A resource that exists only in the target state (managed-resources path).
	fc := newFakeClient(app)
	fc.managed = []apiclient.ResourceDiff{{Kind: evilKind, Namespace: evilNS, Name: evilName, TargetState: manifest(t, evil(evilName)), NormalizedLiveState: "null"}}
	out, errOut, err := runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook")
	requireExitCode(t, err, 1)
	assert.Contains(t, out, "===== /Config [31mMap d [1A/x ]0;pwned  [2J ======")
	assertClean(t, "stdout", out)
	assertClean(t, "stderr", errOut)

	// sync --preview-changes shows the same diff before asking for confirmation.
	out, errOut, err = runCmd(t, testEnvWith(fc), "n\n", "app", "sync", "guestbook", "--preview-changes")
	require.NoError(t, err)
	assert.Contains(t, out, "===== /Config [31mMap d [1A/x ]0;pwned  [2J ======")
	assert.Contains(t, errOut, "Do you want to continue the sync process?")
	assertClean(t, "stdout", out)
	assertClean(t, "stderr", errOut)

	// Revision targets: a duplicate (RepeatedResourceWarning) and a Secret (excluded with a
	// warning), both with control characters in their names.
	fc = newFakeClient(app)
	fc.manifests = []string{
		manifest(t, evil(evilName)), manifest(t, evil(evilName)),
		manifest(t, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s\x1b[2J", "namespace": evilNS}}),
	}
	out, errOut, err = runCmd(t, testEnvWith(fc), "", "app", "diff", "guestbook", "--revision", "v2")
	requireExitCode(t, err, 1)
	assert.Contains(t, errOut, "Resource /Config [31mMap/d [1A/x ]0;pwned  [2J appeared 2 times among application resources.")
	assert.Contains(t, errOut, "Warning: Secret d [1A/s [2J is not supported in local diff and will be ignored")
	assertClean(t, "stdout", out)
	assertClean(t, "stderr", errOut)
}
