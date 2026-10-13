package lua

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	glua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/health"
)

const rolloutYAML = `
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  labels:
    app.kubernetes.io/instance: helm-guestbook
  name: helm-guestbook
  namespace: default
  resourceVersion: "123"
`

const noScriptYAML = `
apiVersion: not-an-endpoint.io/v1alpha1
kind: Test
metadata:
  name: helm-guestbook
  namespace: default
`

const crossplaneYAML = `
apiVersion: ec2.aws.crossplane.io/v1alpha1
kind: Instance
metadata:
  name: sample-crosspalne-ec2-instance
spec:
  forProvider:
    region: us-west-2
`

const healthyWithMessage = `a = {}
a.status = "Healthy"
a.message ="NeedsToBeChanged"
if obj.metadata.name == "helm-guestbook" then
	a.message = "testMessage"
end
return a`

// newTestRuntime uses a deadline far above any legitimate script so tests stay reliable
// on loaded machines and under the race detector; timeouts are tested explicitly.
func newTestRuntime(overrides map[string]ResourceOverride) *Runtime {
	return New(overrides, WithTimeout(30*time.Second))
}

func obj(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &m))
	return &unstructured.Unstructured{Object: m}
}

func TestExecuteHealthResults(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)
	tests := []struct {
		name    string
		script  string
		want    *health.HealthStatus
		wantErr string
	}{
		{name: "status and message", script: healthyWithMessage, want: &health.HealthStatus{Status: health.StatusHealthy, Message: "testMessage"}},
		{name: "keys are case-insensitive", script: `return {Status = "Degraded", MESSAGE = "x"}`, want: &health.HealthStatus{Status: health.StatusDegraded, Message: "x"}},
		{name: "invalid status", script: `local hs = {} hs.status = "test" return hs`, want: &health.HealthStatus{Status: health.StatusUnknown, Message: InvalidHealthStatusMessage}},
		{name: "missing status", script: `return {message = "only"}`, want: &health.HealthStatus{Status: health.StatusUnknown, Message: InvalidHealthStatusMessage}},
		{name: "no return", script: "local hs = {}\nreturn", want: &health.HealthStatus{}},
		{name: "nil return", script: "return nil", want: &health.HealthStatus{}},
		{name: "empty table", script: "local hs = {}\nreturn hs", want: &health.HealthStatus{}},
		{name: "wrong field type", script: `return {status = 5}`, want: &health.HealthStatus{}},
		{name: "last return value wins", script: `return {status = "Degraded"}, {status = "Healthy"}`, want: &health.HealthStatus{Status: health.StatusHealthy}},
		{name: "number return", script: "return 1", wantErr: "expect table output from Lua script, not number"},
		{name: "runtime error has no stack trace", script: "local x = nil\nreturn x.y", wantErr: "<string>:2: attempt to index a non-table object(nil) with key 'y'"},
		{name: "syntax error", script: "return {", wantErr: "<string>"},
		{name: "error() with level 0", script: `error("custom failure", 0)`, wantErr: "custom failure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), tc.script, false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.NotContains(t, err.Error(), "stack traceback")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), "return 1", false)
	require.ErrorIs(t, err, ErrInvalidOutput)
	assert.Equal(t, "expect table output from Lua script, not number", err.Error())

	_, err = rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), "local x = nil\nreturn x.y", false)
	var se *ScriptError
	require.ErrorAs(t, err, &se)
	var apiErr *glua.ApiError
	require.ErrorAs(t, err, &apiErr)
	assert.False(t, se.Timeout)
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	scripts := map[string]string{
		"infinite loop":         `while true do end`,
		"loop inside pcall":     `while true do pcall(function() while true do end end) end`,
		"pcall around the loop": `pcall(function() while true do end end) while true do end`,
		"recursion in loop":     `local function f(n) if n > 50 then return 0 end return f(n+1) end while true do f(0) end`,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rt := New(nil, WithTimeout(100*time.Millisecond))
			start := time.Now()
			_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), script, false)
			elapsed := time.Since(start)
			require.ErrorIs(t, err, ErrTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, err.Error(), "exceeded execution timeout of 100ms")
			assert.Less(t, elapsed, 5*time.Second)
		})
	}

	t.Run("default timeout is one second", func(t *testing.T) {
		t.Parallel()
		rt := New(nil)
		assert.Equal(t, time.Second, rt.timeout)
		start := time.Now()
		_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), `while true do end`, false)
		require.ErrorIs(t, err, ErrTimeout)
		assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond)
	})

	t.Run("caller cancellation", func(t *testing.T) {
		t.Parallel()
		rt := New(nil, WithTimeout(10*time.Second))
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		_, err := rt.ExecuteHealth(ctx, obj(t, rolloutYAML), `while true do end`, false)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrTimeout)
	})

	t.Run("actions time out too", func(t *testing.T) {
		t.Parallel()
		rt := New(nil, WithTimeout(50*time.Millisecond))
		_, err := rt.ExecuteActionScript(context.Background(), obj(t, rolloutYAML), `while true do end`, nil)
		require.ErrorIs(t, err, ErrTimeout)
		_, err = rt.ExecuteDiscovery(context.Background(), obj(t, rolloutYAML), []string{`while true do end`})
		require.ErrorIs(t, err, ErrTimeout)
	})
}

func TestSandboxSafeProfile(t *testing.T) {
	t.Parallel()
	// A generous deadline: these scripts must fail on their own, not by timing out.
	rt := New(nil, WithTimeout(30*time.Second))
	unavailable := map[string]string{
		"os.execute":       `os.execute("echo pwned")`,
		"os.getenv":        `os.getenv("HOME")`,
		"os.remove":        `os.remove("/tmp/x")`,
		"os.exit":          `os.exit(1)`,
		"io.open":          `io.open("/etc/passwd")`,
		"io.write":         `io.write("x")`,
		"dofile":           `dofile("/etc/passwd")`,
		"loadfile":         `loadfile("/etc/passwd")`,
		"load":             `load(function() return nil end)`,
		"loadstring":       `loadstring("return 1")()`,
		"require io":       `require("io")`,
		"require file":     `require("foo")`,
		"module":           `module("x")`,
		"collectgarbage":   `collectgarbage()`,
		"debug":            `debug.getinfo(1)`,
		"string.dump":      `string.dump(function() end)`,
		"coroutine":        `coroutine.create(function() end)`,
		"package":          `package.loadlib("x", "y")`,
		"huge string.rep":  `local s = string.rep("x", 1e10)`,
		"huge method rep":  `local s = ("xy"):rep(1e9)`,
		"huge format":      `local s = string.format("%999999999d", 1)`,
		"huge precision":   `local s = string.format("%.999999999f", 1)`,
		"registry blowout": `local t = {} for i = 1, 70000 do t[i] = i end return {status = tostring(select('#', unpack(t)))}`,
		"stack overflow":   `local function f() return 1 + f() end return f()`,
	}
	for name, script := range unavailable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), script, false)
			require.Error(t, err)
			var se *ScriptError
			require.ErrorAs(t, err, &se)
			assert.False(t, se.Timeout, "must fail fast, not time out: %v", err)
		})
	}

	available := map[string]string{
		"os.time":            `return {status = "Healthy", message = tostring(os.time() > 0)}`,
		"os.date":            `return {status = "Healthy", message = tostring(string.len(os.date("!%Y-%m-%dT%XZ")) == 20)}`,
		"require os":         `local os = require("os") return {status = "Healthy", message = tostring(os.date("*t").year > 2000)}`,
		"string methods":     `local s = "Using lua standard library" return {status = "Healthy", message = tostring(s:find("standard") ~= nil and string.find(s, "lua") ~= nil)}`,
		"math":               `return {status = "Healthy", message = tostring(math.huge > math.floor(1.5))}`,
		"table":              `local t = {3, 1, 2} table.sort(t) return {status = "Healthy", message = tostring(table.concat(t, ",") == "1,2,3" and table.getn(t) == 3)}`,
		"print is a no-op":   `print("hidden") return {status = "Healthy", message = "true"}`,
		"pcall/error":        `local ok = pcall(function() error("x") end) return {status = "Healthy", message = tostring(not ok)}`,
		"small rep":          `return {status = "Healthy", message = tostring(#string.rep("ab", 10) == 20)}`,
		"format width 99":    `return {status = "Healthy", message = tostring(#string.format("%99d", 1) == 99)}`,
		"actionParams empty": `return {status = "Healthy", message = tostring(next(actionParams) == nil)}`,
	}
	for name, script := range available {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), script, false)
			require.NoError(t, err)
			assert.Equal(t, &health.HealthStatus{Status: health.StatusHealthy, Message: "true"}, got)
		})
	}
}

func TestSandboxOpenLibs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-file"), 0o600))
	script := fmt.Sprintf(`local f = io.open(%q, "r")
local s = f:read("*a")
f:close()
return {status = "Healthy", message = s .. "/" .. type(os.execute) .. "/" .. type(debug)}`, file)

	sa := obj(t, "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: test, namespace: test}")
	open := newTestRuntime(map[string]ResourceOverride{"ServiceAccount": {HealthLua: script, UseOpenLibs: true}})
	got, err := open.GetResourceHealth(sa)
	require.NoError(t, err)
	assert.Equal(t, &health.HealthStatus{Status: health.StatusHealthy, Message: "from-file/function/table"}, got)

	closed := newTestRuntime(map[string]ResourceOverride{"ServiceAccount": {HealthLua: script, UseOpenLibs: false}})
	_, err = closed.GetResourceHealth(sa)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attempt to index a non-table object(nil) with key 'open'")

	// Built-in scripts never get the open profile.
	hs, err := newTestRuntime(nil).HealthScript(obj(t, rolloutYAML))
	require.NoError(t, err)
	require.NotNil(t, hs)
	assert.Equal(t, SourceBuiltin, hs.Source)
	assert.False(t, hs.UseOpenLibs)
}

func TestHealthScriptLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		overrides  map[string]ResourceOverride
		doc        string
		wantSource ScriptSource
		wantKey    string
		wantScript string
		wantOpen   bool
	}{
		{
			name:       "exact override",
			overrides:  map[string]ResourceOverride{"argoproj.io/Rollout": {HealthLua: healthyWithMessage}},
			doc:        rolloutYAML,
			wantSource: SourceConfigured, wantKey: "argoproj.io/Rollout", wantScript: healthyWithMessage,
		},
		{
			name:       "exact override keeps useOpenLibs",
			overrides:  map[string]ResourceOverride{"argoproj.io/Rollout": {HealthLua: "return nil", UseOpenLibs: true}},
			doc:        rolloutYAML,
			wantSource: SourceConfigured, wantKey: "argoproj.io/Rollout", wantScript: "return nil", wantOpen: true,
		},
		{
			name:       "kind wildcard",
			overrides:  map[string]ResourceOverride{"argoproj.io/*": {HealthLua: healthyWithMessage}},
			doc:        rolloutYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "argoproj.io/*", wantScript: healthyWithMessage,
		},
		{
			name:       "group wildcard",
			overrides:  map[string]ResourceOverride{"*.io/Rollout": {HealthLua: healthyWithMessage}},
			doc:        rolloutYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "*.io/Rollout", wantScript: healthyWithMessage,
		},
		{
			name:       "group and kind wildcard",
			overrides:  map[string]ResourceOverride{"*.aws.crossplane.io/*": {HealthLua: healthyWithMessage}},
			doc:        crossplaneYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "*.aws.crossplane.io/*", wantScript: healthyWithMessage,
		},
		{
			name: "empty wildcard skipped for non-empty one",
			overrides: map[string]ResourceOverride{
				"*.aws.crossplane.io/*": {HealthLua: ""},
				"*.aws*":                {HealthLua: "return {status = 'UnHealthy'}"},
			},
			doc:        crossplaneYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "*.aws*", wantScript: "return {status = 'UnHealthy'}",
		},
		{
			name: "wildcards are chosen in sorted key order",
			overrides: map[string]ResourceOverride{
				"ec2.*/*":               {HealthLua: "return 2"},
				"*.aws.crossplane.io/*": {HealthLua: "return 1"},
			},
			doc:        crossplaneYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "*.aws.crossplane.io/*", wantScript: "return 1",
		},
		{
			name:       "exact override with empty script falls back to builtin",
			overrides:  map[string]ResourceOverride{"argoproj.io/Rollout": {Actions: "x"}, "*/*": {IgnoreDifferences: "jsonPointers: [/status]"}},
			doc:        rolloutYAML,
			wantSource: SourceBuiltin, wantKey: "argoproj.io/Rollout",
		},
		{
			name:       "builtin wildcard directory",
			doc:        crossplaneYAML,
			wantSource: SourceBuiltinWildcard, wantKey: "_.crossplane.io/_",
		},
		{
			name:       "configured wildcard beats builtin",
			overrides:  map[string]ResourceOverride{"argoproj.io/*": {HealthLua: "return nil"}},
			doc:        rolloutYAML,
			wantSource: SourceConfiguredWildcard, wantKey: "argoproj.io/*", wantScript: "return nil",
		},
		{
			name:      "invalid glob key only fails to match",
			overrides: map[string]ResourceOverride{"argoproj.io/[": {HealthLua: "return nil"}},
			doc:       noScriptYAML,
		},
		{name: "no script", doc: noScriptYAML},
		{name: "wildcard not matching", overrides: map[string]ResourceOverride{"*.aws.crossplane.io/*": {HealthLua: "return nil"}}, doc: "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: x}"},
		{name: "core kind without script", doc: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := New(tc.overrides)
			hs, err := rt.HealthScript(obj(t, tc.doc))
			require.NoError(t, err)
			if tc.wantSource == "" {
				assert.Nil(t, hs)
				got, err := rt.GetResourceHealth(obj(t, tc.doc))
				require.NoError(t, err)
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, hs)
			assert.Equal(t, tc.wantSource, hs.Source)
			assert.Equal(t, tc.wantKey, hs.Key)
			assert.Equal(t, tc.wantOpen, hs.UseOpenLibs)
			if tc.wantScript != "" {
				assert.Equal(t, tc.wantScript, hs.Script)
			} else {
				assert.NotEmpty(t, hs.Script)
			}
		})
	}
}

func TestBuiltinWildcardDirs(t *testing.T) {
	t.Parallel()
	dirs, err := defaultBuiltins().wildcardDirs()
	require.NoError(t, err)
	// Same list as upstream's Test_getHealthScriptPaths.
	assert.Equal(t, []string{
		"_.cnrm.cloud.google.com/_",
		"_.crossplane.io/_",
		"_.upbound.io/_",
		"grafana-org-operator.kubitus-project.gitlab.io/_",
		"operator.victoriametrics.com/_",
	}, dirs)
}

func TestGetResourceHealthViaAssess(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(map[string]ResourceOverride{
		"*.aws.crossplane.io/*": {HealthLua: "hs = {}\nhs.status = \"Healthy\"\nreturn hs"},
		"ServiceAccount":        {HealthLua: "return 1"},
	})
	got, err := health.Assess(obj(t, crossplaneYAML), rt)
	require.NoError(t, err)
	assert.Equal(t, &health.HealthStatus{Status: health.StatusHealthy}, got)

	got, err = health.Assess(obj(t, "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: sa, namespace: ns}"), rt)
	require.ErrorIs(t, err, health.ErrOverride)
	require.ErrorIs(t, err, ErrInvalidOutput)
	assert.Equal(t, &health.HealthStatus{Status: health.StatusUnknown, Message: "expect table output from Lua script, not number"}, got)

	// Lua overrides take precedence over Go built-ins.
	pod := obj(t, "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nstatus: {phase: Running}")
	got, err = health.Assess(pod, newTestRuntime(map[string]ResourceOverride{"Pod": {HealthLua: `return {status = "Suspended"}`}}))
	require.NoError(t, err)
	assert.Equal(t, health.StatusSuspended, got.Status)

	// A nil *Runtime is a valid "no overrides" value and never panics.
	var nilRT *Runtime
	got, err = health.Assess(pod, nilRT)
	require.NoError(t, err)
	assert.Equal(t, health.StatusUnknown, got.Status) // Running pod without restartPolicy
	got, err = nilRT.ExecuteHealth(context.Background(), pod, `return {status = "Healthy"}`, false)
	require.NoError(t, err)
	assert.Equal(t, health.StatusHealthy, got.Status)
	actions, err := nilRT.ListActions(context.Background(), pod)
	require.NoError(t, err)
	assert.Empty(t, actions)
	_, err = nilRT.ExecuteAction(context.Background(), pod, "restart", nil)
	require.ErrorIs(t, err, ErrActionNotFound)
}

func TestApplicationHealthLuaMatchesGo(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(map[string]ResourceOverride{"cd.patrol.example/Application": {HealthLua: ApplicationHealthLua}})
	docs := []string{
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}",
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {sync: {status: Synced}}",
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {health: {status: Healthy}}",
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {health: {status: Degraded, message: boom}}",
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {health: {status: Weird}}",
		"apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata: {name: a}\nstatus: {health: {message: no status}}",
	}
	for i, doc := range docs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			o := obj(t, doc)
			viaLua, err := health.Assess(o, rt)
			require.NoError(t, err)
			viaGo, err := health.Assess(o, health.WithApplicationHealth(nil))
			require.NoError(t, err)
			assert.Equal(t, viaLua, viaGo)
		})
	}
}

func TestDeterministicPairsOrder(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)
	o := obj(t, "apiVersion: v1\nkind: X\nmetadata: {name: a, labels: {d: '4', b: '2', a: '1', c: '3', e: '5'}}")
	script := `local keys = {}
for k, _ in pairs(obj.metadata.labels) do table.insert(keys, k) end
return {status = "Healthy", message = table.concat(keys, ",")}`
	for range 20 {
		got, err := rt.ExecuteHealth(context.Background(), o, script, false)
		require.NoError(t, err)
		require.Equal(t, "a,b,c,d,e", got.Message)
	}
}

func TestConcurrentEvaluation(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(map[string]ResourceOverride{"argoproj.io/Rollout": {HealthLua: healthyWithMessage}})
	o := obj(t, rolloutYAML)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 64 {
		wg.Go(func() {
			got, err := rt.GetResourceHealth(o.DeepCopy())
			if err == nil && got.Message != "testMessage" {
				err = errors.New("unexpected message " + got.Message)
			}
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestOutputEncodingErrors(t *testing.T) {
	t.Parallel()
	rt := newTestRuntime(nil)
	tests := map[string]struct {
		script string
		is     error
	}{
		"cycle":         {`local t = {} t.self = t return t`, ErrNestedTable},
		"sparse array":  {`local t = {} t[1] = 1 t[3] = 3 return t`, ErrSparseArray},
		"mixed keys":    {`return {1, a = "b"}`, ErrInvalidKeys},
		"too deep":      {`local t = {} local c = t for i = 1, 2000 do c.n = {} c = c.n end return t`, ErrValueTooDeep},
		"function leaf": {`return {f = function() end}`, errUnsupportedLV},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), tc.script, false)
			require.ErrorIs(t, err, tc.is)
		})
	}
	// Shared (non-cyclic) subtables are accepted.
	got, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), `local s = {x = 1} return {status = "Healthy", a = s, b = s}`, false)
	require.NoError(t, err)
	assert.Equal(t, health.StatusHealthy, got.Status)
}

func TestCompileCacheReuse(t *testing.T) {
	t.Parallel()
	script := `return {status = "Healthy", message = "cache-` + strings.Repeat("x", 8) + `"}`
	p1, err := compile(script)
	require.NoError(t, err)
	p2, err := compile(script)
	require.NoError(t, err)
	assert.Same(t, p1, p2)
	_, err = compile("return {")
	var se *ScriptError
	require.ErrorAs(t, err, &se)
}

func BenchmarkBuiltinHealth(b *testing.B) {
	rt := New(nil)
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(rolloutYAML), &m); err != nil {
		b.Fatal(err)
	}
	o := &unstructured.Unstructured{Object: m}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rt.GetResourceHealth(o); err != nil {
			b.Fatal(err)
		}
	}
}

func TestBuiltinFSOverride(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"b_.example.com/_/health.lua":          {Data: []byte(`return {status = "Degraded", message = "b"}`)},
		"_.example.com/_/health.lua":           {Data: []byte(`return {status = "Healthy", message = "a"}`)},
		"exact.example.com/Kind/health.lua":    {Data: []byte(`return {status = "Suspended"}`)},
		"exact.example.com/Kind/actions/x.txt": {Data: []byte("ignored")},
	}
	rt := New(nil, withBuiltinFS(fsys), WithTimeout(30*time.Second))
	got, err := rt.GetResourceHealth(obj(t, "apiVersion: b.example.com/v1\nkind: Thing\nmetadata: {name: x}"))
	require.NoError(t, err)
	// Wildcard directories are tried in sorted order: "_.example.com/_" < "b_.example.com/_".
	assert.Equal(t, &health.HealthStatus{Status: health.StatusHealthy, Message: "a"}, got)

	got, err = rt.GetResourceHealth(obj(t, "apiVersion: exact.example.com/v1\nkind: Kind\nmetadata: {name: x}"))
	require.NoError(t, err)
	assert.Equal(t, health.StatusSuspended, got.Status)

	got, err = rt.GetResourceHealth(obj(t, "apiVersion: v1\nkind: Pod\nmetadata: {name: x}"))
	require.NoError(t, err)
	assert.Nil(t, got)

	// A malformed wildcard directory surfaces as an error (upstream: Unknown health).
	bad := New(nil, withBuiltinFS(fstest.MapFS{"a_[/_/health.lua": {Data: []byte("return nil")}}))
	_, err = bad.GetResourceHealth(obj(t, "apiVersion: z.io/v1\nkind: K\nmetadata: {name: x}"))
	require.ErrorContains(t, err, "error while fetching built-in health script")
	hs, err := health.Assess(obj(t, "apiVersion: z.io/v1\nkind: K\nmetadata: {name: x}"), bad)
	require.Error(t, err)
	assert.Equal(t, health.StatusUnknown, hs.Status)
}
