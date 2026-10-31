// Package clusterinfo is the controller's cluster-info updater (upstream clusterInfoUpdater,
// REPO-094, REPO-095, OPS-050): on an interval it writes the connection state, server version, API
// versions, cache statistics and Application count of every Cluster of an owned shard, from the
// live-state caches, and keeps the Kubernetes version label of the clusters that opted in.
//
// Upstream writes to Redis. Patrol has no Redis (ADR-0004), so the information goes to
// Cluster.status in etcd, where every write reaches every Cluster informer. An unchanged status is
// therefore not written, and a change of the cache statistics alone, which a busy cluster has all
// the time, is written at most once per Options.CacheInfoWriteInterval.
package clusterinfo
