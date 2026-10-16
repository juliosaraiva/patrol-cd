package stack

import (
	"io"
	"strconv"
	"strings"
	"testing"
)

// Metrics returns the Prometheus text exposition of a component's metrics listener.
func (s *Stack) Metrics(t testing.TB, component string) string {
	t.Helper()
	addr := map[string]string{
		Controller: s.controllerMetrics, Server: s.serverMetrics, RepoServer: s.repoProbes,
		AppSetController: s.appsetMetrics, NotificationsController: s.notifMetrics,
	}[component]
	for i, c := range s.extraControllers {
		if component == ControllerReplica(i+1) {
			addr = c.metrics
		}
	}
	if addr == "" {
		t.Fatalf("no metrics listener for %s", component)
	}
	resp, err := probeClient.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape %s metrics: %v", component, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		t.Fatalf("read %s metrics: %v", component, err)
	}
	return string(b)
}

// MetricSum sums the samples of metric name whose labels include every pair of labels
// (Prometheus text format; label values without escaped quotes).
func MetricSum(exposition, name string, labels map[string]string) float64 {
	var sum float64
	for _, line := range strings.Split(exposition, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, name)
		if !ok || (rest != "" && rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		lbls := ""
		if strings.HasPrefix(rest, "{") {
			end := strings.Index(rest, "}")
			if end < 0 {
				continue
			}
			lbls, rest = rest[1:end], rest[end+1:]
		}
		match := true
		for k, v := range labels {
			if !strings.Contains(","+lbls+",", ","+k+"=\""+v+"\",") {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
			sum += f
		}
	}
	return sum
}
