package cmp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// maxErrorStderr bounds the stderr excerpt included in command errors (they reach the
// repo-server and the UI, as upstream).
const maxErrorStderr = sandbox.MaxStderrTail

// CommandError reports a plugin command that failed. Its message keeps upstream's
// "`<args>` failed <cause>: <stderr>" shape.
type CommandError struct {
	// Args is the command line, arguments with spaces quoted.
	Args string
	// Cause is the sandbox error (wrapping *sandbox.ExitError, sandbox.ErrTimeout,
	// sandbox.ErrOutputLimit or a context error).
	Cause error
	// Stderr is the trimmed end of the command's standard error (at most 4 KiB).
	Stderr string
}

func (e *CommandError) Error() string {
	s := fmt.Sprintf("`%s` failed %s", e.Args, causeText(e.Cause))
	if e.Stderr != "" {
		s += ": " + e.Stderr
	}
	return s
}

func (e *CommandError) Unwrap() error { return e.Cause }

// causeText renders the failure like os/exec does ("exit status 1", "signal:
// killed") and keeps sandbox and context errors readable.
func causeText(err error) string {
	var ee *sandbox.ExitError
	if errors.As(err, &ee) {
		if ee.Signal != 0 {
			return "signal: " + ee.Signal.String()
		}
		return "exit status " + strconv.Itoa(ee.Code)
	}
	switch {
	case errors.Is(err, sandbox.ErrTimeout) && errors.Is(err, context.DeadlineExceeded):
		return "request deadline exceeded"
	case errors.Is(err, sandbox.ErrTimeout):
		return "exec timeout exceeded"
	case errors.Is(err, sandbox.ErrOutputLimit):
		return "output limit exceeded"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	}
	return err.Error()
}

// commandArgs renders argv for logs and errors (upstream GetCommandArgsToLog): empty
// arguments as "" and arguments containing white space quoted.
func commandArgs(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a == "":
			out[i] = `""`
		case strings.IndexFunc(a, unicode.IsSpace) >= 0:
			out[i] = strconv.Quote(a)
		default:
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// runner executes plugin commands.
type runner struct {
	limits    sandbox.Limits
	pluginEnv []string
	// path is the PATH of the plugin environment, used to resolve bare command
	// names (never the request's PATH, as upstream resolves with the sidecar's).
	path string
	log  *slog.Logger
}

// run executes cmd in dir with env (the plugin environment followed by the request
// environment) and returns its stdout without one trailing newline.
func (r *runner) run(ctx context.Context, op string, cmd Command, dir string, reqEnv []string) (string, error) {
	argv := cmd.argv()
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("Command is empty") //nolint:staticcheck // upstream message
	}
	args := commandArgs(argv)
	exe, err := r.resolve(argv[0], dir)
	if err != nil {
		return "", &CommandError{Args: args, Cause: err}
	}
	env := mergeEnv(r.pluginEnv, reqEnv)
	r.log.LogAttrs(ctx, slog.LevelDebug, "running plugin command",
		slog.String("operation", op), slog.String("command", filepath.Base(argv[0])), slog.Int("args", len(argv)-1))
	res, err := sandbox.Run(ctx, sandbox.Cmd{Path: exe, Args: argv[1:], Dir: dir, Env: env, Limits: r.limits})
	attrs := []slog.Attr{
		slog.String("operation", op),
		slog.String("command", filepath.Base(argv[0])),
		slog.Duration("duration", res.Duration),
		slog.Int("stdoutBytes", len(res.Stdout)),
		slog.Int("stderrBytes", len(res.Stderr)),
	}
	if err != nil {
		cerr := &CommandError{Args: args, Cause: err, Stderr: stderrExcerpt(err, res.Stderr)}
		r.log.LogAttrs(ctx, slog.LevelError, "plugin command failed", append(attrs, slog.String("error", causeText(err)))...)
		return "", cerr
	}
	if len(res.Stdout) == 0 {
		r.log.LogAttrs(ctx, slog.LevelWarn, "Plugin command returned zero output", attrs...)
	} else {
		r.log.LogAttrs(ctx, slog.LevelInfo, "Plugin command successful", attrs...)
	}
	if len(res.Stderr) > 0 {
		// Stderr helps plugin authors, but may echo sensitive input: debug only and
		// bounded.
		r.log.LogAttrs(ctx, slog.LevelDebug, "plugin command stderr",
			slog.String("operation", op), slog.String("stderr", tail(res.Stderr, maxErrorStderr)))
	}
	return strings.TrimSuffix(string(res.Stdout), "\n"), nil
}

// resolve turns argv[0] into an absolute executable path: absolute paths are used as
// is, relative paths with a separator are relative to the command's working directory
// (as os/exec with Cmd.Dir), and bare names are looked up in the plugin PATH.
func (r *runner) resolve(name, dir string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return filepath.Join(dir, name), nil
	}
	return lookPath(name, r.path)
}

// lookPath finds an executable regular file named name in the absolute entries of
// pathList.
func lookPath(name, pathList string) (string, error) {
	for _, d := range filepath.SplitList(pathList) {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("exec: %q: %w", name, fs.ErrNotExist)
}

// stderrExcerpt returns the stderr shown in errors: the sandbox's redacted tail for
// exits, otherwise the end of the captured stream.
func stderrExcerpt(err error, stderr []byte) string {
	var ee *sandbox.ExitError
	if errors.As(err, &ee) {
		return strings.TrimSpace(ee.StderrTail)
	}
	return strings.TrimSpace(tail(stderr, maxErrorStderr))
}

// tail returns at most n bytes from the end of b as valid UTF-8.
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

// mergeEnv returns base followed by extra with one entry per name: a later entry
// replaces an earlier one in place (the "later wins" rule of exec). Entries without
// '=' or with an empty name are dropped.
func mergeEnv(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	index := make(map[string]int, len(base)+len(extra))
	for _, list := range [][]string{base, extra} {
		for _, kv := range list {
			k, _, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			if i, seen := index[k]; seen {
				out[i] = kv
				continue
			}
			index[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// pluginEnvironment builds the plugin environment from the server process
// environment: sandbox.BaseEnv (PATH, HOME, TMPDIR, LANG) plus the variables whose
// names match one of passEnv (exact names, "PREFIX*" or "*"), in environ order.
func pluginEnvironment(environ, passEnv []string) []string {
	var extra []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if matchEnvName(passEnv, k) {
			extra = append(extra, kv)
		}
	}
	return sandbox.BaseEnv(extra...)
}

// matchEnvName reports whether name matches one of the patterns.
func matchEnvName(patterns []string, name string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if p == name {
			return true
		}
	}
	return false
}

// validatePassEnv rejects malformed pass-through patterns.
func validatePassEnv(patterns []string) error {
	for _, p := range patterns {
		name := strings.TrimSuffix(p, "*")
		if p == "" || strings.ContainsAny(name, "=*\x00") {
			return fmt.Errorf("cmp: invalid pass-through environment pattern %q", p)
		}
	}
	return nil
}

// envValue returns the value of key in env (last entry wins).
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}
