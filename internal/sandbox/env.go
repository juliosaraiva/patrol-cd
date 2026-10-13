package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath is used by BaseEnv when the calling process has no PATH.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// BaseEnv returns a minimal environment containing only PATH, HOME, TMPDIR and LANG,
// followed by extra. PATH and HOME come from the calling process (PATH falls back to
// DefaultPath, HOME to the temp directory), TMPDIR is os.TempDir() and LANG is
// C.UTF-8 so tool output does not depend on the host locale.
//
// Each extra entry has the form KEY=VALUE and replaces an earlier entry with the
// same key (base or extra) in place. Entries without '=' or with an empty key are
// dropped.
func BaseEnv(extra ...string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = DefaultPath
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.TempDir()
	}
	env := make([]string, 0, 4+len(extra))
	env = append(env,
		"PATH="+path,
		"HOME="+home,
		"TMPDIR="+os.TempDir(),
		"LANG=C.UTF-8",
	)
	index := make(map[string]int, cap(env))
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		index[k] = i
	}
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if i, seen := index[k]; seen {
			env[i] = kv
			continue
		}
		index[k] = len(env)
		env = append(env, kv)
	}
	return env
}

// lookupEnv returns the value of the last KEY=VALUE entry for key, matching the
// "later wins" rule os/exec applies to duplicate keys.
func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

// resolveExecutable turns Cmd.Path into an absolute executable path. Bare names are
// looked up in the PATH of the command environment; relative PATH entries are
// skipped so a working directory with untrusted content can never supply a binary.
func resolveExecutable(name string, env []string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty command path", ErrInvalidCommand)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		if !filepath.IsAbs(name) {
			return "", fmt.Errorf("%w: relative command path %q", ErrInvalidCommand, name)
		}
		if err := checkExecutable(name); err != nil {
			return "", err
		}
		return name, nil
	}
	pathEnv, _ := lookupEnv(env, "PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if checkExecutable(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: executable %q not found in command PATH", os.ErrNotExist, name)
}

func checkExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInvalidCommand, path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable", os.ErrPermission, path)
	}
	return nil
}
