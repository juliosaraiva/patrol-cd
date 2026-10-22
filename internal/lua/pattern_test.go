package lua

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/gopher-lua/pm"
	"pgregory.net/rapid"
)

// pmFind runs gopher-lua's matcher (the reference) and normalizes its result: one slot
// slice per match, or the error text (including Go runtime panics, which gopher-lua
// reports as script errors).
func pmFind(p, src string, init, limit int) (res [][]int, errText string) {
	defer func() {
		if r := recover(); r != nil {
			res, errText = nil, fmt.Sprint(r)
		}
	}()
	mds, err := pm.Find(p, []byte(src), init, limit)
	if err != nil {
		return nil, err.Error()
	}
	for _, md := range mds {
		slots := make([]int, md.CaptureLength())
		for i := range slots {
			slots[i] = md.Capture(i) << 1
			if md.IsPosCapture(i) {
				slots[i] |= 1
			}
		}
		res = append(res, slots)
	}
	return res, ""
}

func ourFind(ctx context.Context, p, src string, init, limit int) ([][]int, string) {
	pat, err := compileLuaPattern(p)
	if err != nil {
		return nil, err.Error()
	}
	var res [][]int
	it := newPatIter(ctx, pat, src, init, limit)
	for {
		caps, err := it.next()
		if err != nil {
			return nil, err.Error()
		}
		if caps == nil {
			return res, ""
		}
		res = append(res, slices.Clone([]int(caps)))
	}
}

func TestPatternMatchesGopherLua(t *testing.T) {
	t.Parallel()
	cases := []struct{ pattern, subject string }{
		{"(%d+)/(%d+)", "3/5"},
		{"(%d+)/(%d+)", "replicas 12/20 ready"},
		{"(%d+)-(%d+)-(%d+)T(%d+):(%d+):(%d+)Z", "2024-01-02T03:04:05Z"},
		{"^Provisioning", "Provisioning foo"},
		{"^Provisioning", "x Provisioning"},
		{"ERROR, ", "ERROR, bad ERROR, worse"},
		{"%b()", "f(a(b)c) (d"},
		{"%b", "abc"},
		{"%bx", "xabc"},
		{"%f[%w]%w+", "hello world"},
		{"[%a-z]", "-q"},
		{"[a-]", "x-a"},
		{"[]]", "a]"},
		{"[^]]", "]]x"},
		{"[a-c-e]", "d-e"},
		{"[%d-z]", "5z-"},
		{"^(a)$", "a"},
		{"a$b", "a$b"},
		{"()a()", "bab"},
		{"((a)%1)", "xaa"},
		{"(a)%1", "aa"},
		{"%1", "a"},
		{"%", "a%"},
		{"abc%", "abc"},
		{"(", "("},
		{"(()", "x"},
		{")", "x"},
		{"a)", "a"},
		{"[", "["},
		{"[a", "a"},
		{"[^", "a"},
		{"%0", "0"},
		{".-b", "aaab"},
		{"a*", "aaa"},
		{"a+?", "aa?"},
		{"**", "**"},
		{"^", "abc"},
		{"$", "abc"},
		{"^$", ""},
		{"", "abc"},
		{"x*", ""},
		{"%w*", "abc"},
		{"(a*(.)%w(%s*))", "aaab  c"},
		{"%s*(%S+)%s*$", "  trailing  "},
	}
	for _, tc := range cases {
		for _, limit := range []int{-1, 0, 1, 2} {
			for init := 0; init <= len(tc.subject)+1; init++ {
				want, wantErr := pmFind(tc.pattern, tc.subject, init, limit)
				got, gotErr := ourFind(context.Background(), tc.pattern, tc.subject, init, limit)
				require.Equal(t, wantErr, gotErr, "pattern %q subject %q init %d limit %d", tc.pattern, tc.subject, init, limit)
				require.Equal(t, want, got, "pattern %q subject %q init %d limit %d", tc.pattern, tc.subject, init, limit)
			}
		}
	}
}

// TestPatternParityProperty compares the matcher with gopher-lua's on generated
// patterns built from every pattern metacharacter.
func TestPatternParityProperty(t *testing.T) {
	t.Parallel()
	patAlphabet := []byte("%[]^$().*+-?abc019fxADSw")
	srcAlphabet := []byte("abc019 ()[]-x%$^A")
	rapid.Check(t, func(t *rapid.T) {
		p := string(rapid.SliceOfN(rapid.SampledFrom(patAlphabet), 0, 9).Draw(t, "pattern"))
		src := string(rapid.SliceOfN(rapid.SampledFrom(srcAlphabet), 0, 14).Draw(t, "subject"))
		init := rapid.IntRange(0, len(src)+1).Draw(t, "init")
		limit := rapid.IntRange(-1, 3).Draw(t, "limit")
		want, wantErr := pmFind(p, src, init, limit)
		got, gotErr := ourFind(context.Background(), p, src, init, limit)
		if wantErr != gotErr {
			t.Fatalf("pattern %q subject %q: error %q, gopher-lua %q", p, src, gotErr, wantErr)
		}
		if !slices.EqualFunc(want, got, slices.Equal[[]int]) {
			t.Fatalf("pattern %q subject %q init %d limit %d: got %v, gopher-lua %v", p, src, init, limit, got, want)
		}
	})
}

func TestPatternDepthLimitMatchesGopherLua(t *testing.T) {
	if testing.Short() {
		t.Skip("deep recursion in the reference matcher")
	}
	t.Parallel()
	// ".*" recurses once per consumed byte; the limit applies at the same subject length.
	for _, n := range []int{maxPatternDepth - 3, maxPatternDepth - 2, maxPatternDepth - 1} {
		src := strings.Repeat("a", n)
		want, wantErr := pmFind(".*", src, 0, 1)
		got, gotErr := ourFind(context.Background(), ".*", src, 0, 1)
		require.Equal(t, wantErr, gotErr, "length %d", n)
		require.Equal(t, want, got, "length %d", n)
	}
	_, gotErr := ourFind(context.Background(), ".*", strings.Repeat("a", maxPatternDepth), 0, 1)
	assert.Equal(t, "pattern/input too complex", gotErr)
}

// TestPatternHonorsContext is the regression test for slow matches: the matcher stops
// when the context ends instead of running to completion.
func TestPatternHonorsContext(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ pattern, subject string }{
		"quadratic digits":  {"(%d+)/(%d+)", strings.Repeat("1", 200_000)},
		"cubic lazy":        {".-.-.-b", strings.Repeat("a", 5_000)},
		"balanced scan":     {"%b()", strings.Repeat("(", 200_000)},
		"long back ref":     {"(a+)%1b", strings.Repeat("a", 100_000)},
		"anchored no limit": {"^(%d+)/(%d+)$", strings.Repeat("9", 1_000_000)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, errText := ourFind(ctx, tc.pattern, tc.subject, 0, -1)
			elapsed := time.Since(start)
			if errText == "" {
				// Finishing quickly is fine too; it must not overrun.
				assert.Less(t, elapsed, 2*time.Second)
				return
			}
			// Stopping early is the point: on a fast machine the matcher can reach its depth
			// limit before the deadline, on a slow one the deadline comes first.
			assert.Contains(t, []string{context.DeadlineExceeded.Error(), "pattern/input too complex"}, errText)
			assert.Less(t, elapsed, 2*time.Second)
		})
	}
	// Compile errors are independent of the context.
	_, err := compileLuaPattern("(")
	var pe *patternError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "unfinished capture at EOS", pe.Error())
}
