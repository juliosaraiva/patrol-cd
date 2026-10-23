// Command pushcheck is the Claude Code PreToolUse hook of the SDLC push gate
// (docs/guides/claude-code-sdlc.md), run by hack/sdlc/claude-pre-push.sh with the repository root as
// its argument for every command Claude Code runs. It reads the hook input (JSON with
// .tool_input.command and .cwd) on stdin.
//
// The authority is git's own pre-push hook (.githooks/pre-push), which sees the commits actually
// pushed; the wrapper requires it to be enabled before any command runs. This hook keeps that hook
// from being skipped by mistake or shortcut, and gives early, clear refusals. It does not make
// deliberate evasion impossible (a token assembled at run time, a program that pushes on its own):
// the boundary against that is on GitHub, in branch protection and required checks.
//
//  1. The text net: a command line that names what could keep git's hook from running is refused,
//     whatever the shell would do with it (hookToken: --no-verify and the abbreviations git accepts,
//     core.hooksPath, .githooks, a GIT_* variable outside a short safe list, git's push plumbing).
//     No command is exempt; text that only mentions them (a message, a search) is written with the
//     Edit or Write tools, or into a file.
//  2. The walker (parse.go) parses the command as bash with mvdan.cc/sh to find each git push and
//     the directory it runs in. It reads the words as bash expands them, and refuses a hook token
//     there too; it follows wrappers, shells, eval and the commands git runs for a subcommand, and
//     refuses what it cannot resolve and what can point git elsewhere or redefine a git command.
//  3. A push runs only in this clone or one of its worktrees (another repository may not run this
//     hook). The forms the gate cannot check are refused; then hack/sdlc/gate.sh runs on each pushed
//     commit as it is before the command. A ref that does not resolve yet is left to git's hook.
//
// Exit status 2 blocks the command and shows stderr to Claude Code; 0 lets it run.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type hookInput struct {
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"` // the terminal tool's directory, relative to the session's
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
	if token := hookToken(cmd); token != "" {
		return refuse(tokenRefusal(token))
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
		if mentionsGit(cmd) {
			return refuse("cannot parse the command as bash (the Bash tool may run another shell, such as zsh), so what it does with git cannot be checked; run git in a command bash can parse")
		}
		return 0
	}
	if a.Blocked != "" {
		return refuse(a.Blocked)
	}
	var problems []string
	if msg := checkSubcommands(a.Calls, root); msg != "" {
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

// gatePush returns why p may not run, or "" when it may. A push runs only in a work tree of this
// repository: root or one of its worktrees, which share its configuration and records. Another
// repository (a second clone, a bare one, a .git directory) may not run git's pre-push hook.
func gatePush(p push, root string) string {
	if p.Dir == "" {
		return "cannot tell which directory `git push` runs in (a cd that may fail without stopping the command, a variable, `cd -`, `popd` or `~user`); use `cd DIR && git push …` with a literal path, or `git -C DIR push …`"
	}
	if fi, err := os.Stat(p.Dir); err != nil || !fi.IsDir() {
		return fmt.Sprintf("`git push` would run in %s, which is not a directory here; cd to the repository with a literal path", p.Dir)
	}
	top, err := gitOut(p.Dir, "rev-parse", "--show-toplevel")
	if err != nil || !sameRepository(top, root) {
		return "`git push` would run in " + p.Dir + ", which is not a work tree of " + root + " or of one of its worktrees; from Claude Code, push only from there (another repository, a bare one or a git directory may not run git's pre-push hook)"
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
	gate := filepath.Join(top, "hack", "sdlc", "gate.sh")
	if _, err := os.Stat(gate); err != nil {
		return top + " has no hack/sdlc/gate.sh (a branch from before the SDLC gate?): check out a branch based on main"
	}
	if fi, err := os.Stat(filepath.Join(top, ".githooks", "pre-push")); err != nil || fi.Mode()&0o111 == 0 {
		return "git's pre-push hook in " + top + " is missing or not executable, so git would push without it: restore it before pushing"
	}
	// The gate's own files must be as committed: no change git status shows, and none it hides (a
	// file marked assume-unchanged or skip-worktree: a lower-case or S tag in git ls-files -v).
	status, errStatus := gitOut(top, "status", "--porcelain", "--", ".githooks", "hack/sdlc")
	tags, errTags := gitOut(top, "ls-files", "-v", "--", ".githooks", "hack/sdlc")
	if errStatus != nil || errTags != nil || status != "" || hidesChanges(tags) {
		return "the gate's own files in " + top + " (git's hook, hack/sdlc) have uncommitted changes, are marked assume-unchanged or skip-worktree, or git cannot list them: commit or restore them, and clear the marks, before pushing"
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
		if msg := baseGate(top, commit); msg != "" {
			problems = append(problems, msg)
		}
	}
	return strings.Join(problems, "\n")
}

// gateFiles are the paths whose change the gate of the change's base judges too: the gate cannot
// pass a change to itself.
var gateFiles = []string{".githooks", "hack/sdlc", ".claude/settings.json"}

// baseGate runs the gate of commit's base (its merge base with origin/main, replace refs ignored)
// on commit when the change edits the gate's own files, so a gate the change breaks cannot pass the
// change: the base's gate asks for the same check and review records. It returns why that gate
// refuses, or "" (also when the base has no gate yet). git's pre-push hook does the same on its own.
func baseGate(top, commit string) string {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", top}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	base, err := git("merge-base", commit, "refs/remotes/origin/main")
	if err != nil {
		return "cannot find the base of " + commit + " (its merge base with origin/main), whose gate would judge a change to the gate: fetch origin"
	}
	if _, err := git("cat-file", "-e", base+":hack/sdlc/gate.sh"); err != nil {
		return "" // the base has no gate yet
	}
	if _, err := git(append([]string{"diff", "--quiet", base, commit, "--"}, gateFiles...)...); err == nil {
		return "" // the gate's files are the base's
	}
	common, err := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "cannot find the git directory of " + top + ": " + err.Error()
	}
	tmp, err := os.MkdirTemp("", "sdlc-base-gate-")
	if err != nil {
		return "cannot run the base's gate: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	// The base's hack/sdlc, run with this repository's git directory (its records and refs) and the
	// extracted files as its work tree, so the base's gate sources the base's lib.sh.
	extract := exec.Command("sh", "-c", `git -C "$1" archive "$2" hack/sdlc | tar -xf - -C "$3"`, "sh", top, base, tmp)
	if out, err := extract.CombinedOutput(); err != nil {
		return "cannot extract the gate of the base " + base + ": " + strings.TrimSpace(string(out))
	}
	run := exec.Command(filepath.Join(tmp, "hack", "sdlc", "gate.sh"), commit)
	run.Dir = tmp
	run.Env = append(os.Environ(), "GIT_DIR="+common, "GIT_WORK_TREE="+tmp, "GIT_NO_REPLACE_OBJECTS=1")
	if out, err := run.CombinedOutput(); err != nil {
		return "this change edits the gate's own files (" + strings.Join(gateFiles, ", ") + "), so the gate of its base " + base +
			" judges it too, and refuses it:\n" + strings.TrimSpace(string(out))
	}
	return ""
}

// checkSubcommands refuses a git subcommand that is not a git command (builtin, in git's exec path
// or a git-* program on PATH): an alias. Its definition can come from anywhere git reads
// configuration, which the command itself can change on the way (a config file it writes, HOME,
// XDG_CONFIG_HOME, an include), so what it runs cannot be known here.
func checkSubcommands(calls []string, root string) string {
	if len(calls) == 0 {
		return ""
	}
	// The project's tools (git-lfs …) are in its bin/, first on PATH as hack/sdlc/check.sh puts it.
	cmd := exec.Command("git", "-C", root, "--list-cmds=builtins,main,others")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	list := string(out)
	if err != nil {
		return "cannot list git's commands (git --list-cmds) to tell them from aliases: " + err.Error()
	}
	known := map[string]bool{}
	for _, c := range strings.Fields(list) {
		known[c] = true
	}
	var aliases []string
	for _, c := range calls {
		if !known[c] && !slices.Contains(aliases, c) {
			aliases = append(aliases, c)
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	return "git " + strings.Join(aliases, ", git ") + ": not a git command but an alias (or a typo); the command could change what an alias runs, so run the git command it stands for"
}

// hidesChanges reports whether git ls-files -v output tags a file assume-unchanged (lower case) or
// skip-worktree (S): git status does not show such a file's changes.
func hidesChanges(tags string) bool {
	for _, line := range strings.Split(tags, "\n") {
		if line != "" && (line[0] == 'S' || (line[0] >= 'a' && line[0] <= 'z')) {
			return true
		}
	}
	return false
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
