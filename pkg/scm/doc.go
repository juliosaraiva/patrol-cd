// Package scm defines Patrol CD's versioned source-code-management (SCM) provider
// contract (ADR-0010).
//
// A provider implements [Provider] (identity and declared capabilities) plus any of the
// capability interfaces ([RepositoryLister], [BranchLister], [RevisionResolver],
// [PathChecker], [PullRequestLister], [WebhookVerifier], [CommitStatusReporter],
// [CheckRunReporter], [CredentialSource], [InstallationLister]). Callers obtain a
// capability with [As], which fails with an [*UnsupportedError] (errors.Is(err,
// ErrUnsupported)) when the provider does not declare the capability, even if the Go type
// happens to implement the method set. Capabilities are declared, never implied.
//
// Errors returned by providers are normalized to [*Error] with an [ErrorKind] so that
// callers can branch on NotFound / RateLimited / Unauthorized without knowing the
// provider. Shared HTTP plumbing (rate limiting, bounded retries honoring Retry-After and
// rate-limit headers, response size caps, pagination with page caps, log redaction) and
// webhook helpers (raw-body HMAC verification, delivery de-duplication) live here so that
// every provider applies the same rules.
//
// The package intentionally has no dependency on Patrol's CRD types: callers adapt
// SCMProvider / ApplicationSet generator specs into [Config], [RepoQuery] and [PRQuery].
package scm
