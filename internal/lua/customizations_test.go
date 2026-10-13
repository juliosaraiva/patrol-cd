package lua

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// customizationsDir is the on-disk copy of the customizations, including test inputs that
// are not embedded.
const customizationsDir = "customizations"

func loadObj(t *testing.T, fsys fs.FS, p string) *unstructured.Unstructured {
	t.Helper()
	data, err := fs.ReadFile(fsys, p)
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &obj))
	return &unstructured.Unstructured{Object: obj}
}

// TestEmbeddedScriptsMatchDisk guarantees the embedded tree carries every runtime script
// on disk, byte for byte.
func TestEmbeddedScriptsMatchDisk(t *testing.T) {
	t.Parallel()
	disk := os.DirFS(customizationsDir)
	var onDisk []string
	require.NoError(t, fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch path.Base(p) {
		case healthScriptFile, discoveryScriptFile:
		case actionScriptFile:
			if path.Base(path.Dir(path.Dir(p))) != "actions" {
				return nil
			}
		default:
			return nil
		}
		onDisk = append(onDisk, p)
		return nil
	}))
	embedded := defaultBuiltins().fsys
	var inEmbed []string
	require.NoError(t, fs.WalkDir(embedded, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		inEmbed = append(inEmbed, p)
		return nil
	}))
	slices.Sort(onDisk)
	slices.Sort(inEmbed)
	require.Equal(t, onDisk, inEmbed)
	for _, p := range onDisk {
		a, err := fs.ReadFile(disk, p)
		require.NoError(t, err)
		b, err := fs.ReadFile(embedded, p)
		require.NoError(t, err)
		require.Equal(t, a, b, p)
	}
	t.Logf("embedded scripts: %d", len(inEmbed))
}

type healthTestFile struct {
	Tests []struct {
		InputPath    string              `json:"inputPath"`
		HealthStatus health.HealthStatus `json:"healthStatus"`
	} `json:"tests"`
}

// TestBuiltinHealthScripts runs every case of every health_test.yaml.
func TestBuiltinHealthScripts(t *testing.T) {
	t.Parallel()
	disk := os.DirFS(customizationsDir)
	rt := newTestRuntime(nil)
	var cases, files atomic.Int64
	t.Cleanup(func() { t.Logf("health_test.yaml files: %d, cases: %d", files.Load(), cases.Load()) })
	require.NoError(t, fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) != healthScriptFile {
			return err
		}
		dir := path.Dir(p)
		data, err := fs.ReadFile(disk, path.Join(dir, "health_test.yaml"))
		require.NoError(t, err, "every health.lua needs a health_test.yaml (%s)", dir)
		var tf healthTestFile
		require.NoError(t, yaml.Unmarshal(data, &tf))
		require.NotEmpty(t, tf.Tests, dir)
		files.Add(1)
		for _, tc := range tf.Tests {
			cases.Add(1)
			t.Run(path.Join(dir, tc.InputPath), func(t *testing.T) {
				t.Parallel()
				obj := loadObj(t, disk, path.Join(dir, tc.InputPath))
				hs, err := rt.HealthScript(obj)
				require.NoError(t, err)
				require.NotNil(t, hs, "no health script resolved")
				got, err := rt.ExecuteHealth(context.Background(), obj, hs.Script, hs.UseOpenLibs)
				require.NoError(t, err)
				assert.Equal(t, &tc.HealthStatus, got)
				// The health.Overrides entry point must agree.
				viaOverrides, err := health.Assess(obj, rt)
				require.NoError(t, err)
				if obj.GetDeletionTimestamp() == nil {
					assert.Equal(t, got, viaOverrides)
				}
			})
		}
		return nil
	}))
}

type actionTestFile struct {
	DiscoveryTests []struct {
		InputPath string   `json:"inputPath"`
		Result    []Action `json:"result"`
	} `json:"discoveryTests"`
	ActionTests []struct {
		Action               string            `json:"action"`
		InputPath            string            `json:"inputPath"`
		ExpectedOutputPath   string            `json:"expectedOutputPath"`
		ExpectedErrorMessage string            `json:"expectedErrorMessage"`
		Parameters           map[string]string `json:"parameters"`
	} `json:"actionTests"`
}

// TestBuiltinActionScripts runs every discovery and action case of every
// action_test.yaml with upstream's assertions: each discovered action is among the
// expected ones, and each impacted resource matches its expected object.
func TestBuiltinActionScripts(t *testing.T) {
	t.Parallel()
	disk := os.DirFS(customizationsDir)
	rt := newTestRuntime(nil)
	var discovery, actions, files atomic.Int64
	t.Cleanup(func() {
		t.Logf("action_test.yaml files: %d, discovery cases: %d, action cases: %d", files.Load(), discovery.Load(), actions.Load())
	})
	require.NoError(t, fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) != "action_test.yaml" {
			return err
		}
		dir := path.Dir(p)
		data, err := fs.ReadFile(disk, p)
		require.NoError(t, err)
		var tf actionTestFile
		require.NoError(t, yaml.Unmarshal(data, &tf))
		files.Add(1)
		for _, tc := range tf.DiscoveryTests {
			discovery.Add(1)
			t.Run(path.Join(dir, "discovery", tc.InputPath), func(t *testing.T) {
				t.Parallel()
				obj := loadObj(t, disk, path.Join(dir, tc.InputPath))
				got, err := rt.ListActions(context.Background(), obj)
				require.NoError(t, err)
				// Upstream asserts containment only; Patrol also requires the same count.
				assert.Len(t, got, len(tc.Result))
				for _, a := range got {
					assert.Contains(t, tc.Result, a)
				}
			})
		}
		for _, tc := range tf.ActionTests {
			actions.Add(1)
			t.Run(path.Join(dir, "action", tc.Action, tc.InputPath), func(t *testing.T) {
				t.Parallel()
				src := loadObj(t, disk, path.Join(dir, tc.InputPath))
				var params []ActionParameter
				for _, k := range slices.Sorted(maps.Keys(tc.Parameters)) {
					params = append(params, ActionParameter{Name: k, Value: tc.Parameters[k]})
				}
				impacted, err := rt.ExecuteAction(context.Background(), src, tc.Action, params)
				if tc.ExpectedErrorMessage != "" {
					require.EqualError(t, err, tc.ExpectedErrorMessage)
					return
				}
				require.NoError(t, err)
				expected := loadExpectedObjects(t, disk, path.Join(dir, tc.ExpectedOutputPath))
				for _, ir := range impacted {
					got := ir.Object
					want := findExpected(expected, src, got)
					require.NotNil(t, want, "no expected object for %s %s/%s", got.GroupVersionKind(), got.GetNamespace(), got.GetName())
					switch ir.Operation {
					case OperationPatch:
						assert.Equal(t, src.GroupVersionKind(), got.GroupVersionKind())
						assert.Equal(t, src.GetName(), got.GetName())
						assert.Equal(t, src.GetNamespace(), got.GetNamespace())
					case OperationCreate:
						if k := got.GetKind(); k == "Job" || k == "Workflow" {
							got.SetName(want.GetName()) // generated names
						}
					}
					wantN, gotN := normalizeForCompare(t, want), normalizeForCompare(t, got)
					if diffs := subsetDiff("", wantN.Object, gotN.Object); len(diffs) > 0 {
						t.Errorf("output does not match %s:\n%s", tc.ExpectedOutputPath, strings.Join(diffs, "\n"))
					}
				}
			})
		}
		return nil
	}))
}

// TestApplicationToggleAutoSync runs argoproj.io/Application/actions/actions_test.yaml,
// which uses a given/when/expect format that upstream's test runner does not pick up.
func TestApplicationToggleAutoSync(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(path.Join(customizationsDir, "argoproj.io/Application/actions/actions_test.yaml"))
	require.NoError(t, err)
	var tf struct {
		Tests []struct {
			Given map[string]any `json:"given"`
			When  struct {
				Action string `json:"action"`
			} `json:"when"`
			Expect map[string]any `json:"expect"`
		} `json:"tests"`
	}
	require.NoError(t, yaml.Unmarshal(data, &tf))
	require.NotEmpty(t, tf.Tests)
	rt := newTestRuntime(nil)
	for i, tc := range tf.Tests {
		t.Run(fmt.Sprintf("scenario-%d", i+1), func(t *testing.T) {
			t.Parallel()
			src := &unstructured.Unstructured{Object: tc.Given}
			impacted, err := rt.ExecuteAction(context.Background(), src, tc.When.Action, nil)
			require.NoError(t, err)
			require.Len(t, impacted, 1)
			if diffs := subsetDiff("", tc.Expect, impacted[0].Object.Object); len(diffs) > 0 {
				t.Errorf("unexpected result:\n%s", strings.Join(diffs, "\n"))
			}
		})
	}
	t.Logf("Application actions_test.yaml cases: %d", len(tf.Tests))
}

func loadExpectedObjects(t *testing.T, fsys fs.FS, p string) []*unstructured.Unstructured {
	t.Helper()
	data, err := fs.ReadFile(fsys, p)
	require.NoError(t, err)
	if strings.HasPrefix(string(data), "-") {
		var list []map[string]any
		require.NoError(t, yaml.Unmarshal(data, &list))
		out := make([]*unstructured.Unstructured, 0, len(list))
		for _, item := range list {
			obj, ok := item["unstructuredObj"].(map[string]any)
			require.True(t, ok, "wrong type of unstructuredObj in %s", p)
			out = append(out, &unstructured.Unstructured{Object: obj})
		}
		return out
	}
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &obj))
	return []*unstructured.Unstructured{{Object: obj}}
}

// findExpected mirrors upstream's matching: same GVK, namespace and name, except that
// generated Jobs (from CronJobs) and Workflows (from CronWorkflows/WorkflowTemplates) match
// by name prefix.
func findExpected(expected []*unstructured.Unstructured, src, got *unstructured.Unstructured) *unstructured.Unstructured {
	for _, e := range expected {
		if e.GroupVersionKind() != got.GroupVersionKind() || e.GetNamespace() != got.GetNamespace() {
			continue
		}
		generated := (got.GetKind() == "Job" && src.GetKind() == "CronJob") ||
			(got.GetKind() == "Workflow" && (src.GetKind() == "CronWorkflow" || src.GetKind() == "WorkflowTemplate"))
		if generated && strings.HasPrefix(e.GetName(), src.GetName()) {
			return e
		}
		if !generated && e.GetName() == got.GetName() {
			return e
		}
	}
	return nil
}

const zeroTime = "0001-01-01T00:00:00Z"

// normalizeForCompare blanks time-dependent fields exactly where upstream's test
// normalizer does, and drops metadata.creationTimestamp as the upstream diff does.
func normalizeForCompare(t *testing.T, in *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	u := in.DeepCopy()
	set := func(v any, fields ...string) {
		require.NoError(t, unstructured.SetNestedField(u.Object, v, fields...))
	}
	setMap := func(m map[string]string, fields ...string) {
		require.NoError(t, unstructured.SetNestedStringMap(u.Object, m, fields...))
	}
	flux := map[string]string{"reconcile.fluxcd.io/requestedAt": "By Argo CD at: 0001-01-01T00:00:00"}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	group := u.GroupVersionKind().Group
	kind := u.GetKind()
	switch group {
	case "batch":
		if kind == "Job" {
			conds, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			if ok {
				for _, c := range conds {
					if m, ok := c.(map[string]any); ok && m["type"] == "FailureTarget" {
						m["lastTransitionTime"] = zeroTime
					}
				}
				require.NoError(t, unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"))
			}
		}
	case "apps":
		switch kind {
		case "DaemonSet", "Deployment", "StatefulSet":
			setMap(map[string]string{"kubectl.kubernetes.io/restartedAt": zeroTime}, "spec", "template", "metadata", "annotations")
		}
		if kind == "Deployment" {
			set(nil, "status")
			set(nil, "metadata", "creationTimestamp")
			set(nil, "metadata", "generation")
		}
	case "argoproj.io":
		switch kind {
		case "Rollout":
			set(nil, "spec", "restartAt")
		case "Workflow":
			set(nil, "metadata", "resourceVersion")
			set(nil, "metadata", "uid")
			set(nil, "metadata", "annotations", "workflows.argoproj.io/scheduled-time")
		}
	case "external-secrets.io":
		if kind == "ExternalSecret" || kind == "PushSecret" {
			setMap(map[string]string{"force-sync": zeroTime}, "metadata", "annotations")
		}
	case "postgresql.cnpg.io":
		if kind == "Cluster" {
			ann, _, _ := unstructured.NestedStringMap(u.Object, "metadata", "annotations")
			if ann == nil {
				ann = map[string]string{}
			}
			ann["cnpg.io/reloadedAt"] = zeroTime
			ann["kubectl.kubernetes.io/restartedAt"] = zeroTime
			setMap(ann, "metadata", "annotations")
			set(nil, "status", "targetPrimaryTimestamp")
		}
	case "helm.toolkit.fluxcd.io":
		if kind == "HelmRelease" {
			setMap(flux, "metadata", "annotations")
		}
	case "source.toolkit.fluxcd.io":
		switch kind {
		case "Bucket", "GitRepository", "HelmChart", "HelmRepository", "OCIRepository":
			setMap(flux, "metadata", "annotations")
		}
	case "image.toolkit.fluxcd.io":
		if kind == "ImageRepository" || kind == "ImageUpdateAutomation" {
			setMap(flux, "metadata", "annotations")
		}
	case "kustomize.toolkit.fluxcd.io":
		if kind == "Kustomization" {
			setMap(flux, "metadata", "annotations")
		}
	case "notification.toolkit.fluxcd.io":
		if kind == "Receiver" {
			setMap(flux, "metadata", "annotations")
		}
	}
	return u
}

// subsetDiff reports where want is not contained in got: every map key of want must be in
// got with an equal value (extra keys in got are allowed, like upstream's two-way diff
// that only applies the expected object onto the result); lists must have equal length
// and contained elements; numbers compare numerically; a nil or empty map/list in want
// also matches an absent value (typed defaulting in upstream's diff produces those).
func subsetDiff(at string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			if len(w) == 0 && isAbsent(got) {
				return nil
			}
			return []string{fmt.Sprintf("%s: want object %v, got %v", at, w, got)}
		}
		var out []string
		for _, k := range slices.Sorted(maps.Keys(w)) {
			gv, present := g[k]
			if !present {
				if isEmptyContainerOrNil(w[k]) {
					continue
				}
				out = append(out, fmt.Sprintf("%s.%s: missing (want %v)", at, k, w[k]))
				continue
			}
			out = append(out, subsetDiff(at+"."+k, w[k], gv)...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok {
			if len(w) == 0 && isAbsent(got) {
				return nil
			}
			return []string{fmt.Sprintf("%s: want list %v, got %v", at, w, got)}
		}
		if len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %d items, got %d (%v vs %v)", at, len(w), len(g), w, g)}
		}
		var out []string
		for i := range w {
			out = append(out, subsetDiff(fmt.Sprintf("%s[%d]", at, i), w[i], g[i])...)
		}
		return out
	case nil:
		if isAbsent(got) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want null, got %v", at, got)}
	}
	if wf, ok := number(want); ok {
		if gf, ok := number(got); ok && wf == gf {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %v, got %v", at, want, got)}
	}
	if !reflect.DeepEqual(want, got) {
		return []string{fmt.Sprintf("%s: want %#v, got %#v", at, want, got)}
	}
	return nil
}

func isAbsent(v any) bool { return v == nil }

func isEmptyContainerOrNil(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
