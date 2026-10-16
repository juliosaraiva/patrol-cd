package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
)

// Environment variables selecting an external diff program (the first one set wins).
const (
	envDiffProgram    = "PATROL_DIFF_PROGRAM"
	envKubectlExtDiff = "KUBECTL_EXTERNAL_DIFF"
)

// printResourceDiff prints the header of one changed resource and the difference between
// its live and target YAML: through the external program named by PATROL_DIFF_PROGRAM or
// KUBECTL_EXTERNAL_DIFF (called with the live and target files), else in the default
// output format of diff(1), as upstream's default "diff" program prints it.
//
// A target-only resource comes from rendered repository content the API server has not
// validated, and --preview-changes asks for confirmation right after this output: the header
// is sanitized, and so is the built-in diff body (YAML already escapes control characters;
// this also removes bidirectional overrides that could disguise a change).
func (e *env) printResourceDiff(ctx context.Context, w, errW io.Writer, key resourceKey, live, target *unstructured.Unstructured) error {
	output.Printf(w, "\n===== %s/%s %s/%s ======\n", key.Group, key.Kind, key.Namespace, key.Name)
	liveYAML, err := objectYAML(live)
	if err != nil {
		return err
	}
	targetYAML, err := objectYAML(target)
	if err != nil {
		return err
	}
	program := e.getenv(envDiffProgram)
	if program == "" {
		program = e.getenv(envKubectlExtDiff)
	}
	if strings.TrimSpace(program) != "" {
		return runExternalDiff(ctx, w, errW, program, key.Name, liveYAML, targetYAML)
	}
	_, err = io.WriteString(w, output.SanitizeMultiline(normalDiff(liveYAML, targetYAML)))
	return err
}

func objectYAML(obj *unstructured.Unstructured) (string, error) {
	if obj == nil {
		return "", nil
	}
	data, err := yaml.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("encode %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return string(data), nil
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// runExternalDiff writes both documents to private temporary files and runs the program
// (split like a shell command line, without a shell). Exit status 1 means "differences" for
// diff tools and is not an error.
func runExternalDiff(ctx context.Context, w, errW io.Writer, program, name, live, target string) error {
	parts, err := splitCommandLine(program)
	if err != nil {
		return fmt.Errorf("invalid diff program %q: %w", program, err)
	}
	if len(parts) == 0 {
		return errors.New("empty diff program")
	}
	dir, err := os.MkdirTemp("", "patrol-diff-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	safe := unsafeFileChars.ReplaceAllString(name, "_")
	if safe == "" || safe == "." || safe == ".." {
		safe = "resource"
	}
	targetFile := filepath.Join(dir, safe)
	liveFile := filepath.Join(dir, safe+"-live.yaml")
	if err := os.WriteFile(targetFile, []byte(target), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(liveFile, []byte(live), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], liveFile, targetFile)...) //nolint:gosec // user-selected diff program
	cmd.Stdout, cmd.Stderr = w, errW
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run diff program %s: %w", parts[0], err)
	}
	return nil
}

// splitCommandLine splits s into words, honoring single and double quotes and backslash
// escapes (no variable expansion, no globbing).
func splitCommandLine(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, inWord = false, true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// diffLine is one line of a line diff.
type diffLine struct {
	op   diffmatchpatch.Operation
	text string
}

// lineDiff computes a line-level diff of a and b.
func lineDiff(a, b string) []diffLine {
	dmp := diffmatchpatch.New()
	ca, cb, lines := dmp.DiffLinesToChars(a, b)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(ca, cb, false), lines)
	var out []diffLine
	for _, d := range diffs {
		text := d.Text
		for text != "" {
			line, rest, found := strings.Cut(text, "\n")
			if found {
				line += "\n"
			}
			out = append(out, diffLine{op: d.Type, text: line})
			text = rest
		}
	}
	return out
}

// normalDiff renders the difference from a to b in the default ("normal") output format of
// diff(1), which upstream runs when KUBECTL_EXTERNAL_DIFF is unset: change commands
// (NcM, NdM, NaM with line ranges) followed by the removed ("< ") and added ("> ") lines.
// It returns "" when a and b are equal.
func normalDiff(a, b string) string {
	if a == b {
		return ""
	}
	lines := lineDiff(a, b)
	var sb strings.Builder
	rng := func(start, n int) string {
		if n == 1 {
			return strconv.Itoa(start)
		}
		return strconv.Itoa(start) + "," + strconv.Itoa(start+n-1)
	}
	write := func(prefix string, ls []string) {
		for _, l := range ls {
			sb.WriteString(prefix)
			sb.WriteString(l)
			if !strings.HasSuffix(l, "\n") {
				sb.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	consumedA, consumedB := 0, 0
	for i := 0; i < len(lines); {
		if lines[i].op == diffmatchpatch.DiffEqual {
			consumedA++
			consumedB++
			i++
			continue
		}
		var del, ins []string
		for ; i < len(lines) && lines[i].op != diffmatchpatch.DiffEqual; i++ {
			if lines[i].op == diffmatchpatch.DiffDelete {
				del = append(del, lines[i].text)
			} else {
				ins = append(ins, lines[i].text)
			}
		}
		switch {
		case len(del) > 0 && len(ins) > 0:
			fmt.Fprintf(&sb, "%sc%s\n", rng(consumedA+1, len(del)), rng(consumedB+1, len(ins)))
		case len(del) > 0:
			fmt.Fprintf(&sb, "%sd%d\n", rng(consumedA+1, len(del)), consumedB)
		default:
			fmt.Fprintf(&sb, "%da%s\n", consumedA, rng(consumedB+1, len(ins)))
		}
		write("< ", del)
		if len(del) > 0 && len(ins) > 0 {
			sb.WriteString("---\n")
		}
		write("> ", ins)
		consumedA += len(del)
		consumedB += len(ins)
	}
	return sb.String()
}
