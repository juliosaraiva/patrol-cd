package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{name: "command -p and -- run the command", command: "command -p git -C /other push origin main", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "command -v prints, then command -p runs", command: "command -v notgit; command -p git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "command -p cd in a bash script", command: "bash -c 'command -p cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "builtin -- cd", command: "builtin -- cd /x && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},

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
		{name: "pushd -n does not move, but the hook does not track the stack", command: "pushd -n /p && git push", want: []push{{Dir: "", Sources: head}}},
		{name: "popd goes back to a directory the hook does not track", command: "cd /x && pushd /p && popd && git push", want: []push{{Dir: "", Sources: head}}},
		{name: "a cd that may be skipped, in a group", command: "{ true || cd /x; } && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd in an if, then &&", command: "if false; then cd /x; fi && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a loop's cd reaches its next iteration", command: "for i in 1 2; do git push origin a && cd /x; done", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd after a negated cd", command: "! cd /x && cd /y && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a negated cd at the end of a group", command: "{ true && ! cd /x; } && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a chain of cds in a group leaves the directory unknown after it", command: "{ cd /a && cd /b; } && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// What a child process sources or evals does not reach the shell; what the shell does, does.
		{name: "env runs a source in a child", command: "env . ./x.sh && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a wrapper runs a source in a child", command: "nohup . ./x.sh && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a sourced file can cd", command: ". ./x.sh && kubectl exec p -- git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a dynamic eval can cd", command: `eval "$E" && kubectl exec p -- git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "sudo -D runs the command elsewhere", command: "sudo -D /tmp/other git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "sudo --chdir", command: "sudo --chdir=/tmp/other git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "sudo -i runs in the target user's home", command: "sudo -i git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "sudo -A takes no value", command: "sudo -A git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A branch after a condition can run whether the condition's cd worked or not.
		{name: "a negated cd in an if", command: "if ! cd /x; then git push origin a; fi", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "the else of a cd", command: "if cd /x; then :; else git push origin a; fi", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "an until body", command: "until cd /x; do git push origin a; done", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "zsh runs a function's last pipeline element in this shell", command: "f() { true | cd /x; }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "unknown short option", command: "git push -x origin a", want: []push{{Dir: "/repo", Sources: []string{"a"},
			Refused: "unknown git push option -x; spell options out in full"}}},
		{name: "unknown directory", command: `cd "$TMP" && git push`, want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a subshell does not leak", command: "(cd /other && git status); git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in the background does not leak", command: "cd /tmp & git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in a pipeline does not leak", command: "cd /tmp | true; git push", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd after &&", command: "false && cd /tmp; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd before || push leaves the dir unknown", command: "cd /tmp || git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd || exit then push commits the cd", command: "cd /x || exit 1; git push", want: []push{{Dir: "/x", Sources: head}}},
		{name: "CDPATH set in the command makes a relative cd unknown", command: "CDPATH=/tmp/other; cd sub && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "watch runs its command through sh -c", command: "watch -n 5 'cd /elsewhere && git push origin main'", want: []push{{Dir: "/elsewhere", Sources: []string{"main"}}}},
		{name: "watch -x runs an argv", command: "watch -x git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd in an || successor then && push is unknown", command: "true || cd /x && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "|| with no cd keeps the dir", command: "git fetch origin || true; git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "a push retried after ||", command: "git push 2>/dev/null || git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}, {Dir: "/repo", Sources: head}}},
		{name: "|| { …; exit 1; } with no cd", command: "make sdlc-check || { echo failed >&2; exit 1; }\ngit push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd in a subshell inside an if", command: "if [ -d ui ]; then (cd ui && ls); fi; git push -u origin HEAD", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "cd in a substitution of a for list", command: `for p in $(cd internal && ls); do echo "$p"; done; git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "function whose body is a subshell", command: "t() ( cd ui && pnpm test ); t && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "function running its arguments", command: "retry() { for i in 1 2 3; do \"$@\" && return 0; sleep 5; done; return 1; }\nretry git fetch origin && git push origin a",
			want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "cd chain with a return to the repo", command: "cd ui && echo ok && cd .. && git push -u origin f", want: []push{{Dir: "/repo/ui/..", Sources: []string{"f"}}}},
		{name: "cd in an if", command: "if false; then cd /tmp; fi; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "cd in a function", command: "f() { cd /tmp; }; f; git push", want: []push{{Dir: "", Sources: head}}},
		{name: "push in a function", command: "f() { git push; }", want: []push{{Dir: "", Sources: head}}},
		{name: "if without cd", command: "if make sdlc-gate; then git push; fi", want: []push{{Dir: "/repo", Sources: head}}},
		{name: "chdir, zsh's synonym for cd", command: "chdir /other && git push origin b", want: []push{{Dir: "/other", Sources: []string{"b"}}}},
		// A word that is no program on PATH may be a function or alias of the user's shell that cds.
		{name: "a function of the user's shell (zoxide's z)", command: "z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "oh-my-zsh's ... alias", command: "... && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a program on PATH runs as a child", command: "ls && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a program by its path", command: "./build.sh && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function the command defines", command: "f() { echo x; }; f && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a builtin that keeps the directory", command: "set -e && setopt pipefail && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "in a subshell, it cannot reach the push", command: "(z other) && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "in a function body", command: "f() { z other; }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "the last pipeline element runs in zsh's shell", command: "true | z other; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "the last pipeline element, then &&", command: "echo | z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "in a loop body", command: "for i in 1 2; do git push origin a; z other; done", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A function counts as defined only where its definition certainly runs in this shell.
		{name: "a function defined in a subshell", command: "(z() { :; }); z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in a branch", command: "if false; then z() { :; }; fi; z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in a chain", command: "false && z() { :; }; z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in eval", command: "eval 'z() { :; }'; z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in the background", command: "z() { :; } & z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in a pipeline", command: "z() { :; } | true; z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in a |& pipeline", command: "z() { :; } |& true; z other && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A function the command defines may cd wherever it is called, even after a cd that sets the
		// directory again, also through the functions it calls.
		{name: "a function that cds, called after a cd", command: "f() { cd /x; }; cd /repo && f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function that cds through a helper", command: "g() { cd /x; }; f() { g; }; cd /repo && f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a recursive function without a cd", command: "f() { f; }; f && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A "no" for a function found while a function that calls it was cut short (recursion) is not
		// kept: called on its own, it reaches that function's cd. The substitution's walker asks about
		// each function, f first, so g's verdict is first found inside f's.
		{name: "mutual recursion, the cd after the call", command: "f() { g; cd /x; }; g() { f; }; v=$(true); g && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "mutual recursion through a third function", command: "f() { g; cd /x; }; g() { h; }; h() { f; }; v=$(true); g && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "mutual recursion, the cd in the callee", command: "f() { g; g; }; g() { f; cd /x; }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A verdict kept for a function does not outlive a change to what the functions it calls run.
		{name: "a callee redefined after a call", command: "g() { :; }; f() { g; }; cd /repo && f; g() { cd /x; }; cd /repo && f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a callee autoloaded after a call", command: "f() { ls; }; cd /repo && f; autoload ls; cd /repo && f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function that cds, never called", command: "f() { cd /x; }; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a program's name, maybe redefined to cd", command: "if true; then ls() { cd /x; }; fi; cd /repo && ls && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// Called from a function's body, it is found by the walk of that body (unknownWord), not at the call.
		// A body's verdict holds until a word in it is defined: then it is a call.
		{name: "a word in a body defined as a function after a call", command: "g() { f; }; cd /repo && g; f() { :; }; cd /repo && g && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "functions calling each other at the end of a pipeline", command: "f() { true | g; }; g() { true | f; }; f; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a definition that may not run, its callee defined later to cd", command: "if true; then h() { g; }; fi; g() { cd /x; }; h; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A function that runs "$@" (or $1, eval, through noglob or builtin) runs its arguments as a
		// command in this shell: a cd among them moves it.
		{name: "a cd run by a function that runs \"$@\"", command: `r() { "$@"; }; r cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a pushd run by a function that runs \"$@\"", command: `r() { "$@"; }; r pushd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "an eval run by a function that runs \"$@\"", command: `r() { "$@"; }; r eval 'cd /other' && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "builtin cd run by a function that runs \"$@\"", command: `r() { "$@"; }; r builtin cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd run by a function that runs builtin \"$@\"", command: `r() { builtin "$@"; }; r cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd run by a function that runs $1 $2", command: `r() { $1 $2; }; r cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		// The same wherever the call is read for a cd: in a body, at a pipeline's end, run by a runner;
		// and from any tail of the arguments when the body shifts them or slices "${@:N}".
		{name: "a runner given a cd in a function's body", command: `r() { "$@"; }; g() { r cd /other; }; g && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner given a function that cds, in a body", command: `r() { "$@"; }; g() { cd /other; }; h() { r g; }; h && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner given a cd at a pipeline's end", command: `r() { "$@"; }; echo | r cd /other; git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner given a runner given a cd", command: `r() { "$@"; }; r r cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner that shifts", command: `run_logged() { local log=$1; shift; "$@" >"$log" 2>&1; }; run_logged /tmp/x.log cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner that shifts a word away", command: `r() { shift; "$@"; }; r true cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner that slices its arguments", command: `r() { "${@:2}"; }; r true cd /other && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a runner defined again without a shift", command: `r() { shift; "$@"; }; r() { "$@"; }; r git fetch origin && git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a runner that does not shift: later arguments are data", command: `retry() { for i in 1 2 3; do "$@" && return 0; sleep 5; done; return 1; }; retry git fetch origin && git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// `trap - SIG`, `trap SIG` and `trap N…` reset signals; they run nothing.
		{name: "a trap reset", command: "trap 'rm -f /tmp/x' EXIT; make test; trap - EXIT; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a trap reset by one operand", command: "trap INT; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a trap reset by signal numbers", command: "trap 2 15; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A coprocess runs in a subshell, from the directory of its statement.
		{name: "a coprocess", command: "coproc git push origin main", want: []push{{Dir: "/repo", Sources: []string{"main"}}}},
		{name: "a coprocess after a cd", command: "cd /other && coproc git push origin main", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "a named coprocess", command: "coproc P { git push origin main; }", want: []push{{Dir: "/repo", Sources: []string{"main"}}}},
		{name: "a coprocess's cd stays in it", command: "coproc { cd /x; }; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a program run by a function that runs \"$@\"", command: `r() { "$@"; }; r make test && git push origin a`, want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function run through noglob \"$@\"", command: `r() { noglob "$@"; }; d() { cd /other; }; r d && git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a shell script with a long option through arch", command: "arch -arm64 bash --rcfile /dev/null -c 'cd /other && git push origin a'", want: []push{{Dir: "/other", Sources: []string{"a"}}}},
		{name: "a function that cds, run by one that runs \"$@\"", command: `r() { "$@"; }; d() { cd /x; }; r d; git push origin a`, want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a program's name, maybe redefined to cd, called by a function", command: "if true; then ls() { cd /x; }; fi; g() { ls; }; cd /repo && g && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a program's name, maybe defined to cd, called by a function", command: "false && ls() { cd /x; }; g() { ls; }; g && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "zsh runs an anonymous function at once", command: "() {cd /x} && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "an anonymous function as a pipeline's last element", command: "true | () {cd /x}; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A definition chained with && or || is one element of the chain (mvdan.cc/sh reads the chain
		// as the body); the rest runs here.
		{name: "a cd chained after a definition", command: "f() { :; } && cd /x; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a cd chained with || after a definition", command: "f() { :; } || cd /x; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a push chained after a definition and a cd", command: "f() { :; } && cd /x && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a function defined in a chain may cd", command: "f() { cd /x; } && cd /repo && f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function defined in a pipeline then a chain", command: "f() { :; } | cat && cd /x; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// A function runs instead of the builtin or program of its name, but not after command, exec
		// or env, nor in a wrapper, which run programs.
		{name: "a function named echo that cds", command: "echo() { cd /x; }; echo hi; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function named true that cds", command: "true() { cd /x; }; true && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function named after a wrapper", command: "nohup() { cd /x; }; nohup true; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function named after a shell", command: "bash() { cd /x; }; bash -c true; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function named echo without a cd", command: "echo() { :; }; echo hi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "command skips the function", command: "echo() { cd /x; }; command echo hi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "env skips the function", command: "echo() { cd /x; }; env echo hi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a wrapper skips the function", command: "echo() { cd /x; }; nohup echo hi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function maybe defined, named echo", command: "false && echo() { cd /x; }; echo hi; git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function that cds, in a shell the command runs", command: "ls() { cd /x; }; bash -c 'ls && git push origin a'", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function without a cd, in a shell the command runs", command: "f() { :; }; bash -c 'cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "source of a here-string runs it here", command: "cd /other && source /dev/stdin <<< 'git push origin main'", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "source of /dev/fd/0 runs it here", command: "cd /other && source /dev/fd/0 <<< 'git push origin main'", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "source of a heredoc runs it here", command: "cd /other && . /dev/stdin <<'EOF'\ngit push origin main\nEOF", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "source of a heredoc with tabs runs it here", command: "cd /other && . /dev/stdin <<-'EOF'\n\tgit push origin main\n\tEOF", want: []push{{Dir: "/other", Sources: []string{"main"}}}},
		{name: "a trap calls a function without a cd", command: "cleanup() { rm -f x; }; trap cleanup EXIT; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function without a cd is no unknown word in a shell", command: "f() { :; }; bash -c 'f; git push origin a'", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function maybe defined, in a shell the command runs", command: "false && ls() { cd /x; }; bash -c 'ls && git push origin a'", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "env runs the program, not a function of its name", command: "bash() { :; }; env bash -c 'cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a wrapper runs the program, not a function of its name", command: "bash() { :; }; nohup bash -c 'cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a function named after a builtin the walker refuses", command: "enable() { :; }; enable -n cd; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a hook function without a cd", command: "chpwd() { echo moved; }; cd /x && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "set with options", command: "set -e -o pipefail; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "set -A on an ordinary array", command: "set -A arr x y; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "autoload of a completion function", command: "autoload -Uz compinit; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "zsh's anonymous function", command: "() echo hi; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "zsh's anonymous function with a cd", command: "() (cd /x); git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// Builtins that keep the directory, in a retry loop.
		{name: "a retry loop with break", command: "for i in 1 2 3; do git fetch origin && break; sleep 5; done && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a loop with continue", command: "for i in 1 2; do [ -f x ] || continue; echo $i; done && git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A chain that ends in || exit keeps every cd of its && prefix.
		{name: "two cds guarded by || exit", command: "cd /x && cd sub || exit 1; git push origin a", want: []push{{Dir: "/x/sub", Sources: []string{"a"}}}},
		// The exit fallback runs, from wherever the chain got to, before it exits.
		{name: "a push in an exit fallback", command: "cd /x || { git push origin main; exit 1; }", want: []push{{Dir: "", Sources: []string{"main"}}}},
		{name: "an exit fallback keeps the cd for what follows", command: "cd /x || { echo failed; exit 1; }; git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		// zsh code that runs in this shell.
		{name: "emulate -c, as eval", command: "emulate sh -c 'cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "emulate -c carries its cd", command: "emulate zsh -c 'cd /x' && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "repeat runs its command here", command: "repeat 1 cd /x && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a glob qualifier's code may cd", command: "echo *(e:'cd /x; true':) && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function whose glob qualifier may cd", command: "f() { echo *(e:'cd /x; true':); }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a +function glob qualifier may cd", command: "ls *(+myfn) && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// zsh globs, and runs a qualifier's code, wherever it expands words, before the command.
		{name: "a glob qualifier in an array assignment", command: "x=(*(e:'cd /x; true':)) && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a glob qualifier in a typeset array", command: "typeset -a y=(*(e:'cd /x; true':)) && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a glob qualifier in a for list", command: "for f in *(e:'cd /x; true':); do git push origin a; done", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a glob qualifier in a redirection", command: "echo hi > *(e:'cd /x; true':) && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a glob qualifier runs before its command", command: "git push origin a 2>*(e:'cd /x; true':)", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a glob qualifier in a background job", command: "x=(*(e:'cd /x; true':)) & git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "a function whose array assignment may cd", command: "f() { x=(*(e:'cd /x; true':)); }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		// zsh's functions -c and autoload define a function under a name the walker may read as a program.
		{name: "a function copied under a program's name", command: "f() { cd /x; }; functions -c f ls; ls && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "an autoloaded function with a program's name", command: "autoload ls; ls && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a function whose +function glob qualifier may cd", command: "f() { ls *(+myfn); }; f && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "zsh --emulate takes a value", command: "zsh --emulate sh -c 'cd /x && git push origin a'", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a script piped to zsh --emulate", command: "echo 'git push origin a' | zsh --emulate sh", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// git splits a refspec at its last colon.
		{name: "a commit named by a message", command: "git push origin ':/fix:refs/heads/x'", want: []push{{Dir: "/repo", Sources: []string{":/fix"}}}},
		{name: "a reflog date with colons", command: "git push origin 'main@{2026-10-01 10:00}:refs/heads/x'", want: []push{{Dir: "/repo", Sources: []string{"main@{2026-10-01 10:00}"}}}},
		// A path with `..` stays as written, for gatePush's stat and git to resolve as the kernel does:
		// git -C, cd -P and a shell made physical (set -P, zsh's chase_dots and chase_links) go to the
		// parent of a link's target, whatever the walker could tell of the shell's options.
		{name: "git -C with .. stays as written", command: "git -C link/.. push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "git -C .. after a cd", command: "cd link && git -C .. push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "cd -P with ..", command: "cd -P link/.. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "cd -L with .., the same", command: "cd -L link/.. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "cd -P without ..", command: "cd -P /x && git push origin a", want: []push{{Dir: "/x", Sources: []string{"a"}}}},
		{name: "a cd after one through ..", command: "cd -P link/.. && cd sub && git push origin a", want: []push{{Dir: "/repo/link/../sub", Sources: []string{"a"}}}},
		{name: "cd .. after cd -P", command: "cd -P link && cd .. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "cd ../x after cd -P", command: "cd -P link && cd ../x && git push origin a", want: []push{{Dir: "/repo/link/../x", Sources: []string{"a"}}}},
		{name: "set -P makes cd physical", command: "set -P && cd link/.. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "zsh's chase_links", command: "setopt chase_links && cd link && cd .. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		{name: "plain cd with ..", command: "cd link/.. && git push origin a", want: []push{{Dir: "/repo/link/..", Sources: []string{"a"}}}},
		// The shell expands ~ with a HOME the command sets, not the hook's.
		{name: "~ after HOME is set", command: "export HOME=/elsewhere && cd ~/r && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "git -C ~ after HOME is set", command: "HOME=/elsewhere; git -C ~/r push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "HOME reaches a shell the command runs", command: "HOME=/elsewhere && bash -c 'cd ~/r && git push origin a'", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "HOME reaches a piped script", command: "HOME=/elsewhere && echo 'cd ~/r && git push origin a' | bash", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "HOME reaches a heredoc script", command: "HOME=/elsewhere && bash <<'EOF'\ncd ~/r && git push origin a\nEOF", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "zsh's cdpath set in the command", command: "cdpath=(/tmp/other); cd sub && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "CDPATH set by a parameter expansion, before the cd", command: ": ${CDPATH:=/tmp/other}; cd sub && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "CDPATH set in a test, before the cd", command: "[[ ${CDPATH:=/tmp/other} ]] && cd sub && git push origin a", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "CDPATH set by a parameter expansion in a substitution", command: "x=$(: ${CDPATH:=/tmp/other}); cd sub && git push origin a", want: []push{{Dir: "/repo/sub", Sources: []string{"a"}}}},
		{name: "CDPATH reaches a shell the command runs", command: "CDPATH=/tmp/other; bash -c 'cd sub && git push origin a'", want: []push{{Dir: "", Sources: []string{"a"}}}},
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
		// find's terminator is a quoted or escaped `;` (a bare one ends the shell command) or `+`.
		{name: "find -exec a push", command: "find . -maxdepth 0 -exec git push origin a \\;", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -execdir runs from each file's directory", command: "find . -name x -execdir git push origin a ';'", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "find -ok a push", command: "yes | find . -maxdepth 0 -ok git push origin a \\;", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -ok stops at its terminator", command: "yes | find . -maxdepth 0 -ok git push origin a \\; -print", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		// A shell with a script among the arguments of a program the walker does not model, or of a
		// word that may be an alias or function of the shell's setup, which may cd first.
		{name: "a shell script through arch", command: "arch -arm64 sh -c 'cd /other && git push origin a'", want: []push{{Dir: "/other", Sources: []string{"a"}}}},
		{name: "a shell script with options through arch", command: "arch -arm64 bash -o pipefail -ec 'git -C /other push origin a'", want: []push{{Dir: "/other", Sources: []string{"a"}}}},
		{name: "a shell script through an unknown word", command: "nosuch-wrapper bash -lc 'git push origin a'", want: []push{{Dir: "", Sources: []string{"a"}}}},
		{name: "a shell's name as data", command: "arch -arm64 sh ./build.sh; git push origin a", want: []push{{Dir: "/repo", Sources: []string{"a"}}}},
		{name: "find -exec with +", command: "find . -maxdepth 0 -exec git push origin a {} +", want: []push{{Dir: "/repo", Sources: []string{"a", "{}"}}}},
		{name: "find -okdir runs from each file's directory", command: "find /tmp/other -maxdepth 0 -okdir git push origin a \\; <<< y", want: []push{{Dir: "", Sources: []string{"a"}}}},
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
		{name: "-s reads the script from standard input, the operands are its arguments", command: "bash -s x <<'EOF'\ncd /y && git push origin d\nEOF",
			want: []push{{Dir: "/y", Sources: []string{"d"}}}},
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
		runsStep   = "git runs it as a command"
		noVerify   = "--no-verify skips the pre-push hook"
		givenTo    = tokenNet // the net under the walker: a hook token in a command's expanded words
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
		{"command -V git push origin a", "command with options"},
		{"- git push origin a", "a git push run through -"}, // `-` is an alias in oh-my-zsh (cd -), not the precommand
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
		{"echo x | xargs git push origin main", "through xargs"},
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
		{"git send-pack origin HEAD:refs/heads/tmp", "git send-pack pushes without running git's pre-push hook"},
		{"git http-push origin HEAD", "git http-push pushes without running git's pre-push hook"},
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
		{"echo x | xargs -i git push {} origin main", "through xargs"},
		{"echo main | xargs -l git push origin", "through xargs"},
		{"echo x | xargs -e git push origin", "through xargs"},
		{"echo x | xargs --replace git push {} origin main", "through xargs"},
		{"echo push origin main | xargs git -C .", "through xargs"},
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
		{"echo push origin main | xargs -e git", "through xargs"},
		{"echo push origin main | xargs -l git", "through xargs"},
		{"echo push origin main | xargs -i git", "through xargs"},
		{"echo push origin main | xargs --replace git", "through xargs"},
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
		{`alias gp="$G push --no-verify"; eval 'gp origin a'`, "alias defined from a word the hook cannot read"},
		{"hash -p /tmp/x git; git push origin a", "hash"},
		// The red team's seventh round: forms kept refused, with their own reason.
		{`eval "$(ssh-agent -s)" >/dev/null && git push origin HEAD`, "an eval of a dynamic string earlier in the command"},
		{`env GIT_DIR="$X" git push origin a`, gitEnv},
		{"git-config core.hooksPath /dev/null", config},
		// Round 8: quoted or escaped declarations, =env, env options the walker does not model, sudo.
		{`export "GIT_CONFIG_COUNT=1" "GIT_CONFIG_KEY_0=core.hooksPath" "GIT_CONFIG_VALUE_0=/dev/null"; ./release.sh`, gitEnv},
		{`export 'GIT_DIR'=/x/.git; git push origin a`, gitEnv},
		{`declare -x "GIT_DIR=/x/.git"; git push origin a`, gitEnv},
		{`declare -n "r=GIT_DIR"; r=/x/.git; git push`, gitEnv},
		{`export "GIT_$X"; git push`, "may be a GIT_* variable"},
		{`\export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null; ./release.sh`, givenTo},
		{"builtin export GIT_DIR=/x/.git; git push origin a", givenTo},
		{"command export GIT_DIR=/x/.git; git push origin a", givenTo},
		{"=env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null bash -c 'git push origin main'", gitEnv},
		{"env -P /usr/bin:/bin GIT_CONFIG_COUNT=1 ./release.sh", givenTo},
		{"sudo GIT_CONFIG_COUNT=1 make release", givenTo},
		// A script piped to a shell with options, or from another producer; watch; runners' options.
		{"echo 'git push --no-verify origin main' | bash -e", noVerify},
		{"echo 'git push --no-verify origin main' | bash -s --", noVerify},
		{"printf 'git push origin main' | bash", "a script piped to a shell"},
		{"echo 'git push origin main' | tee /dev/null | bash", "a script piped to a shell"},
		{"watch 'git push --no-verify origin main'", noVerify},
		{"watch -d 'git push --no-verify origin main'", noVerify},
		{"git rebase -x'git push --no-verify origin main' HEAD~1", runsStep},
		{"git rebase --exe='git push origin main' HEAD~1", runsStep},
		{"git rebase -ix 'git push origin main' HEAD~1", runsStep},
		{"git difftool -y -x'git push origin a' HEAD~1", runsStep},
		{"git fetch --upload-pack='git push --no-verify origin HEAD:main; git-upload-pack' /tmp/any.git", runsStep},
		{"git clone -u 'git push origin main; git-upload-pack' /tmp/r.git d", runsStep},
		{"git ls-remote --upload-pack='git push origin main' /tmp/r.git", runsStep},
		{"git archive --remote=/tmp/r.git --exec='git push origin main' HEAD", runsStep},
		{"git grep -O'git push origin main' x", runsStep},
		{"git push --receive-pack='git -C /some/clone push origin main; git-receive-pack' /tmp/r.git main", runsStep},
		{`git fetch --upload-pack="$X" origin`, "that the hook cannot read"},
		{`git fetch --upload-pack "$X" origin`, "that the hook cannot read"},
		{"git push --exec='git push origin main' /tmp/r.git main", runsStep},
		{"git send-email --sendmail-cmd='git push origin main' x.patch", runsStep},
		{"git send-email --to-cmd 'git push origin main' x.patch", runsStep},
		{"git filter-branch --setup 'git push origin main' HEAD", runsStep},
		// The remote-tracking ref the gate measures a change from.
		{"git update-ref refs/remotes/origin/main HEAD", "can move origin/main"},
		{"git update-ref --stdin < refs.txt", "can move origin/main"},
		{"git symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/x", "can move origin/main"},
		{"git fetch . HEAD:refs/remotes/origin/main", "can move origin/main"},
		{"git fetch . HEAD:remotes/origin/main", "can move origin/main"},
		{"git pull origin main:remotes/origin/main", "can move origin/main"},
		{"git remote set-url origin /tmp/self", "can move origin/main"},
		{"git remote add up /tmp/self", "can move origin/main"},
		{"git remote rename origin old", "can move origin/main"},
		{"git remote remove origin", "can move origin/main"},
		// GIT_EDITOR and its kin are shell code git runs, and so are EDITOR, VISUAL and PAGER.
		{`GIT_EDITOR='sh -c "gi""t pu""sh origin main"' git commit --amend`, gitEnv},
		{`GIT_EDITOR='g\it pu\sh origin main; :' git commit --amend`, gitEnv},
		{"env GIT_PAGER='git push origin main' git log", gitEnv},
		{"EDITOR='git push origin main' git commit --amend", "shell code git runs"},
		{`VISUAL="$X" git commit`, "shell code git runs"},
		{"export PAGER='sh -c \"git push origin main\"'; git log", "shell code git runs"},
		// The net: shell code or a program that gets a hook token, which the walker cannot follow.
		{"emulate sh -c 'git push --no-verify origin main'", noVerify}, // emulate -c runs its code here, as eval
		{"echo *(e:'git push origin a':)", "a git push through a zsh glob qualifier that runs code"},
		{"git update-ref refs/Remotes/origin/main HEAD", "can move origin/main"},
		{"git fetch . HEAD:refs/Remotes/origin/main", "can move origin/main"},
		{"source /dev/stdin <<< 'git push --no-verify origin main'", "--no-verify skips the pre-push hook"},
		{`python3 -c "import subprocess; subprocess.run(['git','push','--no-verify'])"`, givenTo},
		{"go test ./x -run 'Blocked/--no-verify'", givenTo},
		{"docker run --rm -e GIT_PUSH_OPTION=1 alpine echo push", givenTo},
		{"$GP --no-verify origin main", givenTo},
		// A push in a command or process substitution, also through a shell fed a heredoc, a here-string
		// or a pipe there.
		{"x=$(bash <<'EOF'\ncd /tmp/clone && git push origin HEAD:main\nEOF\n)", "inside a command or process substitution"},
		{"x=$(bash <<< 'git push origin a')", "inside a command or process substitution"},
		{"x=$(echo 'git push origin a' | bash)", "inside a command or process substitution"},
		{"echo x > >(bash <<< 'git push origin a')", "inside a command or process substitution"},
		{"echo \"$(echo $(git push origin a))\"", "inside a command or process substitution"},
		// A refusal inside a command or process substitution refuses the whole command.
		{"x=$(git config include.path /x.cfg)", config},
		{`echo "$(git -c alias.ship=push ship)"; git status`, config},
		// Fail-closed refusals of a push the walker cannot follow, each on its own path.
		{`trap "git push origin $B" EXIT`, "a git push through trap cannot be checked"},
		{`timeout 60 "$X" sh -c 'git push origin a'`, "a git push through timeout cannot be checked"},
		{`timeout 60 "$GIT" push origin main`, "a git push through a command word that is not literal"},
		{`echo | xargs "$GIT" push origin main`, "a git push through a command word that is not literal"},
		{"kubectl exec pod -- git -c alias.ship=push ship origin a", config},
		{`watch -n 1 "git push origin $B"`, "a git push through watch cannot be checked"},
		{"eval 'print -l ${(f)x}; git push origin a'", "cannot parse the string eval runs"},
		{`echo a | xargs "$X" sh -c 'git push origin a'`, "a git push through xargs cannot be checked; "},
		{`bash -c -- "git push origin $B"`, "a git push through a shell script that is not literal"},
		{`bash -- "$S" git push origin a`, "a git push through a shell script that is not literal"},
		{`git submodule foreach "git push origin $B"`, "a git push through git submodule cannot be checked"},
		{"git --frobnicate x push origin a", "a git global option the hook does not model comes before push"},
		{"git config unset", "git configuration (edit)"},
		{`git config "$K" x`, "git config writes a key the hook cannot read"},
		// The fallback text check reads git's global options and their values, and git by its path.
		{`bash -c "git -C $D push origin main"`, "a git push through a shell argument that is not literal"},
		{`eval "git -C $D push origin main"`, "a git push through eval"},
		{"command -pv git -C /other push origin main", "a git push through command with options"},
		{"zsh -c 'git -C /other push origin main; print -l ${(f)x}'", "cannot parse the shell script"},
		{"printf 'git -C /other push origin main' | bash", "a git push through a script piped to a shell"},
		{"parallel ::: 'git -C /x push origin a'", "through parallel"},
		{`eval "git -c color.ui=never push origin $B"`, "a git push through eval"},
		{`eval "/usr/bin/git push origin $B"`, "a git push through eval"},
		{`eval "G=git; git --git-dir /x push origin $B"`, "a git push through eval"},
		{`eval "git-push origin $B"`, "a git push through eval"},
		// A short option's value attached and not literal.
		{`git rebase -x"$C" HEAD~1`, "that the hook cannot read"},
		{`git rebase -ix"$C" HEAD~1`, "that the hook cannot read"},
		{`git clone -u"$X" /tmp/r.git d`, "that the hook cannot read"},
		// Shell code git runs, from a value the hook does not read.
		{`: ${GIT_EDITOR:='git -C /other push origin main'}; git commit --amend`, gitEnv},
		{"for EDITOR in 'git -C /other push origin main'; do git commit --amend; done", "shell code git runs"},
		{"read -r EDITOR < f; git commit --amend", "shell code git runs"},
		{"printf -v VISUAL '%s' 'git push origin main'; git commit", "shell code git runs"},
		{"read -s PAGER; git log", "shell code git runs"},
		{"printf -vEDITOR '%s' 'sh ./x.sh'; git commit", "shell code git runs"},
		// A function or alias the command defines under a name the walker reads as the builtin.
		{"cd() { :; }; cd /repo && git push origin main", "defines a function named cd"},
		{"pushd() { :; }; pushd /repo && git push origin main", "defines a function named pushd"},
		{"exit() { :; }; cd /x || exit; git push origin main", "defines a function named exit"},
		{"alias cd=:; eval 'cd /x'", "defines an alias named cd"},
		{"functions[cd]=':'; cd /repo && git push origin a", "defines cd through zsh's functions"},
		{"typeset functions[cd]=:; cd /repo && git push origin a", "defines cd through zsh's functions"},
		{"n=cd; functions[$n]=:; cd /repo && git push origin a", "under a key the hook cannot read"},
		{`n=cd; alias "$n=:"; cd /repo && git push origin a`, "alias defined from a word the hook cannot read"},
		{"set -A functions cd :; cd /repo && git push origin a", "zsh's functions parameter set by a command the hook does not read"},
		{"set -o pipefail -A functions cd :; cd /repo && git push origin a", "zsh's functions parameter set by a command the hook does not read"},
		{`set -A "$n" cd :; cd /repo && git push origin a`, "set -A assigns an array the hook cannot name"},
		{"read -A functions; cd /repo && git push origin a", "zsh's functions parameter set by a command the hook does not read"},
		{"printf -v 'functions[cd]' :; cd /repo && git push origin a", "zsh's functions parameter set by a command the hook does not read"},
		{"chpwd_functions+=(f); cd /repo && git push origin a", "zsh's chpwd_functions parameter set from a value the hook cannot read"},
		{"chpwd_functions[1]=z; cd /repo && git push origin a", "defines code that runs git or changes the directory"},
		// What turns a builtin off or defines a function under another name, or a hook the shell runs.
		{"disable cd; cd /repo && git push origin a", "disable can change what a command"},
		{"enable -n cd; cd /repo && git push origin a", "enable can change what a command"},
		{"f() { :; }; functions -c f cd; cd /repo && git push origin a", "functions can change what a command"},
		{"autoload cd; cd /repo && git push origin a", "autoload can change what a command"},
		{`disable "$n"; cd /repo && git push origin a`, "disable can change what a command"},
		{"add-zsh-hook chpwd f; cd /repo && git push origin a", "add-zsh-hook can change what a command"},
		{"chpwd() { cd /x; }; cd /repo && git push origin a", "the function chpwd, which the shell runs on its own"},
		{"TRAPDEBUG() { cd /x; }; cd /repo && git push origin a", "the function TRAPDEBUG, which the shell runs on its own"},
		{"trap 'z other' DEBUG; git push origin a", "a trap action that may cd"},
		{"trap 'cd /x' DEBUG; git push origin a", "a trap action that may cd"},
		{"trap 'cd /x' DEBUG; cd /repo && git push origin a", "a trap action that may cd"},
		// A function the command defines runs with a push in its arguments.
		{`f() { "$@"; }; f git push origin a`, "a git push run through f"},
		{`bash() { cd /other; command bash "$@"; }; bash -c 'git push origin a'`, "a git push run through bash"},
		{`f() { "$@"; }; f git $'push' origin a`, "a git push run through f"},
		{`f() { "$@"; }; f git pu\sh origin a`, "a git push run through f"},
		// A glob qualifier's code with a push, wherever zsh globs.
		{"x=(*(e:'git push origin a':))", "a git push through a zsh glob qualifier"},
		{"for f in *(e:'git push origin a':); do :; done", "a git push through a zsh glob qualifier"},
		{"local y=(*(e:'git push origin a':))", "a git push through a zsh glob qualifier"},
		{"x=(*(e:'git push origin a':)) &", "a git push through a zsh glob qualifier"},
		// A trap action the hook cannot read may cd before any later command.
		{`trap "$a" DEBUG; git push origin a`, "a trap action the hook cannot read"},
		// An exit fallback's commands are walked like any others.
		{"cd /x || { git update-ref refs/remotes/origin/main HEAD; exit 1; }", "can move origin/main"},
		{"cd /x || exit $(git push origin main)", "a git push inside a command or process substitution"},
		// git's remote helpers run send-pack themselves, without the pre-push hook.
		{"git remote-codecommit origin codecommit://x", "pushes without running git's pre-push hook"},
		{"git -C . remote-ext origin 'ext::sh -c x'", "pushes without running git's pre-push hook"},
		// printf and zsh's echo turn `\n` into a newline: a push on a later line of a piped script.
		{`cd /other && printf 'true\ngit push origin main\n' | sh`, "a git push through a script piped to a shell"},
		{`cd /other && echo -e 'true\ngit push origin main' | sh`, "a git push through a script piped to a shell"},
		{`echo 'true\ncd /other\ngit push origin main' | sh`, "a git push through a script piped to a shell"},
		{`printf 'true\012git push origin main' | sh`, "a git push through a script piped to a shell"},
		{`printf 'true\x0agit push origin main' | sh`, "a git push through a script piped to a shell"},
		{`printf 'true\u000agit push origin main' | sh`, "a git push through a script piped to a shell"},
		{`printf 'true\U0000000agit push origin main' | sh`, "a git push through a script piped to a shell"},
		// What source reads from a word the hook cannot read.
		{`cd /other && . <(echo 'git push origin main')`, "a git push through source"},
		// A ref or refspec read from standard input, in any abbreviation git accepts, can move origin/main.
		{`git fetch -q --stdin . <<<'change:refs/remotes/origin/main'`, "can move origin/main"},
		{`echo 'refs/heads/change:refs/remotes/origin/main' | git fetch --stdin .`, "can move origin/main"},
		{`git update-ref --stdi <<<'update refs/remotes/origin/main HEAD'`, "can move origin/main"},
		{`git update-ref --std <<<'update refs/remotes/origin/main HEAD'`, "can move origin/main"},
		{`git update-ref --st <<<'update refs/remotes/origin/main HEAD'`, "can move origin/main"},
		{`git update-ref --s <<<'update refs/remotes/origin/main HEAD'`, "can move origin/main"},
		{`git fetch --st . <<<'x:refs/remotes/origin/main'`, "can move origin/main"},
		// One-letter abbreviations of a long option git accepts.
		{"git ls-remote --u='git push origin main' .", runsStep},
		{"git clone --u='git push origin main' /tmp/r.git d", runsStep},
		{"git archive --e='git push origin main' HEAD", runsStep},
		{"mapfile -t PAGER < notes; git log", "shell code git runs"},
		// Configuration under diff. or merge. with a subsection, and what rewrites refs or objects.
		{"git -c diff.x.binary=true diff", config},
		{"git -c merge.x.name=y merge z", config},
		{"git replace HEAD~1 HEAD", "can move origin/main"},
		{"git fast-import < stream", "can move origin/main"},
		// A shell the command runs inherits what a sourced file or a dynamic eval set before it.
		{"source ./x.sh; cd /repo && bash -c 'git push origin a'", "a file sourced earlier"},
		{`eval "$E"; cd /repo && bash -c 'git push origin a'`, "an eval of a dynamic string earlier"},
		// Tokens bash spells out of escapes, which only the expanded words show.
		{`git push $'\x2d-no-verify' origin main`, noVerify},
		{`source /dev/stdin <<< $'git push \x2d-no-verify origin main'`, "--no-verify skips the pre-push hook"},
		{`export $'\x47IT_DIR=/x/.git'; git push origin a`, gitEnv},
		{`git commit -m $'\x47IT_DIR'`, givenTo},
		{`echo $'core.hooks\x50ath'`, givenTo},
		// Without a subcommand git reads no option after the first positional: these set the key.
		{"git config alias.x '!sh' --get", config},
		{"git config alias.x '!sh' -l", config},
		{"git config --file .git/config alias.x '!sh' --name-only", config},
		// git handed to a function the command defines, or to an unknown word, is refused as git is.
		{`f() { "$@"; }; f git config alias.x '!sh'`, config},
		{`f() { "$@"; }; f git -C . -c alias.x='!sh' x`, config},
		{`f() { "$@"; }; f git update-ref refs/remotes/origin/main HEAD`, "can move origin/main"},
		{`f() { "$@"; }; f git send-pack origin HEAD:main`, "pushes without running git's pre-push hook"},
		{"nosuch-runner git config alias.x '!sh'", config},
		{"g() { :; }; chpwd() { g; }; g() { cd /x; }; git push origin a", "the function chpwd, which the shell runs on its own and which may cd"},
		// git config's section commands write every key under them (core.hooksPath under core).
		{"git config --remove-section core && git push origin main", config},
		{"git config --remove-section core", config},
		{"git config remove-section core", config},
		{"git config rename-section color core", config},
		// An export of a quoted or escaped name with no value marks a variable of the shell for export.
		{`export $'\x47IT_DIR'; git push origin a`, gitEnv},
		{`export "GIT_""DIR"; git push origin a`, gitEnv},
		// zsh's functions -c copies code over a name a trap runs: the trap is read again.
		{"ls() { :; }; trap ls DEBUG; e() { cd /other; }; functions -c e ls; git push origin a", "a trap action that may cd"},
		// A trap action runs the functions as they are defined when it runs.
		{"d() { true; }; trap d EXIT; d() { cd /other; }; git push origin a", "a trap action that may cd"},
		// bash 5.3's ${ …; } runs in this shell.
		{": ${ cd /other; } && git push origin a", "run their commands in this shell"},
		// A shell among an unmodeled program's arguments, with options the walker does not read.
		{"arch -arm64 bash --frob x -c 'git push origin a'", "a git push through bash"},
		{"find . -maxdepth 0 -okdir git config alias.x '!sh' ';' <<< y", config},
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
		"awk '/git push/ {print NR}' docs/guides/claude-code-sdlc.md",
		"perl -0pi -e 's/git push origin main/git push -u origin HEAD/g' docs/guides/claude-code-sdlc.md",
		"perl -ne 'print if /pushurl/' .git/config",
		"python3 -c \"import yaml; print(yaml.safe_load(open('.github/workflows/ci.yml')))\"",
		// The default case: git and push as data, not a command and its subcommand.
		"grep -n -w -e git -e push docs/guides/claude-code-sdlc.md",
		"kubectl -n patrol-cd exec deploy/patrol-cd-repo-server -- git -C /tmp/_patrol-repo log --oneline --grep push",
		"env -u GOFLAGS go test ./hack/sdlc/pushcheck/...",
		"git status --short && env -u GOFLAGS go test ./hack/sdlc/pushcheck/...",
		// The GIT_* variables on the safe list, set or printed.
		"GIT_PAGER= git log -3",
		"GIT_PAGER=cat git log -3",
		"printf '%s\\n' \"$GIT_AUTHOR_NAME\" \"${GIT_TERMINAL_PROMPT-}\"",
		// git config reads, and git -c with a safe key, including a dynamic value.
		"for k in user.name user.email; do echo \"$k=$(git config \"$k\")\"; done",
		"git config remote.origin.url",
		"git -c safe.directory=\"$PWD\" status",
		"git -c user.email=\"$EMAIL\" commit -q -m 'chore: x'",
		"git --git-dir=\"$tmp/remote.git\" log --oneline -3",
		"git -c color.ui=never -c user.name=x log --oneline",
		"git -c core.quotePath=false diff --name-only",
		// Other ordinary commands with the word push in them.
		`grep -rn "git push" docs`,
		`gh pr create --title "x" --body "run git push? no"`,
		"GIT_EDITOR=true git rebase --continue",
		"git stash push -m wip",
		"git log --grep push",
		"git switch -c fix/new-branch",
		"cd ui && pnpm typecheck && cd .. && git status",
		"cd /Users/x/patrol-cd || exit 1; git status",
		"git config user.email \"$EMAIL\"",
		"git config --local user.name \"$(id -un)\"",
		// The fifth review's false refusals, each fixed: GIT_ is case-sensitive in the text net, the
		// fallback net reads one command's words (not its siblings'), PATROL_SDLC_* is an ordinary
		// variable, and an exec runner without -- is refused only on a plain git push.
		"env -C /tmp cat testdata/git_push.json",
		"env -C ui pnpm test; grep -rn \"git push\" docs",
		"PATROL_SDLC_BASE=HEAD~1 make sdlc-check",
		"docker run --rm alpine echo push",
		"alias ll='ls -la'",
		"trap 'rm -rf \"$tmp\"' EXIT; go test ./hack/sdlc/...",
		"hash -r",
		// An empty heredoc (mvdan.cc/sh leaves its body nil).
		"cat <<EOF > notes.txt\nEOF",
		"cat <<'EOF' | bash\nEOF",
		"bash <<EOF\nEOF",
		`export "GOFLAGS=-count=1" "GIT_PAGER=cat" && go test ./...`,
		"git fetch origin",
		"git clone --depth 1 https://github.com/x/y d",
		"git grep -O main",
		"git ls-remote origin",
		"git rebase -i --autosquash origin/main",
		"GIT_EDITOR=true git commit --amend --no-edit",
		"sudo -u ci make test",
		"echo 'ls' | bash -e",
		"watch -n 5 git status",
		`GIT_SEQUENCE_EDITOR="sed -i '' '2,\$s/^pick/squash/'" git rebase -i HEAD~3`,
		"test/e2e/fixtures/git-server/rootfs/usr/local/bin/git-seed",
		"chdir ui && pnpm test",
		"EDITOR=vim VISUAL='code --wait' PAGER=less git log -1",
		"git remote -v && git remote show origin",
		"git fetch origin main",
		"git fetch origin main:refs/heads/main-copy",
		"git update-ref refs/heads/scratch HEAD",
		"git symbolic-ref --short HEAD",
		"git --help push",
		"git -C /x --version",
		"export EDITOR; git commit",
		": ${PAGER:=less}; git log -1",
		"VISUAL= git commit",
		`cd "$X"; alias x=:`, // a builtin is no AUTO_CD, even where the directory is unknown
		// git config reads, an option before the key or with a subcommand.
		"git config --get-regexp '^alias\\.'",
		"git config --get-urlmatch http.proxy https://github.com",
		"git config get --all user.name",
		"git config list --show-origin",
		// Options after a subcommand are read as options: these unset and set a safe key.
		"git config unset --all user.name",
		"git config set --comment x user.name y",
		// git handed to a function: a safe git command, and git as data.
		`f() { "$@"; }; f git config user.name x`,
		`f() { "$@"; }; f git status`,
		`f() { "$@"; }; f grep -n -e git -e update-ref refs/remotes/origin/main docs`,
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, hookToken(command), "the text net (main.run) must let it through")
			got, err := findPushes(command, "/repo")
			require.NoError(t, err)
			assert.Empty(t, got.Blocked, "pushes: %+v", got.Pushes)
			assert.Empty(t, got.Pushes)
			for _, u := range got.Unknown { // main.run lets an unknown word run only in the clone
				assert.True(t, u.Dir == "/repo" || strings.HasPrefix(u.Dir, "/repo/"), "%+v", u)
			}
		})
	}
}

// Functions that call each other many times are followed once each: a chain of functions each calling
// the next twice (and itself) would otherwise take time exponential in its length, past the hook's
// timeout, which lets the command run.
func TestFindPushesDeepCalls(t *testing.T) {
	t.Parallel()
	// Each calls the next twice and itself; then each calls the first (a call back to a function
	// still being looked at) and the next twice. No cd anywhere, so every call is followed.
	var self, back strings.Builder
	back.WriteString("f1() { f2; f2; }; ")
	for i := 1; i < 40; i++ {
		fmt.Fprintf(&self, "f%d() { f%d; f%d; f%d; }; ", i, i+1, i+1, i)
		if i > 1 {
			fmt.Fprintf(&back, "f%d() { f1; f%d; f%d; }; ", i, i+1, i+1)
		}
	}
	self.WriteString("f40() { :; }; f1 && git push origin a; v=$(f1)")
	back.WriteString("f40() { f1; }; f1 && git push origin a; v=$(f1)")
	// More trap actions than the walker reads again after each definition.
	var traps strings.Builder
	for i := 0; i <= maxTraps; i++ {
		fmt.Fprintf(&traps, "trap 'echo %d' USR1; ", i)
	}
	a, err := findPushes(traps.String()+"true", "/repo")
	require.NoError(t, err)
	assert.Contains(t, a.Blocked, "more than 64 trap actions")
	// A long chain of definitions, each calling the one before: each body holds the code the bodies
	// it calls could not read (here the possible aliases), so past maxRuns places it is refused, in
	// time well short of the hook's timeout.
	var chain strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&chain, "f%d() { f%d; echo; }; ", i, i-1)
	}
	chain.WriteString("true")
	start := time.Now()
	a, err = findPushes(chain.String(), "/repo")
	require.NoError(t, err)
	assert.Contains(t, a.Blocked, "in more than 256 places")
	assert.Less(t, time.Since(start), 60*time.Second)
	for _, command := range []string{self.String(), back.String()} {
		done := make(chan analysis, 1)
		go func() {
			a, err := findPushes(command, "/repo")
			assert.NoError(t, err)
			done <- a
		}()
		select {
		case a := <-done:
			assert.Equal(t, []push{{Dir: "/repo", Sources: []string{"a"}}}, a.Pushes)
		case <-time.After(30 * time.Second):
			t.Fatalf("the walk of 40 functions calling each other did not finish in 30 s: %.80s…", command)
		}
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
		"yes | find . -maxdepth 0 -ok git ship origin main \\;",
		// A function, or an alias or function of the shell's setup, may hand its arguments to git.
		`f() { "$@"; }; f git ship origin main`,
		`retry() { for i in 1 2 3; do "$@" && return 0; sleep 5; done; return 1; }; retry git ship origin main`,
		`f() { "$@"; }; f git -C . ship origin main`,
		"nosuch-runner git ship origin main",
		"cd /x || { git ship origin main; exit 1; }",
	} {
		got, err := findPushes(command, "/repo")
		require.NoError(t, err)
		assert.Contains(t, got.Calls, "ship", command)
	}
	// A program on PATH that is not a runner the walker models takes git as data (a package to install).
	got, err = findPushes("ls git ship", "/repo")
	require.NoError(t, err)
	assert.Empty(t, got.Calls)
}

// A command word the walker does not know (an alias or function of the shell's setup, or a program
// not on PATH) may push, so main.go learns where it runs; one run as a program (after command, env or
// a wrapper) is no alias or function.
func TestFindPushesUnknownWords(t *testing.T) {
	t.Parallel()
	stdinScript := func(sh string) string {
		return "`" + sh + "` reads a script from standard input that the hook did not read"
	}
	inputScript := func(sh string) string {
		return "`" + sh + "` run by xargs or parallel takes its script from their input, which the hook did not read"
	}
	sourced := "`source` of standard input or of a word the hook cannot read runs a script it did not read"
	staleRun := func(fn string) string {
		return "`" + fn + "` runs a function defined, or defined again, after it (bash and zsh look a function up when it is called), code the hook did not read"
	}
	for _, tt := range []struct {
		command string
		want    []unknownRun
	}{
		{"nosuch-gp origin main", []unknownRun{{Dir: "/repo", Word: "nosuch-gp"}}},
		{"cd /other && nosuch-gp origin main", []unknownRun{{Dir: "/other", Word: "nosuch-gp"}}},
		{"nosuch-z x && nosuch-gp", []unknownRun{{Dir: "/repo", Word: "nosuch-z"}, {Dir: "", Word: "nosuch-gp"}}},
		{"cd /other && cd /repo && - && true", []unknownRun{{Dir: "/repo", Word: "-"}}},
		{"x=$(nosuch-gp)", []unknownRun{{Dir: "/repo", Word: "nosuch-gp"}}},
		{"cd /other && v=$(nosuch-gp)", []unknownRun{{Dir: "/other", Word: "nosuch-gp"}}}, // once, where it runs
		{"cd /x || { nosuch-gp; exit 1; }", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},   // an exit fallback
		{"f() { cd sub && ls; }; v=$(f)", nil},                                            // a function this shell defined, called in a substitution
		// CDPATH or HOME set by a call may take the body's cd elsewhere; a body that does not cd stays.
		{"f() { cd sub && nosuch-gp; }; CDPATH=/tmp/other; f", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"f() { cd sub && nosuch-gp; }; : ${CDPATH:=/tmp/other}; f", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"f() { cd ~/x && nosuch-gp; }; HOME=/tmp/h; f", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"f() { nosuch-gp; }; CDPATH=/tmp/other; f", []unknownRun{{Dir: "/repo", Word: "nosuch-gp"}}},
		// AUTO_CD in a body is relative to the call.
		{"f() { ../x && nosuch-gp; }; f", []unknownRun{{Dir: "/repo", Word: "../x"}, {Dir: "/repo/../x", Word: "nosuch-gp"}}},
		// A function called from several places records each place once.
		{"f() { nosuch-gp; }; cd /x && f; cd /x && f", []unknownRun{{Dir: "/x", Word: "nosuch-gp"}}},
		// After a call that may cd, the next call runs from an unknown directory; nesting adds no more.
		{"f() { nosuch-gp; }; g() { f; f; }; h() { g; g; }; h", []unknownRun{{Dir: "/repo", Word: "nosuch-gp"}, {Dir: "", Word: "nosuch-gp"}}},
		{"f() { cd sub && nosuch-gp; }; f", []unknownRun{{Dir: "/repo/sub", Word: "nosuch-gp"}}},
		{"f() { cd /x && nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "/x", Word: "nosuch-gp"}}},
		// A function runs the functions it calls as they are defined when it is called (bash and zsh look
		// a function up then): one that runs a function defined, or defined again, after it runs code the
		// walker did not read with its body, from the call or, if it may cd now, from anywhere.
		{"g() { true; }; f() { g; }; g() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}},
		{"g() { true; }; f() { g; }; cd /other && f; g() { nosuch-gp; }; f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}},
		{"f() { ls; }; ls() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}},
		{"g() { :; }; f() { g; nosuch-gp; }; g() { cd /x; }; f", []unknownRun{{Dir: "/repo", Word: "nosuch-gp"}, {Dir: "", Word: "f", What: staleRun("f")}}},
		{"f() { g; }; g() { :; }; cd /other && f", []unknownRun{{Dir: "/other", Word: "g"}, {Dir: "/other", Word: "f", What: staleRun("f")}}},
		{"g() { nosuch-gp; }; f() { g; }; cd /other && f", []unknownRun{{Dir: "/other", Word: "nosuch-gp"}}}, // as defined: no change
		{"g() { :; }; f() { noglob g; }; g() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}},
		{"g() { :; }; f() { eval g; }; g() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}},
		{"g() { :; }; chpwd() { g; }; g() { nosuch-gp; }", []unknownRun{{Dir: "", Word: "chpwd", What: staleRun("chpwd")}}},
		// A trap action, or a function the shell calls on its own, that may cd moves the directory
		// before or after any command (DEBUG, ERR): no later directory is known, even after a cd.
		{"trap 'cd /other' DEBUG; nosuch-gp origin HEAD", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"trap 'cd /other' ERR; false; nosuch-gp", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"TRAPDEBUG() { cd /other; }; nosuch-gp", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"trap 'cd /other' DEBUG; cd /repo && nosuch-gp", []unknownRun{{Dir: "", Word: "nosuch-gp"}}},
		{"d() { true; }; trap d EXIT; d() { cd /other && nosuch-gp; }", []unknownRun{{Dir: "", Word: "trap", What: "a trap action that runs a function defined, or defined again, after the trap was set (it looks the function up when it runs), code the hook did not read"}}},
		{`r() { nocorrect "$@"; }; d() { cd /other && nosuch-gp; }; r d`, []unknownRun{{Dir: "/repo", What: "a command named by a word the hook cannot read"}, {Dir: "/repo", Word: "r", What: staleRun("r")}, {Dir: "/other", Word: "nosuch-gp"}}},
		{`r() { repeat 1 "$@"; }; d() { cd /other && nosuch-gp; }; r d`, []unknownRun{{Dir: "/repo", What: "a command named by a word the hook cannot read"}, {Dir: "", Word: "r", What: staleRun("r")}, {Dir: "/other", Word: "nosuch-gp"}}},
		{"h() { :; }; g() { h; }; f() { g; }; h() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "", Word: "f", What: staleRun("f")}}}, // through g
		{"g() { :; }; f() { v=$(g); }; g() { nosuch-gp; }; cd /other && f", []unknownRun{{Dir: "/other", Word: "f", What: staleRun("f")}}},
		{"f() { g; }; f() { :; }; g() { nosuch-gp; }; cd /other && f", nil}, // the second definition replaces the first
		{`r() { eval "$@"; }; d() { cd /other && nosuch-gp; }; r d`, []unknownRun{{Dir: "/repo", Word: "eval", What: "`eval` of a string the hook cannot read runs code it did not read"}, {Dir: "", Word: "r", What: staleRun("r")}, {Dir: "/other", Word: "nosuch-gp"}}},
		// A function given to one that runs "$@", or to an unknown word, runs there.
		{`r() { "$@"; }; d() { nosuch-gp; }; cd /other && r d`, []unknownRun{{Dir: "/other", What: "a command named by a word the hook cannot read"}, {Dir: "/other", Word: "r", What: staleRun("r")}, {Dir: "/other", Word: "nosuch-gp"}}},
		{"d() { cd /x && nosuch-gp; }; nosuch-runner d", []unknownRun{{Dir: "/repo", Word: "nosuch-runner"}, {Dir: "/x", Word: "nosuch-gp"}}},
		{"d() { nosuch-gp; }; nosuch-runner d", []unknownRun{{Dir: "/repo", Word: "nosuch-runner"}, {Dir: "", Word: "nosuch-gp"}}},
		{`r() { echo "$@"; }; d() { nosuch-gp; }; r d`, nil}, // a function that runs no code given at run time
		{"f() { nosuch-gp; }; cd /other && f && cd /y && f", []unknownRun{{Dir: "/other", Word: "nosuch-gp"}, {Dir: "/y", Word: "nosuch-gp"}}},
		{"cd /other && bash -c 'nosuch-gp origin main'", []unknownRun{{Dir: "/other", Word: "nosuch-gp"}}},
		{"command nosuch-gp; env nosuch-gp; nohup nosuch-gp", nil},
		{"f() { :; }; f; ls; true", nil},
		// A shell, source or eval whose script the walker did not read.
		{"cd /other && echo x | env sh", []unknownRun{{Dir: "/other", Word: "sh", What: stdinScript("sh")}}},
		{"(cd /other && bash) <<'EOF'\ngit push origin main\nEOF", []unknownRun{{Dir: "/other", Word: "bash", What: stdinScript("bash")}}},
		{"cd /other && sh < <(echo x)", []unknownRun{{Dir: "/other", Word: "sh", What: stdinScript("sh")}}},
		{"bash -s <<'EOF'\ntrue\nEOF", nil}, // read
		{"echo x | xargs -0 sh -c", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{"echo x | xargs -I{} sh -c {}", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{"echo x | xargs -i sh -c 'echo {}'", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{"echo x | xargs --replace sh -c 'echo {}'", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{"echo x | xargs -J % sh -c 'echo %'", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{`echo x | xargs sh -c 'echo "$0"'`, nil}, // the input is the script's arguments
		{"echo x | xargs sh ./run.sh", nil},       // and here the script file's
		{"echo x | xargs -0 sh", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}, {Dir: "/repo", Word: "sh", What: stdinScript("sh")}}},
		{"echo x | parallel", []unknownRun{{Dir: "/repo", Word: "parallel", What: "`parallel` with no command runs each line of its input, which the hook did not read, as a command"}}},
		{"echo x | parallel sh -c {}", []unknownRun{{Dir: "/repo", Word: "sh", What: inputScript("sh")}}},
		{"echo x | parallel bash -c 'echo {}'", []unknownRun{{Dir: "/repo", Word: "bash", What: inputScript("bash")}}},
		{"cd /other && echo x | source /dev/fd/0", []unknownRun{{Dir: "/other", Word: "source", What: sourced}}},
		{"cd /other && source /dev/stdin", []unknownRun{{Dir: "/other", Word: "source", What: sourced}}},
		{`. "$f"`, []unknownRun{{Dir: "/repo", Word: "source", What: sourced}}},
		{"source /dev/stdin <<<'true'", nil}, // read
		{`eval "$x"`, []unknownRun{{Dir: "/repo", Word: "eval", What: "`eval` of a string the hook cannot read runs code it did not read"}}},
		{`cd /other && "$X" a`, []unknownRun{{Dir: "/other", Word: "", What: "a command named by a word the hook cannot read"}}},
	} {
		got, err := findPushes(tt.command, "/repo")
		require.NoError(t, err)
		// Without the record of a possible alias at each call of a function (TestFindPushesFunctionAliases).
		var runs []unknownRun
		for _, u := range got.Unknown {
			if u.What != aliasWhat(u.Word) {
				runs = append(runs, u)
			}
		}
		assert.Equal(t, tt.want, runs, tt.command)
	}
}

// An alias of the shell's setup runs instead of a function the command defines of its name (zsh
// expands aliases before it looks functions up), so a call of one that is no program on PATH or
// builtin is a word the walker does not know too, where it runs: at the top or in a body.
func TestFindPushesFunctionAliases(t *testing.T) {
	t.Parallel()
	alias := func(dir, fn string) unknownRun { return unknownRun{Dir: dir, Word: fn, What: aliasWhat(fn)} }
	for _, tt := range []struct {
		command string
		want    []unknownRun
	}{
		{"function gp { kubectl get pods; }; cd /other && gp", []unknownRun{alias("/other", "gp")}},
		{"function gp { :; }; f() { gp; }; cd /other && f", []unknownRun{alias("/other", "gp"), alias("/other", "f")}},
		{"f() { cd /x && gp() { :; }; }; true", nil},               // defined, never called
		{"ls() { :; }; cd /other && ls; echo() { :; }; echo", nil}, // a program on PATH, a builtin: their aliases are the guide's Limits
		{"uname() { :; }; cd /other && uname", nil},                // a program on PATH the walker does not model
		{"shopt() { :; }; cd /other && shopt", nil},                // a builtin no program on PATH shares
	} {
		got, err := findPushes(tt.command, "/repo")
		require.NoError(t, err)
		assert.Equal(t, tt.want, got.Unknown, tt.command)
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

// The verdict on a function is kept per directory: a word in its body that names no program on PATH
// but a directory where it is called is a cd there (AUTO_CD), and not elsewhere. With PATH empty, grep
// is such a word.
func TestFindPushesFunctionVerdictPerDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "grep"), 0o755))
	t.Setenv("PATH", t.TempDir())
	got, err := findPushes("f() { grep; }; cd /nonexistent && f; cd "+root+" && f && git push origin a", root)
	require.NoError(t, err)
	assert.Equal(t, []push{{Dir: "", Sources: []string{"a"}}}, got.Pushes)
}

// zsh's AUTO_CD (on in the Bash tool's zsh) runs a lone directory name as a cd.
func TestFindPushesAutoCD(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "ls"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "dirs"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "eval"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "stdstub"), 0o755))
	for _, tt := range []struct{ command, dir string }{
		{"eval && git push origin a", root},                              // a builtin the walker handles by name
		{"stdstub && git push origin a", filepath.Join(root, "stdstub")}, // a modeled program not installed
		{"ls && git push origin a", root},                                // a command on PATH runs, even with a directory of its name
		{"dirs && git push origin a", root},                              // so does a builtin that is no program on PATH
		{"sub() { :; }; sub && git push origin a", root},                 // and a function the command defines
		{"sub() { :; }; true | sub; git push origin a", root},            // also where only changesDir looks
		{"true | ./sub; git push origin a", ""},                          // a path to a directory is AUTO_CD there too
		{"sub && git push origin a", filepath.Join(root, "sub")},
		{"./sub && git push origin a", filepath.Join(root, "sub")},
		{"sub; git push origin a", ""},             // the cd may fail
		{"sub || true; git push origin a", ""},     // a chain with an AUTO_CD counts as a cd
		{"nosuchdir && git push origin a", ""},     // no such directory, nor program: a function may cd
		{"sub x && git push origin a", ""},         // with an argument it is a command, maybe a function
		{"(sub && true); git push origin a", root}, // in a subshell
		{"if true; then sub; fi; git push origin a", ""},
		{"f() { sub; }; f && git push origin a", ""}, // a function that runs AUTO_CD
	} {
		got, err := findPushes(tt.command, root)
		require.NoError(t, err)
		assert.Empty(t, got.Blocked, tt.command)
		assert.Equal(t, []push{{Dir: tt.dir, Sources: []string{"a"}}}, got.Pushes, tt.command)
	}
}

// A shell reads its script from standard input with -s, or with no -c and no script operand.
func TestReadsStdin(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"-e"}, true},
		{[]string{"-o", "pipefail"}, true},
		{[]string{"-eo", "pipefail", "-x"}, true},
		{[]string{"--norc"}, true},
		{[]string{"-s", "--"}, true},
		{[]string{"-s", "arg"}, true},
		{[]string{"-"}, true},
		{[]string{"/dev/stdin"}, true},
		{[]string{"--", "/dev/fd/0"}, true},
		{[]string{"-c", "ls"}, false},
		{[]string{"-ec", "ls"}, false},
		{[]string{"script.sh"}, false},
		{[]string{"--", "script.sh"}, false},
		{[]string{"-e", "script.sh"}, false},
	} {
		lit := make([]bool, len(tt.args))
		for i := range lit {
			lit[i] = true
		}
		assert.Equal(t, tt.want, readsStdin(tt.args, lit), "%q", tt.args)
	}
	assert.False(t, readsStdin([]string{"x"}, []bool{false}), "an argument that is not literal")
}

// A cd with `..` through a symbolic link: bash's logical cd and the kernel's physical one reach
// different directories, so the push's directory is unknown; without a link they agree. git -C is
// physical: its path stays as written, for gatePush's git to resolve.
func TestFindPushesThroughLinks(t *testing.T) {
	t.Parallel()
	work, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root, other := filepath.Join(work, "root"), filepath.Join(work, "other")
	for _, d := range []string{filepath.Join(root, "sub"), filepath.Join(other, "sub")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.Symlink(filepath.Join(other, "sub"), filepath.Join(root, "out")))  // this clone into another
	require.NoError(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(other, "back"))) // another into this clone
	for _, tt := range []struct{ command, dir string }{
		{"cd out && cd .. && git push origin a", ""},
		{"cd out/.. && git push origin a", ""},
		{"cd " + other + "/back && cd .. && git push origin a", ""},
		{"cd sub && cd .. && git push origin a", root},
		{"cd sub/.. && git push origin a", root},
		{"git -C out/.. push origin a", root + "/out/.."},
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
	// Nor may the git the code under test runs read the developer's global or system configuration (a
	// global hooks directory, say); the tests are parallel, so not t.Setenv.
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Nor what check.sh (through lib.sh) exports, which would make the replace-ref and graft cases pass
	// whatever the code does, or anything that points git at another repository, index or config.
	for _, v := range []string{"GIT_NO_REPLACE_OBJECTS", "GIT_GRAFT_FILE", "GIT_DIR", "GIT_WORK_TREE",
		"GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
