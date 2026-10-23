package main

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// push is one `git push` the command would run.
type push struct {
	Dir     string   // directory it runs in; "" when it cannot be known
	Sources []string // pushed commits: refspec sources, HEAD when there is none
	Deletes bool     // --delete, -d or a ":dst" refspec (with --delete, the named refs are deleted, not pushed)
	DryRun  bool
	Refused string // a form the gate cannot check; refused for a push from a clone of this project
}

// gitCall is a git subcommand other than push, which may be an alias.
type gitCall struct {
	Dir string // "" when unknown
	Sub string
}

// analysis is what findPushes learned about a command.
type analysis struct {
	Pushes []push
	Calls  []gitCall
	// Blocked says why the whole command is refused, wherever it runs: a git push the walker cannot
	// follow, or something that can point git at another repository, change its configuration,
	// hooks or aliases.
	Blocked string
}

const runAlone = "run `git push` as a command of its own, with literal arguments"

// normalize removes the quotes and backslashes bash removes, for the textual tests.
func normalize(s string) string {
	return strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(s)
}

// gitPushTokens matches a `git push` the walker could not resolve: a `git` word after a start,
// whitespace or a shell separator (so `;git`, `(git`, `=git` count), any git global options, then
// `push`. hookEvasion matches what would skip git's hook. Both are the last, conservative net for a
// command the walker could not parse or follow; the normal path parses the command.
var (
	gitPushTokens = regexp.MustCompile("(^|[\\s;&|()<>={}`])git(\\s+-\\S+)*\\s+push(\\s|$)")
	// hookEvasion matches what skips git's hook. GIT_ is case-sensitive so a lowercase word such as
	// the fixture name git_push.json is not mistaken for a GIT_* variable.
	hookEvasion = regexp.MustCompile(`(?i:no-verify|hookspath)|GIT_[A-Z]`)
)

func mentionsGitPush(s string) bool {
	n := normalize(s)
	return gitPushTokens.MatchString(n) || (strings.Contains(n, "push") && hookEvasion.MatchString(n))
}

// findPushes parses command as bash and returns what it does with git, starting in cwd.
func findPushes(command, cwd string) (analysis, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return analysis{}, err
	}
	w := newWalker(cwd, command)
	w.run(f)
	return analysis{Pushes: w.pushes, Calls: w.calls, Blocked: w.blocked}, nil
}

// run walks a parsed program on this walker: the statements, then the command and process
// substitutions the statement walk does not reach (a git push there is refused).
func (w *walker) run(f *syntax.File) {
	w.stmts(f.Stmts)
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && !w.seen[c] {
			probe := newWalker(w.dir, w.src)
			probe.call(c, nil)
			if len(probe.pushes) > 0 {
				w.block("a git push inside a command or process substitution cannot be checked; " + runAlone)
			}
			w.block(probe.blocked)
			w.calls = append(w.calls, probe.calls...)
		}
		return true
	})
}

type walker struct {
	dir     string // current directory; "" once it is unknown
	src     string // the whole command
	cmdText string // the text of the command currently being dispatched (its own words, not siblings)
	pushes  []push
	calls   []gitCall
	blocked string
	sourced bool // a file was sourced: it can change the directory and git's environment
	seen    map[*syntax.CallExpr]bool
}

func newWalker(dir, src string) *walker {
	return &walker{dir: dir, src: src, seen: map[*syntax.CallExpr]bool{}}
}

// wordsText prints words (and any heredoc or here-string bodies) as written, for the conservative
// text net. It is scoped to one command's own words, never the whole command line.
func wordsText(words []*syntax.Word, redirs []*syntax.Redirect) string {
	var b bytes.Buffer
	p := syntax.NewPrinter()
	for _, w := range words {
		_ = p.Print(&b, w)
		b.WriteByte(' ')
	}
	for _, r := range redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			_ = p.Print(&b, r.Hdoc)
		case syntax.WordHdoc:
			_ = p.Print(&b, r.Word)
		}
	}
	return b.String()
}

// block records why the command is refused; the first reason wins.
func (w *walker) block(msg string) {
	if w.blocked == "" {
		w.blocked = msg
	}
}

// blockText blocks the command when text names a git push the walker could not resolve.
func (w *walker) blockText(text, how string) {
	if mentionsGitPush(text) {
		w.block("a git push through " + how + " cannot be checked; " + runAlone)
	}
}

func (w *walker) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

// chainSeg is one statement of an && / || chain with the operator that precedes it (0 for the first).
type chainSeg struct {
	op   syntax.BinCmdOperator
	stmt *syntax.Stmt
}

// flattenChain returns the segments of a left-nested && / || chain in source order.
func flattenChain(c *syntax.BinaryCmd) []chainSeg {
	var out []chainSeg
	if x, ok := c.X.Cmd.(*syntax.BinaryCmd); ok && (x.Op == syntax.AndStmt || x.Op == syntax.OrStmt) {
		out = append(out, flattenChain(x)...)
	} else {
		out = append(out, chainSeg{stmt: c.X})
	}
	return append(out, chainSeg{op: c.Op, stmt: c.Y})
}

// pipedScript returns the script a literal producer pipes into a shell, when the pipeline is
// `PRODUCER | SHELL` with SHELL reading its script from standard input and PRODUCER's output known:
// `echo 'script'` or `cat <<'EOF' … EOF`. Anything else piped to a shell is out of reach.
func pipedScript(c *syntax.BinaryCmd) (string, bool) {
	if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
		return "", false
	}
	right, ok := c.Y.Cmd.(*syntax.CallExpr)
	if !ok || len(right.Args) == 0 {
		return "", false
	}
	if name, ok := literalWord(right.Args[0]); !ok || !shells[path.Base(name)] {
		return "", false
	}
	for _, a := range right.Args[1:] { // a -c script, a script file, or an option: handled elsewhere
		if v, ok := literalWord(a); !ok || (v != "-" && v != "-s" && strings.HasPrefix(v, "-")) {
			return "", false
		} else if !strings.HasPrefix(v, "-") {
			return "", false
		}
	}
	left, ok := c.X.Cmd.(*syntax.CallExpr)
	if !ok || len(left.Args) == 0 {
		return "", false
	}
	switch name, _ := literalWord(left.Args[0]); path.Base(name) {
	case "echo":
		var parts []string
		for _, a := range left.Args[1:] {
			v, ok := literalWord(a)
			if !ok || strings.HasPrefix(v, "-") {
				return "", false
			}
			parts = append(parts, v)
		}
		return strings.Join(parts, " "), true
	case "cat":
		for _, r := range c.X.Redirs {
			if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc || r.Op == syntax.WordHdoc {
				return heredocText(r, bodyOf(r))
			}
		}
	}
	return "", false
}

func bodyOf(r *syntax.Redirect) *syntax.Word {
	if r.Op == syntax.WordHdoc {
		return r.Word
	}
	return r.Hdoc
}

// isExit reports whether a statement ends the script: exit, return, or a call to a fatal helper.
func isExit(s *syntax.Stmt) bool {
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Args) == 0 {
		return false
	}
	name, ok := literalWord(c.Args[0])
	return ok && (name == "exit" || name == "return" || path.Base(name) == "die")
}

// anyNonExitOr reports whether a chain has an || whose successor is not exit/return, so a cd in the
// first segment might be skipped.
func anyNonExitOr(segs []chainSeg) bool {
	for _, s := range segs {
		if s.op == syntax.OrStmt && !isExit(s.stmt) {
			return true
		}
	}
	return false
}

func (w *walker) stmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	if s.Background || s.Coprocess {
		saved := w.dir // it runs in a subshell: a cd inside does not reach the commands after it
		w.cmd(s)
		w.dir = saved
		return
	}
	w.cmd(s)
}

// The directory is tracked only through what certainly runs. A compound command or function that
// may change it leaves it unknown, inside and after, so a push there is refused.
func (w *walker) cmd(s *syntax.Stmt) {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		w.seen[c] = true
		w.call(c, s.Redirs)
	case *syntax.DeclClause:
		w.decl(c)
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.AndStmt, syntax.OrStmt:
			// An && / || chain. The directory is carried forward only through a pure && prefix whose
			// segments certainly run: `cd ui && test && cd .. && git push` resolves the push's
			// directory. The first || whose successor is not exit/return, a cd that is not in that
			// prefix, or a negated segment that changes the directory, all mean a cd might be skipped
			// or rolled back, so the directory is unknown from there on and after the chain. `cd /x ||
			// exit; …` keeps the cd, because reaching past the exit means it succeeded.
			segments := flattenChain(c)
			unknownAfter := false
			poisoned := false // once set, the directory is unknown for the rest of the chain
			for i, seg := range segments {
				if i > 0 && seg.op == syntax.OrStmt && !isExit(seg.stmt) {
					poisoned = true
				}
				if poisoned {
					w.dir = ""
				}
				if i > 0 && seg.op == syntax.OrStmt && isExit(seg.stmt) {
					continue // the exit runs only on failure; reaching past it keeps the predecessor's cd
				}
				before := w.dir
				w.stmt(seg.stmt)
				changed := w.dir != before
				if changed && (i > 0 || anyNonExitOr(segments)) {
					unknownAfter = true // a cd that might be skipped: unknown for what follows the chain
				}
				if changed && (poisoned || seg.stmt.Negated) {
					poisoned = true // a negated cd inverts success, so its directory is uncertain
					w.dir = ""
				}
			}
			if unknownAfter || poisoned {
				w.dir = ""
			}
		default: // a pipeline: elements run in subshells, except that zsh keeps the last in the shell
			// A literal producer piped to a shell (`echo '…' | sh`, `cat <<'EOF' … | bash`) feeds that
			// shell a script the walker can read; parse it.
			if s, ok := pipedScript(c); ok {
				w.script(s)
			}
			saved := w.dir
			w.stmt(c.X)
			w.dir = saved
			w.stmt(c.Y)
			w.dir = saved
			if changesDir(c.Y) {
				w.dir = "" // zsh runs the last element in the current shell, so its cd can leak out
			}
		}
	case *syntax.Subshell:
		saved := w.dir
		w.stmts(c.Stmts)
		w.dir = saved
	case *syntax.Block:
		w.stmts(c.Stmts)
	case *syntax.IfClause, *syntax.CaseClause, *syntax.WhileClause, *syntax.ForClause:
		if fc, ok := c.(*syntax.ForClause); ok {
			if it, ok := fc.Loop.(*syntax.WordIter); ok && it.Name != nil {
				w.assign(it.Name.Value, nil, true)
			}
		}
		if changesDir(c) {
			w.dir = ""
			defer func() { w.dir = "" }()
		}
		syntax.Walk(c, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.IfClause:
				for e := n; e != nil; e = e.Else {
					w.stmts(e.Cond)
					w.stmts(e.Then)
				}
				return false
			case *syntax.CaseClause:
				for _, it := range n.Items {
					w.stmts(it.Stmts)
				}
				return false
			case *syntax.WhileClause:
				w.stmts(n.Cond)
				w.stmts(n.Do)
				return false
			case *syntax.ForClause:
				w.stmts(n.Do)
				return false
			}
			return true
		})
	case *syntax.TimeClause:
		w.stmt(c.Stmt)
	case *syntax.CoprocClause: // runs in a subshell
		saved := w.dir
		w.stmt(c.Stmt)
		w.dir = saved
	case *syntax.FuncDecl:
		// The body runs when the function is called, from wherever that is: a push in it is refused,
		// and a cd in it makes the directory unknown from here on.
		saved := w.dir
		w.dir = ""
		w.stmt(c.Body)
		w.dir = saved
		if changesDir(c.Body) {
			w.dir = ""
		}
	}
}

// decl handles declare, typeset, local, export and readonly. A GIT_* variable it sets, or marks for
// export, is refused; -p/-f/-F only print.
func (w *walker) decl(c *syntax.DeclClause) {
	variant := c.Variant.Value
	exportMode := variant == "export" || variant == "readonly"
	printMode := false
	for _, a := range c.Args {
		if a.Name == nil { // an option: -p, -x, -g, ...
			if v, ok := literalWord(a.Value); ok && strings.HasPrefix(v, "-") {
				// -p/-f/-F only print, but only for declare/typeset/local; bash's `export -p NAME=v`
				// and `readonly -p NAME=v` still set the named variable.
				if !exportMode && strings.ContainsAny(strings.TrimPrefix(v, "-"), "pfF") {
					printMode = true
				}
				if strings.Contains(v, "x") {
					exportMode = true
				}
			}
			continue
		}
		switch {
		case a.Value != nil || !a.Naked: // declare GIT_DIR=x, or declare GIT_DIR= (empty): a set
			w.assign(a.Name.Value, a.Value, false)
		case printMode: // declare -p GIT_DIR: a listing, sets nothing
		case exportMode: // export GIT_DIR: an existing value takes effect for child git
			w.assign(a.Name.Value, nil, true)
		}
	}
}

// safeGitEnv lists the GIT_* variables that change neither the repository nor git's configuration,
// hooks or aliases. Any other GIT_* variable set in the command refuses it (GIT_DIR, GIT_WORK_TREE,
// GIT_CONFIG_*, GIT_EXEC_PATH, ...), since it also reaches a git push in a script the command runs.
var safeGitEnv = map[string]bool{"GIT_TERMINAL_PROMPT": true, "GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true,
	"GIT_AUTHOR_DATE": true, "GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "GIT_COMMITTER_DATE": true,
	"GIT_OPTIONAL_LOCKS": true, "GIT_ADVICE": true, "GIT_MERGE_AUTOEDIT": true, "GIT_FLUSH": true,
	"GIT_PROGRESS_DELAY": true, "GIT_LFS_SKIP_SMUDGE": true, "GIT_CURL_VERBOSE": true, "GIT_SSH_VARIANT": true}

// gitEnvCommands are the GIT_* variables whose value is a command git runs: allowed with a literal
// value (including empty) that does not mention git.
var gitEnvCommands = map[string]bool{"GIT_PAGER": true, "GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true}

// assign checks a variable assignment. value is nil for a naked `export NAME` (naked true) or for an
// empty `NAME=` (naked false). A GIT_* assignment that could point git elsewhere is refused, as is a
// PATROL_SDLC_* override that would change which base the gate measures against.
func (w *walker) assign(name string, value *syntax.Word, naked bool) {
	// PATROL_SDLC_BASE only changes what hack/sdlc/check.sh measures locally; the gate (gate.sh)
	// unsets it, so it is not refused here — it is a normal variable for local iteration.
	if !strings.HasPrefix(name, "GIT_") || safeGitEnv[name] || strings.HasPrefix(name, "GIT_TRACE") {
		return
	}
	if gitEnvCommands[name] {
		if value == nil { // empty value disables the pager/editor
			return
		}
		if v, ok := literalWord(value); ok && !strings.Contains(v, "git") && !strings.Contains(v, "push") {
			return
		}
	}
	_ = naked
	w.block(name + " can point git at another repository or change its configuration, hooks or the commands it runs, also for a push in a script; run the command without it")
}

// assignWord checks an assignment written as one NAME=VALUE word (an env prefix).
func (w *walker) assignWord(assign string) {
	name, _, _ := strings.Cut(assign, "=")
	w.assign(name, nil, false)
}

// changesDir reports whether node runs a command that can change the directory.
func changesDir(node syntax.Node) bool {
	found := false
	syntax.Walk(node, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			i := 0
			for i < len(c.Args) { // skip command/builtin/exec/noglog precommand words
				w, ok := literalWord(c.Args[i])
				if ok && (path.Base(w) == "command" || path.Base(w) == "builtin" || path.Base(w) == "exec") {
					i++
					continue
				}
				break
			}
			name, ok := literalWord(c.Args[min(i, len(c.Args)-1)])
			switch path.Base(strings.TrimPrefix(name, "=")) {
			case "cd", "pushd", "popd", "source", ".", "eval", "trap":
				found = true
			}
			found = found || !ok
		}
		return !found
	})
	return found
}

// expandLiteral returns the fields bash makes of word when they do not depend on the environment,
// the filesystem or another command: quote removal, escapes, $'…', brace expansion and ~. ok is
// false for a word with a parameter, command, arithmetic or process substitution, or an unquoted
// glob.
func expandLiteral(word *syntax.Word) ([]string, bool) {
	if word == nil {
		return nil, false
	}
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if pattern.HasMeta(p.Value, 0) {
				return nil, false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if _, ok := q.(*syntax.Lit); !ok {
					return nil, false
				}
			}
		default:
			return nil, false
		}
	}
	fs, err := expand.Fields(&expand.Config{Env: expand.ListEnviron("HOME=" + home)}, word)
	if err != nil {
		return nil, false
	}
	return fs, true
}

// literalWord is expandLiteral for a word that must make exactly one field.
func literalWord(word *syntax.Word) (string, bool) {
	fs, ok := expandLiteral(word)
	if !ok || len(fs) != 1 {
		return "", false
	}
	return fs[0], true
}

// literalPrefix returns the leading literal text of a word (up to its first expanded part), so a
// non-literal option such as `safe.directory=$PWD` still yields its key.
func literalPrefix(word *syntax.Word) string {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if pattern.HasMeta(p.Value, 0) {
				return b.String()
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return b.String()
				}
				b.WriteString(lit.Value)
			}
		default:
			return b.String()
		}
	}
	return b.String()
}

// fields expands words into arguments. For a non-literal word, lit is false and the argument holds
// the word's literal prefix (its leading fixed text), so an option's name survives a dynamic value.
func fields(words []*syntax.Word) (args []string, lit []bool) {
	for _, word := range words {
		if fs, ok := expandLiteral(word); ok {
			for _, f := range fs {
				args, lit = append(args, f), append(lit, true)
			}
			continue
		}
		args, lit = append(args, literalPrefix(word)), append(lit, false)
	}
	return args, lit
}

var home, _ = os.UserHomeDir()

func (w *walker) call(c *syntax.CallExpr, redirs []*syntax.Redirect) {
	for _, a := range c.Assigns {
		if a.Name != nil {
			w.assign(a.Name.Value, a.Value, a.Naked && a.Value == nil)
		}
	}
	saved := w.cmdText
	w.cmdText = wordsText(c.Args, redirs) // this command's own text for the conservative net
	args, lit := fields(c.Args)
	w.command(args, lit, redirs)
	w.cmdText = saved
}

// command dispatches an already-expanded argv.
func (w *walker) command(args []string, lit []bool, redirs []*syntax.Redirect) {
prefix:
	for len(args) > 0 && lit[0] {
		switch path.Base(args[0]) {
		case "command", "exec", "builtin", "noglob", "nocorrect":
			// They run the rest as a command; zsh's noglob/nocorrect are precommand modifiers. With an
			// option (exec -a NAME, command -p), the grammar varies and -v/-V only print, so fall back
			// to a conservative text check. (noglob/nocorrect take no options.)
			base := path.Base(args[0])
			if (base == "command" || base == "exec" || base == "builtin") && len(args) > 1 && lit[1] && strings.HasPrefix(args[1], "-") {
				w.blockText(w.cmdText, base+" with options")
				return
			}
			args, lit = args[1:], lit[1:]
		case "env":
			args, lit = args[1:], lit[1:]
		envOpts:
			for len(args) > 0 && lit[0] {
				switch a := args[0]; {
				case !strings.HasPrefix(a, "-") && strings.Contains(a, "="):
					w.assignWord(a)
					args, lit = args[1:], lit[1:]
				case a == "-i" || a == "--ignore-environment" || a == "-0" || a == "-v" || a == "--debug":
					args, lit = args[1:], lit[1:]
				case a == "-u" || a == "--unset":
					args, lit = args[min(2, len(args)):], lit[min(2, len(lit)):]
				case a == "--":
					// Options end, but NAME=VALUE operands after -- still set the environment.
					args, lit = args[1:], lit[1:]
					for len(args) > 0 && lit[0] && !strings.HasPrefix(args[0], "-") && strings.Contains(args[0], "=") {
						w.assignWord(args[0])
						args, lit = args[1:], lit[1:]
					}
					break envOpts
				case strings.HasPrefix(a, "-"): // -C DIR, -S STRING, unknown: do not misread the command
					w.blockText(w.cmdText, "env with options")
					return
				default:
					break envOpts
				}
			}
		default:
			break prefix
		}
	}
	if len(args) == 0 {
		return
	}
	if !lit[0] {
		for i := 1; i < len(args); i++ {
			if lit[i] && args[i] == "push" {
				w.block("a git push through a command word that is not literal cannot be checked; " + runAlone)
				return
			}
		}
		return
	}
	name := path.Base(strings.TrimPrefix(args[0], "=")) // zsh =cmd expands to the path of cmd
	if sub, ok := strings.CutPrefix(name, "git-"); ok && sub != "" {
		// git's dashed subcommand binaries: git-push is `git push`, so analyse it as git <sub>.
		w.git(append([]string{sub}, args[1:]...), append([]bool{true}, lit[1:]...))
		return
	}
	_, isWrapper := wrapperSkip[name]
	switch {
	case name == "cd" || name == "pushd":
		w.chdir(name, args[1:], lit[1:])
	case name == "popd":
		w.dir = ""
	case name == "source" || name == ".":
		w.dir, w.sourced = "", true
	case name == "git":
		w.git(args[1:], lit[1:])
	case shells[name]:
		w.shell(args, lit, redirs)
	case name == "fish" || name == "csh" || name == "tcsh":
		w.blockText(w.cmdText, name) // their syntax is not bash's
	case name == "eval":
		w.eval(args[1:], lit[1:])
	case name == "trap": // trap [--] ACTION SIGSPEC...: the action runs as a command on the signal
		a, l := args[1:], lit[1:]
		for len(a) > 0 && l[0] && (a[0] == "--" || a[0] == "-p" || a[0] == "-l") {
			if a[0] == "-l" { // -l only lists signal names
				a = nil
				break
			}
			a, l = a[1:], l[1:]
		}
		if len(a) > 0 {
			if l[0] {
				w.script(a[0])
			} else {
				w.blockText(w.cmdText, "trap")
			}
		}
	case name == "xargs" || name == "parallel":
		w.xargs(name, args[1:], lit[1:])
	case name == "read" || name == "mapfile" || name == "readarray" || name == "getopts":
		w.setter(name, args[1:], lit[1:])
	case name == "printf":
		w.printf(args[1:], lit[1:])
	case name == "echo" || pureOutput[name]:
		// prints its arguments
	case isWrapper:
		w.wrapped(name, args, lit, redirs)
	case execRunners[name]:
		w.execRunner(args, lit)
	case name == "find":
		w.findExec(args, lit)
	default:
		// An unrecognized command: it is not a known runner, so its arguments are usually data. But a
		// git push plainly in its arguments (a custom runner, ssh, a tool not modelled) is refused, as
		// the contract promises for "another command that runs its arguments".
		if argvHasGitPush(args, lit) {
			w.block("a git push run through " + name + " cannot be checked; " + runAlone)
		}
	}
}

// argvHasGitPush reports whether argv holds a literal `git` whose subcommand (after git's global
// options) is push. It is the conservative net for a command the walker does not otherwise model;
// `git` and `push` as separate data arguments (grep -e git -e push) do not match.
func argvHasGitPush(args []string, lit []bool) bool {
	for i := range args {
		if !lit[i] || path.Base(args[i]) != "git" {
			continue
		}
		for j := i + 1; j < len(args); j++ {
			if !lit[j] {
				break
			}
			a := args[j]
			if !strings.HasPrefix(a, "-") {
				return a == "push"
			}
			name, _, hasValue := strings.Cut(a, "=")
			if !gitGlobalSafe[name] && !gitGlobalWithValue[name] {
				break // not a git global option: `git` here is data (grep -e git -e push), not a command
			}
			if gitGlobalWithValue[name] && !hasValue {
				j++ // its value is the next argument
			}
		}
	}
	return false
}

var (
	shells     = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "mksh": true}
	pureOutput = map[string]bool{"true": true, "false": true, ":": true, "test": true, "[": true, "cat": true}
	// execRunners run the command after a `--` separator (and are refused on a plain adjacent git push).
	execRunners = map[string]bool{"kubectl": true, "oc": true, "docker": true, "podman": true, "nerdctl": true, "crictl": true, "ctr": true}
)

// wrapperSkip maps a wrapper that runs its trailing argument as a command to the options that take a
// value and the number of leading operands to skip (timeout's DURATION). After them, the command is
// parsed; a git push there is refused like a bare one.
var wrapperSkip = map[string]struct {
	value    map[string]bool
	operands int
}{
	"timeout":    {map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, 1},
	"nice":       {map[string]bool{"-n": true, "--adjustment": true}, 0},
	"ionice":     {map[string]bool{"-c": true, "-n": true, "-p": true, "-P": true, "-u": true, "-t": true}, 0},
	"stdbuf":     {map[string]bool{"-i": true, "-o": true, "-e": true}, 0},
	"nohup":      {nil, 0},
	"setsid":     {nil, 0},
	"caffeinate": {map[string]bool{"-t": true, "-w": true}, 0},
	"sudo":       {map[string]bool{"-u": true, "-g": true, "-U": true, "-C": true, "-h": true, "-p": true, "-r": true, "-t": true, "-A": true, "-D": true, "-R": true}, 0},
	"doas":       {map[string]bool{"-u": true, "-C": true}, 0},
	"chrt":       {map[string]bool{"-T": true, "-P": true}, 0},
	"time":       {map[string]bool{"-o": true, "-f": true, "--format": true}, 0},
	"watch":      {map[string]bool{"-n": true, "--interval": true, "-d": true}, 0},
	"stdstub":    {nil, 0},
}

// skipOptions drops leading -options (and the value of those in value), then n operands.
func skipOptions(args []string, lit []bool, value map[string]bool, operands int) ([]string, []bool) {
	i := 0
	for i < len(args) && lit[i] && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		if args[i] == "--" {
			i++
			break
		}
		opt, _, hasEq := strings.Cut(args[i], "=")
		i++
		if value[opt] && !hasEq && i < len(args) {
			i++ // the option's value is the next argument
		}
	}
	for ; operands > 0 && i < len(args) && lit[i]; operands-- {
		i++
	}
	return args[i:], lit[i:]
}

// wrapped parses the command a wrapper runs.
func (w *walker) wrapped(name string, args []string, lit []bool, redirs []*syntax.Redirect) {
	skip := wrapperSkip[name]
	inner, innerLit := skipOptions(args[1:], lit[1:], skip.value, skip.operands)
	if len(inner) == 0 {
		return
	}
	if !lit[0] || !innerLit[0] { // an option we did not model, or a non-literal command: fall back
		w.blockText(w.cmdText, name)
		return
	}
	w.command(inner, innerLit, redirs)
}

// execRunner parses the command run after a `--` (kubectl exec POD -- CMD); without one, only a
// plain adjacent `git push` in the arguments is refused (so a `--push` flag or a git_push.json file
// name does not trip it), and anything else is left to git's hook.
func (w *walker) execRunner(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		if lit[i] && args[i] == "--" {
			if i+1 < len(args) {
				w.command(args[i+1:], lit[i+1:], nil)
			}
			return
		}
	}
	if argvHasGitPush(args, lit) {
		w.block("a git push run through " + path.Base(args[0]) + " cannot be checked; " + runAlone)
	}
}

// findExec parses the command of a find -exec / -execdir ... ; or +.
func (w *walker) findExec(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		if !lit[i] || (args[i] != "-exec" && args[i] != "-execdir") {
			continue
		}
		end := i + 1
		for end < len(args) && (!lit[end] || (args[end] != ";" && args[end] != "+")) {
			end++
		}
		if end > i+1 {
			w.command(args[i+1:end], lit[i+1:end], nil)
		}
		i = end
	}
}

// eval runs its argument as a script in the current shell, so a literal script's cd, sourced state,
// pushes and refusals all carry back. A dynamic string leaves the directory and sourced state
// unknown, and is refused if eval's own words (not sibling commands) name a push.
func (w *walker) eval(args []string, lit []bool) {
	for _, l := range lit {
		if !l {
			w.blockText(w.cmdText, "eval")
			w.dir, w.sourced = "", true
			return
		}
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(strings.Join(args, " ")), "")
	if err != nil {
		if mentionsGitPush(strings.Join(args, " ")) {
			w.block("cannot parse the string eval runs around git push; " + runAlone)
		}
		return
	}
	w.run(f) // the same walker: eval runs in the current shell, so its cd and source carry back
}

// xargs parses the command xargs or parallel builds. It appends standard input to that command, so
// a git push there (the input adds refs and could add --no-verify), a bare `git` (its subcommand
// would come from input) and a non-literal command word are all refused.
func (w *walker) xargs(name string, args []string, lit []bool) {
	value := map[string]bool{"-n": true, "--max-args": true, "-L": true, "-l": true, "-P": true, "--max-procs": true,
		"-s": true, "--max-chars": true, "-I": true, "-i": true, "--replace": true, "-E": true, "-e": true, "--eof": true,
		"-d": true, "--delimiter": true, "-a": true, "--arg-file": true, "-j": true, "-N": true}
	inner, innerLit := skipOptions(args, lit, value, 0)
	if len(inner) == 0 {
		return // the default command is echo
	}
	if !innerLit[0] {
		w.blockText(w.cmdText, name)
		return
	}
	// Run the command on a fresh walker: a push there (including a bare `git`, whose subcommand the
	// input would supply) refuses the whole command, since the input adds arguments the gate cannot
	// see; any other command (git branch, git add, grep) is allowed.
	s := newWalker(w.dir, w.src)
	if path.Base(inner[0]) == "git" && len(inner) == 1 {
		s.pushes = append(s.pushes, push{})
	} else {
		s.command(inner, innerLit, nil)
	}
	w.calls = append(w.calls, s.calls...)
	if len(s.pushes) > 0 || s.blocked != "" {
		w.block("a git push through " + name + " cannot be checked (it appends arguments from standard input); " + runAlone)
	}
}

// setter checks the variable names a read, mapfile, readarray or getopts sets.
func (w *walker) setter(name string, args []string, lit []bool) {
	value := map[string]bool{"-d": true, "-n": true, "-N": true, "-p": true, "-t": true, "-u": true, "-i": true, "-a": true, "-C": true, "-c": true, "-O": true, "-s": true}
	// -a NAME (read into an array) and the trailing NAMEs are targets.
	for i := 0; i < len(args); i++ {
		if !lit[i] {
			continue
		}
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "--" {
			if (a == "-a" || a == "-A") && i+1 < len(args) && lit[i+1] {
				w.assign(args[i+1], nil, true)
				i++
			} else if value[a] && i+1 < len(args) {
				i++
			}
			continue
		}
		if a == "--" {
			continue
		}
		if name == "getopts" { // getopts OPTSTRING NAME: the second operand
			if i+1 < len(args) && lit[i+1] {
				w.assign(args[i+1], nil, true)
			}
			return
		}
		w.assign(a, nil, true) // a plain NAME operand
	}
}

// printf checks only the target of printf -v NAME.
func (w *walker) printf(args []string, lit []bool) {
	for i := 0; i+1 < len(args); i++ {
		if lit[i] && args[i] == "-v" {
			if lit[i+1] {
				w.assign(args[i+1], nil, true)
			}
			return
		}
		if lit[i] && !strings.HasPrefix(args[i], "-") {
			return // the format operand: the rest is data
		}
	}
}

// shellWithValue lists shell options that take the next argument as their value.
var shellWithValue = map[string]bool{"--rcfile": true, "--init-file": true}

// shell handles a shell that runs a script: `bash -c SCRIPT`, or a heredoc or here-string on its
// standard input. A literal script is parsed like the command itself; a script file is not read
// (git's own hook checks what it pushes).
func (w *walker) shell(args []string, lit []bool, redirs []*syntax.Redirect) {
	command, stdin := false, false
	i := 1
options:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case !lit[i]:
			w.blockText(w.cmdText, "a shell argument that is not literal")
			return
		case a == "--" || a == "-":
			i++
			break options
		case shellWithValue[a]:
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for _, ch := range a[1:] {
				switch ch {
				case 'c':
					command = true
				case 's':
					stdin = true
				case 'o', 'O':
					i++ // each takes the next argument as its value
				}
			}
		default:
			break options
		}
	}
	operands := args[min(i, len(args)):]
	switch {
	case command:
		if len(operands) == 0 {
			return
		}
		if !lit[i] {
			w.blockText(w.cmdText, "a shell script that is not literal")
			return
		}
		w.script(operands[0])
		return
	case !stdin && len(operands) > 0 && !lit[i]:
		w.blockText(w.cmdText, "a shell script that is not literal")
		return
	case !stdin && len(operands) > 0 && operands[0] != "/dev/stdin" && operands[0] != "/dev/fd/0":
		return // a script file
	}
	for _, r := range redirs {
		var body *syntax.Word
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			body = r.Hdoc
		case syntax.WordHdoc:
			body = r.Word
		default:
			continue
		}
		if body == nil {
			continue
		}
		if s, ok := heredocText(r, body); ok {
			w.script(s)
		} else {
			w.blockText(w.cmdText, "a shell script that is not literal")
		}
		return
	}
	// The script comes from a pipe or a file. A literal producer (echo/cat) piped in is handled by
	// pipedScript in the pipeline branch; anything else is out of the hook's reach, like a script
	// file, and left to git's own pre-push hook.
}

// heredocText returns the script a heredoc or here-string gives a shell, when it has no expansions.
func heredocText(r *syntax.Redirect, body *syntax.Word) (string, bool) {
	if r.Op == syntax.WordHdoc {
		return literalWord(body)
	}
	var raw strings.Builder
	for _, part := range body.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			return "", false // $x, $(...): expanded when the shell reads it
		}
		raw.WriteString(lit.Value)
	}
	if r.Word != nil && r.Word.Lit() != "" && !strings.Contains(r.Word.Lit(), `\`) {
		s, err := expand.Document(&expand.Config{Env: expand.ListEnviron("HOME=" + home)}, body)
		return s, err == nil
	}
	return raw.String(), true // <<'EOF': the body as written
}

// script parses the text a shell runs as a script.
func (w *walker) script(s string) {
	inner, err := findPushes(s, w.dir)
	switch {
	case err != nil:
		if mentionsGitPush(s) {
			w.block("cannot parse the shell script around git push; " + runAlone)
		}
	default:
		w.block(inner.Blocked)
		w.pushes = append(w.pushes, inner.Pushes...)
		w.calls = append(w.calls, inner.Calls...)
	}
}

func (w *walker) chdir(name string, args []string, lit []bool) {
	if name == "pushd" && len(args) > 0 && lit[0] && (strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[0], "+")) {
		w.dir = "" // -n, +N, -N
		return
	}
	for len(args) > 0 && lit[0] && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		if args[0] == "--" {
			args, lit = args[1:], lit[1:]
			break
		}
		args, lit = args[1:], lit[1:] // -L, -P, -e, -@
	}
	if len(args) == 0 || !lit[0] || args[0] == "-" {
		w.dir = "" // $HOME, OLDPWD or a variable: unknown here
		return
	}
	if !filepath.IsAbs(args[0]) && os.Getenv("CDPATH") != "" {
		w.dir = "" // CDPATH may take a relative cd elsewhere
		return
	}
	w.dir = w.resolve(args[0])
}

func (w *walker) resolve(dir string) string {
	return w.resolveFrom(w.dir, dir)
}

func (w *walker) resolveFrom(base, dir string) string {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, dir)
}

// gitGlobalWithValue lists git's global options that take the next argument as their value, so the
// subcommand after them is found correctly.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true, "--super-prefix": true, "--attr-source": true, "--list-cmds": true,
	"--shallow-file": true}

// gitPushLike lists git subcommands that push without running the pre-push hook; they are refused.
var gitPushLike = map[string]bool{"send-pack": true, "http-push": true}

// gitGlobalSafe lists git's global options that change neither the repository nor its hooks; -c is
// checked on its own. Any other global option before push refuses the command.
var gitGlobalSafe = map[string]bool{"-C": true, "-c": true, "-p": true, "--paginate": true, "-P": true, "--no-pager": true,
	"--no-replace-objects": true, "--literal-pathspecs": true, "--glob-pathspecs": true, "--noglob-pathspecs": true,
	"--icase-pathspecs": true, "--no-optional-locks": true, "--no-advice": true, "--no-lazy-fetch": true, "--bare": false}

// gitRunners are git subcommands that can run another command given in a specific argument.
var gitRunners = map[string]bool{"rebase": true, "bisect": true, "submodule": true, "for-each-repo": true,
	"filter-branch": true, "difftool": true, "mergetool": true}

func (w *walker) git(args []string, lit []bool) {
	dir := w.dir
	unsafe, unknownOpt := "", false
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !lit[i] && !strings.HasPrefix(a, "-") {
			w.block("a git option or subcommand that is not literal cannot be checked; " + runAlone)
			return
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		name, value, hasValue := strings.Cut(a, "=")
		litVal := lit[i]
		if gitGlobalWithValue[name] && !hasValue {
			if i+1 >= len(args) {
				return
			}
			i++
			value, litVal = args[i], lit[i]
		}
		if !gitGlobalSafe[name] && unsafe == "" {
			unsafe = name // an unknown global option, or --bare: it may move the repository
		}
		if !gitGlobalSafe[name] && !gitGlobalWithValue[name] {
			unknownOpt = true // the subcommand after it cannot be located reliably
		}
		switch name {
		case "-C":
			if litVal {
				dir = w.resolveFrom(dir, value)
			} else {
				dir = ""
			}
		case "-c", "--config-env":
			// -c sets config directly; --config-env names an environment variable holding the value,
			// but the key is still literal. An unsafe or unreadable key is refused whatever the
			// subcommand, since it can define an alias for that subcommand.
			key, _, _ := strings.Cut(value, "=")
			switch {
			case key == "" || (!litVal && !strings.Contains(value, "=")):
				w.block("git " + name + " with a key the hook cannot read is refused in a command; set git configuration in a command of its own")
				return
			case safeConfigKey(key):
			default:
				w.block(name + " " + key + ": " + refuseConfig(key))
				return
			}
		}
	}
	if i >= len(args) {
		return
	}
	sub, rest, restLit := args[i], args[i+1:], lit[i+1:]
	if !lit[i] {
		w.block("a git subcommand that is not literal cannot be checked; " + runAlone)
		return
	}
	if unknownOpt && sub != "push" {
		// An unmodelled global option may have swallowed the real subcommand; if a push is anywhere
		// after it, fail closed.
		for _, a := range rest {
			if a == "push" {
				w.block("a git global option the hook does not model comes before push; " + runAlone)
				return
			}
		}
	}
	if unsafe != "" {
		dir = "" // the repository is not the directory's
	}
	switch {
	case sub == "push":
	case gitPushLike[sub]:
		w.block("git " + sub + " pushes without running git's pre-push hook; use `git push`")
		return
	case sub == "config":
		w.gitConfig(rest, restLit)
		return
	case gitRunners[sub]:
		w.gitRunner(sub, rest, restLit)
		return
	default:
		w.calls = append(w.calls, gitCall{Dir: dir, Sub: sub})
		return
	}
	if unsafe != "" {
		w.block("git " + unsafe + " can point git at another repository or configuration, where the pre-push hook does not run; push from the work tree without it")
		return
	}
	if w.sourced {
		w.block("a file sourced earlier in the command can change the directory and git's environment; " + runAlone)
		return
	}
	if p, ok := pushArgs(dir, rest, restLit); ok {
		w.pushes = append(w.pushes, p)
	}
}

// gitRunner refuses a push in the command-carrying argument of a git subcommand that runs one:
// rebase -x/--exec, bisect run, submodule foreach, difftool/mergetool -x/--extcmd. Such a push runs
// per rebase step or per submodule, which the gate cannot check; refs and paths are data.
func (w *walker) gitRunner(sub string, args []string, lit []bool) {
	how := "git " + sub
	// carries runs probe on a fresh walker; a push or refusal it finds refuses the whole command.
	carries := func(probe func(*walker)) {
		s := newWalker(w.dir, w.src)
		probe(s)
		w.calls = append(w.calls, s.calls...)
		if len(s.pushes) > 0 || s.blocked != "" {
			w.block("a git push through " + how + " cannot be checked (it runs per step); " + runAlone)
		}
	}
	// filterOpts carry a command (or shell snippet) per subcommand.
	filterOpts := map[string]map[string]bool{
		"rebase":        {"-x": true, "--exec": true},
		"difftool":      {"-x": true, "--extcmd": true},
		"mergetool":     {"--tool": false},
		"filter-branch": {"--tree-filter": true, "--index-filter": true, "--parent-filter": true, "--msg-filter": true, "--commit-filter": true, "--tag-name-filter": true, "--env-filter": true},
	}
	switch sub {
	case "rebase", "difftool", "filter-branch":
		opts := filterOpts[sub]
		for i := 0; i < len(args); i++ {
			if !lit[i] {
				continue
			}
			name, value, hasEq := strings.Cut(args[i], "=")
			if !opts[name] {
				continue
			}
			switch {
			case hasEq:
				carries(func(s *walker) { s.script(value) })
			case i+1 < len(args) && lit[i+1]:
				carries(func(s *walker) { s.script(args[i+1]) })
				i++
			case i+1 < len(args):
				w.blockText(w.cmdText, how)
			}
		}
	case "bisect":
		if len(args) > 0 && lit[0] && args[0] == "run" {
			carries(func(s *walker) { s.command(args[1:], lit[1:], nil) })
		}
	case "for-each-repo":
		// git for-each-repo --config=<key> [--] <git subcommand…>: it runs `git <subcommand>` per repo.
		rest, restLit := skipOptions(args, lit, map[string]bool{"--config": true, "-C": true}, 0)
		if len(rest) > 0 {
			carries(func(s *walker) { s.git(rest, restLit) })
		}
	case "submodule":
		for i := 0; i < len(args); i++ {
			if !lit[i] || args[i] != "foreach" {
				continue
			}
			rest, restLit := skipOptions(args[i+1:], lit[i+1:], nil, 0) // --recursive, -q
			if len(rest) == 0 {
				return
			}
			if restLit[0] {
				carries(func(s *walker) { s.script(rest[0]) })
			} else {
				w.blockText(w.cmdText, how)
			}
			return
		}
	}
}

// safeConfig lists the configuration keys (lower case) that `git -c` and `git config` may set in a
// command: none of them runs a command or changes the repository, git's hooks or aliases. Any other
// key refuses the command (core.hooksPath, alias.*, include.*, core.pager, ...).
var (
	safeConfigSections = []string{"color.", "advice.", "user.", "i18n.", "log.", "format.", "column.", "status.", "pretty.", "blame.", "diff.", "merge.", "rebase.", "push.", "pull.", "fetch.", "branch.", "tag.", "commit.", "gc.", "maintenance."}
	safeConfigKeys     = map[string]bool{"core.quotepath": true, "core.abbrev": true, "core.longpaths": true, "core.filemode": true,
		"core.ignorecase": true, "core.autocrlf": true, "core.safecrlf": true, "init.defaultbranch": true, "safe.directory": true,
		"protocol.version": true, "http.version": true, "protocol.file.allow": true, "feature.manyfiles": true,
		"index.threads": true, "pack.threads": true, "checkout.workers": true, "transfer.fsckobjects": true}
)

// unsafeConfigKeys are keys in the safe sections that still run a command or change hooks; they are
// refused. The *.command/textconv/driver forms carry a subsection (diff.<name>.command), matched by
// suffix.
var (
	unsafeConfigKeys    = map[string]bool{"core.pager": true, "core.editor": true, "core.sshcommand": true, "core.hookspath": true, "core.fsmonitor": true, "core.askpass": true, "diff.external": true, "merge.tool": true, "gc.recentobjectshook": true}
	unsafeConfigSuffix  = []string{".command", ".textconv", ".driver", ".smudge", ".clean", ".process", ".helper"}
	safeConfigTwoLevels = map[string]bool{"diff.": true, "merge.": true} // only section.key, no subsection driver
)

func safeConfigKey(key string) bool {
	key = strings.ToLower(key)
	if unsafeConfigKeys[key] {
		return false
	}
	for _, suf := range unsafeConfigSuffix {
		if strings.HasSuffix(key, suf) {
			return false
		}
	}
	if safeConfigKeys[key] {
		return true
	}
	section := strings.SplitN(key, ".", 2)[0] + "."
	for _, s := range safeConfigSections {
		if s != section {
			continue
		}
		if safeConfigTwoLevels[section] && strings.Count(key, ".") != 1 {
			return false // diff.<driver>.* / merge.<driver>.* may run a command
		}
		return true
	}
	return false
}

func refuseConfig(key string) string {
	if key == "" {
		key = "(edit)"
	}
	return "git configuration " + key + " in a command is not on the hook's list of safe settings (it could change git's hooks, aliases or repository, or run a command); change it in a command of its own, by hand, or use an option of the git command"
}

// gitConfig refuses a `git config` that writes a key outside the safe list. A read is allowed, and
// only the key (not its value) needs to be literal. For --rename-section both section names count.
func (w *walker) gitConfig(args []string, lit []bool) {
	type pos struct {
		text string
		lit  bool
	}
	var positional []pos
	write, rename := false, false
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			positional = append(positional, pos{a, false})
			continue
		}
		switch {
		case a == "--get" || a == "--get-all" || a == "--get-regexp" || a == "--get-urlmatch" || a == "--get-color" ||
			a == "--get-colorbool" || a == "-l" || a == "--list" || a == "--name-only":
			return // a read
		case a == "-e" || a == "--edit":
			w.block(refuseConfig(""))
			return
		case a == "--rename-section" || a == "--remove-section":
			write, rename = true, true
		case a == "--add" || a == "--replace-all" || a == "--unset" || a == "--unset-all":
			write = true
		case a == "-f" || a == "--file" || a == "--blob" || a == "--type" || a == "-t" || a == "--default" || a == "--comment" || a == "--value":
			j++
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, pos{a, true})
		}
	}
	if len(positional) > 0 && positional[0].lit {
		switch positional[0].text {
		case "get", "list":
			return
		case "rename-section", "remove-section":
			write, rename = true, true
			positional = positional[1:]
		case "set", "unset", "edit":
			write = true
			positional = positional[1:]
		}
	}
	if !write && len(positional) < 2 { // a read of one key, or none
		return
	}
	if len(positional) == 0 {
		w.block(refuseConfig(""))
		return
	}
	// The key is the first positional; for rename/remove-section every positional is a section name.
	keys := positional[:1]
	if rename {
		keys = positional
	}
	for _, k := range keys {
		if !k.lit {
			w.block("git config writes a key the hook cannot read; change git's configuration in a command of its own")
			return
		}
		if !safeConfigKey(k.text) {
			w.block(refuseConfig(k.text))
			return
		}
	}
}

const (
	optFlag     = iota // no value
	optValue           // the next argument, or --opt=value
	optOptional        // only as --opt=value
)

// pushLong lists git push's long options (git 2.56: `git push --git-completion-helper-all`). git also
// accepts --no-OPT for each of them but --verify, --no-verify, --ipv4 and --ipv6, and any unambiguous
// abbreviation, which the hook refuses: an abbreviated --no-verify would skip the pre-push hook.
var pushLong = map[string]int{
	"--verbose": optFlag, "--quiet": optFlag, "--repo": optValue, "--all": optFlag, "--branches": optFlag, "--mirror": optFlag,
	"--delete": optFlag, "--tags": optFlag, "--dry-run": optFlag, "--porcelain": optFlag, "--force": optFlag,
	"--force-with-lease": optOptional, "--force-if-includes": optFlag, "--recurse-submodules": optValue, "--thin": optFlag,
	"--receive-pack": optValue, "--exec": optValue, "--set-upstream": optFlag, "--progress": optFlag, "--prune": optFlag,
	"--no-verify": optFlag, "--verify": optFlag, "--follow-tags": optFlag, "--signed": optOptional, "--atomic": optFlag,
	"--push-option": optValue, "--ipv4": optFlag, "--ipv6": optFlag,
}

var noNegation = map[string]bool{"--verify": true, "--no-verify": true, "--ipv4": true, "--ipv6": true}

// pushArgs reads the arguments after `git push`. ok is false when git would not push (-h, --help).
func pushArgs(dir string, args []string, lit []bool) (push, bool) {
	p := push{Dir: dir}
	refuse := func(msg string) {
		if p.Refused == "" {
			p.Refused = msg
		}
	}
	var deleteFlag, deleteRefspec, noVerify bool
	every := map[string]bool{} // --all, --branches, --mirror, --tags: the last of each wins
	positional := 0
	options := true
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			refuse("`git push` has an argument that is not literal: it could be an option, or a ref the gate cannot resolve; " + runAlone)
			continue
		}
		switch {
		case options && a == "--":
			options = false
			continue
		case options && (a == "--help" || strings.HasPrefix(a, "--git-completion-helper")):
			return push{}, false
		case options && strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			kind, known := pushLong[name]
			on := true
			if base := "--" + strings.TrimPrefix(name, "--no-"); !known && strings.HasPrefix(name, "--no-") && !noNegation[base] {
				if _, ok := pushLong[base]; ok {
					name, kind, on, known = base, optFlag, false, true
				}
			}
			if !known {
				refuse("unknown or abbreviated git push option " + name + " (git accepts abbreviations, such as one of --no-verify); spell options out in full")
				continue
			}
			switch name {
			case "--dry-run":
				p.DryRun = on
			case "--delete":
				deleteFlag = on
			case "--no-verify":
				noVerify = true
			case "--verify":
				noVerify = false
			case "--all", "--branches", "--mirror", "--tags":
				every[name] = on
			}
			if kind == optValue && on && !hasValue {
				j++
			}
			continue
		case options && strings.HasPrefix(a, "-") && a != "-":
			for k := 1; k < len(a); k++ {
				switch a[k] {
				case 'v', 'q', 'f', 'u', '4', '6':
				case 'd':
					deleteFlag = true
				case 'n':
					p.DryRun = true
				case 'h':
					return push{}, false
				case 'o':
					if k == len(a)-1 {
						j++ // the value is the next argument
					}
					k = len(a) // or the rest of this one
				default:
					refuse("unknown git push option -" + string(a[k]) + "; spell options out in full")
				}
			}
			continue
		}
		positional++
		if positional == 1 {
			continue // the remote
		}
		// git push origin tag NAME, but only when NAME is a tag name, not an option git parses first.
		if a == "tag" && j+1 < len(args) && lit[j+1] && !strings.HasPrefix(args[j+1], "-") {
			p.Sources = append(p.Sources, "refs/tags/"+args[j+1])
			j++
			continue
		}
		src := strings.TrimPrefix(a, "+")
		if src == ":" {
			refuse("the matching refspec ':' pushes refs the gate cannot check one by one; push branches by name")
			continue
		}
		src, _, _ = strings.Cut(src, ":")
		if src == "" {
			deleteRefspec = true // ":dst"
			continue
		}
		if strings.Contains(src, "*") {
			refuse("a glob refspec pushes refs the gate cannot check one by one; push branches by name")
			continue
		}
		p.Sources = append(p.Sources, src)
	}
	if noVerify {
		refuse("--no-verify skips the pre-push hook")
	}
	for _, name := range []string{"--all", "--branches", "--mirror", "--tags"} {
		if every[name] {
			refuse(name + " pushes refs the gate cannot check one by one; push branches by name")
		}
	}
	p.Deletes = deleteFlag || deleteRefspec
	switch {
	case deleteFlag:
		p.Sources = nil
	case len(p.Sources) == 0 && !p.Deletes:
		p.Sources = []string{"HEAD"}
	}
	return p, true
}
