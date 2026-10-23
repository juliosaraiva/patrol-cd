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
		{name: "push in a heredoc", command: "python3 - <<'EOF'\nprint('before')\nEOF\ncat <<EOF\ngit push\nEOF", want: nil},
		{name: "push in a comment", command: "make test # then git push", want: nil},
		{name: "bare push", command: "git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "upstream branch", command: "git push -u origin feat/x 2>&1 | tail -3", want: []push{{Dir: "/repo", Sources: []string{"feat/x"}}}},
		{name: "after a commit (the commit is caught by the git hook)", command: `git add -A && git commit -q -m "ci: push faster" && git push -u origin fix/y`,
			want: []push{{Dir: "/repo", Sources: []string{"fix/y"}}}},
		{name: "refspecs", command: "git push origin +HEAD:refs/heads/x main", want: []push{{Dir: "/repo", Sources: []string{"HEAD", "main"}}}},
		{name: "reflog and upstream syntax stay literal", command: "git push origin HEAD@{1}:refs/heads/x @{u}", want: []push{{Dir: "/repo", Sources: []string{"HEAD@{1}", "@{u}"}}}},

		// Quoting and expansion, as bash does them.
		{name: "quotes inside the subcommand", command: `git p""ush origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "escape inside the subcommand", command: `git pu\sh origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "ANSI-C quoting", command: `git $'push' origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "escaped command word", command: `\git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "brace expansion makes several arguments", command: "git push {--no-verify,origin} main", want: []push{{Dir: "/repo", Sources: []string{"main"},
			Refused: "--no-verify skips the pre-push hook"}}},
		{name: "brace expansion after a dry run", command: "git push -n {--no-dry-run,origin,a}", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},

		// Directory: only what certainly runs moves it.
		{name: "cd then push", command: "cd /other && git push origin b", want: []push{{Dir: "/other", Sources: []string{"b"}}}},
		{name: "cd with an option", command: "cd -P /other && git push", want: []push{{Dir: "/other", Sources: head}}},
		{name: "relative cd", command: "cd sub; git push", want: []push{{Dir: "/repo/sub", Sources: head}}},
		{name: "home cd", command: "cd ~/w && git push", want: []push{{Dir: filepath.Join(home, "w"), Sources: head}}},
		{name: "escaped space", command: `cd /a/with\ space && git push`, want: []push{{Dir: "/a/with space", Sources: head}}},
		{name: "pushd", command: "pushd /p && git push", want: []push{{Dir: "/p", Sources: head}}},
		{name: "pushd -n does not move", command: "pushd -n /p; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd -", command: "cd - && git push", want: []push{{Dir: "", Sources: head}}},
		{name: "popd", command: "pushd /p; popd; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "unknown directory", command: `cd "$TMP" && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a subshell does not leak", command: "(cd /other && git status); git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in the background does not leak", command: "cd /tmp & git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in a pipeline", command: "cd /tmp | true; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd after &&", command: "false && cd /tmp; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd before ||", command: "cd /tmp || git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd in an if", command: "if false; then cd /tmp; fi; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd in an else", command: "if true; then git push origin a; else cd /tmp; fi", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "cd in a loop", command: "for d in a; do git push origin a; cd /tmp; done", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "cd in a function", command: "f() { cd /tmp; }; f; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "push in a function", command: "f() { git push; }", want: []push{{Dir: "", Sources: head}}},
		{name: "if without cd", command: "if make sdlc-gate; then git push; fi", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "git -C", command: "git -C /x -c color.ui=never push origin HEAD", want: []push{{Dir: "/x", Sources: head}}},
		{name: "git -C with a directory that is not literal", command: `git -C "$D" push origin main`, want: []push{{Dir: "", Sources: []string{"main"}}}},
		{name: "two pushes", command: "git push origin a; git -C /b push", want: []push{{Dir: "/repo", Sources: []string{"a"}}, {Dir: "/b", Sources: head}}},

		// Commands around the push.
		{name: "wrappers that keep the push visible", command: "command env A=b git push origin c", want: []push{{Dir: "/repo", Sources: []string{"c"}}}},
		{name: "exec", command: "exec git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "timed", command: "time git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a while loop", command: "while false; do git push; done", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a case", command: "case x in x) git push;; esac", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "bash -c", command: `bash -lc 'cd /y && git push origin d'`, want: []push{{Dir: "/y", Sources: []string{"d"}}}},
		{name: "bash -c with an option after -c", command: `bash -c -e 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "bash -c after -o VALUE", command: `bash -o pipefail -c 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "bash -co VALUE", command: `bash -co pipefail 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "heredoc to a shell", command: "bash <<'EOF'\ncd /y\ngit push --no-verify origin d\nEOF", want: []push{{Dir: "/y", Sources: []string{"d"},
			Refused: "--no-verify skips the pre-push hook"}}},
		{name: "heredoc after -eo VALUE", command: "bash -eo pipefail <<'EOF'\ngit push origin d\nEOF", want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "heredoc with -s and arguments", command: "bash -s x <<'EOF'\ngit push origin d\nEOF", want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "heredoc read as /dev/stdin", command: "bash /dev/stdin <<'EOF'\ngit push origin d\nEOF", want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "unquoted heredoc removes backslashes", command: "bash <<EOF\ngit pu\\\\sh origin d\nEOF", want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "here-string to a shell", command: `sh <<< 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "a script file is not read", command: "bash push.sh <<'EOF'\ngit push origin d\nEOF\ngit push origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "a script file after --", command: "bash -- push.sh; git push origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},

		// git push options.
		{name: "delete flag", command: "git push --delete origin old other", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete refspec only", command: "git push origin :old", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete and push", command: "git push origin :old new", want: []push{{Dir: "/repo", Sources: []string{"new"}, Deletes: true}}},
		{name: "delete flag negated", command: "git push -d --no-delete origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "dry run", command: "git push -n origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}, DryRun: true}}},
		{name: "dry run negated", command: "git push -n --no-dry-run origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "option with a value", command: "git push -o ci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "options with values", command: "git push --recurse-submodules check --repo origin -uo a=b --push-option=c=d -- origin z",
			want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "--force-with-lease takes no next argument", command: "git push --force-with-lease origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "--signed takes no next argument", command: "git push --signed origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "short value in the same word", command: "git push -fuoci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "help", command: "git push -h; git push --help", want: nil},
		{name: "--verify after --no-verify", command: "git push --no-verify --verify origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "--no-verify after a negated dry run", command: "git push -n --no-dry-run --no-verify origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "--no-verify skips the pre-push hook"}}},
		{name: "abbreviated --no-verify", command: "git push --no-veri origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "unknown or abbreviated git push option --no-veri (git accepts abbreviations, such as one of --no-verify); spell options out in full"}}},
		{name: "unknown short option", command: "git push -x origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "unknown git push option -x; spell options out in full"}}},
		{name: "argument that is not literal", command: `git push origin "$BRANCH"`, want: []push{{Dir: "/repo", Sources: head,
			Refused: "`git push` has an argument that is not literal: it could be an option, or a ref the gate cannot resolve; " + runAlone}}},
		{name: "glob argument", command: "git push origin [-]-no-verify main", want: []push{{Dir: "/repo", Sources: []string{"main"},
			Refused: "`git push` has an argument that is not literal: it could be an option, or a ref the gate cannot resolve; " + runAlone}}},
		{name: "--tags", command: "git push --tags origin", want: []push{refusedAll("--tags")}},
		{name: "--tags negated", command: "git push --tags --no-tags origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "--all", command: "git push --all origin", want: []push{refusedAll("--all")}},
		{name: "--branches", command: "git push --branches origin", want: []push{refusedAll("--branches")}},
		{name: "--mirror", command: "git push --mirror origin", want: []push{refusedAll("--mirror")}},
		{name: "matching refspec", command: "git push origin +:", want: []push{{Dir: "/repo", Sources: head,
			Refused: "the matching refspec ':' pushes refs the gate cannot check one by one; push branches by name"}}},
		{name: "--no-verify", command: "git push --no-verify origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}, Refused: "--no-verify skips the pre-push hook"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(tt.command, "/repo")
			require.NoError(t, err)
			assert.Empty(t, got.Blocked)
			assert.Equal(t, tt.want, got.Pushes)
		})
	}
}

// Commands refused as a whole, wherever they run, with the reason.
func TestFindPushesBlocked(t *testing.T) {
	t.Parallel()
	const (
		notLiteral = "not literal cannot be checked"
		gitEnv     = "can point git at another repository or change its configuration"
		config     = "is not on the hook's list of safe settings"
		globalOpt  = "can point git at another repository or configuration, where the pre-push hook does not run"
	)
	for _, tt := range []struct{ command, reason string }{
		// Pushes the walker cannot follow.
		{`out=$(git push origin main 2>&1); echo "$out"`, "command or process substitution"},
		{`echo "$(git push)"`, "command or process substitution"},
		{`export R=$(git push)`, "command or process substitution"},
		{"cat <(git push origin main)", "command or process substitution"},
		{"timeout 120 git push origin main", "through timeout"},
		{"sudo git push origin main", "through sudo"},
		{"nice -n 5 git push", "through nice"},
		{"xargs git push origin < refs", "through xargs"},
		{"echo push --no-verify origin a | xargs git", "through xargs"},
		{"eval 'git push origin main'", "through eval"},
		{`eval $'git \x70ush origin main'`, "through eval"},
		{"nohup git push &", "through nohup"},
		{"trap 'git push --no-verify origin a' EXIT", "through trap"},
		{`python3 -c "import os; os.system('git push --no-verify origin a')"`, "through python3"},
		{"env -u GIT_DIR git push origin main", "env with options"},
		{"env -i git push origin main", "env with options"},
		{"command -p git push origin a", "command with options"},
		{"exec -a x git push origin a", "exec with options"},
		{"setsid git push origin a", "through setsid"},
		{"find . -maxdepth 0 -exec git push origin a ;", "run through find"},
		{`"$GIT" push origin main`, "command word that is not literal"},
		{`git $FLAGS push origin main`, notLiteral},
		{`sub=push; git "$sub" origin main`, notLiteral},
		{`git -c "$X" push origin main`, notLiteral},
		{"git rebase -x 'git push --no-verify origin a' HEAD~1", "through git rebase"},
		{"git submodule foreach 'git push --no-verify'", "through git submodule"},
		{"tcsh -c 'git push --no-verify origin a'", "through tcsh"},
		{"fish -C 'git push --no-verify origin a'", "through fish"},
		{". ./env.sh; git push origin a", "sourced earlier"},
		// Fail-closed branches of a shell script.
		{`bash -c 'timeout 60 git push --no-verify origin a'`, "through timeout"},
		{`bash -c 'git push "unterminated'`, "cannot parse the shell script"},
		{`bash -c "$(cat s)" git push`, "a shell argument that is not literal"},
		{"cat <<'EOF' | bash\ngit push origin a\nEOF", "a script piped to a shell"},
		{"git branch --format='git push origin %(refname:short)' | bash -", "a script piped to a shell"},
		{"echo 'git push origin a' | sh", "a script piped to a shell"},
		{"bash -s <<EOF\ngit push origin $B\nEOF", "a shell script that is not literal"},
		{`bash "$S" <<< 'git push origin a'`, "a shell argument that is not literal"},
		// Anything that points git at another repository or changes its configuration or hooks.
		{"GIT_DIR=/x/.git git push origin a", gitEnv},
		{"GIT_DIR=/x/.git git log", gitEnv},
		{"env GIT_DIR=/x/.git git push origin a", gitEnv},
		{"export GIT_DIR=/x/.git; cd /tmp && git push origin a", gitEnv},
		{"declare -x GIT_WORK_TREE=/x; git push origin a", gitEnv},
		{"set -a; GIT_DIR=/x/.git; git push", gitEnv},
		{"read GIT_DIR <<< /x/.git; git push", gitEnv},
		{"printf -v GIT_DIR /x/.git; git push", gitEnv},
		{"for GIT_DIR in /x/.git; do git push; done", gitEnv},
		{"export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null; bash release.sh", gitEnv},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.ship GIT_CONFIG_VALUE_0=push git ship origin a", gitEnv},
		{"GIT_PAGER='git push --no-verify' git log", gitEnv},
		{"git --git-dir=/x/.git push origin a", globalOpt},
		{"git --work-tree /x push origin a", globalOpt},
		{"git --bare push origin a", globalOpt},
		{"git --config-env=core.hooksPath=H push origin a", globalOpt},
		{"git -c core.hooksPath=/dev/null push origin a", config},
		{"git -c alias.ship='push --no-verify' ship origin a", config},
		{"git -c include.path=/x.cfg push origin a", config},
		{"git -c includeIf.onbranch:main.path=/x.cfg push origin a", config},
		{"git -c core.pager='git push' log", config},
		{"git config core.hooksPath /dev/null && git push origin a", config},
		{"git config alias.ship 'push --no-verify'", config},
		{"git config --local Alias.Ship push", config},
		{"git config set include.path /x.cfg", config},
		{"git config --add include.path /x.cfg", config},
		{"git config --edit", config},
	} {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(tt.command, "/repo")
			require.NoError(t, err)
			assert.Contains(t, got.Blocked, tt.reason, "pushes: %+v", got.Pushes)
		})
	}
}

// Commands that must not be refused: the hook sees every command Claude Code runs.
func TestFindPushesAllowed(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"git commit -q -m \"$(cat <<'EOF'\nfix(sdlc): say git push --no-verify\nEOF\n)\"",
		`grep -rn "git push" docs`,
		`gh pr create --title "x" --body "run git push --no-verify? no"`,
		"git -c color.ui=never -c user.name=x log --oneline",
		"git -c core.quotePath=false diff --name-only",
		"GIT_PAGER=cat git log -1",
		"GIT_EDITOR=true git rebase --continue",
		"git config --get core.hooksPath",
		"git config get alias.ship",
		"git config core.hooksPath",
		"git config user.email t@example.invalid",
		"git config --list --show-origin",
		"make sdlc-git-hooks",
		`printf '%s\n' a b`,
		"for f in a b; do echo $f; done",
		`git -C "$D" status`,
		"bash hack/sdlc/test-sdlc.sh",
		"git log --grep push",
		"git stash push -m wip",
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(command, "/repo")
			require.NoError(t, err)
			assert.Empty(t, got.Blocked)
			assert.Empty(t, got.Pushes)
		})
	}
}

func TestFindPushesCalls(t *testing.T) {
	t.Parallel()
	got, err := findPushes("git status; cd /x && git ship origin a; git --git-dir=/y lg", "/repo")
	require.NoError(t, err)
	assert.Equal(t, []gitCall{{Dir: "/repo", Sub: "status"}, {Dir: "/x", Sub: "ship"}, {Dir: "", Sub: "lg"}}, got.Calls)
}

func TestFindPushesInvalidBash(t *testing.T) {
	t.Parallel()
	_, err := findPushes("git push 'unterminated", "/repo")
	require.Error(t, err)
}
