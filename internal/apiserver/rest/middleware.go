package rest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first one is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// statusWriter records the status and size of a response. It forwards Flush and exposes
// the wrapped writer through Unwrap, so http.ResponseController keeps working (SSE).
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func wrapWriter(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w}
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the wrapped writer (http.ResponseController).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Recover turns handler panics into 500 problem responses and keeps serving. The panic
// value and stack are logged (no request data). http.ErrAbortHandler is re-raised, as
// net/http expects.
func Recover(lg *slog.Logger, m *Metrics) Middleware {
	if lg == nil {
		lg = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := wrapWriter(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				m.panicked()
				// Recover runs outside RequestIDs: recover the ID from the response header.
				ctx := r.Context()
				if RequestID(ctx) == "" {
					if id := sw.Header().Get(RequestIDHeader); id != "" {
						ctx = WithRequestID(ctx, id)
					}
				}
				lg.LogAttrs(ctx, slog.LevelError, "handler panic",
					slog.String("requestId", RequestID(ctx)),
					slog.String("panic", fmt.Sprint(v)),
					slog.String("stack", string(debug.Stack())))
				if !sw.wroteHeader {
					WriteError(sw, r.WithContext(ctx), Internal(fmt.Errorf("panic: %v", v)))
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// RequestIDHeader carries the request ID in requests and responses.
const RequestIDHeader = "X-Request-Id"

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestIDs assigns every request an ID: a well-formed incoming X-Request-Id (1-64
// characters of [A-Za-z0-9._-]) is kept, otherwise a random UUID is generated. The ID is
// echoed in the response header, problem documents, logs and audit entries.
func RequestIDs() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !requestIDRE.MatchString(id) {
				id = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
		})
	}
}

// accessInfo is filled by the route chain for the access log.
type accessInfo struct {
	route atomic.Pointer[string]
	actor atomic.Pointer[string]
}

type accessKey struct{}

func accessInfoFrom(ctx context.Context) *accessInfo {
	ai, _ := ctx.Value(accessKey{}).(*accessInfo)
	return ai
}

// AccessLogConfig configures AccessLog.
type AccessLogConfig struct {
	Logger *slog.Logger
	// QuietPaths are logged at debug level (e.g. "/healthz").
	QuietPaths []string
	// ClientIP derives the client address; nil uses RemoteAddr.
	ClientIP func(*http.Request) string
}

// AccessLog logs one line per request: request ID, method, route pattern (or the path of
// non-API requests), status, size, duration, client address and actor. Query strings,
// headers, bodies and tokens are never logged.
func AccessLog(cfg AccessLogConfig) Middleware {
	lg := cfg.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ai := &accessInfo{}
			sw := wrapWriter(w)
			ctx := context.WithValue(r.Context(), accessKey{}, ai)
			next.ServeHTTP(sw, r.WithContext(ctx))
			route := "unmatched"
			if p := ai.route.Load(); p != nil {
				route = *p
			} else if !strings.HasPrefix(r.URL.Path, "/api/") {
				route = truncatePath(r.URL.Path)
			}
			actor := ""
			if p := ai.actor.Load(); p != nil {
				actor = *p
			}
			level := slog.LevelInfo
			for _, q := range cfg.QuietPaths {
				if r.URL.Path == q {
					level = slog.LevelDebug
				}
			}
			client := r.RemoteAddr
			if cfg.ClientIP != nil {
				client = cfg.ClientIP(r)
			}
			lg.LogAttrs(r.Context(), level, "request",
				slog.String("requestId", RequestID(ctx)),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", sw.code()),
				slog.Int64("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("client", client),
				slog.String("actor", actor),
			)
		})
	}
}

func truncatePath(p string) string {
	const maxLen = 256
	if len(p) > maxLen {
		return p[:maxLen] + "…"
	}
	return p
}

// SecurityHeadersConfig configures SecurityHeaders.
type SecurityHeadersConfig struct {
	// FrameOptions is the X-Frame-Options value ("" omits the header).
	FrameOptions string
	// ContentSecurityPolicy is the CSP value ("" omits the header).
	ContentSecurityPolicy string
	// HSTS sets Strict-Transport-Security on TLS requests.
	HSTS bool
}

// Defaults of SecurityHeadersConfig.
const (
	DefaultFrameOptions          = "sameorigin"
	DefaultContentSecurityPolicy = "default-src 'self'; frame-ancestors 'self'"
	hstsValue                    = "max-age=31536000"
)

// SecurityHeaders sets X-Content-Type-Options, Referrer-Policy, X-Frame-Options,
// Content-Security-Policy and, for TLS requests, Strict-Transport-Security.
func SecurityHeaders(cfg SecurityHeadersConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			if cfg.FrameOptions != "" {
				h.Set("X-Frame-Options", cfg.FrameOptions)
			}
			if cfg.ContentSecurityPolicy != "" {
				h.Set("Content-Security-Policy", cfg.ContentSecurityPolicy)
			}
			if cfg.HSTS && r.TLS != nil {
				h.Set("Strict-Transport-Security", hstsValue)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBody caps every request body at n bytes (routes apply lower caps of their own).
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n > 0 && r.Body != nil && r.Body != http.NoBody {
				if r.ContentLength > n {
					WriteError(w, r, &Error{Status: http.StatusRequestEntityTooLarge, Code: apiclient.CodeResourceExhausted, Detail: "request body too large"})
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
