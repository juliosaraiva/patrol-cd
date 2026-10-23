// Command pushcheck is the Claude Code PreToolUse hook of the SDLC push gate
// (docs/guides/claude-code-sdlc.md), run by hack/sdlc/claude-pre-push.sh with the repository root as
// its argument for every command Claude Code runs. It reads the hook input (JSON with
// .tool_input.command and .cwd) on stdin and parses the command as bash with mvdan.cc/sh: quoted
// text, heredoc bodies and comments are not commands; words are expanded the way bash does it when
// that does not depend on the environment, the filesystem or another command (quotes, escapes,
// $'…', braces); `cd DIR` and `git -C DIR` move the directory only where they certainly run; and
// `bash -c '…'` and a heredoc or here-string given to a shell are parsed too.
//
// It follows the command a wrapper runs (timeout, nice, sudo, env, xargs, find -exec, kubectl/docker
// exec -- ..., command, exec) into that command, and parses bash -c scripts, heredocs and
// here-strings, eval, and git rebase -x / bisect run / submodule foreach. A git push found this way
// is gated like a direct one.
//
// The authority is git's own pre-push hook (.githooks/pre-push), which sees the commits actually
// pushed, also by a script or an alias; the wrapper requires it to be enabled before any command
// runs. This hook gives early, clear refusals and keeps git's hook from being skipped. It refuses,
// wherever the command runs: what can point git at another repository or change its configuration,
// hooks or aliases (GIT_* variables outside a short safe list, set but not merely printed; git -c and
// git config writes of keys outside a short safe list; --git-dir, --work-tree, --bare and other
// global options before push; an alias for a push; a sourced file before a push); a push it cannot
// resolve (inside $(...), eval of a dynamic string, a non-literal command word, git option or shell
// script, a command that does not parse as bash and names a push or a hook override); and a push that
// cannot be gated per step (git rebase -x, submodule foreach).
//
// For a push from a clone of this project (or one of its worktrees) it also refuses --no-verify, an
// abbreviated or unknown push option, a non-literal argument, a push from inside .git, from an
// unknown directory or while core.hooksPath is not .githooks, and the forms the gate cannot check
// ref by ref (--all, --mirror, --tags, ':'); then it runs that clone's hack/sdlc/gate.sh on each
// pushed commit as it is now. A ref that does not resolve yet is left to git's hook. Exit status 2
// blocks the command and shows stderr to Claude Code; 0 lets it run.
//
// What it cannot see: a git invocation assembled at run time (name or subcommand from variables,
// files or $'…' escapes), the content of script files, code given to an interpreter (python -c, awk,
// a command that is not a known wrapper), and commands on a remote host. git's own hook checks those
// pushes; a program that disables it on purpose is out of reach, as the guide's Limits say.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type hookInput struct {
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"` // the terminal tool's directory, relative to the session's
	} `json:"tool_input"`
}

// hookText is what a command that does not parse may not mention, besides git and push.
var hookText = regexp.MustCompile(`(?i)hookspath|no-verify|GIT_[A-Z]`)

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
	cwd := in.Cwd
	if d := in.ToolInput.Cwd; d != "" { // the terminal tool's cwd: absolute, ~, or relative to the session's
		switch {
		case d == "~" || strings.HasPrefix(d, "~/"):
			if h, err := os.UserHomeDir(); err == nil {
				cwd = filepath.Join(h, strings.TrimPrefix(d, "~"))
			}
		case filepath.IsAbs(d) || cwd == "":
			cwd = d
		default:
			cwd = filepath.Join(cwd, d)
		}
	}
	a, err := findPushes(cmd, cwd)
	if err != nil {
		if mentionsGitPush(cmd) || hookText.MatchString(normalize(cmd)) {
			return refuse("cannot parse the command as bash (the Bash tool may run another shell, such as zsh), so what it does with git cannot be checked; " + runAlone)
		}
		return 0
	}
	if a.Blocked != "" {
		return refuse(a.Blocked)
	}
	var problems []string
	if msg := checkAliases(a.Calls, root); msg != "" {
		problems = append(problems, msg)
	}
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
			// A ref that does not exist yet: a branch or tag created earlier in the same command line.
			// core.hooksPath=.githooks is set (checked just above), so git's own pre-push hook gates
			// whatever it ends up pointing at. Leave it to that hook, as for a commit made in the same
			// command.
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

// checkAliases refuses a git subcommand that is an alias for a push or for a change of git's hooks,
// configured before the command (an alias the command itself defines is refused by the parser).
func checkAliases(calls []gitCall, root string) string {
	seen := map[gitCall]bool{}
	for _, c := range calls {
		if c.Dir == "" {
			c.Dir = root
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		v, err := gitOut(c.Dir, "config", "--get", "alias."+c.Sub)
		if err != nil || v == "" {
			continue
		}
		if n := strings.ToLower(normalize(v)); strings.Contains(n, "push") || hookText.MatchString(n) {
			return fmt.Sprintf("git %s is an alias (%q) that pushes or changes git's hooks; run the git command itself", c.Sub, v)
		}
	}
	return ""
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
