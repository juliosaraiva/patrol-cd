// Package cache implements the controller-owned, per-cluster live-state cache.
//
// A [Cluster] discovers every preferred API resource of a destination cluster that supports
// list and watch, lists it with pagination and keeps it current with resourceVersion-based
// watches. Objects are reduced to compact [Resource] records (key, UID, owner references,
// tracking metadata and an optional caller-computed Info value). Full manifests are retained
// only for objects the caller marks as managed, with managedFields and the kubectl
// last-applied annotation stripped to bound memory.
//
// The cache is disposable: it holds no authoritative state and can be rebuilt from the API
// server at any time (see docs/architecture/adr/0004-no-redis.md). A [Manager] owns one
// [Cluster] per destination cluster.
//
// Concurrency model:
//
//   - All graph mutations happen in short write-locked critical sections. Namespace maps are
//     copy-on-write: a map published while the lock is not write-held is never mutated again,
//     so readers (FindResources, IterateHierarchy callbacks, OnResourceUpdated handlers) can
//     use them without holding any lock.
//   - Watch events are converted to [Resource] values outside the lock, queued in order, and
//     applied in batches by a single processor goroutine, which invokes event handlers after
//     releasing the lock.
//   - [Resource] values are immutable once published; updates replace the pointer.
package cache
