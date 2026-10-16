package badge

import (
	"fmt"
	"html"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Badge geometry (pixels), chosen to match the upstream badge sizes so embedded badges keep
// their layout after migration.
const (
	leftWidth             = 77  // health column
	widthWithoutRevision  = 131 // health + sync
	widthWithRevision     = 192 // + abbreviated revision
	widthWithFullRevision = 400 // + full revision
	rowHeight             = 20
	charWidth             = 6 // approximate width of one title character
	maxWidth              = 4096
	shortRevisionLen      = 7
)

// Colors (upstream palette).
const (
	colorBlue   = "rgb(16, 61, 102)"
	colorGreen  = "rgb(11, 97, 42)"
	colorPurple = "rgb(115, 31, 77)"
	colorOrange = "rgb(189, 115, 0)"
	colorRed    = "rgb(167, 46, 38)"
	colorGrey   = "rgb(41, 52, 61)"
)

var healthColors = map[v1alpha1.HealthStatusCode]string{
	v1alpha1.HealthStatusDegraded:    colorRed,
	v1alpha1.HealthStatusHealthy:     colorGreen,
	v1alpha1.HealthStatusMissing:     colorPurple,
	v1alpha1.HealthStatusProgressing: colorBlue,
	v1alpha1.HealthStatusSuspended:   colorGrey,
	v1alpha1.HealthStatusUnknown:     colorPurple,
}

var syncColors = map[v1alpha1.SyncStatusCode]string{
	v1alpha1.SyncStatusCodeSynced:    colorGreen,
	v1alpha1.SyncStatusCodeOutOfSync: colorOrange,
	v1alpha1.SyncStatusCodeUnknown:   colorPurple,
}

// view is everything a badge shows.
type view struct {
	health   v1alpha1.HealthStatusCode
	sync     v1alpha1.SyncStatusCode
	notFound bool
	// revision is shown when not empty (already abbreviated or not).
	revision string
	fullRev  bool
	// title is the Application name row ("" for none).
	title string
	// width overrides the computed width when > 0.
	width int
}

// render returns the SVG document of v. Every text is XML-escaped.
func render(v view) string {
	leftColor := healthColors[v.health]
	if leftColor == "" {
		leftColor = colorGrey
	}
	rightColor := syncColors[v.sync]
	if rightColor == "" {
		rightColor = colorGrey
	}
	leftText, rightText := string(v.health), string(v.sync)
	if v.notFound {
		leftText, rightText = "Not Found", ""
	}
	showRev := !v.notFound && v.revision != ""
	width := widthWithoutRevision
	if showRev {
		width = widthWithRevision
		if v.fullRev {
			width = widthWithFullRevision
		}
	}
	if v.width > 0 {
		width = v.width
	}
	height, y := rowHeight, 0
	showTitle := v.title != ""
	if showTitle {
		width = max(width, len(v.title)*charWidth)
		height, y = 2*rowHeight, rowHeight
	}
	// Columns: health [0, leftWidth), sync [leftWidth, syncEnd), revision [syncEnd, width).
	syncEnd := width
	if showRev {
		syncEnd = min(widthWithoutRevision, width)
	}
	syncEnd = max(syncEnd, min(leftWidth, width))
	revWidth := max(width-syncEnd, 0)

	var b strings.Builder
	label := leftText
	if rightText != "" {
		label += ": " + rightText
	}
	if showRev {
		label += " (" + v.revision + ")"
	}
	if showTitle {
		label = v.title + " " + label
	}
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" role="img" aria-label="%s">`, width, height, esc(label))
	fmt.Fprintf(&b, `<title>%s</title>`, esc(label))
	fmt.Fprintf(&b, `<clipPath id="round"><rect width="%d" height="%d" rx="3" fill="#fff"/></clipPath>`, width, height)
	b.WriteString(`<g clip-path="url(#round)">`)
	if showTitle {
		fmt.Fprintf(&b, `<rect id="titleRect" x="0" y="0" width="%d" height="%d" fill="%s"/>`, width, rowHeight, colorGrey)
	}
	fmt.Fprintf(&b, `<rect id="leftRect" x="0" y="%d" width="%d" height="%d" fill="%s"/>`, y, min(leftWidth, width), rowHeight, leftColor)
	fmt.Fprintf(&b, `<rect id="rightRect" x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, min(leftWidth, width), y, max(syncEnd-leftWidth, 0), rowHeight, rightColor)
	if showRev {
		fmt.Fprintf(&b, `<rect id="revisionRect" x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, syncEnd, y, revWidth, rowHeight, rightColor)
	}
	b.WriteString(`</g>`)
	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,Geneva,sans-serif" font-size="11">`)
	ty := y + 14
	if showTitle {
		fmt.Fprintf(&b, `<text id="titleText" x="%s" y="14">%s</text>`, half(0, width), esc(v.title))
	}
	fmt.Fprintf(&b, `<text id="leftText" x="%s" y="%d">%s</text>`, half(0, min(leftWidth, width)), ty, esc(leftText))
	fmt.Fprintf(&b, `<text id="rightText" x="%s" y="%d">%s</text>`, half(leftWidth, syncEnd), ty, esc(rightText))
	if showRev {
		fmt.Fprintf(&b, `<text id="revisionText" x="%s" y="%d">%s</text>`, half(syncEnd, syncEnd+revWidth), ty, esc("("+v.revision+")"))
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}

// half returns the horizontal center of [a, b) formatted for SVG.
func half(a, b int) string {
	c := float64(a+b) / 2
	if c == float64(int(c)) {
		return fmt.Sprintf("%d", int(c))
	}
	return fmt.Sprintf("%.1f", c)
}

// esc escapes text for XML content and attribute values; characters XML forbids (control
// characters, invalid UTF-8) are dropped.
func esc(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0xfffe || r == 0xffff {
			return -1
		}
		return r
	}, s)
	return html.EscapeString(s)
}
