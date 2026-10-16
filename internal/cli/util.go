package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
)

// maxInputFileBytes bounds files read by commands (keys, certificates, manifests).
const maxInputFileBytes = 4 << 20

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// readInputFile reads a bounded local file; "-" reads the command's standard input.
func readInputFile(f *cmdutil.Factory, path string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		if f.IO.In == nil {
			return nil, fmt.Errorf("no standard input")
		}
		r = f.IO.In
	} else {
		fh, err := os.Open(filepath.Clean(path)) //nolint:gosec // a file named by the user
		if err != nil {
			return nil, err
		}
		defer func() { _ = fh.Close() }()
		r = fh
	}
	b, err := io.ReadAll(io.LimitReader(r, maxInputFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInputFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxInputFileBytes)
	}
	return b, nil
}

// parseLabels parses "key=value" pairs (upstream label.Parse): every entry needs "=".
func parseLabels(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("Label %s is not in the format key=value", e) //nolint:staticcheck // upstream wording
		}
		out[k] = v
	}
	return out, nil
}

// formatUnix formats a Unix timestamp as RFC 3339.
func formatUnix(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// formatExpiry formats an expiry ("never" for 0, " (expired)" suffix when past).
func formatExpiry(exp int64, now time.Time) string {
	if exp <= 0 {
		return "never"
	}
	t := time.Unix(exp, 0)
	s := t.UTC().Format(time.RFC3339)
	if t.Before(now) {
		s += " (expired)"
	}
	return s
}

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
