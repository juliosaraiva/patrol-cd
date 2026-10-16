package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// WriteJSON writes v as an application/json response with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	if v == nil || (reflect.ValueOf(v).Kind() == reflect.Pointer && reflect.ValueOf(v).IsNil()) {
		v = struct{}{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		writeProblemDoc(w, nil, Internal(err), "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// WriteError writes err as an RFC 9457 problem document. 5xx errors are logged with
// their cause and the request ID; the cause is never returned to the client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e := ProblemFor(err)
	if e == nil {
		e = Internal(nil)
	}
	ctx := r.Context()
	if e.Status >= 500 && !isClientGone(ctx) {
		logger(ctx).LogAttrs(ctx, slog.LevelError, "request failed",
			slog.String("requestId", RequestID(ctx)),
			slog.String("route", routeName(ctx, r)),
			slog.Int("status", e.Status),
			slog.Any("error", e.Cause))
	}
	writeProblemDoc(w, r, e, RequestID(ctx))
}

func writeProblemDoc(w http.ResponseWriter, r *http.Request, e *Error, requestID string) {
	code := e.Code
	if code == "" {
		code = apiclient.CodeForStatus(e.Status)
	}
	p := apiclient.Problem{
		Type:      apiclient.ProblemType(code),
		Title:     http.StatusText(e.Status),
		Status:    e.Status,
		Detail:    e.Detail,
		Code:      code,
		RequestID: requestID,
	}
	if r != nil {
		p.Instance = r.URL.Path
	}
	h := w.Header()
	h.Set("Content-Type", apiclient.ProblemContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	if len(e.Allow) > 0 {
		h.Set("Allow", strings.Join(e.Allow, ", "))
	}
	if e.Status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="patrol"`)
	}
	b, err := json.Marshal(p)
	if err != nil {
		b = []byte(`{"title":"Internal Server Error","status":500,"code":"internal"}`)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(append(b, '\n'))
}

func isClientGone(ctx context.Context) bool { return ctx.Err() != nil }

func logger(ctx context.Context) *slog.Logger {
	if st := stateFrom(ctx); st != nil && st.env != nil && st.env.Logger != nil {
		return st.env.Logger
	}
	return slog.Default()
}

func routeName(ctx context.Context, r *http.Request) string {
	if rt := RouteFrom(ctx); rt != nil {
		return rt.Pattern()
	}
	if r != nil {
		return r.Method + " (unmatched)"
	}
	return ""
}
