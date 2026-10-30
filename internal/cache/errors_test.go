package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func TestIsUnreachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, true},
		{"wrapped url error", fmt.Errorf("get server version: %w", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("tls: handshake failure")}), true},
		{"dns", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"503", apierrors.NewServiceUnavailable("down"), true},
		{"504", apierrors.NewTimeoutError("gateway", 1), true},
		{"403", apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("no")), false},
		{"401", apierrors.NewUnauthorized("who"), false},
		{"404", apierrors.NewNotFound(schema.GroupResource{}, "x"), false},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isUnreachable(tc.err))
			classified := classifySyncError("https://x", tc.err)
			assert.Equal(t, tc.want, errors.Is(classified, ErrClusterUnreachable))
			if tc.err != nil {
				assert.ErrorIs(t, classified, tc.err)
			}
		})
	}
}

func closedPortConfig(t *testing.T) *rest.Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return &rest.Config{Host: "http://" + addr, Timeout: 2 * time.Second}
}

func TestEnsureSyncedUnreachableCluster(t *testing.T) {
	t.Parallel()
	cfg := closedPortConfig(t)
	c, err := New(cfg, Settings{SyncRetryTimeout: -1})
	require.NoError(t, err)
	defer c.Close()

	err = c.EnsureSynced(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnreachable)
	var ue *UnreachableError
	require.ErrorAs(t, err, &ue)
	assert.Equal(t, cfg.Host, ue.Server)

	info := c.GetClusterInfo()
	assert.False(t, info.Synced)
	assert.ErrorIs(t, info.SyncError, ErrClusterUnreachable)
	require.NotNil(t, info.LastCacheSyncTime)

	// Retries happen on the next call (retry timeout disabled).
	first := *info.LastCacheSyncTime
	time.Sleep(5 * time.Millisecond)
	require.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClusterUnreachable)
	assert.True(t, c.GetClusterInfo().LastCacheSyncTime.After(first))

	// An invalidation drops the failure along with the state that failed.
	c.Invalidate()
	info = c.GetClusterInfo()
	assert.False(t, info.Synced)
	require.NoError(t, info.SyncError)
	require.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClusterUnreachable)
	assert.ErrorIs(t, c.GetClusterInfo().SyncError, ErrClusterUnreachable)

	_, err = c.GetManagedLiveObjs(context.Background(), nil, nil)
	assert.ErrorIs(t, err, ErrNotSynced)
	_, err = c.IsNamespaced(schema.GroupKind{Kind: "ConfigMap"})
	assert.ErrorIs(t, err, ErrUnknownGroupKind)
}

func TestEnsureSyncedRetryTimeout(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","code":503}`))
	}))
	defer srv.Close()

	c, err := New(&rest.Config{Host: srv.URL}, Settings{SyncRetryTimeout: time.Hour})
	require.NoError(t, err)
	defer c.Close()

	require.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClusterUnreachable)
	n := requests.Load()
	require.Positive(t, n)
	// Within the retry window the previous error is returned without contacting the cluster.
	require.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClusterUnreachable)
	assert.Equal(t, n, requests.Load())
}

func TestEnsureSyncedDoneContextStartsNoSync(t *testing.T) {
	t.Parallel()
	c, err := New(closedPortConfig(t), Settings{})
	require.NoError(t, err)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, c.EnsureSynced(ctx), context.Canceled)
	c.statusMu.Lock()
	assert.Nil(t, c.status.flight, "no sync was started for a caller that was already done")
	c.statusMu.Unlock()
	assert.Nil(t, c.GetClusterInfo().LastCacheSyncTime)
}

// Callers whose deadline is shorter than a sync stop waiting, but the sync is not cancelled:
// it completes once, under the cache's own context, and serves later callers.
func TestEnsureSyncedOutlivesCallerContext(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource)
	release := fc.holdVersion()
	c, err := New(fc.config(), Settings{EventProcessingInterval: -1}, fc.options()...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	t.Cleanup(release) // runs before Close

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			assert.ErrorIs(t, c.EnsureSynced(ctx), context.DeadlineExceeded)
		})
	}
	wg.Wait()
	assert.Nil(t, c.GetClusterInfo().LastCacheSyncTime, "the sync is still running")
	// The shared sync runs on its own goroutine; under load its version request may reach
	// the fake server only after every caller has timed out, so wait for it to arrive.
	require.Eventually(t, func() bool { return fc.versionRequests() >= 1 }, 20*time.Second, 10*time.Millisecond,
		"the shared sync never requested the server version")
	assert.Equal(t, 1, fc.versionRequests(), "callers share one sync")

	waiters := make(chan error, 3)
	for range 3 {
		go func() { waiters <- c.EnsureSynced(context.Background()) }()
	}
	release()
	for range 3 {
		select {
		case err := <-waiters:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("waiters did not receive the sync result")
		}
	}
	require.NoError(t, c.EnsureSynced(context.Background()))
	info := c.GetClusterInfo()
	assert.True(t, info.Synced)
	assert.Equal(t, 1, fc.versionRequests(), "the sync that callers gave up on was not restarted")
}

// An invalidated cache keeps the time of its last sync and reports no error: only Synced tells
// that it holds nothing yet.
func TestClusterInfoOfInvalidatedCache(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource)
	c := fc.start(t, Settings{})
	require.True(t, c.GetClusterInfo().Synced)
	c.Invalidate()
	info := c.GetClusterInfo()
	assert.False(t, info.Synced)
	assert.NotNil(t, info.LastCacheSyncTime)
	assert.NoError(t, info.SyncError)
	assert.Zero(t, info.APIsCount)
}

// A caller that joined a sync must not get its result when that sync completed for an epoch
// older than the call (Invalidate ran between the sync's swap and its completion).
func TestEnsureSyncedIgnoresResultOfOlderEpoch(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource)
	c := fc.start(t, Settings{})
	c.Invalidate()
	stale := &syncFlight{done: make(chan struct{})}
	c.statusMu.Lock()
	c.status.flight = stale
	c.statusMu.Unlock()

	result := make(chan error, 1)
	go func() { result <- c.EnsureSynced(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let the caller join the stale flight
	c.statusMu.Lock()
	stale.err, stale.epoch = nil, 0 // completed for the epoch before Invalidate
	c.status.flight = nil
	c.statusMu.Unlock()
	close(stale.done)

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("EnsureSynced did not return")
	}
	assert.True(t, c.GetClusterInfo().Synced, "the caller waited for a sync of the current epoch")
	assert.Equal(t, 2, fc.versionRequests())
}

func TestEnsureSyncedCloseWhileWaiting(t *testing.T) {
	t.Parallel()
	fc := newFakeCluster(t, cmResource)
	release := fc.holdVersion()
	t.Cleanup(release)
	c, err := New(fc.config(), Settings{EventProcessingInterval: -1}, fc.options()...)
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() { result <- c.EnsureSynced(context.Background()) }()
	eventually(t, 10*time.Second, func() bool { return fc.versionRequests() == 1 }, "sync started")
	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	eventually(t, 10*time.Second, func() bool { return c.rootCtx.Err() != nil }, "close cancelled the cache")
	release() // let the in-flight discovery request return
	select {
	case err := <-result:
		assert.ErrorIs(t, err, ErrClosed)
	case <-time.After(20 * time.Second):
		t.Fatal("waiter not released by Close")
	}
	<-closed
	assert.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClosed)
}

func TestClosedCache(t *testing.T) {
	t.Parallel()
	c, err := New(closedPortConfig(t), Settings{})
	require.NoError(t, err)
	c.Close()
	c.Close() // idempotent
	assert.ErrorIs(t, c.EnsureSynced(context.Background()), ErrClosed)

	_, err = New(nil, Settings{})
	assert.Error(t, err)
}

func TestOutOfScopeErrorMessages(t *testing.T) {
	t.Parallel()
	clusterLevel := &OutOfScopeError{Kind: "ClusterRole", Name: "admin"}
	assert.EqualError(t, clusterLevel, `cluster level ClusterRole "admin" can not be managed when in namespaced mode`)
	namespaced := &OutOfScopeError{Kind: "ConfigMap", Name: "cm", Namespace: "other"}
	assert.EqualError(t, namespaced, `namespace "other" for ConfigMap "cm" is not managed`)
	assert.ErrorIs(t, fmt.Errorf("wrapped: %w", namespaced), ErrOutOfScope)
}

func TestAPIServiceAvailable(t *testing.T) {
	t.Parallel()
	cond := func(status string) objOpt {
		return withField([]any{map[string]any{"type": "Other", "status": "True"}, map[string]any{"type": "Available", "status": status}}, "status", "conditions")
	}
	assert.True(t, apiServiceAvailable(mkObj("apiregistration.k8s.io/v1", "APIService", "", "v1.x", "u", cond("True"))))
	assert.False(t, apiServiceAvailable(mkObj("apiregistration.k8s.io/v1", "APIService", "", "v1.x", "u", cond("False"))))
	assert.False(t, apiServiceAvailable(mkObj("apiregistration.k8s.io/v1", "APIService", "", "v1.x", "u")))
}
