// Package output prints CLI results: JSON (two-space indent), YAML (from the JSON form,
// so JSON field names are used), aligned tables (text/tabwriter, two spaces between
// columns as upstream) and plain names.
//
// Strings that come from the server are untrusted: Sanitize replaces control characters
// (terminal escape sequences, newlines and tabs that would break table layout) before
// they reach a terminal. JSON and YAML output is escaped by the encoders.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"
	"unicode"

	"sigs.k8s.io/yaml"
)

// Output formats.
const (
	JSON  = "json"
	YAML  = "yaml"
	Wide  = "wide"
	Name  = "name"
	Table = "table"
	Short = "short"
	URL   = "url"
)

// UnknownFormat returns the upstream error for an unsupported -o value. supported lists
// the accepted formats (may be empty).
func UnknownFormat(format string, supported ...string) error {
	if len(supported) == 0 {
		return fmt.Errorf("unknown output format: %s", format)
	}
	return fmt.Errorf("unknown output format: %s. Supported formats: %s", format, strings.Join(supported, "|"))
}

// CheckFormat returns UnknownFormat unless format is one of supported.
func CheckFormat(format string, supported ...string) error {
	for _, s := range supported {
		if format == s {
			return nil
		}
	}
	return UnknownFormat(format, supported...)
}

// PrintJSON writes v as indented JSON followed by a newline.
func PrintJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal resource to json: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// PrintYAML writes v as YAML (field names of its JSON form).
func PrintYAML(w io.Writer, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("unable to marshal resource to yaml: %w", err)
	}
	_, err = w.Write(b)
	return err
}

// PrintResource writes one resource as json or yaml.
func PrintResource(w io.Writer, v any, format string) error {
	switch format {
	case JSON:
		return PrintJSON(w, v)
	case YAML:
		return PrintYAML(w, v)
	}
	return UnknownFormat(format)
}

// PrintResourceList writes a list as json or yaml. An empty list prints [] rather than
// null; with single, a one-element list prints that element (upstream PrintResourceList).
func PrintResourceList(w io.Writer, list any, format string, single bool) error {
	v := reflect.ValueOf(list)
	if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
		if single && v.Len() == 1 {
			return PrintResource(w, v.Index(0).Interface(), format)
		}
		if v.Len() == 0 {
			return PrintResource(w, []string{}, format)
		}
	}
	return PrintResource(w, list, format)
}

// Sanitize replaces control characters (including tabs, newlines and escape sequences)
// with spaces so that untrusted values cannot inject terminal escapes or break tables.
func Sanitize(s string) string {
	if strings.IndexFunc(s, isUnsafe) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isUnsafe(r) {
			return ' '
		}
		return r
	}, s)
}

// isUnsafe reports control characters, line/paragraph separators and bidirectional
// override characters (which can disguise text in a terminal).
func isUnsafe(r rune) bool {
	switch {
	case unicode.IsControl(r), r == '\u2028', r == '\u2029':
		return true
	case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
		return true
	}
	return false
}

// SanitizeMultiline keeps newlines but removes every other control character.
func SanitizeMultiline(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if isUnsafe(r) {
			return ' '
		}
		return r
	}, s)
}

// Tab is an aligned table writer (columns separated by at least two spaces).
type Tab struct {
	tw *tabwriter.Writer
}

// NewTable returns a table writing to w.
func NewTable(w io.Writer) *Tab {
	return &Tab{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
}

// Row writes one row; each cell is formatted with %v and sanitized.
func (t *Tab) Row(cells ...any) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = Sanitize(fmt.Sprint(c))
	}
	_, _ = fmt.Fprintln(t.tw, strings.Join(parts, "\t"))
}

// Flush writes the buffered table.
func (t *Tab) Flush() error { return t.tw.Flush() }

// Println writes one sanitized line.
func Println(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, Sanitize(fmt.Sprint(a...)))
}

// Printf writes a formatted line whose arguments are sanitized (the format itself is
// trusted).
func Printf(w io.Writer, format string, a ...any) {
	clean := make([]any, len(a))
	for i, v := range a {
		switch x := v.(type) {
		case string:
			clean[i] = Sanitize(x)
		case fmt.Stringer:
			clean[i] = Sanitize(x.String())
		default:
			clean[i] = v
		}
	}
	_, _ = fmt.Fprintf(w, format, clean...)
}

// JoinOrNone joins values with sep, or returns "<none>" for an empty list.
func JoinOrNone(values []string, sep string) string {
	if len(values) == 0 {
		return "<none>"
	}
	return strings.Join(values, sep)
}

// OrDash returns s or "-" when it is empty.
func OrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
