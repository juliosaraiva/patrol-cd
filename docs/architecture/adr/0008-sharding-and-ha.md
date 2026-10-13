# ADR-0008: Controller sharding and HA

Status: accepted — 2026-10-12

## Decision
- The controller is a StatefulSet. Shards are owned through `coordination.k8s.io/Lease`
  objects `patrol-controller-shard-<n>`; a replica acquires its ordinal's lease and may
  acquire orphaned leases after expiry (faster failover than upstream's fixed ordinal model).
- Cluster → shard assignment: explicit `Cluster.spec.shard`, else algorithm `legacy`
  (FNV-32a of cluster UID mod N), `round-robin` (sorted by cluster ID) or
  `consistent-hashing` (bounded loads), matching upstream semantics.
- Applications are processed by the shard owning their destination cluster. Operations are
  executed only by the lease holder; a lease change mid-operation causes the new owner to
  resume the `Running` operation (see sync state machine).
- Other controllers use controller-runtime leader election; patrol-server and repo-server are
  stateless and horizontally scalable.
