package render

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gobwas/glob"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

func TestOutputValidationAndLimits(t *testing.T) {
	t.Parallel()
	r := newTestRenderer(t)
	root := tempRepo(t, map[string]string{
		"a.yaml": cm("a", nil) + "---\n" + cm("b", nil) + "---\n" + cm("c", nil),
	})
	in := dirInput(root, nil)
	in.Limits.MaxManifests = 2
	if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
		t.Fatalf("error = %v, want ErrTooManyManifests", err)
	}
	in.Limits.MaxManifests = -1 // unlimited
	assertEqual(t, len(generate(t, r, in).Manifests), 3)

	in = dirInput(root, nil)
	in.Limits.MaxOutputBytes = 150
	if err := generateErr(t, r, in); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("error = %v, want ErrOutputTooLarge", err)
	}

	bad := tempRepo(t, map[string]string{"x.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: just-a-string\n"})
	if err := generateErr(t, r, dirInput(bad, nil)); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("error = %v, want ErrInvalidManifest", err)
	}
	noKind := tempRepo(t, map[string]string{"x.json": `{"apiVersion":"v1","kind":"","metadata":{"name":"x"}}`})
	if err := generateErr(t, r, dirInput(noKind, nil)); err == nil {
		t.Fatal("object with empty kind accepted")
	}
}

func TestSplitYAML(t *testing.T) {
	t.Parallel()
	objs, err := SplitYAML([]byte("---\n" + cm("a", nil) + "---\nnull\n---\n\n---\n" + `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	assertSlice(t, names(objs), []string{"ConfigMap/a", "ConfigMap/b"})
	if _, err := SplitYAML([]byte("apiVersion: v1\nmetadata: {}\n")); err == nil {
		t.Fatal("object without kind accepted")
	}
	if _, err := SplitYAML([]byte("- a\n- b\n")); err == nil {
		t.Fatal("non-object document accepted")
	}
}

func TestTrackingMetadata(t *testing.T) {
	t.Parallel()
	root := tempRepo(t, map[string]string{
		"cm.yaml":  cm("a", nil),
		"crd.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n",
	})
	r := newTestRenderer(t)
	in := dirInput(root, nil)
	in.AppName = "my-app"
	in.Tracking = &Tracking{Tracker: tracking.New(tracking.Config{}), Method: tracking.MethodLabel}
	res := generate(t, r, in)
	assertEqual(t, findObj(t, res.Manifests, "ConfigMap", "a").GetLabels()[v1alpha1.LabelKeyAppInstanceDefault], "my-app")
	if l := findObj(t, res.Manifests, "CustomResourceDefinition", "widgets.example.com").GetLabels(); len(l) != 0 {
		t.Fatalf("CRD labeled: %v", l)
	}

	in.Tracking = &Tracking{Tracker: tracking.New(tracking.Config{}), Method: tracking.MethodAnnotation, InstallationID: "inst-1"}
	res = generate(t, r, in)
	ann := findObj(t, res.Manifests, "ConfigMap", "a").GetAnnotations()
	assertEqual(t, ann[v1alpha1.AnnotationKeyTrackingID], "my-app:/ConfigMap:default/a")

	// Without tracking nothing is added.
	in.Tracking = nil
	res = generate(t, r, in)
	if l := findObj(t, res.Manifests, "ConfigMap", "a").GetLabels(); len(l) != 0 {
		t.Fatalf("labels without tracking: %v", l)
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	r := newTestRenderer(t, func(c *Config) { c.Registerer = reg })
	root := tempRepo(t, map[string]string{"a.yaml": cm("a", nil), "j.jsonnet": "[]"})
	generate(t, r, dirInput(root, nil))
	_ = generateErr(t, r, Input{RootDir: root, Source: v1alpha1.ApplicationSource{Path: "missing"}})

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			key := f.GetName() + labelString(m)
			switch {
			case m.GetHistogram() != nil:
				got[key] = float64(m.GetHistogram().GetSampleCount())
			case m.GetCounter() != nil:
				got[key] = m.GetCounter().GetValue()
			}
		}
	}
	for key, want := range map[string]float64{
		"patrol_render_duration_seconds{outcome=success,source_type=Directory}": 1,
		"patrol_render_duration_seconds{outcome=error,source_type=unknown}":     1,
		"patrol_render_jsonnet_evaluations_total{outcome=success}":              1,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v (all: %v)", key, got[key], want, got)
		}
	}
	if _, err := New(Config{Registerer: reg}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate registration: %v", err)
	}
}

func labelString(m *dto.Metric) string {
	var parts []string
	for _, l := range m.GetLabel() {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{WorkDir: "relative"}); err == nil {
		t.Fatal("relative work dir accepted")
	}
	_, err := New(Config{WorkDir: t.TempDir(), PluginTarExclusions: []string{"["}})
	if err == nil || !strings.Contains(err.Error(), `error verifying exclusion pattern "["`) {
		t.Fatalf("invalid exclusion: %v", err)
	}
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, r.cfg.KustomizePath, "kustomize")
	assertEqual(t, r.cfg.PluginSocketDir, "/home/patrol/cmp-server/plugins")
}

func TestMergeLimits(t *testing.T) {
	t.Parallel()
	base := Limits{MaxManifests: 5, MaxOutputBytes: 10}
	got := mergeLimits(base, Limits{MaxOutputBytes: 20, MaxCombinedDirectoryManifestsSize: -1}).normalized()
	assertEqual(t, got.MaxManifests, 5)
	assertEqual(t, got.MaxOutputBytes, int64(20))
	assertEqual(t, got.MaxCombinedDirectoryManifestsSize, int64(-1))
	assertEqual(t, got.MaxScratchCopyFiles, DefaultMaxScratchCopyFiles)
	assertEqual(t, Limits{}.normalized().MaxCombinedDirectoryManifestsSize, DefaultMaxCombinedDirectoryManifestsSize)
}

func TestCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := tempRepo(t, map[string]string{"a.yaml": cm("a", nil)})
	if _, err := newTestRenderer(t).Generate(ctx, dirInput(root, nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestDirGlobMatchesGobwas checks that the linear-time translation of alternation
// patterns matches exactly what gobwas/glob (upstream's matcher) matches.
func TestDirGlobMatchesGobwas(t *testing.T) {
	t.Parallel()
	patternAtoms := []string{"a", "b", "/", ".", "*", "**", "?", "[ab]", "[!a]", "[a-c]", "{a,b}", "{,x}", "{a*,b/?}", "-", "\\*", "{{a,b},c}"}
	textAtoms := []string{"a", "b", "c", "x", "/", ".", "-", "*", "ab", ""}
	rng := rand.New(rand.NewPCG(1, 2))
	compared := 0
	for i := 0; i < 3000; i++ {
		var pb strings.Builder
		for n := 1 + rng.IntN(5); n > 0; n-- {
			pb.WriteString(patternAtoms[rng.IntN(len(patternAtoms))])
		}
		pattern := pb.String()
		ref, refErr := glob.Compile(pattern)
		got, err := compileDirGlob(pattern)
		if refErr != nil {
			if err == nil {
				t.Fatalf("pattern %q: gobwas rejects it but compileDirGlob accepted it", pattern)
			}
			continue
		}
		if err != nil {
			if errors.Is(err, errGlobUntranslatable) {
				continue // matches nothing, documented
			}
			t.Fatalf("pattern %q: %v", pattern, err)
		}
		for j := 0; j < 20; j++ {
			var tb strings.Builder
			for n := rng.IntN(6); n > 0; n-- {
				tb.WriteString(textAtoms[rng.IntN(len(textAtoms))])
			}
			text := tb.String()
			if want, have := ref.Match(text), got.match(text); want != have {
				t.Fatalf("pattern %q text %q: gobwas %v, compileDirGlob %v", pattern, text, want, have)
			}
			compared++
		}
	}
	if compared < 10000 {
		t.Fatalf("only %d comparisons", compared)
	}
}

func TestDirGlobPathologicalAlternation(t *testing.T) {
	t.Parallel()
	pattern := strings.Repeat("{a,a*}", 30) + "b"
	g, err := compileDirGlob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if g.re == nil {
		t.Fatal("alternation pattern not translated")
	}
	if g.match(strings.Repeat("a", 5000)) {
		t.Fatal("unexpected match")
	}
}

func TestCopyTree(t *testing.T) {
	t.Parallel()
	src := tempRepo(t, map[string]string{
		"a/file.txt": "hello",
		"a/link":     "->file.txt",
		".git/HEAD":  "ref",
		"sub/.git":   "gitfile",
		"exec.sh":    "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(src, "exec.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(context.Background(), src, dst, -1, -1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "a", "link"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("link: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "sub", ".git")); !os.IsNotExist(err) {
		t.Fatalf("sub/.git copied: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "exec.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode not kept: %v %v", info, err)
	}

	esc := tempRepo(t, map[string]string{"a/up": "->../../outside"})
	if err := copyTree(context.Background(), esc, filepath.Join(t.TempDir(), "c"), -1, -1); err == nil {
		t.Fatal("escaping relative symlink accepted")
	}
}
