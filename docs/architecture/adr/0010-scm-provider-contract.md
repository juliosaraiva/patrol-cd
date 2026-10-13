# ADR-0010: SCM provider contract

Status: accepted — 2026-10-12

## Decision
`pkg/scm` defines a versioned contract (`ContractVersion = "v1"`). A provider implements
`Provider` (identity + capabilities) and any of the capability interfaces:
`RepositoryLister`, `BranchLister`, `RevisionResolver`, `PathChecker`, `PullRequestLister`,
`WebhookVerifier` (raw body + headers → normalized `Event`), `CommitStatusReporter`,
`CheckRunReporter`, `CredentialSource` (token minting/refresh). Callers request capabilities
through `scm.As[T]`, which returns `ErrUnsupported` with the provider and capability name,
so unsupported operations fail loudly.

Shared rules: every provider paginates fully with a page cap, honors rate-limit headers with
bounded backoff, maps errors to `scm.Error{Kind: NotFound|Unauthorized|RateLimited|Unavailable|Invalid}`,
and never logs tokens. Generic Git transport (`internal/git`) and Helm/OCI sources are
outside this contract.

The conformance suite `pkg/scm/conformance` runs against every provider using recorded or
fake HTTP servers; `pkg/scm/fake` is the reference implementation.
GitHub (`pkg/scm/github`) is the first-class provider (App auth, checks, statuses, webhooks).
Other providers implement the capabilities the baseline ApplicationSet generators and webhooks
use; their capability set is declared, not implied.
