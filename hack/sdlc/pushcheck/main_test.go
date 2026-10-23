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
		{name: "unparseable, git and push split by quotes", command: "git p''ush --no-verify origin main; echo \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "unparseable, a hook override", command: "GIT_CONFIG_COUNT=1 make release; echo \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "a push before a syntax error runs anyway", command: "git push origin main\necho \"oops", want: 2, reason: "cannot parse the command as bash"},
		{name: "zsh syntax", command: "print -l ${(f)x}; git push origin main", want: 2, reason: "cannot parse the command as bash"},
		{name: "push in a substitution", command: "out=$(git push 2>&1)", want: 2, reason: "command or process substitution"},
		{name: "bash -c script that does not parse", command: `bash -c 'git push "unterminated'`, want: 2, reason: "cannot parse the shell script"},
		{name: "GIT_* variable outside the repository", command: "export GIT_DIR=/x/.git; cd / && git push origin main", want: 2, reason: "GIT_DIR can point git"},
		{name: "--git-dir outside the repository", command: "cd / && git --git-dir=/x/.git push origin main", want: 2, reason: "git --git-dir can point git"},
		{name: "push from an unknown directory", command: `git -C "$D" push origin main`, want: 2, reason: "cannot tell which directory"},
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
}

// A repository holding this project is recognised by a pushed commit carrying the gate, or by the
// project's first commit; another repository is not.
func TestHoldsProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "first")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "hack", "sdlc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hack", "sdlc", "gate.sh"), []byte("#!/bin/sh\n"), 0o755))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "gate")

	work := t.TempDir()
	bare := filepath.Join(work, "bare.git")
	gitIn(t, work, "clone", "-q", "--bare", root, bare)
	assert.True(t, holdsProject(bare, []string{"HEAD"}, root), "a bare clone, by the gate in HEAD")
	assert.True(t, holdsProject(bare, []string{"no-such-ref"}, root), "a bare clone, by the first commit")

	shallow := filepath.Join(work, "shallow.git")
	gitIn(t, work, "clone", "-q", "--bare", "--depth", "1", "file://"+root, shallow)
	assert.True(t, holdsProject(shallow, []string{"HEAD"}, root), "a shallow clone without the first commit, by the gate")

	other := filepath.Join(work, "other")
	gitIn(t, work, "init", "-q", other)
	gitIn(t, other, "commit", "-q", "--allow-empty", "-m", "unrelated")
	assert.False(t, holdsProject(other, []string{"HEAD"}, root), "an unrelated repository")
}

// A push from a bare clone of the project is refused; one from an unrelated bare repository is not.
func TestGatePushBareClone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "first")
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitIn(t, root, "clone", "-q", "--bare", root, bare)
	assert.Contains(t, gatePush(push{Dir: bare, Sources: []string{"main"}}, root), "a bare repository holding this project's commits")
	assert.Empty(t, gatePush(push{Dir: bare, Sources: []string{"main"}, DryRun: true}, root), "a dry run pushes nothing")

	other := filepath.Join(t.TempDir(), "other.git")
	gitIn(t, root, "init", "-q", "--bare", other)
	assert.Empty(t, gatePush(push{Dir: other, Sources: []string{"main"}}, root))
}
