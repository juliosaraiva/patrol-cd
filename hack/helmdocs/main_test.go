package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func TestParseValueDocs(t *testing.T) {
	t.Parallel()
	in := `# File header, not a doc comment.
# -- Top-level key.
top: 1
global:
  # -- Registry
  # continued on a second line.
  imageRegistry: ""
  undocumented: true
  # -- A block scalar.
  # @default -- see values.yaml
  text: |
    # not a comment, block content
    key: value
  # -- After the block.
  after: [a]
configs:
  cm:
    # Plain comment.
    # -- Dotted key.
    timeout.reconciliation: 180s
    "quoted.key": x
list:
  - name: a
    # -- inside a list item: ignored
    value: 1
`
	docs, err := parseValueDocs([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]valueDoc{}
	for _, d := range docs {
		got[displayPath(d.Path)] = d
	}
	want := map[string]string{
		"top":                                 "Top-level key.",
		"global.imageRegistry":                "Registry continued on a second line.",
		"global.text":                         "A block scalar.",
		"global.after":                        "After the block.",
		`configs.cm."timeout.reconciliation"`: "Dotted key.",
	}
	if len(got) != len(want) {
		t.Fatalf("documented keys = %v, want %v", keys(got), want)
	}
	for k, desc := range want {
		d, ok := got[k]
		if !ok {
			t.Fatalf("missing %s in %v", k, keys(got))
		}
		if d.Description != desc {
			t.Errorf("%s description = %q, want %q", k, d.Description, desc)
		}
	}
	if got["global.text"].DefaultOverride != "see values.yaml" {
		t.Errorf("@default not parsed: %+v", got["global.text"])
	}
}

func TestParseValueDocsMisindentedComment(t *testing.T) {
	t.Parallel()
	_, err := parseValueDocs([]byte("a:\n  # -- doc\nb: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "indented") {
		t.Fatalf("err = %v, want an indentation error", err)
	}
}

func keys(m map[string]valueDoc) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRenderDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"x<y", "`\"x<y\"`"},
		{true, "`true`"},
		{float64(3), "`3`"},
		{map[string]any{"b": 1.0, "a": "z"}, "`{\"a\":\"z\",\"b\":1}`"},
		{strings.Repeat("x", maxDefaultLen), "see values.yaml"},
	} {
		got, err := renderDefault(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("renderDefault(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestOrderedJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := []byte(`{
  "z": 1,
  "a": {
    "y": [],
    "b": [
      "x<&>",
      2.50,
      null,
      true
    ],
    "e": {}
  }
}
`)
	n, err := parseJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	n.encode(&b, "")
	b.WriteString("\n")
	if !bytes.Equal(b.Bytes(), in) {
		t.Fatalf("round trip changed the document:\n%s", b.String())
	}
	if _, err := parseJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate keys accepted")
	}
}

func TestTemplateCRDRejectsUnexpectedLayout(t *testing.T) {
	t.Parallel()
	good := "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  annotations:\n    a: b\n  name: x.cd.patrol.example\nspec:\n  group: cd.patrol.example\n"
	out, err := templateCRD(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"{{- if .Values.crds.install }}\n", "helm.sh/resource-policy: keep", "app.kubernetes.io/part-of: patrol-cd", "  name: x.cd.patrol.example\nspec:\n", "{{- end }}\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("templated CRD lacks %q:\n%s", want, out)
		}
	}
	if _, err := templateCRD(strings.Replace(good, "  annotations:\n    a: b\n", "", 1)); err == nil {
		t.Error("CRD without annotations accepted")
	}
	if _, err := templateAdmissionPolicy("kind: ValidatingAdmissionPolicy\nmetadata:\n  name: p\n"); err == nil {
		t.Error("policy without labels accepted")
	}
}

func TestRenderCRDTemplatesRejectsDelimiters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  annotations:\n    a: '{{ x }}'\n  name: x.cd.patrol.example\nspec: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "cd.patrol.example_x.yaml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renderCRDTemplates(dir); err == nil || !strings.Contains(err.Error(), "template delimiters") {
		t.Fatalf("err = %v, want a delimiter error", err)
	}
}

func TestSchemaKeySetsFollowSettings(t *testing.T) {
	t.Parallel()
	keys := acceptedParamKeys()
	if len(keys) == 0 {
		t.Fatal("no parameter keys")
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "redis.") {
			t.Errorf("not-applicable key %s accepted", k)
		}
	}
	for _, s := range settings.ParamSpecs() {
		if usable(s) {
			found := false
			for _, k := range keys {
				found = found || k == s.Key
			}
			if !found {
				t.Errorf("usable key %s missing", s.Key)
			}
		}
	}
	if _, err := patrolCMSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := patrolRBACCMSchema(); err != nil {
		t.Fatal(err)
	}
}

// TestChartIsCurrent runs the generator in check mode against the repository's chart: it fails
// when a generated file is stale or a shipped default drifted from internal/settings.
func TestChartIsCurrent(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	chart := filepath.Join(root, "deploy", "helm", "patrol-cd")
	if _, err := os.Stat(filepath.Join(chart, "values.yaml")); err != nil {
		t.Skipf("chart not found: %v", err)
	}
	if err := run(chart, filepath.Join(root, "deploy", "crds"), true); err != nil {
		t.Fatal(err)
	}
}

// TestGenerationIsDeterministic renders every generated file twice.
func TestGenerationIsDeterministic(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	chart := filepath.Join(root, "deploy", "helm", "patrol-cd")
	values, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Skipf("chart not found: %v", err)
	}
	schema, err := os.ReadFile(filepath.Join(chart, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r1, err := renderReadme(chart, values)
		if err != nil {
			t.Fatal(err)
		}
		r2, _ := renderReadme(chart, values)
		p1, _ := renderParamsTemplate()
		p2, _ := renderParamsTemplate()
		s1, _ := renderSchema(schema)
		s2, _ := renderSchema(schema)
		if !bytes.Equal(r1, r2) || !bytes.Equal(p1, p2) || !bytes.Equal(s1, s2) {
			t.Fatal("generation is not deterministic")
		}
	}
}
