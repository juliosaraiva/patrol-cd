package jsonnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeHelperMain()
	MaybeJsonnetMain()
	os.Exit(m.Run())
}

// repo creates a tree under a fresh directory and returns its symlink-resolved path.
// Values starting with "->" create symlinks.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "repo")
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return b.String()
}

func TestEvaluateVarsAndTLAs(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"ext.jsonnet": `{s: std.extVar('s'), c: std.extVar('c').a}`,
		"tla.jsonnet": `function(ns, replicas) {ns: ns, replicas: replicas}`,
		"obj.jsonnet": `{plain: true}`,
	})
	resp, err := Evaluate(Request{
		RepoRoot: root,
		Files:    []string{filepath.Join(root, "ext.jsonnet"), filepath.Join(root, "tla.jsonnet"), filepath.Join(root, "obj.jsonnet")},
		ExtVars:  []Var{{Name: "s", Value: "text"}, {Name: "c", Value: "{a: 1}", Code: true}},
		TLAs:     []Var{{Name: "ns", Value: "prod"}, {Name: "replicas", Value: "3", Code: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("evaluation error: %v", resp.Error)
	}
	want := []string{`{"c":1,"s":"text"}`, `{"ns":"prod","replicas":3}`, `{"plain":true}`}
	if len(resp.Outputs) != len(want) {
		t.Fatalf("outputs = %d, want %d", len(resp.Outputs), len(want))
	}
	for i, w := range want {
		if got := compact(t, resp.Outputs[i]); got != w {
			t.Errorf("output %d = %s, want %s", i, got, w)
		}
	}
}

func TestEvaluateStopsAtFirstError(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"a.jsonnet": `{a: 1}`,
		"b.jsonnet": `{b: std.extVar('missing')}`,
		"c.jsonnet": `{c: 1}`,
	})
	resp, err := Evaluate(Request{RepoRoot: root, Files: []string{
		filepath.Join(root, "a.jsonnet"), filepath.Join(root, "b.jsonnet"), filepath.Join(root, "c.jsonnet"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Index != 1 || len(resp.Outputs) != 1 {
		t.Fatalf("response = %+v, want error at index 1 after one output", resp)
	}
	if !strings.Contains(resp.Error.Message, "Undefined external variable: missing") {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

func TestEvaluateLibraryPathOrder(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"app/main.jsonnet":    `(import 'k.libsonnet') + (import 'local.libsonnet')`,
		"app/local.libsonnet": `{isLocal: true}`,
		"app/k.libsonnet":     `{from: 'app'}`,
		"vendor/k.libsonnet":  `{from: 'vendor'}`,
		"lib2/k.libsonnet":    `{from: 'lib2'}`,
	})
	run := func(jpaths ...string) string {
		resp, err := Evaluate(Request{RepoRoot: root, JPaths: jpaths, Files: []string{filepath.Join(root, "app", "main.jsonnet")}})
		if err != nil || resp.Error != nil {
			t.Fatalf("Evaluate: %v %v", err, resp.Error)
		}
		return compact(t, resp.Outputs[0])
	}
	// The importing file's directory wins over library paths.
	if got := run(filepath.Join(root, "app"), filepath.Join(root, "vendor")); got != `{"from":"app","isLocal":true}` {
		t.Fatalf("got %s", got)
	}
	// Without a local file, the last library path wins (go-jsonnet order).
	if err := os.Remove(filepath.Join(root, "app", "k.libsonnet")); err != nil {
		t.Fatal(err)
	}
	if got := run(filepath.Join(root, "app"), filepath.Join(root, "vendor"), filepath.Join(root, "lib2")); got != `{"from":"lib2","isLocal":true}` {
		t.Fatalf("got %s", got)
	}
	if got := run(filepath.Join(root, "app"), filepath.Join(root, "lib2"), filepath.Join(root, "vendor")); got != `{"from":"vendor","isLocal":true}` {
		t.Fatalf("got %s", got)
	}
}

func TestEvaluateImportConfinement(t *testing.T) {
	t.Parallel()
	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outsideDir, "secret.libsonnet")
	if err := os.WriteFile(outside, []byte(`{secret: true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := repo(t, map[string]string{
		"lib/real.libsonnet":    `{real: true}`,
		"app/inlink.libsonnet":  "->../lib/real.libsonnet",
		"app/outlink.libsonnet": "->" + outside,
		"app/dirlink.libsonnet": "->../lib",
	})
	tests := []struct {
		name, code, wantErr, want string
	}{
		{name: "in-repo symlink", code: `import 'inlink.libsonnet'`, want: `{"real":true}`},
		{name: "absolute outside", code: `import '/etc/passwd'`, wantErr: `jsonnet import "/etc/passwd" resolves outside repository root`},
		{name: "relative traversal", code: `importstr '../../../../../../../../../../../../../../../../../../../../../../../../../../../../../../etc/hosts'`, wantErr: "resolves outside repository root"},
		{name: "device file", code: `importstr '/dev/zero'`, wantErr: `jsonnet import "/dev/zero" resolves outside repository root`},
		{name: "out-of-root symlink", code: `import 'outlink.libsonnet'`, wantErr: `jsonnet import "outlink.libsonnet" resolves outside repository root`},
		{name: "symlink to directory", code: `importstr 'dirlink.libsonnet'`, wantErr: `jsonnet import "dirlink.libsonnet" is not a regular file`},
		{name: "missing", code: `import 'nope.libsonnet'`, wantErr: `couldn't open import "nope.libsonnet": no match locally or in the Jsonnet library paths`},
		{name: "importbin in repo", code: `std.length(importbin '../lib/real.libsonnet')`, want: `12`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file := filepath.Join(root, "app", strings.ReplaceAll(tt.name, " ", "-")+".jsonnet")
			if err := os.WriteFile(file, []byte(tt.code), 0o644); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			resp, err := Evaluate(Request{RepoRoot: root, Files: []string{file}})
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 10*time.Second {
				t.Fatalf("evaluation took %s", time.Since(start))
			}
			if tt.wantErr != "" {
				if resp.Error == nil || !strings.Contains(resp.Error.Message, tt.wantErr) {
					t.Fatalf("error = %+v, want %q", resp.Error, tt.wantErr)
				}
				return
			}
			if resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}
			if got := compact(t, resp.Outputs[0]); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEvaluateLimits(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"big.jsonnet":    `{s: std.repeat('x', 4096)}`,
		"lib.libsonnet":  strings.Repeat(" ", 2048) + "{}",
		"import.jsonnet": `import 'lib.libsonnet'`,
		"deep.jsonnet":   `local f(x) = if x == 0 then 0 else 1 + f(x - 1); f(10000)`,
	})
	resp, err := Evaluate(Request{RepoRoot: root, Files: []string{filepath.Join(root, "big.jsonnet")}, MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "jsonnet output exceeds 1024 bytes") {
		t.Fatalf("output cap: %+v", resp.Error)
	}
	resp, err = Evaluate(Request{RepoRoot: root, Files: []string{filepath.Join(root, "import.jsonnet")}, MaxImportBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "file exceeds 1024 bytes") {
		t.Fatalf("import cap: %+v", resp.Error)
	}
	resp, err = Evaluate(Request{RepoRoot: root, Files: []string{filepath.Join(root, "deep.jsonnet")}, MaxStack: 50})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "max stack frames exceeded") {
		t.Fatalf("stack cap: %+v", resp.Error)
	}
}

func TestRequestValidate(t *testing.T) {
	t.Parallel()
	tests := []Request{
		{RepoRoot: "rel"},
		{RepoRoot: "/abs", Files: []string{"rel.jsonnet"}},
		{RepoRoot: "/abs", JPaths: []string{"rel"}},
		{RepoRoot: "/abs", Files: make([]string, MaxFiles+1)},
	}
	for i, req := range tests {
		if err := req.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("case %d: error %v, want ErrInvalidRequest", i, err)
		}
	}
	if _, err := Evaluate(Request{RepoRoot: "rel"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Evaluate accepted invalid request: %v", err)
	}
}

func TestRunnerChildProcess(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"a.jsonnet":   `[{apiVersion: 'v1', kind: 'ConfigMap', metadata: {name: std.extVar('name')}}]`,
		"b.jsonnet":   `function(n) {n: n}`,
		"bad.jsonnet": `{`,
	})
	r := Runner{Limits: sandbox.Limits{Timeout: time.Minute}}
	resp, err := r.Run(context.Background(), Request{
		RepoRoot: root,
		Files:    []string{filepath.Join(root, "a.jsonnet"), filepath.Join(root, "b.jsonnet")},
		ExtVars:  []Var{{Name: "name", Value: "cm"}},
		TLAs:     []Var{{Name: "n", Value: "7", Code: true}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("evaluation error: %v", resp.Error)
	}
	if got := compact(t, resp.Outputs[0]); got != `[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}]` {
		t.Fatalf("output 0 = %s", got)
	}
	if got := compact(t, resp.Outputs[1]); got != `{"n":7}` {
		t.Fatalf("output 1 = %s", got)
	}

	resp, err = r.Run(context.Background(), Request{RepoRoot: root, Files: []string{filepath.Join(root, "bad.jsonnet")}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Error == nil || resp.Error.Index != 0 {
		t.Fatalf("want evaluation error, got %+v", resp)
	}

	// Empty requests do not start a child.
	resp, err = r.Run(context.Background(), Request{RepoRoot: root})
	if err != nil || len(resp.Outputs) != 0 {
		t.Fatalf("empty request: %+v %v", resp, err)
	}
}

func TestRunnerLimits(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"slow.jsonnet": `std.foldl(function(a, b) a + b, std.range(1, 200000000), 0)`,
		"big.jsonnet":  `[std.repeat('x', 1 << 16)]`,
	})
	r := Runner{Limits: sandbox.Limits{Timeout: 2 * time.Second}}
	start := time.Now()
	_, err := r.Run(context.Background(), Request{RepoRoot: root, Files: []string{filepath.Join(root, "slow.jsonnet")}})
	if !errors.Is(err, ErrChildFailed) || !errors.Is(err, sandbox.ErrTimeout) {
		t.Fatalf("error = %v, want ErrChildFailed wrapping sandbox.ErrTimeout", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("timeout not enforced: %s", time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, Request{RepoRoot: root, Files: []string{filepath.Join(root, "slow.jsonnet")}}); err == nil {
		t.Fatal("canceled context: want error")
	}

	small := Runner{Limits: sandbox.Limits{Timeout: time.Minute, MaxStdout: 1024}}
	if _, err := small.Run(context.Background(), Request{RepoRoot: root, Files: []string{filepath.Join(root, "big.jsonnet")}}); !errors.Is(err, sandbox.ErrOutputLimit) {
		t.Fatalf("error = %v, want sandbox.ErrOutputLimit", err)
	}

	missing := Runner{Path: filepath.Join(root, "no-such-binary")}
	if _, err := missing.Run(context.Background(), Request{RepoRoot: root, Files: []string{filepath.Join(root, "big.jsonnet")}}); !errors.Is(err, ErrChildFailed) {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestChildMainProtocol(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.jsonnet": `{a: 1}`})
	req, err := json.Marshal(Request{RepoRoot: root, Files: []string{filepath.Join(root, "a.jsonnet")}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := childMain(bytes.NewReader(req), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil || len(resp.Outputs) != 1 {
		t.Fatalf("response %q: %v", stdout.String(), err)
	}
	for _, in := range []string{"not json", `{"repoRoot":"relative"}`, strings.Repeat(" ", MaxRequestBytes+1)} {
		stdout.Reset()
		stderr.Reset()
		if code := childMain(strings.NewReader(in), &stdout, &stderr); code != exitProtocol {
			t.Fatalf("input %.20q: exit %d, want %d", in, code, exitProtocol)
		}
		if stderr.Len() == 0 {
			t.Fatalf("input %.20q: no diagnostic", in)
		}
	}
}
