package admin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnifiedDiff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, a, b, want string
	}{
		{name: "equal", a: "x\ny\n", b: "x\ny\n", want: ""},
		{name: "both empty", want: ""},
		{name: "added to empty", a: "", b: "a: 1\nb: 2\n", want: "--- l\n+++ r\n@@ -0,0 +1,2 @@\n+a: 1\n+b: 2\n"},
		{name: "removed all", a: "a: 1\n", b: "", want: "--- l\n+++ r\n@@ -1,1 +0,0 @@\n-a: 1\n"},
		{
			name: "change in the middle with context",
			a:    "1\n2\n3\n4\n5\n6\n7\n8\n9\n",
			b:    "1\n2\n3\n4\nfive\n6\n7\n8\n9\n",
			want: "--- l\n+++ r\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n",
		},
		{
			name: "two distant hunks",
			a:    "a\n1\n2\n3\n4\n5\n6\n7\n8\n9\nz\n",
			b:    "A\n1\n2\n3\n4\n5\n6\n7\n8\n9\nZ\n",
			want: "--- l\n+++ r\n@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -8,4 +8,4 @@\n 7\n 8\n 9\n-z\n+Z\n",
		},
		{
			name: "repeated lines are matched as lines",
			a:    "apiVersion: v1\nspec:\n  policies:\n  - p1\nmetadata:\n  name: x\n",
			b:    "apiVersion: v1\nspec:\n  policies:\n  - p1\n  - p2\nmetadata:\n  name: x\n",
			want: "--- l\n+++ r\n@@ -2,5 +2,6 @@\n spec:\n   policies:\n   - p1\n+  - p2\n metadata:\n   name: x\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, unifiedDiff("l", "r", tc.a, tc.b))
		})
	}
}

func TestLineDiffManyDistinctLines(t *testing.T) {
	t.Parallel()
	// Enough distinct lines to cross the surrogate range of the rune encoding.
	var a, b strings.Builder
	for i := range 60000 {
		fmt.Fprintf(&a, "line %d\n", i)
		if i == 59000 {
			b.WriteString("changed\n")
			continue
		}
		fmt.Fprintf(&b, "line %d\n", i)
	}
	d := unifiedDiff("l", "r", a.String(), b.String())
	assert.Contains(t, d, "-line 59000\n+changed\n")
	assert.Equal(t, 1, strings.Count(d, "@@ -"))
}
