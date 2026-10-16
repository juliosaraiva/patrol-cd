package helm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
)

func TestShowChart(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	meta, err := c.ShowChart(context.Background(), chart)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "mychart" || meta.Version != "0.1.0" || meta.AppVersion != "1.0" {
		t.Fatalf("metadata = %+v", meta)
	}
	want := helm.ChartDetails{
		Description: "A fixture chart for Patrol CD tests",
		Home:        "https://example.com/mychart",
		Maintainers: []string{"Alice <alice@example.com>", "Bob"},
	}
	if got := meta.Details(); !reflect.DeepEqual(got, want) {
		t.Fatalf("details = %+v", got)
	}
	values, err := c.ShowValues(context.Background(), chart)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(values), "replicaCount: 1")
}

func TestGetParameters(t *testing.T) {
	t.Parallel()
	c := templateClient(t)
	chart := copyDir(t, fixtureChart)
	override := filepath.Join(chart, "values-prod.yaml")
	if err := os.WriteFile(override, []byte("image:\n  tag: \"2.0\"\nservice:\n  ports:\n    - name: grpc\n      port: 9090\nextra:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("message: remote\n"))
	}))
	t.Cleanup(remote.Close)
	files := []string{override, filepath.Join(chart, "missing.yaml"), remote.URL + "/values.yaml"}
	got, err := c.GetParameters(context.Background(), chart, chart, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"replicaCount":          "1",
		"image.repository":      "nginx",
		"image.tag":             "2.0",
		"service.ports[0].name": "grpc",
		"service.ports[0].port": "9090",
		"service.ports[1].name": "https",
		"service.ports[1].port": "443",
		"message":               "remote",
		"config":                "",
		"nullable":              "<nil>",
		"extra.enabled":         "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters =\n%v\nwant\n%v", got, want)
	}

	// values.yaml escaping the repository is not read.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "values.yaml"), []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escaping := copyDir(t, fixtureChart)
	if err := os.Remove(filepath.Join(escaping, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "values.yaml"), filepath.Join(escaping, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetParameters(context.Background(), escaping, escaping, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := got["leaked"]; leaked {
		t.Fatal("out-of-bounds values.yaml was read")
	}

	// Oversized value files are refused.
	small := &helm.Client{HelmPath: c.HelmPath, WorkDir: t.TempDir(), MaxValuesBytes: 8}
	if _, err := small.GetParameters(context.Background(), chart, chart, []string{override}); err == nil {
		t.Fatal("oversized value file accepted")
	}
}

func TestReadChartValues(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	chart := copyDir(t, fixtureChart)
	data, ok, err := c.ReadChartValues(chart, chart)
	if err != nil || !ok {
		t.Fatalf("ReadChartValues = %v, %v", ok, err)
	}
	mustContain(t, string(data), "replicaCount: 1")
	if err := os.Remove(filepath.Join(chart, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.ReadChartValues(chart, chart); ok || err != nil {
		t.Fatalf("missing values.yaml: %v, %v", ok, err)
	}
	outside := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(outside, []byte("leak: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(chart, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.ReadChartValues(chart, chart); ok || err != nil {
		t.Fatalf("out-of-bounds values.yaml: %v, %v", ok, err)
	}
}
