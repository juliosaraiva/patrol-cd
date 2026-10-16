# Component: `internal/controller/treeapi`

Status: implemented (server handler and client; the controller does not serve it and
patrol-server does not call it yet). Decision records: [ADR-0004](../adr/0004-no-redis.md) (the
controller serves trees and diffs instead of Redis), [ADR-0006](../adr/0006-internal-apis.md)
(JSON over HTTP, shared token), [ADR-0008](../adr/0008-sharding-and-ha.md) (shard routing).
Related: [compare](compare.md) (tree and managed-resource types), [diff](diff.md) (masking),
[sharding](sharding.md).

## Responsibilities

- Serve the controller-owned Application state to patrol-server: resource tree, managed
  resources (diff data), server-side diffs and live objects of tree nodes.
- Enforce the upstream API's data-protection rules at the serving boundary: Secret data and
  sensitive annotations masked, server-side diff and live fetch limited to resources of the
  Application, masking decided from parsed objects (not caller metadata).
- Authenticate with the shared internal token, cap request and response sizes, validate input.
- Route client calls to the replica that owns the Application's shard and re-route once on a
  421 answer.

Out of scope: RBAC, project checks, the public `/api/v1` routes and the watch stream
(internal/apiserver), computing trees and diffs (internal/compare via the controller's
`StateProvider`), TLS listener setup (callers wrap the handler).

## API

All routes except `GET /healthz` require `Authorization: Bearer <patrol-internal-token>`
(SHA-256 + constant-time compare). Errors are `application/problem+json`
`{type,title,status,detail,code[,shard]}`.

| Method | Path | Query / body | Response |
|--------|------|--------------|----------|
| GET | `/internal/v1/tree` | `namespace`, `name` | `compare.ApplicationTree`; `ETag`, `If-None-Match` → 304 |
| GET | `/internal/v1/managed-resources` | `namespace`, `name`, `group`, `kind`, `resourceNamespace`, `resourceName` | `{items: [ResourceDiff]}`: hooks excluded, exact match on non-empty filters, `"null"` for missing sides |
| POST | `/internal/v1/server-side-diff` | `{namespace, name, liveResources: [ResourceDiff], targetManifests: [string]}` (index-aligned) | `{items: [ResourceDiff], modified}`; `targetState` = predicted, `liveState` = normalized live |
| GET | `/internal/v1/resource` | `namespace`, `name`, `group`, `version`, `kind` (required), `resourceNamespace`, `resourceName` (required) | `{manifest}` (JSON, Secret masked) |
| GET | `/healthz` | — | `ok` |

`ResourceDiff` uses upstream's JSON names (`group, kind, namespace, name, targetState,
liveState, diff (unused), hook, normalizedLiveState, predictedLiveState, resourceVersion,
modified`).

Status codes: 400 `invalid_argument`, 401 `unauthenticated`, 403 `permission_denied`, 404
`not_found`, 413 `resource_exhausted`, 421 `misdirected` (with `shard` member and
`Patrol-Shard` header), 500 `internal`, 503 `unavailable`, 504 `deadline_exceeded`.

### Behavior (upstream parity)

- **Managed resources** (DIFF-038): masking of core Secrets on both object sides
  (`HideSecretData`, equal values get equal masks) and on the normalized/predicted pair
  (`MaskSecretResult`); `resource.sensitive.mask.annotations` from the provider.
- **Server-side diff** (DIFF-039): each non-empty live state must match its declared
  name/namespace/group/kind (`name mismatch: expected X, got Y for live resource g/k/n`, …) and
  be a managed resource of the Application (`<group>/<kind> <name> not found as part of
  application <app>`, 403); empty/`"null"` live = create, empty/`"null"` target = prune; target
  parse errors `error unmarshaling target manifest <i>: …`; metadata of each item from the live
  entry (or the target when there is none); Secret results masked when the parsed target or
  live object is a core Secret.
- **Resource** (API-022): the resource must be a node with UID of the Application's tree
  (`<kind> <group> <name> not found as part of application <app>`, 400); `version` overrides
  the node's version; Secret data masked.

## Public Go API

| Symbol | Purpose |
|--------|---------|
| `StateProvider` | `Tree`, `ManagedResources`, `ServerSideDiff`, `LiveResource`, `SensitiveAnnotations` — implemented by the controller. Errors wrap `ErrNotFound`, `ErrUnavailable`, `ErrInvalidArgument`, `ErrPermissionDenied` or are `*NotOwnedError{Shard}`. |
| `NewHandler(provider, HandlerOptions{Token, MaxRequestBytes (32 MiB), MaxDiffResources (5000), Logger, Registerer})` | The `http.Handler`. |
| `NewClient(router, token, WithHTTPClient, WithMaxResponseBytes (200 MiB))` | `Tree`, `TreeIfChanged(etag)`, `ManagedResources`, `ServerSideDiff`, `Resource`; errors are `*Error` matching the sentinels with `errors.Is`. |
| `Router`, `StaticRouter{URL}`, `ShardRouter{Shards, Holders, StatefulSet, Service, Namespace, Port (8090), Scheme}` | Routing; `ShardRouter` resolves `<holder or patrol-controller-<n>>.<service>.<namespace>.svc:<port>`. |
| `ShardResolver`, `HolderResolver` | Implemented by `sharding.StatusShardResolver` and `*sharding.LeaseHolders`. |

## Invariants

- No route returns plaintext Secret data or sensitive annotation values, including for
  spoofed request metadata.
- A server-side diff never runs for input that failed validation or membership checks.
- Response bodies are bounded on the client; request bodies and diff sizes on the server.
- The client retries at most once (421 with a shard hint, or a transport error after
  invalidating cached holder information).

## Metrics

`patrol_controller_treeapi_requests_total{route,code}`,
`patrol_controller_treeapi_request_duration_seconds{route}`.

## Adapting

- Controller: implement `StateProvider` over the last `compare.Result` per Application
  (`Tree`, `Managed` or `MaskSecrets(...)`, `KubeServerSideDiff` with the Application's diff
  settings, a GET through the destination cluster's config), return `*NotOwnedError{Shard}` when
  `Ownership.OwnsApplication` is false and `ErrUnavailable` before the first comparison; serve
  `NewHandler` on `:8090` (TLS optional) from every replica.
- patrol-server: `NewClient(&ShardRouter{Shards: sharding.StatusShardResolver{Clusters:
  registry}, Holders: sharding.NewLeaseHolders(sharding.NewLeaseClient(kube, ns), 0, nil),
  Namespace: ns, Service: <headless service>}, token)`; on `ErrUnavailable` for a tree, request
  a refresh and retry (upstream `getCachedAppState`).
- Deployment: the controller StatefulSet needs a headless Service whose name matches
  `ShardRouter.Service`; patrol-server needs `get` on Leases in the control-plane namespace.

## Differences from upstream

- Upstream serves these calls from patrol-server's view of Redis (and talks to clusters from
  the API server); Patrol asks the owning controller replica.
- Misaligned live/target arrays are rejected with `invalid_argument` (upstream fails inside the
  diff with an internal error).
- Managed-resource diffs of Secrets are masked as a pair instead of being recomputed on masked
  objects; the masked pair shows the same differences.

## Verification

See [sharding.md](sharding.md#verification) (both packages run together). Tests: HTTP round
trips through the real handler and client (`httptest`) with a fake provider — authentication
(missing, wrong, wrong scheme), tree with ETag/304, error mapping (404/400/403/421/500/503/504),
managed resources (hook exclusion, filters, Secret and annotation masking, `"null"` sides),
server-side diff (masking, spoofed-kind masking, metadata, every validation error with its
message, limits, content type, provider result count), resource (version override, masking,
tree membership), misdirect re-routing between two servers, dead-endpoint retry, response cap,
`ShardRouter` endpoints with and without Lease holders.
