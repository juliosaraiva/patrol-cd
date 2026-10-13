# ADR-0006: Internal service APIs

Status: accepted — 2026-10-12

## Decision
repo-server, cmp-server, commit-server and the controller tree API use JSON over HTTP with
typed Go clients in the same module. Authentication: a shared bearer token mounted from
Secret `patrol-internal-token` (constant-time compare), plus optional mTLS
(`--tls-cert/--tls-key/--tls-client-ca`). CMP traffic uses a unix socket and a tar stream
of the source tree. Request bodies are size-capped; repo-server never logs credential fields
(types implement `slog.LogValuer` redaction).
