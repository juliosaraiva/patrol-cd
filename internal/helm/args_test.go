package helm_test

import (
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
)

func TestCleanSetParameter(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`val`:        `val`,
		`not, clean`: `not\, clean`,
		`a\,b,c`:     `a\,b\,c`,
		`{a,b,c}`:    `{a,b,c}`,
		`,,,,,\,`:    `\,\,\,\,\,\,`,
		`\,,\\,,`:    `\,\,\\,\,`,
		``:           ``,
		`{a,b`:       `{a\,b`,
	} {
		if got := helm.CleanSetParameter(in); got != want {
			t.Errorf("cleanSetParameter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanSetParameterProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		v := rapid.StringMatching(`[a-z,\\{} ]{0,16}`).Draw(t, "v")
		once := helm.CleanSetParameter(v)
		if twice := helm.CleanSetParameter(once); twice != once {
			t.Fatalf("not idempotent: %q -> %q -> %q", v, once, twice)
		}
		if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
			if once != v {
				t.Fatalf("list value changed: %q -> %q", v, once)
			}
			return
		}
		// Every comma is escaped.
		for i, r := range once {
			if r == ',' && (i == 0 || once[i-1] != '\\') {
				t.Fatalf("unescaped comma in %q (from %q)", once, v)
			}
		}
		if !strings.Contains(v, `\`) && strings.ReplaceAll(once, `\,`, ",") != v {
			t.Fatalf("escaping changed content: %q -> %q", v, once)
		}
	})
}

func TestDedupKVProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) helm.KV {
			return helm.KV{Name: rapid.SampledFrom([]string{"a", "b", "c", "d"}).Draw(t, "n"), Value: rapid.StringMatching(`[0-9]{1,2}`).Draw(t, "v")}
		}), 0, 10).Draw(t, "in")
		out := helm.DedupKV(in)
		last := map[string]string{}
		var order []string
		for _, kv := range in {
			if _, seen := last[kv.Name]; !seen {
				order = append(order, kv.Name)
			}
			last[kv.Name] = kv.Value
		}
		if len(out) != len(order) {
			t.Fatalf("dedup(%v) = %v", in, out)
		}
		for i, kv := range out {
			if kv.Name != order[i] || kv.Value != last[kv.Name] {
				t.Fatalf("dedup(%v) = %v", in, out)
			}
		}
	})
}

func TestFlattenValues(t *testing.T) {
	t.Parallel()
	out := map[string]string{}
	helm.FlattenValues(map[string]any{
		"foo":  map[string]any{"bar": "baz"},
		"list": []any{"a", map[string]any{"k": 1.0}, []any{true}},
		"num":  1.0,
		"nil":  nil,
		"e":    map[string]any{},
	}, out)
	want := map[string]string{"foo.bar": "baz", "list[0]": "a", "list[1].k": "1", "list[2][0]": "true", "num": "1", "nil": "<nil>"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("flatten = %v", out)
	}
}

func TestTemplateArgsOrder(t *testing.T) {
	t.Parallel()
	got := helm.TemplateArgs(helm.TemplateOpts{
		ReleaseName: "rel", Namespace: "ns",
		SetParams:            []helm.KV{{Name: "a", Value: "1"}},
		SetStringParams:      []helm.KV{{Name: "b", Value: "2"}},
		SetFileParams:        []helm.KV{{Name: "c", Value: "/f"}},
		ValueFiles:           []string{"/v1.yaml", "https://x/v2.yaml"},
		APIVersions:          []string{"x/v1"},
		SkipSchemaValidation: true, SkipTests: true,
	}, "1.30.0", "/tmp/extra.yaml")
	want := []string{"template", ".", "--name-template", "rel", "--namespace", "ns", "--kube-version", "1.30.0",
		"--set", "a=1", "--set-string", "b=2", "--set-file", "c=/f",
		"--values", "/v1.yaml", "--values", "https://x/v2.yaml", "--values", "/tmp/extra.yaml",
		"--api-versions", "x/v1", "--include-crds", "--skip-schema-validation", "--skip-tests"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n%q\nwant\n%q", got, want)
	}
}

func TestResolveReferenceURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ base, ref, want string }{
		{"https://charts.example.com", "mychart-1.0.0.tgz", "https://charts.example.com/mychart-1.0.0.tgz"},
		{"https://charts.example.com/sub", "mychart-1.0.0.tgz", "https://charts.example.com/sub/mychart-1.0.0.tgz"},
		{"https://charts.example.com/sub/", "charts/x.tgz", "https://charts.example.com/sub/charts/x.tgz"},
		{"https://charts.example.com/sub?token=1", "x.tgz", "https://charts.example.com/sub/x.tgz?token=1"},
		{"https://charts.example.com", "https://cdn.example.com/x.tgz", "https://cdn.example.com/x.tgz"},
		{"https://charts.example.com/a/b", "../x.tgz", "https://charts.example.com/a/x.tgz"},
	}
	for _, tt := range tests {
		got, err := helm.ResolveReferenceURL(tt.base, tt.ref)
		if err != nil || got != tt.want {
			t.Errorf("resolveReferenceURL(%q, %q) = %q, %v; want %q", tt.base, tt.ref, got, err, tt.want)
		}
	}
	if got := helm.RedactCommand("helm pull --username alice --password s3cret x"); got != "helm pull --username ****** --password ****** x" {
		t.Fatalf("RedactCommand = %q", got)
	}
}
