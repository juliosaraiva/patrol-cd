// Command helmdocs keeps the generated parts of the Helm chart (deploy/helm/patrol-cd) in sync
// with values.yaml and with internal/settings, the source of truth for setting keys:
//
//   - README.md: rendered from README.md.gotmpl with a values table built from the "# --"
//     comments of values.yaml (helm-docs conventions: "# -- description", continuation lines,
//     "# @default -- text" to replace a long default).
//   - templates/_generated_params.tpl: the patrol-cmd-params-cm keys and environment variables
//     the install projects into components without Kubernetes access (repo-server,
//     commit-server), and the profiler toggle keys, from settings.ParamSpecs().
//   - templates/crds/*: deploy/crds/* (CRDs and the Operation ValidatingAdmissionPolicy) wrapped
//     in their install guards, with the keep annotation and the chart labels inserted.
//   - values.schema.json: the closed key sets of configs.params (definitions.cmdParamsKeys),
//     configs.cm (definitions.patrolCM) and configs.rbac (definitions.patrolRBACCM), from the
//     settings key register. The rest of the schema is hand-written and left untouched.
//
// It also checks that the settings shipped in values.yaml (resource exclusions and
// ignoreResourceUpdates rules) equal the defaults compiled into internal/settings.
//
// Usage:
//
//	go run ./hack/helmdocs          # write all generated files
//	go run ./hack/helmdocs -check   # fail when a generated file is stale or a default drifted
//
// Output is deterministic: the same inputs produce byte-identical files.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

func main() {
	chart := flag.String("chart", "deploy/helm/patrol-cd", "chart directory")
	crds := flag.String("crds", "deploy/crds", "directory of the generated CRDs and the Operation admission policy")
	check := flag.Bool("check", false, "fail when a generated file is stale (write nothing)")
	flag.Parse()
	if err := run(*chart, *crds, *check); err != nil {
		fmt.Fprintln(os.Stderr, "helmdocs:", err)
		os.Exit(1)
	}
}

// output is one generated file.
type output struct {
	path    string
	content []byte
}

func run(chart, crdDir string, check bool) error {
	valuesPath := filepath.Join(chart, "values.yaml")
	values, err := os.ReadFile(valuesPath)
	if err != nil {
		return err
	}
	if err := checkShippedDefaults(values); err != nil {
		return fmt.Errorf("%s: %w", valuesPath, err)
	}
	readme, err := renderReadme(chart, values)
	if err != nil {
		return err
	}
	params, err := renderParamsTemplate()
	if err != nil {
		return err
	}
	schemaPath := filepath.Join(chart, "values.schema.json")
	schemaIn, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	schema, err := renderSchema(schemaIn)
	if err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	outputs := []output{
		{filepath.Join(chart, "README.md"), readme},
		{filepath.Join(chart, "templates", "_generated_params.tpl"), params},
		{schemaPath, schema},
	}
	crdTemplates, err := renderCRDTemplates(crdDir)
	if err != nil {
		return err
	}
	crdOut := filepath.Join(chart, "templates", "crds")
	names := make([]string, 0, len(crdTemplates))
	for name := range crdTemplates {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		outputs = append(outputs, output{filepath.Join(crdOut, name), crdTemplates[name]})
	}
	var stale []error
	existing, err := filepath.Glob(filepath.Join(crdOut, "*"))
	if err != nil {
		return err
	}
	for _, f := range existing {
		if _, ok := crdTemplates[filepath.Base(f)]; ok {
			continue
		}
		if check {
			stale = append(stale, fmt.Errorf("%s has no source in %s: run `go run ./hack/helmdocs`", f, crdDir))
			continue
		}
		if err := os.Remove(f); err != nil {
			return err
		}
		fmt.Println("removed", f)
	}
	if !check {
		if err := os.MkdirAll(crdOut, 0o755); err != nil {
			return err
		}
	}
	for _, o := range outputs {
		cur, err := os.ReadFile(o.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if bytes.Equal(cur, o.content) {
			continue
		}
		if check {
			stale = append(stale, fmt.Errorf("%s is stale: run `go run ./hack/helmdocs`", o.path))
			continue
		}
		if err := os.WriteFile(o.path, o.content, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", o.path)
	}
	return errors.Join(stale...)
}
