//go:build unix

package sandbox

import (
	"bytes"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestHelperArgsRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		spec := helperSpec{
			cpu:    rapid.Uint64().Draw(rt, "cpu"),
			as:     rapid.Uint64().Draw(rt, "as"),
			nofile: rapid.Uint64().Draw(rt, "nofile"),
			path:   "/" + rapid.StringMatching(`[a-z/]{0,12}`).Draw(rt, "path"),
			// Arbitrary argv, including "--", "x=1" and empty strings.
			argv: rapid.SliceOfN(rapid.String(), 1, 6).Draw(rt, "argv"),
		}
		args := encodeHelperArgs("/helper", spec)
		require.Equal(rt, "/helper", args[0])
		require.Equal(rt, HelperArg, args[1])
		got, err := decodeHelperArgs(args[2:])
		require.NoError(rt, err)
		require.Equal(rt, spec, got)
	})
}

func TestDecodeHelperArgsRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"empty", nil},
		{"missing separator", []string{"cpu=1", "as=0", "nofile=0", "/bin/sh", "sh"}},
		{"missing limit", []string{"cpu=1", "as=0", "--", "/bin/sh", "sh"}},
		{"duplicate limit", []string{"cpu=1", "cpu=2", "as=0", "nofile=0", "--", "/bin/sh", "sh"}},
		{"unknown limit", []string{"cpu=1", "as=0", "nofile=0", "mem=1", "--", "/bin/sh", "sh"}},
		{"not a number", []string{"cpu=x", "as=0", "nofile=0", "--", "/bin/sh", "sh"}},
		{"negative", []string{"cpu=-1", "as=0", "nofile=0", "--", "/bin/sh", "sh"}},
		{"no equals", []string{"cpu", "as=0", "nofile=0", "--", "/bin/sh", "sh"}},
		{"relative target", []string{"cpu=1", "as=0", "nofile=0", "--", "sh", "sh"}},
		{"missing argv0", []string{"cpu=1", "as=0", "nofile=0", "--", "/bin/sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeHelperArgs(tt.args)
			require.ErrorIs(t, err, ErrInvalidCommand)
		})
	}
}

func TestHelperMainFailsClosed(t *testing.T) {
	t.Parallel()
	valid := []string{"cpu=1", "as=0", "nofile=0", "--", "/bin/true", "true"}
	tests := []struct {
		name       string
		args       []string
		withStatus bool
	}{
		{"bad arguments", []string{"bogus"}, true},
		{"relative target", []string{"cpu=1", "as=0", "nofile=0", "--", "relative", "x"}, true},
		// Not started by Run (no status pipe): never exec, even with valid arguments.
		{"no status descriptor", valid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var status *os.File
			var r *os.File
			if tt.withStatus {
				var err error
				r, status, err = os.Pipe()
				require.NoError(t, err)
				defer func() { _ = r.Close() }()
			}
			assert.Equal(t, helperExitSetup, helperMain(tt.args, status))
			if status != nil {
				require.NoError(t, status.Close())
				got, err := io.ReadAll(r)
				require.NoError(t, err)
				// A failing helper reports failure, never readiness.
				assert.Equal(t, []byte{helperStatusFailed}, got)
			}
		})
	}
}

func TestLowerRlimitHandlesExtremes(t *testing.T) {
	t.Parallel()
	// Runs in a helper so the test process keeps its own limits.
	c := sh(`ulimit -t`)
	c.Limits.CPUSeconds = math.MaxUint64
	res, err := Run(t.Context(), c)
	require.NoError(t, err, "stderr: %s", res.Stderr)
	assert.NotEmpty(t, strings.TrimSpace(string(res.Stdout)))
}

// TestCaptureProperty: for any input split into arbitrary reads, capture keeps
// exactly the first min(limit, len) bytes and reports overflow iff len > limit.
func TestCaptureProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		data := rapid.SliceOfN(rapid.Byte(), 0, 4096).Draw(rt, "data")
		limit := rapid.Int64Range(-1, 5000).Draw(rt, "limit")
		var r io.Reader = bytes.NewReader(data)
		if rapid.Bool().Draw(rt, "oneByte") {
			r = iotest.OneByteReader(r)
		} else if rapid.Bool().Draw(rt, "halfReader") {
			r = iotest.HalfReader(r)
		}
		overflows := 0
		c := &capture{limit: limit}
		c.readFrom(r, func() { overflows++ })

		want := data
		wantOverflow := limit >= 0 && int64(len(data)) > limit
		if wantOverflow {
			want = data[:limit]
		}
		require.True(rt, bytes.Equal(want, c.bytes()), "captured %q, want %q", c.bytes(), want)
		require.Equal(rt, wantOverflow, c.exceeded())
		require.Equal(rt, wantOverflow, overflows > 0)
	})
}

func TestRedactString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		items []string
		want  string
	}{
		{"token abc123 used", []string{"abc123"}, "token ****** used"},
		{"no secrets", nil, "no secrets"},
		{"empty item ignored", []string{""}, "empty item ignored"},
		{"pass=abcdef", []string{"abc", "abcdef"}, "pass=******"},
		{"a-b-a", []string{"a"}, "******-b-******"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, redactString(tt.in, tt.items), tt.in)
	}
}
