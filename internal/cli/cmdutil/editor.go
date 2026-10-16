package cmdutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Interactive editing (upstream cli.InteractiveEdit).
const (
	defaultEditor  = "vi"
	maxEditRounds  = 10
	maxEditedBytes = 4 << 20
	commentsHeader = `# Please edit the object below. Lines beginning with a '#' will be ignored,
# and an empty file will abort the edit. If an error occurs while saving this file will be
# reopened with the relevant failures.`
)

// ErrEditCancelled is returned when the edited file is empty or unchanged.
var ErrEditCancelled = errors.New("edit cancelled, no valid changes were saved")

func stripComments(input []byte) []byte {
	var out []byte
	lines := bytes.Split(input, []byte("\n"))
	for i, line := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			continue
		}
		out = append(out, line...)
		if i < len(lines)-1 {
			out = append(out, '\n')
		}
	}
	return out
}

func withComments(input []byte, comments string) []byte {
	input = stripComments(input)
	parts := []string{commentsHeader}
	var lines []string
	for l := range strings.SplitSeq(comments, "\n") {
		if l != "" {
			lines = append(lines, "# "+l)
		}
	}
	if len(lines) > 0 {
		parts = append(parts, strings.Join(lines, "\n"))
	}
	parts = append(parts, string(input))
	return []byte(strings.Join(parts, "\n"))
}

// InteractiveEdit opens data in $EDITOR (default vi) in a private temporary file and
// calls save with the result; a failed save reopens the editor with the error as a
// comment. An empty or unchanged file cancels the edit.
func (f *Factory) InteractiveEdit(ctx context.Context, pattern string, data []byte, save func([]byte) error) error {
	editor, args := defaultEditor, []string(nil)
	if e := strings.Fields(f.Getenv("EDITOR")); len(e) > 0 {
		editor, args = e[0], e[1:]
	}
	comment := ""
	for range maxEditRounds {
		data = withComments(data, comment)
		tmp, err := os.CreateTemp("", pattern)
		if err != nil {
			return fmt.Errorf("create temporary file: %w", err)
		}
		name := tmp.Name()
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(name)
			return fmt.Errorf("write temporary file: %w", errors.Join(werr, cerr))
		}
		cmd := exec.CommandContext(ctx, editor, append(args, name)...) //nolint:gosec // the user's own $EDITOR
		cmd.Stdin, cmd.Stdout, cmd.Stderr = f.IO.In, f.Out(), f.ErrOut()
		if f.Environ != nil {
			cmd.Env = f.Environ()
		}
		if err := cmd.Run(); err != nil {
			_ = os.Remove(name)
			return fmt.Errorf("run editor %s: %w", editor, err)
		}
		updated, err := readFileBounded(name)
		_ = os.Remove(name)
		if err != nil {
			return err
		}
		if len(updated) == 0 || bytes.Equal(updated, data) {
			return ErrEditCancelled
		}
		data = stripComments(updated)
		if err := save(data); err != nil {
			comment = err.Error()
			continue
		}
		return nil
	}
	return fmt.Errorf("giving up after %d edit attempts: %s", maxEditRounds, comment)
}

func readFileBounded(name string) ([]byte, error) {
	fh, err := os.Open(name) //nolint:gosec // our own temporary file
	if err != nil {
		return nil, fmt.Errorf("read edited file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(io.LimitReader(fh, maxEditedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read edited file: %w", err)
	}
	if len(b) > maxEditedBytes {
		return nil, fmt.Errorf("edited file exceeds %d bytes", maxEditedBytes)
	}
	return b, nil
}
