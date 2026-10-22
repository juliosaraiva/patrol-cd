package main

import (
	"slices"
	"strings"
	"testing"
)

func TestParseAllowlist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		rules   int
		wantErr string
	}{
		{name: "empty", in: "", rules: 0},
		{name: "comments and blank lines", in: "# c\n\n   # indented comment\n\t\n", rules: 0},
		{name: "one rule", in: "pkg/a\tTestX\tgit not found\twhy\n", rules: 1},
		{name: "CRLF line ends", in: "pkg/a\tTestX\treason\twhy\r\n", rules: 1},
		{name: "tab inside the comment", in: "pkg/a\tTestX\treason\twhy\tand more\n", rules: 1},
		{name: "empty reason regexp", in: "pkg/a\tTestX\t\tSkipNow without a message\n", rules: 1},
		{name: "three fields", in: "pkg/a\tTestX\treason\n", wantErr: "f:1: want 4 tab-separated fields"},
		{name: "spaces instead of tabs", in: "pkg/a TestX reason why\n", wantErr: "got 1"},
		{name: "missing comment", in: "# x\npkg/a\tTestX\treason\t  \n", wantErr: "f:2: the comment"},
		{name: "empty package regexp", in: "\tTestX\treason\twhy\n", wantErr: "empty package regexp"},
		{name: "empty test regexp", in: "pkg\t\treason\twhy\n", wantErr: "empty test regexp"},
		{name: "bad regexp", in: "pkg\tTest(\treason\twhy\n", wantErr: "f:1: test regexp: error parsing regexp"},
		// A stray ")" would close the ^(?s:...)$ wrapper early: "x)|(.*" would match everything.
		{name: "unbalanced parenthesis in the package", in: "example\\.com/none)|(.*\tTestA\treason\twhy\n", wantErr: "f:1: package regexp: error parsing regexp: unexpected )"},
		{name: "unbalanced parenthesis in the test", in: "pkg\tTestA)|(.*\treason\twhy\n", wantErr: "f:1: test regexp: error parsing regexp: unexpected )"},
		{name: "unbalanced parenthesis in the reason", in: "pkg\tTestA\tr)|(.*\twhy\n", wantErr: "f:1: reason regexp: error parsing regexp: unexpected )"},
		{name: "balanced groups", in: "pkg/(a|b)\t(TestA|TestB)(/.*)?\t(x|y)\twhy\n", rules: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rules, err := parseAllowlist("f", strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) != tt.rules {
				t.Fatalf("got %d rules, want %d", len(rules), tt.rules)
			}
		})
	}
}

func TestAllowlistMatching(t *testing.T) {
	t.Parallel()
	rules, err := parseAllowlist("f", strings.NewReader(strings.Join([]string{
		"example\\.com/p\tTestA\tgit not found\tline 1",
		"example\\.com/.*\tTestB(/.*)?\tcapability \"[a-z]+\" not declared\tline 2",
		"example\\.com/p\tTestC\tfirst line.*\tline 3",
		"example\\.com/p\tTestD\t\tline 4",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	a := &allowlist{rules: rules}
	tests := []struct {
		name              string
		pkg, test, reason string
		want              []int
	}{
		{name: "exact", pkg: "example.com/p", test: "TestA", reason: "git not found", want: []int{1}},
		{name: "package is anchored", pkg: "example.com/pp", test: "TestA", reason: "git not found"},
		{name: "test is anchored", pkg: "example.com/p", test: "TestAB", reason: "git not found"},
		{name: "reason is anchored", pkg: "example.com/p", test: "TestA", reason: "git not found in PATH"},
		{name: "subtest", pkg: "example.com/q", test: "TestB/x/y", reason: `capability "paths" not declared`, want: []int{2}},
		{name: "parent", pkg: "example.com/q", test: "TestB", reason: `capability "paths" not declared`, want: []int{2}},
		{name: "dot matches newline", pkg: "example.com/p", test: "TestC", reason: "first line\nsecond line", want: []int{3}},
		{name: "empty reason", pkg: "example.com/p", test: "TestD", reason: "", want: []int{4}},
		{name: "empty reason rule needs empty reason", pkg: "example.com/p", test: "TestD", reason: "x"},
	}
	for _, tt := range tests {
		if got := a.match(tt.pkg, tt.test, tt.reason); !slices.Equal(got, tt.want) {
			t.Errorf("%s: match = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseMustRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		rules   int
		wantErr string
	}{
		{name: "empty, like /dev/null", in: "", rules: 0},
		{name: "two rules", in: "# c\nexample.com/p\tTestA\twhy\nexample.com/p\tTestB(/.*)?\twhy\n", rules: 2},
		{name: "two fields", in: "example.com/p\tTestA\n", wantErr: "f:1: want 3 tab-separated fields"},
		{name: "package regexp", in: "example.com/.*\tTestA\twhy\n", wantErr: "full import path"},
		{name: "missing comment", in: "example.com/p\tTestA\t\n", wantErr: "comment"},
		{name: "empty test", in: "example.com/p\t\twhy\n", wantErr: "empty test regexp"},
		{name: "bad regexp", in: "example.com/p\tTest[\twhy\n", wantErr: "test regexp"},
		{name: "unbalanced parenthesis", in: "example.com/p\tTestA)|(.*\twhy\n", wantErr: "f:1: test regexp: error parsing regexp: unexpected )"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rules, err := parseMustRun("f", strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) != tt.rules {
				t.Fatalf("got %d rules, want %d", len(rules), tt.rules)
			}
		})
	}
}

func TestAnchored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		expr    string
		match   []string
		noMatch []string
		wantErr bool
	}{
		{expr: "TestA|TestAB", match: []string{"TestA", "TestAB"}, noMatch: []string{"TestABC", "xTestA"}},
		{expr: "a.*b", match: []string{"a\nb"}, noMatch: []string{"a\nbc"}},
		{expr: "x)|(.*", wantErr: true},
		{expr: `\Qx`, wantErr: true},
	}
	for _, tt := range tests {
		re, err := anchored(tt.expr)
		if (err != nil) != tt.wantErr {
			t.Errorf("anchored(%q) error %v, want error %v", tt.expr, err, tt.wantErr)
			continue
		}
		for _, s := range tt.match {
			if !re.MatchString(s) {
				t.Errorf("%q does not match %q", tt.expr, s)
			}
		}
		for _, s := range tt.noMatch {
			if re.MatchString(s) {
				t.Errorf("%q matches %q", tt.expr, s)
			}
		}
	}
}

// A suggested line must parse and allow exactly the skip it was made for.
func TestSuggestRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, pkg, testExpr, test, reason string
	}{
		{name: "plain", pkg: "github.com/x/internal/git", testExpr: "TestLFS", test: "TestLFS", reason: "git-lfs not installed"},
		{name: "metacharacters", pkg: "example.com/p", testExpr: `TestA/case_\(1\)`, test: "TestA/case_(1)", reason: `capability "x" not declared [a+b]`},
		{name: "multi-line", pkg: "example.com/p", testExpr: "TestA", test: "TestA", reason: "sshd rejects test configuration: line 1\nline 2\nline 3"},
		{name: "tab in reason", pkg: "example.com/p", testExpr: "TestA", test: "TestA", reason: "a\tb"},
		{name: "carriage returns in reason", pkg: "example.com/p", testExpr: "TestA", test: "TestA", reason: "a\r::error::x\r## y"},
		{name: "empty reason", pkg: "example.com/p", testExpr: "TestA", test: "TestA", reason: ""},
		{name: "group", pkg: "example.com/p", testExpr: "TestA(/.*)?", test: "TestA/sub", reason: "r"},
		{name: "long first line", pkg: "example.com/p", testExpr: "TestA", test: "TestA", reason: strings.Repeat("é", 400) + "\nsecond line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			line := suggestRule(tt.pkg, tt.testExpr, tt.reason)
			if strings.ContainsAny(line, "\r\n") {
				t.Fatalf("suggestion spans lines: %q", line)
			}
			if len(line) > 2*suggestMaxBytes+len(tt.pkg)+len(tt.testExpr)+100 {
				t.Fatalf("suggestion is %d bytes long", len(line))
			}
			rules, err := parseAllowlist("suggestion", strings.NewReader(line))
			if err != nil {
				t.Fatalf("suggestion %q does not parse: %v", line, err)
			}
			a := &allowlist{rules: rules}
			if got := a.match(tt.pkg, tt.test, tt.reason); len(got) != 1 {
				t.Fatalf("suggestion %q does not match its own skip", line)
			}
			if got := a.match(tt.pkg+"x", tt.test, tt.reason); len(got) != 0 {
				t.Fatalf("suggestion %q matches another package", line)
			}
		})
	}
}
