package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestEnsureSyncedCallerCancellationIsNotRecorded(t *testing.T) {
	t.Parallel()
	c, err := New(closedPortConfig(t), Settings{})
	require.NoError(t, err)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, c.EnsureSynced(ctx), context.Canceled)
	assert.Nil(t, c.GetClusterInfo().LastCacheSyncTime)
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
