// Package sharding distributes destination clusters over application-controller replicas
// (ADR-0008) and decides which replica processes an Application or Operation.
//
// The package has four parts:
//
//   - Pure assignment algorithms with Argo CD v3.5.5 semantics: Legacy (FNV-32a of the cluster
//     ID modulo the replica count), RoundRobin (rank in the clusters sorted by ID) and
//     ConsistentHashing (consistent hashing with bounded loads weighted by the number of
//     Applications per cluster). The cluster ID is the Cluster object UID; the synthetic
//     in-cluster cluster has the empty ID, as upstream's local cluster without a Secret.
//   - Assigner: a concurrency-safe cluster → shard table kept up to date from the cluster
//     registry and the Application informer, with explicit Cluster.spec.shard pins, the
//     skip-reconcile annotation, replica-count changes and change notifications.
//   - LeaseManager: shard ownership through coordination.k8s.io/v1 Leases named
//     patrol-controller-shard-<n>. A replica acquires the Lease of its preferred shard (its
//     StatefulSet ordinal or PATROL_CONTROLLER_SHARD), takes over expired Leases of other shards
//     as a standby, hands borrowed shards back to their preferred replica, and reports gains
//     and losses through callbacks. Lease renewal is the heartbeat of upstream's dynamic
//     cluster distribution.
//   - Ownership: the facade used by the controller (OwnsApplication, controller-runtime
//     predicates for Applications and Operations) plus helpers for patrol-server routing
//     (StatusShardResolver, LeaseHolders) and for writing Cluster.status.assignedShard
//     (AssignedShardWriter).
//
// Kubernetes stays the source of truth: the Assigner and the ownership view are rebuilt from
// informers and Leases after a restart.
package sharding
