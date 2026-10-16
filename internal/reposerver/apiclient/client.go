package apiclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Wire protocol (ADR-0006): every method is `POST <PathPrefix><Method>` with a JSON
// request body (application/json) and a JSON response; ListPlugins takes an empty
// object. GenerateManifestWithFiles is multipart/form-data with, in order, the parts
// PartRequest (JSON ManifestRequest), PartFiles (gzipped tar of the source directory)
// and PartChecksum (hex SHA-256 of the PartFiles bytes). Errors are
// application/problem+json bodies {type, title, status, detail, code}. Requests carry
// `Authorization: Bearer <internal token>`.
const (
	// PathPrefix prefixes every repo-server method path.
	PathPrefix = "/repo/v1/"
	// PathHealthz (liveness) and PathReadyz (readiness) answer GET without
	// authentication.
	PathHealthz = "/healthz"
	PathReadyz  = "/readyz"

	MethodGenerateManifest          = "GenerateManifest"
	MethodGenerateManifestWithFiles = "GenerateManifestWithFiles"
	MethodResolveRevision           = "ResolveRevision"
	MethodTestRepository            = "TestRepository"
	MethodListRefs                  = "ListRefs"
	MethodListOCITags               = "ListOCITags"
	MethodListApps                  = "ListApps"
	MethodListPlugins               = "ListPlugins"
	MethodGetAppDetails             = "GetAppDetails"
	MethodGetRevisionMetadata       = "GetRevisionMetadata"
	MethodGetRevisionChartDetails   = "GetRevisionChartDetails"
	MethodGetOCIMetadata            = "GetOCIMetadata"
	MethodGetHelmCharts             = "GetHelmCharts"
	MethodGetGitFiles               = "GetGitFiles"
	MethodGetGitDirectories         = "GetGitDirectories"
	MethodUpdateRevisionForPaths    = "UpdateRevisionForPaths"

	// Multipart part names of GenerateManifestWithFiles.
	PartRequest  = "request"
	PartFiles    = "files"
	PartChecksum = "checksum"

	// ContentTypeProblem is the RFC 9457 error media type.
	ContentTypeProblem = "application/problem+json"
)

// Methods lists every JSON method (GenerateManifestWithFiles excluded).
var Methods = []string{
	MethodGenerateManifest, MethodResolveRevision, MethodTestRepository, MethodListRefs,
	MethodListOCITags, MethodListApps, MethodListPlugins, MethodGetAppDetails,
	MethodGetRevisionMetadata, MethodGetRevisionChartDetails, MethodGetOCIMetadata,
	MethodGetHelmCharts, MethodGetGitFiles, MethodGetGitDirectories, MethodUpdateRevisionForPaths,
}

// Problem is the RFC 9457 problem details body of an error response.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

// HTTP client defaults.
const (
	// DefaultMaxRequestBytes caps JSON request bodies (upstream's 100 MiB gRPC message
	// size, reposerver.grpc.max.size).
	DefaultMaxRequestBytes int64 = 100 << 20
	// DefaultMaxResponseBytes caps response bodies.
	DefaultMaxResponseBytes int64 = 100 << 20
	// DefaultRequestTimeout bounds one call when the context has no deadline.
	DefaultRequestTimeout = 10 * time.Minute
	// DefaultRetryAttempts, DefaultRetryBase and DefaultRetryMax configure retries of
	// calls that failed because the repo-server was unavailable.
	DefaultRetryAttempts = 3
	DefaultRetryBase     = 100 * time.Millisecond
	DefaultRetryMax      = 2 * time.Second
	maxProblemBytes      = 64 << 10
)

// HTTPClient calls a repo-server over HTTP(S). It is safe for concurrent use.
type HTTPClient struct {
	base             string
	token            string
	hc               *http.Client
	timeout          time.Duration
	maxRequestBytes  int64
	maxResponseBytes int64
	attempts         int
	retryBase        time.Duration
	retryMax         time.Duration
}

var _ Client = (*HTTPClient)(nil)

// HTTPClientOption configures an HTTPClient.
type HTTPClientOption func(*HTTPClient)

// WithHTTPClient replaces the HTTP client (its transport must carry any TLS settings).
func WithHTTPClient(hc *http.Client) HTTPClientOption {
	return func(c *HTTPClient) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithRequestTimeout bounds calls whose context has no deadline (default
// DefaultRequestTimeout; zero or negative disables).
func WithRequestTimeout(d time.Duration) HTTPClientOption {
	return func(c *HTTPClient) { c.timeout = d }
}

// WithRetry configures retries of calls that failed with an unavailable repo-server
// (connection failures, 503 unavailable): attempts in total (1 disables retries) with
// exponential backoff from base to maxDelay (jittered).
func WithRetry(attempts int, base, maxDelay time.Duration) HTTPClientOption {
	return func(c *HTTPClient) {
		if attempts < 1 {
			attempts = 1
		}
		c.attempts = attempts
		if base > 0 {
			c.retryBase = base
		}
		if maxDelay > 0 {
			c.retryMax = maxDelay
		}
	}
}

// WithMaxRequestBytes caps encoded JSON requests (default DefaultMaxRequestBytes).
func WithMaxRequestBytes(n int64) HTTPClientOption {
	return func(c *HTTPClient) {
		if n > 0 {
			c.maxRequestBytes = n
		}
	}
}

// WithMaxResponseBytes caps response bodies (default DefaultMaxResponseBytes).
func WithMaxResponseBytes(n int64) HTTPClientOption {
	return func(c *HTTPClient) {
		if n > 0 {
			c.maxResponseBytes = n
		}
	}
}

// NewHTTPClient returns a client for the repo-server at baseURL ("host:port",
// "http://host:port" or "https://host:port"; a missing scheme means https when
// tlsConfig is set) authenticating with the shared internal token. tlsConfig (may be
// nil) is used for https, including client certificates for mTLS. Environment proxies
// are never used for this internal traffic.
func NewHTTPClient(baseURL, token string, tlsConfig *tls.Config, opts ...HTTPClientOption) (*HTTPClient, error) {
	if token == "" {
		return nil, errors.New("repo-server client: an internal token is required")
	}
	raw := strings.TrimSpace(baseURL)
	if !strings.Contains(raw, "://") {
		if tlsConfig != nil {
			raw = "https://" + raw
		} else {
			raw = "http://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("repo-server client: invalid address %q", baseURL)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if tlsConfig != nil {
		cfg := tlsConfig.Clone()
		if cfg.MinVersion == 0 {
			cfg.MinVersion = tls.VersionTLS12
		}
		transport.TLSClientConfig = cfg
	}
	c := &HTTPClient{
		base:             strings.TrimSuffix(u.String(), "/"),
		token:            token,
		hc:               &http.Client{Transport: transport},
		timeout:          DefaultRequestTimeout,
		maxRequestBytes:  DefaultMaxRequestBytes,
		maxResponseBytes: DefaultMaxResponseBytes,
		attempts:         DefaultRetryAttempts,
		retryBase:        DefaultRetryBase,
		retryMax:         DefaultRetryMax,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// GenerateManifest implements Client.
func (c *HTTPClient) GenerateManifest(ctx context.Context, req *ManifestRequest) (*ManifestResponse, error) {
	return invoke[ManifestResponse](ctx, c, MethodGenerateManifest, req)
}

// ResolveRevision implements Client.
func (c *HTTPClient) ResolveRevision(ctx context.Context, req *ResolveRevisionRequest) (*ResolveRevisionResponse, error) {
	return invoke[ResolveRevisionResponse](ctx, c, MethodResolveRevision, req)
}

// TestRepository implements Client.
func (c *HTTPClient) TestRepository(ctx context.Context, req *TestRepositoryRequest) (*TestRepositoryResponse, error) {
	return invoke[TestRepositoryResponse](ctx, c, MethodTestRepository, req)
}

// ListRefs implements Client.
func (c *HTTPClient) ListRefs(ctx context.Context, req *ListRefsRequest) (*Refs, error) {
	return invoke[Refs](ctx, c, MethodListRefs, req)
}

// ListOCITags implements Client.
func (c *HTTPClient) ListOCITags(ctx context.Context, req *ListRefsRequest) (*Refs, error) {
	return invoke[Refs](ctx, c, MethodListOCITags, req)
}

// ListApps implements Client.
func (c *HTTPClient) ListApps(ctx context.Context, req *ListAppsRequest) (*AppList, error) {
	return invoke[AppList](ctx, c, MethodListApps, req)
}

// ListPlugins implements Client.
func (c *HTTPClient) ListPlugins(ctx context.Context) (*PluginList, error) {
	return invoke[PluginList](ctx, c, MethodListPlugins, struct{}{})
}

// GetAppDetails implements Client.
func (c *HTTPClient) GetAppDetails(ctx context.Context, req *AppDetailsQuery) (*AppDetails, error) {
	return invoke[AppDetails](ctx, c, MethodGetAppDetails, req)
}

// GetRevisionMetadata implements Client.
func (c *HTTPClient) GetRevisionMetadata(ctx context.Context, req *RevisionMetadataRequest) (*RevisionMetadata, error) {
	return invoke[RevisionMetadata](ctx, c, MethodGetRevisionMetadata, req)
}

// GetRevisionChartDetails implements Client.
func (c *HTTPClient) GetRevisionChartDetails(ctx context.Context, req *ChartDetailsRequest) (*ChartDetails, error) {
	return invoke[ChartDetails](ctx, c, MethodGetRevisionChartDetails, req)
}

// GetOCIMetadata implements Client.
func (c *HTTPClient) GetOCIMetadata(ctx context.Context, req *ChartDetailsRequest) (*OCIMetadata, error) {
	return invoke[OCIMetadata](ctx, c, MethodGetOCIMetadata, req)
}

// GetHelmCharts implements Client.
func (c *HTTPClient) GetHelmCharts(ctx context.Context, req *HelmChartsRequest) (*HelmChartsResponse, error) {
	return invoke[HelmChartsResponse](ctx, c, MethodGetHelmCharts, req)
}

// GetGitFiles implements Client.
func (c *HTTPClient) GetGitFiles(ctx context.Context, req *GitFilesRequest) (*GitFilesResponse, error) {
	return invoke[GitFilesResponse](ctx, c, MethodGetGitFiles, req)
}

// GetGitDirectories implements Client.
func (c *HTTPClient) GetGitDirectories(ctx context.Context, req *GitDirectoriesRequest) (*GitDirectoriesResponse, error) {
	return invoke[GitDirectoriesResponse](ctx, c, MethodGetGitDirectories, req)
}

// UpdateRevisionForPaths implements Client.
func (c *HTTPClient) UpdateRevisionForPaths(ctx context.Context, req *UpdateRevisionForPathsRequest) (*UpdateRevisionForPathsResponse, error) {
	return invoke[UpdateRevisionForPathsResponse](ctx, c, MethodUpdateRevisionForPaths, req)
}

// GenerateManifestWithFiles implements Client. The gzipped tar is streamed (never
// buffered) and closed when it implements io.Closer; the call is not retried.
func (c *HTTPClient) GenerateManifestWithFiles(ctx context.Context, req *ManifestRequest, files io.Reader) (*ManifestResponse, error) {
	if closer, ok := files.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	if req == nil {
		return nil, &Error{Code: CodeInvalidArgument, Message: "missing request"}
	}
	if files == nil {
		return nil, &Error{Code: CodeInvalidArgument, Message: "missing files"}
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("repo-server client: encode request: %w", err)
	}
	if int64(len(reqJSON)) > c.maxRequestBytes {
		return nil, &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("request exceeds %d bytes", c.maxRequestBytes)}
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeFilesBody(mw, reqJSON, files))
	}()
	defer func() { _ = pr.Close() }()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+PathPrefix+MethodGenerateManifestWithFiles, pr)
	if err != nil {
		return nil, fmt.Errorf("repo-server client: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	c.setHeaders(httpReq)
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("repo-server %s: %w", MethodGenerateManifestWithFiles, ctxErr)
		}
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("repo-server %s: %v", MethodGenerateManifestWithFiles, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	var res ManifestResponse
	if _, err := c.decode(resp, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// invoke performs a JSON call and returns the decoded response, or nil and the error.
func invoke[T any](ctx context.Context, c *HTTPClient, method string, req any) (*T, error) {
	var out T
	if err := c.call(ctx, method, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// writeFilesBody writes the request, files and checksum parts.
func writeFilesBody(mw *multipart.Writer, reqJSON []byte, files io.Reader) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+PartRequest+`"`)
	h.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := w.Write(reqJSON); err != nil {
		return err
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+PartFiles+`"; filename="source.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	if w, err = mw.CreatePart(h); err != nil {
		return err
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, sum), files); err != nil {
		return fmt.Errorf("read files: %w", err)
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+PartChecksum+`"`)
	h.Set("Content-Type", "text/plain")
	if w, err = mw.CreatePart(h); err != nil {
		return err
	}
	if _, err := io.WriteString(w, hex.EncodeToString(sum.Sum(nil))); err != nil {
		return err
	}
	return mw.Close()
}

func (c *HTTPClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

func (c *HTTPClient) setHeaders(r *http.Request) {
	r.Header.Set("Accept", "application/json, "+ContentTypeProblem)
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("User-Agent", "patrol-reposerver-client")
}

// call performs one JSON method call with retries on unavailability.
func (c *HTTPClient) call(ctx context.Context, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("repo-server client: encode request: %w", err)
	}
	if int64(len(body)) > c.maxRequestBytes {
		return &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("request exceeds %d bytes", c.maxRequestBytes)}
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	endpoint := c.base + PathPrefix + method
	var lastErr error
	for attempt := 1; ; attempt++ {
		retry, err := c.once(ctx, method, endpoint, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt >= c.attempts {
			return lastErr
		}
		if err := c.sleep(ctx, attempt); err != nil {
			return lastErr
		}
	}
}

// once performs one attempt; retry reports whether the failure may be retried.
func (c *HTTPClient) once(ctx context.Context, method, endpoint string, body []byte, out any) (retry bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("repo-server client: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setHeaders(httpReq)
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("repo-server %s: %w", method, ctxErr)
		}
		return true, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("repo-server %s: %v", method, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	return c.decode(resp, out)
}

// decode reads a response into out or maps a problem response to *Error.
func (c *HTTPClient) decode(resp *http.Response, out any) (retry bool, err error) {
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxProblemBytes))
		e := problemError(resp.StatusCode, data)
		return resp.StatusCode == http.StatusServiceUnavailable && e.Code == CodeUnavailable, e
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return true, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("read repo-server response: %v", err)}
	}
	if int64(len(data)) > c.maxResponseBytes {
		return false, &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("repo-server response exceeds %d bytes", c.maxResponseBytes)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("repo-server client: decode response: %w", err)
	}
	return false, nil
}

// problemError maps a non-200 response to *Error.
func problemError(status int, data []byte) *Error {
	var p Problem
	if err := json.Unmarshal(data, &p); err == nil && p.Code != "" {
		msg := p.Detail
		if msg == "" {
			msg = p.Title
		}
		return &Error{Code: p.Code, Message: msg}
	}
	return &Error{Code: CodeForStatus(status), Message: fmt.Sprintf("unexpected repo-server response status %d", status)}
}

// CodeForStatus maps an HTTP status to an error code (used when a response carries no
// problem body) and StatusForCode does the reverse.
func CodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnsupportedMediaType:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return CodeResourceExhausted
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return CodeUnavailable
	case http.StatusGatewayTimeout:
		return CodeDeadlineExceeded
	default:
		return CodeInternal
	}
}

// StatusForCode returns the HTTP status of an error code.
func StatusForCode(code string) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// sleep waits before retry attempt+1 (exponential backoff with full jitter in the
// upper half).
func (c *HTTPClient) sleep(ctx context.Context, attempt int) error {
	d := c.retryBase << (attempt - 1)
	if d <= 0 || d > c.retryMax {
		d = c.retryMax
	}
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
