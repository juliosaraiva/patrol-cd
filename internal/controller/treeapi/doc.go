// Package treeapi is the controller's internal HTTP API (ADR-0004, ADR-0006): patrol-server
// reads Application resource trees, managed resources (diff data), server-side diffs and live
// objects from the controller replica that owns the Application's destination cluster,
// because Patrol has no Redis to share them through.
//
// Routes (JSON; errors are RFC 9457 problem details with a stable code; every route except
// the health check requires the shared internal bearer token):
//
//	GET  /internal/v1/tree?namespace=&name=                         compare.ApplicationTree (ETag / If-None-Match)
//	GET  /internal/v1/managed-resources?namespace=&name=&group=&kind=&resourceNamespace=&resourceName=
//	POST /internal/v1/server-side-diff                              ServerSideDiffRequest → ServerSideDiffResponse
//	GET  /internal/v1/resource?namespace=&name=&group=&version=&kind=&resourceNamespace=&resourceName=
//	GET  /healthz
//
// The controller implements StateProvider; this package owns request validation, Secret
// masking at the serving boundary (internal/diff.HideSecretData), the membership checks of
// the upstream API (a server-side diff or live fetch only for resources of the Application),
// size caps and the wire format. A replica that does not own an Application answers 421 with
// the owning shard, and the Client re-routes once.
//
// The Client routes each call to the replica serving the Application: the shard of the
// destination cluster (ShardResolver), then the current Lease holder of that shard
// (HolderResolver) or the StatefulSet ordinal, reached through the headless Service as
// <pod>.<service>.<namespace>.svc.
package treeapi
