package compare

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

type stateCache struct {
	cache.ClusterCache // unimplemented methods panic if called

	info cache.ClusterInfo
	apis []cache.APIResourceInfo
}

func (s stateCache) GetClusterInfo() cache.ClusterInfo        { return s.info }
func (s stateCache) GetAPIResources() []cache.APIResourceInfo { return s.apis }

func TestClusterCacheState(t *testing.T) {
	t.Parallel()
	assert.Nil(t, ClusterCacheState(nil))

	synced := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	syncErr := errors.New("cluster unreachable")
	got := ClusterCacheState(stateCache{
		info: cache.ClusterInfo{K8SVersion: "v1.36.0", APIsCount: 40, ResourcesCount: 900, LastCacheSyncTime: &synced, SyncError: syncErr},
		apis: []cache.APIResourceInfo{{
			GroupKind:            schema.GroupKind{Group: "apps", Kind: "Deployment"},
			GroupVersionResource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		}},
	})
	assert.Equal(t, &clusters.CacheState{
		ServerVersion: "v1.36.0", APIVersions: []string{"apps/v1", "apps/v1/Deployment"},
		APIsCount: 40, ResourcesCount: 900, LastCacheSyncTime: &synced, SyncError: syncErr,
	}, got)

	info := cache.ClusterInfo{K8SVersion: "v1.36.0", LastCacheSyncTime: &synced, Synced: true}
	assert.Equal(t, &synced, ClusterCacheState(stateCache{info: info}).LastCacheSyncTime)
	info.Synced = false
	assert.Nil(t, ClusterCacheState(stateCache{info: info}).LastCacheSyncTime, "an invalidated cache has no completed sync")
}
