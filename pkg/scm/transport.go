package scm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// BaseTransport returns the innermost transport for cfg: cfg.HTTPClient.Transport when
// an HTTP client is supplied, otherwise a clone of http.DefaultTransport with TLS
// (minimum TLS 1.2, CAData appended to the system pool, Insecure) and proxy settings.
func BaseTransport(cfg Config) (http.RoundTripper, error) {
	if cfg.HTTPClient != nil {
		if cfg.HTTPClient.Transport != nil {
			return cfg.HTTPClient.Transport, nil
		}
		return http.DefaultTransport, nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("scm: http.DefaultTransport is not *http.Transport")
	}
	t := base.Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit operator opt-in, mirrors upstream `insecure`
	}
	if len(cfg.CAData) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(cfg.CAData) {
			return nil, &Error{Kind: KindInvalid, Op: "Config", Err: errors.New("CAData contains no valid PEM certificates")}
		}
		tlsCfg.RootCAs = pool
	}
	t.TLSClientConfig = tlsCfg
	if cfg.ProxyURL != "" {
		proxy, err := ProxyFunc(cfg.ProxyURL, cfg.NoProxy)
		if err != nil {
			return nil, err
		}
		t.Proxy = proxy
	}
	return t, nil
}

// ProxyFunc returns an http.Transport Proxy function that sends every request through
// proxyURL except hosts matched by noProxy (comma-separated entries: "*", host names,
// ".domain" or "domain" suffixes, IP addresses, CIDRs, each optionally with ":port").
func ProxyFunc(proxyURL, noProxy string) (func(*http.Request) (*url.URL, error), error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
		return nil, &Error{Kind: KindInvalid, Op: "Config", Err: fmt.Errorf("invalid proxy URL %q", RedactURLString(proxyURL))}
	}
	m := parseNoProxy(noProxy)
	return func(req *http.Request) (*url.URL, error) {
		if m.bypass(req.URL) {
			return nil, nil
		}
		return u, nil
	}, nil
}

type noProxyEntry struct {
	all    bool
	cidr   *net.IPNet
	ip     net.IP
	domain string // lowercase, without leading dot
	exact  bool   // entry had no leading dot and is matched exactly or as parent domain
	port   string
}

type noProxyMatcher []noProxyEntry

func parseNoProxy(s string) noProxyMatcher {
	var out noProxyMatcher
	for raw := range strings.SplitSeq(s, ",") {
		e := strings.ToLower(strings.TrimSpace(raw))
		if e == "" {
			continue
		}
		if e == "*" {
			out = append(out, noProxyEntry{all: true})
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, noProxyEntry{cidr: n})
			continue
		}
		host, port := e, ""
		if h, p, err := net.SplitHostPort(e); err == nil {
			host, port = h, p
		}
		if ip := net.ParseIP(host); ip != nil {
			out = append(out, noProxyEntry{ip: ip, port: port})
			continue
		}
		exact := !strings.HasPrefix(host, ".")
		out = append(out, noProxyEntry{domain: strings.TrimPrefix(strings.TrimPrefix(host, "*"), "."), exact: exact, port: port})
	}
	return out
}

func (m noProxyMatcher) bypass(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ip := net.ParseIP(host)
	for _, e := range m {
		switch {
		case e.all:
			return true
		case e.cidr != nil:
			if ip != nil && e.cidr.Contains(ip) {
				return true
			}
		case e.ip != nil:
			if ip != nil && e.ip.Equal(ip) && (e.port == "" || e.port == port) {
				return true
			}
		case e.domain != "":
			if e.port != "" && e.port != port {
				continue
			}
			if host == e.domain && e.exact {
				return true
			}
			if strings.HasSuffix(host, "."+e.domain) {
				return true
			}
		}
	}
	return false
}

// LimitResponseBody wraps rt so that every response body fails with ErrResponseTooLarge
// once more than max bytes are read. Responses announcing a larger Content-Length fail
// immediately.
func LimitResponseBody(rt http.RoundTripper, maxBytes int64) http.RoundTripper {
	if maxBytes <= 0 {
		return rt
	}
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := rt.RoundTrip(req)
		if err != nil || resp == nil || resp.Body == nil {
			return resp, err
		}
		if resp.ContentLength > maxBytes {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: content length %d > %d", ErrResponseTooLarge, resp.ContentLength, maxBytes)
		}
		resp.Body = &limitedBody{rc: resp.Body, remaining: maxBytes}
		return resp, nil
	})
}

type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, ErrResponseTooLarge
	}
	// Read one byte beyond the limit to detect overflow.
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.rc.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n + int(l.remaining), ErrResponseTooLarge
	}
	return n, err
}

func (l *limitedBody) Close() error { return l.rc.Close() }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// NewLimiter returns a token-bucket limiter for rl, or nil when pacing is disabled
// (RPS < 0). Zero values select the defaults.
func NewLimiter(rl RateLimit) *rate.Limiter {
	if rl.RPS < 0 {
		return nil
	}
	rps, burst := rl.RPS, rl.Burst
	if rps == 0 {
		rps = DefaultRateLimitRPS
	}
	if burst <= 0 {
		burst = DefaultRateLimitBurst
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

// RetryDecision is the outcome of classifying a response for retry.
type RetryDecision struct {
	Retry bool
	// Wait is the provider-advised wait (Retry-After, rate-limit reset). Zero means use
	// exponential backoff.
	Wait time.Duration
	// RateLimited marks waits caused by rate limiting (logged at warn level).
	RateLimited bool
}

// RetryClassifier decides whether a response should be retried.
type RetryClassifier func(resp *http.Response, now time.Time) RetryDecision

// DefaultRetryClassifier retries 429, 502, 503, 504 and 500 with backoff, and primary
// rate-limit responses (403/429 with a zero remaining-quota header) by waiting until the
// advertised reset. It understands Retry-After (seconds or HTTP date),
// X-RateLimit-Remaining/X-RateLimit-Reset (GitHub, Gitea; epoch seconds) and
// RateLimit-Remaining/RateLimit-Reset (GitLab; epoch seconds).
func DefaultRetryClassifier(resp *http.Response, now time.Time) RetryDecision {
	if resp == nil {
		return RetryDecision{}
	}
	if wait, limited := RateLimitWait(resp.Header, now); limited {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return RetryDecision{Retry: true, Wait: wait, RateLimited: true}
		}
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		wait, _ := ParseRetryAfter(resp.Header.Get("Retry-After"), now)
		return RetryDecision{Retry: true, Wait: wait, RateLimited: true}
	case http.StatusForbidden:
		// GitHub secondary rate limits are 403s carrying Retry-After.
		if wait, ok := ParseRetryAfter(resp.Header.Get("Retry-After"), now); ok {
			return RetryDecision{Retry: true, Wait: wait, RateLimited: true}
		}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		wait, _ := ParseRetryAfter(resp.Header.Get("Retry-After"), now)
		return RetryDecision{Retry: true, Wait: wait}
	}
	return RetryDecision{}
}

// ParseRetryAfter parses a Retry-After header (delta seconds or HTTP date).
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int64(24*time.Hour/time.Second) {
			secs = int64(24 * time.Hour / time.Second)
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// RateLimitWait reports whether headers announce an exhausted primary rate limit and how
// long until it resets. A Retry-After header takes precedence over the reset time.
func RateLimitWait(h http.Header, now time.Time) (time.Duration, bool) {
	remaining := h.Get("X-RateLimit-Remaining")
	reset := h.Get("X-RateLimit-Reset")
	if remaining == "" {
		remaining = h.Get("RateLimit-Remaining")
		reset = h.Get("RateLimit-Reset")
	}
	if strings.TrimSpace(remaining) != "0" {
		return 0, false
	}
	if d, ok := ParseRetryAfter(h.Get("Retry-After"), now); ok {
		return d, true
	}
	if epoch, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil && epoch > 0 {
		d := time.Unix(epoch, 0).Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, true
}

// RequestInfo describes one HTTP attempt made by a provider. It carries no headers, bodies
// or credentials.
type RequestInfo struct {
	Provider string
	Method   string
	Host     string
	// Path is the URL path without query string. It may contain organization, repository
	// and branch names, so it has unbounded cardinality as a metric label.
	Path string
	// StatusCode is 0 when the attempt failed without a response.
	StatusCode int
	// Duration is the time until response headers were received (or the attempt failed),
	// excluding client-side pacing and retry waits.
	Duration time.Duration
	// Err is the transport error of a failed attempt.
	Err error
	// RateLimit is parsed from the response headers.
	RateLimit RateLimitInfo
}

// RateLimitInfo is the provider rate-limit state advertised by response headers
// (X-RateLimit-* as sent by GitHub and Gitea, or RateLimit-* as sent by GitLab).
type RateLimitInfo struct {
	// Present is false when the response carried none of the headers.
	Present bool
	// Limit, Remaining and Used are -1 when the corresponding header is absent or invalid.
	Limit     int64
	Remaining int64
	Used      int64
	// Reset is zero when unknown.
	Reset time.Time
	// Resource is the rate-limit bucket ("core", "search", "graphql"; GitHub only).
	Resource string
}

// ParseRateLimitHeaders extracts rate-limit information from response headers.
func ParseRateLimitHeaders(h http.Header) RateLimitInfo {
	prefix := "X-RateLimit-"
	if h.Get(prefix+"Limit") == "" && h.Get(prefix+"Remaining") == "" && (h.Get("RateLimit-Limit") != "" || h.Get("RateLimit-Remaining") != "") {
		prefix = "RateLimit-"
	}
	num := func(k string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+k)), 10, 64)
		if err != nil || v < 0 {
			return -1
		}
		return v
	}
	info := RateLimitInfo{
		Limit:     num("Limit"),
		Remaining: num("Remaining"),
		Used:      num("Used"),
		Resource:  h.Get(prefix + "Resource"),
	}
	if epoch := num("Reset"); epoch > 0 {
		info.Reset = time.Unix(epoch, 0)
	}
	info.Present = info.Limit >= 0 || info.Remaining >= 0 || info.Used >= 0 || !info.Reset.IsZero()
	return info
}

// RequestObserver is called synchronously after every HTTP attempt of a provider,
// including retries, pagination and GitHub App token requests. It is the hook for API
// metrics (requests by method/path/status, latency, remaining rate limit). It must be
// fast, safe for concurrent use, and must not retain ctx.
type RequestObserver func(ctx context.Context, info RequestInfo)

func observeTransport(next http.RoundTripper, provider string, obs RequestObserver, clock func() time.Time) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		start := clock()
		resp, err := next.RoundTrip(req)
		info := RequestInfo{
			Provider: provider,
			Method:   req.Method,
			Host:     req.URL.Host,
			Path:     req.URL.Path,
			Duration: clock().Sub(start),
			Err:      err,
		}
		if err == nil && resp != nil {
			info.StatusCode = resp.StatusCode
			info.RateLimit = ParseRateLimitHeaders(resp.Header)
		} else {
			info.RateLimit = RateLimitInfo{Limit: -1, Remaining: -1, Used: -1}
		}
		obs(req.Context(), info)
		return resp, err
	})
}

// ChainOptions configures Chain.
type ChainOptions struct {
	// Provider labels log lines.
	Provider string
	Logger   *slog.Logger
	// Limiter paces every attempt; nil disables pacing.
	Limiter *rate.Limiter
	// Observer, when set, is called after every attempt (see RequestObserver).
	Observer RequestObserver
	Retry    RetryPolicy
	// Classifier defaults to DefaultRetryClassifier.
	Classifier RetryClassifier
	// Clock defaults to time.Now.
	Clock func() time.Time
	// Sleep defaults to a context-aware timer (tests inject a fake).
	Sleep func(ctx context.Context, d time.Duration) error
}

// Chain wraps inner (normally an authenticating transport over BaseTransport and
// LimitResponseBody) with request logging (redacted), bounded retries, pacing and the
// optional per-attempt observer:
//
//	logging → retry → rate limit → observer → inner
func Chain(inner http.RoundTripper, o ChainOptions) http.RoundTripper {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Classifier == nil {
		o.Classifier = DefaultRetryClassifier
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	o.Retry = o.Retry.WithDefaults()
	rt := inner
	if o.Observer != nil {
		rt = observeTransport(rt, o.Provider, o.Observer, o.Clock)
	}
	if o.Limiter != nil {
		lim := o.Limiter
		next := rt
		rt = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if err := lim.Wait(req.Context()); err != nil {
				return nil, err
			}
			return next.RoundTrip(req)
		})
	}
	rt = &retryTransport{next: rt, o: o}
	return &loggingTransport{next: rt, provider: o.Provider, logger: o.Logger, clock: o.Clock}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type retryTransport struct {
	next http.RoundTripper
	o    ChainOptions
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	canReplay := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	if !idempotent(req.Method) || !canReplay || t.o.Retry.MaxAttempts <= 1 {
		return t.next.RoundTrip(req)
	}
	ctx := req.Context()
	for attempt := 1; ; attempt++ {
		r := req
		if attempt > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r = req.Clone(ctx)
			r.Body = body
		}
		resp, err := t.next.RoundTrip(r)
		last := attempt >= t.o.Retry.MaxAttempts
		var wait time.Duration
		switch {
		case err != nil:
			if last || ctx.Err() != nil || errors.Is(err, ErrResponseTooLarge) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// Classified failures from inner layers (e.g. a token source that reports
			// Unauthorized or NotFound) are final unless their kind is retryable.
			var se *Error
			if errors.As(err, &se) && !IsRetryable(se) {
				return nil, err
			}
			wait = t.backoff(attempt)
		default:
			d := t.o.Classifier(resp, t.o.Clock())
			if !d.Retry || last {
				return resp, nil
			}
			if d.Wait > t.o.Retry.MaxWait {
				// Too long to block inside a call: surface to the caller with RetryAfter.
				return resp, nil
			}
			wait = d.Wait
			if wait <= 0 {
				wait = t.backoff(attempt)
			}
			if d.RateLimited {
				t.o.Logger.Warn("scm request rate limited; waiting before retry",
					"provider", t.o.Provider, "method", req.Method, "url", RedactURL(req.URL),
					"status", resp.StatusCode, "wait", wait.String(), "attempt", attempt)
			}
			drainAndClose(resp)
		}
		if err := t.o.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (t *retryTransport) backoff(attempt int) time.Duration {
	d := t.o.Retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > t.o.Retry.MaxDelay {
		d = t.o.Retry.MaxDelay
	}
	// Full jitter in [d/2, d].
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1) //nolint:gosec // jitter, not security sensitive
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

type loggingTransport struct {
	next     http.RoundTripper
	provider string
	logger   *slog.Logger
	clock    func() time.Time
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := t.clock()
	resp, err := t.next.RoundTrip(req)
	if !t.logger.Enabled(req.Context(), slog.LevelDebug) {
		return resp, err
	}
	attrs := []any{
		"provider", t.provider,
		"method", req.Method,
		"url", RedactURL(req.URL),
		"duration", t.clock().Sub(start).String(),
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	} else {
		attrs = append(attrs, "status", resp.StatusCode)
		if rem := resp.Header.Get("X-RateLimit-Remaining"); rem != "" {
			attrs = append(attrs, "rateLimitRemaining", rem)
		}
	}
	t.logger.DebugContext(req.Context(), "scm request", attrs...)
	return resp, err
}

// HostGuard routes requests whose host equals host through authed and all other requests
// through plain, so credentials are never attached to cross-host redirects.
func HostGuard(host string, authed, plain http.RoundTripper) http.RoundTripper {
	host = strings.ToLower(host)
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.ToLower(req.URL.Host) == host {
			return authed.RoundTrip(req)
		}
		return plain.RoundTrip(req)
	})
}

// BearerTransport sets "Authorization: Bearer <token>" from tok on a clone of every
// request. When the response is 401 and onUnauthorized is set, onUnauthorized is called
// with the rejected token so the caller can invalidate its cache, and the request is sent
// once more with a freshly obtained token, provided the body can be replayed and tok now
// returns a different token. A 401 is an authentication failure that the server rejects
// before acting, so the replay is safe for every method. When no fresh token can be
// obtained, the original 401 response is returned.
func BearerTransport(next http.RoundTripper, tok TokenFunc, onUnauthorized func(token string)) http.RoundTripper {
	send := func(req *http.Request, v string, body io.ReadCloser) (*http.Response, error) {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = body
		}
		if v != "" {
			r.Header.Set("Authorization", "Bearer "+v)
		}
		return next.RoundTrip(r)
	}
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		ctx := req.Context()
		v, err := tok(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := send(req, v, nil)
		if err != nil || resp.StatusCode != http.StatusUnauthorized || onUnauthorized == nil || v == "" {
			return resp, err
		}
		onUnauthorized(v)
		canReplay := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
		if !canReplay || ctx.Err() != nil {
			return resp, nil
		}
		fresh, terr := tok(ctx)
		if terr != nil || fresh == "" || fresh == v {
			return resp, nil
		}
		var body io.ReadCloser
		if req.GetBody != nil {
			if body, terr = req.GetBody(); terr != nil {
				return resp, nil
			}
		}
		drainAndClose(resp)
		resp, err = send(req, fresh, body)
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			onUnauthorized(fresh)
		}
		return resp, err
	})
}
