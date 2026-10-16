package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// maxInputBytes bounds files, URLs and standard input read by the commands (manifests,
// values blocks, patches).
const maxInputBytes = 16 << 20

// remoteFetchTimeout bounds fetching a manifest or values file over http(s).
const remoteFetchTimeout = 60 * time.Second

// isHTTPURL reports whether s is an absolute http(s) URL (upstream: other strings are
// local paths).
func isHTTPURL(s string) bool {
	u, err := url.ParseRequestURI(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// readFileOrURL reads a local file or an http(s) URL, bounded by maxInputBytes.
func (e *env) readFileOrURL(ctx context.Context, pathOrURL string) ([]byte, error) {
	if isHTTPURL(pathOrURL) {
		return e.httpGet(ctx, pathOrURL)
	}
	f, err := os.Open(pathOrURL) //nolint:gosec // the user names the file to read
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, pathOrURL)
}

// readBounded reads r up to maxInputBytes.
func readBounded(r io.Reader, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", what, maxInputBytes)
	}
	return data, nil
}

// httpGetBounded fetches an http(s) URL with a timeout and a size bound; non-2xx statuses
// are errors. TLS verification uses the system roots.
func httpGetBounded(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", redactURL(rawURL), resp.StatusCode)
	}
	return readBounded(resp.Body, redactURL(rawURL))
}

// redactURL removes user information from a URL for messages.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}
