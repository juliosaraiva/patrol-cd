package rest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type numberEvent struct {
	N int `json:"n"`
}

// streamRoutes registers GET /api/v1/stream/numbers: n events (IDs "1".."n"), odd ones
// filtered by the per-event authorization callback, then the stream stays open until
// the client leaves (or ends when ?end=true).
func streamRoutes(producerDone chan<- error) func(t *rest.Table) {
	return func(t *rest.Table) {
		registerTestRoutes(t)
		rest.RegisterStream(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/stream/numbers", OperationID: "streamNumbers", Access: rest.Authenticated()},
			rest.StreamOptions[numberEvent]{
				Heartbeat:        20 * time.Millisecond,
				ReverifyInterval: 30 * time.Millisecond,
				Authorize:        func(_ context.Context, ev numberEvent) (bool, error) { return ev.N%2 == 0, nil },
				EventName:        func(numberEvent) string { return "number" },
			},
			func(ctx context.Context, req *struct {
				N   int  `query:"n" json:"-"`
				End bool `query:"end" json:"-"`
			}, sink *rest.Sink[numberEvent]) error {
				for i := 1; i <= req.N; i++ {
					if err := sink.SendWithID(strconv.Itoa(i), numberEvent{N: i}); err != nil {
						return err
					}
				}
				if req.End {
					return nil
				}
				<-ctx.Done()
				if producerDone != nil {
					producerDone <- ctx.Err()
				}
				return ctx.Err()
			})
	}
}

func openStream(t *testing.T, h *harness, path string, opts ...reqOpt) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	for _, o := range opts {
		o(req)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body), cancel
}

// readUntil reads lines until pred matches one or the deadline passes.
func readUntil(t *testing.T, r *bufio.Reader, pred func(string) bool) []string {
	t.Helper()
	var lines []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines = append(lines, strings.TrimRight(line, "\n"))
				if pred(strings.TrimRight(line, "\n")) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out; read so far: %q", lines)
	}
	return lines
}

func TestSSEEventsHeartbeatsAndFiltering(t *testing.T) {
	t.Parallel()
	producerDone := make(chan error, 1)
	h := newHarness(t, streamRoutes(producerDone))
	resp, r, cancel := openStream(t, h, "/api/v1/stream/numbers?n=4", bearer(h.loginToken("alice")))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	lines := readUntil(t, r, func(l string) bool { return l == ": heartbeat" })
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "id: 2\nevent: number\ndata: {\"n\":2}")
	assert.Contains(t, joined, "id: 4\nevent: number\ndata: {\"n\":4}")
	assert.NotContains(t, joined, `"n":1`, "per-event authorization filters odd events")
	assert.NotContains(t, joined, `"n":3`)

	// Client disconnect cancels the producer.
	cancel()
	select {
	case err := <-producerDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("producer not canceled after client disconnect")
	}
}

func TestSSEWithClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, streamRoutes(nil))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: h.srv.URL, AuthToken: h.loginToken("alice")})
	require.NoError(t, err)
	s, err := c.Stream(context.Background(), "/api/v1/stream/numbers", &struct {
		N   int  `query:"n" json:"-"`
		End bool `query:"end" json:"-"`
	}{N: 6, End: true}, nil)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	var got []int
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		assert.Equal(t, "number", ev.Name)
		var n numberEvent
		require.NoError(t, ev.Decode(&n))
		got = append(got, n.N)
	}
	assert.Equal(t, []int{2, 4, 6}, got)
	assert.Equal(t, "6", s.LastEventID)

	// Unauthenticated streams are refused before they start.
	c.SetToken("")
	_, err = c.Stream(context.Background(), "/api/v1/stream/numbers", nil, nil)
	require.Error(t, err)
	assert.True(t, apiclient.IsUnauthenticated(err))
}

func TestSSERevokedSessionEndsStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, streamRoutes(nil))
	tok := h.loginToken("alice")
	resp, r, _ := openStream(t, h, "/api/v1/stream/numbers?n=2", bearer(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readUntil(t, r, func(l string) bool { return strings.HasPrefix(l, "data: ") })

	p, err := h.tokens.Verify(context.Background(), tok)
	require.NoError(t, err)
	require.NoError(t, h.tokens.Revoke(context.Background(), p))

	lines := readUntil(t, r, func(l string) bool { return strings.HasPrefix(l, "data: ") && strings.Contains(l, "unauthenticated") })
	require.Contains(t, strings.Join(lines, "\n"), "event: error")
	var prob apiclient.Problem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[len(lines)-1], "data: ")), &prob))
	assert.Equal(t, http.StatusUnauthorized, prob.Status)
	assert.Contains(t, prob.Detail, "token revoked")

	// The server closes the stream after the error event.
	_, err = io.ReadAll(r)
	require.NoError(t, err)
}

// blockingWriter is a ResponseWriter whose writes block until released.
type blockingWriter struct {
	mu      sync.Mutex
	header  http.Header
	buf     strings.Builder
	release chan struct{}
	blocked chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "id:") || strings.HasPrefix(string(p), "data:") {
		w.once.Do(func() { close(w.blocked) })
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *blockingWriter) Flush() {}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestSSESlowConsumerIsDropped(t *testing.T) {
	t.Parallel()
	w := &blockingWriter{header: http.Header{}, release: make(chan struct{}), blocked: make(chan struct{})}
	r := httptest.NewRequest(http.MethodGet, "/stream", nil)
	sendErr := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		rest.ServeSSE(w, r, rest.StreamOptions[numberEvent]{Buffer: 2, Heartbeat: time.Hour, ReverifyInterval: -1},
			func(ctx context.Context, sink *rest.Sink[numberEvent]) error {
				if err := sink.Send(numberEvent{N: 0}); err != nil {
					return err
				}
				<-w.blocked // the writer is stuck on event 0
				for i := 1; ; i++ {
					if err := sink.Send(numberEvent{N: i}); err != nil {
						sendErr <- err
						return err
					}
				}
			})
	}()
	select {
	case err := <-sendErr:
		require.ErrorIs(t, err, rest.ErrSlowConsumer)
	case <-time.After(5 * time.Second):
		t.Fatal("producer was never told the consumer is too slow")
	}
	close(w.release)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}
	out := w.String()
	assert.Contains(t, out, "event: error")
	assert.Contains(t, out, `"code":"resource_exhausted"`)
}

func TestSSEProducerErrorAndPanic(t *testing.T) {
	t.Parallel()
	run := func(produce func(context.Context, *rest.Sink[numberEvent]) error) string {
		rec := httptest.NewRecorder()
		rest.ServeSSE(rec, httptest.NewRequest(http.MethodGet, "/s", nil), rest.StreamOptions[numberEvent]{ReverifyInterval: -1}, produce)
		return rec.Body.String()
	}
	out := run(func(_ context.Context, sink *rest.Sink[numberEvent]) error {
		require.NoError(t, sink.Send(numberEvent{N: 1}))
		return rest.NotFound("gone")
	})
	assert.Contains(t, out, "data: {\"n\":1}", "queued events are drained before the error")
	assert.Contains(t, out, "event: error")
	assert.Contains(t, out, `"code":"not_found"`)

	out = run(func(context.Context, *rest.Sink[numberEvent]) error { panic("producer bug") })
	assert.Contains(t, out, "event: error")
	assert.Contains(t, out, `"code":"internal"`)
	assert.NotContains(t, out, "producer bug")

	out = run(func(context.Context, *rest.Sink[numberEvent]) error { return nil })
	assert.NotContains(t, out, "event: error", "a producer that ends normally closes the stream quietly")
}
