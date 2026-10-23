// Command pushcheck is the Claude Code PreToolUse hook of the SDLC push gate
// (docs/guides/claude-code-sdlc.md), run by hack/sdlc/claude-pre-push.sh. It reads the hook input
// (JSON with .tool_input.command and .cwd) on stdin, parses the command as bash, and finds every
// `git push` it would run: quoted text, heredoc bodies and comments are not commands, `cd DIR` and
// `git -C DIR` move the directory, and `bash -c '...'` and `sh -c '...'` are parsed too. For each push
// from a repository that has the gate, it resolves the pushed commits (the refspec sources, or HEAD)
// and runs that repository's hack/sdlc/gate.sh on each. Exit status 2 blocks the command and shows
// stderr to Claude Code; 0 lets it run.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type hookInput struct {
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

func main() {
	os.Exit(run(os.Stdin, os.Stderr))
}

func run(stdin io.Reader, stderr io.Writer) int {
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return 0 // not a Bash tool call we understand; nothing to gate
	}
	pushes, err := findPushes(in.ToolInput.Command, in.Cwd)
	if err != nil {
		return 0 // bash would not run it either
	}
	status := 0
	for _, p := range pushes {
		if msg := gatePush(p); msg != "" {
			_, _ = fmt.Fprintln(stderr, msg)
			status = 2
		}
	}
	if status != 0 {
		_, _ = fmt.Fprintf(stderr, "sdlc pre-push hook: blocked; see docs/guides/claude-code-sdlc.md\n")
	}
	return status
}

// gatePush returns why p may not run, or "" when it may.
func gatePush(p push) string {
	if p.Refused != "" {
		return "sdlc pre-push hook: " + p.Refused
	}
	if p.DryRun || len(p.Sources) == 0 {
		return "" // a dry run, or deletions only
	}
	if p.Dir == "" {
		return "sdlc pre-push hook: cannot tell which directory `git push` runs in (the directory comes from a variable or a command substitution); cd to the repository with a literal path"
	}
	top, err := gitOut(p.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "" // not a git repository: git push will fail on its own
	}
	gate := filepath.Join(top, "hack", "sdlc", "gate.sh")
	if _, err := os.Stat(gate); err != nil {
		return "" // a repository without the gate
	}
	var problems []string
	for _, src := range p.Sources {
		if src == "" {
			problems = append(problems, "cannot tell which ref `git push` pushes (a variable or command substitution); name the branch")
			continue
		}
		commit, err := gitOut(top, "rev-parse", "--verify", "--quiet", src+"^{commit}")
		if err != nil {
			problems = append(problems, fmt.Sprintf("cannot resolve %q in %s", src, top))
			continue
		}
		cmd := exec.Command(gate, commit)
		cmd.Dir = top
		out, err := cmd.CombinedOutput()
		if err != nil {
			problems = append(problems, strings.TrimSpace(string(out)))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return "sdlc pre-push hook: " + strings.Join(problems, "\n")
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
