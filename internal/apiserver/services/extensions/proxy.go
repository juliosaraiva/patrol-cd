package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// DefaultAllowedHeaders are the request headers forwarded to extension backends.
var DefaultAllowedHeaders = []string{
	"Accept", "Accept-Encoding", "Accept-Language", "Cache-Control", "Pragma",
	"Content-Type", "Content-Encoding", "Content-Language",
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range", "Range",
	"User-Agent", "Traceparent", "Tracestate",
	apiclient.HeaderExtensionApplication, apiclient.HeaderExtensionProject,
}

// neverForwarded are request headers that are never forwarded, whatever the allow-list:
// the caller's credentials, proxy information and the identity headers the server sets.
var neverForwarded = map[string]bool{
	"Authorization": true, "Cookie": true, "Proxy-Authorization": true,
	"Forwarded": true, "X-Forwarded-For": true, "X-Forwarded-Host": true, "X-Forwarded-Proto": true,
	"Host": true, "Connection": true, "Upgrade": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Keep-Alive": true, "Proxy-Connection": true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionNamespace):         true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionTargetClusterName): true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionTargetClusterURL):  true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionUserID):            true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionUsername):          true,
	http.CanonicalHeaderKey(apiclient.HeaderExtensionUserGroups):        true,
}

// webSocketHeaders are forwarded on upgrades when WebSockets are enabled.
var webSocketHeaders = []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions", "Origin"}

func allowedHeaders(extra []string) map[string]bool {
	out := map[string]bool{}
	for _, h := range append(append([]string(nil), DefaultAllowedHeaders...), extra...) {
		k := http.CanonicalHeaderKey(strings.TrimSpace(h))
		if k != "" && !neverForwarded[k] {
			out[k] = true
		}
	}
	return out
}

// headerSafe drops characters that cannot appear in header values.
func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, v)
}

// outgoingHeaders builds the forwarded request headers from base (the request headers
// after the reverse proxy removed hop-by-hop ones): the allow-listed headers, the upgrade
// headers when WebSockets are forwarded, the server's identity and destination headers,
// then the configured headers.
func (s *Service) outgoingHeaders(ctx context.Context, base http.Header, be *backend, app *v1alpha1.Application, upgrade bool) http.Header {
	h := http.Header{}
	for k, vs := range base {
		if s.allowed[k] {
			h[k] = append([]string(nil), vs...)
		}
	}
	if upgrade {
		h.Set("Connection", "Upgrade")
		h.Set("Upgrade", base.Get("Upgrade"))
		for _, k := range webSocketHeaders {
			if vs := base.Values(k); len(vs) > 0 {
				h[k] = append([]string(nil), vs...)
			}
		}
	}
	if id := rest.RequestID(ctx); id != "" {
		h.Set(rest.RequestIDHeader, id)
	}
	h.Set(apiclient.HeaderExtensionNamespace, s.d.Config.Namespace)
	if app.Spec.Destination.Name != "" {
		h.Set(apiclient.HeaderExtensionTargetClusterName, headerSafe(app.Spec.Destination.Name))
	}
	if app.Spec.Destination.Server != "" {
		h.Set(apiclient.HeaderExtensionTargetClusterURL, headerSafe(app.Spec.Destination.Server))
	}
	if id := rest.IdentityFrom(ctx); id != nil {
		if p := id.Principal; p != nil {
			if p.Subject != "" {
				h.Set(apiclient.HeaderExtensionUserID, headerSafe(p.Subject))
			}
			if u := p.Username(); u != "" {
				h.Set(apiclient.HeaderExtensionUsername, headerSafe(u))
			}
		}
		if groups := id.Groups(); len(groups) > 0 {
			h.Set(apiclient.HeaderExtensionUserGroups, headerSafe(strings.Join(groups, ",")))
		}
	}
	// Configured headers last: they may override any other (upstream).
	for _, ch := range be.headers {
		h.Set(ch.Name, headerSafe(ch.Value.Reveal()))
	}
	return h
}

// forwardPath returns the path after "/extensions/<name>" in decoded and escaped form.
func forwardPath(r *http.Request, ext string) (string, string) {
	prefix := "/extensions/" + ext
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if escaped == "" {
		escaped = "/"
	}
	p, err := url.PathUnescape(escaped)
	if err != nil {
		p = strings.TrimPrefix(r.URL.Path, prefix)
	}
	if p == escaped {
		escaped = ""
	}
	return p, escaped
}

var (
	errResponseTooLarge = errors.New("extension response exceeds the size limit")
	errBadGateway       = errors.New("bad gateway")
)

// limitedBody fails the copy once more than limit bytes were read, which aborts the
// client connection (the response cannot be completed).
type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var one [1]byte
		if n, _ := b.rc.Read(one[:]); n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// statusRecorder captures the response status for metrics; Unwrap keeps flushing and
// hijacking (WebSockets) available to the reverse proxy.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// forward proxies r to the backend.
func (s *Service) forward(w http.ResponseWriter, r *http.Request, e *extension, be *backend, app *v1alpha1.Application, upgrade bool) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	ctx := r.Context()
	if !upgrade {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.d.Config.RequestTimeout)
		defer cancel()
	}
	if r.Body != nil && r.Body != http.NoBody {
		if r.ContentLength > s.d.Config.MaxRequestBytes {
			rest.WriteError(w, r, &rest.Error{Status: http.StatusRequestEntityTooLarge, Code: apiclient.CodeResourceExhausted, Detail: "request body too large"})
			s.metrics.observe(e.name, http.StatusRequestEntityTooLarge, time.Since(start))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.d.Config.MaxRequestBytes)
	}
	path, rawPath := forwardPath(r, e.name)
	maxResp := s.d.Config.MaxResponseBytes
	proxy := &httputil.ReverseProxy{
		Transport: e.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme, out.URL.Host = be.target.Scheme, be.target.Host
			out.URL.Path, out.URL.RawPath = path, rawPath
			out.URL.RawQuery = pr.In.URL.RawQuery
			out.URL.User = nil
			out.Host = be.target.Host
			out.Header = s.outgoingHeaders(pr.In.Context(), pr.Out.Header, be, app, upgrade)
		},
		ModifyResponse: func(resp *http.Response) error {
			// A backend must not set cookies on the Patrol origin (session fixation).
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return nil
			}
			if resp.ContentLength > maxResp {
				return errResponseTooLarge
			}
			resp.Body = &limitedBody{rc: resp.Body, remaining: maxResp}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			s.proxyError(w, r, e.name, err)
		},
		ErrorLog: log.New(slogWriter{s.d.Logger}, "", 0),
	}
	defer func() {
		status := rec.status
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				s.d.Logger.LogAttrs(ctx, slog.LevelWarn, "proxy extension response aborted",
					slog.String("requestId", rest.RequestID(ctx)), slog.String("extension", e.name))
				s.metrics.observe(e.name, http.StatusBadGateway, time.Since(start))
			}
			panic(v)
		}
		if status == 0 {
			status = http.StatusOK
		}
		s.metrics.observe(e.name, status, time.Since(start))
	}()
	proxy.ServeHTTP(rec, r.WithContext(ctx))
}

// proxyError answers a failed exchange without revealing backend details.
func (s *Service) proxyError(w http.ResponseWriter, r *http.Request, ext string, err error) {
	ctx := r.Context()
	var maxErr *http.MaxBytesError
	var e *rest.Error
	switch {
	case errors.As(err, &maxErr):
		e = &rest.Error{Status: http.StatusRequestEntityTooLarge, Code: apiclient.CodeResourceExhausted, Detail: "request body too large"}
	case errors.Is(err, errResponseTooLarge):
		e = &rest.Error{Status: http.StatusBadGateway, Code: apiclient.CodeUnavailable, Detail: "the extension response exceeds the size limit"}
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		e = &rest.Error{Status: http.StatusGatewayTimeout, Code: apiclient.CodeUnavailable, Detail: "the extension did not respond in time"}
	case errors.Is(err, context.Canceled):
		// The client went away; nothing useful can be written.
		e = &rest.Error{Status: 499, Code: apiclient.CodeUnavailable, Detail: "request canceled"}
	default:
		e = &rest.Error{Status: http.StatusBadGateway, Code: apiclient.CodeUnavailable, Detail: "the extension backend is unavailable"}
	}
	e.Cause = fmt.Errorf("%w: %v", errBadGateway, err)
	s.d.Logger.LogAttrs(ctx, slog.LevelWarn, "proxy extension request failed",
		slog.String("requestId", rest.RequestID(ctx)), slog.String("extension", ext),
		slog.Int("status", e.Status), slog.String("error", err.Error()))
	if a := rest.AuditFrom(ctx); a != nil {
		a.SetReason(e.Detail)
	}
	rest.WriteError(w, r, e)
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// slogWriter adapts the reverse proxy's error log to slog (debug level: copy errors after
// the response started; never request data).
type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.l.Debug("proxy extension", slog.String("detail", strings.TrimSpace(string(p))))
	return len(p), nil
}

// statusText is used by metrics labels.
func statusText(code int) string { return strconv.Itoa(code) }
