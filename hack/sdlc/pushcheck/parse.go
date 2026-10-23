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
	// hooks or aliases, or run a command the walker does not see.
	Blocked string
}

const runAlone = "run `git push` as a command of its own, with literal arguments"

// normalize removes the quotes and backslashes bash removes, for the textual tests.
func normalize(s string) string {
	return strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(s)
}

var gitPushText = regexp.MustCompile(`git[\s\S]*push`)

// mentionsGitPush is the textual test for text the walker cannot parse or follow.
func mentionsGitPush(s string) bool { return gitPushText.MatchString(normalize(s)) }

// findPushes parses command as bash and returns what it does with git, starting in cwd.
func findPushes(command, cwd string) (analysis, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return analysis{}, err
	}
	w := newWalker(cwd, command)
	w.stmts(f.Stmts)
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CallExpr:
			// Calls the statement walk does not reach: command and process substitutions, declarations,
			// test and arithmetic clauses. A git push there is refused.
			if !w.seen[n] {
				probe := newWalker("", command)
				probe.call(n, nil)
				if len(probe.pushes) > 0 {
					w.block("a git push inside a command or process substitution cannot be checked; " + runAlone)
				}
				w.block(probe.blocked)
				w.calls = append(w.calls, probe.calls...)
			}
		case *syntax.Assign:
			// Every assignment, exported or not: NAME=…, export, declare -x, local, readonly, a prefix.
			if n.Name != nil {
				w.env(n.Name.Value, n.Value)
			}
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

// blockIfPush blocks the command when text mentions git and push.
func (w *walker) blockIfPush(text, how string) {
	if mentionsGitPush(text) {
		w.block("a git push through " + how + " cannot be checked; " + runAlone)
	}
}

// safeGitEnv lists the GIT_* variables that change neither the repository nor git's configuration,
// hooks or aliases. Any other GIT_* variable set in the command refuses it (GIT_DIR, GIT_WORK_TREE,
// GIT_CONFIG_*, GIT_EXEC_PATH, ...), since it also reaches a git push in a script the command runs.
var safeGitEnv = map[string]bool{"GIT_TERMINAL_PROMPT": true, "GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true,
	"GIT_AUTHOR_DATE": true, "GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "GIT_COMMITTER_DATE": true,
	"GIT_OPTIONAL_LOCKS": true, "GIT_ADVICE": true, "GIT_MERGE_AUTOEDIT": true, "GIT_FLUSH": true,
	"GIT_PROGRESS_DELAY": true, "GIT_LFS_SKIP_SMUDGE": true}

// gitEnvCommands are the GIT_* variables whose value is a command git runs: allowed with a literal
// value that does not mention git.
var gitEnvCommands = map[string]bool{"GIT_PAGER": true, "GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true}

// env checks an assignment of name; value is nil when it is not known (export NAME, env NAME=…).
func (w *walker) env(name string, value *syntax.Word) {
	if !strings.HasPrefix(name, "GIT_") || safeGitEnv[name] || strings.HasPrefix(name, "GIT_TRACE") {
		return
	}
	if gitEnvCommands[name] && value != nil {
		if v, ok := literalWord(value); ok && !strings.Contains(v, "git") && !strings.Contains(v, "push") {
			return
		}
	}
	w.block(name + " can point git at another repository or change its configuration, hooks or the commands it runs, also for a push in a script; run the command without it")
}

// envValue checks an assignment written as one NAME=VALUE word (env, read, printf -v ...).
func (w *walker) envValue(assign string) {
	name, value, _ := strings.Cut(assign, "=")
	if gitEnvCommands[name] && !strings.Contains(value, "git") && !strings.Contains(value, "push") {
		return
	}
	w.env(name, nil)
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
		// It runs in a subshell: a cd inside does not reach the commands after it.
		saved := w.dir
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
				w.env(it.Name.Value, nil)
			}
		}
		moves := changesDir(c)
		if moves {
			w.dir = ""
			defer func() { w.dir = "" }()
		}
		syntax.Walk(c, func(n syntax.Node) bool {
			// Each list of statements in the clause, in order; the walk does not descend further, so
			// nested compounds are handled by w.stmts.
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

// changesDir reports whether node runs a command that can change the directory: cd, pushd, popd,
// source, a command word that is not literal.
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

// fields expands literal words into the arguments bash makes of them. lit is false for an
// argument that depends on the environment, the filesystem or another command.
func fields(words []*syntax.Word) (args []string, lit []bool) {
	for _, word := range words {
		fs, ok := expandLiteral(word)
		if !ok {
			args, lit = append(args, ""), append(lit, false)
			continue
		}
		for _, f := range fs {
			args, lit = append(args, f), append(lit, true)
		}
	}
	return args, lit
}

var home, _ = os.UserHomeDir()

// expandLiteral returns the fields bash makes of word when they do not depend on the environment,
// the filesystem or another command: quote removal, escapes, $'…', brace expansion and ~ (the
// user's home). ok is false for a word with a parameter, command, arithmetic or process
// substitution, or an unquoted glob.
func expandLiteral(word *syntax.Word) ([]string, bool) {
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

// text prints words as written, followed by their literal fields.
func text(words ...*syntax.Word) string {
	var b bytes.Buffer
	for _, w := range words {
		_ = syntax.NewPrinter().Print(&b, w)
		b.WriteByte(' ')
	}
	args, _ := fields(words)
	b.WriteString(strings.Join(args, " "))
	return b.String()
}

var (
	// shells run a script given with -c, on standard input or in a file.
	shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "mksh": true}
	// wrappers run a command given in their arguments, possibly as one string.
	wrappers = map[string]bool{"timeout": true, "sudo": true, "nice": true, "eval": true, "nohup": true, "time": true,
		"watch": true, "flock": true, "doas": true, "stdbuf": true, "ionice": true, "caffeinate": true, "busybox": true,
		"trap": true, "alias": true, "script": true, "unbuffer": true, "chronic": true, "setsid": true, "su": true}
	// stdinRunners build a command from their standard input too.
	stdinRunners = map[string]bool{"xargs": true, "parallel": true}
	// interpreters run code given in their arguments.
	interpreters = map[string]bool{"python": true, "python3": true, "perl": true, "ruby": true, "node": true, "deno": true,
		"bun": true, "php": true, "lua": true, "osascript": true, "awk": true, "gawk": true, "expect": true, "Rscript": true}
	// envSetters set variables named in their arguments.
	envSetters = map[string]bool{"read": true, "printf": true, "mapfile": true, "readarray": true, "getopts": true,
		"declare": true, "typeset": true, "export": true, "local": true, "readonly": true, "let": true}
)

var gitEnvName = regexp.MustCompile(`GIT_[A-Za-z0-9_]+(=[^ ]*)?`)

func (w *walker) call(c *syntax.CallExpr, redirs []*syntax.Redirect) {
	args, lit := fields(c.Args)
	// Plain wrappers that run their arguments as the command.
	for len(args) > 0 && lit[0] {
		switch name := path.Base(args[0]); name {
		case "command", "exec", "builtin":
			if len(args) > 1 && lit[1] && strings.HasPrefix(args[1], "-") {
				w.blockIfPush(text(c.Args...), name+" with options")
				return
			}
			args, lit = args[1:], lit[1:]
			continue
		case "env":
			args, lit = args[1:], lit[1:]
			for len(args) > 0 && lit[0] && !strings.HasPrefix(args[0], "-") && strings.Contains(args[0], "=") {
				w.envValue(args[0])
				args, lit = args[1:], lit[1:]
			}
			if len(args) > 0 && lit[0] && strings.HasPrefix(args[0], "-") {
				w.blockIfPush(w.src, "env with options")
				return
			}
			continue
		}
		break
	}
	if len(args) == 0 {
		return
	}
	if !lit[0] {
		// "$GIT" push ...: the command is unknown, but it pushes something.
		for i := 1; i < len(args); i++ {
			if lit[i] && args[i] == "push" {
				w.block("a git push through a command word that is not literal cannot be checked; " + runAlone)
				return
			}
		}
		w.blockIfPush(text(c.Args...), "a command word that is not literal")
		return
	}
	name := path.Base(args[0])
	if envSetters[name] {
		for i := 1; i < len(args); i++ {
			for _, m := range gitEnvName.FindAllString(args[i], -1) {
				w.envValue(m)
			}
		}
	}
	switch {
	case name == "cd" || name == "pushd":
		w.chdir(name, args[1:], lit[1:])
	case name == "popd":
		w.dir = ""
	case name == "source" || name == ".":
		w.dir, w.sourced = "", true
	case name == "fish" || name == "csh" || name == "tcsh":
		// Their options and syntax are not bash's (fish -C runs a command too).
		w.blockIfPush(w.src, name)
	case shells[name]:
		w.shell(args, lit, c, redirs)
	case name == "git":
		w.git(args[1:], lit[1:], c)
	case name == "echo" || name == "printf":
		// They print their arguments.
	case wrappers[name] || interpreters[name]:
		w.blockIfPush(text(c.Args...), name)
	case stdinRunners[name]:
		// The command can come from standard input: refuse when the command line mentions both.
		if strings.Contains(normalize(text(c.Args...)), "git") && strings.Contains(normalize(w.src), "push") {
			w.block("a git push through " + name + " cannot be checked; " + runAlone)
		}
	default:
		// Any other command with the words git and push among its arguments runs them (find -exec, ...).
		for i := 1; i < len(args); i++ {
			if !lit[i] || path.Base(args[i]) != "git" {
				continue
			}
			for j := i + 1; j < len(args); j++ {
				if lit[j] && args[j] == "push" {
					w.block("a git push run through " + name + " cannot be checked; " + runAlone)
					return
				}
			}
		}
	}
}

// shellWithValue lists shell options that take the next argument as their value.
var shellWithValue = map[string]bool{"--rcfile": true, "--init-file": true}

// shell handles a shell that runs a script: `bash -c SCRIPT`, or a heredoc or here-string on its
// standard input. A literal script is parsed like the command itself; a script file is not read
// (git's own hook checks what it pushes).
func (w *walker) shell(args []string, lit []bool, c *syntax.CallExpr, redirs []*syntax.Redirect) {
	command, stdin := false, false
	i := 1
options:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case !lit[i]:
			// An option or an operand: unknown.
			w.blockIfPush(w.src, "a shell argument that is not literal")
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
			w.blockIfPush(w.src, "a shell script that is not literal")
			return
		}
		w.script(operands[0])
		return
	case !stdin && len(operands) > 0 && !lit[i]:
		w.blockIfPush(w.src, "a shell script that is not literal")
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
			w.blockIfPush(w.src, "a shell script that is not literal")
		}
		return
	}
	// The script comes from a pipe or a file the hook does not read.
	w.blockIfPush(w.src, "a script piped to a shell")
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
		// An unquoted delimiter: bash still removes the backslash of \\, \$ and \` in the body.
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
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if w.dir == "" {
		return ""
	}
	return filepath.Join(w.dir, dir)
}

// gitGlobalWithValue lists git's global options that take the next argument as their value.
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true, "--super-prefix": true, "--attr-source": true, "--list-cmds": true}

// gitGlobalSafe lists git's global options that change neither the repository nor its hooks; -c is
// checked on its own. Any other global option (--git-dir, --work-tree, --bare, --config-env, ...)
// before push refuses the command.
var gitGlobalSafe = map[string]bool{"-C": true, "-c": true, "-p": true, "--paginate": true, "-P": true, "--no-pager": true,
	"--no-replace-objects": true, "--literal-pathspecs": true, "--glob-pathspecs": true, "--noglob-pathspecs": true,
	"--icase-pathspecs": true, "--no-optional-locks": true, "--no-advice": true, "--no-lazy-fetch": true}

// safeConfig lists the configuration keys (lower case) that `git -c` and `git config` may set in a
// command: none of them runs a command or changes the repository, git's hooks or aliases. Any
// other key refuses the command (core.hooksPath, alias.*, include.*, core.pager, ...).
var (
	safeConfigSections = []string{"color.", "advice.", "user.", "i18n.", "log.", "format.", "column.", "status.", "pretty.", "blame."}
	safeConfigKeys     = map[string]bool{"core.quotepath": true, "core.abbrev": true, "core.longpaths": true, "core.filemode": true,
		"core.ignorecase": true, "init.defaultbranch": true, "safe.directory": true, "push.default": true,
		"push.autosetupremote": true, "pull.rebase": true, "pull.ff": true, "fetch.prune": true, "diff.renames": true,
		"diff.algorithm": true, "diff.noprefix": true, "merge.conflictstyle": true, "rebase.autosquash": true,
		"rebase.autostash": true, "gc.auto": true, "maintenance.auto": true, "protocol.version": true, "http.version": true,
		"protocol.file.allow": true, "commit.gpgsign": true, "tag.gpgsign": true, "feature.manyfiles": true,
		"index.threads": true, "pack.threads": true, "checkout.workers": true}
)

func safeConfigKey(key string) bool {
	key = strings.ToLower(key)
	if safeConfigKeys[key] {
		return true
	}
	for _, s := range safeConfigSections {
		if strings.HasPrefix(key, s) {
			return true
		}
	}
	return false
}

func refuseConfig(key string) string {
	return "git configuration " + key + " in a command is not on the hook's list of safe settings (it could change git's hooks, aliases or repository, or run a command); change it in a command of its own, by hand, or use an option of the git command"
}

// gitRunners are git subcommands that can run another command given in their arguments.
var gitRunners = map[string]bool{"rebase": true, "bisect": true, "submodule": true, "for-each-repo": true,
	"filter-branch": true, "difftool": true, "mergetool": true}

func (w *walker) git(args []string, lit []bool, c *syntax.CallExpr) {
	dir := w.dir
	unsafe := ""
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !lit[i] {
			w.block("a git option or subcommand that is not literal cannot be checked; " + runAlone)
			return
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !gitGlobalSafe[name] && unsafe == "" {
			unsafe = name
		}
		if gitGlobalWithValue[name] && !hasValue {
			if i+1 >= len(args) {
				return
			}
			i++
			if !lit[i] && name == "-C" {
				dir = "" // a push there is refused: the directory is unknown
				continue
			}
			if !lit[i] {
				w.block("a git option or subcommand that is not literal cannot be checked; " + runAlone)
				return
			}
			value = args[i]
		}
		switch name {
		case "-C":
			dir = w.resolveFrom(dir, value)
		case "-c":
			key, _, _ := strings.Cut(value, "=")
			if !safeConfigKey(key) {
				w.block("-c " + key + ": " + refuseConfig(key))
				return
			}
		}
	}
	if i >= len(args) {
		return
	}
	sub, rest, restLit := args[i], args[i+1:], lit[i+1:]
	if unsafe != "" {
		dir = "" // the repository is not the directory's
	}
	switch {
	case sub == "push":
	case sub == "config":
		w.gitConfig(rest, restLit)
		return
	case gitRunners[sub]:
		w.blockIfPush(text(c.Args...), "git "+sub)
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

func (w *walker) resolveFrom(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, p)
}

// gitConfig refuses a `git config` that writes a key outside the safe list.
func (w *walker) gitConfig(args []string, lit []bool) {
	var positional []string
	write := false
	for j := 0; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			if len(positional) == 0 {
				w.block("git config with a key that is not literal cannot be checked; change git's configuration in a command of its own")
				return
			}
			positional = append(positional, a)
			continue
		}
		switch {
		case a == "--get" || a == "--get-all" || a == "--get-regexp" || a == "--get-urlmatch" || a == "--get-color" ||
			a == "--get-colorbool" || a == "-l" || a == "--list":
			return // a read
		case a == "--add" || a == "--replace-all" || a == "--unset" || a == "--unset-all" || a == "--rename-section" ||
			a == "--remove-section" || a == "-e" || a == "--edit":
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
	if !write && len(positional) < 2 {
		return // a read of one key
	}
	if len(positional) == 0 {
		w.block(refuseConfig("(edit)"))
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
