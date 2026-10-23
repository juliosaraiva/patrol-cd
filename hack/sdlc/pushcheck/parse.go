package main

import (
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

// gitPushTokens matches a `git push` the walker could not resolve: a literal `git`, any git global
// options, then `push`. It is used only when the command word or a wrapped script is not literal, as
// a last, conservative net; the normal path parses the command.
var gitPushTokens = regexp.MustCompile(`(^|\s)git(\s+-\S+)*\s+push(\s|$)`)

func mentionsGitPush(s string) bool { return gitPushTokens.MatchString(normalize(s)) }

// findPushes parses command as bash and returns what it does with git, starting in cwd.
func findPushes(command, cwd string) (analysis, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return analysis{}, err
	}
	w := newWalker(cwd, command)
	w.stmts(f.Stmts)
	// Calls the statement walk does not reach: command and process substitutions. A git push there is
	// refused; the command cannot capture or depend on it.
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && !w.seen[c] {
			probe := newWalker(cwd, command)
			probe.call(c, nil)
			if len(probe.pushes) > 0 {
				w.block("a git push inside a command or process substitution cannot be checked; " + runAlone)
			}
			w.block(probe.blocked)
			w.calls = append(w.calls, probe.calls...)
		}
		return true
	})
	return analysis{Pushes: w.pushes, Calls: w.calls, Blocked: w.blocked}, nil
}

type walker struct {
	dir     string // current directory; "" once it is unknown
	src     string // the whole command
	pushes  []push
	calls   []gitCall
	blocked string
	sourced bool // a file was sourced: it can change the directory and git's environment
	seen    map[*syntax.CallExpr]bool
}

func newWalker(dir, src string) *walker {
	return &walker{dir: dir, src: src, seen: map[*syntax.CallExpr]bool{}}
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
		before := w.dir
		switch c.Op {
		case syntax.AndStmt: // Y runs only after X succeeded, so it sees X's cd
			w.stmt(c.X)
			after := w.dir
			w.stmt(c.Y)
			if w.dir != after {
				w.dir = ""
			}
		case syntax.OrStmt: // Y runs after X failed, maybe after X's cd
			w.stmt(c.X)
			after := w.dir
			if after != before {
				w.dir = ""
			}
			w.stmt(c.Y)
			if after != before || w.dir != after {
				w.dir = ""
			}
		default: // a pipeline: bash runs each element in a subshell, zsh all but the last
			w.stmt(c.X)
			changed := w.dir != before
			w.dir = before
			w.stmt(c.Y)
			if changed || w.dir != before {
				w.dir = ""
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
	for _, a := range c.Args {
		if a.Name == nil { // an option: -p, -x, -g, ...
			if v, ok := literalWord(a.Value); ok {
				if strings.ContainsAny(strings.TrimPrefix(v, "-"), "pfF") && strings.HasPrefix(v, "-") {
					return // a print or function listing: it sets nothing
				}
				if strings.Contains(v, "x") {
					exportMode = true
				}
			}
			continue
		}
		switch {
		case a.Value != nil || !a.Naked: // declare GIT_DIR=x, or declare GIT_DIR= (empty)
			w.assign(a.Name.Value, a.Value, false)
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
// empty `NAME=` (naked false). A GIT_* assignment that could point git elsewhere is refused.
func (w *walker) assign(name string, value *syntax.Word, naked bool) {
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
			name, ok := literalWord(c.Args[0])
			switch path.Base(name) {
			case "cd", "pushd", "popd", "source", ".":
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
	args, lit := fields(c.Args)
	w.command(args, lit, redirs)
}

// command dispatches an already-expanded argv.
func (w *walker) command(args []string, lit []bool, redirs []*syntax.Redirect) {
prefix:
	for len(args) > 0 && lit[0] {
		switch path.Base(args[0]) {
		case "command", "exec", "builtin":
			// They run the rest as a command. With an option (exec -a NAME, command -p), the grammar
			// varies and -v/-V only print, so fall back to a conservative text check.
			if len(args) > 1 && lit[1] && strings.HasPrefix(args[1], "-") {
				w.blockText(strings.Join(args, " "), path.Base(args[0])+" with options")
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
					args, lit = args[1:], lit[1:]
					break envOpts
				case strings.HasPrefix(a, "-"): // -C DIR, -S STRING, unknown: do not misread the command
					w.blockText(w.src, "env with options")
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
	name := path.Base(args[0])
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
		w.blockText(strings.Join(args, " "), name) // their syntax is not bash's
	case name == "eval":
		w.eval(args[1:], lit[1:])
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
	case name == "find" || name == "fd" || name == "fdfind":
		w.findExec(args, lit)
	}
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
		w.blockText(strings.Join(args, " "), name)
		return
	}
	w.command(inner, innerLit, redirs)
}

// execRunner parses the command run after a `--` (kubectl exec POD -- CMD); without one, a plain
// adjacent git push is refused and anything else is left to git's hook.
func (w *walker) execRunner(args []string, lit []bool) {
	for i := 1; i < len(args); i++ {
		if lit[i] && args[i] == "--" {
			if i+1 < len(args) {
				w.command(args[i+1:], lit[i+1:], nil)
			}
			return
		}
	}
	w.blockText(strings.Join(args, " "), path.Base(args[0]))
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

// eval parses the string eval would run.
func (w *walker) eval(args []string, lit []bool) {
	for _, l := range lit {
		if !l {
			w.blockText(strings.Join(args, " "), "eval")
			return
		}
	}
	w.script(strings.Join(args, " "))
}

// xargs parses the command xargs or parallel builds. It appends standard input to that command, so a
// bare `git` (its subcommand would come from input) is refused, as is a non-literal command word.
func (w *walker) xargs(name string, args []string, lit []bool) {
	value := map[string]bool{"-n": true, "--max-args": true, "-L": true, "-l": true, "-P": true, "--max-procs": true,
		"-s": true, "--max-chars": true, "-I": true, "-i": true, "--replace": true, "-E": true, "-e": true, "--eof": true,
		"-d": true, "--delimiter": true, "-a": true, "--arg-file": true, "-j": true, "-N": true}
	inner, innerLit := skipOptions(args, lit, value, 0)
	if len(inner) == 0 {
		return // the default command is echo
	}
	if lit != nil && len(innerLit) > 0 && !innerLit[0] {
		w.blockText(name+" "+strings.Join(args, " "), name)
		return
	}
	if path.Base(inner[0]) == "git" && len(inner) == 1 {
		w.block("a git push through " + name + " cannot be checked (its subcommand would come from standard input); " + runAlone)
		return
	}
	w.command(inner, innerLit, nil)
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
			w.blockText(w.src, "a shell argument that is not literal")
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
			w.blockText(w.src, "a shell script that is not literal")
			return
		}
		w.script(operands[0])
		return
	case !stdin && len(operands) > 0 && !lit[i]:
		w.blockText(w.src, "a shell script that is not literal")
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
			w.blockText(w.src, "a shell script that is not literal")
		}
		return
	}
	w.blockText(w.src, "a script piped to a shell") // the script comes from a pipe or a file
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

// gitGlobalWithValue lists git's global options that take the next argument as their value.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true, "--super-prefix": true, "--attr-source": true, "--list-cmds": true}

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
	unsafe := ""
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
		switch name {
		case "-C":
			if litVal {
				dir = w.resolveFrom(dir, value)
			} else {
				dir = ""
			}
		case "-c":
			key, _, _ := strings.Cut(value, "=")
			switch {
			case key == "" || (!litVal && !strings.Contains(value, "=")):
				if unsafe == "" {
					unsafe = "-c" // the key cannot be read: refuse only before a push
				}
			case safeConfigKey(key):
			default:
				w.block("-c " + key + ": " + refuseConfig(key))
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
	if unsafe != "" {
		dir = "" // the repository is not the directory's
	}
	switch {
	case sub == "push":
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
	switch sub {
	case "rebase", "difftool", "mergetool", "filter-branch", "for-each-repo":
		for i := 0; i < len(args); i++ {
			if !lit[i] {
				continue
			}
			name, value, hasEq := strings.Cut(args[i], "=")
			forRepo := sub == "for-each-repo" && (name == "-C" || name == "--command")
			if name != "-x" && name != "--exec" && name != "--extcmd" && !forRepo {
				continue
			}
			switch {
			case hasEq:
				carries(func(s *walker) { s.script(value) })
			case i+1 < len(args) && lit[i+1]:
				carries(func(s *walker) { s.script(args[i+1]) })
				i++
			case i+1 < len(args):
				w.blockText(w.src, how)
			}
		}
	case "bisect":
		if len(args) > 0 && lit[0] && args[0] == "run" {
			carries(func(s *walker) { s.command(args[1:], lit[1:], nil) })
		}
	case "submodule":
		for i := 0; i < len(args); i++ {
			if lit[i] && args[i] == "foreach" && i+1 < len(args) {
				if lit[i+1] {
					carries(func(s *walker) { s.script(args[i+1]) })
				} else {
					w.blockText(w.src, how)
				}
				return
			}
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

func safeConfigKey(key string) bool {
	key = strings.ToLower(key)
	if safeConfigKeys[key] {
		return true
	}
	for _, s := range safeConfigSections {
		section := strings.SplitN(key, ".", 2)[0] + "."
		if s == section && key != "core.pager" && key != "core.editor" && key != "core.sshcommand" && key != "core.hookspath" &&
			key != "core.fsmonitor" && key != "diff.external" && key != "merge.tool" {
			return true
		}
	}
	return false
}

func refuseConfig(key string) string {
	if key == "" {
		key = "(edit)"
	}
	return "git configuration " + key + " in a command is not on the hook's list of safe settings (it could change git's hooks, aliases or repository, or run a command); change it in a command of its own, by hand, or use an option of the git command"
}

// gitConfig refuses a `git config` that writes a key outside the safe list. A read is allowed.
func (w *walker) gitConfig(args []string, lit []bool) {
	var positional []string
	nonLiteral := false
	write := false
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			positional = append(positional, "")
			nonLiteral = true
			continue
		}
		switch {
		case a == "--get" || a == "--get-all" || a == "--get-regexp" || a == "--get-urlmatch" || a == "--get-color" ||
			a == "--get-colorbool" || a == "-l" || a == "--list":
			return // a read
		case a == "-e" || a == "--edit":
			w.block(refuseConfig(""))
			return
		case a == "--add" || a == "--replace-all" || a == "--unset" || a == "--unset-all" || a == "--rename-section" ||
			a == "--remove-section":
			write = true
		case a == "-f" || a == "--file" || a == "--blob" || a == "--type" || a == "-t" || a == "--default" || a == "--comment" || a == "--value":
			j++
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) > 0 {
		switch positional[0] {
		case "get", "list":
			return
		case "set", "unset", "rename-section", "remove-section", "edit":
			write = true
			positional = positional[1:]
		}
	}
	if !write && len(positional) < 2 { // a read of one key, or none
		return
	}
	if nonLiteral || len(positional) == 0 {
		w.block("git config writes a key the hook cannot read; change git's configuration in a command of its own")
		return
	}
	if !safeConfigKey(positional[0]) {
		w.block(refuseConfig(positional[0]))
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
		if a == "tag" && j+1 < len(args) && lit[j+1] { // git push origin tag NAME
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
