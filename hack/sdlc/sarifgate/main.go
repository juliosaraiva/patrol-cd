// Command sarifgate reads the SARIF files that CodeQL wrote and decides whether a change may be
// pushed (docs/guides/claude-code-sdlc.md). It lists every result and exits 1 when a result of
// level error or warning is neither suppressed in the source nor accepted in the baseline file.
//
// Usage:
//
//	go run ./hack/sdlc/sarifgate [-baseline FILE] [-summary FILE] RESULT.sarif...
//
// The baseline accepts known alerts, one per line, tab separated; blank lines and lines starting
// with # are ignored:
//
//	rule-id <TAB> path <TAB> primaryLocationLineHash <TAB> reason
//
// The fingerprint is CodeQL's line-independent hash of the alert's location, so an accepted alert
// stays accepted when lines above it move. The reason is required. Baseline lines that match no
// result are reported, but do not fail the run (a CodeQL upgrade can change fingerprints).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sarifgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baselinePath := fs.String("baseline", "", "file of accepted alerts (empty: none)")
	summaryPath := fs.String("summary", "", "also write the report to FILE")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "sarifgate: no SARIF files given")
		return 2
	}
	var accepted []acceptance
	if *baselinePath != "" {
		var err error
		if accepted, err = readBaseline(*baselinePath); err != nil {
			_, _ = fmt.Fprintf(stderr, "sarifgate: %v\n", err)
			return 2
		}
	}
	var results []result
	for _, p := range fs.Args() {
		rs, err := readSARIF(p)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "sarifgate: %v\n", err)
			return 2
		}
		results = append(results, rs...)
	}
	rep := evaluate(results, accepted)
	text := rep.String()
	_, _ = fmt.Fprint(stdout, text)
	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, []byte(text), 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "sarifgate: %v\n", err)
			return 2
		}
	}
	if rep.failed() {
		return 1
	}
	return 0
}

var errBaseline = errors.New("invalid baseline")
