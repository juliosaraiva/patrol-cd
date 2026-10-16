# Component: `internal/commitserver`

Status: implemented (service, HTTP API, typed client, `Run` entry point; the
`patrolcd commit-server` subcommand is not wired yet). Decision records: ADR-0006
(JSON over HTTP, shared bearer token, optional mTLS), ADR-0007 (tool isolation through
`internal/git`/`internal/sandbox`). Parity area: `docs/parity/areas/01-application.md`
(APP-119..APP-122, APP-126..APP-129, APP-135); status overlay:
`docs/parity/status/commitserver.yaml`. Caller: [hydrator](hydrator.md).

The commit-server writes the manifests rendered by the source hydrator to git (upstream
`commitserver/commit`). It holds no Kubernetes credentials; the write credentials of the
destination repository arrive with each request and live only in memory and in the
per-command temporary files `internal/git` creates.

## Public API

| Symbol | Purpose |
|--------|---------|
| `CommitHydratedManifestsRequest` | `Repo` (`apiclient.Repository` with write credentials), `SyncBranch`, `TargetBranch`, `DrySHA`, `CommitMessage`, `Author{Name, Email}`, `ReadmeTemplate`, `Paths[]{Path, Manifests []json.RawMessage, Commands}`, `DryCommitMetadata`, `DryRepoURL`. `LogValue` redacts credentials. |
| `CommitHydratedManifestsResponse{HydratedSHA}` | Target branch head holding the manifests (new commit or unchanged head). |
| `Client` | `CommitHydratedManifests(ctx, req)`; implemented by `*Service` (in-process) and `*HTTPClient`. |
| `NewService(ServiceOptions)` | `WorkDir`, `CredentialsDir`, `GitPath`, `GitTimeout`, `SSHKnownHosts`, `TLSCertificates`, `MaxAttempts` (default 5), `MaxConcurrentRequests`, `AllowLocalRepositories` (tests only), `Metrics`, `Logger`. |
| `NewHandler(Client, HandlerOptions)` | `POST /api/v1/commit-hydrated-manifests` (bearer token, constant-time compare, JSON only, body cap 100 MiB default), `GET /healthz`. Errors are RFC 9457 problem details with stable codes (`invalid_argument` 400, `unauthenticated` 401, `permission_denied` 403, `aborted` 409, `deadline_exceeded` 504, `unavailable` 503, `internal` 500). |
| `NewClient(address, token, opts...)` | Typed HTTP client (`host:port` or URL, `WithHTTPClient` for TLS); errors are `*Error` matching `ErrInvalidRequest`/`ErrConcurrentUpdate` with `errors.Is`. |
| `Run(ctx, RunOptions)` | Process entry point: API listener (default `0.0.0.0:8086`, optional TLS/mTLS), metrics listener (default `0.0.0.0:8087`, `/metrics`), graceful shutdown (in-flight commits finish within `ShutdownTimeout`). |
| `NewMetrics(reg)` | `patrol_commitserver_commit_pending_request_total`, `_git_request_total`, `_git_request_duration_seconds`, `_commit_request_duration_seconds`, `_commit_request_total` (upstream names with the `patrol_` prefix). |
| `RenderCommitMessage`, `NewCommitMetadata`, `ParseTemplate` | Template helpers shared with the hydrator (Sprig generic functions without `env`, `expandenv`, `getHostByName`; output capped at 1 MiB). |
| Constants | `ManifestFileName`, `MetadataFileName`, `ReadmeFileName`, `GitAttributesFileName`, `NoteNamespace` (`hydrator.metadata`), `DefaultAuthorName`/`Email`. |

## Behavior and invariants

1. Validation before any git work (every error wraps `ErrInvalidRequest`): repo URL, git
   type, branches, 40-hex dry SHA, 1..10000 paths, bounded messages/templates/commands,
   author identity without control characters or angle brackets, parseable README
   template. Paths must be relative, not the root, not escape the repository, not enter
   `.git` (any case), not collide with the root files, appear once, and not pass through a
   file another path of the request owns (`x` with `x/manifest.yaml/...`,
   `x/README.md/...` or `x/hydrator.metadata/...`; compared ignoring case).
2. Per-repository serialization inside the process (context-aware keyed mutex) and an
   optional global concurrency limit.
3. Fresh clone per attempt in a temporary directory under `WorkDir`, removed afterwards.
4. Sync branch checked out, or created as an orphan with an empty `Initial commit for
   <branch>` when missing; target branch checked out, or created from the sync branch
   (pushed with the first commit).
5. If the target head's note (`refs/notes/hydrator.metadata`) records the same dry SHA and
   manifests digest, the head is returned unchanged (idempotent re-hydration).
6. For each path whose `manifest.yaml` differs from the committed file: the owned files
   (`manifest.yaml`, `hydrator.metadata`, `README.md`) are replaced (a regular file or a
   symbolic link itself, never its target); other files are never touched; an owned file
   name (or root `hydrator.metadata`/`.gitattributes`) occupied by a directory fails the
   request and is left as is, since it may hold another application's output or user files
   (upstream's `O_TRUNC` open fails there too); symlinked or non-directory path components
   are refused (repository content is untrusted). When anything changed, root `hydrator.metadata` and `.gitattributes` are
   rewritten. `manifest.yaml` is a YAML stream of the objects in render order with sorted
   keys and 2-space indentation (deterministic).
7. Commit with the request's author (defaults `Patrol CD <patrol-cd@example.com>`) and
   message, push without force. No empty commits: unchanged manifests return the current
   head.
8. Note `{"drySha": ..., "manifestsDigest": ...}` added to the returned commit (also when
   nothing changed) and pushed without force.
9. A push or note push rejected because another writer moved the branch or notes ref
   (`fetch first`, `non-fast-forward`, `cannot lock ref`, ...) is retried on a fresh clone
   with jittered exponential backoff (100 ms .. 2 s, `MaxAttempts`), re-applying the
   request on the new tip; exhaustion returns `ErrConcurrentUpdate` (HTTP 409).

## Security notes

- Credentials never reach the command line or persistent files (`internal/git` askpass and
  per-command 0600 files under `CredentialsDir`); `stripCredentials` removes passwords from
  URLs written into git content; logs use `SanitizeURL` and redacting `LogValue`s.
- Bearer token compared in constant time on SHA-256 digests; request bodies capped;
  `AllowLocalRepositories` must stay false in production (file:// and local paths).
- Templates cannot read the environment or resolve hosts and their output is bounded.

## Deviations

- The note carries `manifestsDigest` in addition to upstream's `drySha`, so a request with
  the same dry SHA but different manifests (changed spec, added path) is written instead
  of being skipped.
- `DryRepoURL` lets the hydrator record the dry repository in `hydrator.metadata`/README
  (upstream records the destination repository).
- Retry on concurrent updates re-applies the whole request on a fresh clone (upstream
  retries the note push only).
- A malformed note on the target head is logged and overwritten (upstream fails the request
  with `json unmarshal failed ...`).
- Transport is JSON over HTTP (ADR-0006) instead of gRPC; there is no separate version
  service.

## Verification

Commands and results (2026-10-16, darwin/arm64, Go 1.27.2, git CLI from PATH):

```bash
go vet ./internal/commitserver/                                   # ok
golangci-lint run ./internal/commitserver/...                     # 0 issues
go test -race -count=1 -v ./internal/commitserver/                # ok, 27 top-level tests PASS
```

Review follow-up (2026-10-16): `replaceFile` no longer deletes a directory found at an owned
file name, and `prepare` rejects paths overlapping another path's owned files. Regression
tests `TestCommitRejectsPathsOverlappingOwnedFiles`, `TestCommitRefusesDirectoryAtOwnedFile`,
`TestReplaceFile`, `TestWriteTreeKeepsEveryPathProperty` (rapid: every accepted request
writes each path's own manifests) and three new `TestCommitRejectsInvalidRequests` cases
fail on the previous code (checked with `go test -overlay`) and pass now:

```bash
go build ./...                                                    # ok
go vet ./internal/commitserver/                                   # ok
golangci-lint run ./internal/commitserver/...                     # 0 issues
go test -race -count=1 ./internal/commitserver/                   # ok
```

Tests run the service against local bare repositories with the git CLI (orphan branch
bootstrap, target branch creation, idempotence, path cleaning safety, symlink refusal,
file contents, author defaults, rejected-push retry, concurrent writers, per-repository
serialization, metrics) and the HTTP API end to end (`TestHTTPCommitRoundTrip`,
`TestHTTPHandler`, `TestRun`).
