package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindPushes(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	head := []string{"HEAD"}
	tests := []struct {
		name    string
		command string
		want    []push
	}{
		{name: "no push", command: "git status && go test ./...", want: nil},
		{name: "push in a word", command: "echo pushing; git stash push -m x", want: nil},
		{name: "push in quoted text", command: `git commit -m "docs: say git push" && echo 'git push origin main'`, want: nil},
		{name: "push in a heredoc", command: "python3 - <<'EOF'\nprint('before git push (docs)')\nEOF\ncat <<EOF\ngit push\nEOF", want: nil},
		{name: "push in a comment", command: "make test # then git push", want: nil},
		{name: "bare push", command: "git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "upstream branch", command: "git push -u origin feat/x 2>&1 | tail -3", want: []push{{Dir: "/repo", Sources: []string{"feat/x"}}}},
		{name: "after a commit with push in its message", command: `git add -A && git commit -q -m "ci: push faster" && git push -u origin fix/y`,
			want: []push{{Dir: "/repo", Sources: []string{"fix/y"}}}},
		{name: "refspecs", command: "git push origin +HEAD:refs/heads/x main", want: []push{{Dir: "/repo", Sources: []string{"HEAD", "main"}}}},
		{name: "cd then push", command: "cd /other && git push origin b", want: []push{{Dir: "/other", Sources: []string{"b"}}}},
		{name: "relative cd", command: "cd sub; git push", want: []push{{Dir: "/repo/sub", Sources: head}}},
		{name: "home cd", command: "cd ~/w && git push", want: []push{{Dir: filepath.Join(home, "w"), Sources: head}}},
		{name: "cd in a subshell does not leak", command: "(cd /other && git status); git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "git -C", command: "git -C /x -c core.x=y push origin HEAD", want: []push{{Dir: "/x", Sources: head}}},
		{name: "wrappers and assignments", command: "GIT_TRACE=1 command env -i A=b git push origin c", want: []push{{Dir: "/repo", Sources: []string{"c"}}}},
		{name: "bash -c", command: `bash -lc 'cd /y && git push origin d'`, want: []push{{Dir: "/y", Sources: []string{"d"}}}},
		{name: "unknown directory", command: `cd "$TMP" && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "unknown ref", command: `git push origin "$BRANCH"`, want: []push{{Dir: "/repo", Sources: []string{""}}}},
		{name: "delete flag", command: "git push --delete origin old other", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete refspec only", command: "git push origin :old", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete and push", command: "git push origin :old new", want: []push{{Dir: "/repo", Sources: []string{"new"}, Deletes: true}}},
		{name: "dry run", command: "git push -n origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}, DryRun: true}}},
		{name: "option with a value", command: "git push -o ci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "tags refused", command: "git push --tags origin", want: []push{{Dir: "/repo", Sources: head, Refused: "--tags pushes refs the gate cannot check one by one; push branches by name"}}},
		{name: "two pushes", command: "git push origin a; git -C /b push", want: []push{{Dir: "/repo", Sources: []string{"a"}}, {Dir: "/b", Sources: head}}},
		{name: "in an if", command: "if make sdlc-gate; then git push; fi", want: []push{{Dir: "/repo", Sources: head}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(tt.command, "/repo")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFindPushesInvalidBash(t *testing.T) {
	t.Parallel()
	_, err := findPushes("git push 'unterminated", "/repo")
	require.Error(t, err)
}
