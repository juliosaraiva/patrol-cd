package apiclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type recorded struct {
	Method, Path, RawPath, Query, Body string
	Header                             http.Header
}

// recorder answers every request with handler after recording it.
func recorder(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recorded{Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery, Body: string(b), Header: r.Header.Clone()})
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), reqs...)
	}
}

func okJSON(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

type listReq struct {
	Repo     string   `path:"repo" json:"-"`
	Sub      string   `path:"sub" json:"-"`
	Projects []string `query:"projects" json:"-"`
	Limit    int64    `query:"limit" json:"-"`
	Refresh  *bool    `query:"refresh" json:"-"`
	Empty    string   `query:"empty" json:"-"`
	Value    string   `json:"value"`
}

type bodyReq struct {
	Name string            `path:"name" json:"-"`
	Obj  map[string]string `body:"true" json:"-"`
}

func TestDoBuildsRequests(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, okJSON(map[string]string{"value": "ok"}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL + "/base", RootPath: "/patrol/", AuthToken: "a.b.c",
		Headers: []string{"X-Extra: 1"}, UserAgent: "patrol-cli/v1"})
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/base/patrol", c.BaseURL())

	f := false
	var out map[string]string
	err = c.Do(context.Background(), http.MethodPost, "/api/v1/repositories/{repo}/files/{sub...}",
		&listReq{Repo: "https://github.com/a/b", Sub: "dir/a b.yaml", Projects: []string{"p1", "p2"}, Limit: 5, Refresh: &f, Value: "v"}, &out)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"value": "ok"}, out)
	r := reqs()[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/base/patrol/api/v1/repositories/https:%2F%2Fgithub.com%2Fa%2Fb/files/dir/a%20b.yaml", r.RawPath,
		"single-segment values are escaped whole, multi-segment ones per segment")
	assert.Equal(t, "/base/patrol/api/v1/repositories/https://github.com/a/b/files/dir/a b.yaml", r.Path)
	assert.Equal(t, "limit=5&projects=p1&projects=p2&refresh=false", r.Query, "zero values are omitted, pointers are sent when set")
	assert.JSONEq(t, `{"value":"v"}`, r.Body)
	assert.Equal(t, "Bearer a.b.c", r.Header.Get("Authorization"))
	assert.Equal(t, "patrol", r.Header.Get(apiclient.CSRFHeader))
	assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
	assert.Equal(t, "1", r.Header.Get("X-Extra"))
	assert.Equal(t, "patrol-cli/v1", r.Header.Get("User-Agent"))

	require.NoError(t, c.Do(context.Background(), http.MethodPut, "/api/v1/things/{name}", &bodyReq{Name: "x", Obj: map[string]string{"k": "v"}}, nil))
	assert.JSONEq(t, `{"k":"v"}`, reqs()[1].Body, "the body field is the whole body")

	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/things/{name}", &bodyReq{Name: "x", Obj: map[string]string{"k": "v"}}, nil))
	assert.Empty(t, reqs()[2].Body, "GET carries no body")

	require.NoError(t, c.Do(context.Background(), http.MethodPost, "/api/v1/plain", nil, nil))
	assert.Equal(t, "{}", reqs()[3].Body)

	err = c.Do(context.Background(), http.MethodGet, "/api/v1/things/{name}", &bodyReq{}, nil)
	require.ErrorContains(t, err, `missing path parameter "name"`)
	err = c.Do(context.Background(), http.MethodGet, "/api/v1/x", "not a struct", nil)
	require.Error(t, err)
}

func TestNewClientOptions(t *testing.T) {
	t.Parallel()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: "patrol.example.com:8443", GRPCWebRootPath: "argo", GRPCWeb: true, PortForward: true})
	require.NoError(t, err)
	assert.Equal(t, "https://patrol.example.com:8443/argo", c.BaseURL(), "gRPC-only flags are ignored; the gRPC-Web root path is honored")
	c, err = apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: "localhost:8080", PlainText: true})
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", c.BaseURL())

	for _, bad := range []*apiclient.ClientOptions{
		nil, {}, {ServerAddr: "ftp://x"}, {ServerAddr: "https://user:pw@x"}, {ServerAddr: "https://x?y=1"},
		{ServerAddr: "x", Headers: []string{"no-colon"}}, {ServerAddr: "x", CertFile: "/nonexistent.pem"},
		{ServerAddr: "x", ClientCertFile: "c.pem"},
	} {
		_, err := apiclient.NewClient(bad)
		assert.Error(t, err, "%+v", bad)
	}
}

func TestProblemErrors(t *testing.T) {
	t.Parallel()
	srv, _ := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/problem":
			w.Header().Set("Content-Type", apiclient.ProblemContentType)
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"urn:patrol-cd:problem:resource_exhausted","title":"Too Many Requests","status":429,"detail":"slow down","code":"resource_exhausted","requestId":"r-1"}`))
		case "/text":
			w.Header().Set(apiclient.RequestIDHeader, "r-2")
			http.Error(w, "upstream proxy exploded", http.StatusBadGateway)
		case "/notfound":
			w.Header().Set("Content-Type", apiclient.ProblemContentType)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"detail":"app x not found","code":"not_found"}`))
		}
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()

	err = c.Do(ctx, http.MethodGet, "/problem", nil, nil)
	var ae *apiclient.APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, 429, ae.StatusCode)
	assert.Equal(t, 7, ae.RetryAfter)
	assert.Equal(t, "r-1", ae.Problem.RequestID)
	assert.True(t, apiclient.IsResourceExhausted(err))
	assert.Equal(t, "slow down (HTTP 429, resource_exhausted)", err.Error())

	err = c.Do(ctx, http.MethodGet, "/text", nil, nil)
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, apiclient.CodeUnavailable, ae.Code())
	assert.Equal(t, "upstream proxy exploded", ae.Problem.Detail)
	assert.Equal(t, "r-2", ae.Problem.RequestID)
	assert.True(t, apiclient.IsUnavailable(err))

	err = c.Do(ctx, http.MethodGet, "/notfound", nil, nil)
	assert.True(t, apiclient.IsNotFound(err))
	assert.False(t, apiclient.IsPermissionDenied(err))
	assert.False(t, apiclient.IsNotFound(errors.New("plain")))

	codes := map[int]apiclient.ErrorCode{400: "invalid_argument", 401: "unauthenticated", 403: "permission_denied", 404: "not_found",
		409: "conflict", 412: "failed_precondition", 413: "resource_exhausted", 422: "invalid_argument", 500: "internal", 503: "unavailable"}
	for status, code := range codes {
		assert.Equal(t, code, apiclient.CodeForStatus(status), status)
	}
}

func TestRetriesAndRenewal(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv, _ := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(apiclient.RenewTokenHeader, "new.renewed.token")
		okJSON(map[string]string{})(w, r)
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, HTTPRetryMax: 2, AuthToken: "old.session.token"})
	require.NoError(t, err)
	var renewed string
	c.OnTokenRenewed(func(tok string) { renewed = tok })
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/x", nil, nil))
	assert.Equal(t, int32(3), calls.Load())
	assert.Equal(t, "new.renewed.token", c.Token())
	assert.Equal(t, "new.renewed.token", renewed)

	// Logout never adopts or reports a renewed token, even if a server sends one.
	srvOut, _ := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiclient.RenewTokenHeader, "after.logout.token")
		okJSON(map[string]string{})(w, r)
	})
	co, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srvOut.URL, AuthToken: "old.session.token"})
	require.NoError(t, err)
	renewed = ""
	co.OnTokenRenewed(func(tok string) { renewed = tok })
	_, err = co.Logout(context.Background())
	require.NoError(t, err)
	assert.Empty(t, renewed, "no renewed token reported on logout")
	assert.Empty(t, co.Token())

	// Mutations are never retried.
	calls.Store(-10)
	var posts atomic.Int32
	srv2, _ := recorder(t, func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c2, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv2.URL, HTTPRetryMax: 3})
	require.NoError(t, err)
	err = c2.Do(context.Background(), http.MethodPost, "/x", nil, nil)
	assert.True(t, apiclient.IsUnavailable(err))
	assert.Equal(t, int32(1), posts.Load())
}

func TestStream(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/denied":
			w.Header().Set("Content-Type", apiclient.ProblemContentType)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"code":"permission_denied"}`))
			return
		case "/notsse":
			okJSON(map[string]string{})(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		write := func(s string) { _, _ = io.WriteString(w, s); fl.Flush() }
		write(": heartbeat\n\n")
		write("id: 1\ndata: {\"type\":\"ADDED\",\"object\":{\"name\":\"a\"}}\n\n")
		write("id: 2\nevent: custom\ndata: {\"type\":\"MODIFIED\",\n")
		write("data: \"object\":{\"name\":\"b\"}}\n\n")
		if r.URL.Path == "/fail" {
			write("event: error\ndata: {\"title\":\"Unauthorized\",\"status\":401,\"detail\":\"token revoked\",\"code\":\"unauthenticated\"}\n\n")
		}
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, AuthToken: "t.o.k"})
	require.NoError(t, err)
	ctx := context.Background()

	type obj struct {
		Name string `json:"name"`
	}
	s, err := c.Stream(ctx, "/ok", nil, &apiclient.StreamOptions{LastEventID: "0"})
	require.NoError(t, err)
	var got []string
	require.NoError(t, apiclient.Watch(s, func(ev apiclient.WatchEvent[obj]) error {
		got = append(got, ev.Type+":"+ev.Object.Name)
		return nil
	}))
	assert.Equal(t, []string{"ADDED:a", "MODIFIED:b"}, got)
	assert.Equal(t, "2", s.LastEventID)
	r := reqs()[0]
	assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
	assert.Equal(t, "0", r.Header.Get("Last-Event-ID"))
	assert.Equal(t, "Bearer t.o.k", r.Header.Get("Authorization"))

	s, err = c.Stream(ctx, "/fail", nil, nil)
	require.NoError(t, err)
	var n int
	err = apiclient.Watch(s, func(apiclient.WatchEvent[obj]) error { n++; return nil })
	assert.Equal(t, 2, n)
	assert.True(t, apiclient.IsUnauthenticated(err))
	assert.Contains(t, err.Error(), "token revoked")

	s, err = c.Stream(ctx, "/ok", nil, nil)
	require.NoError(t, err)
	err = apiclient.Watch(s, func(apiclient.WatchEvent[obj]) error { return apiclient.ErrStopWatch })
	require.NoError(t, err)

	_, err = c.Stream(ctx, "/denied", nil, nil)
	assert.True(t, apiclient.IsPermissionDenied(err))
	_, err = c.Stream(ctx, "/notsse", nil, nil)
	require.ErrorContains(t, err, "unexpected content type")

	s, err = c.Stream(ctx, "/ok", nil, &apiclient.StreamOptions{MaxEventBytes: 16})
	require.NoError(t, err)
	_, err = s.Next()
	require.Error(t, err, "events larger than the bound are rejected")
	_ = s.Close()
}

func TestCoreMethods(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiclient.PathSession:
			if r.Method == http.MethodPost {
				okJSON(apiclient.SessionResponse{Token: "s.e.s"})(w, r)
				return
			}
			okJSON(apiclient.LogoutResponse{})(w, r)
		case "/api/v1/account/can-i/applications/get/proj/app":
			okJSON(apiclient.CanIResponse{Value: "yes"})(w, r)
		default:
			okJSON(map[string]any{})(w, r)
		}
	})
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = c.Login(ctx, "admin", "pw")
	require.NoError(t, err)
	assert.Equal(t, "s.e.s", c.Token())
	assert.JSONEq(t, `{"username":"admin","password":"pw"}`, reqs()[0].Body)
	ok, err := c.CanI(ctx, "applications", "get", "proj/app")
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = c.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "ci", ExpiresIn: 60})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/account/ci/token", reqs()[2].Path)
	assert.JSONEq(t, `{"expiresIn":60}`, reqs()[2].Body)
	require.NoError(t, c.DeleteAccountToken(ctx, "ci", "id/1"))
	assert.Equal(t, "/api/v1/account/ci/token/id%2F1", reqs()[3].RawPath)
	for _, f := range []func() error{
		func() error { _, err := c.UserInfo(ctx); return err },
		func() error { _, err := c.ListAccounts(ctx); return err },
		func() error { _, err := c.GetAccount(ctx, "a"); return err },
		func() error { return c.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{NewPassword: "x"}) },
		func() error { _, err := c.GetSettings(ctx); return err },
		func() error { _, err := c.GetPluginSettings(ctx); return err },
		func() error { _, err := c.Version(ctx); return err },
		func() error {
			_, err := c.ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: "i.d.t"})
			return err
		},
	} {
		require.NoError(t, f())
	}
	_, err = c.Logout(ctx)
	require.NoError(t, err)
	assert.Empty(t, c.Token())
	var paths []string
	for _, r := range reqs() {
		paths = append(paths, r.Method+" "+r.Path)
	}
	assert.Contains(t, paths, "PUT /api/v1/account/password")
	assert.Contains(t, paths, "POST /api/v1/session/oidc-exchange")
	assert.Contains(t, paths, "DELETE /api/v1/session")
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv, _ := recorder(t, func(http.ResponseWriter, *http.Request) { <-block })
	defer close(block)
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, Timeout: 50 * time.Millisecond})
	require.NoError(t, err)
	err = c.Do(context.Background(), http.MethodGet, "/slow", nil, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(fmt.Sprint(err), "deadline"), err)
}
