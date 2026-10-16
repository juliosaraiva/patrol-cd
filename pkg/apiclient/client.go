package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Headers and media types shared with the server.
const (
	// CSRFHeader and CSRFHeaderValue are sent on every request; the server requires them
	// on cookie-authenticated state-changing requests (ADR-0009).
	CSRFHeader      = "X-Requested-With"
	CSRFHeaderValue = "patrol"
	// RequestIDHeader carries the request ID echoed in responses and problem documents.
	RequestIDHeader = "X-Request-Id"
	// RenewTokenHeader carries a renewed login session token on responses to
	// bearer-authenticated requests whose session was about to expire (browsers get a
	// refreshed patrol.token cookie instead).
	RenewTokenHeader = "Patrol-Renew-Token"
	// DefaultUserAgent identifies this client.
	DefaultUserAgent = "patrol-client"
)

// Default limits of the client.
const (
	// DefaultTimeout bounds a unary request (connect to last body byte).
	DefaultTimeout = 5 * time.Minute
	// DefaultMaxResponseBytes bounds the size of a decoded JSON response.
	DefaultMaxResponseBytes int64 = 256 << 20
	// maxErrorBodyBytes bounds the error body read for a problem document.
	maxErrorBodyBytes = 64 << 10
)

// ClientOptions configures a Client. The field names follow the Argo CD CLI's global
// flags; flags that only apply to its gRPC transport are accepted and ignored.
type ClientOptions struct {
	// ServerAddr is "host[:port]" or a full URL ("https://patrol.example.com/sub").
	ServerAddr string
	// PlainText uses http:// for a ServerAddr without a scheme.
	PlainText bool
	// Insecure skips verification of the server certificate.
	Insecure bool
	// CertFile is a PEM bundle of CAs trusted for the server (in addition to none of
	// the system roots, as upstream --server-crt).
	CertFile string
	// ClientCertFile and ClientCertKeyFile enable mutual TLS.
	ClientCertFile    string
	ClientCertKeyFile string
	// AuthToken is the bearer token (session, account API or project-role token).
	AuthToken string
	// RootPath is the server's --rootpath (e.g. "patrol"); GRPCWebRootPath is accepted as
	// an alias for CLI compatibility.
	RootPath        string
	GRPCWebRootPath string
	// Headers are extra "Name: value" headers sent with every request.
	Headers []string
	// UserAgent overrides DefaultUserAgent.
	UserAgent string
	// Timeout bounds unary requests (default DefaultTimeout; streams are bounded by their
	// context only).
	Timeout time.Duration
	// HTTPRetryMax retries idempotent requests (GET) that failed with a transport error
	// or 502/503/504 up to this many times.
	HTTPRetryMax int
	// MaxResponseBytes bounds JSON responses (default DefaultMaxResponseBytes).
	MaxResponseBytes int64
	// HTTPClient replaces the client built from the TLS options (tests, proxies).
	HTTPClient *http.Client

	// Ignored: gRPC-Web transport, port forwarding and core mode do not apply to the
	// JSON/HTTP API.
	GRPCWeb              bool
	PortForward          bool
	PortForwardNamespace string
	Core                 bool
	KubeOverrides        any
}

// Client calls the Patrol CD public API. It is safe for concurrent use.
type Client struct {
	base       *url.URL
	hc         *http.Client
	headers    http.Header
	userAgent  string
	timeout    time.Duration
	retries    int
	maxBody    int64
	tokenMu    sync.RWMutex
	token      string
	onNewToken func(string)
}

// NewClient returns a client for opts.
func NewClient(opts *ClientOptions) (*Client, error) {
	if opts == nil {
		return nil, errors.New("apiclient: nil options")
	}
	if strings.TrimSpace(opts.ServerAddr) == "" {
		return nil, errors.New("apiclient: server address is required")
	}
	base, err := baseURL(opts)
	if err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	if hc == nil {
		tr, err := transport(opts)
		if err != nil {
			return nil, err
		}
		hc = &http.Client{Transport: tr}
	}
	hdr := http.Header{}
	for _, h := range opts.Headers {
		name, value, ok := strings.Cut(h, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("apiclient: header %q must have the form \"Name: value\"", h)
		}
		hdr.Add(name, strings.TrimSpace(value))
	}
	c := &Client{
		base:      base,
		hc:        hc,
		headers:   hdr,
		userAgent: opts.UserAgent,
		timeout:   opts.Timeout,
		retries:   max(opts.HTTPRetryMax, 0),
		maxBody:   opts.MaxResponseBytes,
		token:     opts.AuthToken,
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxResponseBytes
	}
	return c, nil
}

func baseURL(opts *ClientOptions) (*url.URL, error) {
	raw := strings.TrimSpace(opts.ServerAddr)
	if !strings.Contains(raw, "://") {
		scheme := "https://"
		if opts.PlainText {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("apiclient: invalid server address: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("apiclient: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("apiclient: server address has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("apiclient: server address must not contain credentials, a query or a fragment")
	}
	root := opts.RootPath
	if root == "" {
		root = opts.GRPCWebRootPath
	}
	p := strings.Trim(u.Path, "/")
	if r := strings.Trim(root, "/"); r != "" {
		if p != "" {
			p += "/"
		}
		p += r
	}
	u.Path = ""
	if p != "" {
		u.Path = "/" + p
	}
	u.RawPath = ""
	return u, nil
}

func transport(opts *ClientOptions) (*http.Transport, error) {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("apiclient: unexpected default transport")
	}
	tr = tr.Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.Insecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit user opt-in (--insecure)
	}
	if opts.CertFile != "" {
		pem, err := os.ReadFile(opts.CertFile)
		if err != nil {
			return nil, fmt.Errorf("apiclient: read server certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("apiclient: %s contains no PEM certificate", opts.CertFile)
		}
		tlsCfg.RootCAs = pool
	}
	if opts.ClientCertFile != "" || opts.ClientCertKeyFile != "" {
		if opts.ClientCertFile == "" || opts.ClientCertKeyFile == "" {
			return nil, errors.New("apiclient: client certificate and key must be set together")
		}
		cert, err := tls.LoadX509KeyPair(opts.ClientCertFile, opts.ClientCertKeyFile)
		if err != nil {
			return nil, fmt.Errorf("apiclient: load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	tr.TLSClientConfig = tlsCfg
	return tr, nil
}

// BaseURL returns the server base URL including the root path.
func (c *Client) BaseURL() string { return c.base.String() }

// Token returns the current bearer token.
func (c *Client) Token() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.token
}

// SetToken replaces the bearer token (e.g. after login).
func (c *Client) SetToken(token string) {
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
}

// OnTokenRenewed registers fn, called when the server returns a renewed session token
// (RenewTokenHeader); the client switches to the new token before calling fn. Use it to
// persist the token. Register it before issuing requests.
func (c *Client) OnTokenRenewed(fn func(token string)) { c.onNewToken = fn }

// Do sends a request built from req to the route pattern path (e.g.
// "/api/v1/account/{name}") and decodes the JSON response into out (which may be nil).
// req is a pointer to a struct tagged as described in the package documentation, or nil.
// Non-2xx responses return *APIError.
func (c *Client) Do(ctx context.Context, method, path string, req, out any) error {
	b, err := buildRequest(method, path, req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.send(ctx, method, b, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > c.maxBody {
		return fmt.Errorf("response exceeds %d bytes", c.maxBody)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response of %s %s: %w", method, path, err)
	}
	return nil
}

// DoRaw sends a request with a caller-provided body (e.g. a tar.gz upload) and returns
// the raw response for 2xx statuses; the caller closes its body. Non-2xx responses
// return *APIError.
func (c *Client) DoRaw(ctx context.Context, method, path string, req any, body io.Reader, contentType string) (*http.Response, error) {
	b, err := buildRequest(method, path, req)
	if err != nil {
		return nil, err
	}
	b.body = body
	b.rawBody = true
	resp, err := c.send(ctx, method, b, contentType)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, errorFromResponse(resp)
	}
	return resp, nil
}

func (c *Client) send(ctx context.Context, method string, b *builtRequest, contentType string) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + b.path
	u.RawPath = c.base.Path + b.rawPath
	u.RawQuery = b.query.Encode()
	attempts := 1
	if method == http.MethodGet && !b.rawBody {
		attempts += c.retries
	}
	var lastErr error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i) * 250 * time.Millisecond):
			}
		}
		var body io.Reader
		if b.rawBody {
			body = b.body
		} else if b.json != nil {
			body = bytes.NewReader(b.json)
		}
		hr, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		for k, vs := range c.headers {
			for _, v := range vs {
				hr.Header.Add(k, v)
			}
		}
		hr.Header.Set("Accept", "application/json, application/problem+json")
		hr.Header.Set("User-Agent", c.userAgent)
		hr.Header.Set(CSRFHeader, CSRFHeaderValue)
		if body != nil {
			hr.Header.Set("Content-Type", contentType)
		}
		if tok := c.Token(); tok != "" {
			hr.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.hc.Do(hr)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w", method, b.path, err)
			if ctx.Err() != nil {
				return nil, lastErr
			}
			continue
		}
		if ctx.Value(noRenewalKey{}) == nil {
			c.captureRenewal(resp)
		}
		if i < attempts-1 && isRetryableStatus(resp.StatusCode) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s %s: HTTP %d", method, b.path, resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func isRetryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// noRenewalKey marks requests that end the session (Logout): a renewed token on their
// response is never adopted nor reported, even from a server that sends one.
type noRenewalKey struct{}

// captureRenewal adopts a renewed session token sent in RenewTokenHeader.
func (c *Client) captureRenewal(resp *http.Response) {
	tok := strings.TrimSpace(resp.Header.Get(RenewTokenHeader))
	if tok == "" || strings.Count(tok, ".") != 2 || tok == c.Token() {
		return
	}
	c.SetToken(tok)
	if c.onNewToken != nil {
		c.onNewToken(tok)
	}
}

func errorFromResponse(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	e := &APIError{StatusCode: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s > 0 {
			e.RetryAfter = s
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == ProblemContentType || mt == "application/json" {
		var p Problem
		if json.Unmarshal(data, &p) == nil && (p.Status != 0 || p.Code != "" || p.Detail != "") {
			e.Problem = p
		}
	}
	if e.Problem.Status == 0 {
		e.Problem.Status = resp.StatusCode
	}
	if e.Problem.Title == "" {
		e.Problem.Title = http.StatusText(resp.StatusCode)
	}
	if e.Problem.Code == "" {
		e.Problem.Code = CodeForStatus(resp.StatusCode)
	}
	if e.Problem.Detail == "" && mt != ProblemContentType {
		e.Problem.Detail = truncate(strings.TrimSpace(string(data)), 512)
	}
	if e.Problem.RequestID == "" {
		e.Problem.RequestID = resp.Header.Get(RequestIDHeader)
	}
	return e
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// builtRequest is the HTTP form of a tagged request struct.
type builtRequest struct {
	path    string
	rawPath string
	query   url.Values
	json    []byte
	body    io.Reader
	rawBody bool
}

var textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()

// buildRequest fills the path pattern and query from req's tagged fields and encodes its
// body (the `body:"true"` field, or the JSON-visible fields of req itself).
func buildRequest(method, pattern string, req any) (*builtRequest, error) {
	b := &builtRequest{query: url.Values{}}
	values := map[string]string{}
	var v reflect.Value
	if req != nil {
		v = reflect.ValueOf(req)
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v = reflect.Value{}
				break
			}
			v = v.Elem()
		}
	}
	hasJSON := false
	var bodyField reflect.Value
	if v.IsValid() {
		if v.Kind() != reflect.Struct {
			return nil, fmt.Errorf("apiclient: request must be a struct, got %s", v.Type())
		}
		for _, f := range reflect.VisibleFields(v.Type()) {
			if !f.IsExported() || f.Anonymous {
				continue
			}
			fv, err := v.FieldByIndexErr(f.Index)
			if err != nil {
				continue
			}
			switch {
			case f.Tag.Get("path") != "":
				s, _, err := scalarString(fv)
				if err != nil {
					return nil, fmt.Errorf("apiclient: path field %s: %w", f.Name, err)
				}
				values[f.Tag.Get("path")] = s
			case f.Tag.Get("query") != "":
				if err := addQuery(b.query, f.Tag.Get("query"), fv); err != nil {
					return nil, fmt.Errorf("apiclient: query field %s: %w", f.Name, err)
				}
			case f.Tag.Get("body") == "true":
				bodyField = fv
			default:
				if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "-" {
					hasJSON = true
				}
			}
		}
	}
	p, raw, err := expandPath(pattern, values)
	if err != nil {
		return nil, err
	}
	b.path, b.rawPath = p, raw
	if method == http.MethodGet || method == http.MethodDelete || method == http.MethodHead {
		return b, nil
	}
	switch {
	case bodyField.IsValid():
		data, err := json.Marshal(bodyField.Interface())
		if err != nil {
			return nil, fmt.Errorf("apiclient: encode body: %w", err)
		}
		b.json = data
	case hasJSON:
		data, err := json.Marshal(v.Interface())
		if err != nil {
			return nil, fmt.Errorf("apiclient: encode body: %w", err)
		}
		b.json = data
	default:
		b.json = []byte("{}")
	}
	return b, nil
}

// expandPath replaces "{name}" and "{name...}" wildcards; single-segment values are
// escaped whole (slashes become %2F), multi-segment values per segment.
func expandPath(pattern string, values map[string]string) (string, string, error) {
	var plain, raw strings.Builder
	rest := pattern
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			plain.WriteString(rest)
			raw.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return "", "", fmt.Errorf("apiclient: malformed path pattern %q", pattern)
		}
		plain.WriteString(rest[:i])
		raw.WriteString(rest[:i])
		name := rest[i+1 : i+j]
		multi := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		val, ok := values[name]
		if !ok || val == "" {
			return "", "", fmt.Errorf("apiclient: missing path parameter %q", name)
		}
		plain.WriteString(val)
		if multi {
			segs := strings.Split(val, "/")
			for k, s := range segs {
				segs[k] = url.PathEscape(s)
			}
			raw.WriteString(strings.Join(segs, "/"))
		} else {
			raw.WriteString(url.PathEscape(val))
		}
		rest = rest[i+j+1:]
	}
	return plain.String(), raw.String(), nil
}

func addQuery(q url.Values, name string, fv reflect.Value) error {
	if fv.Kind() == reflect.Slice && !fv.Type().Implements(textMarshalerType) {
		for i := range fv.Len() {
			s, _, err := scalarString(fv.Index(i))
			if err != nil {
				return err
			}
			q.Add(name, s)
		}
		return nil
	}
	s, set, err := scalarString(fv)
	if err != nil {
		return err
	}
	if set {
		q.Set(name, s)
	}
	return nil
}

// scalarString formats a scalar; set is false for zero values of non-pointer fields and
// nil pointers (omitted query parameters).
func scalarString(v reflect.Value) (string, bool, error) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", false, nil
		}
		s, _, err := scalarString(v.Elem())
		return s, true, err
	}
	if v.CanInterface() {
		if tm, ok := v.Interface().(encoding.TextMarshaler); ok {
			b, err := tm.MarshalText()
			return string(b), len(b) > 0, err
		}
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), v.String() != "", nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), v.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), v.Int() != 0, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), v.Uint() != 0, nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, v.Type().Bits()), v.Float() != 0, nil
	}
	return "", false, fmt.Errorf("unsupported parameter type %s", v.Type())
}
