package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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

// The terminal tool gives its own directory, relative to the session's.
func TestRunTerminalDirectory(t *testing.T) {
	t.Parallel()
	in, err := json.Marshal(map[string]any{"cwd": "/nonexistent-session-dir", "tool_input": map[string]string{"command": "git push origin main", "cwd": "sub"}})
	require.NoError(t, err)
	var stderr bytes.Buffer
	assert.Equal(t, 2, run(bytes.NewReader(in), &stderr, t.TempDir()))
	assert.Contains(t, stderr.String(), "/nonexistent-session-dir/sub, which is not a directory here")
}

// checkAliases must refuse a git subcommand that is a configured alias for a push or a hook change,
// following alias chains, and allow an ordinary alias.
func TestCheckAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		require.NoError(t, cmd.Run(), "git %v", args)
	}
	run("init", "-q")
	run("config", "alias.ship", "!git push --no-verify origin main")
	run("config", "alias.off", "config core.hooksPath /dev/null")
	run("config", "alias.p", "ship") // a chain: p -> ship -> push
	run("config", "alias.lg", "log --oneline")

	for _, c := range []struct {
		sub  string
		want string
	}{
		{"ship", "that pushes or changes git's hooks"},
		{"off", "that pushes or changes git's hooks"},
		{"p", "that pushes or changes git's hooks"}, // followed through the chain
		{"lg", ""},
		{"status", ""}, // not an alias
	} {
		got := checkAliases([]gitCall{{Dir: root, Sub: c.sub}}, root)
		if c.want == "" {
			assert.Empty(t, got, "alias %q", c.sub)
		} else {
			assert.Contains(t, got, c.want, "alias %q", c.sub)
		}
	}
	// A call with an empty Dir is resolved against root.
	assert.Contains(t, checkAliases([]gitCall{{Sub: "ship"}}, root), "alias", "empty Dir resolves to root")
}
