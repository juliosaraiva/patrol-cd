package cmp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"
	"sigs.k8s.io/yaml"
)

func TestSplitManifests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		want  []string
		error string
	}{
		{name: "empty", in: "", want: []string{}},
		{name: "separators and comments only", in: "---\n# comment\n---\n", want: []string{}},
		{name: "null documents", in: "null\n---\n~\n", want: []string{}},
		{
			name: "yaml stream",
			in:   "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: b\n",
			want: []string{
				`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
				`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"b"}}`,
			},
		},
		{
			name: "json stream",
			in:   `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}` + "\n" + `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"}}`,
			want: []string{
				`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
				`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"}}`,
			},
		},
		{name: "invalid yaml", in: "a: [\n", error: "failed to unmarshal manifest"},
		{name: "invalid json", in: "{", error: "failed to unmarshal manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := splitManifests([]byte(tt.in))
			if tt.error != "" {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				assertContains(t, err.Error(), tt.error)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				var a, b any
				if err := json.Unmarshal([]byte(got[i]), &a); err != nil {
					t.Fatalf("document %d is not JSON: %q", i, got[i])
				}
				_ = json.Unmarshal([]byte(tt.want[i]), &b)
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("document %d = %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSplitManifestsRoundTrip checks that any list of objects written as a YAML
// stream splits back into the same objects, in order.
func TestSplitManifestsRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(t, "n")
		var objs []map[string]any
		var docs []string
		for i := range n {
			data := rapid.MapOf(rapid.StringMatching(`[a-z][a-z0-9.-]{0,8}`), rapid.StringMatching(`[\p{L}\p{N} .:/_#{}"'-]{0,16}`)).Draw(t, fmt.Sprintf("data%d", i))
			obj := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": fmt.Sprintf("cm-%d", i)},
			}
			if len(data) > 0 {
				d := map[string]any{}
				for k, v := range data {
					d[k] = v
				}
				obj["data"] = d
			}
			out, err := yaml.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			objs = append(objs, obj)
			docs = append(docs, string(out))
		}
		got, err := splitManifests([]byte(strings.Join(docs, "---\n")))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(objs) {
			t.Fatalf("got %d documents, want %d", len(got), len(objs))
		}
		for i, s := range got {
			var m map[string]any
			if err := json.Unmarshal([]byte(s), &m); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m, objs[i]) {
				t.Fatalf("document %d = %v, want %v", i, m, objs[i])
			}
		}
	})
}
