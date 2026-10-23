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
	refusedAll := func(flag string) push {
		return push{Dir: "/repo", Sources: head, Refused: flag + " pushes refs the gate cannot check one by one; push branches by name"}
	}
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
		{name: "after a commit (the commit is caught by the git hook)", command: `git add -A && git commit -q -m "ci: push faster" && git push -u origin fix/y`,
			want: []push{{Dir: "/repo", Sources: []string{"fix/y"}}}},
		{name: "refspecs", command: "git push origin +HEAD:refs/heads/x main", want: []push{{Dir: "/repo", Sources: []string{"HEAD", "main"}}}},
		{name: "cd then push", command: "cd /other && git push origin b", want: []push{{Dir: "/other", Sources: []string{"b"}}}},
		{name: "cd with an option", command: "cd -P /other && git push", want: []push{{Dir: "/other", Sources: head}}},
		{name: "relative cd", command: "cd sub; git push", want: []push{{Dir: "/repo/sub", Sources: head}}},
		{name: "home cd", command: "cd ~/w && git push", want: []push{{Dir: filepath.Join(home, "w"), Sources: head}}},
		{name: "pushd", command: "pushd /p && git push", want: []push{{Dir: "/p", Sources: head}}},
		{name: "cd -", command: "cd - && git push", want: []push{{Dir: "", Sources: head}}},
		{name: "popd", command: "pushd /p; popd; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "escaped space is not taken literally", command: `cd /a/with\ space && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a subshell does not leak", command: "(cd /other && git status); git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "git -C", command: "git -C /x -c core.x=y push origin HEAD", want: []push{{Dir: "/x", Sources: head}}},
		{name: "wrappers that keep the push visible", command: "command env A=b git push origin c", want: []push{{Dir: "/repo", Sources: []string{"c"}}}},
		{name: "bash -c", command: `bash -lc 'cd /y && git push origin d'`, want: []push{{Dir: "/y", Sources: []string{"d"}}}},
		{name: "bash -c with an option after -c", command: `bash -c -e 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "unknown directory", command: `cd "$TMP" && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "unknown ref", command: `git push origin "$BRANCH"`, want: []push{{Dir: "/repo", Sources: []string{""}}}},
		{name: "delete flag", command: "git push --delete origin old other", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete refspec only", command: "git push origin :old", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete and push", command: "git push origin :old new", want: []push{{Dir: "/repo", Sources: []string{"new"}, Deletes: true}}},
		{name: "dry run", command: "git push -n origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}, DryRun: true}}},
		{name: "option with a value", command: "git push -o ci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "--tags", command: "git push --tags origin", want: []push{refusedAll("--tags")}},
		{name: "--all", command: "git push --all origin", want: []push{refusedAll("--all")}},
		{name: "--branches", command: "git push --branches origin", want: []push{refusedAll("--branches")}},
		{name: "--mirror", command: "git push --mirror origin", want: []push{refusedAll("--mirror")}},
		{name: "matching refspec", command: "git push origin +:", want: []push{{Dir: "/repo", Sources: head,
			Refused: "the matching refspec ':' pushes refs the gate cannot check one by one; push branches by name"}}},
		{name: "--no-verify", command: "git push --no-verify origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}, Refused: "--no-verify skips the pre-push hook"}}},
		{name: "hooksPath override", command: "git -c core.hooksPath=/dev/null push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "-c core.hooksPath would skip the pre-push hook"}}},
		{name: "git-dir", command: "git --git-dir=/x/.git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "--git-dir can point git at another repository or configuration; push from the work tree itself"}}},
		{name: "GIT_DIR", command: "GIT_DIR=/x/.git git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "GIT_* environment variables can point git at another repository or configuration; push without them"}}},
		{name: "two pushes", command: "git push origin a; git -C /b push", want: []push{{Dir: "/repo", Sources: []string{"a"}}, {Dir: "/b", Sources: head}}},
		{name: "in an if", command: "if make sdlc-gate; then git push; fi", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a for loop", command: "for b in x; do git push origin x; done", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "in a while loop", command: "while false; do git push; done", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a case", command: "case x in x) git push;; esac", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "timed", command: "time git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a function", command: "f() { git push; }", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "exec", command: "exec git push", want: []push{{Dir: "/repo", Sources: head}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(tt.command, "/repo")
			require.NoError(t, err)
			assert.Empty(t, got.Hidden)
			assert.Equal(t, tt.want, got.Pushes)
		})
	}
}

// Pushes the walker cannot follow are reported as hidden, so the hook refuses the command.
func TestFindPushesHidden(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		`out=$(git push origin main 2>&1); echo "$out"`,
		`echo "$(git push)"`,
		`export R=$(git push)`,
		"cat <(git push origin main)",
		"timeout 120 git push origin main",
		"sudo git push origin main",
		"nice -n 5 git push",
		"xargs git push origin < refs",
		"eval 'git push origin main'",
		"nohup git push &",
		"env -u GIT_DIR git push origin main",
		`bash -c "git push origin $B"`,
		`"$GIT" push origin main`,
		`\git push origin main`,
		`git $FLAGS push origin main`,
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(command, "/repo")
			require.NoError(t, err)
			assert.NotEmpty(t, got.Hidden, "pushes: %+v", got.Pushes)
		})
	}
}

func TestFindPushesInvalidBash(t *testing.T) {
	t.Parallel()
	_, err := findPushes("git push 'unterminated", "/repo")
	require.Error(t, err)
}
