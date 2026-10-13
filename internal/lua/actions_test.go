package lua

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const cronJobYAML = `
apiVersion: batch/v1
kind: CronJob
metadata:
  name: hello
  namespace: test-ns
`

const validDiscoveryLua = `
scaleParams = { {name = "replicas", type = "number"} }
scale = {name = 'scale', params = scaleParams}

resume = {name = 'resume'}

a = {scale = scale, resume = resume}

return a
`

const additionalDiscoveryLua = `
scaleParams = { {name = "override", type = "number"} }
scale = {name = 'scale', params = scaleParams}
prebuilt = {prebuilt = 'prebuilt', type = 'number'}

a = {scale = scale, prebuilt = prebuilt}

return a
`

func impactedFromYAML(t *testing.T, doc string) []ImpactedResource {
	t.Helper()
	var entries []struct {
		Operation Operation      `json:"operation"`
		Resource  map[string]any `json:"resource"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &entries))
	out := make([]ImpactedResource, 0, len(entries))
	for _, e := range entries {
		out = append(out, ImpactedResource{Object: &unstructured.Unstructured{Object: e.Resource}, Operation: e.Operation})
	}
	return out
}

func TestExecuteActionScriptOldStyle(t *testing.T) {
	t.Parallel()
	got, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), obj(t, rolloutYAML), `
obj.metadata.labels["test"] = "test"
return obj
`, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, OperationPatch, got[0].Operation)
	assert.Equal(t, obj(t, `
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  labels:
    app.kubernetes.io/instance: helm-guestbook
    test: test
  name: helm-guestbook
  namespace: default
  resourceVersion: "123"
`), got[0].Object)
}

func TestExecuteActionScriptNewStyle(t *testing.T) {
	t.Parallel()
	const createJob = `
job = {}
job.apiVersion = "batch/v1"
job.kind = "Job"
job.metadata = {}
job.metadata.name = "hello-1"
job.metadata.namespace = "test-ns"
return {{operation = "create", resource = job}}
`
	const createTwo = `
local function job(n)
  return {apiVersion = "batch/v1", kind = "Job", metadata = {name = "hello-" .. n, namespace = "test-ns"}}
end
return {{operation = "create", resource = job(1)}, {operation = "create", resource = job(2)}}
`
	const mixed = `
local job1 = {apiVersion = "batch/v1", kind = "Job", metadata = {name = "hello-1", namespace = obj.metadata.namespace}}
obj.metadata.labels = {}
obj.metadata.labels["test"] = "test"
return {{operation = "create", resource = job1}, {operation = "patch", resource = obj}}
`
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{"create single", createJob, `
- operation: create
  resource: {apiVersion: batch/v1, kind: Job, metadata: {name: hello-1, namespace: test-ns}}`},
		{"create multiple", createTwo, `
- operation: create
  resource: {apiVersion: batch/v1, kind: Job, metadata: {name: hello-1, namespace: test-ns}}
- operation: create
  resource: {apiVersion: batch/v1, kind: Job, metadata: {name: hello-2, namespace: test-ns}}`},
		{"mixed create and patch", mixed, `
- operation: create
  resource: {apiVersion: batch/v1, kind: Job, metadata: {name: hello-1, namespace: test-ns}}
- operation: patch
  resource: {apiVersion: batch/v1, kind: CronJob, metadata: {name: hello, namespace: test-ns, labels: {test: test}}}`},
		{"empty list", `return {}`, `[]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), obj(t, cronJobYAML), tc.script, nil)
			require.NoError(t, err)
			assert.Equal(t, impactedFromYAML(t, tc.want), got)
		})
	}
}

func TestExecuteActionScriptErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		script  string
		wantErr string
		is      error
	}{
		{"unsupported operation", `return {{operation = "thisShouldFail", resource = obj}}`, "unsupported operation", nil},
		{"non-table", `return 1`, "expect table output from Lua script, not number", ErrInvalidOutput},
		{"nil", `return nil`, "expect table output from Lua script, not nil", ErrInvalidOutput},
		{"object without kind", `newObj = {} newObj["test"] = "test" return newObj`, "Object 'Kind' is missing", nil},
		{"entry without resource", `return {{operation = "create"}}`, "impacted resource 0 has no resource", ErrInvalidOutput},
		{"entry without operation", `return {{resource = obj}}`, "impacted resource 0 has no operation", ErrInvalidOutput},
		{"patch of another object", `obj.metadata.name = "other" return obj`, "differs from action source", ErrInvalidOutput},
		{"patch of another kind via list", `return {{operation = "patch", resource = {apiVersion = "v1", kind = "ConfigMap", metadata = {name = "hello", namespace = "test-ns"}}}}`, "differs from action source", ErrInvalidOutput},
		{"script error", `error("invalid number: x", 0)`, "invalid number: x", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), obj(t, cronJobYAML), tc.script, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
		})
	}
	_, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), nil, "return obj", nil)
	require.ErrorIs(t, err, ErrInvalidOutput)
}

func TestCleanPatch(t *testing.T) {
	t.Parallel()
	const base = `
apiVersion: argoproj.io/v1alpha1
kind: Test
metadata:
  name: helm-guestbook
  namespace: default
  resourceVersion: "123"
`
	tests := []struct {
		name, in, action, want string
	}{
		{
			name: "empty struct preserved",
			in: base + `spec:
  resources: {}
  updated:
    something: true
  containers:
   - name: name1
     test: {}
     anotherList:
     - name: name2
       test2: {}
`,
			action: "obj.spec.updated = {}\nreturn obj",
			want: base + `spec:
  resources: {}
  updated: {}
  containers:
   - name: name1
     test: {}
     anotherList:
     - name: name2
       test2: {}
`,
		},
		{
			name: "new item added to array",
			in: base + `spec:
  containers:
   - name: name1
     test: {}
`,
			action: `table.insert(obj.spec.containers, {name = "added", testArray = {}, another = {supported = true}})
return obj`,
			want: base + `spec:
  containers:
   - name: name1
     test: {}
   - name: added
     testArray: []
     another:
       supported: true
`,
		},
		{
			name: "last item removed from array",
			in: base + `spec:
  containers:
   - name: name1
     test: {}
   - name: name3
     test: {}
`,
			action: "table.remove(obj.spec.containers)\nreturn obj",
			want: base + `spec:
  containers:
   - name: name1
     test: {}
`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), obj(t, tc.in), tc.action, nil)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, obj(t, tc.want), got[0].Object)
		})
	}
}

func TestExecuteActionWithParams(t *testing.T) {
	t.Parallel()
	const action = `obj.spec.replicas = tonumber(actionParams["replicas"])
return obj`
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			src := obj(t, fmt.Sprintf("apiVersion: apps/v1\nkind: %s\nmetadata: {name: x, namespace: default}\nspec: {replicas: 1}", kind))
			got, err := newTestRuntime(nil).ExecuteActionScript(context.Background(), src, action, []ActionParameter{{Name: "replicas", Value: "3"}})
			require.NoError(t, err)
			require.Len(t, got, 1)
			replicas, found, err := unstructured.NestedInt64(got[0].Object.Object, "spec", "replicas")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, int64(3), replicas)
		})
	}
	// Built-in scale action rejects non-numbers with the upstream message.
	src := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: x, namespace: default}\nspec: {replicas: 1}")
	_, err := newTestRuntime(nil).ExecuteAction(context.Background(), src, "scale", []ActionParameter{{Name: "replicas", Value: "nope"}})
	require.EqualError(t, err, "invalid number: nope")
}

func TestActionScriptLookup(t *testing.T) {
	t.Parallel()
	rollout := obj(t, rolloutYAML)

	def, err := newTestRuntime(nil).ActionScript(rollout, "resume")
	require.NoError(t, err)
	assert.Equal(t, "resume", def.Name)
	assert.NotEmpty(t, def.ActionLua)

	_, err = newTestRuntime(nil).ActionScript(obj(t, noScriptYAML), "test")
	require.ErrorIs(t, err, ErrActionNotFound)

	for _, name := range []string{"../resume", "resume/..", "..", "", "a/b", `a\b`} {
		_, err = newTestRuntime(nil).ActionScript(rollout, name)
		require.ErrorIs(t, err, ErrActionNotFound, name)
	}
	// Path traversal into another kind's actions is rejected.
	_, err = newTestRuntime(nil).ActionScript(obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: x}"), "../../../batch/CronJob/actions/create-job")
	require.ErrorIs(t, err, ErrActionNotFound)

	custom := newTestRuntime(map[string]ResourceOverride{"argoproj.io/Rollout": {Actions: `
definitions:
- name: test
  action.lua: return obj
`}})
	def, err = custom.ActionScript(rollout, "test")
	require.NoError(t, err)
	assert.Equal(t, ActionDefinition{Name: "test", ActionLua: "return obj"}, def)
	// Built-ins remain reachable by name even when custom actions are configured.
	_, err = custom.ActionScript(rollout, "resume")
	require.NoError(t, err)

	_, err = newTestRuntime(map[string]ResourceOverride{"argoproj.io/Rollout": {Actions: "definitions: {"}}).ActionScript(rollout, "test")
	require.ErrorIs(t, err, ErrInvalidCustomization)
}

func TestDiscoveryScripts(t *testing.T) {
	t.Parallel()
	rollout := obj(t, rolloutYAML)

	scripts, err := newTestRuntime(nil).DiscoveryScripts(rollout)
	require.NoError(t, err)
	require.Len(t, scripts, 1)

	scripts, err = newTestRuntime(nil).DiscoveryScripts(obj(t, noScriptYAML))
	require.NoError(t, err)
	assert.Empty(t, scripts)

	replace := newTestRuntime(map[string]ResourceOverride{"argoproj.io/Rollout": {Actions: "discovery.lua: |\n" + indent(validDiscoveryLua)}})
	scripts, err = replace.DiscoveryScripts(rollout)
	require.NoError(t, err)
	require.Len(t, scripts, 1)
	assert.Equal(t, validDiscoveryLua[1:], scripts[0])

	merge := newTestRuntime(map[string]ResourceOverride{"argoproj.io/Rollout": {Actions: "mergeBuiltinActions: true\ndiscovery.lua: |\n" + indent(validDiscoveryLua)}})
	scripts, err = merge.DiscoveryScripts(rollout)
	require.NoError(t, err)
	require.Len(t, scripts, 2)
	assert.Equal(t, validDiscoveryLua[1:], scripts[0])
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "  " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines[1:] // drop the leading empty line of the raw string constants
}

func TestExecuteDiscovery(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)
	rollout := obj(t, rolloutYAML)

	got, err := rt.ExecuteDiscovery(context.Background(), rollout, []string{validDiscoveryLua})
	require.NoError(t, err)
	assert.Equal(t, []Action{
		{Name: "resume"},
		{Name: "scale", Params: []ActionParam{{Name: "replicas"}}},
	}, got)

	got, err = rt.ExecuteDiscovery(context.Background(), rollout, []string{validDiscoveryLua, additionalDiscoveryLua})
	require.NoError(t, err)
	assert.Equal(t, []Action{
		{Name: "prebuilt"},
		{Name: "resume"},
		{Name: "scale", Params: []ActionParam{{Name: "replicas"}}}, // first script wins
	}, got)

	got, err = rt.ExecuteDiscovery(context.Background(), rollout, []string{`return {a = {}, b = {disabled = true, iconClass = "x", displayName = "B"}}`})
	require.NoError(t, err)
	assert.Equal(t, []Action{{Name: "a"}, {Name: "b", Disabled: true, IconClass: "x", DisplayName: "B"}}, got)

	got, err = rt.ExecuteDiscovery(context.Background(), rollout, []string{`return {}`})
	require.NoError(t, err)
	assert.Empty(t, got)

	for name, script := range map[string]string{
		"syntax":         "resume = {name = 'resume', invalidField: \"test\"\"}\nreturn {resume = resume}",
		"number":         "a = 1\nreturn a",
		"array":          "return {1, 2}",
		"string entry":   `return {a = "x"}`,
		"disabled typed": `return {a = {disabled = "yes"}}`,
	} {
		_, err := rt.ExecuteDiscovery(context.Background(), rollout, []string{script})
		require.Error(t, err, name)
	}
	_, err = rt.ExecuteDiscovery(context.Background(), rollout, nil)
	require.ErrorIs(t, err, ErrNoDiscovery)

	list, err := rt.ListActions(context.Background(), obj(t, noScriptYAML))
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.NotNil(t, list)
}

func TestBuiltinActionsForCoreKinds(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)
	tests := []struct {
		doc  string
		want []string
	}{
		{"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d, namespace: n}\nspec: {}", []string{"pause", "restart", "resume", "scale"}},
		{"apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: s, namespace: n}\nspec: {}", []string{"restart", "scale"}},
		{"apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: d, namespace: n}\nspec: {}", []string{"restart"}},
		{"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: c, namespace: n}\nspec: {}", []string{"create-job", "suspend"}},
	}
	for _, tc := range tests {
		o := obj(t, tc.doc)
		t.Run(o.GetKind(), func(t *testing.T) {
			t.Parallel()
			got, err := rt.ListActions(context.Background(), o)
			require.NoError(t, err)
			names := make([]string, 0, len(got))
			for _, a := range got {
				names = append(names, a.Name)
			}
			assert.Equal(t, tc.want, names)
		})
	}

	// restart patches the pod template with a restartedAt annotation.
	dep := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d, namespace: n}\nspec: {template: {spec: {}}}")
	out, err := rt.ExecuteAction(context.Background(), dep, "restart", nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	ts, found, err := unstructured.NestedString(out[0].Object.Object, "spec", "template", "metadata", "annotations", "kubectl.kubernetes.io/restartedAt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, ts)

	// create-job creates a Job owned by the CronJob.
	cj := obj(t, "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: hello, namespace: n, uid: u1}\nspec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}}}")
	out, err = rt.ExecuteAction(context.Background(), cj, "create-job", nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, OperationCreate, out[0].Operation)
	assert.Equal(t, "Job", out[0].Object.GetKind())
	assert.Regexp(t, `^hello-\d{10}$`, out[0].Object.GetName())
	require.Len(t, out[0].Object.GetOwnerReferences(), 1)
	assert.Equal(t, "u1", string(out[0].Object.GetOwnerReferences()[0].UID))
}

// Patrol's own Application and ApplicationSet reuse the upstream Argo CD built-ins.
func TestPatrolKindAliases(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)

	app := obj(t, `apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: child, namespace: patrol}
spec:
  syncPolicy:
    automated: {prune: true}
status:
  health: {status: Degraded}
`)
	actions, err := rt.ListActions(context.Background(), app)
	require.NoError(t, err)
	assert.Equal(t, []Action{{Name: "toggle-auto-sync", DisplayName: "Disable Auto-Sync"}}, actions)

	out, err := rt.ExecuteAction(context.Background(), app, "toggle-auto-sync", nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	enabled, found, err := unstructured.NestedBool(out[0].Object.Object, "spec", "syncPolicy", "automated", "enabled")
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, enabled)
	assert.Equal(t, "cd.patrol.example/v1alpha1", out[0].Object.GetAPIVersion())

	// No built-in Application health: it stays opt-in.
	hs, err := rt.HealthScript(app)
	require.NoError(t, err)
	assert.Nil(t, hs)

	appSet := obj(t, `apiVersion: cd.patrol.example/v1alpha1
kind: ApplicationSet
metadata: {name: set, namespace: patrol}
status:
  conditions:
  - {type: ErrorOccurred, status: "True", message: generator failed}
`)
	hs, err = rt.HealthScript(appSet)
	require.NoError(t, err)
	require.NotNil(t, hs)
	assert.Equal(t, SourceBuiltin, hs.Source)
	assert.Equal(t, "argoproj.io/ApplicationSet", hs.Key)
	got, err := rt.GetResourceHealth(appSet)
	require.NoError(t, err)
	assert.Equal(t, "Degraded", string(got.Status))
	assert.Equal(t, "generator failed", got.Message)
}
