package main

import (
	"bytes"
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
	Sources []string // pushed commits: refspec sources, HEAD when there is none; "" for a non-literal refspec
	Deletes bool     // --delete, -d or a ":dst" refspec (with --delete, the named refs are deleted, not pushed)
	DryRun  bool
	Refused string // a form the gate cannot check
}

// analysis is what findPushes learned about a command.
type analysis struct {
	Pushes []push
	// Hidden names a git push the walker cannot follow (inside $(...), eval, a wrapper such as
	// timeout or sudo, a non-literal script or command word); such a command is refused.
	Hidden string
}

const runAlone = "run `git push` as a command of its own, with literal arguments"

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
	w := walker{dir: cwd, seen: map[*syntax.CallExpr]bool{}}
	w.stmts(f.Stmts)
	// Calls the statement walk does not reach: command and process substitutions, declarations,
	// test and arithmetic clauses. A git push there is refused.
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && !w.seen[c] && w.hidden == "" {
			probe := walker{seen: map[*syntax.CallExpr]bool{}}
			probe.call(c)
			if len(probe.pushes) > 0 || probe.hidden != "" {
				w.hidden = "a git push inside a command or process substitution cannot be checked; " + runAlone
			}
		}
		return true
	})
	return analysis{Pushes: w.pushes, Hidden: w.hidden}, nil
}

type walker struct {
	dir    string // current directory; "" once it is unknown
	pushes []push
	hidden string
	seen   map[*syntax.CallExpr]bool
}

func (w *walker) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		w.cmd(s.Cmd)
	}
}

func (w *walker) cmd(c syntax.Command) {
	switch c := c.(type) {
	case *syntax.CallExpr:
		w.seen[c] = true
		w.call(c)
	case *syntax.BinaryCmd: // &&, ||, |
		w.cmd(c.X.Cmd)
		w.cmd(c.Y.Cmd)
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
		if c.Stmt != nil {
			w.cmd(c.Stmt.Cmd)
		}
	case *syntax.FuncDecl:
		// A function body runs only when called; a push in it is checked as if it ran here.
		if c.Body != nil {
			w.cmd(c.Body.Cmd)
		}
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

// text prints a word as written.
func text(word *syntax.Word) string {
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, word)
	return b.String()
}

// wrappers run the rest of their arguments as a command; a git push behind them is refused.
var wrappers = map[string]bool{"timeout": true, "sudo": true, "nice": true, "xargs": true, "eval": true, "nohup": true,
	"time": true, "watch": true, "flock": true, "parallel": true, "doas": true, "stdbuf": true, "ionice": true, "caffeinate": true}

func (w *walker) call(c *syntax.CallExpr) {
	args := make([]string, len(c.Args))
	lit := make([]bool, len(c.Args))
	for i, a := range c.Args {
		args[i], lit[i] = literal(a)
	}
	gitEnv := false
	for _, a := range c.Assigns {
		if a.Name != nil && strings.HasPrefix(a.Name.Value, "GIT_") {
			gitEnv = true
		}
	}
	// Plain wrappers that run their arguments as the command.
	for len(args) > 0 && lit[0] {
		switch args[0] {
		case "command", "exec", "builtin":
			args, lit = args[1:], lit[1:]
			continue
		case "env":
			args, lit = args[1:], lit[1:]
			for len(args) > 0 && lit[0] && strings.Contains(args[0], "=") {
				if strings.HasPrefix(args[0], "GIT_") {
					gitEnv = true
				}
				args, lit = args[1:], lit[1:]
			}
			if len(args) > 0 && strings.HasPrefix(args[0], "-") {
				w.hideIfPush(args, lit, c, "env with options")
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
		w.hideIfPush(args, lit, c, "a command word that is not literal")
		return
	}
	switch name := path.Base(args[0]); name {
	case "cd", "pushd":
		w.chdir(args[1:], lit[1:])
	case "popd":
		w.dir = ""
	case "bash", "sh", "zsh", "dash", "ksh":
		w.shell(args, lit, c)
	case "git":
		w.git(args[1:], lit[1:], gitEnv)
	default:
		if wrappers[name] {
			w.hideIfPush(args, lit, c, name)
		}
	}
}

// hideIfPush marks the command as hidden when its words mention git and push.
func (w *walker) hideIfPush(args []string, lit []bool, c *syntax.CallExpr, how string) {
	var all strings.Builder
	for _, a := range c.Args {
		all.WriteString(text(a))
		all.WriteByte(' ')
	}
	if mentionsGitPush.MatchString(all.String()) {
		w.hidden = "a git push through " + how + " cannot be checked; " + runAlone
	}
}

// shell handles `bash -c SCRIPT`: a literal script is parsed like the command itself.
func (w *walker) shell(args []string, lit []bool, c *syntax.CallExpr) {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !lit[i] || !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") || !strings.Contains(a, "c") {
			continue
		}
		// The script is the first operand after the options.
		for j := i + 1; j < len(args); j++ {
			if lit[j] && strings.HasPrefix(args[j], "-") {
				continue
			}
			if !lit[j] {
				w.hideIfPush(args, lit, c, "a shell script that is not literal")
				return
			}
			inner, err := findPushes(args[j], w.dir)
			switch {
			case err != nil:
				if mentionsGitPush.MatchString(args[j]) {
					w.hidden = "cannot parse the shell script around git push; " + runAlone
				}
			case inner.Hidden != "":
				w.hidden = inner.Hidden
			default:
				w.pushes = append(w.pushes, inner.Pushes...)
			}
			return
		}
		return
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
var gitGlobalWithValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true, "--config-env": true}

// pushWithValue lists `git push` options that take the next argument as their value.
var pushWithValue = map[string]bool{"-o": true, "--push-option": true, "--repo": true, "--receive-pack": true, "--exec": true}

func (w *walker) git(args []string, lit []bool, gitEnv bool) {
	dir := w.dir
	refused := ""
	if gitEnv {
		refused = "GIT_* environment variables can point git at another repository or configuration; push without them"
	}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !lit[i] {
			// A global option that is not literal: if push follows, refuse.
			for j := i + 1; j < len(args); j++ {
				if lit[j] && args[j] == "push" {
					w.hidden = "a git option before push that is not literal cannot be checked; " + runAlone
					return
				}
			}
			return
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		name, _, hasValue := strings.Cut(a, "=")
		switch name {
		case "--git-dir", "--work-tree", "--namespace", "--config-env":
			refused = name + " can point git at another repository or configuration; push from the work tree itself"
		}
		if !gitGlobalWithValue[name] || hasValue {
			continue
		}
		if i+1 >= len(args) {
			break
		}
		value := args[i+1]
		switch {
		case name == "-C":
			if lit[i+1] {
				if dir != "" || filepath.IsAbs(value) {
					dir = (&walker{dir: dir}).resolve(value)
				}
			} else {
				dir = ""
			}
		case name == "-c" && strings.Contains(strings.ToLower(value), "hookspath"):
			refused = "-c core.hooksPath would skip the pre-push hook"
		}
		i++
	}
	if i >= len(args) || !lit[i] || args[i] != "push" {
		return
	}
	p := push{Dir: dir, Refused: refused}
	positional := 0
	deleteFlag := false
	for j := i + 1; j < len(args); j++ {
		a := args[j]
		if !lit[j] {
			if positional >= 1 {
				p.Sources = append(p.Sources, "")
			}
			positional++
			continue
		}
		switch {
		case a == "--":
			continue
		case a == "--delete" || a == "-d":
			p.Deletes, deleteFlag = true, true
		case a == "--dry-run" || a == "-n":
			p.DryRun = true
		case a == "--no-verify":
			p.Refused = "--no-verify skips the pre-push hook"
		case a == "--all" || a == "--branches" || a == "--mirror" || a == "--tags":
			p.Refused = a + " pushes refs the gate cannot check one by one; push branches by name"
		case pushWithValue[a]:
			j++
		case strings.HasPrefix(a, "-"):
		default:
			positional++
			if positional == 1 {
				continue // the remote
			}
			src := strings.TrimPrefix(a, "+")
			if src == ":" {
				p.Refused = "the matching refspec ':' pushes refs the gate cannot check one by one; push branches by name"
				continue
			}
			src, _, _ = strings.Cut(src, ":")
			if src == "" {
				p.Deletes = true // ":dst"
				continue
			}
			p.Sources = append(p.Sources, src)
		}
	}
	switch {
	case deleteFlag:
		p.Sources = nil
	case len(p.Sources) == 0 && !p.Deletes:
		p.Sources = []string{"HEAD"}
	}
	w.pushes = append(w.pushes, p)
}
