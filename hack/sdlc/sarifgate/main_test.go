package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeBaseline(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "baseline.txt")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	return p
}

func TestReadSARIFResolvesRules(t *testing.T) {
	t.Parallel()
	rs, err := readSARIF("testdata/sample.sarif")
	require.NoError(t, err)
	require.Len(t, rs, 4)
	assert.Equal(t, result{Tool: "CodeQL", Rule: "go/path-injection", Level: "error", Severity: 7.5, Path: "internal/a/a.go", Line: 12,
		Message: "This path depends on a user-provided value.\nSecond line.", Fingerprint: "aaa:1"}, rs[0], "level from the rule's default configuration")
	assert.Equal(t, "warning", rs[1].Level, "the result's own level")
	assert.Equal(t, "note", rs[2].Level)
	assert.True(t, rs[3].Suppressed)

	rs, err = readSARIF("testdata/levels.sarif")
	require.NoError(t, err)
	require.Len(t, rs, 5)
	assert.Equal(t, "warning", rs[0].Level, "the result's level wins over the rule's default (note)")
	assert.Equal(t, "warning", rs[1].Level, "no level anywhere: SARIF's default")
	assert.Equal(t, "error", rs[2].Level, "no level of its own: the driver rule's default")
	assert.Equal(t, "none", rs[3].Level)
	assert.InDelta(t, 9.1, rs[4].Severity, 1e-9, "a security-severity given as a number")

	rs, err = readSARIF("testdata/suppressions.sarif")
	require.NoError(t, err)
	require.Len(t, rs, 5)
	for i, want := range []bool{true, true, false, false, true} {
		assert.Equal(t, want, rs[i].Suppressed, rs[i].Message)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sarifs   []string // default: testdata/sample.sarif
		baseline []string
		wantCode int
		want     []string // in this order
		notWant  []string
		wantErr  string
	}{
		{
			name:     "no baseline: the error and the warning block, listed first",
			wantCode: 1,
			want: []string{
				"CodeQL gate: FAILED: 4 results, 2 blocking, 0 accepted in the baseline, 1 suppressed in the source, 1 notes",
				"fail       error   go/path-injection internal/a/a.go:12 (security-severity 7.5)",
				"           fingerprint aaa:1",
				"fail       warning go/unhandled-writable-file-close internal/b/b.go:3",
				"suppressed error   go/log-injection internal/d/d.go:40",
				"info       note    go/useless-assignment internal/c/c.go:7",
			},
			notWant: []string{"Second line."},
		},
		{
			name:     "within a state, by path, then line",
			sarifs:   []string{"testdata/order.sarif"},
			wantCode: 1,
			want: []string{
				"fail       warning x/r a.go:2",
				"fail       warning x/r a.go:9",
				"fail       warning x/r b.go:5",
			},
		},
		{
			name:     "several SARIF files, one per language: all of them are gated",
			sarifs:   []string{"testdata/sample.sarif", "testdata/levels.sarif"},
			wantCode: 1,
			want: []string{
				"CodeQL gate: FAILED: 9 results, 6 blocking",
				"fail       warning x/note-by-default a.go:1",
				"fail       warning x/no-default b.go:2",
				"fail       error   x/error-by-default c.go:3",
				"fail       error   x/numeric-severity e.go:5 (security-severity 9.1)",
				"info       none    x/no-default d.go:4", // level none blocks nothing
			},
		},
		{
			name: "both accepted",
			baseline: []string{
				"# accepted alerts",
				"",
				"   ",
				"go/path-injection\tinternal/a/a.go\taaa:1\tthe path is joined under a checked root",
				"go/unhandled-writable-file-close\tinternal/b/b.go\tbbb:1\tread-only file",
			},
			wantCode: 0,
			want: []string{
				"CodeQL gate: OK: 4 results, 0 blocking, 2 accepted in the baseline",
				"accepted   error   go/path-injection internal/a/a.go:12",
				"           accepted: the path is joined under a checked root",
				"accepted   warning go/unhandled-writable-file-close internal/b/b.go:3",
				"suppressed error   go/log-injection internal/d/d.go:40",
				"info       note    go/useless-assignment internal/c/c.go:7",
			},
			// Only a failing result prints its fingerprint, to be copied into the baseline.
			notWant: []string{"unused baseline line", "fingerprint aaa:1", "fingerprint bbb:1", "fingerprint ccc:1", "fingerprint ddd:1"},
		},
		{
			name: "a line for another file does not accept, and is reported as unused",
			baseline: []string{
				"go/path-injection\tinternal/a/other.go\taaa:1\twrong file",
				"go/unhandled-writable-file-close\tinternal/b/b.go\tbbb:1\tread-only file",
			},
			wantCode: 1,
			want: []string{
				"1 blocking, 1 accepted",
				"fail       error   go/path-injection internal/a/a.go:12",
				"accepted   warning go/unhandled-writable-file-close internal/b/b.go:3",
				"unused baseline line 1: go/path-injection internal/a/other.go aaa:1",
			},
			notWant: []string{"unused baseline line 2"},
		},
		{
			name: "a line for another fingerprint does not accept",
			baseline: []string{
				"go/path-injection\tinternal/a/a.go\tzzz:1\tanother alert in the same file",
				"go/unhandled-writable-file-close\tinternal/b/b.go\tbbb:1\tread-only file",
			},
			wantCode: 1,
			want:     []string{"1 blocking, 1 accepted", "unused baseline line 1: go/path-injection internal/a/a.go zzz:1"},
		},
		{
			name: "a line for another rule does not accept",
			baseline: []string{
				"go/sql-injection\tinternal/a/a.go\taaa:1\tanother rule at the same place",
				"go/unhandled-writable-file-close\tinternal/b/b.go\tbbb:1\tread-only file",
			},
			wantCode: 1,
			want:     []string{"1 blocking, 1 accepted", "unused baseline line 1: go/sql-injection internal/a/a.go aaa:1"},
		},
		{
			name: "an unused line is reported but does not fail the run",
			baseline: []string{
				"go/path-injection\tinternal/a/a.go\taaa:1\tjoined under a checked root",
				"go/unhandled-writable-file-close\tinternal/b/b.go\tbbb:1\tread-only file",
				"go/old-rule\tinternal/gone.go\tggg:1\tfixed since",
			},
			wantCode: 0,
			want:     []string{"CodeQL gate: OK", "unused baseline line 3: go/old-rule internal/gone.go ggg:1"},
		},
		{
			name:     "a baseline line with three columns is rejected",
			baseline: []string{"go/path-injection\tinternal/a/a.go\taaa:1\t"},
			wantCode: 2,
			wantErr:  "want 4 non-empty tab-separated columns",
		},
		{
			name:     "a baseline line with an empty column is rejected",
			baseline: []string{"go/path-injection\t\taaa:1\tno path"},
			wantCode: 2,
			wantErr:  "want 4 non-empty tab-separated columns",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := []string{}
			if tt.baseline != nil {
				args = append(args, "-baseline", writeBaseline(t, tt.baseline...))
			}
			summary := filepath.Join(t.TempDir(), "summary.txt")
			sarifs := tt.sarifs
			if sarifs == nil {
				sarifs = []string{"testdata/sample.sarif"}
			}
			args = append(args, "-summary", summary)
			args = append(args, sarifs...)
			var out, errOut bytes.Buffer
			code := run(args, &out, &errOut)
			require.Equal(t, tt.wantCode, code, "stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
			rest := out.String()
			for _, w := range tt.want {
				i := strings.Index(rest, w)
				if !assert.GreaterOrEqual(t, i, 0, "%q, after the lines before it, in:\n%s", w, out.String()) {
					break
				}
				rest = rest[i+len(w):]
			}
			for _, w := range tt.notWant {
				assert.NotContains(t, out.String(), w)
			}
			assert.Contains(t, errOut.String(), tt.wantErr)
			if code != 2 {
				written, err := os.ReadFile(summary)
				require.NoError(t, err)
				assert.Equal(t, out.String(), string(written))
			}
		})
	}
}

// A summary it cannot write, or a baseline it cannot read to the end, stops the gate.
func TestRunWriteAndScanErrors(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	missingDir := filepath.Join(t.TempDir(), "missing", "summary.txt")
	assert.Equal(t, 2, run([]string{"-summary", missingDir, "testdata/sample.sarif"}, &out, &errOut))
	assert.Contains(t, errOut.String(), missingDir)

	errOut.Reset()
	long := writeBaseline(t, "go/path-injection\tinternal/a/a.go\taaa:1\t"+strings.Repeat("x", 70_000))
	assert.Equal(t, 2, run([]string{"-baseline", long, "testdata/sample.sarif"}, &out, &errOut))
	assert.Contains(t, errOut.String(), "token too long")
}

func TestRunNeedsInput(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	assert.Equal(t, 2, run(nil, &out, &errOut))
	assert.Equal(t, 2, run([]string{"testdata/missing.sarif"}, &out, &errOut))
	assert.Contains(t, errOut.String(), "testdata/missing.sarif")
}

// A SARIF file that does not parse stops the gate; it never counts as zero results.
func TestRunMalformedSARIF(t *testing.T) {
	t.Parallel()
	for _, content := range []string{`{"version": "2.1.0", "runs": [`, `not json`, `{}`, `{"version":"2.1.0","runs":[]}`, `{"version":"2.1.0","runs":[{"tool":{"driver":{}}}]}`} {
		p := filepath.Join(t.TempDir(), "broken.sarif")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		var out, errOut bytes.Buffer
		assert.Equal(t, 2, run([]string{"testdata/sample.sarif", p}, &out, &errOut), "stdout:\n%s", out.String())
		assert.Contains(t, errOut.String(), p)
		assert.Empty(t, out.String(), "no report, passing or not, from a file it cannot read")
	}
	// A run with a driver but another or no SARIF version is refused for its version.
	for _, content := range []string{`{"version":"2.0.0","runs":[{"tool":{"driver":{"name":"CodeQL"}}}]}`, `{"runs":[{"tool":{"driver":{"name":"CodeQL"}}}]}`} {
		p := filepath.Join(t.TempDir(), "version.sarif")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		var out, errOut bytes.Buffer
		assert.Equal(t, 2, run([]string{p}, &out, &errOut), "stdout:\n%s", out.String())
		assert.Contains(t, errOut.String(), "want 2.1.0")
	}
}
