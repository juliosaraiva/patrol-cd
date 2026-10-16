package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain":                "plain",
		"tab\tand\nnewline":    "tab and newline",
		"\x1b[31mred\x1b[0m":   " [31mred [0m",
		"bell\a":               "bell ",
		"rtl\u202eoverride":    "rtl override",
		"isolate\u2066x\u2069": "isolate x ",
		"line\u2028sep\u2029":  "line sep ",
		"unicode ünïcødé 日本":   "unicode ünïcødé 日本",
		"del\x7f":              "del ",
		"c1\u0085":             "c1 ",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeMultiline("a\nb\x1b\tc"); got != "a\nb  c" {
		t.Errorf("SanitizeMultiline = %q", got)
	}
}

func TestPrinters(t *testing.T) {
	t.Parallel()
	type item struct {
		Name string `json:"name"`
	}
	var b bytes.Buffer
	if err := PrintResourceList(&b, []item{}, JSON, false); err != nil || b.String() != "[]\n" {
		t.Fatalf("empty json list: %q %v", b.String(), err)
	}
	b.Reset()
	if err := PrintResourceList(&b, []item{{"a"}}, YAML, true); err != nil || b.String() != "name: a\n" {
		t.Fatalf("single yaml: %q %v", b.String(), err)
	}
	b.Reset()
	if err := PrintResourceList(&b, []item{{"a"}}, JSON, false); err != nil || b.String() != "[\n  {\n    \"name\": \"a\"\n  }\n]\n" {
		t.Fatalf("json list: %q %v", b.String(), err)
	}
	if err := PrintResource(&b, item{}, "toml"); err == nil || err.Error() != "unknown output format: toml" {
		t.Fatalf("unknown: %v", err)
	}
	if err := CheckFormat("x", JSON, YAML); err == nil || err.Error() != "unknown output format: x. Supported formats: json|yaml" {
		t.Fatalf("check: %v", err)
	}
	if CheckFormat(YAML, JSON, YAML) != nil {
		t.Fatal("yaml rejected")
	}
}

func TestTableAndPrint(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	tab := NewTable(&b)
	tab.Row("NAME", "VALUE")
	tab.Row("long-name", "x\ty")
	tab.Row("a", 3)
	if err := tab.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "NAME       VALUE\nlong-name  x y\na          3\n"
	if b.String() != want {
		t.Fatalf("table:\n%q\nwant\n%q", b.String(), want)
	}
	b.Reset()
	Printf(&b, "%s=%d %v\n", "k\x1b", 1, true)
	Println(&b, "line\nbreak")
	if b.String() != "k =1 true\nline break\n" {
		t.Fatalf("print: %q", b.String())
	}
	if JoinOrNone(nil, ",") != "<none>" || JoinOrNone([]string{"a", "b"}, ",") != "a,b" || OrDash("") != "-" || OrDash("x") != "x" {
		t.Fatal("helpers")
	}
	if strings.Contains(UnknownFormat("x").Error(), "Supported") {
		t.Fatal("UnknownFormat without list")
	}
}
