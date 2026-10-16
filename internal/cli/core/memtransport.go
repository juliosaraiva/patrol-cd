package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errTransportClosed is returned for requests sent after Close.
var errTransportClosed = errors.New("core mode: the in-process API server is closed")

// transportCloseTimeout bounds how long Close waits for handlers to return.
const transportCloseTimeout = 10 * time.Second

// HandlerTransport is an http.RoundTripper that serves every request with an http.Handler
// in the CLI process: no socket is opened and no port is bound, so the in-process API
// server is unreachable for other processes. Response bodies stream (watch and log
// streams), WebSocket upgrades are served through http.Hijacker (net.Pipe), and Close
// cancels the requests in flight and waits for their handlers.
type HandlerTransport struct {
	h      http.Handler
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	open   map[*exchange]struct{}
}

// NewHandlerTransport returns a transport serving requests with h.
func NewHandlerTransport(h http.Handler) *HandlerTransport {
	ctx, cancel := context.WithCancel(context.Background())
	return &HandlerTransport{h: h, ctx: ctx, cancel: cancel, open: map[*exchange]struct{}{}}
}

// RoundTrip implements http.RoundTripper.
func (t *HandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		closeRequestBody(req)
		return nil, errors.New("core mode: request without URL")
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		closeRequestBody(req)
		return nil, errTransportClosed
	}
	ctx, cancel := context.WithCancel(req.Context())
	unwatch := context.AfterFunc(t.ctx, cancel)
	pr, pw := io.Pipe()
	x := &exchange{
		req: req, cancel: cancel, header: http.Header{}, ready: make(chan struct{}), pr: pr, pw: pw,
		upgrade: isUpgrade(req),
	}
	t.open[x] = struct{}{}
	t.wg.Add(1)
	t.mu.Unlock()

	go t.serve(x, serverRequest(ctx, req), unwatch)

	select {
	case <-x.ready:
	case <-ctx.Done():
		x.abort(ctx.Err())
		return nil, ctx.Err()
	}
	return x.response()
}

// serve runs the handler of one exchange.
func (t *HandlerTransport) serve(x *exchange, r *http.Request, unwatch func() bool) {
	defer t.wg.Done()
	defer func() {
		unwatch()
		x.cancel()
		closeRequestBody(x.req)
		t.mu.Lock()
		delete(t.open, x)
		t.mu.Unlock()
	}()
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared like net/http does
				x.abort(errors.New("core mode: the in-process handler aborted the response"))
				return
			}
			x.fail(fmt.Errorf("core mode: the in-process handler panicked: %v", p))
		}
	}()
	t.h.ServeHTTP(x, r)
	x.finish()
}

// Close cancels the requests in flight, ends their response bodies and hijacked
// connections, and waits (bounded) for the handlers to return. Later requests fail.
func (t *HandlerTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	open := make([]*exchange, 0, len(t.open))
	for x := range t.open {
		open = append(open, x)
	}
	t.mu.Unlock()
	t.cancel()
	for _, x := range open {
		x.abort(errTransportClosed)
	}
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(transportCloseTimeout):
		return fmt.Errorf("core mode: in-process handlers still running %s after close", transportCloseTimeout)
	}
}

func closeRequestBody(req *http.Request) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
}

func isUpgrade(req *http.Request) bool {
	if req.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// serverRequest turns a client request into the request a server handler receives.
func serverRequest(ctx context.Context, req *http.Request) *http.Request {
	r := req.Clone(ctx)
	r.URL = &url.URL{Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	r.RequestURI = req.URL.RequestURI()
	if r.Host == "" {
		r.Host = req.URL.Host
	}
	r.RemoteAddr = "in-process"
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/1.1", 1, 1
	r.TLS = nil
	r.Close = false
	if r.Body == nil {
		r.Body = http.NoBody
	}
	r.GetBody = nil
	return r
}

// exchange is one request: the server-side http.ResponseWriter and the client-side
// response it produces.
type exchange struct {
	req     *http.Request
	cancel  context.CancelFunc
	upgrade bool

	mu          sync.Mutex
	header      http.Header
	sent        http.Header // header snapshot taken when the status was committed
	status      int
	wroteHeader bool
	failure     error
	deadline    *time.Timer
	// client and server are the ends of a hijacked connection (WebSocket upgrades).
	client, server net.Conn

	ready     chan struct{}
	readyOnce sync.Once
	pr        *io.PipeReader
	pw        *io.PipeWriter
}

func (x *exchange) markReady() { x.readyOnce.Do(func() { close(x.ready) }) }

// Header implements http.ResponseWriter.
func (x *exchange) Header() http.Header { return x.header }

// WriteHeader implements http.ResponseWriter.
func (x *exchange) WriteHeader(code int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.writeHeaderLocked(code)
}

func (x *exchange) writeHeaderLocked(code int) {
	if x.wroteHeader || x.client != nil {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return // informational answers are not relayed
	}
	x.status, x.wroteHeader = code, true
	x.sent = x.header.Clone()
	if code != http.StatusSwitchingProtocols {
		x.markReady()
	}
}

// Write implements http.ResponseWriter. Writes block until the client reads them (or
// closes the body), so nothing is buffered.
func (x *exchange) Write(b []byte) (int, error) {
	x.mu.Lock()
	if x.client != nil {
		x.mu.Unlock()
		return 0, http.ErrHijacked
	}
	if !x.wroteHeader {
		if x.header.Get("Content-Type") == "" && len(b) > 0 {
			x.header.Set("Content-Type", http.DetectContentType(b))
		}
		x.writeHeaderLocked(http.StatusOK)
	}
	x.mu.Unlock()
	return x.pw.Write(b)
}

// FlushError implements the flushing interface of http.ResponseController: the header is
// committed; written data has already reached the client.
func (x *exchange) FlushError() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.client != nil {
		return http.ErrHijacked
	}
	x.writeHeaderLocked(http.StatusOK)
	return nil
}

// Flush implements http.Flusher.
func (x *exchange) Flush() { _ = x.FlushError() }

// SetWriteDeadline implements the deadline interface of http.ResponseController: a write
// still blocked at the deadline (a client not reading) ends the response.
func (x *exchange) SetWriteDeadline(d time.Time) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.deadline != nil {
		x.deadline.Stop()
		x.deadline = nil
	}
	if d.IsZero() {
		return nil
	}
	x.deadline = time.AfterFunc(time.Until(d), func() {
		_ = x.pw.CloseWithError(fmt.Errorf("core mode: in-process response write: %w", os.ErrDeadlineExceeded))
	})
	return nil
}

// Hijack implements http.Hijacker for upgrade requests (WebSocket): the handler gets one
// end of an in-memory connection, the client the other.
func (x *exchange) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case x.client != nil:
		return nil, nil, http.ErrHijacked
	case !x.upgrade:
		return nil, nil, errors.New("core mode: only upgrade requests can be hijacked")
	case x.wroteHeader && x.status != http.StatusSwitchingProtocols:
		return nil, nil, errors.New("core mode: the response was already started")
	}
	x.client, x.server = net.Pipe()
	x.markReady()
	return x.server, bufio.NewReadWriter(bufio.NewReader(x.server), bufio.NewWriter(x.server)), nil
}

// finish completes the response after the handler returned.
func (x *exchange) finish() {
	x.mu.Lock()
	hijacked := x.client != nil
	if !hijacked {
		x.writeHeaderLocked(http.StatusOK)
	}
	if x.deadline != nil {
		x.deadline.Stop()
	}
	x.mu.Unlock()
	x.markReady()
	if !hijacked {
		_ = x.pw.Close()
	}
}

// fail ends the exchange after a handler panic: a 500 when nothing was sent yet,
// otherwise a broken body.
func (x *exchange) fail(err error) {
	x.mu.Lock()
	if !x.wroteHeader && x.client == nil {
		x.failure = err
	}
	if x.server != nil {
		_ = x.server.Close()
	}
	x.mu.Unlock()
	x.markReady()
	_ = x.pw.CloseWithError(err)
}

// abort ends the exchange from the client side (cancellation, transport closed).
func (x *exchange) abort(err error) {
	x.cancel()
	_ = x.pr.CloseWithError(err)
	_ = x.pw.CloseWithError(err)
	x.mu.Lock()
	if x.client != nil {
		_ = x.client.Close()
		_ = x.server.Close()
	}
	x.mu.Unlock()
}

// response builds the client response once the exchange is ready.
func (x *exchange) response() (*http.Response, error) {
	x.mu.Lock()
	client, failure, status, wrote := x.client, x.failure, x.status, x.wroteHeader
	header := x.sent
	x.mu.Unlock()
	if failure != nil {
		x.abort(failure)
		return nil, failure
	}
	if client != nil {
		return x.upgraded(client, wrote, status, header)
	}
	if !wrote {
		// Ready without a status: the handler panicked or was aborted.
		x.abort(errTransportClosed)
		return nil, errors.New("core mode: the in-process handler returned no response")
	}
	resp := &http.Response{
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: header, Body: &responseBody{x: x}, ContentLength: -1, Request: x.req,
	}
	if cl, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && cl >= 0 {
		resp.ContentLength = cl
	}
	if x.req.Method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified {
		_ = resp.Body.Close()
		resp.Body = http.NoBody
		if status != http.StatusOK || x.req.Method != http.MethodHead {
			resp.ContentLength = 0
		}
	}
	return resp, nil
}

// upgraded returns the response of a hijacked exchange: the 101 the handler committed
// before hijacking, else the response it wrote on the connection.
func (x *exchange) upgraded(client net.Conn, wrote bool, status int, header http.Header) (*http.Response, error) {
	if wrote && status == http.StatusSwitchingProtocols {
		return &http.Response{
			Status:     "101 " + http.StatusText(http.StatusSwitchingProtocols),
			StatusCode: http.StatusSwitchingProtocols, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: header, Body: &upgradedBody{Reader: client, conn: client}, ContentLength: -1, Request: x.req,
		}, nil
	}
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, x.req)
	if err != nil {
		x.abort(err)
		return nil, fmt.Errorf("core mode: read the upgrade response: %w", err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = &upgradedBody{Reader: br, conn: client}
	}
	return resp, nil
}

// responseBody is the client side of a response pipe; closing it ends the handler's
// request context.
type responseBody struct {
	x    *exchange
	once sync.Once
}

func (b *responseBody) Read(p []byte) (int, error) { return b.x.pr.Read(p) }

func (b *responseBody) Close() error {
	b.once.Do(func() {
		b.x.cancel()
		_ = b.x.pr.Close()
	})
	return nil
}

// upgradedBody is the io.ReadWriteCloser of a 101 response (WebSocket clients write
// through the response body).
type upgradedBody struct {
	io.Reader
	conn net.Conn
}

func (b *upgradedBody) Write(p []byte) (int, error) { return b.conn.Write(p) }

func (b *upgradedBody) Close() error { return b.conn.Close() }
