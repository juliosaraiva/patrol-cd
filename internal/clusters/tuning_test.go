package clusters

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"pgregory.net/rapid"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestTuningFromLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want func(*Tuning)
	}{
		{name: "defaults", env: map[string]string{}, want: func(*Tuning) {}},
		{name: "qps drives the default burst", env: map[string]string{EnvK8sClientQPS: "20"}, want: func(t *Tuning) { t.QPS, t.Burst = 20, 40 }},
		{name: "explicit burst", env: map[string]string{EnvK8sClientQPS: "20", EnvK8sClientBurst: "7"}, want: func(t *Tuning) { t.QPS, t.Burst = 20, 7 }},
		{name: "invalid qps keeps default", env: map[string]string{EnvK8sClientQPS: "fast"}, want: func(*Tuning) {}},
		{name: "negative qps keeps default", env: map[string]string{EnvK8sClientQPS: "-1"}, want: func(*Tuning) {}},
		{name: "idle connections", env: map[string]string{EnvK8sClientMaxIdleConnections: "10"}, want: func(t *Tuning) { t.MaxIdleConns = 10 }},
		{name: "durations", env: map[string]string{
			EnvK8sTCPTimeout: "5s", EnvK8sTCPKeepAlive: "6s", EnvK8sTLSHandshakeTimeout: "7s",
			EnvK8sTCPIdleConnTimeout: "1d", EnvK8sServerSideTimeout: "2m",
		}, want: func(t *Tuning) {
			t.TCPTimeout, t.TCPKeepAlive, t.TLSHandshakeTimeout = 5*time.Second, 6*time.Second, 7*time.Second
			t.IdleConnTimeout, t.ServerSideTimeout = 24*time.Hour, 2*time.Minute
		}},
		{name: "negative duration keeps default", env: map[string]string{EnvK8sTCPTimeout: "-5s"}, want: func(*Tuning) {}},
		{name: "retries", env: map[string]string{EnvK8sClientRetryMax: "3", EnvK8sClientRetryBaseBackoff: "250"}, want: func(t *Tuning) {
			t.RetryMax, t.RetryBaseBackoff = 3, 250*time.Millisecond
		}},
		{name: "zero retries is the default", env: map[string]string{EnvK8sClientRetryMax: "0", EnvK8sClientRetryBaseBackoff: "0"}, want: func(*Tuning) {}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := DefaultTuning()
			tc.want(&want)
			assert.Equal(t, want, TuningFromLookup(lookupMap(tc.env), nil))
		})
	}
}

// TestTuningFromLookupBounds: any integer value is either accepted within bounds or replaced by
// the default.
func TestTuningFromLookupBounds(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.Int64Range(math.MinInt32, math.MaxInt32).Draw(rt, "n")
		tu := TuningFromLookup(lookupMap(map[string]string{
			EnvK8sClientMaxIdleConnections: strconv.FormatInt(n, 10),
			EnvK8sClientRetryMax:           strconv.FormatInt(n, 10),
			EnvK8sTCPTimeout:               strconv.FormatInt(n, 10) + "ms",
		}), nil)
		if n >= 0 {
			assert.Equal(rt, int(n), tu.MaxIdleConns)
			assert.Equal(rt, time.Duration(n)*time.Millisecond, tu.TCPTimeout)
		} else {
			assert.Equal(rt, DefaultMaxIdleConns, tu.MaxIdleConns)
			assert.Equal(rt, DefaultTCPTimeout, tu.TCPTimeout)
		}
		if n >= 1 {
			assert.Equal(rt, n, tu.RetryMax)
		} else {
			assert.Zero(rt, tu.RetryMax)
		}
	})
}

// innerTransport walks client-go wrappers down to the *http.Transport.
func TestClusterInfoIntervalFromLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value    string
		want     time.Duration
		warnings int
	}{
		{value: "", want: 10 * time.Second},
		{value: "10s", want: 10 * time.Second},
		{value: "30s", want: 30 * time.Second},
		{value: "1m", want: time.Minute},
		{value: "9s", want: 10 * time.Second, warnings: 1},
		{value: "61s", want: 10 * time.Second, warnings: 1},
		{value: "30", want: 10 * time.Second, warnings: 1},
		{value: "soon", want: 10 * time.Second, warnings: 1},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			lookup := func(name string) (string, bool) { return tc.value, name == EnvUpdateClusterInfoTimeout }
			got := ClusterInfoIntervalFromLookup(lookup, slog.New(slog.NewTextHandler(&logs, nil)))
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.warnings, strings.Count(logs.String(), "level=WARN"))
		})
	}
}

func innerTransport(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	for i := 0; i < 20 && rt != nil; i++ {
		if tr, ok := rt.(*http.Transport); ok {
			return tr
		}
		w, ok := rt.(interface{ WrappedRoundTripper() http.RoundTripper })
		require.True(t, ok, "%T does not expose its inner transport", rt)
		rt = w.WrappedRoundTripper()
	}
	t.Fatalf("no *http.Transport found")
	return nil
}

// TestApplyTuningTransport covers REPO-105: configured values are reflected in the transport,
// and the TLS/auth fields are baked in.
func TestApplyTuningTransport(t *testing.T) {
	t.Parallel()
	tu := Tuning{
		QPS: 11, Burst: 22, MaxIdleConns: 33, TCPTimeout: time.Second, TCPKeepAlive: time.Second,
		TLSHandshakeTimeout: 4 * time.Second, IdleConnTimeout: 5 * time.Second, ServerSideTimeout: 6 * time.Second,
	}
	c := cluster("t", "https://t.example", func(c *v1alpha1.Cluster) {
		c.Spec.Config.TLSClientConfig.Insecure = true
		c.Spec.Config.DisableCompression = true
	})
	cfg, err := RestConfig(context.Background(), c, map[string][]byte{"bearerToken": []byte("x")}, Options{Tuning: &tu})
	require.NoError(t, err)
	assert.Equal(t, rest.TLSClientConfig{}, cfg.TLSClientConfig)
	assert.Nil(t, cfg.ExecProvider)
	assert.Nil(t, cfg.WrapTransport, "no retries configured")
	assert.Equal(t, 6*time.Second, cfg.Timeout)
	assert.InDelta(t, 11, cfg.QPS, 0.001)
	assert.Equal(t, 22, cfg.Burst)
	tr := innerTransport(t, cfg.Transport)
	assert.Equal(t, 33, tr.MaxIdleConns)
	assert.Equal(t, 33, tr.MaxIdleConnsPerHost)
	assert.Equal(t, 33, tr.MaxConnsPerHost)
	assert.Equal(t, 4*time.Second, tr.TLSHandshakeTimeout)
	assert.Equal(t, 5*time.Second, tr.IdleConnTimeout)
	assert.True(t, tr.DisableCompression)
	require.NotNil(t, tr.TLSClientConfig)
	assert.True(t, tr.TLSClientConfig.InsecureSkipVerify)

	tu.RetryMax = 2
	cfg, err = RestConfig(context.Background(), c, nil, Options{Tuning: &tu})
	require.NoError(t, err)
	assert.NotNil(t, cfg.WrapTransport)
}

// TestRetries covers REPO-105: requests are retried on 5xx when RETRY_MAX > 0.
func TestRetries(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)
	data := map[string][]byte{"caData": srv.caPEM()}
	c := cluster("r", srv.URL)

	srv.mu.Lock()
	srv.statuses = []int{http.StatusServiceUnavailable, http.StatusInternalServerError}
	srv.mu.Unlock()
	tu := DefaultTuning()
	tu.RetryMax, tu.RetryBaseBackoff = 2, time.Millisecond
	_, err := probeWith(t, c, data, Options{Tuning: &tu})
	require.NoError(t, err)
	assert.Equal(t, 3, srv.requestCount())

	srv.mu.Lock()
	srv.statuses = []int{http.StatusServiceUnavailable}
	srv.mu.Unlock()
	_, err = probeWith(t, c, data, Options{})
	require.Error(t, err, "retries are disabled by default")
	assert.Equal(t, 4, srv.requestCount())
}

// TestRetryTransportProperties: a request failing k times succeeds after min(k, max)+1
// attempts, and 501 or 4xx responses are never retried.
func TestRetryTransportProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		maxRetries := rapid.Int64Range(0, 4).Draw(rt, "max")
		failures := rapid.IntRange(0, 6).Draw(rt, "failures")
		failStatus := rapid.SampledFrom([]int{0, 429, 500, 502, 503, 501, 404}).Draw(rt, "status")
		var calls atomic.Int32
		inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := int(calls.Add(1))
			body, _ := io.ReadAll(r.Body)
			assert.Equal(rt, "payload", string(body), "body replayed on attempt %d", n)
			if n <= failures {
				if failStatus == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: failStatus, Body: io.NopCloser(strings.NewReader("err"))}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		tr := &retryTransport{inner: inner, maxRetries: maxRetries, backoff: time.Microsecond}
		req, err := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader("payload"))
		require.NoError(rt, err)
		req.GetBody = nil
		resp, err := tr.RoundTrip(req)
		retriable := failStatus == 0 || failStatus == 429 || (failStatus >= 500 && failStatus != 501)
		wantCalls := 1
		if failures > 0 && retriable {
			wantCalls = min(failures, int(maxRetries)) + 1
		}
		assert.Equal(rt, int32(wantCalls), calls.Load())
		if failures == 0 || (retriable && int64(failures) <= maxRetries) {
			require.NoError(rt, err)
			assert.Equal(rt, 200, resp.StatusCode)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 503, Body: http.NoBody}, nil
	})
	tr := &retryTransport{inner: inner, maxRetries: 5, backoff: time.Hour}
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil).WithContext(ctx)
	_, err := tr.RoundTrip(req)
	require.ErrorIs(t, err, context.Canceled)
}
