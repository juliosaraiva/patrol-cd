# ADR-0004: No Redis

Status: accepted — 2026-10-12

## Context
Argo CD uses Redis to share rendered manifests, resource trees and managed-resource diffs
between controller, server and repo-server.

## Decision
No Redis. Repo-server keeps a bounded in-memory + tmpfs LRU render cache keyed by immutable
inputs. The controller owns the live object graph and serves resource trees and diffs to
patrol-server over an internal API routed to the shard owning the destination cluster.

## Consequences
- One fewer stateful dependency and no cache poisoning surface shared across components.
- Repo-server replicas warm their caches independently (more renders after scale-out).
- patrol-server availability for tree/diff views depends on the owning controller shard.
- Revisit if benchmarks (docs/verification/benchmarks.md) show render cost dominates.
