package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRedirects = 5

// httpClient performs bounded, retried HTTP requests for one service instance.
type httpClient struct {
	service   string
	client    *http.Client
	retry     RetryPolicy
	validate  func(*url.URL) error
	maxBody   int64
	userAgent string
	redactor  *Redactor
	sleep     func(ctx context.Context, d time.Duration) error
}

type httpResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// newHTTPClient builds a client for serviceType honoring the shared network settings and
// the per-service transport options. wrap, when non-nil, decorates the transport (used for
// GitHub App authentication).
func newHTTPClient(serviceType string, deps Deps, topts TransportOptions, redactor *Redactor, wrap func(http.RoundTripper) http.RoundTripper) (*httpClient, error) {
	ts, err := topts.settings()
	if err != nil {
		return nil, err
	}
	tr := newTransport(deps, ts)
	var rt http.RoundTripper = tr
	if wrap != nil {
		rt = wrap(tr)
	}
	c := &httpClient{
		service:   serviceType,
		retry:     deps.retry(),
		validate:  deps.Network.ValidateURL,
		maxBody:   deps.maxResponseBytes(),
		userAgent: deps.userAgent(),
		redactor:  redactor,
		sleep:     sleepCtx,
	}
	c.client = &http.Client{
		Transport: rt,
		Timeout:   deps.timeout(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return c.checkURL(req.URL)
		},
	}
	return c, nil
}

// newTransport builds an *http.Transport with the shared dialer and TLS trust settings.
func newTransport(deps Deps, ts transportSettings) *http.Transport {
	tr := &http.Transport{
		DialContext:           deps.dialer(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: deps.timeout(),
		TLSClientConfig:       tlsConfig(deps, "", ts.insecureSkipVerify),
	}
	if !deps.Network.DisableProxyFromEnvironment {
		tr.Proxy = http.ProxyFromEnvironment
	}
	if ts.maxIdleConns > 0 {
		tr.MaxIdleConns = ts.maxIdleConns
	}
	if ts.maxIdleConnsPerHost > 0 {
		tr.MaxIdleConnsPerHost = ts.maxIdleConnsPerHost
	}
	if ts.maxConnsPerHost > 0 {
		tr.MaxConnsPerHost = ts.maxConnsPerHost
	}
	if ts.idleConnTimeout > 0 {
		tr.IdleConnTimeout = ts.idleConnTimeout
	}
	if deps.Network.RootCAs != nil && !ts.insecureSkipVerify {
		// Direct TLS connections are verified against the dialed host, which also covers IP
		// address endpoints (no SNI is sent for them, so the negotiated server name that
		// the shared configuration sees is empty). Proxied connections keep the shared
		// configuration, which refuses an empty server name.
		dial := deps.dialer()
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			cfg := tlsConfig(deps, host, false)
			cfg.NextProtos = []string{"h2", "http/1.1"}
			tc := tls.Client(raw, cfg)
			hctx, cancel := context.WithTimeout(ctx, tr.TLSHandshakeTimeout)
			defer cancel()
			if err := tc.HandshakeContext(hctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return tc, nil
		}
	}
	return tr
}

// tlsConfig returns a client TLS configuration. When a RootCAs resolver is configured the
// chain is verified against the resolver's pool for the negotiated server name, which also
// works for proxied (CONNECT) connections. For IP address endpoints no server name is
// negotiated: the certificate is then verified against serverName (the dialed host), and
// refused when serverName is empty.
func tlsConfig(deps Deps, serverName string, insecure bool) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if insecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // explicitly requested by service configuration
		return cfg
	}
	resolver := deps.Network.RootCAs
	if resolver == nil {
		return cfg
	}
	cfg.InsecureSkipVerify = true //nolint:gosec // verification is performed in VerifyConnection below
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("tls: server presented no certificates")
		}
		name := cs.ServerName
		if name == "" {
			name = serverName
		}
		if name == "" {
			return errors.New("tls: no server name to verify the certificate against")
		}
		pool, err := resolver(name)
		if err != nil {
			return fmt.Errorf("tls: resolving trust roots for %s: %w", name, err)
		}
		if pool == nil {
			pool, err = x509.SystemCertPool()
			if err != nil {
				return fmt.Errorf("tls: loading system roots: %w", err)
			}
		}
		opts := x509.VerifyOptions{DNSName: name, Roots: pool, Intermediates: x509.NewCertPool()}
		for _, c := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(c)
		}
		_, err = cs.PeerCertificates[0].Verify(opts)
		return err
	}
	return cfg
}

// checkURL enforces http/https and the injected validation hook.
func (c *httpClient) checkURL(u *url.URL) error {
	return validateEndpoint(u, []string{"http", "https"}, c.validate)
}

func validateEndpoint(u *url.URL, schemes []string, hook func(*url.URL) error) error {
	if u == nil {
		return fmt.Errorf("%w: empty URL", ErrURLNotAllowed)
	}
	scheme := strings.ToLower(u.Scheme)
	allowed := false
	for _, s := range schemes {
		if scheme == s {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: scheme %q is not allowed (allowed: %s)", ErrURLNotAllowed, u.Scheme, strings.Join(schemes, ", "))
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: URL has no host", ErrURLNotAllowed)
	}
	if hook != nil {
		if err := hook(u); err != nil {
			if errors.Is(err, ErrURLNotAllowed) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrURLNotAllowed, err)
		}
	}
	return nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: empty URL", ErrURLNotAllowed)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL: %v", ErrURLNotAllowed, err)
	}
	return u, nil
}

// do sends a request, retrying transient failures. body may be nil. The returned response
// body is capped at maxBody bytes.
func (c *httpClient) do(ctx context.Context, method, rawURL string, header http.Header, body []byte) (*httpResponse, error) {
	u, err := parseEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	if err := c.checkURL(u); err != nil {
		return nil, err
	}
	display := SanitizeURL(u.String())
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
		if err != nil {
			return nil, fmt.Errorf("%s: building request: %w", c.service, err)
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		if host := header.Get("Host"); host != "" {
			req.Host = host
		}
		resp, err := c.client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%s: %s %s: %w", c.service, method, display, ctxErr)
			}
			if !retryableError(err) || attempt >= c.retry.MaxRetries {
				return nil, fmt.Errorf("%s: %s %s: %w", c.service, method, display, unwrapURLError(err))
			}
			if serr := c.sleep(ctx, backoff(c.retry, attempt, "")); serr != nil {
				return nil, fmt.Errorf("%s: %s %s: %w", c.service, method, display, serr)
			}
			continue
		}
		data, readErr := readLimited(resp.Body, c.maxBody)
		_ = resp.Body.Close()
		if retryableStatus(resp.StatusCode) && attempt < c.retry.MaxRetries {
			if serr := c.sleep(ctx, backoff(c.retry, attempt, resp.Header.Get("Retry-After"))); serr != nil {
				return nil, fmt.Errorf("%s: %s %s: %w", c.service, method, display, serr)
			}
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("%s: %s %s: reading response: %w", c.service, method, display, readErr)
		}
		return &httpResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}, nil
	}
}

// doJSON marshals payload as JSON (when non-nil) and sends it.
func (c *httpClient) doJSON(ctx context.Context, method, rawURL string, header http.Header, payload any) (*httpResponse, error) {
	if header == nil {
		header = http.Header{}
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("%s: encoding request: %w", c.service, err)
		}
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/json")
		}
	}
	return c.do(ctx, method, rawURL, header, body)
}

// statusError builds a StatusError with a truncated, redacted body.
func (c *httpClient) statusError(method, rawURL string, resp *httpResponse) error {
	return &StatusError{
		Service:    c.service,
		Method:     method,
		URL:        SanitizeURL(rawURL),
		StatusCode: resp.StatusCode,
		Body:       c.redactor.Redact(truncateBytes(resp.Body, 512)),
	}
}

// expect2xx returns a StatusError unless the response status is 2xx.
func (c *httpClient) expect2xx(method, rawURL string, resp *httpResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return c.statusError(method, rawURL, resp)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return data, err
	}
	// Drain a bounded remainder so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	return data, nil
}

func truncateBytes(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	return truncateRunes(s, n)
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func retryableStatus(code int) bool {
	switch {
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return true
	case code == http.StatusNotImplemented, code == http.StatusHTTPVersionNotSupported:
		return false
	case code >= 500 && code <= 599:
		return true
	}
	return false
}

func retryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrURLNotAllowed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return false
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.Contains(urlErr.Err.Error(), "redirect") {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	return true
}

func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// backoff computes the wait before retry number attempt+1: exponential between WaitMin and
// WaitMax with jitter, or the server's Retry-After (bounded) when present.
func backoff(p RetryPolicy, attempt int, retryAfter string) time.Duration {
	if d, ok := parseRetryAfter(retryAfter); ok {
		if d < p.WaitMin {
			d = p.WaitMin
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	}
	if p.WaitMax <= 0 {
		return 0
	}
	wait := p.WaitMin
	for i := 0; i < attempt && wait < p.WaitMax; i++ {
		if wait == 0 {
			wait = time.Millisecond
		}
		wait *= 2
	}
	if wait > p.WaitMax {
		wait = p.WaitMax
	}
	// Jitter in [wait/2, wait], never below WaitMin.
	half := wait / 2
	if half > 0 {
		wait = half + time.Duration(rand.Int64N(int64(half)+1))
	}
	if wait < p.WaitMin {
		wait = p.WaitMin
	}
	return wait
}

func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
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

// joinURL appends a path to a base URL, keeping exactly one slash between them.
func joinURL(base, p string) string {
	if p == "" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
}

// ensureScheme prefixes https:// to host-only endpoints.
func ensureScheme(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "https://" + raw
}
