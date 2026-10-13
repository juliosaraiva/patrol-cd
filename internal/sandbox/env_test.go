package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestBaseEnv(t *testing.T) {
	t.Setenv("PATH", "/opt/tools/bin:/usr/bin")
	t.Setenv("HOME", "/home/patrol")
	t.Setenv("PATROL_SECRET_CANARY", "x")

	tests := []struct {
		name  string
		extra []string
		want  []string
	}{
		{
			name: "base only",
			want: []string{"PATH=/opt/tools/bin:/usr/bin", "HOME=/home/patrol", "TMPDIR=" + os.TempDir(), "LANG=C.UTF-8"},
		},
		{
			name:  "extras appended, overrides in place, invalid dropped",
			extra: []string{"HELM_CACHE_HOME=/tmp/h", "HOME=/work", "noequals", "=novalue", "HELM_CACHE_HOME=/tmp/h2", "EMPTY="},
			want: []string{
				"PATH=/opt/tools/bin:/usr/bin", "HOME=/work", "TMPDIR=" + os.TempDir(), "LANG=C.UTF-8",
				"HELM_CACHE_HOME=/tmp/h2", "EMPTY=",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BaseEnv(tt.extra...)
			assert.Equal(t, tt.want, got)
			for _, kv := range got {
				assert.False(t, strings.HasPrefix(kv, "PATROL_SECRET_CANARY="))
			}
		})
	}

	t.Run("fallbacks", func(t *testing.T) {
		t.Setenv("PATH", "")
		t.Setenv("HOME", "")
		got := BaseEnv()
		assert.Equal(t, "PATH="+DefaultPath, got[0])
		assert.Equal(t, "HOME="+os.TempDir(), got[1])
	})
}

// TestBaseEnvProperties: keys are unique, the four base keys come first in a fixed
// order, and for every key the last valid extra wins.
func TestBaseEnvProperties(t *testing.T) {
	t.Parallel()
	keyGen := rapid.SampledFrom([]string{"PATH", "HOME", "TMPDIR", "LANG", "A", "B", "HELM_HOME", ""})
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 12).Draw(rt, "n")
		extra := make([]string, 0, n)
		lastWins := map[string]string{}
		for range n {
			k := keyGen.Draw(rt, "key")
			v := rapid.StringMatching(`[a-z=/]{0,6}`).Draw(rt, "value")
			if rapid.Bool().Draw(rt, "withEquals") {
				extra = append(extra, k+"="+v)
				if k != "" {
					lastWins[k] = v
				}
			} else {
				extra = append(extra, k) // no '=' at all
			}
		}
		got := BaseEnv(extra...)
		seen := map[string]bool{}
		for i, kv := range got {
			k, v, ok := strings.Cut(kv, "=")
			require.True(rt, ok, kv)
			require.NotEmpty(rt, k)
			require.False(rt, seen[k], "duplicate key %q in %v", k, got)
			seen[k] = true
			if i < 4 {
				require.Equal(rt, []string{"PATH", "HOME", "TMPDIR", "LANG"}[i], k)
			}
			if want, ok := lastWins[k]; ok {
				require.Equal(rt, want, v, "key %q", k)
			}
			val, _ := lookupEnv(got, k)
			require.Equal(rt, v, val)
		}
		for k := range lastWins {
			require.True(rt, seen[k], "extra key %q missing", k)
		}
	})
}

func TestLookupEnvLastWins(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/a", "X=1", "PATH=/b", "broken"}
	v, ok := lookupEnv(env, "PATH")
	assert.True(t, ok)
	assert.Equal(t, "/b", v)
	_, ok = lookupEnv(env, "broken")
	assert.False(t, ok)
}
