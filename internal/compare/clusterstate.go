package compare

import (
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// ClusterCacheState is what the live-state cache cc knows about its cluster, in the form
// clusters.ComputeClusterInfo takes. A nil cache has no state.
func ClusterCacheState(cc cache.ClusterCache) *clusters.CacheState {
	if cc == nil {
		return nil
	}
	info := cc.GetClusterInfo()
	return &clusters.CacheState{
		ServerVersion:     info.K8SVersion,
		APIVersions:       cache.APIVersionStrings(cc.GetAPIResources()),
		APIsCount:         int64(info.APIsCount),
		ResourcesCount:    int64(info.ResourcesCount),
		LastCacheSyncTime: info.LastCacheSyncTime,
		SyncError:         info.SyncError,
	}
}
