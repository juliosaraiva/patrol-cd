package core_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/cli/core"
)

func get(t *testing.T, c *http.Client, ctx context.Context, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kubernetes"+path, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	return resp
}

func TestHandlerTransportServesRequests(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /echo/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Remote", r.RemoteAddr)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "%s %s %s q=%s tls=%v", r.Method, r.PathValue("name"), b, r.URL.Query().Get("q"), r.TLS != nil)
	})
	mux.HandleFunc("GET /implicit", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"a":1}`) })
	mux.HandleFunc("GET /empty", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	ht := core.NewHandlerTransport(mux)
	c := &http.Client{Transport: ht}
	assert.True(t, core.IsInProcess(c))
	assert.False(t, core.IsInProcess(http.DefaultClient))

	resp, err := c.Post("http://kubernetes/echo/x?q=1", "text/plain", strings.NewReader("body"))
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "POST x body q=1 tls=false", string(b))
	assert.Equal(t, "in-process", resp.Header.Get("X-Remote"))

	resp = get(t, c, t.Context(), "/implicit")
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"a":1}`, string(b))
	assert.NotEmpty(t, resp.Header.Get("Content-Type"))

	resp = get(t, c, t.Context(), "/empty")
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, b)

	resp = get(t, c, t.Context(), "/missing")
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://kubernetes/panic", nil)
	_, err = c.Do(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panicked")

	require.NoError(t, ht.Close())
	_, err = c.Get("http://kubernetes/implicit")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in-process API server is closed")
}

// TestHandlerTransportStreams checks that streamed responses (Server-Sent Events) arrive
// as they are written, that closing the body ends the handler, and that Close ends a
// stream the client never closed.
func TestHandlerTransportStreams(t *testing.T) {
	t.Parallel()
	ended := make(chan string, 4)
	stream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				ended <- "write error"
				return
			}
			if err := rc.Flush(); err != nil {
				ended <- "flush error"
				return
			}
			select {
			case <-r.Context().Done():
				ended <- "canceled"
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	ht := core.NewHandlerTransport(http.HandlerFunc(stream))
	c := &http.Client{Transport: ht}

	resp := get(t, c, t.Context(), "/stream")
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	br := bufio.NewReader(resp.Body)
	for i := range 3 {
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("data: %d\n", i), line)
		_, _ = br.ReadString('\n')
	}
	require.NoError(t, resp.Body.Close())
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body did not end the handler")
	}

	// A stream left open is ended by Close, which waits for the handler.
	resp = get(t, c, t.Context(), "/stream")
	_, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, ht.Close())
	select {
	case <-ended:
	default:
		t.Fatal("Close returned before the handler ended")
	}
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err)
}

// TestHandlerTransportCancel: a request canceled before the handler answers fails with
// the context error and the handler sees the cancellation; SetWriteDeadline ends a write
// the client does not read.
func TestHandlerTransportCancel(t *testing.T) {
	t.Parallel()
	seen := make(chan error, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		seen <- r.Context().Err()
	})
	mux.HandleFunc("/deadline", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		require.NoError(t, rc.SetWriteDeadline(time.Now().Add(50*time.Millisecond)))
		_, err := w.Write([]byte("never read"))
		seen <- err
	})
	ht := core.NewHandlerTransport(mux)
	defer func() { _ = ht.Close() }()
	c := &http.Client{Transport: ht}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://kubernetes/slow", nil)
	_, err := c.Do(req)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Error(t, <-seen, "the handler sees the client's cancellation")

	resp := get(t, c, t.Context(), "/deadline")
	defer func() { _ = resp.Body.Close() }()
	select {
	case err := <-seen:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("write deadline not applied")
	}
}

// TestHandlerTransportWebSocket: upgrade requests are served through Hijack (the web
// terminal uses a WebSocket).
func TestHandlerTransportWebSocket(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		for {
			typ, msg, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if err := ws.Write(r.Context(), typ, append([]byte("echo: "), msg...)); err != nil {
				return
			}
		}
	})
	ht := core.NewHandlerTransport(h)
	c := &http.Client{Transport: ht}
	ws, resp, err := websocket.Dial(t.Context(), "ws://kubernetes/terminal", &websocket.DialOptions{HTTPClient: c})
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NoError(t, ws.Write(t.Context(), websocket.MessageText, []byte("hi")))
	_, msg, err := ws.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "echo: hi", string(msg))
	require.NoError(t, ht.Close(), "Close ends the hijacked connection and waits for the handler")
	_, _, err = ws.Read(t.Context())
	require.Error(t, err)

	// A plain request cannot hijack.
	ht2 := core.NewHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _, err := w.(http.Hijacker).Hijack()
		http.Error(w, err.Error(), http.StatusTeapot)
	}))
	defer func() { _ = ht2.Close() }()
	resp = get(t, &http.Client{Transport: ht2}, t.Context(), "/")
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusTeapot, resp.StatusCode)
	assert.Contains(t, string(b), "only upgrade requests")
	assert.False(t, errors.Is(err, io.EOF))
}
