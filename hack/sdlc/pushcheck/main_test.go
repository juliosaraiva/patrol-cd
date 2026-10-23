package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
		{name: "unparseable without git", command: "echo 'push unterminated", want: 0},
		{name: "unparseable, git without push", command: "x=gi; ${=x}t status; git log", want: 2, reason: "cannot parse the command as bash"},
		{name: "unparseable, git and push split by quotes", command: "git p''ush origin main; echo \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "a push before a syntax error runs anyway", command: "git push origin main\necho \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "zsh syntax", command: "print -l ${(f)x}; git push origin main", want: 2, reason: "cannot parse the command as bash"},
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
		{name: "a safe GIT_* variable", command: "GIT_AUTHOR_NAME=x GIT_EDITOR=true git commit --amend --no-edit", want: 0},
		{name: "a git alias", command: "git ship origin main", want: 2, reason: "git ship: not a git command but an alias"},
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
	} {
		c.p.Dir = c.dir
		assert.Contains(t, gatePush(c.p, root), c.want, c.dir)
	}
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
	gate, hook := filepath.Join(root, "hack", "sdlc", "gate.sh"), filepath.Join(root, ".githooks", "pre-push")
	require.NoError(t, os.MkdirAll(filepath.Dir(gate), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\necho \"gate saw $1\"\nexit 1\n"), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")
	gitIn(t, root, "config", "core.hooksPath", ".githooks")
	p := push{Dir: root, Sources: []string{"main"}}

	assert.Contains(t, gatePush(p, root), "gate saw ", "the gate runs on the pushed commit and refuses it")
	require.NoError(t, os.Chmod(hook, 0o644))
	assert.Contains(t, gatePush(p, root), "missing or not executable")
	require.NoError(t, os.Chmod(hook, 0o755))
	require.NoError(t, os.WriteFile(gate, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	assert.Contains(t, gatePush(p, root), "uncommitted changes", "an edited gate does not decide")
	gitIn(t, root, "commit", "-q", "-am", "gate passes")
	assert.Empty(t, gatePush(p, root))
	// An edit git status is told not to show does not decide either.
	for _, mark := range []string{"assume-unchanged", "skip-worktree"} {
		gitIn(t, root, "update-index", "--"+mark, "hack/sdlc/gate.sh")
		assert.Contains(t, gatePush(p, root), "assume-unchanged or skip-worktree", mark)
		gitIn(t, root, "update-index", "--no-"+mark, "hack/sdlc/gate.sh")
	}
	assert.Empty(t, gatePush(p, root))
}

func TestHidesChanges(t *testing.T) {
	t.Parallel()
	assert.False(t, hidesChanges(""))
	assert.False(t, hidesChanges("H hack/sdlc/gate.sh\nH hack/sdlc/lib.sh"))
	assert.True(t, hidesChanges("H hack/sdlc/gate.sh\nh hack/sdlc/lib.sh"), "assume-unchanged")
	assert.True(t, hidesChanges("S hack/sdlc/gate.sh"), "skip-worktree")
	assert.True(t, hidesChanges("s hack/sdlc/gate.sh"), "both")
}
