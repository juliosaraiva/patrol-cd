package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

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

// analysis is what findPushes learned about a command.
type analysis struct {
	Pushes []push
	// Hidden says why the whole command is refused, wherever it runs: a git push the walker cannot
	// follow (inside $(...), eval, a wrapper such as timeout or sudo, a script piped to a shell, a
	// non-literal script, command word or git option), or something next to a push that points git at
	// another repository or changes its hooks (GIT_* variables, --git-dir, -c alias.*, git config of
	// core.hooksPath or an alias).
	Hidden string
}

const runAlone = "run `git push` as a command of its own, with literal arguments"

const gitEnvRefusal = "GIT_* variables can point git at another repository or configuration, where the pre-push hook does not run; push without them"

// mentionsGitPush is the textual test used when the command cannot be parsed.
var mentionsGitPush = regexp.MustCompile(`git[\s\S]*push`)

// findPushes parses command as bash and returns the git pushes it runs, starting in cwd.
func findPushes(command, cwd string) (analysis, error) {
	if !strings.Contains(command, "push") {
		return analysis{}, nil
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return analysis{}, err
	}
	w := walker{dir: cwd, src: command, seen: map[*syntax.CallExpr]bool{}}
	w.stmts(f.Stmts)
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CallExpr:
			// Calls the statement walk does not reach: command and process substitutions, declarations,
			// test and arithmetic clauses. A git push there is refused.
			if !w.seen[n] && w.hidden == "" {
				probe := walker{src: command, seen: map[*syntax.CallExpr]bool{}}
				probe.call(n, nil)
				if len(probe.pushes) > 0 || probe.hidden != "" {
					w.hidden = "a git push inside a command or process substitution cannot be checked; " + runAlone
				}
				w.gitEnv = w.gitEnv || probe.gitEnv
			}
		case *syntax.Assign:
			// Any GIT_* assignment, exported or not: `export GIT_DIR=…`, `declare -x`, `set -a`, a prefix.
			if n.Name != nil && strings.HasPrefix(n.Name.Value, "GIT_") {
				w.gitEnv = true
			}
		}
		return true
	})
	if w.gitEnv && len(w.pushes) > 0 && w.hidden == "" {
		w.hidden = gitEnvRefusal
	}
	return analysis{Pushes: w.pushes, Hidden: w.hidden}, nil
}

type walker struct {
	dir    string // current directory; "" once it is unknown
	src    string // the whole command
	pushes []push
	hidden string
	gitEnv bool // a GIT_* variable is set somewhere in the command
	seen   map[*syntax.CallExpr]bool
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
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		w.seen[c] = true
		w.call(c, s.Redirs)
	case *syntax.BinaryCmd: // &&, ||, |
		w.stmt(c.X)
		w.stmt(c.Y)
	case *syntax.Subshell: // ( ... ): a cd inside does not leak out
		saved := w.dir
		w.stmts(c.Stmts)
		w.dir = saved
	case *syntax.Block:
		w.stmts(c.Stmts)
	case *syntax.IfClause:
		for ; c != nil; c = c.Else {
			w.stmts(c.Cond)
			w.stmts(c.Then)
		}
	case *syntax.WhileClause:
		w.stmts(c.Cond)
		w.stmts(c.Do)
	case *syntax.ForClause:
		w.stmts(c.Do)
	case *syntax.CaseClause:
		for _, it := range c.Items {
			w.stmts(it.Stmts)
		}
	case *syntax.TimeClause:
		w.stmt(c.Stmt)
	case *syntax.FuncDecl:
		// A function body runs only when called; a push in it is checked as if it ran here.
		w.stmt(c.Body)
	}
}

// literal returns the word's value when it is fully literal (quotes removed), ok=false otherwise.
// A backslash escape outside quotes counts as not literal: its value is not the text.
func literal(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.Contains(p.Value, `\`) {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false // $'...' has escapes
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok || strings.Contains(lit.Value, `\`) {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// text prints words as written.
func text(words ...*syntax.Word) string {
	var b bytes.Buffer
	for _, w := range words {
		_ = syntax.NewPrinter().Print(&b, w)
		b.WriteByte(' ')
	}
	return b.String()
}

// wrappers run a command given in their arguments, possibly as one string; a git push behind them
// is refused.
var wrappers = map[string]bool{"timeout": true, "sudo": true, "nice": true, "xargs": true, "eval": true, "nohup": true,
	"time": true, "watch": true, "flock": true, "parallel": true, "doas": true, "stdbuf": true, "ionice": true, "caffeinate": true}

func (w *walker) call(c *syntax.CallExpr, redirs []*syntax.Redirect) {
	args := make([]string, len(c.Args))
	lit := make([]bool, len(c.Args))
	for i, a := range c.Args {
		args[i], lit[i] = literal(a)
	}
	// Plain wrappers that run their arguments as the command.
	for len(args) > 0 && lit[0] {
		switch name := path.Base(args[0]); name {
		case "command", "exec", "builtin":
			if len(args) > 1 && lit[1] && strings.HasPrefix(args[1], "-") {
				w.hideIfPush(text(c.Args...), name+" with options")
				return
			}
			args, lit = args[1:], lit[1:]
			continue
		case "env":
			args, lit = args[1:], lit[1:]
			for len(args) > 0 && lit[0] && !strings.HasPrefix(args[0], "-") && strings.Contains(args[0], "=") {
				if strings.HasPrefix(args[0], "GIT_") {
					w.gitEnv = true
				}
				args, lit = args[1:], lit[1:]
			}
			if len(args) > 0 && lit[0] && strings.HasPrefix(args[0], "-") {
				w.hideIfPush(text(c.Args...), "env with options")
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
				w.hidden = "a git push through a command word that is not literal cannot be checked; " + runAlone
				return
			}
		}
		w.hideIfPush(text(c.Args...), "a command word that is not literal")
		return
	}
	switch name := path.Base(args[0]); name {
	case "cd", "pushd":
		w.chdir(args[1:], lit[1:])
	case "popd":
		w.dir = ""
	case "bash", "sh", "zsh", "dash", "ksh":
		w.shell(args, lit, c, redirs)
	case "git":
		w.git(args[1:], lit[1:])
	case "echo", "printf":
		// They print their arguments.
	default:
		if wrappers[name] {
			w.hideIfPush(text(c.Args...), name)
			return
		}
		// Any other command with the words git and push among its arguments runs them (setsid, chronic,
		// find -exec, ...).
		for i := 1; i < len(args); i++ {
			if !lit[i] || path.Base(args[i]) != "git" {
				continue
			}
			for j := i + 1; j < len(args); j++ {
				if lit[j] && args[j] == "push" {
					w.hidden = "a git push run through " + name + " cannot be checked; " + runAlone
					return
				}
			}
		}
	}
}

// hideIfPush marks the command as hidden when text mentions git and push.
func (w *walker) hideIfPush(text, how string) {
	if mentionsGitPush.MatchString(text) {
		w.hidden = "a git push through " + how + " cannot be checked; " + runAlone
	}
}

// shellWithValue lists shell options that take the next argument as their value.
var shellWithValue = map[string]bool{"-o": true, "+o": true, "-O": true, "+O": true, "--rcfile": true, "--init-file": true}

// shell handles a shell that runs a script: `bash -c SCRIPT`, or a heredoc or here-string on its
// standard input. A literal script is parsed like the command itself.
func (w *walker) shell(args []string, lit []bool, c *syntax.CallExpr, redirs []*syntax.Redirect) {
	operand := false // a script file, which git's own hook covers
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !lit[i] {
			if w.hideIfPush(text(c.Args...), "a shell argument that is not literal"); w.hidden != "" {
				return
			}
			operand = true
			continue
		}
		switch {
		case shellWithValue[a]:
			i++
		case a == "--":
			operand = operand || i+1 < len(args)
			i = len(args)
		case a == "-":
			// the script comes from standard input
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c"):
			// The script is the first operand after the options.
			for j := i + 1; j < len(args); j++ {
				if lit[j] && shellWithValue[args[j]] {
					j++
					continue
				}
				if lit[j] && len(args[j]) > 1 && (args[j][0] == '-' || args[j][0] == '+') {
					continue
				}
				if !lit[j] {
					w.hideIfPush(text(c.Args...), "a shell script that is not literal")
					return
				}
				w.script(args[j])
				return
			}
			return
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			// another option
		default:
			operand = true
		}
	}
	if operand {
		return
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
		if s, ok := literal(body); ok {
			w.script(s)
		} else {
			w.hideIfPush(text(body), "a shell script that is not literal")
		}
		return
	}
	// The script comes from a pipe or a file the hook does not read.
	w.hideIfPush(w.src, "a script piped to a shell")
}

// script parses the text a shell runs as a script.
func (w *walker) script(s string) {
	inner, err := findPushes(s, w.dir)
	switch {
	case err != nil:
		if mentionsGitPush.MatchString(s) {
			w.hidden = "cannot parse the shell script around git push; " + runAlone
		}
	case inner.Hidden != "":
		w.hidden = inner.Hidden
	default:
		w.pushes = append(w.pushes, inner.Pushes...)
	}
}

func (w *walker) chdir(args []string, lit []bool) {
	for len(args) > 0 && lit[0] && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		args, lit = args[1:], lit[1:] // -L, -P, -e, -@
	}
	if len(args) == 0 || !lit[0] || args[0] == "-" {
		w.dir = "" // $HOME, OLDPWD or a variable: unknown here
		return
	}
	w.dir = w.resolve(args[0])
}

func (w *walker) resolve(dir string) string {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if strings.HasPrefix(dir, "~") {
		return "" // ~user: unknown
	}
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

func (w *walker) git(args []string, lit []bool) {
	dir := w.dir
	refused, unsafe := "", ""
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !lit[i] {
			w.hidden = "a git option or subcommand that is not literal, in a command that mentions push, cannot be checked; " + runAlone
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
			if !lit[i] {
				w.hidden = "a git option or subcommand that is not literal, in a command that mentions push, cannot be checked; " + runAlone
				return
			}
			value = args[i]
		}
		switch name {
		case "-C":
			if dir != "" || filepath.IsAbs(value) {
				dir = (&walker{dir: dir}).resolve(value)
			}
		case "-c":
			v := strings.ToLower(value)
			if strings.HasPrefix(v, "alias.") {
				w.hidden = "-c alias.… defines a git alias whose push the hook cannot follow; " + runAlone
				return
			}
			if strings.Contains(v, "hookspath") {
				refused = "-c core.hooksPath would skip the pre-push hook"
			}
		}
	}
	if i >= len(args) {
		return
	}
	switch args[i] {
	case "push":
	case "config":
		for j := i + 1; j < len(args); j++ {
			v := strings.ToLower(args[j])
			if lit[j] && (strings.Contains(v, "hookspath") || strings.HasPrefix(v, "alias.")) {
				w.hidden = "a command that mentions push may not change core.hooksPath or a git alias; change git's configuration in a command of its own, and not to skip the pre-push hook"
				return
			}
		}
		return
	default:
		return
	}
	if unsafe != "" {
		w.hidden = "git " + unsafe + " can point git at another repository or configuration, where the pre-push hook does not run; push from the work tree without it"
		return
	}
	if p, ok := pushArgs(dir, refused, args[i+1:], lit[i+1:]); ok {
		w.pushes = append(w.pushes, p)
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
func pushArgs(dir, refused string, args []string, lit []bool) (push, bool) {
	p := push{Dir: dir, Refused: refused}
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
					refuse(fmt.Sprintf("unknown git push option -%c; spell options out in full", a[k]))
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
