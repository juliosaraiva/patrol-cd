package settings

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var simpleDurationRE = regexp.MustCompile(`^(\d+)([smhd])$`)

// parseSimpleDuration parses the "<n><s|m|h|d>" form accepted by Argo CD's
// github.com/argoproj/pkg time.ParseDuration.
func parseSimpleDuration(v string) (time.Duration, error) {
	m := simpleDurationRE.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q: expected format <duration><unit> (e.g. 3h)", v)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", v, err)
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	if n > int64(1<<63-1)/int64(unit) {
		return 0, fmt.Errorf("invalid duration %q: out of range", v)
	}
	return time.Duration(n) * unit, nil
}

// parseDuration parses a Go duration ("1h30m") and falls back to the "<n><unit>" form that
// additionally accepts days ("7d"), like upstream env.ParseDurationFromEnv.
func parseDuration(v string) (time.Duration, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return d, nil
	}
	return parseSimpleDuration(v)
}
