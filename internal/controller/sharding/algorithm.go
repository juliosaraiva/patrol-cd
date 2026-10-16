package sharding

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"strconv"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// Algorithm is a cluster distribution algorithm (controller.sharding.algorithm).
type Algorithm string

// Algorithms (upstream common.*ShardingAlgorithm names).
const (
	// AlgorithmLegacy assigns FNV-32a(cluster ID) mod replicas: stable per cluster, not uniform.
	AlgorithmLegacy Algorithm = "legacy"
	// AlgorithmRoundRobin assigns the rank of the cluster in the clusters sorted by ID modulo
	// replicas: uniform (±1), but adding or removing a cluster reshuffles the ones after it.
	AlgorithmRoundRobin Algorithm = "round-robin"
	// AlgorithmConsistentHashing assigns clusters with consistent hashing with bounded loads
	// weighted by Applications per cluster: near-uniform with few moves on changes.
	AlgorithmConsistentHashing Algorithm = "consistent-hashing"
	// DefaultAlgorithm is used when none (or an unknown one) is configured.
	DefaultAlgorithm = AlgorithmLegacy
)

// ParseAlgorithm returns the algorithm named s. Empty selects the default; unknown names log
// upstream's "distribution type <x> is not supported, defaulting to legacy".
func ParseAlgorithm(s string, logger *slog.Logger) Algorithm {
	switch a := Algorithm(s); a {
	case AlgorithmLegacy, AlgorithmRoundRobin, AlgorithmConsistentHashing:
		return a
	case "":
		return DefaultAlgorithm
	default:
		if logger != nil {
			logger.Warn(fmt.Sprintf("distribution type %s is not supported, defaulting to %s", s, DefaultAlgorithm), slog.String("algorithm", s))
		}
		return DefaultAlgorithm
	}
}

// ClusterInfo is the sharding view of a cluster.
type ClusterInfo struct {
	// ID is the cluster identity used by the algorithms: the Cluster object UID, "" for the
	// synthetic in-cluster cluster (upstream: the cluster Secret UID, "" for the local cluster
	// without a Secret).
	ID string
	// Server is the normalized server URL (the key of the assignment table).
	Server string
	// Name is spec.name (for messages and metrics).
	Name string
	// Namespace and ObjectName identify the Cluster object; ObjectName is empty for the
	// synthetic in-cluster cluster, whose status cannot be written.
	Namespace  string
	ObjectName string
	// Shard is spec.shard (an explicit pin), nil when unset.
	Shard *int64
	// SkipReconcile reports the cd.patrol.example/skip-reconcile annotation: no shard
	// processes the cluster's Applications.
	SkipReconcile bool
}

// ClusterInfoFrom returns the sharding view of c.
func ClusterInfoFrom(c *v1alpha1.Cluster) ClusterInfo {
	info := ClusterInfo{
		ID:            string(c.UID),
		Server:        clusters.NormalizeServer(c.Spec.Server),
		Name:          c.Spec.Name,
		Namespace:     c.Namespace,
		ObjectName:    c.Name,
		SkipReconcile: clusters.IsSkipReconcile(c),
	}
	if c.Spec.Shard != nil {
		s := *c.Spec.Shard
		info.Shard = &s
	}
	return info
}

// pinned returns the explicit shard when it is valid for replicas.
func (c *ClusterInfo) pinned(replicas int) (int, bool) {
	if c.Shard == nil || *c.Shard < 0 || *c.Shard >= int64(replicas) {
		return 0, false
	}
	return int(*c.Shard), true
}

// LegacyShard is upstream's LegacyDistributionFunction: -1 without replicas, 0 for a nil
// cluster or an empty ID, the explicit shard when lower than replicas, else
// FNV-32a(ID) mod replicas.
func LegacyShard(c *ClusterInfo, replicas int) int {
	if replicas <= 0 {
		return -1
	}
	if c == nil {
		return 0
	}
	if s, ok := c.pinned(replicas); ok {
		return s
	}
	if c.ID == "" {
		return 0
	}
	return legacyHash(c.ID, replicas)
}

func legacyHash(id string, replicas int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(replicas)) //nolint:gosec // replicas is in (0, MaxReplicas]
}

// sortedByID returns the clusters sorted by ID (ties by server, so the order is total).
func sortedByID(list []ClusterInfo) []ClusterInfo {
	out := slices.Clone(list)
	slices.SortStableFunc(out, func(a, b ClusterInfo) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Server, b.Server))
	})
	return out
}

// RoundRobinShard is upstream's RoundRobinDistributionFunction: -1 without replicas, 0 for a
// nil cluster, the explicit shard when lower than replicas, -1 when c is not in list (by
// ID), else its rank in list sorted by ID modulo replicas.
func RoundRobinShard(c *ClusterInfo, list []ClusterInfo, replicas int) int {
	if replicas <= 0 {
		return -1
	}
	if c == nil {
		return 0
	}
	if s, ok := c.pinned(replicas); ok {
		return s
	}
	rank := roundRobinRanks(list)
	r, ok := rank[c.ID]
	if !ok {
		return -1
	}
	return r % replicas
}

// roundRobinRanks maps IDs to their rank in the clusters sorted by ID (a repeated ID keeps
// its last rank, as upstream's map).
func roundRobinRanks(list []ClusterInfo) map[string]int {
	sorted := sortedByID(list)
	rank := make(map[string]int, len(sorted))
	for i, c := range sorted {
		rank[c.ID] = i
	}
	return rank
}

// ConsistentHashingShard is upstream's ConsistentHashingWithBoundedLoadsDistributionFunction
// for one cluster: -1 without replicas, 0 for a nil cluster, the explicit shard when lower
// than replicas, -1 when c is not in list (by ID and server), else the bounded-load ring
// assignment of list (see ConsistentHashingShards).
func ConsistentHashingShard(c *ClusterInfo, list []ClusterInfo, appsPerServer map[string]int64, replicas int) int {
	if replicas <= 0 {
		return -1
	}
	if c == nil {
		return 0
	}
	if s, ok := c.pinned(replicas); ok {
		return s
	}
	if !slices.ContainsFunc(list, func(o ClusterInfo) bool { return o.ID == c.ID && o.Server == c.Server }) {
		return -1
	}
	shard, ok := ConsistentHashingShards(list, appsPerServer, replicas)[c.ID]
	if !ok {
		return -1
	}
	return shard
}

// ConsistentHashingShards assigns every cluster of list (keyed by ID) like upstream's
// createConsistentHashingWithBoundLoads: a ring with members "0".."replicas-1" (1000 virtual
// nodes each, BLAKE2b-512 hashes); clusters are visited in ID order, each takes the least
// loaded acceptable member clockwise from the hash of its ID, then that member's load grows
// by the number of Applications whose destination server is the cluster's server. A ring
// lookup failure assigns shard 0. Explicit pins are not considered here (they only occupy a
// position in the order).
func ConsistentHashingShards(list []ClusterInfo, appsPerServer map[string]int64, replicas int) map[string]int {
	out := make(map[string]int, len(list))
	if replicas <= 0 {
		return out
	}
	r := newBoundedRing(replicas)
	loads := make(map[string]int64, replicas)
	for _, c := range sortedByID(list) {
		member, err := r.getLeast(c.ID)
		if err != nil {
			out[c.ID] = 0
			continue
		}
		shard, err := strconv.Atoi(member)
		if err != nil {
			shard = 0
		}
		out[c.ID] = shard
		loads[member] += appsPerServer[c.Server]
		r.updateLoad(member, loads[member])
	}
	return out
}

// Distribute computes the shard of every cluster (keyed by server) the way upstream's
// ClusterSharding.updateDistribution does at runtime:
//
//   - with fewer than two replicas every cluster maps to shard 0 (no sharding);
//   - an explicit spec.shard lower than replicas wins; a higher one is ignored with upstream's
//     warning "Specified cluster shard (n) for cluster: <server> is greater than the number of
//     available shard (r). Using shard 0." and the cluster maps to shard 0;
//   - otherwise the algorithm decides; a ring failure or an unassignable cluster maps to 0.
//
// appsPerServer counts Applications by spec.destination.server (consistent hashing only).
func Distribute(alg Algorithm, list []ClusterInfo, appsPerServer map[string]int64, replicas int, logger *slog.Logger) map[string]int {
	out := make(map[string]int, len(list))
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	var ranks map[string]int
	var ring map[string]int
	switch {
	case replicas <= 1:
	case alg == AlgorithmRoundRobin:
		ranks = roundRobinRanks(list)
	case alg == AlgorithmConsistentHashing:
		ring = ConsistentHashingShards(list, appsPerServer, replicas)
	}
	for i := range list {
		c := &list[i]
		shard := 0
		switch {
		case c.Shard != nil:
			if s, ok := c.pinned(max(replicas, 1)); ok {
				shard = s
			} else {
				logger.Warn(fmt.Sprintf("Specified cluster shard (%d) for cluster: %s is greater than the number of available shard (%d). Using shard 0.", *c.Shard, c.Server, replicas),
					slog.String("server", c.Server), slog.Int64("requestedShard", *c.Shard), slog.Int("replicas", replicas))
			}
		case replicas <= 1:
		case alg == AlgorithmRoundRobin:
			shard = ranks[c.ID] % replicas
		case alg == AlgorithmConsistentHashing:
			shard = ring[c.ID]
		default:
			shard = LegacyShard(c, replicas)
		}
		out[c.Server] = shard
	}
	return out
}
