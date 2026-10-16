# Component: `internal/controller/sharding`

Status: implemented (library; the controller binary does not wire it yet).
Decision records: [ADR-0008](../adr/0008-sharding-and-ha.md) (Lease-based shard ownership,
algorithms), [ADR-0004](../adr/0004-no-redis.md), [ADR-0003](../adr/0003-resource-model.md)
(Cluster CRD). Related: [clusters](clusters.md) (registry, StatusWriter),
[treeapi](treeapi.md) (routing to the owning replica).

Clusters are the unit of sharding, as upstream: each controller replica processes the
Applications (and Operations) whose destination cluster maps to a shard it owns, and keeps
live-state caches only for those clusters. Patrol replaces upstream's "trust the StatefulSet
ordinal" model (and the dynamic-distribution ConfigMap) with one `coordination.k8s.io/v1`
Lease per shard, which gives failover without waiting for a pod to be rescheduled.

## Responsibilities

1. Configuration from the environment with upstream parsing rules (`ConfigFromEnv`).
2. Cluster → shard algorithms with Argo CD v3.5.5 semantics: `legacy`, `round-robin`,
   `consistent-hashing` (bounded loads), explicit `Cluster.spec.shard` pins.
3. A concurrency-safe assignment table fed by the cluster registry and the Application
   informer, recomputed on relevant changes and on replica-count changes (`Assigner`).
4. Shard ownership through Leases: preferred shard, standby takeover of expired Leases,
   handoff back to the preferred replica, release on shutdown and scale-down (`LeaseManager`).
5. Dynamic cluster distribution: replica count from the controller workload (`WorkloadReplicas`).
6. Ownership decisions and controller-runtime predicates for Applications and Operations
   (`Ownership`).
7. `Cluster.status.assignedShard` maintenance (`AssignedShardWriter`).
8. Helpers for patrol-server routing: `StatusShardResolver` (app → shard from Cluster status)
   and `LeaseHolders` (shard → pod holding its Lease).
9. Metrics.

Out of scope: wiring into the controller manager (informer filters, cache scoping, re-queue
on ownership changes), `patrol admin cluster shards/stats` (OPS-133/134), the Helm chart and
RBAC manifests.

## Public API

| Symbol | Purpose |
|--------|---------|
| `Config`, `ConfigFromEnv(lookup, hostname, logger)` | `PATROL_CONTROLLER_REPLICAS` (0..1024, default 0 = one replica), `PATROL_CONTROLLER_SHARD` (default -1), `PATROL_CONTROLLER_SHARDING_ALGORITHM`, `PATROL_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION` (`true`/`false` any case), `PATROL_CONTROLLER_HEARTBEAT_TIME` (10..60 s, default 10), `PATROL_APPLICATION_CONTROLLER_NAME` (default `patrol-controller`). Out-of-range or unparsable values keep the default with upstream's warning text. |
| `(Config).PreferredShard(replicas, logger)`, `InferShard(hostname)`, `(Config).LeaseDuration()` | Preferred shard: 0 with ≤ 1 replica; explicit shard, else hostname ordinal; no ordinal → 0 with upstream's warning (static) or -1 (dynamic: claim any free shard); preferred ≥ replicas → -1 (standby). Lease duration = 3 × heartbeat. |
| `LeaseName(n)`, `ShardFromLeaseName` | `patrol-controller-shard-<n>`. |
| `Algorithm`, `ParseAlgorithm` | `legacy` (default), `round-robin`, `consistent-hashing`; unknown → legacy with "distribution type <x> is not supported, defaulting to legacy". |
| `ClusterInfo`, `ClusterInfoFrom(*v1alpha1.Cluster)` | Sharding view: ID = object UID ("" for the synthetic in-cluster cluster), normalized server, pin, skip-reconcile. |
| `LegacyShard`, `RoundRobinShard`, `ConsistentHashingShard`, `ConsistentHashingShards` | Upstream distribution functions (return -1 when unassignable). |
| `Distribute(alg, clusters, appsPerServer, replicas, logger)` | Runtime table (upstream `updateDistribution`). |
| `NewAssigner(AssignerOptions{Algorithm, Replicas, Logger, Registerer})` | `Init`, `SetCluster`, `DeleteCluster`, `HandleClusterEvent(clusters.Event)`, `SetApplication`, `DeleteApplication`, `SetReplicas`, `Lookup`, `ShardForServer` (unknown → 0, skip → not managed), `Distribution`, `AppDistribution`, `Clusters`, `Subscribe(func([]Change))`. |
| `NewLeaseManager(LeaseManagerOptions{Client, Namespace, Identity, Config, Replicas, LeaseDuration, CallTimeout, MaxShards, DisableStandby, OnAcquired, OnLost, OnReplicasChanged, Clock, Logger, Registerer})` | `Run(ctx)`, `OwnsShard`, `OwnedShards`, `Replicas`, `PreferredShard`, `Healthz`. `CallTimeout` bounds the API calls of one heartbeat (default and maximum: half the heartbeat). `NewLeaseClient(kube, ns)` adapts a clientset; its `List` returns every Lease of the namespace (shard Leases are recognized by name, not by label). |
| `ReplicaSource`, `StaticReplicas`, `WorkloadReplicas{Client, Namespace, Name, Kind}` | Static count or the controller StatefulSet/Deployment `spec.replicas` (dynamic distribution). |
| `NewOwnership(assigner, owner, destinations, logger)` | `ShardForApplication`, `OwnsApplication`, `OwnsCluster(server)`, `OwnsClusterObject(*Cluster)` (signature of `core.Deps.OwnsCluster`), `OwnsOperation`, `ApplicationPredicate()`, `OperationPredicate(ApplicationGetter)`; `ClientApplicationGetter(client.Reader)`. |
| `NewAssignedShardWriter(assigner, owner, ClusterStatusUpdater, logger)` | `Sync`, `Trigger`, `Run(ctx, resync)`; `*clusters.StatusWriter` is the updater. |
| `StatusShardResolver{Clusters}`, `NewLeaseHolders(client, ttl, clock)` | patrol-server side: app → `status.assignedShard` / `spec.shard` / 0; shard → current holder (5 s cache, `Invalidate`). |
| `AnnotationShardHandoff` | `cd.patrol.example/shard-handoff-requested-by` on shard Leases. |

### Algorithms

- **legacy**: `fnv32a(ID) % replicas`; nil cluster or empty ID → 0; replicas 0 → -1. Golden
  values equal the baseline's tests.
- **round-robin**: rank of the cluster in the list sorted by ID (ties by server) modulo
  replicas; the synthetic in-cluster cluster (empty ID) is ranked first; a cluster not in the
  list → -1.
- **consistent-hashing**: ring with members `"0".."N-1"`, 1000 virtual nodes each hashed from
  `"<member><i>"` with BLAKE2b-512 (first 8 bytes, little endian; a later member takes over a
  colliding virtual node such as `"1"+"11"` = `"11"+"1"`). Clusters are visited in ID order; a
  cluster takes the first virtual node strictly after the hash of its ID whose member is under
  `ceil(max(floor((T+1)/N),1) × 1.25)`; that member's load then grows by the Applications whose
  `spec.destination.server` is the cluster's server. Ring nodes are cached per member count.
- **pins**: `spec.shard` lower than replicas wins for every algorithm. At runtime a higher pin
  uses shard 0 with upstream's warning (the baseline's `updateDistribution`).
- Fewer than two replicas: every cluster maps to shard 0.

### Lease protocol

Heartbeats run at a fixed rate (default every 10 s); the API calls of one heartbeat share a
deadline of `CallTimeout` (default half the heartbeat), so a hung call cannot hold renewals
past the renewal deadline. Every heartbeat each replica:

1. reads the replica count (static, or the controller workload with dynamic distribution) and
   reports changes (`OnReplicasChanged` → `Assigner.SetReplicas`). Until a count has been read
   once, a failed read ends the heartbeat: the replica claims no shard, `OnReplicasChanged` does
   not run and `Healthz` fails (no count is guessed; a guessed count would map other shards'
   clusters to a shard this replica can own). Later failures keep the last count;
2. reports shards whose renewal deadline (lease duration − heartbeat) passed as lost
   (`OnLost`), so a late heartbeat never bridges a lapse silently;
3. lists the Leases of the namespace, recognizes shard Leases by name (labels are restored on
   every write but never relied on, so a pre-created or relabeled Lease stays visible) and
   records, per shard, the local time at which the Lease record (or its absence) was last seen
   to change;
4. renews held Leases (optimistic concurrency) only while their deadline has not passed,
   counting the deadline from the send time; releases shards that no longer exist or that a
   preferred replica asked for (borrowed shards only); drops shards whose holder changed;
5. acquires its preferred shard when the Lease is absent, free, its own or unchanged for a
   lease duration; otherwise writes the handoff annotation. A `Create` rejected with
   AlreadyExists (a Lease that `List` did not return) reads the Lease and takes it when it is
   free or its own;
6. as a standby, takes at most one other shard whose Lease stayed free/absent/unchanged for a
   lease duration plus one heartbeat per shard it already holds (replicas with fewer shards win;
   `MaxShards` caps the count), or its own Lease dropped after a missed deadline unless another
   replica asked for it.

`Healthz` reflects the last heartbeat: it fails on a replica-count read error, on any Lease API
error other than a lost race (Conflict, AlreadyExists), and when the preferred shard could not
be acquired for a full lease duration although its Lease was absent, free, its own or expired.

On shutdown held Leases are released (callbacks first) so a survivor can take over without
waiting for expiry.

## Invariants

- At most one replica owns a shard at a time: acquisition and renewal are compare-and-swap
  updates; a replica stops counting a shard as owned at its renewal deadline, strictly before
  any other replica can see the Lease expire (others wait a full lease duration measured from
  their own observation of the last change; no cross-pod clock comparison).
- `OnLost` runs before a voluntary release is written, so processing stops before another
  replica can start.
- Every period in which `OwnsShard(n)` is true starts with `OnAcquired(n)` and ends with
  `OnLost(n)` (late by at most one heartbeat): a renewal is sent only before the deadline, and a
  lapsed shard is reported lost before it is re-acquired, so the controller re-queues work that
  the predicates dropped during the lapse.
- No shard is claimed before the replica count is known.
- The assignment table equals `Distribute` over its current inputs after every update
  (tested against fresh computations), and change batches are delivered in recomputation order.
- Skip-reconcile clusters are owned by no replica; unresolvable destinations and unknown
  servers belong to shard 0 (one writer reports the error).
- Kubernetes stays the source of truth: the table is rebuilt from informers, ownership from
  Leases; nothing is persisted elsewhere.

## Failure modes

| Situation | Result |
|-----------|--------|
| Lease API unreachable | renewals fail; shards are dropped at the renewal deadline (`OnLost`); `Healthz` = `error while updating the heartbeat for the shard leases: …` |
| Lease call rejected (Forbidden, timeout, other non-race error) | `Healthz` fails at once (`… renew shard <n>: …`, `acquire shard <n>`, `request handoff of shard <n>`, `release shard <n>`); `heartbeat_errors_total{op,reason="error"}` |
| Lost update race (Conflict, AlreadyExists) | retried at the next heartbeat from fresh data; `heartbeat_errors_total{op,reason="conflict"}`; readiness unaffected unless the preferred shard keeps failing for a lease duration (`cannot acquire the preferred shard <n> for <d>: …`) |
| Shard Lease without the shard labels | found by name, acquired or renewed normally, labels restored |
| Hung API call | cut at `CallTimeout`; the heartbeat fails readiness and the deadline check still runs |
| Heartbeat late past the renewal deadline (slow call or callback) | `OnLost`, then `OnAcquired` when the Lease is re-acquired; never a silent lapse |
| Holder overwritten by someone else | shard dropped at the next heartbeat; the preferred replica asks for it back |
| Workload missing / unreadable / replicas nil / ≤ 0 (dynamic) | `(dynamic cluster distribution) failed to get app controller statefulset: …`, `… replica count`, `application controller statefulset replicas is not set or is less than 0, replicas: <n>`; `Healthz` fails. Before a count was ever read: no shard is claimed and `OnReplicasChanged` does not run (prefix `cannot read the controller replica count; no shard is claimed until it is known`), retried every heartbeat. Afterwards: the last known count is kept |
| Requester of a handoff disappears after asking | the borrower releases; the shard is free and taken by a standby after one lease duration |
| Two replicas prefer the same shard | first holder keeps it; the other is a standby (no handoff between preferred owners) |
| Status write fails | joined error from `Sync`; `Run` retries with backoff (1 s doubling to 1 min) |

## Metrics

`patrol_controller_cluster_shard{server,name}` (assigned shard), `patrol_controller_shard_clusters{shard}`,
`patrol_controller_shard_replicas`, `patrol_controller_shard_owned{shard}` (1/0),
`patrol_controller_shard_lease_transitions_total{shard,event}` (acquired, lost, released,
handoff_requested), `patrol_controller_shard_heartbeat_errors_total{op,reason}` (op: replicas,
list, renew, acquire, get, handoff, release; reason: `conflict` for a lost race, `error`
otherwise).

## Adapting (controller wiring)

1. `cfg := sharding.ConfigFromEnv(os.LookupEnv, hostname, log)`; algorithm may also come from
   `controller.sharding.algorithm` in patrol-cmd-params-cm.
2. `assigner := NewAssigner(...)`; `assigner.Init(registry.All(), apps)`; subscribe it to
   `registry.Subscribe(assigner.HandleClusterEvent)`; feed Application informer events to
   `SetApplication`/`DeleteApplication`.
3. Replica source: `StaticReplicas(cfg.Replicas)` or `WorkloadReplicas{Name: cfg.ControllerName}`
   when `cfg.DynamicDistribution`.
4. `lm := NewLeaseManager({Client: NewLeaseClient(kube, ns), Identity: hostname, Config: cfg,
   OnReplicasChanged: func(n){ assigner.SetReplicas(n) }, OnAcquired/OnLost: re-queue owned
   Applications and Operations, start/stop cluster caches, writer.Trigger()})`; run `lm.Run` and
   expose `lm.Healthz` in the readiness probe.
5. `own := NewOwnership(assigner, lm, registry, log)`: `ApplicationPredicate()` /
   `OperationPredicate(ClientApplicationGetter(mgr.GetClient()))` on the watches,
   `core.Deps.OwnsCluster = own.OwnsClusterObject`, and re-check `OwnsApplication` at the start
   of each reconcile; subscribe to `assigner.Subscribe` to re-queue Applications of moved
   clusters.
6. `NewAssignedShardWriter(assigner, lm, clusters.NewStatusWriter(dyn), log).Run(ctx, 0)`.
7. RBAC: `coordination.k8s.io/leases` get/list/watch/create/update in the control-plane
   namespace; `apps/statefulsets` (or deployments) get with dynamic distribution; Cluster
   `status` update.

## Differences from upstream

- Lease ownership and handoff instead of the fixed ordinal (static) or the
  `argocd-app-controller-shard-cm` ConfigMap heartbeat (dynamic); a replica can own several
  shards while a peer is down.
- A preferred shard ≥ replicas makes the replica a standby (upstream: shard 0 when greater,
  kept when equal).
- Unresolvable destinations are processed by shard 0 only (upstream: every shard).
- A replica count that cannot be read before one is known: upstream's `GetClusterSharding`
  returns an error and the controller exits at startup; Patrol keeps running without claiming a
  shard and fails readiness until the read succeeds.
- Consistent hashing counts Applications by normalized destination server, keyed by
  namespace/name, and recomputes when a destination changes.
- Dynamic distribution reads a StatefulSet by default (Deployment supported).

## Requested API addition

`AnnotationShardHandoff` (`cd.patrol.example/shard-handoff-requested-by`) belongs in
`api/v1alpha1/keys.go` by convention; it is declared in this package until the API owner adds
it.

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0), after the review
fixes (first-read replica failure, label-independent Lease discovery and Healthz coverage,
lapse callbacks and bounded heartbeat calls):

```bash
go build ./...                                                  # exit 0
go vet ./internal/controller/sharding/... ./internal/controller/treeapi/...   # exit 0
golangci-lint run ./internal/controller/sharding/... ./internal/controller/treeapi/...  # 0 issues
go test -race -count=1 ./internal/controller/sharding/... ./internal/controller/treeapi/...  # ok (5 envtest tests skip)
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 -v ./internal/controller/sharding/... ./internal/controller/treeapi/...
  # ok; 62 top-level tests and 67 subtests pass, none skipped (both packages)
go test -race -count=20 -run TestLeaseManager ./internal/controller/sharding/   # ok
KUBEBUILDER_ASSETS=... go test -race -count=3 -run Envtest ./internal/controller/sharding/  # ok
go run ./hack/parity                                            # exit 0, no warnings
```

Tests: golden legacy and round-robin values from the baseline tests, FNV-32a property test,
BLAKE2b test vector, consistent-hashing distribution (±10 % over 133 clusters), scale-down
stability, bounded-load property, no movement when an unloaded cluster is added, a
differential test against an independent mirror of the baseline ring, assigner table/notifications/
metrics/concurrency, Lease manager scenarios on an in-memory API with conflict checking and a
fake clock (acquire/renew, partition → loss at 20 s and takeover after expiry, handoff, scale
up/down, standby race with one winner, shutdown release, overwritten holder), failure modes
(`lease_failure_test.go`: a restarted replica whose first replica-count read is forbidden claims
nothing and reports no count while its peer keeps and takes over shards; unlabeled Leases found
through a clientset `List`; Forbidden/Conflict on acquire and renew → readiness and
`reason`-labeled error counts; a Lease hidden from `List` taken via AlreadyExists → Get, or
failing readiness after a lease duration when held elsewhere; a late heartbeat reports
`-0,-1,+1,+0`; a hung Update cut by `CallTimeout`; a borrower whose handoff release loses a race
does not take the shard back), and envtest: failover and handoff over real Leases, dynamic
distribution following a StatefulSet's `spec.replicas` (2 → 3 → 2 → 0), no shard claimed until
the StatefulSet exists, an unlabeled and later relabeled shard Lease kept without a loss,
`status.assignedShard` written through the real CRD's status subresource. Each failure-mode test
was run against the pre-fix code (via `go test -overlay`, scratchpad only) and failed there.
