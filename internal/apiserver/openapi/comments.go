package openapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// goComments extracts the doc comments of the exported types and struct fields declared
// in dir (non-test files) as "<importPath>.<Type>" and "<importPath>.<Type>.<Field>".
func goComments(importPath, dir string, out map[string]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, n))
	}
	slices.Sort(files)
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", f, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if !ts.Name.IsExported() {
					continue
				}
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				key := importPath + "." + ts.Name.Name
				if txt := commentText(doc); txt != "" {
					out[key] = txt
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					txt := commentText(field.Doc)
					if txt == "" {
						continue
					}
					for _, n := range field.Names {
						if n.IsExported() {
							out[key+"."+n.Name] = txt
						}
					}
				}
			}
		}
	}
	return nil
}

func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(strings.Fields(cg.Text()), " "))
}
