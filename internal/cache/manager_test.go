package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestManagerLookup(t *testing.T) {
	t.Parallel()
	m := NewManager(ManagerOptions{})
	defer m.Close()

	_, ok := m.Lookup("never")
	assert.False(t, ok, "no Get created it")
	assert.Empty(t, m.ClustersInfo(), "a lookup creates nothing")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.Get(ctx, "down", &rest.Config{Host: "http://127.0.0.1:1"}, Settings{})
	require.Error(t, err)
	c, ok := m.Lookup("down")
	require.True(t, ok, "a failed sync keeps the cache")
	assert.Error(t, c.GetClusterInfo().SyncError)
	assert.NotNil(t, c.GetClusterInfo().LastCacheSyncTime)

	m.Invalidate("down")
	_, ok = m.Lookup("down")
	assert.False(t, ok, "invalidated")

	_, err = m.Get(ctx, "down", &rest.Config{Host: "http://127.0.0.1:1"}, Settings{})
	require.Error(t, err)
	m.Close()
	_, ok = m.Lookup("down")
	assert.False(t, ok, "closed")
}

func TestManagerLookupWaitsForOnCacheCreated(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	m := NewManager(ManagerOptions{OnCacheCreated: func(string, ClusterCache) {
		close(entered)
		<-release
	}})
	defer m.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = m.Get(ctx, "c", &rest.Config{Host: "http://127.0.0.1:1"}, Settings{})
	}()
	<-entered
	_, ok := m.Lookup("c")
	assert.False(t, ok, "the handlers are not registered yet")
	close(release)
	<-done
	_, ok = m.Lookup("c")
	assert.True(t, ok)
}
