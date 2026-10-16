package server

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// Log formats.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// LogOptions select the process logger.
type LogOptions struct {
	// Level is a slog level name (debug, info, warn, error).
	Level string
	// Format is json or text.
	Format string
	// TimestampLayout formats the time attribute (Go layout; "" keeps RFC 3339).
	TimestampLayout string
}

// NewLogger returns a logger writing to w.
func NewLogger(w io.Writer, o LogOptions) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(o.Level))); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", o.Level, err)
	}
	hopts := &slog.HandlerOptions{Level: level}
	if layout := o.TimestampLayout; layout != "" {
		hopts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String(slog.TimeKey, t.Format(layout))
				}
			}
			return a
		}
	}
	switch strings.ToLower(strings.TrimSpace(o.Format)) {
	case LogFormatJSON:
		return slog.New(slog.NewJSONHandler(w, hopts)), nil
	case LogFormatText:
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	}
	return nil, fmt.Errorf("unknown log format '%s' (json or text)", o.Format)
}

// installLogger makes l the default logger of the process: log/slog, the standard log
// package and controller-runtime (whose logr sink would otherwise discard and warn).
func installLogger(l *slog.Logger) {
	slog.SetDefault(l)
	ctrllog.SetLogger(logr.FromSlogHandler(l.Handler()))
}

// inCluster reports whether the process runs in a Kubernetes pod.
func inCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != ""
}

// defaultLogFormat is the format used when none was configured anywhere: JSON in a pod (log
// collectors), text on a terminal.
func defaultLogFormat() string {
	if inCluster() {
		return LogFormatJSON
	}
	return LogFormatText
}
