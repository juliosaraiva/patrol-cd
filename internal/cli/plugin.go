package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

// Plugin environment (documented in docs/architecture/components/cli.md). Plugins inherit
// the caller's environment unchanged, plus these variables when they are not already
// set; credentials are never added.
const (
	// PluginPrefix prefixes plugin executables: "patrol foo" runs "patrol-foo".
	PluginPrefix = CommandName + "-"
	// EnvPluginBinary is the absolute path of the patrol executable that ran the plugin.
	EnvPluginBinary = "PATROL_BINARY"
	// EnvPluginName is the plugin name (the first argument).
	EnvPluginName = "PATROL_PLUGIN_NAME"
)

// errPluginRelative is returned for a plugin found through a relative PATH entry.
var errPluginRelative = errors.New("found in a relative PATH entry; refusing to run it")

// findPlugin looks up patrol-<name> in the PATH of the factory's environment. Built-in
// commands always win (the caller only asks for unknown commands); names with path
// separators are refused.
func findPlugin(f *cmdutil.Factory, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, "-") {
		return "", nil
	}
	file := PluginPrefix + name
	for _, dir := range filepath.SplitList(f.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, file)
		if !isExecutable(p) {
			continue
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("plugin '%s' %w", file, errPluginRelative)
		}
		return p, nil
	}
	return "", nil
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// runPlugin executes the plugin named by args[0] with the remaining arguments and the
// command's streams. handled is false when no plugin exists.
func runPlugin(ctx context.Context, f *cmdutil.Factory, args []string) (code int, handled bool) {
	path, err := findPlugin(f, args[0])
	if err != nil {
		_, _ = fmt.Fprintf(f.ErrOut(), "Error: %v\n", err)
		return cmdutil.ExitFatal, true
	}
	if path == "" {
		return 0, false
	}
	cmd := exec.CommandContext(ctx, path, args[1:]...) //nolint:gosec // the user's own executable from PATH
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f.IO.In, f.Out(), f.ErrOut()
	cmd.Env = pluginEnv(f, args[0])
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), true
		}
		_, _ = fmt.Fprintf(f.ErrOut(), "Error: %v\n", err)
		return cmdutil.ExitFatal, true
	}
	return cmdutil.ExitOK, true
}

func pluginEnv(f *cmdutil.Factory, name string) []string {
	var env []string
	if f.Environ != nil {
		env = f.Environ()
	} else {
		env = os.Environ()
	}
	has := func(k string) bool {
		return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
	}
	if exe, err := os.Executable(); err == nil && !has(EnvPluginBinary) {
		env = append(env, EnvPluginBinary+"="+exe)
	}
	if !has(config.EnvConfig) {
		env = append(env, config.EnvConfig+"="+f.ConfigPath())
	}
	env = append(env, EnvPluginName+"="+name)
	return env
}

// listPlugins returns the names of the executable patrol-<name> files on PATH (absolute
// entries only), sorted and de-duplicated, for shell completion.
func listPlugins(f *cmdutil.Factory) []string {
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(f.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutPrefix(e.Name(), PluginPrefix)
			if !ok || name == "" || e.IsDir() || strings.ContainsAny(name, `/\`) {
				continue
			}
			if e.Type()&fs.ModeSymlink == 0 {
				if info, err := e.Info(); err != nil || info.Mode().Perm()&0o111 == 0 {
					continue
				}
			} else if !isExecutable(filepath.Join(dir, e.Name())) {
				continue
			}
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
