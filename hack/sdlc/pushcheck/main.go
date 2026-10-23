// Command pushcheck is the Claude Code PreToolUse hook of the SDLC push gate
// (docs/guides/claude-code-sdlc.md), run by hack/sdlc/claude-pre-push.sh with the repository root as
// its argument. It reads the hook input (JSON with .tool_input.command and .cwd) on stdin and parses
// the command as bash: quoted text, heredoc bodies and comments are not commands, `cd DIR` and
// `git -C DIR` move the directory, and `bash -c '...'` and a heredoc or here-string given to a shell
// are parsed too.
//
// The hook checks pushes early, with clear messages; the authority is git's own pre-push hook
// (.githooks/pre-push), which sees the commits actually pushed, also by a script or an alias. So the
// wrapper requires core.hooksPath=.githooks before any command runs, and this hook refuses what would
// keep git's hook from running (--no-verify in full or abbreviated, -c core.hooksPath, GIT_*
// variables, --git-dir and other global options that move the repository, a push from inside a
// .git directory, -c alias.* and git config of core.hooksPath or an alias next to a push), every form
// it cannot analyze (a push inside $(...), eval, timeout, sudo, a script piped to a shell, a
// non-literal script, command word or argument, a command it cannot parse), and the forms the gate
// cannot check ref by ref (--all, --mirror, --tags, ':'). For a push from a clone of this project
// (or one of its worktrees) it then runs that clone's hack/sdlc/gate.sh on each pushed commit as it
// is now. Exit status 2 blocks the command and shows stderr to Claude Code; 0 lets it run.
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
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: pushcheck REPOSITORY_ROOT <hook-input.json")
		os.Exit(2)
	}
	os.Exit(run(os.Stdin, os.Stderr, os.Args[1]))
}

func run(stdin io.Reader, stderr io.Writer, root string) int {
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return 0 // not a Bash tool call we understand; nothing to gate
	}
	cmd := in.ToolInput.Command
	refuse := func(msg string) int {
		_, _ = fmt.Fprintf(stderr, "sdlc pre-push hook: %s\n  (docs/guides/claude-code-sdlc.md)\n", msg)
		return 2
	}
	a, err := findPushes(cmd, in.Cwd)
	if err != nil {
		if mentionsGitPush.MatchString(cmd) {
			return refuse("cannot parse the command as bash (the Bash tool may run another shell, such as zsh), so its git push cannot be checked; " + runAlone)
		}
		return 0
	}
	if a.Hidden != "" {
		return refuse(a.Hidden)
	}
	var problems []string
	for _, p := range a.Pushes {
		if msg := gatePush(p, root); msg != "" {
			problems = append(problems, msg)
		}
	}
	if len(problems) > 0 {
		return refuse(strings.Join(problems, "\n"))
	}
	return 0
}

// gatePush returns why p may not run, or "" when it may.
func gatePush(p push, root string) string {
	if p.Dir == "" {
		return "cannot tell which directory `git push` runs in (a variable, `cd -`, `popd` or `~user`); cd to the repository with a literal path"
	}
	if fi, err := os.Stat(p.Dir); err != nil || !fi.IsDir() {
		return fmt.Sprintf("`git push` would run in %s, which is not a directory here; cd to the repository with a literal path", p.Dir)
	}
	where, err := gitOut(p.Dir, "rev-parse", "--is-inside-work-tree", "--is-bare-repository")
	if err != nil {
		return "" // not a git repository: git push fails on its own
	}
	switch where {
	case "true\nfalse": // a work tree
	case "false\ntrue":
		return "" // a bare repository: not a clone of this project
	default:
		// Inside a .git directory git finds no work tree, and the relative core.hooksPath no hook.
		return "`git push` would run inside a git directory (" + p.Dir + "), where git's pre-push hook does not run; push from the work tree"
	}
	top, err := gitOut(p.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "" // git push fails on its own
	}
	// Every clone of this project is gated (its own records), and this repository on any branch.
	gate := filepath.Join(top, "hack", "sdlc", "gate.sh")
	_, noGate := os.Stat(gate)
	if noGate != nil && !sameRepository(top, root) {
		return "" // another repository: not this gate's business
	}
	if p.Refused != "" {
		return p.Refused
	}
	if p.DryRun || len(p.Sources) == 0 {
		return "" // a dry run, or deletions only: no commit is pushed
	}
	if hp, _ := gitOut(top, "config", "--get", "core.hooksPath"); hp != ".githooks" {
		return "git's pre-push hook is not enabled in " + top + " (core.hooksPath is " + quoteOrUnset(hp) + "): run `make sdlc-git-hooks` once, then push again"
	}
	if noGate != nil {
		return top + " has no hack/sdlc/gate.sh (a branch from before the SDLC gate?): rebase it on main"
	}
	var problems []string
	for _, src := range p.Sources {
		commit, err := gitOut(top, "rev-parse", "--verify", "--quiet", src+"^{commit}")
		if err != nil {
			problems = append(problems, fmt.Sprintf("cannot resolve %q in %s", src, top))
			continue
		}
		cmd := exec.Command(gate, commit)
		cmd.Dir = top
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = fmt.Sprintf("%s %s: %v", gate, commit, err)
			}
			problems = append(problems, msg)
		}
	}
	return strings.Join(problems, "\n")
}

// sameRepository reports whether two work trees share a git directory (a clone and its worktrees).
func sameRepository(a, b string) bool {
	ga, errA := gitOut(a, "rev-parse", "--path-format=absolute", "--git-common-dir")
	gb, errB := gitOut(b, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if errA != nil || errB != nil {
		return false
	}
	ra, errA := filepath.EvalSymlinks(ga)
	rb, errB := filepath.EvalSymlinks(gb)
	return errA == nil && errB == nil && ra == rb
}

func quoteOrUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return fmt.Sprintf("%q", s)
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
