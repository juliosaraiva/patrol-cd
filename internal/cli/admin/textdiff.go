package admin

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/sergi/go-diff/diffmatchpatch"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// diffContext is the number of unchanged lines around each change (diff -u).
const diffContext = 3

type lineOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// splitLines splits s into lines without their terminators.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lineDiff computes the line edit script turning a into b. Each distinct line is encoded
// as one rune and diffed with diffmatchpatch's rune diff (its own line mode,
// DiffLinesToChars, mis-hydrates diffs in v1.3.x because it encodes lines as decimal
// indexes that the character diff can split).
func lineDiff(a, b string) []lineOp {
	la, lb := splitLines(a), splitLines(b)
	index := map[string]rune{}
	lines := map[rune]string{}
	next := rune(1)
	encode := func(in []string) ([]rune, bool) {
		out := make([]rune, len(in))
		for i, l := range in {
			r, ok := index[l]
			if !ok {
				if next >= 0xD800 && next <= 0xDFFF {
					next = 0xE000 // skip surrogates: they do not survive string conversions
				}
				if next > unicode.MaxRune {
					return nil, false
				}
				r = next
				next++
				index[l] = r
				lines[r] = l
			}
			out[i] = r
		}
		return out, true
	}
	ra, okA := encode(la)
	rb, okB := encode(lb)
	var ops []lineOp
	if !okA || !okB {
		for _, l := range la {
			ops = append(ops, lineOp{kind: '-', text: l})
		}
		for _, l := range lb {
			ops = append(ops, lineOp{kind: '+', text: l})
		}
		return ops
	}
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = 5 * time.Second
	for _, d := range dmp.DiffMainRunes(ra, rb, false) {
		var kind byte
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			kind = ' '
		case diffmatchpatch.DiffDelete:
			kind = '-'
		case diffmatchpatch.DiffInsert:
			kind = '+'
		}
		for _, r := range d.Text {
			ops = append(ops, lineOp{kind: kind, text: lines[r]})
		}
	}
	return ops
}

// unifiedDiff renders a unified diff (as "diff -u") of a and b, or "" when they are equal.
func unifiedDiff(aLabel, bLabel, a, b string) string {
	ops := lineDiff(a, b)
	changed := false
	for _, op := range ops {
		if op.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aLabel, bLabel)
	// Positions (1-based) of each op in a and b.
	type pos struct{ a, b int }
	at := make([]pos, len(ops)+1)
	ai, bi := 1, 1
	for i, op := range ops {
		at[i] = pos{ai, bi}
		switch op.kind {
		case ' ':
			ai++
			bi++
		case '-':
			ai++
		case '+':
			bi++
		}
	}
	at[len(ops)] = pos{ai, bi}
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-diffContext, 0)
		end := i
		// Extend the hunk while changes are within 2*context lines of each other.
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*diffContext {
				end = min(end+diffContext, len(ops))
				break
			}
			end = run
		}
		aCount, bCount := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aCount++
			}
			if op.kind != '-' {
				bCount++
			}
		}
		aStart, bStart := at[start].a, at[start].b
		if aCount == 0 {
			aStart--
		}
		if bCount == 0 {
			bStart--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for _, op := range ops[start:end] {
			sb.WriteByte(op.kind)
			sb.WriteString(op.text)
			sb.WriteByte('\n')
		}
		i = end
	}
	return sb.String()
}

// objectYAML renders an object as YAML ("" for nil).
func objectYAML(obj *unstructured.Unstructured) (string, error) {
	if obj == nil || obj.Object == nil {
		return "", nil
	}
	data, err := yaml.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("error marshaling %s: %w", obj.GetName(), err)
	}
	return string(data), nil
}

// printObjectDiff prints the unified diff of the YAML of live and target (upstream
// cli.PrintDiff, without an external diff tool). Nil objects are empty documents.
func printObjectDiff(w io.Writer, name string, live, target *unstructured.Unstructured) error {
	a, err := objectYAML(live)
	if err != nil {
		return err
	}
	b, err := objectYAML(target)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, unifiedDiff("live/"+name, "target/"+name, a, b))
	return err
}
