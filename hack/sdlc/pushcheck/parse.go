package main

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// push is one `git push` the command would run.
type push struct {
	Dir     string   // directory it runs in; "" when it comes from a variable
	Sources []string // pushed commits: refspec sources, HEAD when there is none; "" for a non-literal refspec
	Deletes bool     // --delete, -d or a ":dst" refspec (with --delete, the named refs are deleted, not pushed)
	DryRun  bool
	Refused string // a form the gate cannot check (--all, --mirror, --tags)
}

// findPushes parses command as bash and returns the git pushes it runs, starting in cwd.
func findPushes(command, cwd string) ([]push, error) {
	if !strings.Contains(command, "push") {
		return nil, nil
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, err
	}
	w := walker{dir: cwd}
	w.stmts(f.Stmts)
	return w.pushes, nil
}

type walker struct {
	dir    string // current directory; "" once it is unknown
	pushes []push
}

func (w *walker) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		w.cmd(s.Cmd)
	}
}

func (w *walker) cmd(c syntax.Command) {
	switch c := c.(type) {
	case *syntax.CallExpr:
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
		// A function body runs only when called; a push in it is checked if it is written inline.
		if c.Body != nil {
			w.cmd(c.Body.Cmd)
		}
	}
}

// literal returns the word's value when it is fully literal (quotes removed), ok=false otherwise.
func literal(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
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

func (w *walker) call(c *syntax.CallExpr) {
	args := make([]string, len(c.Args))
	lit := make([]bool, len(c.Args))
	for i, a := range c.Args {
		args[i], lit[i] = literal(a)
	}
	// Wrappers that run their arguments as a command.
	for len(args) > 0 && lit[0] {
		switch args[0] {
		case "command", "exec", "nohup", "builtin":
			args, lit = args[1:], lit[1:]
			continue
		case "env":
			args, lit = args[1:], lit[1:]
			for len(args) > 0 && lit[0] && (strings.Contains(args[0], "=") || strings.HasPrefix(args[0], "-")) {
				args, lit = args[1:], lit[1:]
			}
			continue
		}
		break
	}
	if len(args) == 0 || !lit[0] {
		return
	}
	switch name := path.Base(args[0]); name {
	case "cd", "pushd":
		w.chdir(args[1:], lit[1:])
	case "bash", "sh", "zsh", "dash":
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "-c" || (strings.HasPrefix(args[i], "-") && strings.HasSuffix(args[i], "c") && !strings.HasPrefix(args[i], "--")) {
				if lit[i+1] {
					if inner, err := findPushes(args[i+1], w.dir); err == nil {
						w.pushes = append(w.pushes, inner...)
					}
				}
				break
			}
		}
	case "git":
		w.git(args[1:], lit[1:])
	}
}

func (w *walker) chdir(args []string, lit []bool) {
	if len(args) == 0 {
		w.dir = "" // cd to $HOME: not this repository's business, but unknown here
		return
	}
	if !lit[0] || args[0] == "-" {
		w.dir = ""
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

func (w *walker) git(args []string, lit []bool) {
	dir := w.dir
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		if gitGlobalWithValue[a] && i+1 < len(args) {
			if a == "-C" {
				if lit[i+1] {
					if dir != "" || filepath.IsAbs(args[i+1]) {
						dir = (&walker{dir: dir}).resolve(args[i+1])
					}
				} else {
					dir = ""
				}
			}
			i++
		}
	}
	if i >= len(args) || !lit[i] || args[i] != "push" {
		return
	}
	p := push{Dir: dir}
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
