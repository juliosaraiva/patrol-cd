package commitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client defaults.
const (
	// DefaultClientTimeout bounds one commit request end to end (clone, write, push).
	DefaultClientTimeout = 10 * time.Minute
	maxResponseBytes     = 1 << 20
)

// HTTPClient calls a commit-server over HTTP(S).
type HTTPClient struct {
	endpoint string
	token    string
	hc       *http.Client
}

var _ Client = (*HTTPClient)(nil)

// ClientOption configures an HTTPClient.
type ClientOption func(*HTTPClient)

// WithHTTPClient sets the HTTP client (for TLS/mTLS transports). Its Timeout bounds requests.
func WithHTTPClient(hc *http.Client) ClientOption { return func(c *HTTPClient) { c.hc = hc } }

// NewClient returns a client for address ("host:port", "http://host:port" or
// "https://host:port", as in the commit.server parameter) authenticating with token.
func NewClient(address, token string, opts ...ClientOption) (*HTTPClient, error) {
	if token == "" {
		return nil, errors.New("commit-server client: an internal token is required")
	}
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("commit-server client: invalid address %q", address)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + PathCommitHydratedManifests
	c := &HTTPClient{endpoint: u.String(), token: token, hc: &http.Client{Timeout: DefaultClientTimeout}}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// CommitHydratedManifests implements Client. Server errors are returned as *Error.
func (c *HTTPClient) CommitHydratedManifests(ctx context.Context, req *CommitHydratedManifestsRequest) (*CommitHydratedManifestsResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("commit-server client: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("commit-server client: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, application/problem+json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("commit-server client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("commit-server client: read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("commit-server client: response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e.Code = CodeInternal
			e.Message = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		}
		e.Status = resp.StatusCode
		return nil, e
	}
	var out CommitHydratedManifestsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("commit-server client: decode response: %w", err)
	}
	if out.HydratedSHA == "" {
		return nil, errors.New("commit-server client: response has no hydrated SHA")
	}
	return &out, nil
}
