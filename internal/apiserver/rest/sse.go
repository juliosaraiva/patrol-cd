package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Stream defaults (api-contract.md: heartbeat comment every 15 s).
const (
	DefaultHeartbeat        = 15 * time.Second
	DefaultStreamBuffer     = 256
	DefaultStreamWrite      = 30 * time.Second
	DefaultReverifyInterval = time.Minute
	producerGrace           = 5 * time.Second
)

// Stream errors.
var (
	// ErrSlowConsumer is returned by Sink.Send when the client did not drain the event
	// buffer; the stream is then closed with an error event.
	ErrSlowConsumer = errors.New("stream consumer too slow")
	// ErrStreamClosed is returned by Sink.Send after the stream ended.
	ErrStreamClosed = errors.New("stream closed")
)

// StreamOptions configures ServeSSE.
type StreamOptions[E any] struct {
	// Heartbeat is the interval of ": heartbeat" comments (default 15 s).
	Heartbeat time.Duration
	// Buffer is the number of events queued for a client (default 256). A producer that
	// outpaces the client by more than Buffer events gets ErrSlowConsumer and the client
	// receives an error event and is disconnected.
	Buffer int
	// WriteTimeout bounds each write to the client (default 30 s).
	WriteTimeout time.Duration
	// ReverifyInterval is how often the caller's credentials are re-checked (default
	// 1 min; negative disables). A revoked or expired session ends the stream with an
	// unauthenticated error event.
	ReverifyInterval time.Duration
	// Authorize is called for every event before it is written; events it rejects are
	// skipped (per-event authorization, e.g. RBAC on the object's project). An error ends
	// the stream.
	Authorize func(ctx context.Context, ev E) (bool, error)
	// EventName optionally returns the SSE "event:" field of ev.
	EventName func(ev E) string
}

func (o StreamOptions[E]) normalized() StreamOptions[E] {
	if o.Heartbeat <= 0 {
		o.Heartbeat = DefaultHeartbeat
	}
	if o.Buffer <= 0 {
		o.Buffer = DefaultStreamBuffer
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultStreamWrite
	}
	if o.ReverifyInterval == 0 {
		o.ReverifyInterval = DefaultReverifyInterval
	}
	return o
}

type sseItem[E any] struct {
	id string
	ev E
}

// Sink receives a producer's events. Send never blocks.
type Sink[E any] struct {
	ctx    context.Context
	ch     chan sseItem[E]
	cancel context.CancelCauseFunc
}

// Context is done when the stream ends (client gone, slow consumer, revoked session).
func (s *Sink[E]) Context() context.Context { return s.ctx }

// Send queues ev without an event ID.
func (s *Sink[E]) Send(ev E) error { return s.SendWithID("", ev) }

// SendWithID queues ev with an SSE event ID (e.g. a resourceVersion clients can resume
// from with Last-Event-ID). It returns ErrSlowConsumer (and ends the stream) when the
// buffer is full, and the stream's end cause once it ended.
func (s *Sink[E]) SendWithID(id string, ev E) error {
	if s.ctx.Err() != nil {
		if c := context.Cause(s.ctx); c != nil && !errors.Is(c, context.Canceled) {
			return c
		}
		return ErrStreamClosed
	}
	select {
	case s.ch <- sseItem[E]{id: id, ev: ev}:
		return nil
	default:
		s.cancel(ErrSlowConsumer)
		return ErrSlowConsumer
	}
}

// ServeSSE streams the events produced by produce as Server-Sent Events: one
// "data: <json>" event per item (with "id:" and "event:" fields when given), heartbeat
// comments, per-event authorization, periodic credential re-verification, and bounded
// buffering. The stream ends when the client disconnects, produce returns, the client
// is too slow, or the caller's session is no longer valid; the last three send an
// "event: error" with a problem document first.
func ServeSSE[E any](w http.ResponseWriter, r *http.Request, opts StreamOptions[E], produce func(ctx context.Context, sink *Sink[E]) error) {
	opts = opts.normalized()
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		logger(r.Context()).LogAttrs(r.Context(), slog.LevelError, "streaming unsupported by response writer",
			slog.String("requestId", RequestID(r.Context())), slog.Any("error", err))
		return
	}

	op := ""
	st := stateFrom(r.Context())
	if st != nil && st.route != nil {
		op = st.route.OperationID
	}
	var m *Metrics
	if st != nil && st.env != nil {
		m = st.env.Metrics
	}
	m.streamOpened(op, 1)
	defer m.streamOpened(op, -1)

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	sink := &Sink[E]{ctx: ctx, ch: make(chan sseItem[E], opts.Buffer), cancel: cancel}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				logger(ctx).LogAttrs(ctx, slog.LevelError, "stream producer panic",
					slog.String("requestId", RequestID(ctx)), slog.String("panic", fmt.Sprint(v)))
				done <- fmt.Errorf("stream producer panic: %v", v)
			}
		}()
		done <- produce(ctx, sink)
	}()

	sw := &sseWriter{w: w, rc: rc, timeout: opts.WriteTimeout}
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()
	var reverify <-chan time.Time
	if opts.ReverifyInterval > 0 {
		t := time.NewTicker(opts.ReverifyInterval)
		defer t.Stop()
		reverify = t.C
	}

	producerExited := false
	finish := func(cause error) {
		cancel(cause)
		if producerExited {
			return
		}
		select {
		case <-done: //nolint:errcheck // the stream already ended; the producer's error has no recipient
			producerExited = true
		case <-time.After(producerGrace):
			logger(ctx).LogAttrs(ctx, slog.LevelWarn, "stream producer did not stop",
				slog.String("requestId", RequestID(ctx)))
		}
	}

	deliver := func(it sseItem[E]) bool {
		if opts.Authorize != nil {
			ok, err := opts.Authorize(ctx, it.ev)
			if err != nil {
				sw.errorEvent(ctx, err)
				finish(err)
				return false
			}
			if !ok {
				return true
			}
		}
		name := ""
		if opts.EventName != nil {
			name = opts.EventName(it.ev)
		}
		if err := sw.event(it.id, name, it.ev); err != nil {
			finish(err)
			return false
		}
		return true
	}

	// ended handles a canceled stream context: a slow consumer gets an error event (the
	// queued events are dropped), any other cause ends the stream quietly.
	ended := func() {
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrSlowConsumer) {
			m.slowConsumer(op)
			sw.errorEvent(ctx, &Error{Status: http.StatusTooManyRequests, Code: apiclient.CodeResourceExhausted,
				Detail: "the client did not keep up with the event stream; reconnect to resume"})
		}
		finish(cause)
	}

	for {
		// Cancellation wins over queued events.
		if ctx.Err() != nil {
			ended()
			return
		}
		select {
		case <-ctx.Done():
			ended()
			return
		case it := <-sink.ch:
			if !deliver(it) {
				return
			}
		case err := <-done:
			producerExited = true
			if ctx.Err() != nil {
				ended()
				return
			}
			// Drain what the producer queued before it returned.
		drain:
			for {
				select {
				case it := <-sink.ch:
					if !deliver(it) {
						return
					}
				default:
					break drain
				}
			}
			if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				sw.errorEvent(ctx, err)
			}
			cancel(nil)
			return
		case <-heartbeat.C:
			if err := sw.comment("heartbeat"); err != nil {
				finish(err)
				return
			}
		case <-reverify:
			if err := Reverify(ctx); err != nil {
				sw.errorEvent(ctx, err)
				finish(err)
				return
			}
		}
	}
}

type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (s *sseWriter) write(p string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	if _, err := s.w.Write([]byte(p)); err != nil {
		return err
	}
	if err := s.rc.Flush(); err != nil {
		return err
	}
	_ = s.rc.SetWriteDeadline(time.Time{})
	return nil
}

func sanitizeField(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

func (s *sseWriter) event(id, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	var sb strings.Builder
	if id != "" {
		sb.WriteString("id: " + sanitizeField(id) + "\n")
	}
	if name != "" {
		sb.WriteString("event: " + sanitizeField(name) + "\n")
	}
	sb.WriteString("data: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	return s.write(sb.String())
}

func (s *sseWriter) comment(text string) error {
	return s.write(": " + sanitizeField(text) + "\n\n")
}

// errorEvent writes "event: error" with a problem document for err.
func (s *sseWriter) errorEvent(ctx context.Context, err error) {
	e := ProblemFor(err)
	p := apiclient.Problem{
		Type:      apiclient.ProblemType(e.Code),
		Title:     http.StatusText(e.Status),
		Status:    e.Status,
		Detail:    e.Detail,
		Code:      e.Code,
		RequestID: RequestID(ctx),
	}
	if e.Status >= 500 {
		logger(ctx).LogAttrs(ctx, slog.LevelError, "stream failed",
			slog.String("requestId", RequestID(ctx)), slog.Any("error", e.Cause))
	}
	_ = s.event("", "error", p)
}
