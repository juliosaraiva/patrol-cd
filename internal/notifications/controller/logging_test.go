package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEngineLoggerDemotesDeliveryFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		level     slog.Level
		msg       string
		recLevel  slog.Level
		want      string
		wantEmpty bool
	}{
		{name: "failure hidden at info", level: slog.LevelInfo, msg: engineDeliveryFailedMessage, recLevel: slog.LevelWarn, wantEmpty: true},
		{name: "failure kept as debug", level: slog.LevelDebug, msg: engineDeliveryFailedMessage, recLevel: slog.LevelWarn, want: "level=DEBUG"},
		{name: "other warnings unchanged", level: slog.LevelInfo, msg: "notifications: failed to get AppProject", recLevel: slog.LevelWarn, want: "level=WARN"},
		{name: "errors of other messages unchanged", level: slog.LevelInfo, msg: "x", recLevel: slog.LevelError, want: "level=ERROR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
			l := newEngineLogger(base).With("k", "v").WithGroup("g")
			l.Log(t.Context(), tc.recLevel, tc.msg, "error", "boom")
			out := buf.String()
			if tc.wantEmpty {
				assert.Empty(t, out)
				return
			}
			assert.Contains(t, out, tc.want)
			assert.Contains(t, out, "component=notifications-engine")
			assert.Contains(t, out, "k=v")
			assert.True(t, strings.Contains(out, "g.error=boom"), out)
		})
	}
}
