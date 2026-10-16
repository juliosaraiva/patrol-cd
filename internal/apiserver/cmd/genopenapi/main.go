// Command genopenapi writes the OpenAPI 3.1 document of patrol-server's public API
// (api/openapi/openapi.json) from the route table. Run it from the repository root:
//
//	go run ./internal/apiserver/cmd/genopenapi            # write the document
//	go run ./internal/apiserver/cmd/genopenapi -check     # fail when it is stale
//
// Output is deterministic.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
)

func main() {
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", "", "output file (default <root>/"+apiserver.OpenAPIPath+")")
	check := flag.Bool("check", false, "compare with the existing file instead of writing it")
	flag.Parse()
	if err := run(*root, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "genopenapi:", err)
		os.Exit(1)
	}
}

func run(root, out string, check bool) error {
	if out == "" {
		out = filepath.Join(root, apiserver.OpenAPIPath)
	}
	doc, err := apiserver.OpenAPIDocument(root)
	if err != nil {
		return err
	}
	if check {
		cur, err := os.ReadFile(out) //nolint:gosec // path from the command line
		if err != nil {
			return fmt.Errorf("read %s: %w", out, err)
		}
		if !bytes.Equal(cur, doc) {
			return fmt.Errorf("%s is stale; run: go run ./internal/apiserver/cmd/genopenapi", out)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	if err := os.WriteFile(out, doc, 0o644); err != nil { //nolint:gosec // generated public document
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}
