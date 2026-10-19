package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// valueDoc is one documented key of values.yaml.
type valueDoc struct {
	// Path is the key path (segments as written, quotes removed).
	Path []string
	// Description is the "# --" comment with its continuation lines joined.
	Description string
	// DefaultOverride replaces the rendered default ("# @default -- text").
	DefaultOverride string
}

var (
	keyLineRe = regexp.MustCompile(`^( *)("(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_.\-/]+):(?:\s+(.*))?$`)
	docStart  = regexp.MustCompile(`^( *)# -- ?(.*)$`)
	docAttr   = regexp.MustCompile(`^ *# @default -- ?(.*)$`)
	docCont   = regexp.MustCompile(`^ *#(?: (.*))?$`)
)

// parseValueDocs extracts the documented keys of a values file. The parser follows the block
// YAML style of the chart's values.yaml: two-space indentation, comments directly above the
// key they document, keys inside list items are not documented.
func parseValueDocs(data []byte) ([]valueDoc, error) {
	type frame struct {
		indent int
		key    string
	}
	var (
		stack      []frame
		docs       []valueDoc
		pending    *valueDoc
		pendIndent = -1
		blockFrom  = -1 // indentation of the key owning a block scalar being skipped
		listFrom   = -1 // indentation of the "- " of the list item being skipped
	)
	lines := strings.Split(string(data), "\n")
	for n, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if blockFrom >= 0 {
			if trimmed == "" || indent > blockFrom {
				continue
			}
			blockFrom = -1
		}
		if trimmed == "" {
			continue
		}
		if listFrom >= 0 {
			if indent > listFrom || (indent == listFrom && strings.HasPrefix(trimmed, "-")) {
				pending = nil
				continue
			}
			listFrom = -1
		}
		if m := docStart.FindStringSubmatch(line); m != nil {
			pending = &valueDoc{Description: strings.TrimSpace(m[2])}
			pendIndent = len(m[1])
			continue
		}
		if m := docAttr.FindStringSubmatch(line); m != nil && pending != nil {
			pending.DefaultOverride = strings.TrimSpace(m[1])
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if m := docCont.FindStringSubmatch(line); m != nil && pending != nil && !strings.HasPrefix(trimmed, "# @") {
				if txt := strings.TrimSpace(m[1]); txt != "" {
					pending.Description += " " + txt
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			// List item: keys inside lists are not documented.
			pending = nil
			listFrom = indent
			continue
		}
		m := keyLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("values.yaml:%d: cannot parse %q", n+1, line)
		}
		key := unquoteKey(m[2])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		path := make([]string, 0, len(stack)+1)
		for _, f := range stack {
			path = append(path, f.key)
		}
		path = append(path, key)
		if pending != nil {
			if pendIndent != indent {
				return nil, fmt.Errorf("values.yaml:%d: \"# --\" comment indented %d documents key %q indented %d", n+1, pendIndent, key, indent)
			}
			pending.Path = path
			docs = append(docs, *pending)
			pending = nil
		}
		rest := strings.TrimSpace(m[3])
		if strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			blockFrom = indent
			continue
		}
		if rest == "" {
			stack = append(stack, frame{indent: indent, key: key})
		}
	}
	return docs, nil
}

func unquoteKey(k string) string {
	if len(k) >= 2 && (k[0] == '"' && k[len(k)-1] == '"' || k[0] == '\'' && k[len(k)-1] == '\'') {
		return k[1 : len(k)-1]
	}
	return k
}

// lookup returns the value at path in a decoded values document.
func lookup(doc any, path []string) (any, bool) {
	cur := doc
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// displayPath renders a key path; segments containing dots are quoted.
func displayPath(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		if strings.ContainsAny(p, "./") {
			parts[i] = `"` + p + `"`
		} else {
			parts[i] = p
		}
	}
	return strings.Join(parts, ".")
}

func typeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "any"
	case bool:
		return "bool"
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "int"
		}
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// maxDefaultLen bounds rendered defaults; longer ones point to values.yaml.
const maxDefaultLen = 160

func renderDefault(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := strings.TrimSpace(buf.String())
	if len(s) > maxDefaultLen {
		return "see values.yaml", nil
	}
	return "`" + s + "`", nil
}

func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// escapeText escapes free text for a Markdown table cell: pipes, and angle brackets, which
// Markdown renderers would take for HTML tags (e.g. "<component>").
func escapeText(s string) string {
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	return escapeCell(s)
}

// valueRow is one row of the values table.
type valueRow struct {
	Key, Type, Default, Description string
}

// valueSection groups the rows of one top-level key.
type valueSection struct {
	Name string
	Rows []valueRow
}

func buildSections(values []byte) ([]valueSection, error) {
	docs, err := parseValueDocs(values)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return nil, fmt.Errorf("values.yaml: %w", err)
	}
	var sections []valueSection
	index := map[string]int{}
	for _, d := range docs {
		v, ok := lookup(doc, d.Path)
		if !ok {
			return nil, fmt.Errorf("values.yaml: documented key %s not found after parsing", displayPath(d.Path))
		}
		def := escapeText(d.DefaultOverride)
		if def == "" {
			if def, err = renderDefault(v); err != nil {
				return nil, err
			}
			def = escapeCell(def)
		}
		top := d.Path[0]
		if len(d.Path) == 1 {
			top = "top-level"
		}
		i, ok := index[top]
		if !ok {
			i = len(sections)
			index[top] = i
			sections = append(sections, valueSection{Name: top})
		}
		sections[i].Rows = append(sections[i].Rows, valueRow{
			Key:         escapeCell(displayPath(d.Path)),
			Type:        typeOf(v),
			Default:     def,
			Description: escapeText(d.Description),
		})
	}
	return sections, nil
}

// chartMeta is the part of Chart.yaml the README shows.
type chartMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	AppVersion  string `json:"appVersion"`
	KubeVersion string `json:"kubeVersion"`
	Description string `json:"description"`
}

func renderReadme(chart string, values []byte) ([]byte, error) {
	tplPath := filepath.Join(chart, "README.md.gotmpl")
	tplData, err := os.ReadFile(tplPath)
	if err != nil {
		return nil, err
	}
	chartData, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	if err != nil {
		return nil, err
	}
	var meta chartMeta
	if err := yaml.Unmarshal(chartData, &meta); err != nil {
		return nil, fmt.Errorf("parse Chart.yaml: %w", err)
	}
	sections, err := buildSections(values)
	if err != nil {
		return nil, err
	}
	var table bytes.Buffer
	for i, s := range sections {
		if i > 0 {
			table.WriteString("\n")
		}
		fmt.Fprintf(&table, "### %s\n\n| Key | Type | Default | Description |\n|-----|------|---------|-------------|\n", s.Name)
		for _, r := range s.Rows {
			fmt.Fprintf(&table, "| `%s` | %s | %s | %s |\n", r.Key, r.Type, r.Default, r.Description)
		}
	}
	tpl, err := template.New("README.md.gotmpl").Option("missingkey=error").Parse(string(tplData))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tplPath, err)
	}
	var out bytes.Buffer
	err = tpl.Execute(&out, map[string]any{
		"Chart":       meta,
		"ValuesTable": strings.TrimRight(table.String(), "\n"),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tplPath, err)
	}
	b := bytes.TrimRight(out.Bytes(), "\n")
	return append(b, '\n'), nil
}
