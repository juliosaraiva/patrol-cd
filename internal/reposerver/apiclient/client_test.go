package apiclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail, Code: code})
}

func TestNewHTTPClientAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr string
		tls  bool
		want string
		err  bool
	}{
		{"repo-server:8081", false, "http://repo-server:8081", false},
		{"repo-server:8081", true, "https://repo-server:8081", false},
		{"https://repo-server:8081/", false, "https://repo-server:8081", false},
		{"http://repo-server:8081/prefix", false, "http://repo-server:8081/prefix", false},
		{"ftp://repo-server", false, "", true},
		{"http://user:pw@repo-server", false, "", true},
		{"http://repo-server?x=1", false, "", true},
		{"", false, "", true},
	} {
		var cfg *tls.Config
		if tc.tls {
			cfg = &tls.Config{}
		}
		c, err := NewHTTPClient(tc.addr, "token", cfg)
		if tc.err {
			require.Error(t, err, tc.addr)
			continue
		}
		require.NoError(t, err, tc.addr)
		assert.Equal(t, tc.want, c.base, tc.addr)
	}
	_, err := NewHTTPClient("repo-server:8081", "", nil)
	require.Error(t, err)
}

func TestHTTPClientRequestAndProblemMapping(t *testing.T) {
	t.Parallel()
	var gotAuth, gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotCT = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		var req ResolveRevisionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Revision {
		case "bad":
			problem(w, http.StatusBadRequest, CodeInvalidArgument, "revision is bad")
		case "garbage":
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, "<html>")
		default:
			_ = json.NewEncoder(w).Encode(ResolveRevisionResponse{Revision: "abc", AmbiguousRevision: req.Revision + " (abc)"})
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, "s3cret", nil)
	require.NoError(t, err)

	resp, err := c.ResolveRevision(context.Background(), &ResolveRevisionRequest{Revision: "main"})
	require.NoError(t, err)
	assert.Equal(t, "abc", resp.Revision)
	assert.Equal(t, "Bearer s3cret", gotAuth)
	assert.Equal(t, PathPrefix+MethodResolveRevision, gotPath)
	assert.Equal(t, "application/json", gotCT)

	resp, err = c.ResolveRevision(context.Background(), &ResolveRevisionRequest{Revision: "bad"})
	require.Error(t, err)
	assert.Nil(t, resp)
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, CodeInvalidArgument, e.Code)
	assert.Equal(t, "revision is bad", e.Message)

	_, err = c.ResolveRevision(context.Background(), &ResolveRevisionRequest{Revision: "garbage"})
	require.ErrorAs(t, err, &e)
	assert.Equal(t, CodeInternal, e.Code)
}

func TestHTTPClientRetriesUnavailable(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			problem(w, http.StatusServiceUnavailable, CodeUnavailable, "busy")
			return
		}
		_ = json.NewEncoder(w).Encode(Refs{Branches: []string{"main"}})
	}))
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, "t", nil, WithRetry(3, time.Millisecond, 5*time.Millisecond))
	require.NoError(t, err)
	refs, err := c.ListRefs(context.Background(), &ListRefsRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"main"}, refs.Branches)
	assert.Equal(t, int32(3), calls.Load())

	// Other errors are not retried.
	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		problem(w, http.StatusInternalServerError, CodeInternal, "boom")
	}))
	t.Cleanup(srv2.Close)
	c2, err := NewHTTPClient(srv2.URL, "t", nil, WithRetry(5, time.Millisecond, time.Millisecond))
	require.NoError(t, err)
	_, err = c2.ListRefs(context.Background(), &ListRefsRequest{})
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())

	// Connection failures are retried, then reported as unavailable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	c3, err := NewHTTPClient(addr, "t", nil, WithRetry(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)
	_, err = c3.ListRefs(context.Background(), &ListRefsRequest{})
	require.Error(t, err)
	assert.True(t, IsCode(err, CodeUnavailable), "%v", err)

	// A cancelled context is returned as such.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c3.ListRefs(ctx, &ListRefsRequest{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestHTTPClientSizeLimits(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"manifests":["`+strings.Repeat("x", 4096)+`"]}`)
	}))
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, "t", nil, WithMaxResponseBytes(1024), WithMaxRequestBytes(512))
	require.NoError(t, err)
	_, err = c.GenerateManifest(context.Background(), &ManifestRequest{AppName: "a"})
	require.Error(t, err)
	assert.True(t, IsCode(err, CodeResourceExhausted), "%v", err)
	_, err = c.GenerateManifest(context.Background(), &ManifestRequest{AppName: strings.Repeat("a", 1024)})
	require.Error(t, err)
	assert.True(t, IsCode(err, CodeResourceExhausted), "%v", err)
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

func TestHTTPClientGenerateManifestWithFiles(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		require.NoError(t, err)
		var names []string
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			names = append(names, p.FormName())
			data, _ := io.ReadAll(p)
			if p.FormName() == PartChecksum {
				assert.Len(t, string(data), 64)
			}
		}
		assert.Equal(t, []string{PartRequest, PartFiles, PartChecksum}, names)
		_ = json.NewEncoder(w).Encode(ManifestResponse{Revision: "streamed"})
	}))
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL, "t", nil)
	require.NoError(t, err)
	files := &closeRecorder{Reader: strings.NewReader("tar-bytes")}
	resp, err := c.GenerateManifestWithFiles(context.Background(), &ManifestRequest{AppName: "a"}, files)
	require.NoError(t, err)
	assert.Equal(t, "streamed", resp.Revision)
	assert.True(t, files.closed, "files are closed by the client")
}

func TestStatusCodeMapping(t *testing.T) {
	t.Parallel()
	for _, code := range []string{CodeInvalidArgument, CodeNotFound, CodePermissionDenied, CodeUnauthenticated, CodeUnavailable, CodeDeadlineExceeded, CodeInternal} {
		assert.Equal(t, code, CodeForStatus(StatusForCode(code)), code)
	}
	assert.Equal(t, CodeResourceExhausted, CodeForStatus(StatusForCode(CodeResourceExhausted)))
}
