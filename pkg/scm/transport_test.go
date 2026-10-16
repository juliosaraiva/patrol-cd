package scm_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// scripted is a RoundTripper returning scripted responses in order.
type scripted struct {
	mu        sync.Mutex
	responses []func(*http.Request) (*http.Response, error)
	requests  []*http.Request
	bodies    []string
}

func (s *scripted) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(b))
	}
	if len(s.responses) == 0 {
		return resp(r, 200, nil, "ok"), nil
	}
	f := s.responses[0]
	s.responses = s.responses[1:]
	return f(r)
}

func resp(r *http.Request, status int, h http.Header, body string) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: -1}
}

func status(code int, kv ...string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return resp(r, code, h, `{"message":"x"}`), nil
	}
}

type sleeper struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (s *sleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.sleeps = append(s.sleeps, d)
	s.mu.Unlock()
	return ctx.Err()
}

var fixedNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func chain(rt http.RoundTripper, sl *sleeper, p scm.RetryPolicy) *http.Client {
	return &http.Client{Transport: scm.Chain(rt, scm.ChainOptions{Provider: "test", Retry: p, Sleep: sl.Sleep, Clock: func() time.Time { return fixedNow }})}
}

func TestRetryTransport(t *testing.T) {
	t.Parallel()
	reset := strconv.FormatInt(fixedNow.Add(20*time.Second).Unix(), 10)
	farReset := strconv.FormatInt(fixedNow.Add(time.Hour).Unix(), 10)
	tests := []struct {
		name       string
		method     string
		script     []func(*http.Request) (*http.Response, error)
		wantStatus int
		wantCalls  int
		wantSleeps []time.Duration // exact waits; nil = don't check
	}{
		{"503 then success", http.MethodGet, []func(*http.Request) (*http.Response, error){status(503), status(200)}, 200, 2, nil},
		{"429 honors Retry-After", http.MethodGet, []func(*http.Request) (*http.Response, error){status(429, "Retry-After", "2"), status(200)}, 200, 2, []time.Duration{2 * time.Second}},
		{"primary rate limit waits until reset", http.MethodGet,
			[]func(*http.Request) (*http.Response, error){status(403, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset), status(200)}, 200, 2, []time.Duration{20 * time.Second}},
		{"reset beyond MaxWait is surfaced", http.MethodGet,
			[]func(*http.Request) (*http.Response, error){status(403, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", farReset)}, 403, 1, []time.Duration{}},
		{"secondary limit with Retry-After", http.MethodGet, []func(*http.Request) (*http.Response, error){status(403, "Retry-After", "3"), status(200)}, 200, 2, []time.Duration{3 * time.Second}},
		{"plain 403 not retried", http.MethodGet, []func(*http.Request) (*http.Response, error){status(403)}, 403, 1, []time.Duration{}},
		{"404 not retried", http.MethodGet, []func(*http.Request) (*http.Response, error){status(404)}, 404, 1, []time.Duration{}},
		{"POST not retried", http.MethodPost, []func(*http.Request) (*http.Response, error){status(503)}, 503, 1, []time.Duration{}},
		{"attempts exhausted", http.MethodGet, []func(*http.Request) (*http.Response, error){status(502), status(502), status(502)}, 502, 3, nil},
		{"network error retried", http.MethodGet, []func(*http.Request) (*http.Response, error){
			func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }, status(200)}, 200, 2, nil},
		{"GitLab RateLimit headers", http.MethodGet,
			[]func(*http.Request) (*http.Response, error){status(429, "RateLimit-Remaining", "0", "RateLimit-Reset", reset), status(200)}, 200, 2, []time.Duration{20 * time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &scripted{responses: tc.script}
			sl := &sleeper{}
			c := chain(s, sl, scm.RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond, MaxWait: time.Minute})
			req, _ := http.NewRequestWithContext(t.Context(), tc.method, "https://api.example/x", nil)
			r, err := c.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			_ = r.Body.Close()
			if r.StatusCode != tc.wantStatus || len(s.requests) != tc.wantCalls {
				t.Fatalf("status %d after %d calls, want %d after %d", r.StatusCode, len(s.requests), tc.wantStatus, tc.wantCalls)
			}
			if tc.wantSleeps != nil {
				if len(sl.sleeps) != len(tc.wantSleeps) {
					t.Fatalf("sleeps = %v, want %v", sl.sleeps, tc.wantSleeps)
				}
				for i := range sl.sleeps {
					if sl.sleeps[i] != tc.wantSleeps[i] {
						t.Fatalf("sleeps = %v, want %v", sl.sleeps, tc.wantSleeps)
					}
				}
			}
			for _, d := range sl.sleeps {
				if d < 0 || d > time.Minute {
					t.Fatalf("sleep %v out of bounds", d)
				}
			}
		})
	}
}

func TestRetryHonorsClassifiedInnerErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind      scm.ErrorKind
		wantCalls int
	}{
		{scm.KindUnauthorized, 1},
		{scm.KindNotFound, 1},
		{scm.KindInvalid, 1},
		{scm.KindUnavailable, 3},
		{scm.KindRateLimited, 3},
	}
	for _, tc := range tests {
		calls := 0
		inner := roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, &scm.Error{Kind: tc.kind, Op: "Token", Err: errors.New("token source")}
		})
		sl := &sleeper{}
		c := chain(inner, sl, scm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond})
		_, err := c.Get("https://api.example/x")
		if scm.KindOf(err) != tc.kind {
			t.Errorf("%s: err = %v", tc.kind, err)
		}
		if calls != tc.wantCalls {
			t.Errorf("%s: %d attempts, want %d", tc.kind, calls, tc.wantCalls)
		}
	}
}

func TestRetryReplaysBody(t *testing.T) {
	t.Parallel()
	s := &scripted{responses: []func(*http.Request) (*http.Response, error){status(503), status(200)}}
	c := chain(s, &sleeper{}, scm.RetryPolicy{MaxAttempts: 2})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://api.example/x", bytes.NewBufferString(`{"a":1}`))
	r, err := c.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("%v %v", r, err)
	}
	if len(s.bodies) != 2 || s.bodies[0] != `{"a":1}` || s.bodies[1] != `{"a":1}` {
		t.Fatalf("bodies = %q", s.bodies)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	s := &scripted{responses: []func(*http.Request) (*http.Response, error){status(503), status(200)}}
	ctx, cancel := context.WithCancel(t.Context())
	sl := &sleeper{}
	c := &http.Client{Transport: scm.Chain(s, scm.ChainOptions{Retry: scm.RetryPolicy{MaxAttempts: 3}, Sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		return sl.Sleep(ctx, d)
	}})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/x", nil)
	if _, err := c.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(s.requests) != 1 {
		t.Fatalf("requests = %d", len(s.requests))
	}
}

func TestLimitResponseBody(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 100)
	tests := []struct {
		name          string
		contentLength int64
		max           int64
		wantErr       bool
	}{
		{"declared too large", 100, 50, true},
		{"streamed too large", -1, 50, true},
		{"exactly at limit", -1, 100, false},
		{"under limit", 100, 1000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := scm.LimitResponseBody(roundTrip(func(r *http.Request) (*http.Response, error) {
				res := resp(r, 200, nil, big)
				res.ContentLength = tc.contentLength
				return res, nil
			}), tc.max)
			req, _ := http.NewRequest(http.MethodGet, "https://x", nil)
			res, err := rt.RoundTrip(req)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, scm.ErrResponseTooLarge) {
				t.Fatalf("err = %v, want ErrResponseTooLarge", err)
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{" 0 ", 0, true},
		{"-1", 0, false},
		{"abc", 0, false},
		{fixedNow.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{fixedNow.Add(-30 * time.Second).Format(http.TimeFormat), 0, true},
		{"999999999", 24 * time.Hour, true},
	}
	for _, tc := range tests {
		got, ok := scm.ParseRetryAfter(tc.in, fixedNow)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseRetryAfter(%q) = %v %v, want %v %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRateLimitWait(t *testing.T) {
	t.Parallel()
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	reset := strconv.FormatInt(fixedNow.Add(time.Minute).Unix(), 10)
	tests := []struct {
		name    string
		h       http.Header
		want    time.Duration
		limited bool
	}{
		{"no headers", h(), 0, false},
		{"remaining non-zero", h("X-RateLimit-Remaining", "10", "X-RateLimit-Reset", reset), 0, false},
		{"exhausted", h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset), time.Minute, true},
		{"exhausted with Retry-After", h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset, "Retry-After", "7"), 7 * time.Second, true},
		{"exhausted reset in past", h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", "1"), 0, true},
		{"gitlab style", h("RateLimit-Remaining", "0", "RateLimit-Reset", reset), time.Minute, true},
	}
	for _, tc := range tests {
		got, limited := scm.RateLimitWait(tc.h, fixedNow)
		if got != tc.want || limited != tc.limited {
			t.Errorf("%s: got %v %v, want %v %v", tc.name, got, limited, tc.want, tc.limited)
		}
	}
}

func TestProxyFunc(t *testing.T) {
	t.Parallel()
	if _, err := scm.ProxyFunc("://bad", ""); err == nil {
		t.Fatal("expected error for invalid proxy URL")
	}
	if _, err := scm.ProxyFunc("ftp://proxy:21", ""); err == nil {
		t.Fatal("expected error for unsupported proxy scheme")
	}
	f, err := scm.ProxyFunc("http://user:pw@proxy.corp:3128", "internal.example, .corp.example,10.0.0.0/8,192.168.1.5,git.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		url    string
		bypass bool
	}{
		{"https://api.github.com/x", false},
		{"https://internal.example/x", true},
		{"https://sub.internal.example/x", true},
		{"https://notinternal.example/x", false},
		{"https://a.corp.example/x", true},
		{"https://corp.example/x", false},
		{"https://10.1.2.3/x", true},
		{"https://192.168.1.5/x", true},
		{"https://192.168.1.6/x", false},
		{"https://git.example:8443/x", true},
		{"https://git.example/x", false},
	}
	for _, tc := range tests {
		u, _ := url.Parse(tc.url)
		got, err := f(&http.Request{URL: u})
		if err != nil {
			t.Fatal(err)
		}
		if (got == nil) != tc.bypass {
			t.Errorf("%s: proxy=%v, want bypass=%v", tc.url, got, tc.bypass)
		}
	}
	all, _ := scm.ProxyFunc("http://proxy:1", "*")
	u, _ := url.Parse("https://anything/")
	if p, _ := all(&http.Request{URL: u}); p != nil {
		t.Fatal("* must bypass every host")
	}
}

func TestHostGuardAndBearer(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	seen := map[string]string{}
	plain := roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen[r.URL.Host] = r.Header.Get("Authorization")
		mu.Unlock()
		code := 200
		if r.URL.Path == "/unauthorized" {
			code = 401
		}
		return resp(r, code, nil, ""), nil
	})
	var rejected []string
	authed := scm.BearerTransport(plain, func(context.Context) (string, error) { return "tok", nil }, func(tok string) { rejected = append(rejected, tok) })
	rt := scm.HostGuard("api.example", authed, plain)
	for _, u := range []string{"https://api.example/x", "https://objects.example/blob", "https://api.example/unauthorized"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		res, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if req.Header.Get("Authorization") != "" {
			t.Fatal("BearerTransport mutated the caller's request")
		}
	}
	if seen["api.example"] != "Bearer tok" || seen["objects.example"] != "" {
		t.Fatalf("authorization headers: %v", seen)
	}
	if len(rejected) != 1 || rejected[0] != "tok" {
		t.Fatalf("onUnauthorized calls = %v", rejected)
	}
	failing := scm.BearerTransport(plain, func(context.Context) (string, error) { return "", errors.New("no token") }, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://api.example/x", nil)
	if _, err := failing.RoundTrip(req); err == nil {
		t.Fatal("token error must fail the request")
	}
}

func TestBearerReplaysOnceWithFreshToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		valid      map[string]bool // tokens the server accepts
		tokens     []string        // successive values returned by the token func
		body       bool
		getBody    bool
		wantStatus int
		wantSent   []string
		wantReject []string
	}{
		{name: "fresh token accepted", valid: map[string]bool{"new": true}, tokens: []string{"old", "new"}, body: true, getBody: true,
			wantStatus: 200, wantSent: []string{"old", "new"}, wantReject: []string{"old"}},
		{name: "fresh token rejected too", valid: map[string]bool{}, tokens: []string{"old", "new"},
			wantStatus: 401, wantSent: []string{"old", "new"}, wantReject: []string{"old", "new"}},
		{name: "same token is not replayed", valid: map[string]bool{}, tokens: []string{"old", "old"},
			wantStatus: 401, wantSent: []string{"old"}, wantReject: []string{"old"}},
		{name: "unreplayable body", valid: map[string]bool{"new": true}, tokens: []string{"old", "new"}, body: true,
			wantStatus: 401, wantSent: []string{"old"}, wantReject: []string{"old"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sent, bodies, rejected []string
			next := roundTrip(func(r *http.Request) (*http.Response, error) {
				tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				sent = append(sent, tok)
				if r.Body != nil {
					b, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(b))
				}
				if tc.valid[tok] {
					return resp(r, 200, nil, "ok"), nil
				}
				return resp(r, 401, nil, `{"message":"Bad credentials"}`), nil
			})
			calls := 0
			tokFn := func(context.Context) (string, error) {
				v := tc.tokens[min(calls, len(tc.tokens)-1)]
				calls++
				return v, nil
			}
			rt := scm.BearerTransport(next, tokFn, func(tok string) { rejected = append(rejected, tok) })
			var req *http.Request
			switch {
			case tc.body && tc.getBody:
				req, _ = http.NewRequest(http.MethodPost, "https://api.example/x", strings.NewReader(`{"a":1}`))
			case tc.body:
				req, _ = http.NewRequest(http.MethodPost, "https://api.example/x", io.NopCloser(strings.NewReader(`{"a":1}`)))
			default:
				req, _ = http.NewRequest(http.MethodGet, "https://api.example/x", nil)
			}
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if !slices.Equal(sent, tc.wantSent) || !slices.Equal(rejected, tc.wantReject) {
				t.Fatalf("sent %v rejected %v; want %v, %v", sent, rejected, tc.wantSent, tc.wantReject)
			}
			if tc.body && tc.getBody {
				for _, b := range bodies {
					if b != `{"a":1}` {
						t.Fatalf("replayed body = %q", b)
					}
				}
			}
		})
	}
}

func TestParseRateLimitHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    map[string]string
		want scm.RateLimitInfo
	}{
		{"none", nil, scm.RateLimitInfo{Limit: -1, Remaining: -1, Used: -1}},
		{"github", map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "4990", "X-RateLimit-Used": "10", "X-RateLimit-Reset": "1760000000", "X-RateLimit-Resource": "core"},
			scm.RateLimitInfo{Present: true, Limit: 5000, Remaining: 4990, Used: 10, Reset: time.Unix(1760000000, 0), Resource: "core"}},
		{"gitlab", map[string]string{"RateLimit-Limit": "600", "RateLimit-Remaining": "0", "RateLimit-Reset": "1760000001"},
			scm.RateLimitInfo{Present: true, Limit: 600, Remaining: 0, Used: -1, Reset: time.Unix(1760000001, 0)}},
		{"garbage", map[string]string{"X-RateLimit-Limit": "lots", "X-RateLimit-Remaining": "-3"},
			scm.RateLimitInfo{Limit: -1, Remaining: -1, Used: -1}},
	}
	for _, tc := range tests {
		h := http.Header{}
		for k, v := range tc.h {
			h.Set(k, v)
		}
		got := scm.ParseRateLimitHeaders(h)
		if got.Present != tc.want.Present || got.Limit != tc.want.Limit || got.Remaining != tc.want.Remaining ||
			got.Used != tc.want.Used || !got.Reset.Equal(tc.want.Reset) || got.Resource != tc.want.Resource {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestObserverSeesEveryAttempt(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []scm.RequestInfo
	obs := func(_ context.Context, info scm.RequestInfo) {
		mu.Lock()
		seen = append(seen, info)
		mu.Unlock()
	}
	transportErr := errors.New("connection reset")
	rt := &scripted{responses: []func(*http.Request) (*http.Response, error){
		status(502),
		func(*http.Request) (*http.Response, error) { return nil, transportErr },
		status(200, "X-RateLimit-Remaining", "41", "X-RateLimit-Limit", "60"),
	}}
	sl := &sleeper{}
	c := &http.Client{Transport: scm.Chain(rt, scm.ChainOptions{
		Provider: "test", Observer: obs, Sleep: sl.Sleep, Clock: func() time.Time { return fixedNow },
		Retry: scm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond},
	})}
	res, err := c.Get("https://api.example/repos/o/r/branches?page=2&access_token=x")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("observed %d attempts, want 3: %+v", len(seen), seen)
	}
	wantStatus := []int{502, 0, 200}
	for i, info := range seen {
		if info.Provider != "test" || info.Method != http.MethodGet || info.Host != "api.example" || info.Path != "/repos/o/r/branches" {
			t.Errorf("attempt %d: %+v", i, info)
		}
		if info.StatusCode != wantStatus[i] {
			t.Errorf("attempt %d: status %d, want %d", i, info.StatusCode, wantStatus[i])
		}
	}
	if !errors.Is(seen[1].Err, transportErr) || seen[1].RateLimit.Present {
		t.Errorf("failed attempt: %+v", seen[1])
	}
	if rl := seen[2].RateLimit; !rl.Present || rl.Remaining != 41 || rl.Limit != 60 {
		t.Errorf("rate limit = %+v", rl)
	}
}

func TestLoggingRedaction(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	inner := scm.BearerTransport(http.DefaultTransport, func(context.Context) (string, error) { return "ghp_topsecret", nil }, nil)
	c := &http.Client{Transport: scm.Chain(inner, scm.ChainOptions{Provider: "github", Logger: logger})}
	u := srv.URL + "/repos?access_token=leaked&page=2"
	u = strings.Replace(u, "http://", "http://user:pw@", 1)
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "scm request") || !strings.Contains(out, `"rateLimitRemaining":"42"`) {
		t.Fatalf("missing log line: %s", out)
	}
	for _, secret := range []string{"ghp_topsecret", "leaked", ":pw@"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q: %s", secret, out)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestRedactHeadersAndURL(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Authorization", "Bearer x")
	h.Set("X-Hub-Signature-256", "sha256=abc")
	h.Set("Accept", "application/json")
	r := scm.RedactHeaders(h)
	if r.Get("Authorization") != "REDACTED" || r.Get("X-Hub-Signature-256") != "REDACTED" || r.Get("Accept") != "application/json" {
		t.Fatalf("RedactHeaders = %v", r)
	}
	if h.Get("Authorization") != "Bearer x" {
		t.Fatal("RedactHeaders mutated its input")
	}
	if got := scm.RedactURLString("https://u:p@h/x?private_token=s&client_secret=c&page=2#frag"); strings.Contains(got, "p@") || strings.Contains(got, "=s") || strings.Contains(got, "=c") || !strings.Contains(got, "page=2") || strings.Contains(got, "frag") {
		t.Fatalf("RedactURLString = %q", got)
	}
	if scm.RedactURLString("%zz") != "REDACTED" || scm.RedactURL(nil) != "" {
		t.Fatal("unparsable / nil URL handling")
	}
}

func TestBaseTransport(t *testing.T) {
	t.Parallel()
	if _, err := scm.BaseTransport(scm.Config{CAData: []byte("not pem")}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("invalid CA: %v", err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	get := func(cfg scm.Config) error {
		rt, err := scm.BaseTransport(cfg)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Transport: rt, Timeout: 5 * time.Second}).Get(srv.URL)
		if err != nil {
			return err
		}
		return res.Body.Close()
	}
	if err := get(scm.Config{}); err == nil {
		t.Fatal("self-signed server must fail without CA")
	}
	caPEM := pemCert(srv.Certificate().Raw)
	if err := get(scm.Config{CAData: caPEM}); err != nil {
		t.Fatalf("with CAData: %v", err)
	}
	if err := get(scm.Config{Insecure: true}); err != nil {
		t.Fatalf("insecure: %v", err)
	}
	custom := &http.Client{Transport: srv.Client().Transport}
	if rt, err := scm.BaseTransport(scm.Config{HTTPClient: custom}); err != nil || rt != custom.Transport {
		t.Fatalf("HTTPClient transport not used: %v", err)
	}
	if _, err := scm.BaseTransport(scm.Config{ProxyURL: "::"}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
}

func TestNewLimiter(t *testing.T) {
	t.Parallel()
	if scm.NewLimiter(scm.RateLimit{RPS: -1}) != nil {
		t.Fatal("negative RPS must disable pacing")
	}
	l := scm.NewLimiter(scm.RateLimit{})
	if l == nil || l.Burst() != scm.DefaultRateLimitBurst || float64(l.Limit()) != scm.DefaultRateLimitRPS {
		t.Fatalf("defaults not applied: %v", l)
	}
	// A limiter with burst 1 delays the second request; with a short deadline the
	// second request fails instead of blocking.
	lim := scm.NewLimiter(scm.RateLimit{RPS: 0.001, Burst: 1})
	c := &http.Client{Transport: scm.Chain(roundTrip(func(r *http.Request) (*http.Response, error) { return resp(r, 200, nil, ""), nil }), scm.ChainOptions{Limiter: lim, Retry: scm.RetryPolicy{MaxAttempts: 1}})}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://x/", nil)
	if res, err := c.Do(req); err != nil {
		t.Fatalf("first request: %v", err)
	} else {
		_ = res.Body.Close()
	}
	// The next token is ~1000s away, far beyond this deadline, so Wait fails at once.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://x/", nil)
	if _, err := c.Do(req2); err == nil {
		t.Fatal("second request should be paced beyond the deadline")
	}
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// blockUntilDone simulates a server that never answers: the attempt ends only when its
// request context does, the way http.Transport reports it.
func blockUntilDone(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: r.Context().Err()}
}

// stallingBody returns headers at once and then blocks reads until ctx ends.
type stallingBody struct{ ctx context.Context }

func (b stallingBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b stallingBody) Close() error             { return nil }

func TestAttemptTimeout(t *testing.T) {
	t.Parallel()
	const d = 50 * time.Millisecond
	isAttemptTimeout := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, scm.ErrAttemptTimeout) {
			t.Fatalf("err = %v, want ErrAttemptTimeout", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt timeout must not look like the caller's deadline: %v", err)
		}
		var ne interface{ Timeout() bool }
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("attempt timeout must be a timeout net.Error: %v", err)
		}
	}

	t.Run("stalled attempt is retried", func(t *testing.T) {
		t.Parallel()
		s := &scripted{responses: []func(*http.Request) (*http.Response, error){blockUntilDone, status(200)}}
		c := chain(scm.AttemptTimeout(s, d), &sleeper{}, scm.RetryPolicy{MaxAttempts: 3})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/x", nil)
		r, err := c.Do(req)
		if err != nil || r.StatusCode != 200 || len(s.requests) != 2 {
			t.Fatalf("resp=%v err=%v calls=%d", r, err, len(s.requests))
		}
		_ = r.Body.Close()
	})
	t.Run("every attempt stalls", func(t *testing.T) {
		t.Parallel()
		s := &scripted{responses: []func(*http.Request) (*http.Response, error){blockUntilDone, blockUntilDone, blockUntilDone}}
		c := chain(scm.AttemptTimeout(s, d), &sleeper{}, scm.RetryPolicy{MaxAttempts: 3})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/x", nil)
		_, err := c.Do(req)
		isAttemptTimeout(t, err)
		if len(s.requests) != 3 {
			t.Fatalf("calls = %d, want 3", len(s.requests))
		}
	})
	t.Run("body read bounded", func(t *testing.T) {
		t.Parallel()
		rt := scm.AttemptTimeout(roundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: stallingBody{r.Context()}, Request: r}, nil
		}), d)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/x", nil)
		r, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		_, err = io.ReadAll(r.Body)
		isAttemptTimeout(t, err)
	})
	t.Run("deadline released on close", func(t *testing.T) {
		t.Parallel()
		var attemptCtx context.Context
		rt := scm.AttemptTimeout(roundTrip(func(r *http.Request) (*http.Response, error) {
			attemptCtx = r.Context()
			return resp(r, 200, nil, "ok"), nil
		}), time.Hour)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/x", nil)
		r, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if attemptCtx.Err() != nil {
			t.Fatal("attempt context ended before the body was closed")
		}
		_ = r.Body.Close()
		if attemptCtx.Err() == nil {
			t.Fatal("closing the body must release the attempt deadline")
		}
	})
	t.Run("caller cancellation is not an attempt timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		rt := scm.AttemptTimeout(roundTrip(func(r *http.Request) (*http.Response, error) {
			cancel()
			return blockUntilDone(r)
		}), time.Hour)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/x", nil)
		_, err := rt.RoundTrip(req)
		if !errors.Is(err, context.Canceled) || errors.Is(err, scm.ErrAttemptTimeout) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("caller deadline is not an attempt timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), d)
		defer cancel()
		rt := scm.AttemptTimeout(roundTrip(blockUntilDone), time.Hour)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/x", nil)
		_, err := rt.RoundTrip(req)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, scm.ErrAttemptTimeout) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Parallel()
		var inner http.RoundTripper = roundTrip(func(r *http.Request) (*http.Response, error) { return resp(r, 200, nil, ""), nil })
		got := scm.AttemptTimeout(inner, 0)
		if _, ok := got.(roundTrip); !ok {
			t.Fatalf("AttemptTimeout(rt, 0) must return rt unchanged, got %T", got)
		}
	})
}

// TestRetryWaitBeyondCallerDeadline: when the caller's deadline is closer than the next
// wait, the chain returns the last response or error immediately instead of sleeping
// into a bare deadline error.
func TestRetryWaitBeyondCallerDeadline(t *testing.T) {
	t.Parallel()
	policy := scm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Minute, MaxDelay: time.Minute, MaxWait: time.Hour}
	tests := []struct {
		name       string
		step       func(*http.Request) (*http.Response, error)
		wantStatus int // 0: want the step's error
	}{
		{"secondary rate limit", status(403, "Retry-After", "30"), 403},
		{"429", status(429, "Retry-After", "30"), 429},
		{"primary rate limit", status(403, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10)), 403},
		{"503 backoff", status(503), 503},
		{"network error backoff", func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &scripted{responses: []func(*http.Request) (*http.Response, error){tc.step, status(200)}}
			sl := &sleeper{}
			c := &http.Client{Transport: scm.Chain(s, scm.ChainOptions{Provider: "test", Retry: policy, Sleep: sl.Sleep})}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/x", nil)
			r, err := c.Do(req)
			if tc.wantStatus == 0 {
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connection reset") {
					t.Fatalf("err = %v, want the attempt's own error", err)
				}
			} else {
				if err != nil || r.StatusCode != tc.wantStatus {
					t.Fatalf("resp=%v err=%v, want status %d", r, err, tc.wantStatus)
				}
				_ = r.Body.Close()
			}
			if len(s.requests) != 1 || len(sl.sleeps) != 0 {
				t.Fatalf("calls=%d sleeps=%v; want 1 call and no sleep", len(s.requests), sl.sleeps)
			}
		})
	}
}
