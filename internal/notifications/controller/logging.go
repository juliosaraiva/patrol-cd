package controller

import (
	"context"
	"log/slog"
)

// engineDeliveryFailedMessage is the message the engine logs (at Warn, without application
// identity) for every failed delivery. The controller logs failures itself with the application
// and suppresses failures another configuration handles, so engine records are demoted to Debug.
const engineDeliveryFailedMessage = "notifications: delivery failed"

// demoteHandler lowers the engine's per-delivery failure records to Debug.
type demoteHandler struct{ inner slog.Handler }

func newEngineLogger(base *slog.Logger) *slog.Logger {
	return slog.New(demoteHandler{inner: base.Handler()}).With("component", "notifications-engine")
}

func (h demoteHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h demoteHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level > slog.LevelDebug && r.Message == engineDeliveryFailedMessage {
		if !h.inner.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
		r.Level = slog.LevelDebug
	}
	return h.inner.Handle(ctx, r)
}

func (h demoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return demoteHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h demoteHandler) WithGroup(name string) slog.Handler {
	return demoteHandler{inner: h.inner.WithGroup(name)}
}
