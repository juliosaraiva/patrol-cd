# Component: `pkg/scm` (SCM provider contract) and `pkg/scm/github`

Status: implemented (phase 1, library only). Owner task: T1.16 (scm-core part: contract,
registry, fake, conformance, GitHub). Decision record: ADR-0010.

`pkg/scm` is the versioned contract (`ContractVersion = "v1"`) between Patrol components that
talk to SCM platforms (ApplicationSet SCM Provider and Pull Request generators, webhook
ingestion, notifications, repository credential resolution, hydrator commit identity) and
the provider implementations. `pkg/scm/github` is the first-class GitHub provider
(github.com, GitHub Enterprise Server, GitHub Enterprise Cloud with data residency). The
packages do not import `api/v1alpha1`; callers adapt CRD types (see below).

## Responsibilities

- Define provider identity (`Info`) and a closed set of **declared** capabilities;
  `scm.As[T]` serves a capability only if it is declared *and* implemented, otherwise it
  returns `*UnsupportedError` (`errors.Is(err, ErrUnsupported)`).
- Normalize errors into `*scm.Error{Kind, Provider, Op, StatusCode, RetryAfter, Err}` with
  sentinels for `errors.Is` (`ErrNotFound`, `ErrRateLimited`, …).
- Provide shared HTTP plumbing so every provider applies the same rules: TLS/CA/proxy base
  transport, client-side pacing (`x/time/rate`), bounded retries honoring `Retry-After` and
  rate-limit headers, response body size cap, cursor-generic pagination with a page cap and
  loop detection, credential host pinning, and redacted request logging.
- Provide webhook helpers: bounded body reading, constant-time HMAC-SHA256 verification over
  raw bytes, normalized `Event` model, changed-file normalization, and the replay/dedup
  `DeliveryCache` (bounded LRU + TTL).
- Provide a registry of provider factories, an in-memory reference provider with fault
  injection (`pkg/scm/fake`) and a conformance suite every provider runs
  (`pkg/scm/conformance`).
- GitHub: anonymous, personal access token and GitHub App authentication; repositories,
  branches, revisions, path checks, pull requests, webhooks, commit statuses, check runs,
  credentials for Git over HTTPS, installations.

Out of scope: HTTP routing and status codes of `/api/webhook` (`internal/webhook`),
ApplicationSet filter semantics and template parameters (`internal/appset`), matching
webhook events to Applications, notification templates (`internal/notifications`), Git
transport (`internal/git`), Secret resolution and the SCMProvider CRD (`api/v1alpha1`).

## Public API

### Contract (`pkg/scm`)

| Symbol | Purpose |
|--------|---------|
| `ContractVersion`, `Capability`, `AllCapabilities()` | Contract version `v1`; capabilities `repositories`, `branches`, `revisions`, `paths`, `pullRequests`, `webhooks`, `commitStatuses`, `checkRuns`, `credentials`, `installations`. |
| `Provider{Info() Info}`, `Info{Type, Name, APIURL, ContractVersion, Capabilities}` | Identity and declared capabilities. |
| `As[T](Provider) (T, error)`, `CapabilityFor[T]()` | Capability lookup; fails loudly with `*UnsupportedError{Provider, Name, Capability}`. |
| `RepositoryLister.ListRepositories(ctx, RepoQuery) ([]Repository, error)` | `RepoQuery{Organization, Topics, Archived (Default/Include/Exclude), CloneProtocol ("", ssh, https), AllBranches, Match}`. Without `AllBranches`: one entry per repository for its default branch with the head SHA; empty repositories omitted. With it: one entry per branch. `Match` is called sequentially before branch expansion (pushdown of repository-level filters). |
| `BranchLister.ListBranches(ctx, RepoRef, all)` | `all=false` returns only the default branch (empty slice for empty repositories). |
| `RevisionResolver.ResolveRevision(ctx, RepoRef, ref)` | Branch/tag/SHA → full SHA; unknown → `KindNotFound`. |
| `PathChecker.PathExists(ctx, RepoRef, revision, path)` | Missing path → `(false, nil)`; `..` → `KindInvalid`. |
| `PullRequestLister.ListPullRequests(ctx, RepoRef, PRQuery)` | `PRQuery{Labels (all required, exact), State (open default, closed, merged, all), TargetBranch}`; missing repository → `KindNotFound`. |
| `WebhookVerifier{MatchesWebhook(h), VerifyAndParse(h, rawBody, secret)}` | Raw-body signature verification and normalization to `*Event`. |
| `CommitStatusReporter.SetCommitStatus(ctx, RepoRef, sha, CommitStatus)` | `CommitStatus{State pending/success/failure/error, Context, Description, TargetURL}`. |
| `CheckRunReporter.UpsertCheckRun(ctx, RepoRef, sha, CheckRun) (id, error)` | Idempotent by `(Name, ExternalID)`. |
| `CredentialSource.Token(ctx) (Token, error)` | `Token{Value, ExpiresAt, Username}`; `String`/`GoString`/`LogValue` redact the value. |
| `InstallationLister{ListInstallations, FindInstallation(owner)}` | App installations. |
| Data types | `RepoRef{Owner, Name}` (+`Validate`, `FullName`, `ParseFullName`), `Repository`, `Branch`, `PullRequest`, `Installation`, `InstallationRef`, `Event`, `EventRepo`. |
| Errors | `Error`, `ErrorKind` (`NotFound`, `Unauthorized`, `Forbidden`, `RateLimited`, `Unavailable`, `Invalid`, `Conflict`), `KindOf`, `IsNotFound`, `IsRateLimited`, `IsRetryable`, `RetryAfter`, `NewError`; extra sentinels `ErrPageLimit`, `ErrPaginationLoop`, `ErrResponseTooLarge`, `ErrUnknownProviderType`, `ErrSignatureMissing`, `ErrSignatureMismatch`, `ErrPayloadTooLarge`, `ErrMalformedPayload`, `ErrUnsupportedEvent`. |
| Registry | `Factory func(ctx, Config) (Provider, error)`, `NewRegistry`, `(*Registry).Register/New/Types`, package-level `DefaultRegistry`, `Register`, `New`. `New` rejects providers reporting another contract version. Factories must not do network I/O. |
| `Config` | `Type, Name, APIURL, Insecure, CAData, ProxyURL, NoProxy, Token TokenFunc, GitHubApp *GitHubAppConfig{AppID, InstallationID, PrivateKey, EnterpriseBaseURL, Owner}, HTTPClient, RateLimit{RPS, Burst}, Retry RetryPolicy{MaxAttempts, BaseDelay, MaxDelay, MaxWait}, MaxPages, MaxResponseBytes, Timeout, UserAgent, Logger, Clock`; `WithDefaults()`; `LogValue` and `GitHubAppConfig.String/LogValue` redact secrets. |
| Transport helpers | `BaseTransport(cfg)`, `ProxyFunc(proxyURL, noProxy)`, `LimitResponseBody(rt, max)`, `NewLimiter(RateLimit)`, `Chain(inner, ChainOptions)` (logging → retry → pacing → inner), `DefaultRetryClassifier`, `ParseRetryAfter`, `RateLimitWait`, `HostGuard(host, authed, plain)`, `BearerTransport(next, TokenFunc, onUnauthorized)`, `RedactURL`, `RedactURLString`, `RedactHeaders`. |
| Pagination | `PageFunc[T, C]`, `Pages(ctx, maxPages, fetch) iter.Seq2[[]T, error]`, `CollectPages` (no partial results on error). |
| Webhook helpers | `ReadWebhookBody(r, max)`, `SignHMACSHA256`, `VerifyHMACSHA256(header, prefix, body, secret)`, `ParseRevision`, `NormalizeChangedFiles`, `DeliveryKey(provider, id)`, `NewDeliveryCache(capacity, ttl, WithDeliveryClock)` with `Seen`, `Forget`, `Len`, `WebhookSignatureError`. |
| Utilities | `ShortSHA(sha, n)` (for `short_sha`/`head_short_sha`), `TruncateRunes`. |

### Reference provider (`pkg/scm/fake`)

`fake.New(Options{Name, Capabilities, PageSize, MaxPages, Clock})`; seeding with `AddRepo`,
`SetBranch`, `AddPullRequest`, `AddInstallation`, `SetToken`; fault injection with
`InjectError(op, err, times)`, `InjectRateLimit(retryAfter, times)`, `SetLatency`,
`SetFaultHook`, `ClearFaults`; observation with `Calls(op)`, `CommitStatuses`, `CheckRuns`;
webhooks with `SignedDelivery(Delivery, id, secret)` (headers `X-Fake-Event`,
`X-Fake-Delivery`, `X-Fake-Signature: sha256=…`); `Factory()` / `Register(r, p)`.

### Conformance suite (`pkg/scm/conformance`)

`Run(t, func(t) (scm.Provider, Fixture), Options{Skip, Timeout})`. The provider test supplies
a `Fixture` (`Data()`, `WebhookDelivery`, `SignatureHeaders`, `InjectRateLimit`,
`CommitStatuses`, `CheckRuns`). A fresh provider is built per check. Checks: info validity;
undeclared capabilities return `ErrUnsupported`; repository pagination completeness, archived
filter, default-branch SHA and empty-repository omission, all-branches expansion, `Match`
pushdown, clone protocol and invalid protocol, not-found mapping; branch listing (all/default,
not found); revision resolution (not found); path existence and traversal rejection; PR
pagination, label AND-filter, not found; webhook valid signature, tampered body, wrong
secret, missing signature, empty-secret policy, delivery dedup with `DeliveryCache`, PR event;
commit status write and pre-validation; check run idempotent upsert; credential token;
installations; rate-limit mapping with `RetryAfter`.

### GitHub (`pkg/scm/github`)

| Symbol | Purpose |
|--------|---------|
| `Type = "github"`, `New` (factory), `Register(*scm.Registry)`, `NewProvider(ctx, Config) (*Provider, error)` | Construction (no network I/O). |
| `APIURLs(apiURL) (base, upload string, error)` | `""`/`https://api.github.com` → github.com; `https://host[/prefix]` or `…/api/v3` → GHES (`/api/v3/`, uploads `/api/uploads/`); `https://api.<tenant>.ghe.com` → GHE.com (uploads host `uploads.<tenant>.ghe.com`). User info, query and fragment are rejected. |
| `(*Provider)` capability methods | All contract capabilities. Declared capabilities depend on auth: anonymous = read capabilities + webhooks; token = + `commitStatuses`, `credentials`; App = + `checkRuns`, `installations`. |
| `(*Provider).Client() *github.Client` | go-github v88 client with the same auth/pacing/retries/caps, for GitHub-only operations (deployments, PR comments for notifications). |
| `(*Provider).BotIdentity(ctx)` | `<slug>[bot]` and `<id>+<slug>[bot]@users.noreply.github.com` (hydrator author). |
| `WebhookVerifier{}` | Stateless verifier usable without credentials (webhook ingestion). |
| `GitUsername = "x-access-token"`, header constants | Git over HTTPS user name; `X-GitHub-Event`, `X-GitHub-Delivery`, `X-Hub-Signature-256`. |

GitHub behavior:

- Repositories: `GET /orgs/{org}/repos` (100/page, all pages); on 404 falls back to
  `GET /users/{user}/repos` (Patrol extension; upstream supports organizations only).
  `Labels` = topics; `RepositoryID` = int64 ID; clone URL SSH by default, HTTPS with
  `https`, other values fail with `unknown clone protocol for GitHub <p>`. Archived
  repositories are included unless `ArchivedExclude` (upstream `excludeArchivedRepos`).
  Default-branch head via `GET /repos/{o}/{r}/branches/{b}` (404 = empty repository → no
  entry); all branches via `GET …/branches`. Branch lookups run with bounded parallelism (4)
  and keep API order.
- Revisions: `GET /repos/{o}/{r}/commits/{ref}` with `application/vnd.github.v3.sha`;
  `refs/heads/`/`refs/tags/` prefixes are stripped; 404/422 → `KindNotFound`.
- Paths: `HEAD /repos/{o}/{r}/contents/{path}?ref=` (GET fallback on 405/501); each path
  segment is escaped; 404 → false.
- Pull requests: `GET /repos/{o}/{r}/pulls?state=&base=`; labels filtered client-side
  (exact, all required); `merged` = `state=closed` + `merged_at`.
- Commit statuses: `POST /repos/{o}/{r}/statuses/{sha}`; the SHA must be full hex;
  description truncated to 140 characters; 422 (including the 1000-statuses limit) →
  `KindInvalid`.
- Check runs: `GET /repos/{o}/{r}/commits/{sha}/check-runs?check_name=&filter=all&app_id=<app>`
  then `PATCH …/check-runs/{id}` when `external_id` matches, otherwise `POST …/check-runs`.
  `completed_at` defaults to now for conclusions; output title/summary/text truncated to
  65535 characters.
- Webhooks: see "Webhook semantics" below.
- App auth: JWT (RS256, via `ghinstallation.AppsTransport`, PKCS#1 or PKCS#8 keys) for
  `/app/*` and installation discovery; installation tokens from
  `POST /app/installations/{id}/access_tokens`, cached process-wide per
  sha256(API URL, App ID, private key) and installation, refreshed when less than 5 minutes
  remain, minted once for concurrent callers (singleflight), and invalidated when the API
  answers 401. When `InstallationID` is 0 the installation is discovered for
  `GitHubApp.Owner` (case-insensitive) with `GET /orgs/{owner}/installation`, then
  `GET /users/{owner}/installation`, cached for 60 minutes; a miss returns `KindNotFound`
  with `installation not found for org: <owner>: ensure the GitHub App (ID: <id>) is
  installed …`. A missing owner fails with `installation ID is required for github`.
- Errors: `RateLimitError` (403/429 with `X-RateLimit-Remaining: 0`) → `RateLimited` with
  `RetryAfter` until `X-RateLimit-Reset`; secondary limits (`AbuseRateLimitError`, or a 403
  whose message mentions "rate limit") → `RateLimited` with `Retry-After` or 60s; 400/422 →
  `Invalid`, 401 → `Unauthorized`, 403 → `Forbidden`, 404/410 → `NotFound`, 409 →
  `Conflict`, 429 → `RateLimited`, 5xx/202/network/malformed JSON → `Unavailable`, `..` in a
  path and oversize responses → `Invalid`. Context errors keep `errors.Is(err, ctx.Err())`.

### Webhook semantics (all providers)

1. The caller reads the body with `ReadWebhookBody` (size cap; GitHub max 25 MiB).
2. `VerifyAndParse` verifies the signature **over the exact raw bytes before inspecting the
   event type**; GitHub requires `X-Hub-Signature-256: sha256=<hex>` (HMAC-SHA256,
   constant-time compare). The legacy SHA-1 `X-Hub-Signature` alone is rejected as missing,
   matching upstream's go-playground/webhooks v6 behavior.
3. Empty secret policy: with no configured secret no verification is possible; the event is
   returned with `Verified=false` and the caller decides (upstream accepts unsigned events;
   Patrol webhook ingestion can require signatures).
4. GitHub events: `push` (ref, revision, before/after, created/deleted/forced,
   `TouchedDefaultBranch` only for `refs/heads/<default>`, changed files = per commit
   added+modified+removed, de-duplicated, capped at 10000 with `ChangedFilesTruncated`),
   `pull_request` (all actions; normalized PR incl. labels, draft, merged), `installation`
   and `installation_repositories` (type `installation`; repositories added/removed),
   `repository`, `ping`. Other events → `ErrUnsupportedEvent` (after verification);
   `package` is left to the GHCR registry parser (`MatchesWebhook` = false), as are Gogs/Gitea
   deliveries that also carry GitHub headers. JSON and form-encoded (`payload=`) bodies are
   accepted.
5. Dedup: `DeliveryCache.Seen(DeliveryKey(ev.Provider, ev.DeliveryID))` after verification;
   `Forget` when processing fails so a redelivery is handled.

### Adapting CRD types (callers)

- **SCMProvider / generator auth**: `tokenRef` → `Config.Token` (closure reading the Secret
  from an informer cache); `appSecretName` (repo-creds keys `githubAppID`,
  `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppEnterpriseBaseUrl`) →
  `Config.GitHubApp`, with `Owner` = generator `organization` / `owner` (or the owner parsed
  from the repository URL for repository credentials). App takes precedence over token.
  `api` → `Config.APIURL`; `insecure`/`caRef` → `Insecure`/`CAData`;
  `--scm-proxy-url`/`--scm-no-proxy` → `ProxyURL`/`NoProxy`.
- **SCM Provider generator (GitHub)**: `organization` → `RepoQuery.Organization`;
  `allBranches` → `AllBranches`; `excludeArchivedRepos: true` → `ArchivedExclude`;
  `cloneProtocol` → `CloneProtocol`; repository-level filters (`repositoryMatch`,
  `labelMatch` over `Labels`) → `Match`; branch-level filters (`branchMatch`,
  `pathsExist`/`pathsDoNotExist` via `PathChecker.PathExists(ref, entry.Branch, path)`) are
  applied by the generator to the returned entries. `short_sha`/`short_sha_7` via `ShortSHA`.
- **Pull Request generator (GitHub)**: `owner`/`repo` → `RepoRef`; `labels` →
  `PRQuery.Labels`; `continueOnRepoNotFoundError` → treat `scm.IsNotFound(err)` as zero
  results; `head_short_sha` via `ShortSHA`.
- **Webhook ingestion**: `webhook.github.secret` → `secret`; refresh matching uses
  `Event.Repository.HTMLURL` (git/app matching), `Event.Repository.APIURL` + owner/name (PR
  generators), `Revision`, `TouchedDefaultBranch`, `ChangedFiles`.
- **Notifications**: commit statuses through `CommitStatusReporter`; check runs through
  `CheckRunReporter` (App only); deployments and PR comments through `Provider.Client()`.
- **Repository credentials**: `CredentialSource.Token` gives `x-access-token` + installation
  token for Git over HTTPS; callers must not persist it.

## Invariants

- Capabilities are declared, never implied: `As` checks `Info().Capabilities`, and GitHub
  methods for undeclared capabilities also return `ErrUnsupported`.
- Listings are complete or fail: pagination errors (page cap, cursor loop, any page error)
  return no partial result, so generators never delete Applications because of a truncated
  listing.
- Every response body is size-capped; every listing is page-capped; retries are bounded in
  attempts and in honored wait (`MaxWait`, default 60s; longer waits surface as
  `RateLimited` with `RetryAfter`); only idempotent methods with replayable bodies are
  retried.
- Credentials are attached only to requests whose host equals the configured API host
  (`HostGuard`), so redirects to other hosts never receive tokens.
- Untrusted names (owner, repository, path, revision) are validated before URL
  interpolation (`RepoRef.Validate`, `..` rejection); go-github additionally refuses `..`
  segments.
- Webhook signatures are verified on raw bytes before any parsing that depends on the event
  type; `Verified` is true only for a matching signature with a configured secret.
- Factories never perform network I/O; App discovery and token minting are lazy.
- Provider instances are safe for concurrent use.

## Failure modes

| Failure | Behavior |
|---------|----------|
| API down / 5xx / network error | Idempotent requests retried with jittered exponential backoff (default 4 attempts), then `KindUnavailable`. |
| Primary rate limit | Waited out inside the call if the reset is within `MaxWait`; otherwise `KindRateLimited` with `RetryAfter`; go-github also stops sending requests until the known reset. |
| Secondary rate limit | Waited out when `Retry-After` ≤ `MaxWait`; else `KindRateLimited` (`RetryAfter` from header or 60s). |
| Token revoked / expired early | 401 → `KindUnauthorized`; the cached installation token is invalidated, the next call mints a new one. |
| App not installed for owner | `KindNotFound` with the upstream-style message; nothing cached. |
| Invalid App key | `NewProvider` fails with `KindInvalid` (message never contains key material). |
| Oversized response | `ErrResponseTooLarge` (`KindInvalid`). |
| Organization with more than `MaxPages × 100` repositories | `ErrPageLimit` (`KindInvalid`); raise `MaxPages`. |
| Repository missing or invisible | `KindNotFound` (`continueOnRepoNotFoundError` applies); `PathExists` returns false. |
| Bad webhook signature / missing signature | `KindUnauthorized` + `ErrSignatureMismatch` / `ErrSignatureMissing`. |
| Unknown webhook event | `ErrUnsupportedEvent` (`KindInvalid`) after verification. |
| Process restart | All caches (tokens, discovery, deliveries) are memory-only and rebuilt on demand; delivery dedup is per replica (effects are idempotent). |

## Security notes

- Tokens, private keys and webhook secrets are never logged: `Token`, `GitHubAppConfig` and
  `Config` implement redacting `String`/`GoString`/`LogValue`; request logs (debug level)
  contain method, redacted URL (user info and token-like query parameters removed), status,
  duration and remaining rate limit, never headers or bodies. Errors from key parsing do not
  include key material.
- The installation token cache key is a SHA-256 over API URL, App ID and private key, so
  different keys or GitHub hosts never share tokens. Tokens live only in memory.
- No implicit credential sources: the upstream undocumented `GITHUB_TOKEN` environment
  fallback is intentionally not implemented.
- TLS minimum version 1.2; `Insecure` is an explicit opt-in; extra CAs are appended to the
  system pool.
- Webhook bodies, PR metadata, branch names and file paths are untrusted: bounded reads,
  bounded changed-file lists, strict JSON decoding into minimal structs, and name validation
  before URL construction.
- Callers must apply the ApplicationSet API URL allow-list (ASET-101) to `Config.APIURL`
  before building providers, to avoid SSRF through user-controlled `api` fields.

## Parity feature areas served

- `03-repositories-clusters-webhooks`: REPO-015 (GitHub App auth, token for Git HTTPS),
  REPO-016 (installation discovery, case-insensitive, 60-minute cache, upstream error text),
  REPO-017 (enterprise base URL, trailing slash tolerated, token request at
  `<base>/app/installations/<id>/access_tokens`), REPO-117 (GitHub webhook verification and
  push normalization: revision parsing, touched HEAD, changed files), REPO-114/115/116 (the
  verification and size-cap building blocks; HTTP status codes are `internal/webhook`'s job),
  REPO-123 (`package` events left to the GHCR parser).
- `06-applicationset`: ASET-027/028 (GitHub SCM provider data: pagination, topics as labels,
  repository ID, empty repositories, archived handling, clone protocols), ASET-029 (App auth
  for SCM and PR generators), ASET-038/039 (path checks), ASET-042 (clone protocol), ASET-043
  (`allBranches`), ASET-044/045 (TLS, CA, proxy), ASET-046/047 (GitHub PR data), ASET-056
  (labels AND filter), ASET-057 (repository-not-found mapping), ASET-105/106 (normalized
  push and pull_request events with API URL, owner, repo and action).
- Notifications GitHub service (commit statuses, check runs; deployments/comments through
  `Client()`).

## Known gaps and differences

- Only the GitHub provider exists; GitLab, Gitea, Bitbucket Cloud/Server, Azure DevOps and
  AWS CodeCommit adapters (rest of T1.16) must implement the contract and pass
  `conformance.Run`.
- GitHub API metrics (ASET-129) are not built in; callers can wrap `Config.HTTPClient`'s
  transport (it sees every attempt, including rate-limit headers).
- Installation discovery uses the per-owner endpoints (organization, then user) instead of
  listing every installation; the repository endpoint (`/repos/{o}/{r}/installation`) is not
  used.
- Installation tokens are refreshed 5 minutes before expiry (upstream's ghinstallation: 1
  minute); upstream's `ARGOCD_GITHUB_APP_CREDS_EXPIRATION_DURATION` transport TTL has no
  equivalent because tokens are cached by their own expiry.
- `ListRepositories` falls back to user repositories for non-organization owners (upstream:
  organizations only). For user accounts `/users/{user}/repos` lists only repositories visible
  to the token (no `/user/repos` support for the authenticated user's private repositories).
- Form-encoded GitHub webhook deliveries are accepted (upstream rejects them because the
  go-playground parser requires JSON). `TouchedDefaultBranch` is false for tag pushes even if
  a tag has the default branch's name (upstream compares the short name only).
- Check run upserts are not atomic across concurrent callers for the same (SHA, name,
  external ID); callers serialize per Application.
- Webhook delivery dedup is per replica (by design, see architecture overview).

## Verification

Commands and results are recorded in the final task report and in
`docs/verification/log.md` by the integrating agent:

```bash
go build ./...
go vet ./pkg/scm/...
go test -race ./pkg/scm/...
```
