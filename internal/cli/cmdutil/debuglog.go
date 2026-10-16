package cmdutil

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// withDebugLog returns a copy of hc whose requests are logged at debug level: method,
// host, path (user:password@ segments redacted), status, request ID and duration. Headers,
// queries and bodies, which carry tokens and credentials, are never logged.
func withDebugLog(hc *http.Client, l *slog.Logger) *http.Client {
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &debugTransport{base: base, log: l}
	return &c
}

type debugTransport struct {
	base http.RoundTripper
	log  *slog.Logger
}

var userinfoInPath = regexp.MustCompile(`[^/@:]+:[^/@]*@`)

// RedactPath removes "user:password@" segments (credentials embedded in repository
// URLs that end up in API paths).
func RedactPath(p string) string { return userinfoInPath.ReplaceAllString(p, "redacted@") }

func (d *debugTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := d.base.RoundTrip(r)
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("host", r.URL.Host),
		slog.String("path", output.Sanitize(RedactPath(r.URL.Path))),
		slog.Duration("duration", time.Since(start)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", output.Sanitize(RedactPath(err.Error()))))
	} else {
		attrs = append(attrs, slog.Int("status", resp.StatusCode))
		if id := resp.Header.Get(apiclient.RequestIDHeader); id != "" {
			attrs = append(attrs, slog.String("requestId", output.Sanitize(id)))
		}
	}
	d.log.LogAttrs(r.Context(), slog.LevelDebug, "API request", attrs...)
	return resp, err
}
