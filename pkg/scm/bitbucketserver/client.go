package bitbucketserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

const (
	// maxErrorBody bounds how much of an error response is read to extract a message.
	maxErrorBody = 64 << 10
	// maxErrorMessage bounds the server-provided message carried by errors.
	maxErrorMessage = 512
	// defaultRateLimitWait is reported when a rate-limit response carries no reset hint.
	defaultRateLimitWait = time.Minute
)

// apiError is a non-2xx API response. It is the Err of the *scm.Error returned to callers.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// headerFunc sets authentication headers on an outgoing request.
type headerFunc func(ctx context.Context, h http.Header) error

// client is a small JSON-over-HTTP client bound to one API base URL.
type client struct {
	hc       *http.Client
	base     string // absolute API base URL ending in "/"
	host     string
	ua       string
	now      func() time.Time
	maxPages int
}

// newClient builds the HTTP stack shared by all requests of a provider instance:
// base transport (TLS, CA, proxy) → per-attempt timeout → response size cap →
// authentication pinned to the API host → scm.Chain (logging, bounded retries, pacing,
// observer).
func newClient(cfg scm.Config, base *url.URL, auth headerFunc) (*client, error) {
	rt, err := scm.BaseTransport(cfg)
	if err != nil {
		return nil, err
	}
	attemptTimeout := cfg.Timeout
	if cfg.HTTPClient != nil && cfg.HTTPClient.Timeout > 0 {
		attemptTimeout = cfg.HTTPClient.Timeout
	}
	// The per-attempt timeout sits below authentication and retries, so retry and
	// rate-limit waits are not charged to it (calls are bounded by their context and
	// Retry.CallBudget).
	limited := scm.LimitResponseBody(scm.AttemptTimeout(rt, attemptTimeout), cfg.MaxResponseBytes)
	inner := limited
	if auth != nil {
		inner = scm.HostGuard(base.Host, authTransport{next: limited, set: auth}, limited)
	}
	chained := scm.Chain(inner, scm.ChainOptions{
		Provider: Type,
		Logger:   cfg.Logger,
		Limiter:  scm.NewLimiter(cfg.RateLimit),
		Observer: cfg.Observer,
		Retry:    cfg.Retry,
		Clock:    cfg.Clock,
	})
	return &client{
		// No http.Client.Timeout: it would span every retry and rate-limit wait.
		hc:       &http.Client{Transport: chained},
		base:     base.String(),
		host:     base.Host,
		ua:       cfg.UserAgent,
		now:      cfg.Clock,
		maxPages: cfg.MaxPages,
	}, nil
}

// authTransport sets credentials on a clone of every request, so the caller's request is
// never mutated and credentials never reach outer layers (logging, observer).
type authTransport struct {
	next http.RoundTripper
	set  headerFunc
}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if err := t.set(req.Context(), r.Header); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.next.RoundTrip(r)
}

// tokenSource normalizes failures of a caller-supplied token function (for example a
// missing Secret) to KindUnauthorized, keeping context errors and *scm.Error values.
func tokenSource(tok scm.TokenFunc) scm.TokenFunc {
	return func(ctx context.Context) (string, error) {
		v, err := tok(ctx)
		if err == nil {
			return v, nil
		}
		var se *scm.Error
		if errors.As(err, &se) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", &scm.Error{Kind: scm.KindUnauthorized, Provider: Type, Op: "Token", Err: fmt.Errorf("token source failed: %w", err)}
	}
}

// url joins the API base with an already escaped relative path and a query.
func (c *client) url(path string, q url.Values) string {
	s := c.base + strings.TrimPrefix(path, "/")
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// do sends one request to rawURL, which must address the API host, and decodes a 2xx JSON
// body into out (nil discards the body). The returned response has its body closed; only
// the status and headers are meaningful. Non-2xx responses become *scm.Error.
func (c *client) do(ctx context.Context, op, method, rawURL string, out any) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, invalidErr(op, fmt.Errorf("invalid request URL: %w", err))
	}
	if !strings.EqualFold(u.Host, c.host) {
		return nil, invalidErr(op, fmt.Errorf("refusing to request host %q outside the configured API host", u.Host))
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, invalidErr(op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, op, err)
	}
	defer drainClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, c.statusError(op, resp)
	}
	if out != nil && method != http.MethodHead && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, decodeError(op, err)
		}
	}
	return resp, nil
}

// get is do with GET on a path relative to the API base.
func (c *client) get(ctx context.Context, op, path string, q url.Values, out any) (*http.Response, error) {
	return c.do(ctx, op, http.MethodGet, c.url(path, q), out)
}

func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func (c *client) statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &scm.Error{
		Provider:   Type,
		Op:         op,
		StatusCode: resp.StatusCode,
		Kind:       kindForStatus(resp.StatusCode),
		Err:        &apiError{Status: resp.StatusCode, Message: scm.TruncateRunes(errorMessage(body), maxErrorMessage)},
	}
	now := c.now()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if d, limited := scm.RateLimitWait(resp.Header, now); limited {
			e.Kind = scm.KindRateLimited
			e.RetryAfter = d
		}
	}
	switch e.Kind {
	case scm.KindRateLimited:
		if e.RetryAfter <= 0 {
			if d, ok := scm.ParseRetryAfter(resp.Header.Get("Retry-After"), now); ok && d > 0 {
				e.RetryAfter = d
			} else {
				e.RetryAfter = defaultRateLimitWait
			}
		}
	case scm.KindUnavailable:
		if d, ok := scm.ParseRetryAfter(resp.Header.Get("Retry-After"), now); ok {
			e.RetryAfter = d
		}
	}
	return e
}

func kindForStatus(status int) scm.ErrorKind {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return scm.KindNotFound
	case status == http.StatusUnauthorized:
		return scm.KindUnauthorized
	case status == http.StatusForbidden:
		return scm.KindForbidden
	case status == http.StatusConflict:
		return scm.KindConflict
	case status == http.StatusTooManyRequests:
		return scm.KindRateLimited
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return scm.KindInvalid
	case status >= 500:
		return scm.KindUnavailable
	case status >= 300 && status < 400:
		return scm.KindInvalid // unfollowed redirect
	}
	return scm.KindUnknown
}

// errorMessage extracts Bitbucket Server's error text
// ({"errors":[{"message":...,"exceptionName":...}]}) without echoing non-JSON bodies.
func errorMessage(body []byte) string {
	var v struct {
		Errors []struct {
			Message       string `json:"message"`
			ExceptionName string `json:"exceptionName"`
		} `json:"errors"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	var msgs []string
	for _, e := range v.Errors {
		if e.Message != "" {
			msgs = append(msgs, e.Message)
		}
	}
	if len(msgs) == 0 {
		return v.Message
	}
	return strings.Join(msgs, "; ")
}

func transportError(ctx context.Context, op string, err error) error {
	var se *scm.Error
	if errors.As(err, &se) {
		c := *se
		if c.Provider == "" {
			c.Provider = Type
		}
		if c.Op == "" {
			c.Op = op
		}
		return &c
	}
	e := &scm.Error{Provider: Type, Op: op, Err: err}
	switch {
	case ctx.Err() != nil:
		// Keep the context error recognizable; no kind so callers do not retry blindly.
	case errors.Is(err, scm.ErrResponseTooLarge):
		e.Kind = scm.KindInvalid
	default:
		e.Kind = scm.KindUnavailable
	}
	return e
}

func decodeError(op string, err error) error {
	if errors.Is(err, scm.ErrResponseTooLarge) {
		return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
	}
	return &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("malformed API response: %w", err)}
}

func invalidErr(op string, err error) error {
	return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
}

// fillOp sets Provider and Op on *scm.Error values produced by shared helpers
// (pagination limits) that do not know them.
func fillOp(op string, err error) error {
	var se *scm.Error
	if errors.As(err, &se) && (se.Provider == "" || se.Op == "" || se.Op == "Paginate") {
		c := *se
		c.Provider, c.Op = Type, op
		return &c
	}
	return err
}

// hasStatus reports whether err is an *scm.Error caused by one of the HTTP statuses.
func hasStatus(err error, codes ...int) bool {
	var se *scm.Error
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.StatusCode == c {
			return true
		}
	}
	return false
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q)+2)
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}
