// Command openapifixture regenerates internal/diff/testdata/openapi-v2.json, the OpenAPI v2
// schema subset used by the diff package tests to build a managedfields.GvkParser.
//
// It starts a local envtest kube-apiserver (KUBEBUILDER_ASSETS must point at the envtest
// binaries), downloads /openapi/v2, keeps only the definitions reachable from the root types
// listed below and writes them as indented JSON. The output is deterministic for a given
// apiserver version, so the fixture can be refreshed and reviewed as a normal diff:
//
//	KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
//	  go run ./internal/diff/internal/gen/openapifixture -out internal/diff/testdata/openapi-v2.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// roots are the OpenAPI definitions the diff tests need. Everything they reference is kept.
var roots = []string{
	"io.k8s.api.apps.v1.Deployment",
	"io.k8s.api.apps.v1.StatefulSet",
	"io.k8s.api.core.v1.Service",
	"io.k8s.api.core.v1.ConfigMap",
	"io.k8s.api.core.v1.Secret",
	"io.k8s.api.core.v1.ServiceAccount",
	"io.k8s.api.autoscaling.v1.HorizontalPodAutoscaler",
	"io.k8s.api.autoscaling.v2.HorizontalPodAutoscaler",
	"io.k8s.api.rbac.v1.ClusterRole",
}

func main() {
	out := flag.String("out", "internal/diff/testdata/openapi-v2.json", "output file")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*out); err != nil {
		logger.Error("generate openapi fixture", "error", err)
		os.Exit(1)
	}
}

func run(out string) (err error) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return errors.New("KUBEBUILDER_ASSETS is not set")
	}
	env := &envtest.Environment{}
	cfg, err := envtestx.Start(env)
	if err != nil {
		return fmt.Errorf("start envtest: %w", err)
	}
	defer func() {
		if stopErr := env.Stop(); stopErr != nil && err == nil {
			err = fmt.Errorf("stop envtest: %w", stopErr)
		}
	}()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, err := cs.Discovery().RESTClient().Get().AbsPath("/openapi/v2").
		SetHeader("Accept", "application/json").Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("fetch /openapi/v2: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode openapi: %w", err)
	}
	defs, ok := doc["definitions"].(map[string]any)
	if !ok {
		return errors.New("openapi document has no definitions")
	}
	keep := map[string]bool{}
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if keep[name] {
			continue
		}
		def, ok := defs[name]
		if !ok {
			return fmt.Errorf("definition %q not found", name)
		}
		keep[name] = true
		queue = append(queue, refs(def)...)
	}
	filtered := map[string]any{}
	for name := range keep {
		filtered[name] = defs[name]
	}
	info, _ := doc["info"].(map[string]any)
	result := map[string]any{
		"swagger":     doc["swagger"],
		"info":        info,
		"paths":       map[string]any{},
		"definitions": filtered,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode fixture: %w", err)
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil { //nolint:gosec // fixture checked into the repository

		return fmt.Errorf("write fixture: %w", err)
	}
	return nil
}

// refs returns the definition names referenced by $ref anywhere inside v.
func refs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == "$ref" {
				if s, ok := child.(string); ok {
					out = append(out, strings.TrimPrefix(s, "#/definitions/"))
				}
				continue
			}
			out = append(out, refs(child)...)
		}
	case []any:
		for _, child := range t {
			out = append(out, refs(child)...)
		}
	}
	return out
}
