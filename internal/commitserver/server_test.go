package commitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

const testToken = "internal-token-for-tests"

func newTestServer(t *testing.T, svc Client, opts HandlerOptions) *httptest.Server {
	t.Helper()
	if opts.Token == "" {
		opts.Token = testToken
	}
	if opts.Logger == nil {
		opts.Logger = discard
	}
	h, err := NewHandler(svc, opts)
	require.NoError(t, err)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// APP-135: the commit-server HTTP API end to end with the typed client.
func TestHTTPCommitRoundTrip(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	ts := newTestServer(t, newTestService(t, ServiceOptions{}), HandlerOptions{})
	c, err := NewClient(strings.TrimPrefix(ts.URL, "http://"), testToken)
	require.NoError(t, err)

	req := newRequest(r.url(), "main", drySHA1, pathOf("apps/a", configMap("a", "1")))
	req.Repo.Password = "write-secret"
	req.Repo.Username = "bot"
	resp, err := c.CommitHydratedManifests(testCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, r.head("main"), resp.HydratedSHA)

	// Errors keep their code across the wire.
	bad := newRequest(r.url(), "main", drySHA1, pathOf("../escape", configMap("a", "1")))
	_, err = c.CommitHydratedManifests(testCtx(t), bad)
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusBadRequest, e.Status)
	assert.Equal(t, CodeInvalidArgument, e.Code)
	assert.Contains(t, e.Message, "outside the repository")
	assert.ErrorIs(t, err, ErrInvalidRequest)

	branch := newRequest(r.url(), "main", drySHA1, pathOf("a", configMap("a", "1")))
	branch.TargetBranch = "bad..branch"
	branch.SyncBranch = "bad..branch"
	_, err = c.CommitHydratedManifests(testCtx(t), branch)
	require.ErrorAs(t, err, &e)
	assert.Equal(t, CodeInvalidArgument, e.Code, "invalid branch names are client errors")
}

type fakeService struct {
	err  error
	got  *CommitHydratedManifestsRequest
	sha  string
	seen int
}

func (f *fakeService) CommitHydratedManifests(_ context.Context, req *CommitHydratedManifestsRequest) (*CommitHydratedManifestsResponse, error) {
	f.seen++
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return &CommitHydratedManifestsResponse{HydratedSHA: f.sha}, nil
}

// ADR-0006, T19: token authentication, content type, body size cap and error mapping.
func TestHTTPHandler(t *testing.T) {
	t.Parallel()
	svc := &fakeService{sha: drySHA2}
	ts := newTestServer(t, svc, HandlerOptions{MaxRequestBytes: 4096})
	body, err := json.Marshal(newRequest("https://git.example.com/r.git", "main", drySHA1, pathOf("a", configMap("a", "1"))))
	require.NoError(t, err)

	do := func(method, path, token, contentType string, body []byte) (*http.Response, problem) {
		t.Helper()
		req, err := http.NewRequestWithContext(testCtx(t), method, ts.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var p problem
		if resp.Header.Get("Content-Type") == "application/problem+json" {
			require.NoError(t, json.Unmarshal(data, &p))
		}
		return resp, p
	}

	resp, _ := do(http.MethodGet, PathHealthz, "", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "health needs no token")

	resp, p := do(http.MethodPost, PathCommitHydratedManifests, "", "application/json", body)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, CodeUnauthenticated, p.Code)
	resp, _ = do(http.MethodPost, PathCommitHydratedManifests, "wrong", "application/json", body)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = do(http.MethodPost, PathCommitHydratedManifests, testToken+"x", "application/json", body)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Zero(t, svc.seen, "unauthenticated requests never reach the service")

	resp, _ = do(http.MethodPost, PathCommitHydratedManifests, testToken, "text/plain", body)
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	resp, p = do(http.MethodPost, PathCommitHydratedManifests, testToken, "application/json", []byte("{"))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, CodeInvalidArgument, p.Code)
	resp, p = do(http.MethodPost, PathCommitHydratedManifests, testToken, "application/json", bytes.Repeat([]byte(" "), 5000))
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, CodeTooLarge, p.Code)
	resp, _ = do(http.MethodGet, PathCommitHydratedManifests, testToken, "", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp, _ = do(http.MethodGet, "/other", testToken, "", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, _ = do(http.MethodPost, PathCommitHydratedManifests, testToken, "application/json; charset=utf-8", body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, svc.seen)
	assert.Equal(t, drySHA1, svc.got.DrySHA)

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{invalidf("x"), http.StatusBadRequest, CodeInvalidArgument},
		{ErrConcurrentUpdate, http.StatusConflict, CodeAborted},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, CodeDeadlineExceeded},
		{errors.New("boom"), http.StatusInternalServerError, CodeInternal},
	} {
		svc.err = tc.err
		resp, p = do(http.MethodPost, PathCommitHydratedManifests, testToken, "application/json", body)
		assert.Equal(t, tc.status, resp.StatusCode, tc.err.Error())
		assert.Equal(t, tc.code, p.Code)
		assert.Equal(t, tc.status, p.Status)
	}
}

func TestNewHandlerAndClientValidation(t *testing.T) {
	t.Parallel()
	_, err := NewHandler(&fakeService{}, HandlerOptions{})
	require.Error(t, err, "token required")
	_, err = NewHandler(nil, HandlerOptions{Token: "x"})
	require.Error(t, err)
	_, err = NewClient("host:8086", "")
	require.Error(t, err)
	_, err = NewClient("ftp://host", "t")
	require.Error(t, err)
	c, err := NewClient("https://commit.example:8086/", "t")
	require.NoError(t, err)
	assert.Equal(t, "https://commit.example:8086"+PathCommitHydratedManifests, c.endpoint)
}

func TestClientHandlesUnexpectedResponses(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Case") {
		case "html":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "t", WithHTTPClient(&http.Client{Transport: headerTransport{"X-Case", "html"}}))
	require.NoError(t, err)
	_, err = c.CommitHydratedManifests(testCtx(t), &CommitHydratedManifestsRequest{Repo: apiclient.Repository{URL: "x"}})
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusBadGateway, e.Status)
	assert.Equal(t, CodeInternal, e.Code)

	c, err = NewClient(ts.URL, "t")
	require.NoError(t, err)
	_, err = c.CommitHydratedManifests(testCtx(t), &CommitHydratedManifestsRequest{})
	require.ErrorContains(t, err, "no hydrated SHA")
}

type headerTransport struct{ key, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(r)
}
