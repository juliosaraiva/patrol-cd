package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

// forbiddenAPIServer answers every request with 403 Forbidden after hold(n) returns, n
// counting requests from 1.
func forbiddenAPIServer(t *testing.T, hold func(r *http.Request, n int64)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if hold != nil {
			hold(r, n)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"secrets is forbidden (test)","reason":"Forbidden","code":403}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestBootstrapClientNotThrottled (finding BS-3): the command's client has no client-side rate
// limiter, so a run that keeps failing with Forbidden ends at the deadline with that Forbidden
// error. With client-go's token bucket, a low QPS made the limiter refuse a request before the
// context ended ("rate: Wait(n=1) would exceed context deadline", not a context error), and
// that error was reported instead of the Forbidden.
func TestBootstrapClientNotThrottled(t *testing.T) {
	t.Parallel()
	srv, requests := forbiddenAPIServer(t, nil)
	// After the first request client-go's token bucket would allow the next one 10 s later.
	kube, err := newBootstrapClient(&rest.Config{Host: srv.URL, QPS: 0.1, Burst: 1})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = RunBootstrap(ctx, BootstrapConfig{Namespace: "patrol", Kube: kube, Logger: slog.New(slog.DiscardHandler)})
	require.ErrorContains(t, err, "bootstrap did not converge before the deadline")
	assert.True(t, apierrors.IsForbidden(err), err)
	assert.NotContains(t, err.Error(), "rate limiter")
	assert.Greater(t, requests.Load(), int64(2), "attempts are spaced by the backoff only")
}

// TestBootstrapDeadlineCutsRequest: when the deadline ends while a request is in flight, the
// error still names the last retriable failure (Forbidden), followed by the interrupted
// attempt's error.
func TestBootstrapDeadlineCutsRequest(t *testing.T) {
	t.Parallel()
	srv, _ := forbiddenAPIServer(t, func(r *http.Request, n int64) {
		if n > 1 {
			<-r.Context().Done() // hold every request after the first until the client gives up
		}
	})
	kube, err := newBootstrapClient(&rest.Config{Host: srv.URL})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = RunBootstrap(ctx, BootstrapConfig{Namespace: "patrol", Kube: kube, Logger: slog.New(slog.DiscardHandler)})
	require.ErrorContains(t, err, "bootstrap did not converge before the deadline")
	require.ErrorContains(t, err, "(last attempt: ")
	assert.True(t, apierrors.IsForbidden(err), err)
}
