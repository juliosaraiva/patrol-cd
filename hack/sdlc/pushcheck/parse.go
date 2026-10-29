package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

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

// unknownRun is code the walker could not read that may push, and the directory it runs in ("" when
// that is unknown): a command word it does not know (no program on PATH, builtin or function the
// command defines), which may be an alias or function of the shell's setup (oh-my-zsh's gp is `git
// push`), or a shell, source or eval whose script it did not read.
type unknownRun struct {
	Dir  string
	Word string
	What string // what runs, when it is not an unknown word (a shell reading a script the hook did not read)
}

// analysis is what findPushes learned about a command.
type analysis struct {
	Pushes  []push
	Unknown []unknownRun
	Calls   []string // git subcommands other than push; one that is not a git command is an alias
	// Blocked says why the whole command is refused, wherever it runs: a git push the walker cannot
	// follow, one that skips git's pre-push hook, or something that can point git at another
	// repository, change its configuration, hooks or aliases.
	Blocked string
}

const runAlone = "run `git push` as a command of its own, with literal arguments"

// normalize removes the quotes and backslashes bash removes, for the textual tests.
func normalize(s string) string {
	return strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(s)
}

// hookEvasion matches what skips git's hook. GIT_ is case-sensitive so a lowercase word such as the
// fixture name git_push.json is not mistaken for a GIT_* variable.
var hookEvasion = regexp.MustCompile(`(?i:no-verify|hookspath)|GIT_[A-Z]`)

// mentionsGitPush reports whether text names a `git push` the walker could not resolve: a word that
// is git, or a path to it, followed by git's global options (with the values of those that take
// one) and then push; or git-push; or push with something that would skip git's hook. Words are
// split at whitespace and shell separators after the quotes and backslashes bash removes, and a
// NAME=git or zsh =git word counts as git. A backslash is read both ways (backslashRead): bash removes
// it (`git p\ush`), and printf or zsh's echo turn `\n` into a newline (`true\ngit push`). It is the
// last, conservative net for a command the walker could not parse or follow; the normal path parses
// the command.
func mentionsGitPush(s string) bool {
	return backslashRead(s, scanGitPush)
}

// backslashRead runs a textual test on s with its backslashes removed, as bash removes them, and with
// each escape sequence read as a word break, as printf and zsh's echo turn `\n`, `\012` or `\x0a`
// into a newline.
func backslashRead(s string, test func(string) bool) bool {
	return test(normalize(s)) || test(normalize(escapeSequence.ReplaceAllString(s, " ")))
}

// escapeSequence matches what printf and echo -e (zsh's echo by default) expand after a backslash.
var escapeSequence = regexp.MustCompile(`\\(?:[0-7]{1,4}|x[0-9A-Fa-f]{1,2}|u[0-9A-Fa-f]{1,4}|U[0-9A-Fa-f]{1,8}|.)`)

// scanGitPush is mentionsGitPush on normalized text.
func scanGitPush(n string) bool {
	words := strings.FieldsFunc(n, func(r rune) bool { // `:` too, for zsh's e:CODE: qualifier
		return unicode.IsSpace(r) || strings.ContainsRune(";&|()<>{}`:", r)
	})
	for i, word := range words {
		if !strings.HasPrefix(word, "-") {
			if _, v, ok := strings.Cut(word, "="); ok {
				word = v // NAME=git, =git
			}
		}
		switch path.Base(word) {
		case "git-push":
			return true
		case "git":
			if gitArgsPush(words[i+1:]) {
				return true
			}
		}
	}
	return strings.Contains(n, "push") && hookEvasion.MatchString(n)
}

// gitArgsPush reports whether git's arguments, past its global options, start with push.
func gitArgsPush(args []string) bool {
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !strings.HasPrefix(a, "-") {
			return a == "push"
		}
		if name, _, hasValue := strings.Cut(a, "="); gitGlobalWithValue[name] && !hasValue {
			j++ // -C DIR, -c KEY=VALUE, --git-dir DIR …
		}
	}
	return false
}

// gitWord matches git as a word, for a command that does not parse.
var gitWord = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])git([^A-Za-z0-9_]|$)`)

// mentionsGit reports whether s names git as a word, or anything that would skip git's hook.
func mentionsGit(s string) bool {
	return backslashRead(s, func(n string) bool { return gitWord.MatchString(n) || hookEvasion.MatchString(n) })
}

// skipsHook reports whether a literal argument is --no-verify or an abbreviation git accepts for it.
func skipsHook(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	return len(name) >= len("--no-v") && strings.HasPrefix("--no-verify", name)
}

// parseBash parses a script as bash.
func parseBash(script string) (*syntax.File, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return nil, err
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		if s, ok := n.(*syntax.Stmt); ok {
			hoistFuncChain(s)
		}
		return true
	})
	return f, nil
}

// hoistFuncChain undoes a reading of mvdan.cc/sh: `f() { …; } && cmd` (or ||, |, |&) parses as a
// function whose body is the whole chain, where the shell defines f with the first element as its
// body and runs the rest of the chain after the definition, in this shell. (The parser puts `!`, `&`
// and redirections on the outer statement or on the chain's elements, never on the body itself.)
func hoistFuncChain(s *syntax.Stmt) {
	fd, ok := s.Cmd.(*syntax.FuncDecl)
	if !ok || fd.Body == nil {
		return
	}
	chain, ok := fd.Body.Cmd.(*syntax.BinaryCmd)
	if !ok {
		return
	}
	def := *fd
	def.Body = chain.X
	// parseBash's walk reaches first next and splits it too (f() { …; } | cmd && cmd).
	first := &syntax.Stmt{Position: s.Position, Cmd: &def}
	s.Cmd = &syntax.BinaryCmd{OpPos: chain.OpPos, Op: chain.Op, X: first, Y: chain.Y}
}

// findPushes parses command as bash and returns what it does with git, starting in cwd.
func findPushes(command, cwd string) (analysis, error) {
	f, err := parseBash(command)
	if err != nil {
		return analysis{}, err
	}
	w := newWalker(cwd, command)
	w.run(f)
	return analysis{Pushes: w.pushes, Unknown: w.unknown, Calls: w.calls, Blocked: w.blocked}, nil
}

// run walks a parsed program on this walker, statement by statement; each statement's words (the
// command and process substitutions in them, a git push there being refused, and the assignments
// made inside words and arithmetic, ${NAME:=value}, $((NAME=1))) go with it.
func (w *walker) run(f *syntax.File) {
	// A function counts as defined only where its definition certainly runs in this shell: a
	// statement of its own at the top (not in a subshell, a branch, a chain or pipeline, the
	// background or eval).
	if w.inEval == 0 {
		for _, s := range f.Stmts {
			if fd, ok := s.Cmd.(*syntax.FuncDecl); ok && !s.Background && !s.Coprocess {
				w.topFuncs[fd] = true
			}
		}
	}
	w.stmts(f.Stmts)
}

// probe walks code that runs in a child of this shell (a command or process substitution), from the
// current directory, on a walker of its own: a push there is refused, as is what the walker refuses
// anywhere, and code it could not read is recorded where it runs.
func (w *walker) probe(walk func(*walker)) {
	p := w.child(w.dir, w.src)
	walk(p)
	if len(p.pushes) > 0 {
		w.block("a git push inside a command or process substitution cannot be checked; " + runAlone)
	}
	w.block(p.blocked)
	w.calls = append(w.calls, p.calls...)
	w.unread(p.unknown...)
}

// words walks what a statement's own words do as the shell expands them, before the statement runs,
// from its directory: the command and process substitutions with their pipelines, redirections and
// heredocs (probe), and the assignments in parameter expansions and arithmetic (${NAME:=value},
// $((NAME=1))). The statements inside it are left to stmt, which walks them where they run.
func (w *walker) words(s *syntax.Stmt) {
	syntax.Walk(s, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Stmt:
			return n == s
		case *syntax.CmdSubst:
			if n.TempFile || n.ReplyVar {
				w.block("`${ …; }` and `${| …; }` run their commands in this shell (bash 5.3, mksh), which the hook does not follow; " + runAlone)
				return false
			}
			w.probe(func(p *walker) { p.stmts(n.Stmts) })
			return false
		case *syntax.ProcSubst:
			w.probe(func(p *walker) { p.stmts(n.Stmts) })
			return false
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) && n.Param != nil {
				if n.Exp.Word == nil {
					w.assignValue(n.Param.Value, "", true) // ${NAME:=}
				} else {
					w.assign(n.Param.Value, n.Exp.Word)
				}
			}
		case *syntax.BinaryArithm:
			if arithAssign[n.Op] {
				w.assign(arithName(n.X), nil)
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				w.assign(arithName(n.X), nil)
			}
		}
		return true
	})
}

var arithAssign = map[syntax.BinAritOperator]bool{syntax.Assgn: true, syntax.AddAssgn: true, syntax.SubAssgn: true,
	syntax.MulAssgn: true, syntax.QuoAssgn: true, syntax.RemAssgn: true, syntax.AndAssgn: true, syntax.OrAssgn: true,
	syntax.XorAssgn: true, syntax.ShlAssgn: true, syntax.ShrAssgn: true, syntax.AndBoolAssgn: true,
	syntax.OrBoolAssgn: true, syntax.XorBoolAssgn: true, syntax.PowAssgn: true}

// arithName returns the variable an arithmetic assignment sets, or "".
func arithName(x syntax.ArithmExpr) string {
	if word, ok := x.(*syntax.Word); ok {
		return word.Lit()
	}
	return ""
}

type walker struct {
	dir     string // current directory; "" once it is unknown
	src     string // the whole command
	cmdText string // the text of the command currently being dispatched (its own words, not siblings)
	pushes  []push
	unknown []unknownRun
	calls   []string
	blocked string
	// envUnknown says why git's environment and the directory may have changed in ways the walker
	// cannot see (a sourced file, an eval of a dynamic string); a later push is refused.
	envUnknown string
	cdpath     bool                            // CDPATH was set in the command: a relative cd may go elsewhere
	homeSet    bool                            // HOME was set in the command: a word starting with ~ is not literal
	funcs      map[string]*syntax.Stmt         // functions the command defines, with their bodies
	maybeFuncs map[string][]*syntax.Stmt       // the bodies of definitions that may not run
	cdMemo     map[string]map[string]cdVerdict // what a function's own body does to the directory, by directory (directCD)
	reachMemo  map[cdKey]bool                  // whether a call of a function may cd, through the ones it calls
	gen        map[string]int                  // how many times each name has been defined as a function so far
	gens       int                             // how many definitions there have been so far
	refs       map[string]map[string]int       // the command words each function's body runs, with their gen then
	hooks      map[string]bool                 // the functions defined that the shell calls on its own (hookFunction)
	traps      []trapAction                    // the trap actions set so far, which run the functions defined then
	// dirLost: a trap action or a function the shell calls on its own may cd before or after any
	// command from here on (DEBUG, ERR, chpwd), so no later directory is known, even after a cd.
	dirLost    bool
	shifts     map[string]bool           // functions whose body shifts its arguments or slices them (${@:2})
	passes     map[string][]string       // the command words each function passes its arguments to (run "$@")
	paths      map[string]bool           // whether each name is a program on PATH (onPath), shared with children
	cdFuncs    map[string]bool           // functions defined where the definition may not run, whose body may cd
	topFuncs   map[*syntax.FuncDecl]bool // definitions that certainly run in this shell
	inEval     int                       // inside eval, whose definitions may sit in a branch
	program    bool                      // the command runs as a program (a wrapper's), which no function shadows
	input      inputArgs                 // xargs or parallel adds its input to the command's arguments
	body       []string                  // the names of the function whose body is being walked, at its definition
	funcUnread map[string][]unknownRun   // the code each function's body runs that the walker could not read
	defined    map[string]bool           // functions whose definition certainly ran in this shell (or its parent)
}

// cdKey is what a verdict on a function depends on: its name, and the directory its body's AUTO_CD
// words resolve against.
type cdKey struct{ name, dir string }

// cdVerdict is what a function's own body does to the directory: whether it may cd, and the
// functions it calls, whose bodies may.
type cdVerdict struct {
	cd    bool
	calls []string
}

// trapAction is a trap's action, kept to be read again when a function it runs is defined again.
type trapAction struct {
	code *syntax.File
	refs map[string]int
}

// anyName stands in walker.refs for every name: a body that runs code given as text (eval, source)
// or a command word the walker cannot read may run any function.
const anyName = ""

// bodyDir is the directory a function body is walked from: it stands for the directory of each call,
// where callRun puts it.
const bodyDir = "/\x00function-body"

// callRun places code a function body runs, recorded relative to bodyDir, at a call of the function
// from the current directory. A directory the body set itself (an absolute cd, an unknown one) stays,
// unless CDPATH or HOME is set by the call, which may take the body's cd elsewhere.
func (w *walker) callRun(u unknownRun) unknownRun {
	switch {
	case u.Dir == bodyDir:
		u.Dir = w.dir
	case u.Dir == "":
	case w.cdpath || w.homeSet || os.Getenv("CDPATH") != "":
		u.Dir = ""
	case strings.HasPrefix(u.Dir, bodyDir+"/"):
		u.Dir = w.resolve(strings.TrimPrefix(u.Dir, bodyDir+"/"))
	}
	return u
}

// inputArgs is how xargs or parallel gives its input to the command it runs.
type inputArgs int

const (
	_             inputArgs = iota // no input
	inputAppended                  // after the command's own arguments
	inputReplaced                  // in place of a replace string (xargs -I, parallel's {}), anywhere
)

func newWalker(dir, src string) *walker {
	return &walker{dir: dir, src: src, funcs: map[string]*syntax.Stmt{}, maybeFuncs: map[string][]*syntax.Stmt{},
		cdMemo: map[string]map[string]cdVerdict{}, reachMemo: map[cdKey]bool{}, gen: map[string]int{}, hooks: map[string]bool{},
		refs: map[string]map[string]int{}, paths: map[string]bool{}, shifts: map[string]bool{}, passes: map[string][]string{}, cdFuncs: map[string]bool{},
		topFuncs: map[*syntax.FuncDecl]bool{}, funcUnread: map[string][]unknownRun{}, defined: map[string]bool{}}
}

// child is a walker for code that runs in a child of this shell (a substitution, a shell the command
// runs, a trap action), from dir: it inherits CDPATH, HOME and whatever a sourced file or an eval set
// before it. A function this shell defines may reach the child (a subshell, bash's export -f) or not,
// so one whose body may cd may run there and cd; one whose body cannot runs as a function that does
// not cd, or as the program of its name (a child shell has no alias or function of the user's setup).
func (w *walker) child(dir, src string) *walker {
	c := newWalker(dir, src)
	c.cdpath, c.envUnknown, c.homeSet, c.paths = w.cdpath, w.envUnknown, w.homeSet, w.paths
	for name := range w.cdFuncs {
		c.cdFuncs[name] = true
	}
	for name, runs := range w.funcUnread {
		c.funcUnread[name] = runs
	}
	for name := range w.defined {
		c.defined[name] = true
	}
	c.gen, c.gens, c.hooks, c.shifts, c.passes = maps.Clone(w.gen), w.gens, maps.Clone(w.hooks), maps.Clone(w.shifts), maps.Clone(w.passes)
	for name, refs := range w.refs {
		c.refs[name] = maps.Clone(refs)
	}
	for name, bodies := range w.maybeFuncs {
		c.maybeFuncs[name] = slices.Clone(bodies)
	}
	// In a fixed order, as each verdict found settles those of the functions searched on the way: a
	// run must not depend on Go's map order.
	for _, name := range slices.Sorted(maps.Keys(w.funcs)) {
		if w.unknownWord(name) {
			c.cdFuncs[name] = true
		} else {
			c.funcs[name] = w.funcs[name]
		}
	}
	return c
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
			if r.Hdoc != nil { // nil for an empty heredoc
				_ = p.Print(&b, r.Hdoc)
			}
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

// unread records code the walker could not read, which may push. In a function body, walked where
// the function is defined, it is kept with the function instead: each call of it records it, placed
// at the call (callRun).
func (w *walker) unread(runs ...unknownRun) {
	if len(w.body) > 0 {
		for _, name := range w.body {
			w.funcUnread[name] = w.appendNew(w.funcUnread[name], runs...)
		}
		return
	}
	if w.dirLost {
		for i := range runs {
			runs[i].Dir = ""
		}
	}
	w.unknown = w.appendNew(w.unknown, runs...)
}

// loseDir notes that a trap action or a hook function may move the directory between any two
// commands from here on: a push after it is refused (why), and anything else runs from an unknown
// directory.
func (w *walker) loseDir(why string) {
	w.envUnknown, w.dir, w.dirLost = why, "", true
}

// maxTraps bounds the trap actions the walker keeps to read again after each definition.
const maxTraps = 64

// isUnsigned reports whether s is an unsigned decimal integer (a signal number).
func isUnsigned(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// maxRuns bounds the places a function (or the command) runs code the walker did not read: each body
// holds those of the functions it calls, so a long chain of functions would otherwise hold a number
// growing with the square of its length, and take time growing with the cube, past the hook's
// timeout, which lets the command run.
const maxRuns = 256

// appendNew appends the runs list does not hold yet: a function called from several places, or from
// other functions, records each place once. Past maxRuns places the command is refused.
func (w *walker) appendNew(list []unknownRun, runs ...unknownRun) []unknownRun {
	for _, u := range runs {
		if slices.Contains(list, u) {
			continue
		}
		if len(list) >= maxRuns {
			w.block(fmt.Sprintf("the command runs code the hook did not read in more than %d places, more than it follows; run it in parts", maxRuns))
			return list
		}
		list = append(list, u)
	}
	return list
}

// blockText blocks the command when text names a git push the walker could not resolve.
func (w *walker) blockText(text, how string) {
	if mentionsGitPush(text) {
		w.block("a git push through " + how + " cannot be checked; " + runAlone)
	}
}

// stmts walks a statement list. A cd that may fail without stopping the list (not guarded by
// `|| exit`) leaves the directory unknown for the statements after it, which run in either
// directory. The last statement's cd is left to the caller: its status is the list's.
func (w *walker) stmts(list []*syntax.Stmt) {
	for i, s := range list {
		before := w.dir
		w.stmt(s)
		if i < len(list)-1 && w.dir != before && !exitGuarded(s) {
			w.dir = ""
		}
	}
}

// condition walks an if or while condition. A branch after it can run whether a cd in it worked or
// failed (an else, a negated condition, an until body), so such a cd leaves the directory unknown.
func (w *walker) condition(list []*syntax.Stmt) {
	before := w.dir
	w.stmts(list)
	if w.dir != before {
		w.dir = ""
	}
}

// exitGuarded reports whether s is a chain ending in `|| exit` with no other ||: whatever step of
// it fails, the script exits, so reaching the next statement means every cd in it worked.
func exitGuarded(s *syntax.Stmt) bool {
	c, ok := s.Cmd.(*syntax.BinaryCmd)
	if !ok || s.Negated || (c.Op != syntax.AndStmt && c.Op != syntax.OrStmt) {
		return false
	}
	segs := flattenChain(c)
	last := segs[len(segs)-1]
	if last.op != syntax.OrStmt || !isExit(last.stmt) {
		return false
	}
	for _, seg := range segs[1 : len(segs)-1] {
		if seg.op == syntax.OrStmt || seg.stmt.Negated {
			return false
		}
	}
	return !segs[0].stmt.Negated
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
	if !pipesToShell(c) {
		return "", false
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
			// zsh's echo expands escapes (`\n`) by default, bash's does not: such text is not read.
			if !ok || strings.HasPrefix(v, "-") || strings.Contains(v, `\`) {
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

// pipesToShell reports whether a pipeline's last element is a shell that reads its script from the
// pipe.
func pipesToShell(c *syntax.BinaryCmd) bool {
	if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
		return false
	}
	right, ok := c.Y.Cmd.(*syntax.CallExpr)
	if !ok || len(right.Args) == 0 {
		return false
	}
	// The shell may come behind a command that runs it (env, command, nice, sudo, timeout 60, a
	// program the walker does not model): it still reads the pipe. One that reads its input as data
	// (grep sh), or hands it to the command as arguments (xargs), runs no shell on it.
	args, lit := fields(right.Args)
	if len(args) > 0 && lit[0] && (dataCommands[path.Base(args[0])] || execRunners[path.Base(args[0])] ||
		path.Base(args[0]) == "xargs" || path.Base(args[0]) == "parallel") {
		return false
	}
	for i := range args {
		if lit[i] && shells[path.Base(args[i])] {
			return readsStdin(args[i+1:], lit[i+1:])
		}
	}
	return false
}

// readsStdin reports whether a shell given these arguments reads its script from standard input:
// with -s, or with no -c and no script operand (/dev/stdin counts as none), whatever its other
// options.
func readsStdin(args []string, lit []bool) bool {
	stdin := false
	operand := func(a string) bool { return stdin || a == "/dev/stdin" || a == "/dev/fd/0" }
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !lit[i]:
			return false
		case a == "--" || a == "-":
			return i+1 >= len(args) || (lit[i+1] && operand(args[i+1]))
		case shellWithValue[a]:
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for _, ch := range a[1:] {
				switch ch {
				case 'c':
					return false
				case 's':
					stdin = true
				case 'o', 'O':
					i++ // each takes the next argument as its value
				}
			}
		default:
			return operand(a)
		}
	}
	return true
}

func bodyOf(r *syntax.Redirect) *syntax.Word {
	if r.Op == syntax.WordHdoc {
		return r.Word
	}
	return r.Hdoc
}

// isExit reports whether a statement ends the script: `exit`, or a `{ …; exit; }` group. (return
// outside a function, and a helper such as die that may be undefined, let the script go on.)
func isExit(s *syntax.Stmt) bool {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Args) == 0 {
			return false
		}
		name, ok := literalWord(c.Args[0])
		return ok && name == "exit"
	case *syntax.Block:
		return len(c.Stmts) > 0 && isExit(c.Stmts[len(c.Stmts)-1])
	}
	return false
}

func (w *walker) stmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	w.words(s)
	code := qualifierCode(s)
	if code != "" {
		w.blockText(code, "a zsh glob qualifier that runs code")
	}
	if s.Background || s.Coprocess {
		saved := w.dir // it runs in a subshell: a cd inside does not reach the commands after it
		w.cmd(s)
		w.dir = saved
		return
	}
	if code != "" {
		w.dir = "" // zsh runs the qualifier's code in this shell as it globs, before the command
	}
	w.cmd(s)
}

// The directory is tracked only through what certainly runs. A compound command or function that
// may change it leaves it unknown, inside and after, so a push there is refused.
func (w *walker) cmd(s *syntax.Stmt) {
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		w.call(c, s.Redirs)
	case *syntax.DeclClause:
		w.decl(c)
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.AndStmt, syntax.OrStmt:
			// An && / || chain. The directory is carried forward only through a pure && prefix whose
			// segments certainly run: `cd ui && test && cd .. && git push` resolves the push's
			// directory. The first || whose successor is not exit, a cd that is not in that prefix,
			// or a negated segment that changes the directory, all mean a cd might be skipped or
			// rolled back, so the directory is unknown from there on and after the chain. `cd /x ||
			// exit; …` keeps the cd, because reaching past the exit means it succeeded. A chain
			// with no cd at all (`git fetch || true`) leaves the directory as it is.
			segments := flattenChain(c)
			if !w.changesDir(c) {
				for _, seg := range segments {
					w.stmt(seg.stmt)
				}
				return
			}
			unknownAfter, guarded := false, exitGuarded(s)
			poisoned := false // once set, the directory is unknown for the rest of the chain
			for i, seg := range segments {
				if i > 0 && seg.op == syntax.OrStmt && !isExit(seg.stmt) {
					poisoned = true
				}
				if poisoned {
					w.dir = ""
				}
				if i > 0 && seg.op == syntax.OrStmt && isExit(seg.stmt) {
					// The exit runs only on failure, from wherever the chain got to: its commands are
					// walked from an unknown directory, and reaching past it keeps the predecessor's cd.
					before := w.dir
					w.dir = ""
					w.stmt(seg.stmt)
					w.dir = before
					continue
				}
				before := w.dir
				w.stmt(seg.stmt)
				changed := w.dir != before
				if changed && i > 0 && !guarded {
					// A cd that might be skipped: unknown for what follows the chain. (A first segment's cd
					// is skipped only past an || that is not exit, which poisons the chain; in a chain that
					// ends in `|| exit`, reaching what follows means every cd in it worked.)
					unknownAfter = true
				}
				if changed && seg.stmt.Negated {
					// A negated cd inverts success, so its directory is uncertain: the next segment, or
					// the end of the chain, clears it.
					poisoned = true
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
			} else if pipesToShell(c) {
				// Another producer (printf, echo -e, a chain through tee): refuse a push in its text.
				var b bytes.Buffer
				_ = syntax.NewPrinter().Print(&b, c.X)
				w.blockText(b.String(), "a script piped to a shell")
			} else if runner, ok := pipesToContainer(c); ok {
				// A script piped to a shell in a container: a push in it is the container's.
				var b bytes.Buffer
				_ = syntax.NewPrinter().Print(&b, c.X)
				if mentionsGitPush(b.String()) {
					w.block(containerPush(runner))
				}
			}
			saved := w.dir
			w.stmt(c.X)
			w.dir = saved
			w.stmt(c.Y)
			w.dir = saved
			if w.changesDir(c.Y) {
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
				w.assign(it.Name.Value, nil)
			}
		}
		if w.changesDir(c) {
			w.dir = ""
			defer func() { w.dir = "" }()
		}
		switch n := c.(type) {
		case *syntax.IfClause:
			for e := n; e != nil; e = e.Else {
				w.condition(e.Cond)
				w.stmts(e.Then)
			}
		case *syntax.CaseClause:
			for _, it := range n.Items {
				w.stmts(it.Stmts)
			}
		case *syntax.WhileClause:
			w.condition(n.Cond)
			w.stmts(n.Do)
		case *syntax.ForClause:
			w.stmts(n.Do)
		}
	case *syntax.TimeClause:
		w.stmt(c.Stmt)
	case *syntax.CoprocClause: // runs in a subshell
		saved := w.dir
		w.stmt(c.Stmt)
		w.dir = saved
	case *syntax.FuncDecl:
		names := c.Names   // zsh's `f g () { … }`
		if c.Name != nil { // nil for zsh's anonymous `() CMD`
			names = append([]*syntax.Lit{c.Name}, names...)
		}
		if len(names) == 0 {
			w.stmt(c.Body) // zsh runs an anonymous function at once, in this shell
			return
		}
		// A named function's body runs when it is called, from wherever that is, and runs the
		// functions it calls as they are defined then (bash and zsh look a function up when it is
		// called): a push in it is refused, a call of it may cd where its body or theirs may
		// (funcMayCD), and a call of it after one of them was defined again runs as an unknown word
		// (stale). A definition that certainly runs is followed into its body; any other one that may
		// cd makes its name unknown.
		for _, n := range names {
			if modeledNames[n.Value] {
				w.block("the command defines a function named " + n.Value + ", which the hook reads as the builtin or program of that name; " + runAlone)
			}
			w.defining(n.Value)
			if w.topFuncs[c] {
				w.funcs[n.Value] = c.Body
				w.defined[n.Value] = true
				w.refs[n.Value] = map[string]int{} // it replaces the earlier definition
				delete(w.funcUnread, n.Value)
				delete(w.shifts, n.Value)
				delete(w.passes, n.Value)
			} else {
				w.maybeFuncs[n.Value] = append(w.maybeFuncs[n.Value], c.Body)
				if w.refs[n.Value] == nil {
					w.refs[n.Value] = map[string]int{}
				}
			}
			w.bodyRefs(w.refs[n.Value], c.Body)
			if shiftsArgs(c.Body) {
				w.shifts[n.Value] = true
			}
			for _, to := range passedTo(c.Body) {
				if !slices.Contains(w.passes[n.Value], to) {
					w.passes[n.Value] = append(w.passes[n.Value], to)
				}
			}
		}
		w.redefined()
		saved, body, pushes := w.dir, w.body, len(w.pushes)
		w.dir, w.body = bodyDir, nil
		for _, n := range names {
			w.body = append(w.body, n.Value)
		}
		w.stmt(c.Body)
		// A redirection on the definition applies at each call (`f() { sh; } <<'EOF' … EOF`): a heredoc
		// or here-string there is input a shell in the body runs as a script, read as part of the body.
		// The parser puts it on the body (one before the name is zsh's, which does not parse as bash).
		runsShell := runsAShell(c.Body)
		for _, r := range c.Body.Redirs {
			if r.Op != syntax.Hdoc && r.Op != syntax.DashHdoc && r.Op != syntax.WordHdoc {
				continue
			}
			body := bodyOf(r)
			if body == nil {
				continue
			}
			text, ok := heredocText(r, body)
			switch {
			case ok && runsShell:
				w.dir = bodyDir
				w.script(text)
			case ok:
				w.blockText(text, "a redirection of a function definition")
			default:
				w.blockText(wordText(body), "a redirection of a function definition")
			}
		}
		w.dir, w.body = saved, body
		for i := pushes; i < len(w.pushes); i++ {
			w.pushes[i].Dir = "" // a push in it runs from wherever the function is called
		}
		for _, n := range names {
			if hookFunction(n.Value) { // the shell calls it on its own, from any directory
				for _, u := range w.funcUnread[n.Value] {
					u.Dir = ""
					w.unread(u)
				}
			}
		}
	}
}

// defining notes a definition of the function name: the verdicts kept on functions no longer hold,
// and a function whose body runs name now runs other code than the walker read (stale).
func (w *walker) defining(name string) {
	w.gen[name]++
	w.gens++
	if hookFunction(name) {
		w.hooks[name] = true
	}
	w.invalidate(name)
}

// invalidate drops the verdicts that depend on what the name runs: its own body's, and those of the
// bodies that run it (a definition makes a word a call, a definition that may not run makes it one
// that may cd), and every reachability verdict. Others hold, so a long chain of definitions costs
// time linear in its length.
func (w *walker) invalidate(name string) {
	delete(w.cdMemo, name)
	for g, refs := range w.refs {
		if _, ok := refs[name]; ok {
			delete(w.cdMemo, g)
		}
	}
	clear(w.reachMemo)
}

// bodyRefs notes, in refs, the command words a function's body runs, with each one's definition as it
// is now, wherever in the body they run (substitutions too). A command word the walker cannot read,
// and code given as text (eval, source), may run any function.
func (w *walker) bodyRefs(refs map[string]int, body syntax.Node) {
	syntax.Walk(body, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			word, ok := commandWord(c.Args)
			switch {
			case !ok:
				w.ref(refs, anyName)
			case word == "eval" || word == "source" || word == ".":
				w.ref(refs, anyName)
			}
			if ok {
				w.ref(refs, word)
				w.ref(refs, strings.TrimLeft(word, "{=")) // as stripBraces and zsh's =cmd read it
			}
		}
		return true
	})
}

// commandWord returns a simple command's command word, after the precommands that run the rest in
// this shell (noglob, nocorrect, builtin, command, exec, zsh's repeat N), and whether it is literal.
func commandWord(args []*syntax.Word) (string, bool) {
	cw, _ := commandWordAt(args)
	if cw == nil { // assignments only
		return "", false
	}
	return literalWord(cw)
}

// commandWordAt returns a simple command's command word, after the precommands commandWord skips, and
// the words after it; nil for assignments only.
func commandWordAt(args []*syntax.Word) (*syntax.Word, []*syntax.Word) {
	for len(args) > 1 {
		word, ok := literalWord(args[0])
		switch {
		case ok && precommand[word]:
			args = args[1:]
		case ok && word == "repeat" && len(args) > 2:
			args = args[2:]
		default:
			return args[0], args[1:]
		}
	}
	if len(args) == 0 {
		return nil, nil
	}
	return args[0], nil
}

// ref notes in refs that a body runs the command word name (anyName: any), with its definition as it
// is now. A path runs a program, never a function.
func (w *walker) ref(refs map[string]int, name string) {
	if _, ok := refs[name]; ok || strings.Contains(name, "/") {
		return
	}
	if name == anyName {
		refs[name] = w.gens
	} else {
		refs[name] = w.gen[name]
	}
}

// stale reports whether a call of fn may run code the walker did not read when it walked fn's body:
// a function fn's body runs, or one theirs runs, was defined again (or first) since.
func (w *walker) stale(fn string) bool {
	return w.staleRefs(w.refs[fn], map[string]bool{fn: true})
}

// staleRefs reports whether a name in refs, or one a function among them runs (seen: the functions
// looked at already), was defined since it was noted.
func (w *walker) staleRefs(refs map[string]int, seen map[string]bool) bool {
	for ref, gen := range refs {
		if ref == anyName {
			if w.gens != gen {
				return true
			}
			continue
		}
		if w.gen[ref] != gen {
			return true
		}
		if !seen[ref] {
			seen[ref] = true
			if w.staleRefs(w.refs[ref], seen) {
				return true
			}
		}
	}
	return false
}

// redefined re-reads, after a definition, what depends on which functions are defined: a name whose
// definition may not have run may cd if a body of it may now, and a function the shell calls on its
// own (hookFunction) that may now cd can move the directory between any two commands, and one that
// may run code the walker did not read (stale) runs it from anywhere.
func (w *walker) redefined() {
	for changed := true; changed; {
		changed = false
		for _, name := range slices.Sorted(maps.Keys(w.maybeFuncs)) {
			if w.cdFuncs[name] {
				continue
			}
			for _, b := range w.maybeFuncs[name] {
				// One that may cd, or that runs its arguments (log cd DIR), whichever name it has.
				if w.changesDir(b) || w.runsArgs(name) {
					w.cdFuncs[name] = true
					w.invalidate(name)
					changed = true
					break
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(w.hooks)) {
		if _, ok := w.funcs[name]; w.cdFuncs[name] || ok && w.funcMayCD(name) {
			// The shell calls it on its own (before each command, on each cd, on a signal), so it may
			// move the directory between any two commands from here on.
			w.loseDir("the function " + name + ", which the shell runs on its own and which may cd,")
		}
		if w.stale(name) {
			w.unread(unknownRun{Dir: "", Word: name, What: staleWhat(name)})
		}
	}
	// A trap action likewise runs the functions as they are defined when it runs.
	for _, t := range w.traps {
		if w.changesDir(t.code) {
			w.loseDir("a trap action that may cd, set earlier in the command,")
		}
		if w.staleRefs(t.refs, map[string]bool{}) {
			w.unread(unknownRun{Dir: "", Word: "trap", What: "a trap action that runs a function defined, or defined again, after the trap was set (it looks the function up when it runs), code the hook did not read"})
		}
	}
}

// hookFunction reports whether the shell calls a function of this name on its own: zsh's hook
// functions and TRAPNAL signal and DEBUG/ZERR/EXIT traps, and the handlers of a command not found.
func hookFunction(name string) bool {
	switch name {
	case "chpwd", "precmd", "preexec", "periodic", "zshexit", "zshaddhistory", "command_not_found_handler",
		"command_not_found_handle":
		return true
	}
	return strings.HasPrefix(name, "TRAP") && len(name) > len("TRAP")
}

// decl handles declare, typeset, local, export and readonly. A GIT_* variable it sets, or marks for
// export, is refused; -p/-f/-F only print.
func (w *walker) decl(c *syntax.DeclClause) {
	variant := c.Variant.Value
	exportMode := variant == "export" || variant == "readonly"
	printMode, nameref := false, false
	for _, a := range c.Args {
		if a.Name == nil { // an option (-p, -x, -g, ...), or a quoted operand ("NAME=VALUE", 'NAME')
			v, ok := literalWord(a.Value)
			switch {
			case ok && strings.Contains(v, "=") && !strings.HasPrefix(v, "-"):
				name, value, _ := strings.Cut(v, "=")
				w.assignValue(name, value, true)
				if nameref {
					w.assignValue(value, "", true)
				}
				continue
			case ok && !strings.HasPrefix(v, "-"):
				if exportMode && !printMode {
					w.assignValue(v, "", true)
				}
				continue
			case !ok:
				if name, _, found := strings.Cut(literalPrefix(a.Value), "="); found {
					w.assignValue(name, "", false)
				} else if strings.Contains(wordsText([]*syntax.Word{a.Value}, nil), "GIT_") {
					w.block("a variable set from a word the hook cannot read may be a GIT_* variable; run the command without it")
				}
				continue
			}
			if strings.HasPrefix(v, "-") {
				// -p/-f/-F only print, but only for declare/typeset/local; bash's `export -p NAME=v`
				// and `readonly -p NAME=v` still set the named variable.
				if !exportMode && strings.ContainsAny(strings.TrimPrefix(v, "-"), "pfF") {
					printMode = true
				}
				if strings.Contains(v, "x") {
					exportMode = true
				}
				if !exportMode && strings.Contains(v, "n") { // declare -n REF=NAME: REF sets NAME
					nameref = true
				}
			}
			continue
		}
		if nameref && a.Value != nil {
			if target, ok := literalWord(a.Value); ok {
				w.assign(target, nil)
			}
		}
		switch {
		case zshCode[a.Name.Value] && (a.Value != nil || a.Array != nil || !a.Naked):
			w.zshCodeAssign(a) // typeset functions[cd]=…
		case a.Value != nil || !a.Naked: // declare GIT_DIR=x, or declare GIT_DIR= (empty): a set
			w.assignExpr(a)
		case printMode: // declare -p GIT_DIR: a listing, sets nothing
		case exportMode: // export GIT_DIR: an existing value takes effect for child git
			w.assignValue(a.Name.Value, "", true)
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

// assign checks a variable assignment. value is nil for a naked `export NAME`, an empty `NAME=`, or
// when the value is not at hand.
// assign checks NAME=VALUE; a nil value is one the hook does not have (read, printf -v, a for loop,
// arithmetic), which is not literal.
func (w *walker) assign(name string, value *syntax.Word) {
	text, literal := "", false
	if value != nil {
		text, literal = literalWord(value)
	}
	w.assignValue(name, text, literal)
}

// assignExpr checks an assignment as written: NAME= is empty, NAME=(…) an array the hook does not
// read as one value.
func (w *walker) assignExpr(a *syntax.Assign) {
	if a.Value == nil && a.Array == nil {
		w.assignValue(a.Name.Value, "", true)
		return
	}
	w.assign(a.Name.Value, a.Value)
}

// assignValue checks NAME=VALUE. A GIT_* variable outside the safe list is refused (PATROL_SDLC_BASE
// is not: the gate and the review input ignore it); GIT_PAGER, GIT_EDITOR and GIT_SEQUENCE_EDITOR
// pass with a literal value that is safe shell code (safeCommand), since git runs it through sh, and
// so do EDITOR, VISUAL and PAGER, which git falls back to. CDPATH set in the command makes a later
// relative cd's directory unknown.
func (w *walker) assignValue(name, value string, literal bool) {
	if param, _, _ := strings.Cut(name, "["); zshCode[param] {
		// Reached only by an assignment the hook does not read as written (read -A, printf -v, set -A,
		// a for loop, arithmetic): code or a key it cannot check.
		w.block("zsh's " + param + " parameter set by a command the hook does not read defines code it cannot check; " + runAlone)
		return
	}
	if name == "CDPATH" || name == "cdpath" {
		w.cdpath = true
	}
	if name == "HOME" {
		w.homeSet = true
	}
	if name == "EDITOR" || name == "VISUAL" || name == "PAGER" {
		if !literal || !safeCommand(value) {
			w.block(name + " is shell code git runs when its own editor or pager setting is unset, and this value could run git; run the command without it")
		}
		return
	}
	if !strings.HasPrefix(name, "GIT_") || safeGitEnv[name] || strings.HasPrefix(name, "GIT_TRACE") {
		return
	}
	if gitEnvCommands[name] && literal && safeCommand(value) {
		return
	}
	w.block(name + " can point git at another repository or change its configuration, hooks or the commands it runs, also for a push in a script; run the command without it")
}

// safeCommand reports whether shell code git runs (an editor, a pager) may be set in a command:
// empty, or code that names neither git nor a hook override and in which the walker finds no push.
func safeCommand(code string) bool {
	if code == "" {
		return true
	}
	if mentionsGit(code) {
		return false
	}
	a, err := findPushes(code, "")
	return err == nil && len(a.Pushes) == 0 && a.Blocked == ""
}

// assignWord checks an assignment written as one NAME=VALUE word (an env prefix).
func (w *walker) assignWord(assign string) {
	name, value, _ := strings.Cut(assign, "=")
	w.assignValue(name, value, true)
}

// changesDir reports whether node runs a command that can change this shell's directory: cd and
// its kin, or a directory name that zsh's AUTO_CD runs as a cd, where they run in this shell. A
// subshell, command or process substitution, background job or pipeline element before the last
// cannot. A command word that is not literal counts as no cd, as at top level (a cd assembled at
// run time is out of the hook's reach).
func (w *walker) changesDir(node syntax.Node) bool {
	return w.changesDirCalls(node, nil)
}

// changesDirCalls is changesDir, but with calls not nil a call of a function the command defines is
// added to calls instead of followed into its body.
func (w *walker) changesDirCalls(node syntax.Node, calls *[]string) bool {
	found := false
	syntax.Walk(node, func(n syntax.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		case *syntax.FuncDecl: // defining a function moves nothing; a call of it counts (unknownWord)
			return n.Name == nil && len(n.Names) == 0 // zsh runs an anonymous one at once
		case *syntax.Stmt:
			if n.Background || n.Coprocess {
				return false
			}
			found = qualifierCode(n) != ""
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				found = w.changesDirCalls(n.Y, calls) // zsh runs the last element in this shell
				return false
			}
		case *syntax.CallExpr:
			args, lit := fields(n.Args)
			found = w.argvMayCD(args, lit, calls)
		}
		return !found
	})
	return found
}

// argvMayCD reports whether a command of this argv, run in this shell, may change its directory: cd
// and its kin, code that may (source, eval, a trap), a directory name AUTO_CD enters, or a word the
// walker does not know. With calls not nil a function the command defines is added to calls instead
// of followed into its body.
//
// A function that runs code given at run time ("$@", $1, eval) runs its arguments as a command in this
// shell (`r cd DIR`, `r r cd DIR`), or, if its body shifts them, any literal tail of them (`r : cd DIR`).
func (w *walker) argvMayCD(args []string, lit []bool, calls *[]string) bool {
	args, lit = stripBraces(args, lit)
	memo, anyMemo := map[int]bool{}, map[int]bool{}
	var from func(i int) bool // whether args[i:], as a command, may cd
	// anyFrom: whether some literal tail args[j:], j >= i, may: each is asked once, so a shifting
	// runner given many words costs time linear in them.
	var anyFrom func(i int) bool
	anyFrom = func(i int) bool {
		if i >= len(args) {
			return false
		}
		v, ok := anyMemo[i]
		if !ok {
			v = lit[i] && from(i) || anyFrom(i+1)
			anyMemo[i] = v
		}
		return v
	}
	from = func(i int) bool {
		if v, ok := memo[i]; ok {
			return v
		}
		found := w.autoCD(args[i:], lit[i:])
		for len(args)-i > 1 && lit[i] && (precommand[path.Base(args[i])]) {
			i++
		}
		if i < len(args) && lit[i] {
			switch name := path.Base(strings.TrimPrefix(args[i], "=")); name {
			case "cd", "chdir", "pushd", "popd", "source", ".", "eval", "trap", "emulate", "repeat":
				found = true
			default:
				_, isFunc := w.funcs[name]
				isFunc = isFunc && !w.cdFuncs[name] && !strings.Contains(args[i], "/")
				if isFunc && w.runsArgs(name) {
					if w.shiftsOf(name) {
						found = anyFrom(i + 1)
					} else {
						found = i+1 < len(args) && lit[i+1] && from(i+1)
					}
				}
				if isFunc && calls != nil {
					*calls = append(*calls, name)
				} else {
					found = found || w.unknownWord(args[i])
				}
			}
		}
		memo[i] = found
		return found
	}
	return from(0)
}

// runsAShell reports whether a body runs a shell, which may read its input as a script.
func runsAShell(body syntax.Node) bool {
	found := false
	syntax.Walk(body, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok {
			args, lit := fields(c.Args)
			for i := range args {
				found = found || lit[i] && shells[path.Base(args[i])]
			}
		}
		return !found
	})
	return found
}

// fromFirstArg are the words that run, or pass on, a function's arguments from the first ("$@", $1);
// any other expansion of them ("$2", "${@:2}", "${a[@]:1}", zsh's ${argv[2,-1]}) may start later.
var fromFirstArg = map[string]bool{`"$@"`: true, `$@`: true, `"${@}"`: true, `${@}`: true, `"$*"`: true,
	`$*`: true, `"${*}"`: true, `${*}`: true, `$1`: true, `"$1"`: true, `${1}`: true, `"${1}"`: true}

// wordText prints a word as written.
func wordText(word *syntax.Word) string {
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, word)
	return b.String()
}

// usesArgs reports whether a word expands the function's arguments ($@, $*, $N, zsh's $argv).
func usesArgs(word *syntax.Word) bool {
	found := false
	syntax.Walk(word, func(n syntax.Node) bool {
		if p, ok := n.(*syntax.ParamExp); ok && p.Param != nil {
			v := p.Param.Value
			found = found || v == "@" || v == "*" || v == "argv" || isUnsigned(v) && v != "0"
		}
		return !found
	})
	return found
}

// shiftsArgs reports whether a body may run its arguments from one after the first: it shifts them,
// takes a slice of them (${@:2}), runs a command word other than "$@" or $1 that the walker cannot
// read ("$2", "${a[@]:1}"), or passes such an expansion of them to another command. Then the code
// it runs from them may start at any of them.
func shiftsArgs(body syntax.Node) bool {
	found := false
	syntax.Walk(body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CallExpr:
			cw, rest := commandWordAt(n.Args)
			if cw == nil { // assignments only
				break
			}
			if word, ok := literalWord(cw); !ok {
				found = !fromFirstArg[wordText(cw)]
			} else if word == "shift" {
				found = true
			} else {
				for _, a := range rest {
					found = found || usesArgs(a) && !fromFirstArg[wordText(a)]
				}
			}
		case *syntax.ParamExp:
			if n.Slice != nil && n.Param != nil && (n.Param.Value == "@" || n.Param.Value == "*") {
				found = true
			}
		}
		return !found
	})
	return found
}

// passedTo returns the command words a body runs with its arguments among theirs (`run "$@"`): if one
// is a function that runs its arguments, so does the body.
func passedTo(body syntax.Node) []string {
	var names []string
	syntax.Walk(body, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok {
			cw, rest := commandWordAt(c.Args)
			if word, ok := literalWord(cw); cw != nil && ok && slices.ContainsFunc(rest, usesArgs) && !slices.Contains(names, word) {
				names = append(names, word)
			}
		}
		return true
	})
	return names
}

// runsArgs reports whether the function name runs code given at run time ("$@", $1, eval, source),
// which may be its arguments, itself or through a function it passes them to (as each is defined
// now).
func (w *walker) runsArgs(name string) bool {
	return w.passesArgs(name, map[string]bool{}, func(n string) bool {
		_, ok := w.refs[n][anyName]
		return ok
	})
}

// shiftsOf reports whether the function name may run its arguments from one after the first,
// itself or through a function it passes them to.
func (w *walker) shiftsOf(name string) bool {
	return w.passesArgs(name, map[string]bool{}, func(n string) bool { return w.shifts[n] })
}

// passesArgs reports whether has holds for the function name or for a function it passes its
// arguments to, followed through the functions the command defines (seen: looked at already).
func (w *walker) passesArgs(name string, seen map[string]bool, has func(string) bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	if has(name) {
		return true
	}
	for _, n := range w.passes[name] {
		if w.passesArgs(n, seen, has) {
			return true
		}
	}
	return false
}

// codeQualifier matches the zsh glob qualifiers that run code (e:…:, e{…}, +function): zsh evaluates
// them in this shell while it globs, so their code can cd or push. (bash reads them as extglobs.)
var codeQualifier = regexp.MustCompile(`e[^A-Za-z0-9_\s|]|\+[A-Za-z_]`)

// qualifierCode returns the words of a statement that hold a glob qualifier that runs code, as
// written, or "". zsh globs, and so runs that code, in the arguments, the assignments (arrays too),
// a for list and the redirections, before the command runs.
func qualifierCode(s *syntax.Stmt) string {
	var words []*syntax.Word
	for _, r := range s.Redirs {
		words = append(words, r.Word)
	}
	assigns := func(list []*syntax.Assign) {
		for _, a := range list {
			words = append(words, a.Value)
			if a.Array != nil {
				for _, e := range a.Array.Elems {
					words = append(words, e.Value)
				}
			}
		}
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		words = append(words, c.Args...)
		assigns(c.Assigns)
	case *syntax.DeclClause:
		assigns(c.Args)
	case *syntax.ForClause:
		if it, ok := c.Loop.(*syntax.WordIter); ok {
			words = append(words, it.Items...)
		}
	}
	var b bytes.Buffer
	for _, word := range words {
		if word == nil {
			continue
		}
		for _, part := range word.Parts {
			if g, ok := part.(*syntax.ExtGlob); ok && codeQualifier.MatchString(g.Pattern.Value) {
				_ = syntax.NewPrinter().Print(&b, word)
				b.WriteByte(' ')
				break
			}
		}
	}
	return b.String()
}

// autoCD reports whether zsh's AUTO_CD (on in the Bash tool's zsh) runs a command of one word as
// `cd WORD`: the word is no command but a directory, here or wherever the unknown directory is.
func (w *walker) autoCD(args []string, lit []bool) bool {
	if len(args) != 1 || !lit[0] || args[0] == "" {
		return false
	}
	if w.onPath(args[0]) {
		return false
	}
	if _, ok := w.funcs[args[0]]; ok || w.cdFuncs[args[0]] || shellCommand(args[0]) {
		return false // a builtin (popd, eval …) or function, which zsh does not take for a directory
	}
	d := w.resolve(args[0])
	if d == "" || strings.HasPrefix(d, bodyDir+"/") {
		return true // relative to a directory unknown here (already, or a function's call)
	}
	fi, err := os.Stat(d)
	return err == nil && fi.IsDir()
}

// precommand lists the words that run the rest of the command in this shell.
var precommand = map[string]bool{"command": true, "builtin": true, "exec": true, "noglob": true, "nocorrect": true}

// stripBraces removes the braces of a zsh brace group written without spaces, `{git push …}`,
// which bash would read as a command named `{git`.
func stripBraces(args []string, lit []bool) ([]string, []bool) {
	if len(args) == 0 || !lit[0] || !strings.HasPrefix(args[0], "{") {
		return args, lit
	}
	n := len(args[0]) - len(strings.TrimLeft(args[0], "{"))
	args = append([]string{args[0][n:]}, args[1:]...)
	lit = append([]bool(nil), lit...)
	if last := len(args) - 1; lit[last] {
		trimmed := args[last]
		for k := 0; k < n && strings.HasSuffix(trimmed, "}"); k++ {
			trimmed = strings.TrimSuffix(trimmed, "}")
		}
		args[last] = trimmed
		if trimmed == "" && last > 0 { // `}` stood alone
			args, lit = args[:last], lit[:last]
		}
	}
	if args[0] == "" { // `{` stood alone
		args, lit = args[1:], lit[1:]
	}
	return args, lit
}

// expandLiteral returns the fields bash makes of word when they do not depend on the environment,
// the filesystem or another command: quote removal, escapes, $'…', brace expansion and ~. ok is
// false for a word with a parameter, command, arithmetic or process substitution, an unquoted glob,
// a $ that bash leaves as text (zsh expands `$=x`, `$^x` and `$~x`), or ~+ / ~- (the directories).
func expandLiteral(word *syntax.Word) ([]string, bool) {
	if word == nil {
		return nil, false
	}
	for i, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if pattern.HasMeta(p.Value, 0) || bareDollar(p.Value) >= 0 ||
				(i == 0 && (strings.HasPrefix(p.Value, "~+") || strings.HasPrefix(p.Value, "~-"))) {
				return nil, false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if lit, ok := q.(*syntax.Lit); !ok || bareDollar(lit.Value) >= 0 {
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

// bareDollar returns the index of the first $ in a literal that no backslash escapes, or -1. bash
// leaves such a $ as text, but zsh expands `$=x`, `$^x` and `$~x`; `\$` is text in both.
func bareDollar(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // the next character is escaped
		case '$':
			return i
		}
	}
	return -1
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
			if i := bareDollar(p.Value); i >= 0 {
				return b.String() + p.Value[:i]
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
				if i := bareDollar(lit.Value); i >= 0 {
					return b.String() + lit.Value[:i]
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

// fields expands a command's words as fields() does, except that once the command has set HOME, a
// word starting with ~ is not literal: the shell expands it with the new HOME, not the hook's.
func (w *walker) fields(words []*syntax.Word) (args []string, lit []bool) {
	if !w.homeSet {
		return fields(words)
	}
	for _, word := range words {
		a, l := fields([]*syntax.Word{word})
		if p, ok := word.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(p.Value, "~") { // a word has a part
			l = make([]bool, len(l))
		}
		args, lit = append(args, a...), append(lit, l...)
	}
	return args, lit
}

var home, _ = os.UserHomeDir()

// zshCode lists zsh's special parameters whose values are shell code or the names of functions the
// shell runs: assigning one defines a function or an alias (functions[gp]='git push --no-verify
// "$@"'), or makes zsh call a function on each cd or before each command (chpwd_functions+=(f)).
var zshCode = map[string]bool{"functions": true, "dis_functions": true, "aliases": true, "dis_aliases": true,
	"galiases": true, "dis_galiases": true, "saliases": true, "dis_saliases": true, "chpwd_functions": true,
	"precmd_functions": true, "preexec_functions": true, "periodic_functions": true, "zshexit_functions": true,
	"zshaddhistory_functions": true}

// zshCodeAssign checks an assignment to one of zsh's code parameters (zshCode) as written:
// PARAM[KEY]=CODE or PARAM=CODE with a literal KEY that is not a name the walker models and literal
// CODE that passes definition. Anything else is refused.
func (w *walker) zshCodeAssign(a *syntax.Assign) {
	param := a.Name.Value
	if a.Index != nil { // functions[NAME]=…: the key names the function or alias
		key, ok := a.Index.(*syntax.Word)
		name := ""
		if ok {
			name, ok = literalWord(key)
		}
		if !ok {
			w.block("zsh's " + param + " parameter set under a key the hook cannot read can redefine cd or git; " + runAlone)
			return
		}
		if modeledNames[name] {
			w.block("the command defines " + name + " through zsh's " + param + ", which the hook reads as the builtin or program of that name; " + runAlone)
			return
		}
	}
	code, ok := literalWord(a.Value)
	if !ok || a.Array != nil {
		w.block("zsh's " + param + " parameter set from a value the hook cannot read defines code it cannot check; " + runAlone)
		return
	}
	w.definition("zsh's "+param+" parameter", code)
}

func (w *walker) call(c *syntax.CallExpr, redirs []*syntax.Redirect) {
	for _, a := range c.Assigns {
		switch {
		case a.Name == nil:
		case zshCode[a.Name.Value]:
			w.zshCodeAssign(a)
		default:
			w.assignExpr(a)
		}
	}
	saved := w.cmdText
	w.cmdText = wordsText(c.Args, redirs) // this command's own text for the conservative net
	args, lit := w.fields(c.Args)
	w.command(args, lit, redirs)
	// The net under the walker, on the words and the heredoc or here-string bodies as bash expands
	// them ($'\x2d…' included): a hook token is refused in any command, as main.run refuses it in the
	// command line's text.
	expanded := strings.Join(args, " ")
	for _, r := range redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			if s, ok := heredocText(r, bodyOf(r)); ok {
				expanded += "\n" + s
			}
		}
	}
	if token := hookToken(expanded); token != "" {
		w.block(tokenRefusal(token))
	}
	w.cmdText = saved
}

var (
	noVerifyToken = regexp.MustCompile(`(?i)no-verify|--no-v(e(r(i(f)?)?)?)?([^a-z]|$)`)
	gitVarToken   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(GIT_[A-Z0-9_]+)`)
	plumbingToken = regexp.MustCompile(`(?i)(send|receive|upload)-pack|http-push|git-remote-|remote-(https?|ftps?|ext|fd)\b`)
)

// hookToken returns what in text could keep git's pre-push hook from running, or "": --no-verify or
// an abbreviation git accepts for it, core.hooksPath, the .githooks directory, a GIT_* variable
// outside the safe list, or git's plumbing that pushes, or serves a push, without the hook.
func hookToken(text string) string {
	n := normalize(text)
	lower := strings.ToLower(n)
	switch {
	case noVerifyToken.MatchString(n):
		return "--no-verify"
	case strings.Contains(lower, "hookspath"):
		return "core.hooksPath"
	case strings.Contains(lower, ".githooks"):
		return ".githooks"
	}
	if m := plumbingToken.FindString(n); m != "" {
		return m
	}
	for _, m := range gitVarToken.FindAllStringSubmatch(n, -1) {
		if v := m[1]; !safeGitEnv[v] && !gitEnvCommands[v] && !strings.HasPrefix(v, "GIT_TRACE") {
			return v
		}
	}
	return ""
}

// tokenRefusal is the refusal of a command line that names a hook token, in its text or its words.
func tokenRefusal(token string) string {
	return token + " in a command line could keep git's pre-push hook from running (a program, shell code, a shell alias or an option the hook does not model can hand it to git), so the hook refuses it in any command; write text that names it with the Edit or Write tools or into a file (git commit -F FILE, gh … --body-file FILE), and search with Claude Code's Grep tool where it has one, or with a quoted pattern that does not spell the token out (grep -rn 'hooks[P]ath')"
}

// definition checks code the command defines to run later under a name (an alias, or a zsh function
// set through its parameter). The walker cannot follow a name to where it runs, so code that runs
// git, mentions a hook override, or changes the directory is refused.
func (w *walker) definition(how, code string) {
	f, err := parseBash(code)
	if mentionsGit(code) || (err == nil && w.changesDir(f)) {
		w.block(how + " defines code that runs git or changes the directory, which the hook cannot follow to where it runs; " + runAlone)
	}
}

// command dispatches an already-expanded argv.
func (w *walker) command(args []string, lit []bool, redirs []*syntax.Redirect) {
	args, lit = stripBraces(args, lit)
	// env runs the rest as a child process, whose cd cannot move this shell.
	dir, envUnknown, child := w.dir, w.envUnknown, false
	defer func() {
		if child {
			w.dir, w.envUnknown = dir, envUnknown
		}
	}()
	program := w.program // command, builtin, exec and env skip the shell's functions
prefix:
	for len(args) > 0 && lit[0] {
		switch path.Base(strings.TrimPrefix(args[0], "=")) { // zsh's =cmd is the path of cmd
		case "command", "exec", "builtin", "noglob", "nocorrect":
			// They run the rest as a command; zsh's noglob/nocorrect are precommand modifiers, which
			// take no options. (zsh's `-` modifier is not read as one: an alias of that name, `cd -`
			// in oh-my-zsh, comes first.) After -- (and command's -p) the command follows as well; with
			// another option (exec -a NAME, command -v) the grammar varies and -v/-V only print, so
			// fall back to a conservative text check.
			base := path.Base(strings.TrimPrefix(args[0], "="))
			args, lit = args[1:], lit[1:]
			for len(args) > 1 && lit[0] && (args[0] == "--" || args[0] == "-p" && base == "command") {
				args, lit = args[1:], lit[1:]
			}
			if (base == "command" || base == "exec" || base == "builtin") && len(args) > 0 && lit[0] && strings.HasPrefix(args[0], "-") {
				w.blockText(w.cmdText, base+" with options")
				return
			}
			program = program || base == "command" || base == "exec" || base == "builtin"
		case "env":
			child, program = true, true
			args, lit = args[1:], lit[1:]
		envOpts:
			for len(args) > 0 {
				if !lit[0] { // NAME=$VALUE: still an assignment when NAME is literal
					if !envName.MatchString(args[0]) {
						break envOpts
					}
					w.assignWord(args[0])
					args, lit = args[1:], lit[1:]
					continue
				}
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
		w.dynamicWord(args, lit)
		w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "a command named by a word the hook cannot read"})
		return
	}
	if fn := args[0]; !program {
		// A function the command defines runs instead of the builtin or program of its name: its body
		// was walked where it was defined, so only its cd matters here, code in it the walker could
		// not read (which runs from here), and a push in its arguments, which it may hand to git or a
		// shell ("$@").
		for _, name := range w.body { // the body being walked runs fn, as it is defined now
			w.ref(w.refs[name], fn)
		}
		w.runFunc(fn, w.dir)
		if w.cdFuncs[fn] {
			// A function that may cd runs instead. If its definition may not have run, the name may
			// also be an alias or function of the shell's setup, which runs from here.
			if !w.defined[fn] {
				w.unread(unknownRun{Dir: w.dir, Word: fn})
			}
			w.dir = ""
		}
		if _, ok := w.funcs[fn]; ok {
			if !knownCommand(fn) && !w.onPath(fn) {
				// zsh expands an alias before it looks a function up, so an alias of this name in the
				// shell's setup (oh-my-zsh's gp is `git push`) runs instead: from here, as any word
				// the walker does not know.
				w.unread(unknownRun{Dir: w.dir, Word: fn, What: aliasWhat(fn)})
			}
			if argvHasGitPush(args, lit) || mentionsGitPush(w.cmdText) {
				w.block("a git push run through " + fn + " cannot be checked; " + runAlone)
			}
			w.argvGit(args, lit)
			// Its body may cd, or, if it runs code given at run time ("$@", eval), its arguments may be a
			// command that moves this shell (r cd DIR, argvMayCD), and a function named among them
			// runs there (argvFuncs).
			bodyMayCD := w.unknownWord(fn)
			mayCD := bodyMayCD || w.argvMayCD(args, lit, nil)
			if w.runsArgs(fn) && w.argvFuncs(args, lit, bodyMayCD) {
				mayCD = true
			}
			if mayCD {
				w.dir = ""
			}
			return
		}
	}
	name := path.Base(strings.TrimPrefix(args[0], "=")) // zsh =cmd expands to the path of cmd
	if sub, ok := strings.CutPrefix(name, "git-"); ok && (sub == "push" || sub == "config" || pushLike(sub) || gitRunners[sub] || cmdOptions[sub] != nil) {
		// git's dashed subcommand binaries: git-push is `git push`, so analyse it as git <sub>. Any
		// other git-* program (git-seed, git-lfs) is run by its own name, never as an alias, so it is
		// an ordinary command.
		w.git(append([]string{sub}, args[1:]...), append([]bool{true}, lit[1:]...))
		return
	}
	_, isWrapper := wrapperSkip[name]
	switch {
	case w.autoCD(args, lit):
		// An alias of that name would run instead (zsh expands it first), so it is an unknown word too.
		w.unread(unknownRun{Dir: w.dir, Word: args[0]})
		w.chdir("cd", args, lit)
	case name == "cd" || name == "chdir" || name == "pushd": // chdir: zsh's synonym for cd
		w.chdir(name, args[1:], lit[1:])
	case name == "popd":
		w.dir = ""
	case name == "source" || name == ".":
		w.source(args[1:], lit[1:], redirs)
	case name == "git":
		w.git(args[1:], lit[1:])
	case shells[name]:
		w.shell(args, lit, redirs)
	case name == "fish" || name == "csh" || name == "tcsh":
		w.blockText(w.cmdText, name) // their syntax is not bash's
	case name == "eval":
		w.eval(args[1:], lit[1:])
	case name == "emulate": // zsh: emulate [-LR] MODE ... -c CODE runs CODE in this shell, as eval
		for i := 1; i < len(args)-1; i++ {
			if lit[i] && args[i] == "-c" {
				w.eval(args[i+1:i+2], lit[i+1:i+2])
				break
			}
		}
	case name == "repeat" && len(args) > 2: // zsh: repeat N COMMAND... runs it in this shell
		w.command(args[2:], lit[2:], redirs)
	case name == "trap": // trap [--] ACTION SIGSPEC...: the action runs as a command on the signal
		a, l := args[1:], lit[1:]
		for len(a) > 0 && l[0] && (a[0] == "--" || a[0] == "-p" || a[0] == "-l") {
			if a[0] == "-l" { // -l only lists signal names
				a = nil
				break
			}
			a, l = a[1:], l[1:]
		}
		// `trap - SIG…`, `trap SIG` and `trap N…` (an unsigned number first) reset the signals: no action.
		if len(a) > 0 && l[0] && (a[0] == "-" || len(a) == 1 || isUnsigned(a[0])) {
			a = nil
		}
		if len(a) > 0 {
			if l[0] {
				// The action runs later, on exit or a signal (DEBUG: before each command), from
				// wherever the shell is then: a push in it runs from an unknown directory, and a cd in
				// it may move the directory between any two commands from here on, so a later push is
				// refused.
				saved := w.dir
				w.dir = ""
				w.script(a[0])
				w.dir = saved
				f, err := parseBash(a[0])
				if err != nil || w.changesDir(f) {
					w.loseDir("a trap action that may cd, set earlier in the command,")
				}
				if err == nil { // it runs the functions defined when it runs: redefined reads it again
					if len(w.traps) >= maxTraps {
						w.block(fmt.Sprintf("the command sets more than %d trap actions, more than the hook follows; run it in parts", maxTraps))
					} else {
						t := trapAction{code: f, refs: map[string]int{}}
						w.bodyRefs(t.refs, f)
						w.traps = append(w.traps, t)
					}
				}
			} else {
				w.blockText(w.cmdText, "trap")
				w.loseDir("a trap action the hook cannot read, set earlier in the command,")
			}
		}
	case name == "alias": // alias [-gs…] NAME=VALUE…: VALUE runs wherever NAME is used later
		for i, a := range args[1:] {
			switch {
			case !lit[i+1]:
				w.block("an alias defined from a word the hook cannot read can redefine cd or run git; " + runAlone)
			case strings.Contains(a, "=") && !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+"):
				aliasName, value, _ := strings.Cut(a, "=")
				if modeledNames[aliasName] {
					w.block("the command defines an alias named " + aliasName + ", which the hook reads as the builtin or program of that name; " + runAlone)
				}
				w.definition("alias "+aliasName, value)
			}
		}
	case name == "set": // zsh: set -A NAME VALUE… (or +A) assigns the array NAME
		for i := 1; i < len(args); i++ {
			a := args[i]
			if !lit[i] || len(a) < 2 || a == "--" || (a[0] != '-' && a[0] != '+') {
				break
			}
			if strings.Contains(a[1:], "o") { // -o NAME: an option's name
				i++
				continue
			}
			if strings.Contains(a[1:], "A") && i+1 < len(args) {
				if !lit[i+1] {
					w.block("set -A assigns an array the hook cannot name; " + runAlone)
				} else {
					w.assign(args[i+1], nil)
				}
				break
			}
		}
	case name == "enable" || name == "disable" || name == "functions" || name == "autoload" || name == "add-zsh-hook":
		// They can turn a builtin off (disable cd, bash's enable -n cd), load one (enable -f), define
		// a function under another name (zsh's functions -c f ls, autoload ls) or have the shell call
		// one on each cd or before each command (add-zsh-hook): refuse one aimed at a name the walker
		// models, at a hook, or at a word it cannot read. Any other name they touch may now run code
		// the walker has not seen, which may cd.
		for i, a := range args[1:] {
			if name == "add-zsh-hook" || !lit[i+1] || modeledNames[a] || hookFunction(a) {
				w.block(name + " can change what a command or a hook of the shell runs, which the hook cannot follow; " + runAlone)
				break
			}
			if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
				w.cdFuncs[a] = true
				w.defining(a)
			}
		}
		w.redefined()
	case name == "hash": // hash -p PATH NAME, zsh's hash NAME=PATH: the name runs another program
		if mentionsGit(w.cmdText) {
			w.block("hash can make the name git run another program; " + runAlone)
		}
	case name == "xargs" || name == "parallel":
		w.xargs(name, args[1:], lit[1:], redirs)
	case name == "read" || name == "mapfile" || name == "readarray" || name == "getopts":
		w.setter(name, args[1:], lit[1:])
	case name == "printf":
		w.printf(args[1:], lit[1:])
	case name == "echo" || pureOutput[name]:
		// prints its arguments
	case isWrapper:
		w.wrapped(name, args, lit, redirs)
	case execRunners[name]:
		w.execRunner(args, lit, redirs)
	case name == "find":
		w.findExec(args, lit)
	case name == "watch":
		w.watch(args[1:], lit[1:])
	default:
		// An unrecognized command: it is not a known runner, so its arguments are usually data. But a
		// git push plainly in its arguments (a custom runner, ssh, a tool not modelled) is refused, as
		// the contract promises for "another command that runs its arguments". (A --no-verify or
		// core.hooksPath given to it is refused by the net in call().)
		if !dataCommands[name] && argvHasGitPush(args, lit) {
			w.block("a git push run through " + name + " cannot be checked; " + runAlone)
		}
		unknown := !program && w.unknownWord(args[0])
		if !dataCommands[name] {
			// A shell with a script among its arguments (arch -arm64 sh -c '…', script -q /dev/null
			// bash -c '…') is walked as it runs: from here, or from anywhere after a word that may cd.
			dir := w.dir
			if unknown {
				dir = ""
			}
			w.argvShell(args, lit, dir)
		}
		if unknown {
			// An alias or function of the shell's setup (or a program not on PATH): it may cd, push
			// (oh-my-zsh's gp is `git push`, which main.go lets run only in this clone, where git's hook
			// judges it) or hand its arguments to git (a git alias among them is checked). A function the
			// command defines that may cd was recorded above, where it ran from.
			if !w.cdFuncs[args[0]] {
				w.unread(unknownRun{Dir: w.dir, Word: args[0]})
			}
			w.argvGit(args, lit)
			w.argvFuncs(args, lit, true)
			w.dir = ""
		}
	}
}

// argvShell walks the first shell given a script (`sh -c '…'`, `bash -lc '…'`) among the arguments of
// a command the walker does not model, as that shell runs from dir.
func (w *walker) argvShell(args []string, lit []bool, dir string) {
	for i := 1; i < len(args); i++ {
		if lit[i] && shells[path.Base(args[i])] {
			if shellScriptNext(args[i+1:], lit[i+1:]) {
				w.inChild(dir, args[i:], lit[i:], nil)
				return
			}
			w.blockText(w.cmdText, args[i]) // options it does not read may still give a script
		}
	}
}

// shellScriptNext reports whether a shell's arguments start with options that give it a script as an
// argument (-c, alone or in a cluster such as -lc or -ec).
func shellScriptNext(args []string, lit []bool) bool {
	for j := 0; j < len(args) && lit[j] && len(args[j]) > 1 && (args[j][0] == '-' || args[j][0] == '+'); j++ {
		switch {
		case args[j] == "--":
			return false
		case args[j] == "-o" || args[j] == "+o" || args[j] == "-O" || args[j] == "+O" || shellWithValue[args[j]]:
			j++ // its value is the next argument
		case args[j][0] == '-' && args[j][1] != '-' && strings.Contains(args[j], "c"):
			return true
		}
	}
	return false
}

// runFunc records the code a call of the function fn from dir runs that the walker could not read:
// what its body (and the bodies it called, as they were) could not, placed at the call, and, when a
// function it runs was defined again since (stale), the function itself as an unknown word, from
// dir or, if it may cd, from anywhere.
func (w *walker) runFunc(fn, dir string) {
	saved := w.dir
	w.dir = dir
	defer func() { w.dir = saved }()
	for _, u := range w.funcUnread[fn] {
		w.unread(w.callRun(u))
	}
	if w.stale(fn) {
		u := unknownRun{Dir: dir, Word: fn, What: staleWhat(fn)}
		if w.unknownWord(fn) {
			u.Dir = ""
		}
		w.unread(u)
	}
}

// aliasWhat says why a call of a function the command defines may run an alias of the setup.
func aliasWhat(fn string) string {
	return "`" + fn + "` is a function the command defines, but an alias of that name in the shell's setup would run instead (zsh expands aliases before it looks functions up)"
}

// staleWhat says why a stale function runs code the walker did not read.
func staleWhat(fn string) string {
	return "`" + fn + "` runs a function defined, or defined again, after it (bash and zsh look a function up when it is called), code the hook did not read"
}

// argvFuncs records the functions the command defines that a command may run from its arguments (a
// function that runs "$@", an alias or function of the shell's setup), as runFunc does for a call:
// from here, or from anywhere when what runs them may cd first. It reports whether one may cd.
func (w *walker) argvFuncs(args []string, lit []bool, mayCDFirst bool) bool {
	dir, mayCD := w.dir, false
	if mayCDFirst {
		dir = ""
	}
	for i := 1; i < len(args); i++ {
		if !lit[i] || strings.Contains(args[i], "/") || w.refs[args[i]] == nil {
			continue
		}
		w.runFunc(args[i], dir)
		mayCD = mayCD || w.unknownWord(args[i])
	}
	return mayCD
}

// argvGit walks each literal `git` in the arguments of a command that may hand them to git (a
// function the command defines with "$@", an alias or function of the shell's setup) as the git
// command it would be: a configuration, ref write or remote helper refused in a command is refused
// here too, and a git alias among them is checked (checkSubcommands). Only a `git` followed by git's
// global options and a literal subcommand counts, so `git` as data (grep -e git -e config) does not.
// A push among them is refused by the caller.
func (w *walker) argvGit(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		if !lit[i] || path.Base(args[i]) != "git" || !gitSubcommandNext(args[i+1:], lit[i+1:]) {
			continue
		}
		s := w.child(w.dir, w.src)
		s.git(args[i+1:], lit[i+1:])
		w.block(s.blocked)
		w.calls = append(w.calls, s.calls...)
	}
}

// gitSubcommandNext reports whether args (those after a literal `git`) are git's global options
// followed by a literal subcommand.
func gitSubcommandNext(args []string, lit []bool) bool {
	for j := 0; j < len(args) && lit[j]; j++ {
		a := args[j]
		if !strings.HasPrefix(a, "-") {
			return true
		}
		name, _, hasValue := strings.Cut(a, "=")
		if !gitGlobalSafe[name] && !gitGlobalWithValue[name] {
			return false
		}
		if gitGlobalWithValue[name] && !hasValue {
			j++ // its value is the next argument
		}
	}
	return false
}

// unknownWord reports whether a command word may be a function or alias of the user's shell setup,
// which the Bash tool loads (zoxide's z, oh-my-zsh's ... and take), or a builtin the walker does not
// know, and so may cd: no program on PATH (which runs as a child, whose cd cannot move this shell),
// no path, no builtin or command the walker knows. A function the command defines may cd when its
// body may, followed through the functions it calls.
func (w *walker) unknownWord(word string) bool {
	name := path.Base(strings.TrimPrefix(word, "="))
	if w.cdFuncs[name] && !strings.Contains(word, "/") {
		return true // a function that may be defined, and may cd, whatever else the name is
	}
	if _, ok := w.funcs[name]; ok && !strings.Contains(word, "/") {
		return w.funcMayCD(name)
	}
	if strings.Contains(word, "/") || knownCommand(name) {
		return false
	}
	return !w.onPath(name)
}

// onPath reports whether name is a program on PATH, asking once per name and command: the walker
// asks about the same words many times.
func (w *walker) onPath(name string) bool {
	v, ok := w.paths[name]
	if !ok {
		_, err := exec.LookPath(name)
		v = err == nil
		w.paths[name] = v
	}
	return v
}

// funcMayCD reports whether a call of the function name may cd: whether its own body, or the body of a
// function it calls, as defined now, may (directCD). Each body is looked at once per directory, so
// functions that call each other many times cost no more than their text (a chain of functions each
// calling the next twice would otherwise take time exponential in its length, past the hook's
// timeout, which lets the command run).
func (w *walker) funcMayCD(name string) bool {
	key := cdKey{name, w.dir}
	if v, ok := w.reachMemo[key]; ok {
		return v
	}
	seen, v := map[string]bool{name: true}, false
	for queue := []string{name}; len(queue) > 0 && !v; queue = queue[1:] {
		if r, ok := w.reachMemo[cdKey{queue[0], w.dir}]; ok {
			v = r // looked at already: it, and all it reaches, may cd or not
			continue
		}
		d := w.directCD(queue[0])
		v = d.cd
		for _, c := range d.calls {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	if !v {
		// Nothing it reaches may cd, so nothing any of those reaches may either: one search settles
		// them all (a trap read again after each definition asks about each of them).
		for n := range seen {
			w.reachMemo[cdKey{n, w.dir}] = false
		}
	}
	w.reachMemo[key] = v
	return v
}

// directCD is what the body of the function name does to the directory itself, and the functions it
// calls (whose bodies funcMayCD looks at), from the current directory.
func (w *walker) directCD(name string) cdVerdict {
	v, ok := w.cdMemo[name][w.dir]
	if !ok {
		v.cd = w.changesDirCalls(w.funcs[name], &v.calls)
		if w.cdMemo[name] == nil {
			w.cdMemo[name] = map[string]cdVerdict{}
		}
		w.cdMemo[name][w.dir] = v
	}
	return v
}

// shellCommand reports whether name is a builtin or keyword of the shell the walker knows. (A program
// it models but that is not installed can still be a directory zsh's AUTO_CD enters.)
func shellCommand(name string) bool {
	return shellBuiltins[name] || shellOnly[name] || modeledNames[name] && name != "git" && name != "env"
}

// shellOnly are builtins the walker handles by name that its other lists do not mark as builtins.
var shellOnly = map[string]bool{"true": true, "false": true, ":": true, "test": true, "[": true, "mapfile": true,
	"readarray": true, "getopts": true, "add-zsh-hook": true}

// knownCommand reports whether the walker knows name as a builtin or a command it models.
func knownCommand(name string) bool {
	_, wrapper := wrapperSkip[name]
	return wrapper || shellBuiltins[name] || namedCommands[name] || shells[name] || execRunners[name] ||
		dataCommands[name] || pureOutput[name]
}

// modeledNames are the names the walker reads as a builtin or program it models: a function or
// alias of that name the command defines would run instead, unseen, so such a definition is refused.
var modeledNames = map[string]bool{"cd": true, "chdir": true, "pushd": true, "popd": true, "exit": true,
	"return": true, "source": true, ".": true, "eval": true, "trap": true, "emulate": true, "repeat": true,
	"git": true, "command": true, "builtin": true, "exec": true, "env": true, "noglob": true, "nocorrect": true,
	"alias": true, "hash": true, "set": true, "export": true, "declare": true, "typeset": true, "readonly": true,
	"local": true, "read": true, "printf": true, "unset": true}

// namedCommands are the commands command() handles by name that change no directory (those that do
// are in changesDir's list).
var namedCommands = map[string]bool{"git": true, "alias": true, "hash": true, "read": true, "mapfile": true,
	"readarray": true, "getopts": true, "printf": true, "watch": true, "xargs": true, "parallel": true, "find": true,
	"fish": true, "csh": true, "tcsh": true, "env": true, "command": true, "exec": true, "builtin": true,
	"noglob": true, "nocorrect": true, "add-zsh-hook": true}

// shellBuiltins are bash and zsh builtins and keywords, not programs on PATH, that never change the
// shell's directory (the ones that do, and those that run code, are handled by name above).
var shellBuiltins = map[string]bool{"export": true, "local": true, "declare": true, "typeset": true,
	"readonly": true, "integer": true, "float": true, "unset": true, "set": true, "setopt": true, "unsetopt": true,
	"shopt": true, "shift": true, "return": true, "exit": true, "logout": true, "true": true, "false": true, ":": true,
	"test": true, "[": true, "print": true, "type": true, "whence": true, "where": true, "which": true,
	"unalias": true, "rehash": true, "unhash": true, "wait": true, "jobs": true, "bg": true, "fg": true,
	"disown": true, "kill": true, "let": true, "ulimit": true, "umask": true, "times": true, "history": true,
	"fc": true, "dirs": true, "autoload": true, "zmodload": true, "bindkey": true, "compdef": true, "zstyle": true,
	"zparseopts": true, "enable": true, "disable": true, "caller": true, "help": true, "suspend": true, "limit": true,
	"unlimit": true, "functions": true, "vared": true, "zle": true, "sched": true, "pwd": true, "echo": true,
	"break": true, "continue": true, "complete": true, "compgen": true, "compopt": true, "bind": true, "unfunction": true}

// envName matches the literal prefix of an env operand NAME=VALUE whose value is not literal.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// dataCommands read their arguments as data (patterns, text, files) and never run them, so a git
// push or a --no-verify among them is not handed to git.
var dataCommands = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"ugrep": true, "sed": true, "awk": true, "gawk": true, "jq": true, "yq": true, "sort": true, "uniq": true, "wc": true,
	"head": true, "tail": true, "cut": true, "tr": true, "diff": true, "tee": true, "less": true, "more": true,
	"man": true, "ls": true, "print": true}

// dynamicWord refuses a command whose word is not literal when a literal push follows it: "$GIT"
// push could be git push.
func (w *walker) dynamicWord(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		if lit[i] && args[i] == "push" {
			w.block("a git push through a command word that is not literal cannot be checked; " + runAlone)
			return
		}
	}
}

// source walks what the shell sources. The text of standard input given in a heredoc or here-string
// (`source /dev/stdin <<<…`) runs as eval runs its string. Anything else is out of reach: a file
// leaves the directory and git's environment unknown after it, and standard input from elsewhere or
// an operand the hook cannot read (`. <(…)`, whose text is checked for a push) may push, so it runs
// only in this clone.
func (w *walker) source(args []string, lit []bool, redirs []*syntax.Redirect) {
	stdin := len(args) > 0 && lit[0] && (args[0] == "/dev/stdin" || args[0] == "/dev/fd/0")
	if stdin {
		for _, r := range redirs {
			if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc || r.Op == syntax.WordHdoc {
				if s, ok := heredocText(r, bodyOf(r)); ok {
					w.eval([]string{s}, []bool{true})
					return
				}
			}
		}
	}
	if len(args) > 0 && !lit[0] {
		w.blockText(w.cmdText, "source")
	}
	if stdin || len(args) > 0 && !lit[0] {
		w.unread(unknownRun{Dir: w.dir, Word: "source", What: "`source` of standard input or of a word the hook cannot read runs a script it did not read"})
	}
	w.dir, w.envUnknown = "", "a file sourced earlier in the command"
}

// inChild walks the command a child process runs, from dir: its cd and source do not reach this shell.
func (w *walker) inChild(dir string, args []string, lit []bool, redirs []*syntax.Redirect) {
	savedDir, envUnknown, program := w.dir, w.envUnknown, w.program
	w.dir, w.program = dir, true // a wrapper execs a program: the shell's functions do not apply
	w.command(args, lit, redirs)
	w.dir, w.envUnknown, w.program = savedDir, envUnknown, program
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
	"sudo": {map[string]bool{"-u": true, "-g": true, "-U": true, "-C": true, "-h": true, "-p": true, "-r": true, "-t": true, "-D": true, "-R": true,
		"--user": true, "--group": true, "--other-user": true, "--close-from": true, "--host": true, "--prompt": true, "--role": true,
		"--type": true, "--chdir": true, "--chroot": true}, 0},
	"doas":    {map[string]bool{"-u": true, "-C": true}, 0},
	"chrt":    {map[string]bool{"-T": true, "-P": true}, 0},
	"time":    {map[string]bool{"-o": true, "-f": true, "--format": true}, 0},
	"stdstub": {nil, 0},
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
		w.dynamicWord(inner, innerLit)
		w.blockText(w.cmdText, name)
		return
	}
	dir := w.dir
	if name == "sudo" && sudoMoves(args[1:len(args)-len(inner)]) {
		dir = "" // the command runs in another directory or root
	}
	w.inChild(dir, inner, innerLit, redirs)
}

// sudoMoves reports whether sudo's options run the command elsewhere: -D/--chdir, -R/--chroot, or
// -i/--login (the target user's home).
func sudoMoves(opts []string) bool {
	for _, o := range opts {
		switch {
		case strings.HasPrefix(o, "--chdir"), strings.HasPrefix(o, "--chroot"), o == "--login":
			return true
		case len(o) > 1 && o[0] == '-' && o[1] != '-' && strings.ContainsAny(o[1:], "DRi"):
			return true
		}
	}
	return false
}

// containerPush is why a git push run in a container is refused: it pushes the container's clone,
// whose commits the gate does not judge, and git's pre-push hook does not run there.
func containerPush(runner string) string {
	return "a git push run in a container (" + runner + " … git push) cannot be checked: the gate judges this clone's commits, and git's pre-push hook does not run there; push from this clone, or run it outside Claude Code"
}

// execRunner reads a command run in a container (kubectl exec POD -- CMD, docker exec CTR CMD), where
// a git push is the container's clone's (containerPush). It parses the command after a `--`, with the
// statement's heredocs and here-strings; a shell among the arguments, with or without `--`, runs a
// script of the command's own words or redirections there, so a push in that text is refused; and
// without either, only a plain adjacent `git push` in the arguments is (so a `--push` flag or a
// git_push.json file name does not trip it). Anything else is left to git's hook.
func (w *walker) execRunner(args []string, lit []bool, redirs []*syntax.Redirect) {
	runner := path.Base(args[0])
	for i := 1; i < len(args); i++ {
		if lit[i] && shells[path.Base(args[i])] && mentionsGitPush(w.cmdText) {
			w.block(containerPush(runner))
			break
		}
	}
	for i := 1; i < len(args); i++ {
		if lit[i] && args[i] == "--" {
			if i+1 < len(args) {
				// Its cd stays in the container, and its git reads the container's configuration (so
				// its subcommands are not checked for aliases here). A refusal still counts.
				s := newWalker(w.dir, w.src)
				s.cmdText = w.cmdText
				s.command(args[i+1:], lit[i+1:], redirs)
				w.block(s.blocked)
				if len(s.pushes) > 0 {
					w.block(containerPush(runner))
				}
			}
			return
		}
	}
	if argvHasGitPush(args, lit) {
		w.block(containerPush(runner))
	}
}

// pipesToContainer returns the runner when a pipeline's last element runs a shell in a container (`…
// | kubectl exec -i pod -- sh`), which reads its script from the pipe.
func pipesToContainer(c *syntax.BinaryCmd) (string, bool) {
	if c.Op != syntax.Pipe && c.Op != syntax.PipeAll {
		return "", false
	}
	right, ok := c.Y.Cmd.(*syntax.CallExpr)
	if !ok || len(right.Args) == 0 {
		return "", false
	}
	args, lit := fields(right.Args)
	if len(args) == 0 || !lit[0] || !execRunners[path.Base(args[0])] {
		return "", false
	}
	for i := 1; i < len(args); i++ {
		if lit[i] && shells[path.Base(args[i])] {
			return path.Base(args[0]), true
		}
	}
	return "", false
}

// watch runs its command again and again: through `sh -c` with its operands joined, or, with
// -x/--exec, as an argv.
func (w *walker) watch(args []string, lit []bool) {
	execArgv := false
	i := 0
	for ; i < len(args) && lit[i] && strings.HasPrefix(args[i], "-") && args[i] != "-"; i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if a == "--exec" || (!strings.HasPrefix(a, "--") && strings.Contains(a, "x")) {
			execArgv = true
		}
		if a == "-n" || a == "--interval" || a == "-q" || a == "--equexit" {
			i++ // the option's value is the next argument
		}
	}
	if i >= len(args) {
		return
	}
	if execArgv {
		w.inChild(w.dir, args[i:], lit[i:], nil)
		return
	}
	for _, l := range lit[i:] {
		if !l {
			w.blockText(w.cmdText, "watch")
			return
		}
	}
	w.script(strings.Join(args[i:], " "))
}

// findExec parses the command of a find -exec ... ; or +, which runs from the current directory,
// and of -execdir, which runs from each file's directory (unknown here).
func (w *walker) findExec(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		// -ok and -okdir run their command as -exec and -execdir do once their prompt is answered
		// (from standard input).
		if !lit[i] || (args[i] != "-exec" && args[i] != "-execdir" && args[i] != "-ok" && args[i] != "-okdir") {
			continue
		}
		end := i + 1
		for end < len(args) && (!lit[end] || (args[end] != ";" && args[end] != "+")) {
			end++
		}
		if end > i+1 {
			dir := w.dir
			if args[i] == "-execdir" || args[i] == "-okdir" {
				dir = ""
			}
			w.inChild(dir, args[i+1:end], lit[i+1:end], nil)
		}
		i = end
	}
}

// eval runs its argument as a script in the current shell, so a literal script's cd, sourced state,
// pushes and refusals all carry back. A dynamic string (eval "$(ssh-agent -s)") leaves the directory
// and git's environment unknown, and is refused if eval's own words (not sibling commands) name a push.
func (w *walker) eval(args []string, lit []bool) {
	for _, l := range lit {
		if !l {
			w.blockText(w.cmdText, "eval")
			w.unread(unknownRun{Dir: w.dir, Word: "eval", What: "`eval` of a string the hook cannot read runs code it did not read"})
			w.dir, w.envUnknown = "", "an eval of a dynamic string earlier in the command"
			return
		}
	}
	f, err := parseBash(strings.Join(args, " "))
	if err != nil {
		if mentionsGitPush(strings.Join(args, " ")) {
			w.block("cannot parse the string eval runs around git push; " + runAlone)
		}
		return
	}
	w.inEval++
	w.run(f) // the same walker: eval runs in the current shell, so its cd and source carry back
	w.inEval--
}

// replacesInput reports whether xargs' options put its input in place of a replace string (-I, -i,
// --replace, BSD's -J) instead of after the command.
func replacesInput(opts []string, lit []bool) bool {
	for i, a := range opts {
		if lit[i] && (strings.HasPrefix(a, "-I") || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--replace") || strings.HasPrefix(a, "-J")) {
			return true
		}
	}
	return false
}

// xargsValue lists the options that take the next argument as their value: xargs' (GNU and BSD;
// GNU's -i, -l and -e take only an attached value, so they are flags) and GNU parallel's common ones.
var xargsValue = map[string]map[string]bool{
	"xargs": {"-n": true, "--max-args": true, "-L": true, "-P": true, "--max-procs": true, "-s": true,
		"--max-chars": true, "-I": true, "-E": true, "-d": true, "--delimiter": true, "-a": true, "--arg-file": true,
		"--process-slot-var": true, "-J": true, "-R": true, "-S": true},
	"parallel": {"-n": true, "--max-args": true, "-L": true, "-l": true, "-P": true, "-j": true, "--jobs": true,
		"-N": true, "-I": true, "-i": true, "--replace": true, "-E": true, "-e": true, "--eof": true, "-d": true,
		"--delimiter": true, "-a": true, "--arg-file": true, "-S": true, "--sshlogin": true},
}

// xargs parses the command xargs or parallel builds. It appends standard input to that command, so
// a git push there (the input adds refs and could add --no-verify), a git with no subcommand (it
// would come from input) and a non-literal command word are all refused. parallel also runs its
// ::: arguments as commands, so a git push anywhere in its words is refused.
func (w *walker) xargs(name string, args []string, lit []bool, redirs []*syntax.Redirect) {
	refuse := func() {
		w.block("a git push through " + name + " cannot be checked (it appends arguments from standard input); " + runAlone)
	}
	if argvHasGitPush(args, lit) || (name == "parallel" && mentionsGitPush(w.cmdText)) {
		refuse() // whatever options were misread on the way
		return
	}
	inner, innerLit := skipOptions(args, lit, xargsValue[name], 0)
	if len(inner) == 0 {
		if name == "parallel" { // GNU parallel with no command runs each input line as a command
			w.unread(unknownRun{Dir: w.dir, Word: name, What: "`parallel` with no command runs each line of its input, which the hook did not read, as a command"})
		}
		return // xargs' default command is echo
	}
	if !innerLit[0] {
		w.dynamicWord(inner, innerLit)
		w.blockText(w.cmdText, name)
		return
	}
	// Run the command on a fresh walker, as the program it is: a push there refuses the whole command,
	// since the input adds arguments the gate cannot see; any other command (git branch, git add,
	// grep) is allowed. A shell there may take its script from the input (shell()).
	s := newWalker(w.dir, w.src)
	s.program, s.input = true, inputAppended
	if name == "parallel" || replacesInput(args[:len(args)-len(inner)], lit) {
		s.input = inputReplaced
	}
	s.command(inner, innerLit, nil)
	w.calls = append(w.calls, s.calls...)
	w.unread(s.unknown...)
	// A shell it runs may take its script from the input (shell()): a heredoc or here-string given to
	// it is that script, read as one (`xargs -0 sh -c <<< '…'`).
	if shells[path.Base(inner[0])] {
		for _, r := range redirs {
			if r.Op != syntax.Hdoc && r.Op != syntax.DashHdoc && r.Op != syntax.WordHdoc {
				continue
			}
			if text, ok := heredocText(r, bodyOf(r)); ok {
				w.script(text)
			} else {
				w.blockText(w.cmdText, name)
			}
		}
	}
	if len(s.pushes) > 0 || s.blocked != "" || (path.Base(inner[0]) == "git" && gitSubcommand(inner[1:], innerLit[1:]) < 0) {
		refuse()
	}
}

// gitSubcommand returns the index of git's subcommand in args (git's arguments), past its global
// options, or -1 when there is none.
func gitSubcommand(args []string, lit []bool) int {
	for j := 0; j < len(args); j++ {
		if !lit[j] || !strings.HasPrefix(args[j], "-") {
			return j
		}
		name, _, hasValue := strings.Cut(args[j], "=")
		if gitGlobalWithValue[name] && !hasValue {
			j++ // its value is the next argument
		}
	}
	return -1
}

// setter checks the variable names a read, mapfile, readarray or getopts sets.
func (w *walker) setter(name string, args []string, lit []bool) {
	// The options that take a value, per builtin: read -s (silent) and mapfile -t (strip) are flags.
	value := map[string]bool{"-d": true, "-n": true, "-N": true, "-p": true, "-t": true, "-u": true, "-i": true, "-a": true}
	if name == "mapfile" || name == "readarray" {
		value = map[string]bool{"-d": true, "-n": true, "-O": true, "-s": true, "-u": true, "-C": true, "-c": true}
	}
	// -a NAME (read into an array) and the trailing NAMEs are targets.
	for i := 0; i < len(args); i++ {
		if !lit[i] {
			continue
		}
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "--" {
			if (a == "-a" || a == "-A") && i+1 < len(args) && lit[i+1] {
				w.assign(args[i+1], nil)
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
				w.assign(args[i+1], nil)
			}
			return
		}
		w.assign(a, nil) // a plain NAME operand
	}
}

// printf checks only the target of printf -v NAME.
func (w *walker) printf(args []string, lit []bool) {
	for i := 0; i < len(args); i++ {
		if lit[i] && strings.HasPrefix(args[i], "-v") && len(args[i]) > 2 { // bash: -vNAME
			w.assign(args[i][2:], nil)
			return
		}
		if lit[i] && args[i] == "-v" && i+1 < len(args) {
			if lit[i+1] {
				w.assign(args[i+1], nil)
			}
			return
		}
		if lit[i] && !strings.HasPrefix(args[i], "-") {
			return // the format operand: the rest is data
		}
	}
}

// shellWithValue lists shell options that take the next argument as their value.
var shellWithValue = map[string]bool{"--rcfile": true, "--init-file": true, "--emulate": true}

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
			w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` runs a script the hook cannot read"})
			return
		case a == "--" || a == "-":
			i++
			break options
		case a == "--version" || a == "--help":
			return // it prints and runs nothing
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
	if w.input == inputReplaced || w.input == inputAppended && len(operands) == 0 {
		// With no script (-c) or script file of its own, or one the input fills in, the input is the
		// script or names it.
		w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` run by xargs or parallel takes its script from their input, which the hook did not read"})
	}
	switch {
	case command:
		if len(operands) == 0 {
			return
		}
		if !lit[i] {
			w.blockText(w.cmdText, "a shell script that is not literal")
			w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` runs a script the hook cannot read"})
			return
		}
		w.script(operands[0])
		return
	case !stdin && len(operands) > 0 && !lit[i]:
		w.blockText(w.cmdText, "a shell script that is not literal")
		w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` runs a script the hook cannot read"})
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
			w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` runs a script the hook cannot read"})
		}
		return
	}
	// The script comes from a pipe, a file or a redirection of an enclosing command (`(cd X && sh)
	// <<EOF`, `exec 0<<<…`). A literal producer piped straight in is parsed by pipedScript in the
	// pipeline branch; the rest is out of the hook's reach and may push, so it runs only in this clone.
	w.unread(unknownRun{Dir: w.dir, Word: args[0], What: "`" + args[0] + "` reads a script from standard input that the hook did not read"})
}

// heredocText returns the script a heredoc or here-string gives a shell, when it has no expansions.
func heredocText(r *syntax.Redirect, body *syntax.Word) (string, bool) {
	if body == nil { // an empty heredoc
		return "", true
	}
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
	f, err := parseBash(s)
	if err != nil {
		if mentionsGitPush(s) {
			w.block("cannot parse the shell script around git push; " + runAlone)
		}
		return
	}
	inner := w.child(w.dir, s)
	inner.run(f)
	w.block(inner.blocked)
	w.pushes = append(w.pushes, inner.pushes...)
	w.calls = append(w.calls, inner.calls...)
	w.unread(inner.unknown...)
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
		args, lit = args[1:], lit[1:] // -L, -P, -e, -@ (resolveFrom serves -L and -P alike)
	}
	if len(args) != 1 || !lit[0] || args[0] == "-" {
		w.dir = "" // $HOME, OLDPWD, a variable, or zsh's `cd OLD NEW`: unknown here
		return
	}
	if !filepath.IsAbs(args[0]) && (os.Getenv("CDPATH") != "" || w.cdpath) {
		w.dir = "" // CDPATH may take a relative cd elsewhere
		return
	}
	w.dir = w.resolve(args[0])
}

func (w *walker) resolve(dir string) string {
	return w.resolveFrom(w.dir, dir)
}

// resolveFrom resolves the operand of a cd from base. With `..`, bash's and zsh's default cd goes back
// lexically (`link/..` is base), while cd -P and a shell made physical (set -P, zsh's chase_dots,
// which the walker cannot follow) go to the parent of the link's target, as the kernel does: when the
// two readings reach different directories, the directory is unknown. A path that does not exist here
// is kept as written (gatePush refuses a directory it cannot stat).
func (w *walker) resolveFrom(base, dir string) string {
	written := w.physicalFrom(base, dir)
	if written == "" || !hasDotDot(written) {
		return written
	}
	lexical := filepath.Clean(written)
	physical, errP := filepath.EvalSymlinks(written)
	logical, errL := filepath.EvalSymlinks(lexical)
	switch {
	case errP != nil || errL != nil:
		return written
	case physical != logical:
		return ""
	}
	return lexical
}

// physicalFrom resolves dir from base as the kernel does for git -C, whose -C is a chdir(2): a path
// with `..` is kept as written, for gatePush's stat and git to resolve (after a symbolic link, the
// parent of its target).
func (w *walker) physicalFrom(base, dir string) string {
	switch {
	case filepath.IsAbs(dir) && hasDotDot(dir):
		return dir
	case filepath.IsAbs(dir):
		return filepath.Clean(dir)
	case base == "":
		return ""
	case hasDotDot(dir) || hasDotDot(base):
		return base + "/" + dir
	}
	return filepath.Join(base, dir)
}

// hasDotDot reports whether a path has a `..` element.
func hasDotDot(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

// gitGlobalWithValue lists git's global options that take the next argument as their value, so the
// subcommand after them is found correctly.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true, "--super-prefix": true, "--attr-source": true, "--list-cmds": true,
	"--shallow-file": true}

// gitPushLike lists git subcommands that push without running the pre-push hook; they are refused.
var gitPushLike = map[string]bool{"send-pack": true, "http-push": true}

// pushLike reports whether a git subcommand pushes without git's pre-push hook: send-pack, http-push,
// and the remote helpers (remote-https, remote-ext …), which run send-pack themselves.
func pushLike(sub string) bool {
	return gitPushLike[sub] || strings.HasPrefix(sub, "remote-")
}

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
		if a == "--help" || a == "-h" || a == "--version" || a == "-v" {
			return // git --help push is git help push: it shows help and runs nothing
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
				dir = w.physicalFrom(dir, value)
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
			case name == "-c" && litVal && configCommands[strings.ToLower(key)] && safeCommand(configValue(value)):
				// An editor or pager given as literal code that runs no git, as GIT_EDITOR,
				// GIT_SEQUENCE_EDITOR and GIT_PAGER may be (`git -c core.editor=true rebase --continue`).
			default:
				w.block(name + " " + key + ": " + refuseConfig(key))
				return
			}
		}
	}
	if i >= len(args) {
		return
	}
	sub, rest, restLit := args[i], args[i+1:], lit[i+1:] // literal: the loop refused it otherwise
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
	if how := remoteRefWrite(sub, rest, restLit); how != "" {
		w.block(how + " can move origin/main, which the gate measures a change from; fetch with git's own refspecs, in a command of its own")
		return
	}
	switch {
	case sub == "push":
		w.gitRunner(sub, rest, restLit) // --receive-pack and --exec run a command
	case pushLike(sub):
		w.block("git " + sub + " pushes without running git's pre-push hook; use `git push`")
		return
	case sub == "config":
		w.gitConfig(rest, restLit)
		return
	case gitRunners[sub]:
		w.gitRunner(sub, rest, restLit)
		return
	default:
		if cmdOptions[sub] != nil {
			w.gitRunner(sub, rest, restLit)
		}
		w.calls = append(w.calls, sub)
		return
	}
	if unsafe != "" {
		w.block("git " + unsafe + " can point git at another repository or configuration, where the pre-push hook does not run; push from the work tree without it")
		return
	}
	if w.envUnknown != "" {
		w.block(w.envUnknown + " can change the directory and git's environment; run `git push` in a command of its own")
		return
	}
	p, skip, ok := pushArgs(dir, rest, restLit)
	switch {
	case skip != "":
		w.block(skip) // wherever it runs: a push that skips git's hook is never gated
	case ok:
		w.pushes = append(w.pushes, p)
	}
}

// cmdOption is an option of a git subcommand whose value git runs as a shell command.
type cmdOption struct {
	long     string // --exec
	short    byte   // 'x', or 0
	optional bool   // the value can only be attached: -O<cmd>, --open-files-in-pager=<cmd>
}

// cmdOptions lists, per git subcommand, the options whose value git runs through the shell: per
// rebase step or changed file (rebase, difftool, filter-branch), to reach the other side of a
// local transport (upload-pack for fetch, pull, clone and ls-remote; receive-pack for push;
// archive's exec), or as a pager (grep -O).
var cmdOptions = map[string][]cmdOption{
	"rebase":   {{long: "--exec", short: 'x'}},
	"difftool": {{long: "--extcmd", short: 'x'}},
	"filter-branch": {{long: "--tree-filter"}, {long: "--index-filter"}, {long: "--parent-filter"},
		{long: "--msg-filter"}, {long: "--commit-filter"}, {long: "--tag-name-filter"}, {long: "--env-filter"},
		{long: "--setup"}},
	"send-email": {{long: "--sendmail-cmd"}, {long: "--to-cmd"}, {long: "--cc-cmd"}, {long: "--header-cmd"}},
	"fetch":      {{long: "--upload-pack"}},
	"pull":       {{long: "--upload-pack"}},
	"clone":      {{long: "--upload-pack", short: 'u'}},
	"ls-remote":  {{long: "--upload-pack", short: 'u'}},
	"push":       {{long: "--receive-pack"}, {long: "--exec"}},
	"archive":    {{long: "--exec"}},
	"grep":       {{long: "--open-files-in-pager", short: 'O', optional: true}},
}

// optionValues returns the values args give opts, read the way git's parse-options reads them: a
// short option's value attached (-xCMD) or next (-x CMD), also after bundled flags (-ixCMD); a long
// one as --name=value or --name value, in any abbreviation (--e and --ex for --exec; an ambiguous one
// git rejects, so reading it too is harmless). unreadable is set when such a value is not literal.
func optionValues(args []string, lit []bool, opts []cmdOption) (values []string, unreadable bool) {
	next := func(j int, optional bool) int {
		switch {
		case optional || j+1 >= len(args):
		case lit[j+1]:
			values = append(values, args[j+1])
			return j + 1
		default:
			unreadable = true
			return j + 1
		}
		return j
	}
	for j := 0; j < len(args); j++ {
		a := args[j]
		if lit[j] && a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, value, hasEq := strings.Cut(a, "=")
			for _, o := range opts {
				if len(name) < len("--x") || !strings.HasPrefix(o.long, name) {
					continue
				}
				switch {
				case hasEq && lit[j]:
					values = append(values, value)
				case hasEq || !lit[j]:
					unreadable = true
				default:
					j = next(j, o.optional)
				}
				break
			}
			continue
		}
	cluster: // -abc: flags, then perhaps one of opts with its value
		for k := 1; k < len(a); k++ {
			for _, o := range opts {
				if o.short == 0 || a[k] != o.short {
					continue
				}
				switch rest := a[k+1:]; {
				case !lit[j]:
					unreadable = true
				case rest != "":
					values = append(values, rest)
				default:
					j = next(j, o.optional)
				}
				break cluster
			}
		}
	}
	return values, unreadable
}

// gitRunner refuses a push in a command a git subcommand runs: the value of a command-carrying
// option (cmdOptions), bisect run, for-each-repo and submodule foreach. git runs it per step, per
// file or per repository, or to reach the other side of a transport, out of the gate's reach.
func (w *walker) gitRunner(sub string, args []string, lit []bool) {
	how := "git " + sub
	// carries runs probe on a fresh walker; a push or refusal it finds refuses the whole command.
	carries := func(probe func(*walker)) {
		s := newWalker(w.dir, w.src)
		probe(s)
		w.calls = append(w.calls, s.calls...)
		if len(s.pushes) > 0 || s.blocked != "" {
			w.block("a git push through " + how + " cannot be checked (git runs it as a command, per step, file or repository); " + runAlone)
		}
	}
	if opts := cmdOptions[sub]; opts != nil {
		values, unreadable := optionValues(args, lit, opts)
		for _, v := range values {
			carries(func(s *walker) { s.script(v) })
		}
		if unreadable {
			w.block("a command given to " + how + " that the hook cannot read can run a git push; " + runAlone)
		}
	}
	switch sub {
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
			// One word is a script for the shell (`foreach 'git push'`); several are the command
			// and its arguments (`foreach git push origin main`).
			switch {
			case len(rest) == 1 && restLit[0]:
				carries(func(s *walker) { s.script(rest[0]) })
			case !slices.Contains(restLit, false):
				carries(func(s *walker) { s.command(rest, restLit, nil) })
			default:
				w.blockText(w.cmdText, how)
			}
			return
		}
	}
}

// remoteRefWrite reports how a git command writes the remote-tracking ref the gate measures from
// (refs/remotes/origin/main): update-ref or symbolic-ref of a remotes/ ref, a fetch or pull refspec
// into remotes/ (git also reads a short `remotes/origin/main` destination as that ref), or a `git
// remote` subcommand that changes a remote (add, remove, rename, set-url, set-branches), which
// changes what a later fetch brings. git fast-import writes any ref, and git replace makes a commit
// read as another (the gate's scripts ignore replace refs, but git's other commands do not).
func remoteRefWrite(sub string, args []string, lit []bool) string {
	if sub == "fast-import" || sub == "replace" {
		return "git " + sub
	}
	if sub == "symbolic-ref" && symbolicRefReads(args, lit) {
		return "" // it prints the ref (git symbolic-ref --short refs/remotes/origin/HEAD)
	}
	for i, a := range args {
		if !lit[i] {
			continue
		}
		_, dst, isRefspec := strings.Cut(a, ":")
		switch {
		// remotes/ in any case: on a case-insensitive file system (macOS) refs/Remotes/… is that ref.
		case (sub == "update-ref" || sub == "symbolic-ref") && strings.Contains(strings.ToLower(a), "remotes/"):
			return "git " + sub + " " + a
		case (sub == "update-ref" || sub == "symbolic-ref" || sub == "fetch" || sub == "pull") && len(a) >= len("--s") && strings.HasPrefix("--stdin", a):
			return "git " + sub + " " + a // refs or refspecs read from standard input, in any abbreviation git accepts
		case (sub == "fetch" || sub == "pull") && isRefspec && strings.Contains(strings.ToLower(dst), "remotes/"):
			return "git " + sub + " " + a
		case sub == "remote" && (a == "add" || a == "remove" || a == "rm" || a == "rename" || a == "set-url" || a == "set-branches"):
			return "git remote " + a
		}
	}
	return ""
}

// symbolicRefReads reports whether `git symbolic-ref ARGS` only reads a ref: one name, no -d or
// --delete, and only the options of a read (-q, --short, --recurse, --no-recurse, -m and its value).
// Anything else, a word that is not literal included, may write.
func symbolicRefReads(args []string, lit []bool) bool {
	names := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !lit[i]:
			return false
		case a == "--":
			names += len(args) - i - 1
			i = len(args)
		case a == "-m":
			i++ // its value
		case a == "-q" || a == "--quiet" || a == "--short" || a == "--recurse" || a == "--no-recurse" || strings.HasPrefix(a, "-m"):
		case strings.HasPrefix(a, "-"):
			return false
		default:
			names++
		}
	}
	return names == 1
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

// configCommands are the configuration keys (lower case) whose value is an editor or pager git runs:
// `git -c` may set them as their environment variables may be set (gitEnvCommands, safeCommand).
var configCommands = map[string]bool{"core.editor": true, "core.pager": true, "sequence.editor": true}

// configValue returns the value of a `git -c` KEY=VALUE; a key alone is a boolean true.
func configValue(kv string) string {
	if _, v, ok := strings.Cut(kv, "="); ok {
		return v
	}
	return "true"
}

func refuseConfig(key string) string {
	if key == "" {
		key = "(edit)"
	}
	return "git configuration " + key + " in a command is not on the hook's list of safe settings (it could change git's hooks, aliases or repository, or run a command); change it in a command of its own, by hand, or use an option of the git command"
}

// configSubcommands are `git config`'s subcommands (git 2.46 and later), named by its first positional.
var configSubcommands = map[string]bool{
	"list": true, "get": true, "set": true, "unset": true, "rename-section": true, "remove-section": true, "edit": true,
}

// gitConfig refuses a `git config` that writes a key outside the safe list. A read is allowed, and
// only the key (not its value) needs to be literal. For --rename-section both section names count.
func (w *walker) gitConfig(args []string, lit []bool) {
	type pos struct {
		text string
		lit  bool
	}
	var positional []pos
	write, rename, stop := false, false, false
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] || stop || !strings.HasPrefix(a, "-") {
			positional = append(positional, pos{a, lit[j]})
			// Without a subcommand, git reads no option after the first positional: `git config KEY
			// VALUE --get` sets KEY (--get is then the value pattern).
			stop = !positional[0].lit || !configSubcommands[positional[0].text]
			continue
		}
		switch a {
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--get-color", "--get-colorbool", "-l", "--list", "--name-only":
			return // a read
		case "-e", "--edit":
			w.block(refuseConfig(""))
			return
		case "--rename-section", "--remove-section":
			write, rename = true, true
		case "--add", "--replace-all", "--unset", "--unset-all":
			write = true
		case "-f", "--file", "--blob", "--type", "-t", "--default", "--comment", "--value":
			j++
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

// pushArgs reads the arguments after `git push`. skip says why the push may skip git's pre-push
// hook (--no-verify, an abbreviation of it, or an argument that is not literal and could be one),
// which refuses it wherever it runs. ok is false when git would not push (-h, --help).
func pushArgs(dir string, args []string, lit []bool) (p push, skip string, ok bool) {
	p = push{Dir: dir}
	refuse := func(msg string) {
		if p.Refused == "" {
			p.Refused = msg
		}
	}
	var deleteFlag, deleteRefspec, noVerify, nonLiteral bool
	abbrev := ""
	every := map[string]bool{} // --all, --branches, --mirror, --tags: the last of each wins
	positional := 0
	options := true
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			nonLiteral = true
			continue
		}
		switch {
		case options && a == "--":
			options = false
			continue
		case options && (a == "--help" || strings.HasPrefix(a, "--git-completion-helper")):
			return push{}, "", false
		case options && strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			kind, known := pushLong[name]
			on := true
			if base := "--" + strings.TrimPrefix(name, "--no-"); !known && strings.HasPrefix(name, "--no-") && !noNegation[base] {
				if _, ok := pushLong[base]; ok {
					name, kind, on, known = base, optFlag, false, true
				}
			}
			if !known && skipsHook(name) {
				abbrev = name
				continue
			}
			if !known {
				refuse("unknown or abbreviated git push option " + name + "; spell options out in full")
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
					return push{}, "", false
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
		if i := strings.LastIndex(src, ":"); i >= 0 {
			src = src[:i] // git splits a refspec at its last colon (`:/fix:dst` pushes the commit `:/fix`)
		}
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
	switch {
	case noVerify:
		skip = "--no-verify skips the pre-push hook; push without it"
	case abbrev != "":
		skip = abbrev + " is git's abbreviation of --no-verify, which skips the pre-push hook; push without it"
	case nonLiteral:
		skip = "`git push` has an argument that is not literal: it could be --no-verify, or a ref the gate cannot resolve; " + runAlone
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
	return p, skip, true
}
