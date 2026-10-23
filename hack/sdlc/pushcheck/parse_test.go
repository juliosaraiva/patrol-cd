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
		{name: "brace expansion makes several arguments", command: "git push {origin,main}", want: []push{{Dir: "/repo", Sources: []string{"main"}}}},
		{name: "zsh brace group without spaces", command: "{git push origin a}", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "zsh brace group keeps a ref's braces", command: "{git push origin HEAD@{1}:refs/heads/x}", want: []push{{Dir: "/repo", Sources: []string{"HEAD@{1}"}}}},
		{name: "zsh - precommand", command: "- git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},

		// git push tag forms.
		{name: "push a tag by the tag keyword", command: "git push origin tag v0.1.0", want: []push{{Dir: "/repo", Sources: []string{"refs/tags/v0.1.0"}}}},
		{name: "push a branch and a tag", command: "git push origin ok tag v0.1.0", want: []push{{Dir: "/repo", Sources: []string{"ok", "refs/tags/v0.1.0"}}}},
		{name: "push a lightweight tag by name", command: "git push origin v0.1.1", want: []push{{Dir: "/repo", Sources: []string{"v0.1.1"}}}},

		// Directory: only what certainly runs moves it.
		{name: "cd then push", command: "cd /other && git push origin b", want: []push{{Dir: "/other", Sources: []string{"b"}}}},
		{name: "relative cd", command: "cd sub && git push", want: []push{{Dir: "/repo/sub", Sources: head}}},
		{name: "cd then ; push is unknown: the cd may fail", command: "cd sub; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd with two operands (zsh cd OLD NEW) is unknown", command: "cd /tmp x && git push", want: []push{{Dir: "", Sources: head}}},
		{name: "a guarded cd, then an unguarded one", command: "cd /x || exit 1; cd sub; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd || { …; exit; } keeps the cd", command: "cd /x || { echo no >&2; exit 1; }; git push", want: []push{{Dir: "/x", Sources: head}}},
		{name: "cd in a ; list inside bash -c", command: "bash -c 'cd /y; git push origin d'", want: []push{{Dir: "", Sources: []string{"d"}}}},
		{name: "home cd", command: "cd ~/w && git push", want: []push{{Dir: filepath.Join(home, "w"), Sources: head}}},
		{name: "pushd -n does not move", command: "pushd -n /p; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "popd", command: "pushd /p; popd; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "unknown directory", command: `cd "$TMP" && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a subshell does not leak", command: "(cd /other && git status); git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in the background does not leak", command: "cd /tmp & git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in a pipeline does not leak", command: "cd /tmp | true; git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd after &&", command: "false && cd /tmp; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd before || push leaves the dir unknown", command: "cd /tmp || git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd || exit then push commits the cd", command: "cd /x || exit 1; git push", want: []push{{Dir: "/x", Sources: head}}},
		{name: "cd in an || successor then && push is unknown", command: "true || cd /x && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "|| with no cd keeps the dir", command: "git fetch origin || true; git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "a push retried after ||", command: "git push 2>/dev/null || git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}, {Dir: "/repo", Sources: head}}},
		{name: "|| { …; exit 1; } with no cd", command: "make sdlc-check || { echo failed >&2; exit 1; }\ngit push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd in a subshell inside an if", command: "if [ -d ui ]; then (cd ui && ls); fi; git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in a substitution of a for list", command: `for p in $(cd internal && ls); do echo "$p"; done; git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "function whose body is a subshell", command: "t() ( cd ui && pnpm test ); t && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "function running its arguments", command: "retry() { for i in 1 2 3; do \"$@\" && return 0; sleep 5; done; return 1; }\nretry git fetch origin && git push origin a",
			want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd chain with a return to the repo", command: "cd ui && echo ok && cd .. && git push -u origin f", want: []push{{Dir: "/repo", Sources: []string{"f"}}}},
		{name: "cd in an if", command: "if false; then cd /tmp; fi; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a function", command: "f() { cd /tmp; }; f; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "push in a function", command: "f() { git push; }", want: []push{{Dir: "", Sources: head}}},
		{name: "if without cd", command: "if make sdlc-gate; then git push; fi", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "git -C", command: "git -C /x -c color.ui=never push origin HEAD", want: []push{{Dir: "/x", Sources: head}}},
		{name: "git -C with a directory that is not literal", command: `git -C "$D" push origin main`, want: []push{{Dir: "", Sources: []string{"main"}}}},
		{name: "two pushes", command: "git push origin a; git -C /b push", want: []push{{Dir: "/repo", Sources: []string{"a"}}, {Dir: "/b", Sources: head}}},

		// Commands that run a git push as a command: detected and gated, not refused outright.
		{name: "exec", command: "exec git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "eval of a literal script", command: "eval 'git push origin main'", want: []push{{Dir: "/repo", Sources: []string{"main"}}}},
		{name: "eval carries its cd", command: "eval 'cd /x' && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "eval's cd may fail before a ;", command: "eval 'cd /x'; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "trap action runs later, from an unknown directory", command: "trap -- 'git push origin a' EXIT", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "trap action runs after a later cd", command: "trap 'git push origin a' EXIT; cd /other && true", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd in a trap leaves the directory unknown", command: "trap 'cd /x' DEBUG; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a trap without a cd keeps it", command: "trap 'rm -f /tmp/x' EXIT; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd in a background job inside an if", command: "if true; then cd /tmp & fi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "git-push dashed binary", command: "git-push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd || die leaves the dir unknown (die may be undefined)", command: "cd /x || die; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd || return leaves the dir unknown (return at top level goes on)", command: "cd /x || return; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "mixed chain leaves the dir unknown", command: "cd /x || echo failed && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "push after a failed-branch cd is unknown", command: "cd /x && false || git push origin b", want: []push{{Dir: "", Sources: []string{"b"}}}},
		{name: "negated cd leaves the dir unknown", command: "! cd /x && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "cd in the last pipeline element leaks in zsh", command: "true | cd /x; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "push a tag by name after options", command: "git push --force origin tag v1", want: []push{{Dir: "/repo", Sources: []string{"refs/tags/v1"}}}},
		{name: "timed", command: "time git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "timeout wraps a push", command: "timeout 60 git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "nice wraps a push", command: "nice -n 5 git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "sudo wraps a push", command: "sudo -u ci git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "env unset then push", command: "env -i git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "env with an assignment that is not literal", command: `env PATH="$PWD/bin:$PATH" git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -exec a push", command: "find . -maxdepth 0 -exec git push origin a ;", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -execdir runs from each file's directory", command: "find . -name x -execdir git push origin a ;", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "kubectl exec a push", command: "kubectl exec pod -- git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A cd run by a child process (env, a wrapper, find -exec, kubectl exec) does not move the shell.
		{name: "env cd", command: "env cd /tmp && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "sudo cd", command: "sudo cd /tmp; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "nohup cd", command: "nohup cd /tmp; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -exec cd", command: "find . -maxdepth 0 -exec cd /tmp ';' ; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "kubectl exec cd", command: "kubectl exec p -- cd /tmp; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "in a while loop", command: "while false; do git push; done", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "in a case", command: "case x in x) git push;; esac", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "bash -c", command: `bash -lc 'cd /y && git push origin d'`, want: []push{{Dir: "/y", Sources: []string{"d"}}}},
		{name: "bash -co VALUE", command: `bash -co pipefail 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "heredoc to a shell", command: "bash <<'EOF'\ncd /y && git push origin d\nEOF", want: []push{{Dir: "/y", Sources: []string{"d"}}}},
		{name: "heredoc after -eo VALUE", command: "bash -eo pipefail <<'EOF'\ngit push origin d\nEOF", want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "here-string to a shell", command: `sh <<< 'git push origin d'`, want: []push{{Dir: "/repo", Sources: []string{"d"}}}},
		{name: "echo piped to a shell", command: "echo 'git push origin a' | sh", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cat heredoc piped to a shell", command: "cat <<'EOF' | bash\ngit push origin a\nEOF", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "echo of a push piped to a shell", command: "echo 'cd /y && git push origin main' | bash", want: []push{{Dir: "/y", Sources: []string{"main"}}}},

		// git push options.
		{name: "delete flag", command: "git push --delete origin old other", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete refspec only", command: "git push origin :old", want: []push{{Dir: "/repo", Deletes: true}}},
		{name: "delete flag negated", command: "git push -d --no-delete origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "dry run", command: "git push -n origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}, DryRun: true}}},
		{name: "dry run negated", command: "git push -n --no-dry-run origin x", want: []push{{Dir: "/repo", Sources: []string{"x"}}}},
		{name: "option with a value", command: "git push -o ci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "--force-with-lease takes no next argument", command: "git push --force-with-lease origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "--signed takes no next argument", command: "git push --signed origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "short value in the same word", command: "git push -fuoci.skip origin z", want: []push{{Dir: "/repo", Sources: []string{"z"}}}},
		{name: "help", command: "git push -h; git push --help", want: nil},
		{name: "unknown option", command: "git push --frobnicate origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "unknown or abbreviated git push option --frobnicate; spell options out in full"}}},
		{name: "glob refspec", command: "git push origin 'refs/heads/*:refs/heads/*'", want: []push{{Dir: "/repo", Sources: head,
			Refused: "a glob refspec pushes refs the gate cannot check one by one; push branches by name"}}},
		{name: "--tags", command: "git push --tags origin", want: []push{refusedAll("--tags")}},
		{name: "--all", command: "git push --all origin", want: []push{refusedAll("--all")}},
		{name: "matching refspec", command: "git push origin +:", want: []push{{Dir: "/repo", Sources: head,
			Refused: "the matching refspec ':' pushes refs the gate cannot check one by one; push branches by name"}}},
		{name: "--no-verify then --verify", command: "git push --no-verify --verify origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "noglob precommand", command: "noglob git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "zsh =cmd", command: "=git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "setsid wrapper", command: "setsid git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
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

// Commands refused as a whole, wherever they run, with the reason. (A push's own Refused, which
// only counts for a clone of this project, does not count here.)
func TestFindPushesBlocked(t *testing.T) {
	t.Parallel()
	const (
		notLiteral = "not literal cannot be checked"
		gitEnv     = "can point git at another repository or change its configuration"
		config     = "is not on the hook's list of safe settings"
		globalOpt  = "can point git at another repository or configuration, where the pre-push hook does not run"
		runsStep   = "runs per step"
		noVerify   = "--no-verify skips the pre-push hook"
		givenTo    = "which the hook does not model"
	)
	for _, tt := range []struct{ command, reason string }{
		// Pushes the walker cannot follow.
		{`out=$(git push origin main 2>&1); echo "$out"`, "command or process substitution"},
		{`echo "$(git push)"`, "command or process substitution"},
		{"cat <(git push origin main)", "command or process substitution"},
		{`"$GIT" push origin main`, "command word that is not literal"},
		{`git $FLAGS push origin main`, notLiteral},
		{`sub=push; git "$sub" origin main`, notLiteral},
		{`git -c "$X" push origin main`, "key the hook cannot read"},
		{"git rebase -x 'git push --no-verify origin a' HEAD~1", runsStep},
		{"git rebase --exec='git push origin a' main", runsStep},
		{"git submodule foreach 'git push --no-verify'", runsStep},
		{"git bisect run git push origin a", runsStep},
		{"tcsh -c 'git push --no-verify origin a'", "through tcsh"},
		{"fish -C 'git push --no-verify origin a'", "through fish"},
		{". ./env.sh; git push origin a", "sourced earlier"},
		{"command -v notgit; command -p git push origin a", "command with options"},
		{"exec -a x git push origin a", "exec with options"},
		{"env -S 'git push origin a'", "env with options"},
		// Fail-closed branches of a shell script.
		{`bash -c 'git push "unterminated'`, "cannot parse the shell script"},
		{`bash -c "$(cat s)" git push`, "not literal"},
		{"bash -s <<EOF\ngit push origin $B\nEOF", "a shell script that is not literal"},
		{`bash "$S" <<< 'git push origin a'`, "a shell argument that is not literal"},
		// Anything that points git at another repository or changes its configuration or hooks.
		{"GIT_DIR=/x/.git git push origin a", gitEnv},
		{"GIT_DIR=/x/.git git log", gitEnv},
		{"env GIT_DIR=/x/.git git push origin a", gitEnv},
		{"export GIT_DIR=/x/.git; cd /tmp && git push origin a", gitEnv},
		{"declare -x GIT_WORK_TREE=/x; git push origin a", gitEnv},
		{"export GIT_DIR; git push", gitEnv},
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
		{"git --config-env=core.hooksPath=H push origin a", config},
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
		// Regressions the fourth review caught: runners, trap, xargs-stdin, dynamic eval, export -p.
		{"echo --no-verify | xargs git push origin main", "through xargs"},
		{"echo a | xargs git push origin", "through xargs"},
		{`eval "$E git push --no-verify origin main"`, "through eval"},
		{"export -p GIT_DIR=/x/.git; cd /tmp && git push origin a", gitEnv},
		{"nocorrect git -c core.hooksPath=/dev/null push origin a", config},
		{"git submodule foreach --recursive 'git push --no-verify origin a'", "git submodule"},
		{"git for-each-repo --config=x push origin a", "git for-each-repo"},
		{"git filter-branch --tree-filter 'git push --no-verify origin a' HEAD", "git filter-branch"},
		{"git config color.x /y && git config --rename-section color core", config},
		{"git -c diff.x.command='git push --no-verify origin a' diff", config},
		{"myrunner git push origin a", "run through myrunner"},
		// Round 5: concrete parser gaps.
		{"git --shallow-file /dev/null push --no-verify origin main", "--shallow-file"},
		{"trap -- 'git push --no-verify origin a' EXIT", "--no-verify"},
		{"git --config-env=alias.ship=V ship origin a", config},
		{`git -c "$K=!git push" ship origin a`, "key the hook cannot read"},
		{"env -- GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.ship GIT_CONFIG_VALUE_0=push git ship", gitEnv},
		{"fd PATTERN -x git -c x.y=z push --no-verify origin main", "run through fd"},
		{"git push -u origin tag --no-verify t1", noVerify},
		{"git push -u origin tag --no-veri t1", "abbreviation of --no-verify"},
		{"git send-pack origin HEAD:refs/heads/tmp", "send-pack"},
		{"git http-push origin HEAD", "http-push"},
		{"/opt/homebrew/libexec/git-core/git-push --no-verify origin main", "--no-verify"},
		{"echo a | parallel git push origin", "through parallel"},
		{"readonly -p GIT_DIR=/x/.git; git push origin a", gitEnv},
		{"true || cd /x && git push --no-verify origin a", "--no-verify"},
		{`G=git; eval "$G push --no-verify origin main"`, "eval"},
		{"eval 'cd /x && git push --no-verify origin a'", "--no-verify"},
		{"eval '. ./env.sh'; git push origin a", "sourced earlier"},
		// Round 6: a push that skips git's hook is refused wherever the walker thinks it runs.
		{"git push --no-verify origin a", noVerify},
		{"git push {--no-verify,origin} main", noVerify},
		{"git push --no-veri origin a", "abbreviation of --no-verify"},
		{`git push origin "$BRANCH"`, "not literal: it could be --no-verify"},
		{"cd /var/root; git push --no-verify origin main", noVerify},
		{"cd /tmp x; git push --no-verify origin main", noVerify},
		{"env cd /tmp; git push --no-verify origin main", noVerify},
		{"cd /var/root || die; git push --no-verify origin main", noVerify},
		{"noglob git push --no-verify origin a", noVerify},
		{"=git push --no-verify origin a", noVerify},
		{"setsid git push --no-verify origin a", noVerify},
		{"trap 'git push --no-verify origin a' EXIT", noVerify},
		{"bash <<'EOF'\ncd /y\ngit push --no-verify origin d\nEOF", noVerify},
		{"echo 'cd .;git push --no-verify origin main' | bash", noVerify},
		// xargs options with an attached value only, and a git whose subcommand comes from input.
		{"echo --no-verify | xargs -i git push {} origin main", "through xargs"},
		{"echo '--no-verify main' | xargs -l git push origin", "through xargs"},
		{"echo x | xargs -e git push origin", "through xargs"},
		{"echo --no-verify | xargs --replace git push {} origin main", "through xargs"},
		{"echo push --no-verify origin main | xargs git -C .", "through xargs"},
		{"parallel ::: 'git push --no-verify origin a'", "through parallel"},
		// Assignments made inside words, arithmetic, let and namerefs.
		{"set -a; : ${GIT_DIR:=/x/.git}; git push origin wip", gitEnv},
		{": ${GIT_DIR=/x/.git}; git push origin a", gitEnv},
		{"(( GIT_DIR=5 )); git push", gitEnv},
		{"let GIT_DIR=5; git push", gitEnv},
		{"declare -n r=GIT_DIR; r=/x/.git; git push", gitEnv},
		// zsh spellings bash reads differently.
		{"{git push --no-verify origin a}", noVerify},
		{"{git -c core.hooksPath=/dev/null push origin a}", config},
		{"x='push --no-verify'; git $=x origin a", notLiteral},
		{"x=(push --no-verify); git $^x origin a", notLiteral},
		{"y='-c core.hooksPath=/dev/null push'; git $=y origin a", notLiteral},
		{"git $~x origin a", notLiteral},
		// --no-verify or core.hooksPath handed to a command the hook does not model: a shell alias or
		// function (oh-my-zsh's gp='git push', ggp) may pass it to git.
		{"gp --no-verify origin main", givenTo},
		{"ggp --no-verify", givenTo},
		{"_ gp --no-verify origin main", givenTo},
		{"gp --no-veri origin main", givenTo},
		{"gcfg core.hooksPath /dev/null", givenTo},
		// Round 7: config keys in the allowed sections that run a command.
		{"git -c diff.external='git push --no-verify origin a' diff", config},
		{"git -c Diff.External=x diff", config},
		{"git -c gc.recentObjectsHook='git push --no-verify origin a' gc", config},
		{"git config diff.external x", config},
		{"git -c merge.tool=x mergetool", config},
		// xargs options that take only an attached value, with git's subcommand from standard input.
		{"echo push --no-verify origin main | xargs -e git", "through xargs"},
		{"echo push --no-verify origin main | xargs -l git", "through xargs"},
		{"echo push --no-verify origin main | xargs -i git", "through xargs"},
		{"echo push --no-verify origin main | xargs --replace git", "through xargs"},
		{"git difftool -y -x 'git push --no-verify origin a' HEAD~1", "git difftool"},
		{"git difftool --extcmd='git push origin a' HEAD~1", "git difftool"},
		{"docker exec c git push --no-verify origin main", "run through docker"},
		{"declare -x GIT_DIR; git push", gitEnv},
		{"(( GIT_DIR++ )); git push", gitEnv},
		// Code the command defines under a name, run later through it.
		{`functions[gp]='git push --no-verify "$@"'; gp origin change`, "defines code"},
		{"aliases[gp]='git push --no-verify'; eval 'gp origin change'", "defines code"},
		{"alias gp='git push --no-verify'; eval 'gp origin change'", "defines code"},
		{"bash -c 'shopt -s expand_aliases\nalias gp=\"git -c core.hooksPath=/dev/null push\"\ngp origin change'", "defines code"},
		{"alias up='cd /tmp'; eval up; git push origin a", "defines code"},
		{`functions[gp]="$X"; gp origin change`, "cannot read"},
		{"aliases=(gp 'git push --no-verify'); eval 'gp origin a'", "cannot read"},
		{`alias gp="$G push --no-verify"; eval 'gp origin a'`, "alias defined from a value the hook cannot read"},
		{"hash -p /tmp/x git; git push origin a", "hash"},
		// The red team's seventh round: forms kept refused, with their own reason.
		{`eval "$(ssh-agent -s)" >/dev/null && git push origin HEAD`, "an eval of a dynamic string earlier in the command"},
		{`env GIT_DIR="$X" git push origin a`, gitEnv},
		{"git-config core.hooksPath /dev/null", config},
	} {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(tt.command, "/repo")
			require.NoError(t, err)
			assert.Contains(t, got.Blocked, tt.reason, "the command must be refused as a whole; pushes: %+v", got.Pushes)
		})
	}
}

// Ordinary commands on this repository must run: not refused, and not taken for a push. Many name
// "push" in a branch, path, pattern or variable; the word alone must not trip the hook.
func TestFindPushesAllowed(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		// git subcommands that run a command, with the push only in a ref, path or pattern.
		"git rebase origin/main docs/ci-docs-only-push",
		"git rebase --onto origin/main main fix/push-gate-abbrev",
		"git rebase --exec 'go test ./hack/sdlc/pushcheck/...' origin/main",
		"git bisect run go test ./hack/sdlc/pushcheck/",
		"git bisect start HEAD origin/main -- hack/sdlc/pushcheck",
		"bash -c 'git fetch origin && git rebase origin/main docs/ci-docs-only-push'",
		// xargs running a command that is not a git push.
		"git branch --list 'fix/push-*' | xargs git branch -D",
		"git ls-files hack/sdlc/pushcheck | xargs -n1 git log -1 --format='%h %s' --",
		"git diff --name-only -- hack/sdlc/pushcheck | xargs git add",
		"find docs -name '*.md' -print0 | xargs -0 grep -n \"git push\"",
		"ls hack/sdlc/pushcheck/*.go | xargs -n1 git blame --line-porcelain",
		// Wrappers around a command that is not a git push.
		"timeout 60 git fetch origin docs/ci-docs-only-push",
		"timeout 600 go test github.com/juliosaraiva/patrol-cd/hack/sdlc/pushcheck -count=1",
		"timeout 60 git log --oneline -5 -- hack/sdlc/pushcheck",
		// Interpreters: their code is out of the hook's reach (git's own hook gates any push).
		"awk '/push:/,/pull_request:/' .github/workflows/ci.yml",
		"awk 'NR<=20' .githooks/pre-push",
		"awk '/git push/ {print NR}' docs/guides/claude-code-sdlc.md",
		"perl -0pi -e 's/git push origin main/git push -u origin HEAD/g' docs/guides/claude-code-sdlc.md",
		"perl -ne 'print if /pushurl/' .git/config",
		"python3 -c \"import yaml; print(yaml.safe_load(open('.github/workflows/ci.yml')))\"",
		// The default case: git and push as data, not a command and its subcommand.
		"grep -n -w -e git -e push docs/guides/claude-code-sdlc.md",
		"kubectl -n patrol-cd exec deploy/patrol-cd-repo-server -- git -C /tmp/_patrol-repo log --oneline --grep push",
		"env -u GOFLAGS go test ./hack/sdlc/pushcheck/...",
		"git status --short && env -u GOFLAGS go test ./hack/sdlc/pushcheck/...",
		// printf, declare -p and read with GIT_* names as data, not as variables set.
		"printf '%s\\n' 'GIT_DIR=/tmp/x git push origin main'",
		"printf 'GIT_DIR=%s\\n' \"${GIT_DIR-}\"",
		"printf '| %s | %s |\\n' GIT_TAG \"$(git describe --tags --always)\"",
		"declare -p GIT_DIR GIT_WORK_TREE 2>/dev/null || echo unset",
		"typeset -p GIT_DIR 2>/dev/null",
		"GIT_PAGER= git log -3",
		"GIT_PAGER=cat git log -3",
		// git config reads, and git -c with a safe key, including a dynamic value.
		"for k in user.name core.hooksPath; do echo \"$k=$(git config \"$k\")\"; done",
		"git config remote.origin.url",
		"git -c safe.directory=\"$PWD\" status",
		"git -c user.email=\"$EMAIL\" commit -q -m 'chore: x'",
		"git --git-dir=\"$tmp/remote.git\" log --oneline -3",
		"git -c color.ui=never -c user.name=x log --oneline",
		"git -c core.quotePath=false diff --name-only",
		// Other ordinary commands with the word push in them.
		`grep -rn "git push" docs`,
		`gh pr create --title "x" --body "run git push --no-verify? no"`,
		"GIT_EDITOR=true git rebase --continue",
		"git config --get core.hooksPath",
		"git stash push -m wip",
		"git log --grep push",
		"git switch -c fix/new-branch",
		"cd ui && pnpm typecheck && cd .. && git status",
		"cd /Users/x/patrol-cd || exit 1; git status",
		"git config user.email \"$EMAIL\"",
		"git config --local user.name \"$(id -un)\"",
		"declare -p GIT_DIR GIT_WORK_TREE",
		// The fifth review's false refusals, each fixed: GIT_ is case-sensitive in the text net, the
		// net reads one command's words (not its siblings'), PATROL_SDLC_* is an ordinary variable,
		// and an exec runner without -- is refused only on a plain git push.
		"env -C /tmp cat testdata/git_push.json",
		"env -C ui pnpm test; grep -rn \"git push --no-verify\" docs",
		"PATROL_SDLC_BASE=HEAD~1 make sdlc-check",
		"docker run --rm -e GIT_PUSH_OPTION=1 alpine echo push",
		// --no-verify as data, or for a git command other than push.
		"grep -rn -- --no-verify docs",
		"man git push | col -b | grep -n -A3 'no-verify'",
		"echo --no-verify; printf '%s\\n' --no-verify",
		"go test ./hack/sdlc/pushcheck -run 'TestFindPushesBlocked/--no-verify'",
		"git commit --no-verify -m 'chore: x'",
		"alias ll='ls -la'",
		"trap 'rm -rf \"$tmp\"' EXIT; go test ./hack/sdlc/...",
		"hash -r",
		`GIT_SEQUENCE_EDITOR="sed -i '' '2,\$s/^pick/squash/'" git rebase -i HEAD~3`,
		"test/e2e/fixtures/git-server/rootfs/usr/local/bin/git-seed",
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			got, err := findPushes(command, "/repo")
			require.NoError(t, err)
			assert.Empty(t, got.Blocked, "pushes: %+v", got.Pushes)
			assert.Empty(t, got.Pushes)
		})
	}
}

func TestFindPushesCalls(t *testing.T) {
	t.Parallel()
	got, err := findPushes("git status; cd /x && git ship origin a; git --git-dir=/y lg; echo a | xargs git ls", "/repo")
	require.NoError(t, err)
	assert.Equal(t, []string{"status", "ship", "lg", "ls"}, got.Calls)
	// A git-* program run by its own name, and git in a container, are not alias lookups here.
	for _, command := range []string{
		"kubectl -n patrol-e2e run git-seed-manual --rm -i --restart=Never --image=patrol-cd/git-server:dev -- /usr/local/bin/git-seed",
		"test/e2e/fixtures/git-server/rootfs/usr/local/bin/git-seed",
		"kubectl -n patrol-system exec deploy/patrol-repo-server -- git lfs env",
	} {
		got, err := findPushes(command, "/repo")
		require.NoError(t, err)
		assert.Empty(t, got.Calls, command)
		assert.Empty(t, got.Blocked, command)
	}
	// A git subcommand (perhaps an alias) is found wherever the walker follows a command.
	for _, command := range []string{
		"x=$(git ship origin main)",
		"bash -c 'git ship origin main'",
		"bash <<'EOF'\ngit ship origin main\nEOF",
		"trap 'git ship origin main' EXIT",
		"eval 'git ship origin main'",
		"git rebase -x 'git ship origin main' HEAD~1",
		"git bisect run git ship origin main",
	} {
		got, err := findPushes(command, "/repo")
		require.NoError(t, err)
		assert.Contains(t, got.Calls, "ship", command)
	}
}

// With CDPATH set, a relative cd may go elsewhere, so its directory is unknown; an absolute one is not.
func TestFindPushesCDPATH(t *testing.T) {
	t.Setenv("CDPATH", t.TempDir())
	got, err := findPushes("cd sub && git push origin a", "/repo")
	require.NoError(t, err)
	assert.Equal(t, []push{{Dir: "", Sources: []string{"a"}}}, got.Pushes)
	got, err = findPushes("cd /abs/sub && git push origin a", "/repo")
	require.NoError(t, err)
	assert.Equal(t, []push{{Dir: "/abs/sub", Sources: []string{"a"}}}, got.Pushes)
}

// zsh's AUTO_CD (on in the Bash tool's zsh) runs a lone directory name as a cd.
func TestFindPushesAutoCD(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "ls"), 0o755))
	for _, tt := range []struct{ command, dir string }{
		{"ls && git push origin a", root}, // a command on PATH runs, even with a directory of its name
		{"sub && git push origin a", filepath.Join(root, "sub")},
		{"./sub && git push origin a", filepath.Join(root, "sub")},
		{"sub; git push origin a", ""},             // the cd may fail
		{"sub || true; git push origin a", ""},     // a chain with an AUTO_CD counts as a cd
		{"nosuchdir && git push origin a", root},   // no such directory: an unknown command
		{"sub x && git push origin a", root},       // with an argument it is a command, not a cd
		{"(sub && true); git push origin a", root}, // in a subshell
		{"if true; then sub; fi; git push origin a", ""},
	} {
		got, err := findPushes(tt.command, root)
		require.NoError(t, err)
		assert.Empty(t, got.Blocked, tt.command)
		assert.Equal(t, []push{{Dir: tt.dir, Sources: []string{"a"}}}, got.Pushes, tt.command)
	}
}

func TestFindPushesInvalidBash(t *testing.T) {
	t.Parallel()
	_, err := findPushes("git push 'unterminated", "/repo")
	require.Error(t, err)
}

func TestMain(m *testing.M) {
	_ = os.Unsetenv("CDPATH") // relative-cd cases must not depend on the developer's CDPATH
	os.Exit(m.Run())
}
