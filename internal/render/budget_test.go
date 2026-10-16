package render

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
)

// tinyDocs is a YAML stream of n minimal documents, the worst case for the ratio of
// decoded objects to input bytes.
func tinyDocs(n int) []byte {
	return bytes.Repeat([]byte("kind: A\n---\n"), n)
}

func TestManifestBudgetStopsDecodingEarly(t *testing.T) {
	t.Parallel()
	listDoc := "kind: List\nitems: [{kind: A}, {kind: B}]\n---\n"
	tests := []struct {
		name    string
		decode  func(b *manifestBudget) (int, error)
		max     int
		decoded int // objects returned when the budget is exceeded
	}{
		{"splitYAML", func(b *manifestBudget) (int, error) {
			objs, err := splitYAML(tinyDocs(100000), b)
			return len(objs), err
		}, 10, 10},
		{"splitYAMLOrJSON", func(b *manifestBudget) (int, error) {
			objs, err := splitYAMLOrJSON(bytes.NewReader(tinyDocs(100000)), b)
			return len(objs), err
		}, 10, 10},
		{"list items count individually", func(b *manifestBudget) (int, error) {
			objs, err := splitYAML([]byte(strings.Repeat(listDoc, 100)), b)
			return len(objs), err
		}, 10, 5},
		{"JSON array", func(b *manifestBudget) (int, error) {
			objs, _, err := decodeJSONArray([]byte("["+strings.Repeat(`{"kind":"A"},`, 100000)+`{"kind":"A"}]`), b)
			return len(objs), err
		}, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newManifestBudget(tt.max)
			n, err := tt.decode(b)
			if !errors.Is(err, ErrTooManyManifests) {
				t.Fatalf("error = %v, want ErrTooManyManifests", err)
			}
			assertEqual(t, n, tt.decoded)
			if b.n > tt.max+2 {
				t.Fatalf("decoded %d objects past a budget of %d", b.n, tt.max)
			}
		})
	}
}

func TestManifestBudgetAccounting(t *testing.T) {
	t.Parallel()
	// Null documents, null Lists and nil budgets cost nothing; the exported SplitYAML
	// is unlimited.
	b := newManifestBudget(2)
	objs, err := splitYAML([]byte("---\nnull\n---\nkind: List\nitems: null\n---\nkind: A\n---\nkind: List\nitems: [{kind: B}]\n"), b)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(objs), 3)
	assertEqual(t, b.n, 2)
	if _, err := splitYAML(tinyDocs(3), nil); err != nil {
		t.Fatal(err)
	}
	if objs, err := SplitYAML(tinyDocs(1001)); err != nil || len(objs) != 1001 {
		t.Fatalf("SplitYAML: %d objects, %v", len(objs), err)
	}
	if err := newManifestBudget(-1).take(nil); err != nil {
		t.Fatal(err)
	}
	unlimited := newManifestBudget(-1)
	if _, err := splitYAML(tinyDocs(50), unlimited); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJSONArray(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		ok    bool
		count int
	}{
		{`[]`, true, 0},
		{` [ {"kind":"A"}, null, {"kind":"B"} ] `, true, 3},
		{`{"kind":"A"}`, false, 0},
		{`[{"kind":"A"}] trailing`, false, 0},
		{`[{"kind":"A"}`, false, 0},
		{`[1, 2]`, false, 0},
		{`[{"apiVersion":"v1"}]`, false, 0}, // no kind: json.Unmarshal into []*Unstructured fails too
		{`"string"`, false, 0},
	}
	for _, tt := range tests {
		objs, ok, err := decodeJSONArray([]byte(tt.in), newManifestBudget(10))
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if ok != tt.ok || len(objs) != tt.count {
			t.Errorf("%s: ok=%v count=%d, want ok=%v count=%d", tt.in, ok, len(objs), tt.ok, tt.count)
		}
	}
	objs, _, _ := decodeJSONArray([]byte(`[{"kind":"A"},null]`), nil)
	if objs[1] != nil {
		t.Fatalf("null element decoded to %v", objs[1])
	}
}

// TestManifestLimitBoundsMemory renders a Directory source made of many tiny
// documents with a small MaxManifests: the call must fail before materializing the
// whole stream. Decoding all 700k documents of this 8 MiB file allocates about
// 250 MiB; stopping at the limit allocates a small multiple of the file size. Not
// parallel: it measures the process-wide allocation counter.
func TestManifestLimitBoundsMemory(t *testing.T) {
	r := newTestRenderer(t)
	root := tempRepo(t, map[string]string{"many.yaml": string(tinyDocs(8 << 20 / len("kind: A\n---\n")))})
	in := dirInput(root, nil)
	in.Limits.MaxManifests = 1000
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := generateErr(t, r, in)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Fatalf("allocated %d MiB before failing (%v), want < 64 MiB", alloc>>20, err)
	}
	if !errors.Is(err, ErrTooManyManifests) {
		t.Fatalf("error = %v, want ErrTooManyManifests", err)
	}
}

func TestManifestLimitAcrossSources(t *testing.T) {
	t.Parallel()
	t.Run("directory files and jsonnet share one budget", func(t *testing.T) {
		t.Parallel()
		r := newTestRenderer(t)
		root := tempRepo(t, map[string]string{
			"a.yaml":    cm("a", nil) + "---\n" + cm("b", nil),
			"b.json":    `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`,
			"c.jsonnet": `[{apiVersion: "v1", kind: "ConfigMap", metadata: {name: "j" + i}} for i in std.range(1, 3)]`,
		})
		in := dirInput(root, nil)
		in.Limits.MaxManifests = 6
		assertEqual(t, len(generate(t, r, in).Manifests), 6)
		in.Limits.MaxManifests = 5
		if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
		in.Limits.MaxManifests = 2 // exceeded by the JSON file, before Jsonnet output
		if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
	})
	t.Run("jsonnet array", func(t *testing.T) {
		t.Parallel()
		r := newTestRenderer(t)
		root := tempRepo(t, map[string]string{
			"big.jsonnet": `[{apiVersion: "v1", kind: "ConfigMap", metadata: {name: "j" + i}} for i in std.range(1, 5000)]`,
		})
		in := dirInput(root, nil)
		in.Limits.MaxManifests = 100
		if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
	})
	t.Run("jsonnet object", func(t *testing.T) {
		t.Parallel()
		r := newTestRenderer(t)
		root := tempRepo(t, map[string]string{
			"a.yaml":    cm("a", nil),
			"o.jsonnet": `{apiVersion: "v1", kind: "List", items: [{apiVersion: "v1", kind: "ConfigMap", metadata: {name: "x" + i}} for i in std.range(1, 3)]}`,
		})
		in := dirInput(root, nil)
		in.Limits.MaxManifests = 3
		if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
	})
	t.Run("helm", func(t *testing.T) {
		t.Parallel()
		r := newTestRenderer(t, withHelm(t))
		root := tempRepo(t, map[string]string{
			"chart/Chart.yaml":         "apiVersion: v2\nname: many\nversion: 0.1.0\n",
			"chart/templates/cms.yaml": "{{- range until 3000 }}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-{{ . }}\n{{- end }}\n",
		})
		in := Input{RootDir: root, AppName: "many", Source: v1alpha1.ApplicationSource{Path: "chart"}}
		in.Limits.MaxManifests = 100
		if err := generateErr(t, r, in); !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
		in.Limits.MaxManifests = 3000
		assertEqual(t, len(generate(t, r, in).Manifests), 3000)
	})
	t.Run("plugin", func(t *testing.T) {
		t.Parallel()
		dir := pluginSocketDir(t)
		p := &fakePlugin{generate: func(pluginRequest) (cmpwire.ManifestResponse, *cmpwire.Error) {
			out := make([]string, 0, 50)
			for i := range 50 {
				out = append(out, fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: p%d\n---\n%s", i, tinyDocs(100)))
			}
			return cmpwire.ManifestResponse{Manifests: out}, nil
		}}
		p.serve(t, dir, "p")
		r := newTestRenderer(t, withPlugins(dir))
		in := pluginInput(t, pluginRepoFiles, &v1alpha1.ApplicationSourcePlugin{Name: "p"})
		in.Limits.MaxManifests = 1000
		err := generateErr(t, r, in)
		if !errors.Is(err, ErrTooManyManifests) {
			t.Fatalf("error = %v, want ErrTooManyManifests", err)
		}
		assertContains(t, err.Error(), "failed to convert CMP manifests to unstructured objects")
	})
}
