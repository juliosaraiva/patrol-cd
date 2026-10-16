package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperMainRE matches the exported helper dispatchers libraries provide.
var helperMainRE = regexp.MustCompile(`^Maybe[A-Z][A-Za-z0-9]*Main$`)

// moduleRoot walks up to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "go.mod not found")
		dir = parent
	}
}

// TestEveryHelperMainIsDispatched finds every exported MaybeXxxMain function of the module
// (non-test files of internal/ and pkg/) and checks DispatchHelpers calls it.
func TestEveryHelperMainIsDispatched(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	modulePath := "github.com/juliosaraiva/patrol-cd"
	found := map[string]bool{}
	for _, top := range []string{"internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !helperMainRE.MatchString(fn.Name.Name) {
					continue
				}
				rel, err := filepath.Rel(root, filepath.Dir(p))
				if err != nil {
					return err
				}
				found[modulePath+"/"+filepath.ToSlash(rel)+"."+fn.Name.Name] = true
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, found)

	var dispatched []string
	for _, h := range Helpers() {
		name := runtime.FuncForPC(reflect.ValueOf(h.Main).Pointer()).Name()
		dispatched = append(dispatched, name)
	}
	for fn := range found {
		assert.Contains(t, dispatched, fn, "helper %s is not dispatched by server.DispatchHelpers", fn)
	}
	// The sandbox helper must come first: it execs targets that may be helpers again.
	require.NotEmpty(t, dispatched)
	assert.True(t, strings.HasSuffix(dispatched[0], "/internal/sandbox.MaybeHelperMain"), dispatched[0])
	assert.False(t, slices.ContainsFunc(dispatched, func(s string) bool { return s == "" }))
}
