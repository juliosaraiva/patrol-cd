package git

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaT = "dddddddddddddddddddddddddddddddddddddddd"
)

func TestParseLsRemote(t *testing.T) {
	t.Parallel()
	out := "ref: refs/heads/main\tHEAD\n" +
		shaA + "\tHEAD\n" +
		shaA + "\trefs/heads/main\n" +
		shaB + "\trefs/heads/v1.0\n" +
		shaT + "\trefs/tags/v1.0\n" +
		shaC + "\trefs/tags/v1.0^{}\n" +
		shaB + "\trefs/tags/light\n" +
		shaC + "\trefs/pull/7/head\n"
	list, err := parseLsRemote([]byte(out))
	require.NoError(t, err)

	got, ok := list.resolve("HEAD")
	require.True(t, ok)
	assert.Equal(t, shaA, got)
	got, ok = list.resolve("v1.0")
	require.True(t, ok)
	assert.Equal(t, shaB, got, "first match in listing order (branch before tag) wins, as upstream")
	got, ok = list.resolve("refs/tags/v1.0")
	require.True(t, ok)
	assert.Equal(t, shaC, got, "annotated tag resolves to the peeled commit")
	got, ok = list.resolve("pull/7/head")
	require.True(t, ok)
	assert.Equal(t, shaC, got)
	_, ok = list.resolve("refs/heads/missing")
	assert.False(t, ok)
	_, ok = list.resolve("heads/main")
	assert.False(t, ok, "the short name of refs/heads/main is main, not heads/main (go-git Short)")

	s := list.summary()
	assert.Equal(t, []string{"main", "v1.0"}, s.Branches)
	assert.Equal(t, []string{"light", "v1.0"}, s.Tags)
	assert.Equal(t, shaA, s.HEAD)
	assert.Equal(t, "refs/heads/main", s.HEADRef)
}

func TestParseLsRemoteSymbolicHEADWithoutHashLine(t *testing.T) {
	t.Parallel()
	list, err := parseLsRemote([]byte("ref: refs/heads/trunk\tHEAD\n" + shaB + "\trefs/heads/trunk\n"))
	require.NoError(t, err)
	got, ok := list.resolve("HEAD")
	require.True(t, ok)
	assert.Equal(t, shaB, got)
	assert.Equal(t, shaB, list.summary().HEAD)
}

func TestParseLsRemoteRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"nohash\n", "zzzz\trefs/heads/x\n", shaA[:39] + "\trefs/heads/x\n", shaA + "\t\n"} {
		_, err := parseLsRemote([]byte(in))
		assert.Error(t, err, "%q", in)
	}
	list, err := parseLsRemote(nil)
	require.NoError(t, err)
	assert.Equal(t, Refs{Branches: []string{}, Tags: []string{}}, list.summary())
}

func TestShortRefName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"refs/heads/main":          "main",
		"refs/heads/feature/x":     "feature/x",
		"refs/tags/v1.0.0":         "v1.0.0",
		"refs/remotes/origin/main": "origin/main",
		"refs/pull/1/head":         "pull/1/head",
		"HEAD":                     "HEAD",
		"main":                     "main",
	}
	for in, want := range tests {
		assert.Equal(t, want, shortRefName(in), in)
	}
	assert.True(t, isShortRef("main"))
	assert.False(t, isShortRef("refs/heads/main"))
}

// TestRefListProperties: summaries are sorted and every listed branch/tag
// resolves to the hash advertised for it (peeled for tags) unless an earlier
// ref shares its short name.
func TestRefListProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		names := rapid.SliceOfNDistinct(rapid.StringMatching(`(refs/heads|refs/tags)/[a-z]{1,4}`), 0, 10, rapid.ID[string]).Draw(t, "names")
		hashes := map[string]string{}
		var b strings.Builder
		for i, n := range names {
			h := strings.Repeat(string("0123456789abcdef"[i%16]), 40)
			hashes[n] = h
			b.WriteString(h + "\t" + n + "\n")
		}
		list, err := parseLsRemote([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		s := list.summary()
		for _, l := range [][]string{s.Branches, s.Tags} {
			for i := 1; i < len(l); i++ {
				if l[i-1] > l[i] {
					t.Fatalf("not sorted: %v", l)
				}
			}
		}
		for _, n := range names {
			got, ok := list.resolve(n)
			if !ok || got != hashes[n] {
				t.Fatalf("resolve(%q) = %q, %v; want %q", n, got, ok, hashes[n])
			}
		}
	})
}
