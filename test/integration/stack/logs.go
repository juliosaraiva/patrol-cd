package stack

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// defaultAllowedErrors are ERROR records that components log during normal operation of
// the tests (shutdowns, restarts, simulated outages). Each entry documents why it is
// expected.
var defaultAllowedErrors = []string{
	// Components stopped by the harness (restarts, cleanup): controller-runtime and client-go
	// report the cancelled context of in-flight requests and watches.
	`context canceled`,
	`context deadline exceeded.*shutting down`,
}

// logSink collects the text log records of every component of a Stack and remembers the
// ERROR records.
type logSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	errs    []string
	allowed []*regexp.Regexp
	// marks are positions in buf (Mark/Since).
}

// Write receives exactly one record per call (slog.TextHandler writes whole lines).
func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Contains(p, []byte(" level=ERROR ")) {
		s.errs = append(s.errs, strings.TrimSpace(string(p)))
	}
	return s.buf.Write(p)
}

func (s *logSink) logger(component string, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: level})).With("component", component)
}

// allow adds allow-list patterns (regular expressions matched against the whole record).
func (s *logSink) allow(patterns ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("allowed error pattern %q: %w", p, err)
		}
		s.allowed = append(s.allowed, re)
	}
	return nil
}

// unexpected returns the ERROR records no allow-list pattern matches.
func (s *logSink) unexpected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.errs {
		ok := false
		for _, re := range s.allowed {
			if re.MatchString(e) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, e)
		}
	}
	return out
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Len is the current size of the log (a mark for Since).
func (s *logSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// Since returns the log written after mark.
func (s *logSink) Since(mark int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buf.Bytes()
	if mark > len(b) {
		return ""
	}
	return string(b[mark:])
}

// finish runs at Stack cleanup: unexpected ERROR records fail the test; on failure the
// log tail is printed; with PATROL_INTEGRATION_LOG_DIR the whole log is kept.
func (s *logSink) finish(t testing.TB) {
	t.Helper()
	if bad := s.unexpected(); len(bad) > 0 {
		const max = 20
		shown := bad
		if len(shown) > max {
			shown = shown[:max]
		}
		t.Errorf("components logged %d unexpected ERROR record(s) (add an allow-list pattern only when the error is expected):\n%s",
			len(bad), strings.Join(shown, "\n"))
	}
	if dir := os.Getenv(EnvLogDir); dir != "" {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + ".log"
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(s.String()), 0o644); err != nil {
				t.Logf("write component log: %v", err)
			}
		}
	}
	if t.Failed() {
		const tail = 96 << 10
		all := s.String()
		if len(all) > tail {
			all = "…\n" + all[len(all)-tail:]
		}
		t.Logf("component logs (tail; set %s for the complete log):\n%s", EnvLogDir, all)
	}
}
