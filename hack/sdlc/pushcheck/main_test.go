package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenNet is part of the text net's refusal (tokenRefusal).
const tokenNet = "in a command line could keep git's pre-push hook from running"

// The hook fails closed: a command it cannot parse or follow is refused when it involves a push.
// (Pushes it can follow are checked end to end by hack/sdlc/test-sdlc.sh.)
func TestRunFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    int
		reason  string
	}{
		{name: "not a push", command: "echo hi", want: 0},
		{name: "a coprocess pushing from another directory", command: "cd / && coproc git push origin main", want: 2, reason: "which is not a work tree of"},
		{name: "unparseable without git", command: "echo 'push unterminated", want: 2, reason: "cannot parse the command as bash"},
		{name: "unparseable, with an alias of the shell's setup", command: "cd / && nosuch-gp origin main; print -l ${(f)x}", want: 2, reason: "cannot parse the command as bash"},
		{name: "a zsh glob qualifier bash cannot parse", command: "cd / && nosuch-gp origin main && ls *.go(.)", want: 2, reason: "cannot parse the command as bash"},
		{name: "unparseable, git without push", command: "x=gi; ${=x}t status; git log", want: 2, reason: "cannot parse the command as bash"},
		{name: "unparseable, git and push split by quotes", command: "git p''ush origin main; echo \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "a push before a syntax error runs anyway", command: "git push origin main\necho \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "zsh syntax", command: "print -l ${(f)x}; git push origin main", want: 2, reason: "cannot parse the command as bash"},
		{name: "zsh anonymous function with a brace body", command: "() { cd /x; } && git push origin a", want: 2, reason: "cannot parse the command as bash"},
		{name: "push in a substitution", command: "out=$(git push 2>&1)", want: 2, reason: "command or process substitution"},
		{name: "bash -c script that does not parse", command: `bash -c 'git push "unterminated'`, want: 2, reason: "cannot parse the shell script"},
		{name: "--git-dir outside the repository", command: "cd / && git --git-dir=/x/.git push origin main", want: 2, reason: "git --git-dir can point git"},
		{name: "push from an unknown directory", command: `git -C "$D" push origin main`, want: 2, reason: "cannot tell which directory"},
		{name: "push from another repository", command: "cd / && git push origin main", want: 2, reason: "which is not a work tree of"},
		// The text net: a hook token anywhere in the command line, whatever the shell does with it and
		// whether or not it parses.
		{name: "--no-verify in a commit message", command: "git commit -m 'never use --no-verify'", want: 2, reason: tokenNet},
		{name: "--no-verify in a command that does not parse", command: "git p''ush --no-verify origin main; echo \"oops", want: 2, reason: tokenNet},
		{name: "a hook override in a command that does not parse", command: "GIT_CONFIG_COUNT=1 make release; echo \"oops", want: 2, reason: tokenNet},
		{name: "a GIT_* variable set before a push", command: "export GIT_DIR=/x/.git; cd / && git push origin main", want: 2, reason: tokenNet},
		{name: "an abbreviation in a heredoc", command: "python3 - <<'EOF'\nprint('--no-veri')\nEOF", want: 2, reason: tokenNet},
		{name: "core.hooksPath in any case", command: "grep -rn CORE.HOOKSPATH docs", want: 2, reason: tokenNet},
		{name: "the hook's directory", command: "chmod -x .githooks/pre-push", want: 2, reason: tokenNet},
		{name: "push plumbing", command: "git send-pack origin HEAD", want: 2, reason: tokenNet},
		{name: "a remote helper", command: "printf 'push refs/heads/a:refs/heads/a\\n\\n' | git remote-https origin https://example.invalid/r.git", want: 2, reason: tokenNet},
		{name: "a remote helper by its program", command: "git-remote-http origin https://example.invalid/r.git <<<x", want: 2, reason: tokenNet},
		{name: "a remote helper handed to a function", command: `f() { "$@"; }; f git remote-codecommit origin x`, want: 2, reason: "git remote-codecommit pushes without running git's pre-push hook"},
		{name: "a safe GIT_* variable", command: "GIT_AUTHOR_NAME=x GIT_EDITOR=true git commit --amend --no-edit", want: 0},
		{name: "a git alias", command: "git ship origin main", want: 2, reason: "git ship: not a git command but an alias"},
		{name: "a git alias through a function", command: `f() { "$@"; }; f git ship origin main`, want: 2, reason: "git ship: not a git command but an alias"},
		{name: "an alias of the shell's setup outside this clone", command: "nosuch-gp origin main", want: 2, reason: "outside "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in, err := json.Marshal(map[string]any{"cwd": t.TempDir(), "tool_input": map[string]string{"command": tt.command}})
			require.NoError(t, err)
			var stderr bytes.Buffer
			assert.Equal(t, tt.want, run(bytes.NewReader(in), &stderr, t.TempDir()), stderr.String())
			assert.Contains(t, stderr.String(), tt.reason)
		})
	}
}

// A command word the walker does not know may be an alias that pushes (oh-my-zsh's gp): it runs in
// this clone, or a subdirectory, and not in another repository or a directory the hook cannot tell.
func TestRunUnknownWords(t *testing.T) {
	t.Parallel()
	root, other := t.TempDir(), t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "before the gate")
	gitIn(t, root, "branch", "old")
	for _, f := range []string{".githooks/pre-push", "hack/sdlc/gate.sh"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	gitIn(t, root, "worktree", "add", "-q", wt, "old") // a worktree of this clone without the gate
	gitIn(t, other, "init", "-q")
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	for _, tt := range []struct {
		command string
		want    int
		reason  string
	}{
		{"nosuch-gp origin main", 0, ""},
		{"cd " + wt + " && nosuch-gp origin main", 2, wt + " has no hack/sdlc/gate.sh"},
		// A function's unread code runs where the function is called; a builtin is no AUTO_CD.
		{`retry() { for i in 1 2 3; do "$@" && return 0; sleep 5; done; return 1; }; retry go test ./...`, 0, ""},
		{`f() { nosuch-tool; }; echo hi`, 0, ""},
		{`f() { "$@"; }; cd ` + other + ` && f ls`, 2, "a command named by a word the hook cannot read, which can push; it would run in " + other},
		{"pushd sub && true; popd", 0, ""},
		{"cd sub; ls; popd", 0, ""},
		// A substitution runs from the directory of its statement.
		{"cd " + other + " || exit 1; out=$(nosuch-gp origin main); cd " + root + " || exit 1; echo \"$out\"", 2, "it would run in " + other},
		{"v=$(nosuch-tool --version); cd sub; ls", 0, ""},
		{"f() { v=$(nosuch-tool); }; echo hi", 0, ""}, // a substitution in a body runs where f is called
		// A function that may cd, called in a substitution or defined in a branch, runs from here.
		{"f() { cd sub && ls; }; v=$(f)", 0, ""},
		{"if true; then f() { cd sub && ls; }; fi; f", 0, ""},
		{"if true; then f() { cd sub && ls; }; fi; cd " + other + " && f", 2, "`f` is no program on PATH"},
		{"f() { v=$(nosuch-gp origin main); }; cd " + other + " && f", 2, "`nosuch-gp` is no program on PATH, builtin or function the command defines, so it may be an alias or function of the shell's setup, which can push (oh-my-zsh's gp is `git push`); it would run in " + other},
		{"f() { cat <(nosuch-gp); }; cd " + other + " && f", 2, "it would run in " + other},
		{"f() { nosuch-tool; }; cd " + other + " && bash -c f", 2, "`nosuch-tool` is no program on PATH, builtin or function the command defines, so it may be an alias or function of the shell's setup, which can push (oh-my-zsh's gp is `git push`); it would run in " + other},
		// A cd in the body moves the code after it; an absolute or unknown one is kept.
		{"f() { cd " + other + " && nosuch-gp; }; f", 2, "it would run in " + other},
		{"f() { (cd " + other + " && nosuch-gp); }; f", 2, "it would run in " + other},
		{`f() { cd "$1" && nosuch-gp; }; f ` + other, 2, "a directory the hook cannot tell"},
		{"f() { cd sub && nosuch-gp; }; f", 0, ""},
		{"chpwd() { nosuch-gp; }; cd sub", 2, "a directory the hook cannot tell"}, // zsh calls it on each cd
		// A function runs the functions it calls as they are defined when it is called.
		{"g() { true; }; f() { g; }; g() { nosuch-gp; }; cd " + other + " && f", 2, "`f` runs a function defined, or defined again, after it"},
		{"g() { true; }; f() { g; }; cd " + other + " && f; g() { nosuch-gp; }; f", 2, "`f` runs a function defined, or defined again, after it"},
		{"f() { g; }; g() { :; }; f", 0, ""}, // a helper defined after its caller, in this clone
		// An alias of the setup of a function's name runs instead; a cd among a "$@" function's arguments moves.
		{"function nosuch-gp { :; }; cd " + other + " && nosuch-gp", 2, "`nosuch-gp` is a function the command defines, but an alias of that name in the shell's setup would run instead"},
		{`r() { "$@"; }; r cd ` + other + ` && git push origin HEAD`, 2, "cannot tell which directory `git push` runs in"},
		{"d() { true; }; trap d EXIT; d() { cd " + other + " && nosuch-gp; }", 2, "a trap action that runs a function defined, or defined again"},
		{"trap 'cd " + other + "' DEBUG; nosuch-gp origin main", 2, "a directory the hook cannot tell"},
		{`run() { "$@"; }; deploy() { run cd ` + other + `; }; deploy && git push origin HEAD`, 2, "cannot tell which directory `git push` runs in"},
		// A shell script handed to a program the walker does not model runs as written.
		{"arch -arm64 sh -c 'cd " + other + " && nosuch-gp'", 2, "it would run in " + other},
		{"/usr/bin/arch -arm64 sh -c 'nosuch-gp'", 0, ""}, // by path, a program on any host
		{`r() { "$@"; }; d() { cd ` + other + ` && nosuch-gp; }; r d`, 2, "it would run in " + other},
		// Nested substitutions run where their statement runs.
		{"x=$(echo $(nosuch-tool))", 0, ""},
		{"cd " + other + " || exit 1; x=$(echo $(nosuch-gp))", 2, "it would run in " + other},
		{"cd sub && nosuch-gp origin main", 0, ""},
		{"cd " + other + " && nosuch-gp origin main", 2, "it would run in " + other + ", outside " + root},
		{"nosuch-z x && nosuch-gp origin main", 2, "a directory the hook cannot tell"},
		{"cd " + other + " && command nosuch-gp origin main", 0, ""}, // command runs a program: no alias
		// A shell whose script the walker did not read, in this clone and outside it.
		{"echo 'git status' | env sh", 0, ""},
		{"cd " + other + " && echo 'git status' | env sh", 2, "`sh` reads a script from standard input that the hook did not read, which can push; it would run in " + other},
		{"cd " + other + " && echo 'git status' | xargs -0 sh -c", 2, "`sh` run by xargs or parallel takes its script from their input"},
		{"cd " + other + ` && sh -c "$X"`, 2, "`sh` runs a script the hook cannot read, which can push; it would run in " + other},
		{`sh -c "$X"`, 0, ""},
		{"cd " + other + " && bash --version", 0, ""},
		// A push in a container: a branch this clone does not have is no reason to let it through.
		{"kubectl exec pod -- git push origin feature", 2, "a git push run in a container"},
		{"docker exec ctr sh -c 'git push origin feature'", 2, "a git push run in a container"},
		{"git symbolic-ref --short refs/remotes/origin/HEAD", 0, ""}, // a read
		// A sourced file may cd: a literal cd after it places what follows.
		{". ./x.sh && nosuch-gp", 2, "placed after any `source`"},
		{". ./x.sh && cd " + root + " && nosuch-gp", 0, ""},
		// A runner in either order of definition, or maybe defined, given a cd.
		{`step() { run "$@"; }; run() { "$@"; }; step cd ` + other + ` && git push origin HEAD`, 2, "cannot tell which directory `git push` runs in"},
		{`log() { "$@"; } && log cd ` + other + ` && git push origin HEAD`, 2, "cannot tell which directory `git push` runs in"},
	} {
		in, err := json.Marshal(map[string]any{"cwd": root, "tool_input": map[string]string{"command": tt.command}})
		require.NoError(t, err)
		var stderr bytes.Buffer
		assert.Equal(t, tt.want, run(bytes.NewReader(in), &stderr, root), "%s: %s", tt.command, stderr.String())
		assert.Contains(t, stderr.String(), tt.reason, tt.command)
	}
	// Nor while the gate's own files differ from what is committed: git would run the edited hook.
	unread := func() (int, string) {
		in, err := json.Marshal(map[string]any{"cwd": root, "tool_input": map[string]string{"command": "nosuch-gp origin main"}})
		require.NoError(t, err)
		var stderr bytes.Buffer
		return run(bytes.NewReader(in), &stderr, root), stderr.String()
	}
	hook := filepath.Join(root, ".githooks", "pre-push")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0 # skips the gate\n"), 0o755))
	rc, msg := unread()
	assert.Equal(t, 2, rc)
	assert.Contains(t, msg, "have uncommitted changes")
	gitIn(t, root, "checkout", "-q", "--", ".githooks/pre-push")
	gitIn(t, root, "update-index", "--skip-worktree", ".githooks/pre-push")
	rc, msg = unread()
	assert.Equal(t, 2, rc)
	assert.Contains(t, msg, "skip-worktree")
	gitIn(t, root, "update-index", "--no-skip-worktree", ".githooks/pre-push")
	rc, _ = unread()
	assert.Equal(t, 0, rc)
}

// Hook input the hook cannot read could hold any command, so it is refused.
func TestRunUnreadableInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"not json", `{"tool_input":{"command":"git push origin main","cwd":1}}`, ""} {
		var stderr bytes.Buffer
		assert.Equal(t, 2, run(strings.NewReader(in), &stderr, t.TempDir()), in)
		assert.Contains(t, stderr.String(), "cannot read the hook input", in)
	}
}

// The terminal tool gives its own directory: relative to the session's, absolute, or under ~.
func TestRunTerminalDirectory(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	for _, tt := range []struct{ cwd, want string }{
		{"sub", "/nonexistent-session-dir/sub"},
		{"/nonexistent-abs", "/nonexistent-abs"},
		{"~/nonexistent-x", filepath.Join(home, "nonexistent-x")},
	} {
		in, err := json.Marshal(map[string]any{"cwd": "/nonexistent-session-dir", "tool_input": map[string]string{"command": "git push origin main", "cwd": tt.cwd}})
		require.NoError(t, err)
		var stderr bytes.Buffer
		assert.Equal(t, 2, run(bytes.NewReader(in), &stderr, t.TempDir()), tt.cwd)
		assert.Contains(t, stderr.String(), tt.want+", which is not a directory here", tt.cwd)
	}
}

// hookRuns only reads the index: a git status that refreshed a gate file's stale stat data would
// write the index back under index.lock while another command of Claude Code's may be using it.
func TestHookRunsLeavesTheIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	for _, f := range []string{".githooks/pre-push", "hack/sdlc/gate.sh"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(root, "hack/sdlc/gate.sh"), later, later)) // stale stat data
	index := filepath.Join(root, ".git", "index")
	require.NoError(t, os.Chtimes(index, earlier, earlier))
	assert.Empty(t, hookRuns(root))
	fi, err := os.Stat(index)
	require.NoError(t, err)
	assert.True(t, fi.ModTime().Before(time.Now().Add(-30*time.Minute)), "the index was written at %v", fi.ModTime())
}

// gitIn runs git in dir with no global or system configuration.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// checkSubcommands refuses any git subcommand that is not a git command, configured as an alias or
// not (the command could define it on the way), and allows git's own commands.
func TestCheckSubcommands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "config", "alias.ship", "!git push --no-verify origin main")
	gitIn(t, root, "config", "alias.lg", "log --oneline")
	for _, c := range []struct{ sub, want string }{
		{"ship", "git ship: not a git command but an alias"},
		{"lg", "git lg: not a git command but an alias"},  // a harmless alias, as the command could redefine it
		{"undefined", "git undefined: not a git command"}, // an alias the command may write into a config file
		{"status", ""},
		{"rev-parse", ""},
		{"log", ""},
		// git commands that push without the pre-push hook (the walker refuses them first, wherever
		// it finds them; this holds if one reaches the list another way).
		{"send-pack", "git send-pack pushes without running git's pre-push hook"},
		{"remote-https", "git remote-https pushes without running git's pre-push hook"},
	} {
		got := checkSubcommands([]string{c.sub}, root)
		if c.want == "" {
			assert.Empty(t, got, c.sub)
		} else {
			assert.Contains(t, got, c.want, c.sub)
		}
	}
	assert.Empty(t, checkSubcommands(nil, root))
	// The project's own tools in bin/ (git-lfs …) are git commands too.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "git-projtool"), []byte("#!/bin/sh\n"), 0o755))
	assert.Empty(t, checkSubcommands([]string{"projtool"}, root))
	assert.Contains(t, checkSubcommands([]string{"status", "a", "b", "a"}, root), "git a, git b:", "each alias named once")
	assert.Contains(t, checkSubcommands([]string{"status"}, filepath.Join(root, "nonexistent")), "cannot list git's commands",
		"a git that cannot list its commands refuses")
}

// A push runs only in this repository's work trees: root and its worktrees reach the gate; a second
// clone, a bare one, another repository, a .git directory or a plain directory are refused, dry runs
// and deletions too.
func TestGatePushOnlyFromThisRepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "first")
	work := t.TempDir()
	worktree := filepath.Join(work, "wt")
	gitIn(t, root, "worktree", "add", "-q", "-b", "wt", worktree)
	clone, bare, other, plain := filepath.Join(work, "clone"), filepath.Join(work, "bare.git"), filepath.Join(work, "other"), filepath.Join(work, "plain")
	gitIn(t, work, "clone", "-q", root, clone)
	gitIn(t, work, "clone", "-q", "--bare", root, bare)
	gitIn(t, work, "init", "-q", other)
	require.NoError(t, os.Mkdir(plain, 0o755))
	// A link in this clone into another repository: `link/..` is that repository to git and the kernel.
	require.NoError(t, os.Mkdir(filepath.Join(other, "sub"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(other, "sub"), filepath.Join(root, "link")))

	const notHere, notEnabled = "which is not a work tree of", "git's pre-push hook is not enabled in"
	for _, c := range []struct {
		dir  string
		p    push
		want string
	}{
		{root, push{Sources: []string{"main"}}, notEnabled},
		{root + "/.git/..", push{Sources: []string{"main"}}, notEnabled}, // through the git directory, not cleaned
		{worktree, push{Sources: []string{"wt"}}, notEnabled},
		{clone, push{Sources: []string{"main"}}, notHere},
		{clone, push{Sources: []string{"main"}, DryRun: true}, notHere},
		{clone, push{Deletes: true}, notHere},
		{bare, push{Sources: []string{"main"}}, notHere},
		{other, push{Sources: []string{"HEAD"}}, notHere},
		{filepath.Join(root, ".git"), push{Sources: []string{"main"}}, notHere},
		{plain, push{Sources: []string{"HEAD"}}, notHere},
		{root + "/link/..", push{Sources: []string{"HEAD"}}, notHere},
		{root + "/link/../..", push{Sources: []string{"HEAD"}}, notHere},
	} {
		c.p.Dir = c.dir
		assert.Contains(t, gatePush(c.p, root), c.want, c.dir)
	}
	// A dry run and deletions push no commit: nothing to gate in this clone, even with git's hook off.
	assert.Empty(t, gatePush(push{Dir: root, Sources: []string{"main"}, DryRun: true}, root))
	assert.Empty(t, gatePush(push{Dir: root, Deletes: true}, root))
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"main"}}, root), "(core.hooksPath is unset)")
	gitIn(t, root, "config", "core.hooksPath", "/elsewhere")
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"main"}}, root), `(core.hooksPath is "/elsewhere")`)
	// With git's hook enabled, a branch from before the gate has no gate to run.
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"main"}}, root), "has no hack/sdlc/gate.sh")
}

// With git's hook enabled, the gate's own files must be committed and the hook executable before
// the gate runs; then its verdict decides.
func TestGatePushChecksTheGateFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "base, without a gate")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gate, hook := filepath.Join(root, "hack", "sdlc", "gate.sh"), filepath.Join(root, ".githooks", "pre-push")
	settings := filepath.Join(root, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(gate), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(settings), 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\necho \"gate saw $1\"\nexit 1\n"), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(settings, []byte("{}\n"), 0o644))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	p := push{Dir: root, Sources: []string{"main"}}
	gitIn(t, root, "switch", "-q", "-c", "other")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "other")
	gitIn(t, root, "switch", "-q", "main")
	other, err := gitOut(root, "rev-parse", "other")
	require.NoError(t, err)

	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"other"}}, root), "gate saw "+other,
		"the gate runs on the pushed commit, not HEAD, and refuses it")
	require.NoError(t, os.Chmod(hook, 0o644))
	assert.Contains(t, gatePush(p, root), "missing or not executable")
	require.NoError(t, os.Chmod(hook, 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	assert.Contains(t, gatePush(p, root), "uncommitted changes", "an edited gate does not decide")
	gitIn(t, root, "commit", "-q", "-am", "gate passes")
	assert.Empty(t, gatePush(p, root))
	// The Claude Code hook's settings are a gate file too.
	require.NoError(t, os.WriteFile(settings, []byte("{\"hooks\": {}}\n"), 0o644))
	assert.Contains(t, gatePush(p, root), "uncommitted changes", "an edited hook setting does not decide")
	gitIn(t, root, "checkout", "-q", "--", ".claude/settings.json")
	assert.Empty(t, gatePush(p, root))
	// A form the gate cannot check ref by ref is refused, even with the gate passing.
	for _, command := range []string{"git push --tags origin", "git push --all origin", "git push origin :",
		"git push origin 'refs/heads/*:refs/heads/*'", "git push --frobnicate origin main", "git push -x origin main"} {
		a, err := findPushes(command, root)
		require.NoError(t, err)
		require.Len(t, a.Pushes, 1, command)
		require.NotEmpty(t, a.Pushes[0].Refused, command)
		assert.Equal(t, a.Pushes[0].Refused, gatePush(a.Pushes[0], root), command)
	}
	// An edit git status is told not to show does not decide either.
	for _, mark := range []string{"assume-unchanged", "skip-worktree"} {
		gitIn(t, root, "update-index", "--"+mark, "hack/sdlc/gate.sh")
		assert.Contains(t, gatePush(p, root), "assume-unchanged or skip-worktree", mark)
		gitIn(t, root, "update-index", "--no-"+mark, "hack/sdlc/gate.sh")
	}
	assert.Empty(t, gatePush(p, root))
	// A gate that refuses without a word still refuses.
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	gitIn(t, root, "commit", "-q", "-am", "silent gate")
	head, err := gitOut(root, "rev-parse", "main")
	require.NoError(t, err)
	assert.Contains(t, gatePush(p, root), "gate.sh "+head+": exit status 1")
}

// A change to the gate's own files is judged by its base's gate too, which a broken gate in the
// change cannot pass; a change elsewhere, and a base without a gate, are judged by the tree's gate.
func TestGatePushRunsTheBaseGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gate, hook := filepath.Join(root, "hack", "sdlc", "gate.sh"), filepath.Join(root, ".githooks", "pre-push")
	require.NoError(t, os.MkdirAll(filepath.Dir(gate), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	// The base's gate notes the work tree it runs in, and refuses a commit whose subject is "refuse".
	ran := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ngit rev-parse --show-toplevel >>" + ran + "\n" +
		"if [ \"$(git log -1 --format=%s \"$1\")\" = refuse ]; then echo \"gate refused $1\"; exit 1; fi\n"
	require.NoError(t, os.WriteFile(gate, []byte(script), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "base gate")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	ranIn := func() []string {
		b, _ := os.ReadFile(ran)
		_ = os.Remove(ran)
		return strings.Fields(string(b))
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	// A change that breaks the gate (it passes everything), with a subject the base's gate refuses.
	gitIn(t, root, "switch", "-q", "-c", "broken")
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	gitIn(t, root, "commit", "-q", "-am", "refuse")
	head, err := gitOut(root, "rev-parse", "HEAD")
	require.NoError(t, err)
	got := gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root)
	assert.Contains(t, got, "so the gate of its base")
	assert.Contains(t, got, "gate refused "+head, "the base's gate judges the pushed commit")
	where := ranIn()
	require.Len(t, where, 1, "only the base's gate wrote, once")
	assert.NotEqual(t, resolvedRoot, where[0], "the base's gate runs from its own files, not the work tree")

	// Replace refs choose neither the gate that judges (the base read as the broken commit) nor hide
	// the change from it (the pushed commit read as one with the base's files).
	base, err := gitOut(root, "rev-parse", "refs/remotes/origin/main")
	require.NoError(t, err)
	gitIn(t, root, "replace", base, head)
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root), "gate refused "+head, "base replaced")
	gitIn(t, root, "replace", "-d", base)
	fake := exec.Command("git", "-C", root, "commit-tree", base+"^{tree}", "-p", base, "-m", "fake")
	fake.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	fakeOut, err := fake.Output()
	require.NoError(t, err)
	gitIn(t, root, "replace", head, strings.TrimSpace(string(fakeOut)))
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root), "gate refused "+head, "pushed commit replaced")
	gitIn(t, root, "replace", "-d", head)
	ranIn()
	// Nor do grafts: one making the pushed commit a root and the base its child would make the pushed
	// commit its own base.
	grafts := filepath.Join(root, ".git", "info", "grafts")
	require.NoError(t, os.MkdirAll(filepath.Dir(grafts), 0o755))
	require.NoError(t, os.WriteFile(grafts, []byte(head+"\n"+base+" "+head+"\n"), 0o644))
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root), "gate refused "+head, "grafts")
	require.NoError(t, os.Remove(grafts))
	ranIn()

	// The gate checked out (broken, on this branch) judges a push of another branch, whose own commit
	// changes no gate file: the base's gate judges it too.
	gitIn(t, root, "switch", "-q", "-c", "plain", "main")
	require.NoError(t, os.WriteFile(filepath.Join(root, "PLAIN.md"), []byte("x\n"), 0o644))
	gitIn(t, root, "add", "PLAIN.md")
	gitIn(t, root, "commit", "-q", "-m", "refuse")
	plain, err := gitOut(root, "rev-parse", "HEAD")
	require.NoError(t, err)
	gitIn(t, root, "switch", "-q", "broken")
	got = gatePush(push{Dir: root, Sources: []string{"plain"}}, root)
	assert.Contains(t, got, "or the gate checked out to judge it")
	assert.Contains(t, got, "gate refused "+plain)
	ranIn()

	// Each of the gate's own paths sends the change to the base's gate, not only hack/sdlc.
	for _, edit := range []struct{ path, content string }{
		{".githooks/pre-push", "#!/bin/sh\nexit 0\n# edited\n"},
		{".claude/settings.json", "{}\n"},
	} {
		gitIn(t, root, "switch", "-q", "-C", "edit", "main")
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(edit.path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, edit.path), []byte(edit.content), 0o755))
		gitIn(t, root, "add", edit.path)
		gitIn(t, root, "commit", "-q", "-m", "refuse")
		sha, err := gitOut(root, "rev-parse", "HEAD")
		require.NoError(t, err)
		got := gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root)
		assert.Contains(t, got, "so the gate of its base", edit.path)
		assert.Contains(t, got, "gate refused "+sha, edit.path)
		ranIn()
	}

	// A change to the gate that its base's gate accepts goes through, judged by both gates.
	gitIn(t, root, "switch", "-q", "-C", "gatefix", "main")
	require.NoError(t, os.WriteFile(gate, []byte(script+"# a comment\n"), 0o755))
	gitIn(t, root, "commit", "-q", "-am", "fine")
	assert.Empty(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root))
	where = ranIn()
	assert.Len(t, where, 2, "the work tree's gate and the base's")
	assert.Contains(t, where, resolvedRoot)

	// A change elsewhere is the tree's gate's alone (the same script here, which notes the work tree).
	gitIn(t, root, "switch", "-q", "-c", "elsewhere", "main")
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("x\n"), 0o644))
	gitIn(t, root, "add", "README.md")
	gitIn(t, root, "commit", "-q", "-m", "fine")
	assert.Empty(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root))
	assert.Equal(t, []string{resolvedRoot}, ranIn(), "the base's gate did not run")

	// Without origin/main there is no base to ask: refused.
	gitIn(t, root, "update-ref", "-d", "refs/remotes/origin/main")
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"broken"}}, root), "cannot find the base of")
}

// When the base's gate cannot run, the push is refused, not waved through. (Not parallel: TMPDIR.)
func TestGatePushBaseGateCannotRun(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gate, hook := filepath.Join(root, "hack", "sdlc", "gate.sh"), filepath.Join(root, ".githooks", "pre-push")
	require.NoError(t, os.MkdirAll(filepath.Dir(gate), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "base gate")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 0\n# changed\n"), 0o755))
	gitIn(t, root, "commit", "-q", "-am", "change the gate")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	assert.Contains(t, gatePush(push{Dir: root, Sources: []string{"HEAD"}}, root), "cannot run the base's gate")
}

// Every pushed commit is gated, whatever its place among the sources.
func TestGatePushGatesEverySource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "base, without a gate")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gate, hook := filepath.Join(root, "hack", "sdlc", "gate.sh"), filepath.Join(root, ".githooks", "pre-push")
	require.NoError(t, os.MkdirAll(filepath.Dir(gate), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nif [ \"$(git log -1 --format=%s \"$1\")\" = other ]; then echo \"gate refused $1\"; exit 1; fi\n"), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	gitIn(t, root, "switch", "-q", "-c", "other")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "other")
	gitIn(t, root, "switch", "-q", "main")
	other, err := gitOut(root, "rev-parse", "other")
	require.NoError(t, err)

	// A source that does not resolve yet is left to git's hook; the others are still gated.
	for _, sources := range [][]string{{"main", "other"}, {"other", "main"}, {"other", "no-such-branch"}, {"no-such-branch", "other"}} {
		assert.Contains(t, gatePush(push{Dir: root, Sources: sources}, root), "gate refused "+other, sources)
	}
	a, err := findPushes("git push origin main other", root)
	require.NoError(t, err)
	require.Len(t, a.Pushes, 1)
	assert.Contains(t, gatePush(a.Pushes[0], root), "gate refused "+other)
	assert.Empty(t, gatePush(push{Dir: root, Sources: []string{"main"}}, root))
}

func TestHidesChanges(t *testing.T) {
	t.Parallel()
	assert.False(t, hidesChanges(""))
	assert.False(t, hidesChanges("H hack/sdlc/gate.sh\nH hack/sdlc/lib.sh"))
	assert.True(t, hidesChanges("H hack/sdlc/gate.sh\nh hack/sdlc/lib.sh"), "assume-unchanged")
	assert.True(t, hidesChanges("S hack/sdlc/gate.sh"), "skip-worktree")
	assert.True(t, hidesChanges("s hack/sdlc/gate.sh"), "both")
}
