package badge

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// wellFormed parses svg as XML.
func wellFormed(t *testing.T, svg string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(svg))
	for {
		_, err := d.Token()
		if err == io.EOF {
			return
		}
		require.NoError(t, err, svg)
	}
}

func TestRenderEscapesAndStaysWellFormed(t *testing.T) {
	t.Parallel()
	cases := []view{
		{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced},
		{health: "<script>alert(1)</script>", sync: `"&'`},
		{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced, revision: "a<b>\x00\x1b\xff&c", fullRev: true},
		{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced, title: `x"><svg onload=alert(1)>`},
		{notFound: true, revision: "abc", title: "t"},
		{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced, width: 10, revision: "abc"},
		{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced, width: 4096, title: strings.Repeat("n", 253)},
	}
	for _, v := range cases {
		svg := render(v)
		wellFormed(t, svg)
		require.NotContains(t, svg, "<script")
		require.NotContains(t, svg, "<svg onload")
		require.Equal(t, 1, strings.Count(svg, "<svg"), "no injected elements")
		require.Equal(t, 1, strings.Count(svg, "<title>"))
		require.NotContains(t, svg, "\x00")
		require.NotContains(t, svg, `width="-`, "no negative widths")
	}
}

func TestRenderColorsAndGeometry(t *testing.T) {
	t.Parallel()
	svg := render(view{health: v1alpha1.HealthStatusSuspended, sync: "Weird"})
	require.Contains(t, svg, `id="leftRect" x="0" y="0" width="77" height="20" fill="rgb(41, 52, 61)"`)
	require.Contains(t, svg, `id="rightRect" x="77" y="0" width="54" height="20" fill="rgb(41, 52, 61)"`, "unknown sync status is grey")
	svg = render(view{health: v1alpha1.HealthStatusMissing, sync: v1alpha1.SyncStatusCodeUnknown, revision: "abc1234"})
	require.Contains(t, svg, `id="revisionRect" x="131" y="0" width="61"`)
	require.Contains(t, svg, `fill="rgb(115, 31, 77)"`)
	svg = render(view{health: v1alpha1.HealthStatusHealthy, sync: v1alpha1.SyncStatusCodeSynced, title: strings.Repeat("x", 40)})
	require.Contains(t, svg, `width="240" height="40"`, "title widens the badge")
	require.Contains(t, svg, `id="rightRect" x="77" y="20" width="163"`)
}
