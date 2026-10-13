# ADR-0005: Public API contract

Status: accepted — 2026-10-12

## Decision
- JSON over HTTPS under `/api/v1`, paths aligned with Argo CD's REST gateway where the
  semantics match (eases migration of scripts).
- The route table in `internal/apiserver` is the single source for routing and for the
  generated OpenAPI 3.1 document `api/openapi/openapi.json` (checked in; CI fails if stale).
  CRD bodies reuse the structural schemas generated for the CRDs.
- Errors: RFC 9457 problem details with stable `type` codes.
- Streaming: Server-Sent Events for watches, logs and operation progress; WebSocket for the
  terminal. Every stream re-checks authorization at open and on resource change.
- Clients: TypeScript types generated with `openapi-typescript`; Go client `pkg/apiclient`
  shares request/response types with the server.
- Pagination: `limit` + opaque `continue` token on list endpoints; `resourceVersion` for
  optimistic concurrency on updates (HTTP 409 on conflict).

## Alternatives considered
gRPC + grpc-gateway (Argo's approach) requires protobuf mirrors of CRD types and a second
gateway layer. Connect-RPC still needs protobuf CRD mirrors. Rejected for duplication.
