package lua

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// builtinScripts holds the Lua scripts of the built-in customizations (copied from
// upstream resource_customizations/, see customizations/README.md). Only scripts are
// embedded; test inputs stay on disk.
//
//go:embed customizations/*/*/health.lua customizations/*/*/actions/discovery.lua customizations/*/*/actions/*/action.lua
var builtinScripts embed.FS

const (
	builtinRoot         = "customizations"
	healthScriptFile    = "health.lua"
	actionScriptFile    = "action.lua"
	discoveryScriptFile = "discovery.lua"
)

// errNoScript means the requested built-in script does not exist.
var errNoScript = errors.New("built-in script does not exist")

// builtinFS is the script source used by a Runtime (replaceable in tests).
type builtinFS struct {
	fsys fs.FS

	globOnce sync.Once
	globs    []string
	globErr  error
}

func newBuiltinFS(fsys fs.FS) *builtinFS { return &builtinFS{fsys: fsys} }

var defaultBuiltins = sync.OnceValue(func() *builtinFS {
	sub, err := fs.Sub(builtinScripts, builtinRoot)
	if err != nil {
		// fs.Sub only fails for an invalid directory name, which is a constant here.
		return newBuiltinFS(builtinScripts)
	}
	return newBuiltinFS(sub)
})

// read returns the script at key/<rel...>; errNoScript when absent or when key would not
// form a valid path (group/kind come from object fields).
func (b *builtinFS) read(elems ...string) (string, error) {
	p := path.Join(elems...)
	if !fs.ValidPath(p) {
		return "", errNoScript
	}
	data, err := fs.ReadFile(b.fsys, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
			return "", errNoScript
		}
		return "", fmt.Errorf("read built-in script %s: %w", p, err)
	}
	return string(data), nil
}

// builtinAliases lets Patrol's own kinds reuse the built-in customizations upstream ships
// for the corresponding Argo CD kinds, whose spec and status layout they share
// (ApplicationSet health from status.conditions; Application toggle-auto-sync action).
// There is no built-in Application health script, so Application health stays opt-in.
var builtinAliases = map[schema.GroupKind]schema.GroupKind{
	{Group: health.ApplicationGroup, Kind: "Application"}:    {Group: "argoproj.io", Kind: "Application"},
	{Group: health.ApplicationGroup, Kind: "ApplicationSet"}: {Group: "argoproj.io", Kind: "ApplicationSet"},
}

// gkElems returns the path elements of gk inside the customization tree, or false when a
// component could escape its directory (group and kind come from object fields).
func gkElems(gk schema.GroupKind) ([]string, bool) {
	elems := []string{gk.Kind}
	if gk.Group != "" {
		elems = []string{gk.Group, gk.Kind}
	}
	for _, e := range elems {
		if !validElem(e) {
			return nil, false
		}
	}
	return elems, true
}

func validElem(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

// readFor reads rel under the built-in directory of gk or, when that has no such file,
// of gk's alias. It returns the script and the group/kind key of the directory used, or
// errNoScript.
func (b *builtinFS) readFor(gk schema.GroupKind, rel ...string) (script, key string, err error) {
	candidates := []schema.GroupKind{gk}
	if alias, ok := builtinAliases[gk]; ok {
		candidates = append(candidates, alias)
	}
	for _, c := range candidates {
		elems, ok := gkElems(c)
		if !ok {
			continue
		}
		s, err := b.read(append(elems, rel...)...)
		if err == nil {
			return s, OverrideKey(c), nil
		}
		if !errors.Is(err, errNoScript) {
			return "", "", err
		}
	}
	return "", "", errNoScript
}

// wildcardDirs returns, sorted, the group/kind directories holding a health.lua whose
// name contains "_" (the built-in wildcard marker).
func (b *builtinFS) wildcardDirs() ([]string, error) {
	b.globOnce.Do(func() {
		var dirs []string
		err := fs.WalkDir(b.fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || path.Base(p) != healthScriptFile {
				return nil
			}
			dir := path.Dir(p)
			if !strings.Contains(dir, "_") {
				return nil
			}
			pattern := strings.ReplaceAll(dir, "_", "*")
			if !doublestar.ValidatePattern(pattern) {
				return fmt.Errorf("invalid built-in wildcard directory %q", dir)
			}
			dirs = append(dirs, dir)
			return nil
		})
		slices.Sort(dirs)
		b.globs, b.globErr = dirs, err
	})
	return b.globs, b.globErr
}

// wildcardHealth returns the first (sorted) wildcard health script matching key.
func (b *builtinFS) wildcardHealth(key string) (script, dir string, err error) {
	dirs, err := b.wildcardDirs()
	if err != nil {
		return "", "", err
	}
	for _, d := range dirs {
		if !doublestar.MatchUnvalidated(strings.ReplaceAll(d, "_", "*"), key) {
			continue
		}
		s, err := b.read(d, healthScriptFile)
		if err != nil {
			return "", "", err
		}
		return s, d, nil
	}
	return "", "", nil
}
