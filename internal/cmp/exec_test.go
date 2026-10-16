package cmp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMergeEnv(t *testing.T) {
	t.Parallel()
	got := mergeEnv(
		[]string{"PATH=/bin", "HOME=/h", "NOEQ", "=x"},
		[]string{"A=1", "PATH=/evil", "A=2", "B=x=y"},
	)
	want := []string{"PATH=/evil", "HOME=/h", "A=2", "B=x=y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
}

// TestMergeEnvProperties checks that every name appears once, with the value of its
// last occurrence, in order of first occurrence.
func TestMergeEnvProperties(t *testing.T) {
	t.Parallel()
	entry := rapid.Custom(func(t *rapid.T) string {
		name := rapid.SampledFrom([]string{"A", "B", "PATH", "HOME", "X_1"}).Draw(t, "name")
		value := rapid.StringMatching(`[a-z=/]{0,6}`).Draw(t, "value")
		return name + "=" + value
	})
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.SliceOf(entry).Draw(t, "base")
		extra := rapid.SliceOf(entry).Draw(t, "extra")
		got := mergeEnv(base, extra)
		all := append(append([]string{}, base...), extra...)
		last := map[string]string{}
		var order []string
		for _, kv := range all {
			k, _, _ := strings.Cut(kv, "=")
			if _, ok := last[k]; !ok {
				order = append(order, k)
			}
			last[k] = kv
		}
		if len(got) != len(order) {
			t.Fatalf("len = %d, want %d (%q)", len(got), len(order), got)
		}
		for i, k := range order {
			if got[i] != last[k] {
				t.Fatalf("entry %d = %q, want %q", i, got[i], last[k])
			}
		}
	})
}

func TestPluginEnvironment(t *testing.T) {
	t.Parallel()
	environ := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/patrol", "SECRET_TOKEN=s3cr3t", "KUBERNETES_SERVICE_HOST=10.0.0.1",
		"AVP_TYPE=vault", "AVP_AUTH_TYPE=k8s", "HELM_CACHE_HOME=/cache", "PATROL_CMP_PASS_ENV=AVP_*",
	}
	env := pluginEnvironment(environ, []string{"AVP_*", "HELM_CACHE_HOME"})
	names := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		names[k] = v
	}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "AVP_TYPE", "AVP_AUTH_TYPE", "HELM_CACHE_HOME"} {
		if _, ok := names[k]; !ok {
			t.Errorf("missing %s in %q", k, env)
		}
	}
	for _, k := range []string{"SECRET_TOKEN", "KUBERNETES_SERVICE_HOST", "PATROL_CMP_PASS_ENV"} {
		if _, ok := names[k]; ok {
			t.Errorf("leaked %s", k)
		}
	}
	if names["AVP_TYPE"] != "vault" {
		t.Errorf("AVP_TYPE = %q", names["AVP_TYPE"])
	}

	all := pluginEnvironment(environ, []string{"*"})
	if len(all) < len(environ) {
		t.Fatalf("'*' passed %d of %d variables", len(all), len(environ))
	}
}

func TestValidatePassEnv(t *testing.T) {
	t.Parallel()
	for _, ok := range [][]string{nil, {"A"}, {"AVP_*"}, {"*"}} {
		if err := validatePassEnv(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{""}, {"A=B"}, {"A*B"}, {"**"}} {
		if err := validatePassEnv(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCommandArgs(t *testing.T) {
	t.Parallel()
	got := commandArgs([]string{"sh", "-c", "echo hi", "", `a"b`})
	want := `sh -c "echo hi" "" a"b`
	if got != want {
		t.Fatalf("commandArgs = %q, want %q", got, want)
	}
}

func TestLookPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := lookPath("tool", "relative:"+dir); err != nil || got != exe {
		t.Fatalf("lookPath = %q, %v", got, err)
	}
	if _, err := lookPath("plain", dir); err == nil {
		t.Fatal("non-executable file found")
	}
	if _, err := lookPath("tool", "relative"); err == nil {
		t.Fatal("relative PATH entry used")
	}
}

func newTestRunner(t testing.TB, lim sandbox.Limits) *runner {
	t.Helper()
	env := pluginEnvironment([]string{"PATH=" + os.Getenv("PATH")}, nil)
	return &runner{limits: lim, pluginEnv: env, path: envValue(env, "PATH"), log: discardLogger()}
}

func TestRunnerErrors(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, sandbox.Limits{Timeout: 30 * time.Second})
	dir := t.TempDir()
	ctx := context.Background()

	out, err := r.run(ctx, "generate", Command{Command: []string{"sh", "-c"}, Args: []string{"printf 'a\\n\\n'"}}, dir, nil)
	if err != nil || out != "a\n" {
		t.Fatalf("out = %q, err = %v (one trailing newline is trimmed)", out, err)
	}

	_, err = r.run(ctx, "generate", Command{Command: []string{"sh", "-c", "echo oops >&2; exit 3"}}, dir, nil)
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if want := "`sh -c \"echo oops >&2; exit 3\"` failed exit status 3: oops"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}

	_, err = r.run(ctx, "generate", Command{Command: []string{"no-such-binary-patrol"}}, dir, nil)
	assertContains(t, err.Error(), "`no-such-binary-patrol` failed exec: \"no-such-binary-patrol\"")

	if _, err := r.run(ctx, "generate", Command{}, dir, nil); err == nil || err.Error() != "Command is empty" {
		t.Fatalf("err = %v", err)
	}

	short := newTestRunner(t, sandbox.Limits{Timeout: 200 * time.Millisecond})
	_, err = short.run(ctx, "generate", Command{Command: []string{"sleep", "10"}}, dir, nil)
	if !errors.Is(err, sandbox.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	assertContains(t, err.Error(), "`sleep 10` failed exec timeout exceeded")

	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = r.run(dctx, "generate", Command{Command: []string{"sleep", "10"}}, dir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	assertContains(t, err.Error(), "failed request deadline exceeded")

	capped := newTestRunner(t, sandbox.Limits{Timeout: 30 * time.Second, MaxStdout: 10})
	_, err = capped.run(ctx, "generate", Command{Command: []string{"sh", "-c", "yes | head -c 1000"}}, dir, nil)
	if !errors.Is(err, sandbox.ErrOutputLimit) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunnerRelativeCommand(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, sandbox.Limits{Timeout: 30 * time.Second})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gen.sh"), []byte("#!/bin/sh\necho from-script\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := r.run(context.Background(), "generate", Command{Command: []string{"./gen.sh"}}, dir, nil)
	if err != nil || out != "from-script" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}
