# Component: additional SCM providers (`pkg/scm/{gitlab,gitea,gogs,bitbucketcloud,bitbucketserver,azuredevops,awscodecommit}`)

Status: implemented (phase 1, library only). Owner task: T1.16 (scm-providers part).
Decision record: ADR-0010. Contract: `pkg/scm` (see `scm.md`).

> **Verification level: fake / contract-tested only.** Every behavior below was exercised
> against in-process `httptest` fakes written from the providers' public API documentation
> and upstream Argo CD v3.5.5 source (CodeCommit: a fake AWS endpoint driven by the real
> aws-sdk-go-v2 clients). Nothing was verified against live GitLab, Gitea, Gogs, Bitbucket,
> Azure DevOps or AWS services. Parity evidence from these tests is `simulated`.

The packages implement, against the unchanged `pkg/scm` contract v1, exactly the
capabilities the Argo CD v3.5.5 baseline uses each SCM for: the SCM Provider generator
(repositories, branches, path checks), the Pull Request generator, the API server and
ApplicationSet webhooks, plus revision resolution where it costs one or two API calls. No
package imports `api/v1alpha1`; callers adapt CRD types (see "Adapting CRD types").

## Responsibilities

- Translate the contract queries (`RepoQuery`, `PRQuery`, `RepoRef`) into each provider's
  REST API with upstream's field mapping, defaults, filters and error texts.
- Paginate every listing completely with the shared page cap (`scm.CollectPages`): GitLab
  `X-Next-Page` (Link fallback), Gitea `page`/`limit` with `Link rel="next"` (full-page
  fallback), Bitbucket Server `isLastPage`/`nextPageStart`, Bitbucket Cloud absolute `next`
  links (refused unless on the API host), Azure DevOps `$top`/`$skip` (pull requests) and
  `x-ms-continuationtoken` (refs), AWS `NextToken`/`PaginationToken`.
- Normalize errors to `*scm.Error` kinds (`NotFound`, `Unauthorized`, `Forbidden`,
  `RateLimited` with `RetryAfter`, `Invalid`, `Conflict`, `Unavailable`) so that
  `continueOnRepoNotFoundError` and retry decisions are provider-agnostic.
- Authenticate and normalize webhook deliveries (`WebhookVerifier`) into `scm.Event`.
- Use the shared transport rules of `pkg/scm` for every HTTP request: TLS/CA/proxy base
  transport, per-attempt timeout, response size cap, credentials pinned to the API host,
  pacing, bounded retries of idempotent requests, request observer, redacted debug logs.

## Capability matrix (declared capabilities)

| Provider (`Type`) | repositories | branches | revisions | paths | pullRequests | webhooks | Auth (Config/Options) | Webhook authentication |
|---|---|---|---|---|---|---|---|---|
| GitLab (`gitlab`) | yes | yes | yes | yes | yes | yes | `Token` → `PRIVATE-TOKEN`; anonymous | `X-Gitlab-Token` == secret (constant time; sender only) |
| Gitea (`gitea`) | yes | yes | yes | yes | yes | yes | `Token` → `Authorization: token`; anonymous | HMAC-SHA256 hex in `X-Gitea-Signature` (fallbacks `X-Gogs-Signature`, `X-Hub-Signature-256`) |
| Gogs (`gogs`) | — | — | — | — | — | yes (push) | none (webhook only) | HMAC-SHA256 hex in `X-Gogs-Signature` |
| Bitbucket Cloud (`bitbucketcloud`) | yes | yes | yes | yes | yes | yes | `Options.Username` + `Token` → basic; `Token` alone (or user `x-token-auth`) → bearer; anonymous | `X-Hook-UUID` == secret (UUID; sender only) |
| Bitbucket Server / DC (`bitbucketserver`) | yes | yes | yes | yes | yes | yes | `Options.Username` + `Token` → basic; `Token` alone → bearer; anonymous | `X-Hub-Signature: sha256=<hex HMAC>` |
| Azure DevOps (`azuredevops`) | yes | yes | yes | yes | yes | yes | `Token` → PAT basic (`:<pat>`); anonymous | HTTP basic auth == `user:password` secret (sender only) |
| AWS CodeCommit (`awscodecommit`) | yes | yes | — | yes | — | — | AWS default chain or `Options.AWSConfig`; optional STS role | — (no webhooks upstream) |

Not declared anywhere (not used by the baseline for these SCMs): `commitStatuses`,
`checkRuns`, `credentials`, `installations`. CodeCommit has no Pull Request generator and no
webhook upstream; CodeCommit revision resolution is not declared (no tag API).

Each package exports `Type`, `New` (the `scm.Factory`, default options), `Register(*scm.Registry)`,
`NewProvider(ctx, scm.Config[, Options])` and a credential-free `WebhookVerifier{}` usable by
webhook ingestion without building a provider.

## Public API

| Package | Exported symbols |
|---------|------------------|
| `gitlab` | `Type="gitlab"`, `DefaultAPIURL`, `Options{IncludeSubgroups, IncludeSharedProjects *bool}`, `NewProvider(ctx, cfg, Options)`, `APIBaseURL(url)`, `PRStateLocked`, `WebhookVerifier`, headers `HeaderEvent`, `HeaderToken`, `HeaderEventUUID`, event names `EventPushHook`, `EventTagPushHook`, `EventMergeRequestHook`, `EventSystemHook`. |
| `gitea` | `Type="gitea"`, `NewProvider(ctx, cfg)`, `APIBaseURL(url)`, `WebhookVerifier`, `HeaderEvent`, `HeaderEventType`, `HeaderDelivery`, `HeaderSignature`. |
| `gogs` | `Type="gogs"`, `New`, `Register`, `Provider` (webhooks only), `WebhookVerifier`, `HeaderEvent`, `HeaderDelivery`, `HeaderSignature`. |
| `bitbucketcloud` | `Type="bitbucketcloud"`, `DefaultAPIURL`, `TokenAuthUsername="x-token-auth"`, `Options{Username}`, `NewProvider(ctx, cfg, Options)`, `APIBaseURL(url)`, `(*Provider).ChangedFiles(ctx, ref, before, after)`, `(*Provider).TouchesMainBranch(ctx, ref, revision)`, `APIBaseURLFromRepositoryURL(selfHref, fullName)`, `ErrLabelsUnsupported`, `WebhookVerifier`, `HeaderEventKey`, `HeaderHookUUID`, `HeaderRequestUUID`. |
| `bitbucketserver` | `Type="bitbucketserver"`, `Options{Username}`, `NewProvider(ctx, cfg, Options)`, `APIBaseURL(url)`, `ErrLabelsUnsupported`, `WebhookVerifier`, `HeaderEventKey`, `HeaderRequestID`, `HeaderSignature`. |
| `azuredevops` | `Type="azuredevops"`, `DefaultAPIURL`, `Options{Organization}`, `NewProvider(ctx, cfg, Options)`, `(*Provider).Organization()`, `OrganizationURL(api, org)`, `WebhookVerifier`, `SplitSecret`, `JoinSecret`, `HeaderActivityID`, event types `EventGitPush`, `EventPullRequestCreated/Updated/Merged`. |
| `awscodecommit` | `Type="awscodecommit"`, `Options{Region, Role, TagFilters []TagFilter{Key, Value}, AWSConfig *aws.Config}`, `NewProvider(ctx, cfg, Options)`, `CloneProtocolHTTPSFIPS="https-fips"`, `FIPSCloneURL(url)`. |

## Provider behavior

### GitLab (ASET-030, ASET-048, REPO-118)

- API base: `""` → `https://gitlab.com/api/v4/`; `/api/v4` appended unless present.
- Repositories: `GET /groups/:id/projects?per_page=100&include_subgroups=&with_shared=[&topic=][&archived=]`.
  `Organization` = group numeric ID or full path. `Options.IncludeSubgroups` (default false),
  `Options.IncludeSharedProjects` (nil = true, upstream default). `RepoQuery.Topics` →
  `topic=` (comma-joined; matching by GitLab). Archived: `ArchivedInclude` → `archived=true`,
  `ArchivedExclude` → `archived=false` (+ client-side drop), `ArchivedDefault` → omitted —
  exactly upstream's wire behavior (GitLab treats `archived=true` as "only archived" and an
  omitted parameter as "all"; the fake models that). Entry fields: `Organization` =
  namespace full path, `Name` = project path, `RepositoryID` = int64 project ID, `Labels` =
  topics (legacy `tag_list` fallback), clone URL SSH by default, HTTPS with `https`, other
  protocols fail with `unknown clone protocol for Gitlab <p>`. Default-branch head via
  `GET /projects/:id/repository/branches/:branch` (404 → no entry); all branches paginated
  (404 → no entries, as upstream).
- `RepoRef`: owner = namespace path (`group/sub`), name = project path; `RepoRef{Name:"<id>"}`
  addresses a project by numeric ID. Paths are URL-encoded as one segment (`%2F`).
- Paths: Files API `HEAD` (GET fallback on 405/501; upstream downloads the file with GET),
  then Tree API `?path=&per_page=1` for directories; 404 or an empty tree → false.
- Revisions: `GET /projects/:id/repository/commits/:ref`.
- Merge requests: `GET /projects/:id/merge_requests?per_page=100&state=&labels=&target_branch=`;
  `State` `""`/open → `opened`, closed → `closed`, merged → `merged`, `PRStateLocked` →
  `locked`, all → `all`. Labels are sent as GitLab's server-side `labels` filter only (no
  client-side re-check, as upstream), so GitLab's semantics apply: every listed label is
  required, and the predefined keywords `None` (no labels) and `Any` (at least one label)
  work case-insensitively. This deliberately widens `PRQuery.Labels`' "all, exact match"
  wording (contract note 9).
  `Number` = IID, `HeadSHA` = `sha`, `Author` = username, `State` = raw GitLab state.
- Webhooks: `Push Hook`, `Tag Push Hook` → push; `Merge Request Hook` → pull_request
  (actions open→opened, reopen→reopened, close/merge→closed, update→synchronize, others
  verbatim; `Repository.ID` = target project ID, which upstream matches against numeric
  generator projects; `Repository.HTMLURL` = project web URL); `System Hook` routed by
  `object_kind` (push, tag_push, merge_request; others unsupported). Delivery ID:
  `X-Gitlab-Event-UUID`. `ChangedFilesTruncated` is set when `total_commits_count` exceeds
  the commits carried (GitLab sends at most 20).

### Gitea (ASET-031, ASET-049)

- API base: required `api` + `/api/v1`. No network I/O at construction (upstream's SDK
  queries the server version).
- Repositories: `GET /orgs/{org}/repos` (falls back to `/users/{user}/repos`, a Patrol
  extension), every page (upstream reads the first page only). Per repository the issue
  labels are read (`GET /repos/{o}/{r}/labels`, every page) and become `Labels` (upstream).
  `Organization` = configured owner; `https` → repository **HTML URL** (`HTTPSURL` is set to
  the same value), SSH default; unknown protocol → upstream text
  `unknown clone protocol for GitHub <p>`. `ArchivedExclude` drops archived repositories
  (upstream `excludeArchivedRepos`); `Topics` filter on repository topics. Repositories flagged
  `empty` are omitted; a non-empty repository whose default branch is missing fails with
  upstream's `got 404 while getting default branch "<b>" for repo "<r>" - check your repo config`.
- Paths: `GET /repos/{o}/{r}/contents/{path}?ref=` — files and directories exist, 404 → false.
- Revisions: branch, then tag, then (hex) commit.
- Pull requests: `state=open` (upstream) by default; closed/all/merged supported; labels and
  target branch client-side. `Author` = poster login.
- Webhooks: `X-Gitea-Event` push and pull_request (Gitea collapses PR sub-events into
  `pull_request`; `synchronized`→synchronize, `label_updated`→labeled,
  `label_cleared`→unlabeled). Delivery: `X-Gitea-Delivery` (fallback `X-Gogs-Delivery`).

### Gogs (REPO-121)

Webhook only: `X-Gogs-Event: push` with `X-Gogs-Signature` (hex HMAC-SHA256). Other events
fail with `ErrUnsupportedEvent`. A push without `repository` is returned without
`Repository` (upstream then has no URL and ignores the event). `MatchesWebhook` excludes
deliveries that also carry `X-Gitea-Event`.

### Bitbucket Server / Data Center (ASET-032, ASET-050, REPO-120)

- API base: required `api`; `/rest` appended unless present (upstream), then `/api/1.0/`.
  Authenticated requests also send `X-Atlassian-Token: no-check` and
  `X-Requested-With: XMLHttpRequest` (upstream).
- Project keys: `RepoQuery.Organization` / `RepoRef.Owner` is the project key. Personal
  projects use `~` + user slug (`~jdoe`), as in the REST API and upstream (which passes the
  key through); a single leading `~` is accepted before the usual safe characters
  (`[A-Za-z0-9_.-]`), and listed repositories in personal projects keep their `~KEY`.
- Repositories: `GET /projects/{key}/repos?limit=100&start=`; the default branch of every
  repository is read (`GET .../branches/default`, 404/204 → repository skipped, before any
  repository-level filter, as upstream); `SHA` = latest commit; `Labels` empty; `https` →
  `http` clone link; unknown protocol → `unknown clone protocol for Bitbucket Server <p>`.
  `Name` is the slug (see gaps). `ArchivedExclude` drops repositories flagged archived (8.0+).
- Paths: `GET .../browse/{path}?at=&type=true`; 404 → false.
- Revisions: `GET .../commits?until=<ref>&limit=1`.
- Pull requests: `state=OPEN` default (closed = DECLINED+MERGED, merged = MERGED);
  `TargetBranch` → `at=refs/heads/<b>&direction=INCOMING`. A label filter fails with
  `KindInvalid` + `ErrLabelsUnsupported` (Bitbucket Server PRs have no labels; upstream has
  no labels field).
- Webhooks: `repo:refs_changed` (first change, as upstream; `TouchedDefaultBranch=true`,
  `ChangedFilesTruncated=true`; `Repository.CloneURL`/`SSHURL` = `http`/`ssh` clone links that
  upstream matches), `pr:opened|from_ref_updated|modified|merged|declined|deleted`,
  `diagnostics:ping` (EventPing). Delivery: `X-Request-Id`. `MatchesWebhook` requires
  `X-Event-Key` without `X-Hook-UUID`.

### Bitbucket Cloud (ASET-034, ASET-051, REPO-119)

- API base: default `https://api.bitbucket.org/2.0`, used as given otherwise.
- Repositories: `GET /repositories/{workspace}?role=member&pagelen=100` (upstream); `Name` =
  slug, `RepositoryID` = UUID, `Labels` empty, clone link by protocol name (`ssh` default);
  repositories without `mainbranch` are omitted; the main branch head comes from
  `GET .../refs/branches/{name}`.
- Paths: `GET .../src/{commit}/{path}?format=meta` (200 → true, 404 → false, other → error),
  as upstream; branch/tag revisions are first resolved to their commit because the source
  API takes a commit (upstream passes the entry SHA).
- Revisions: hex → `GET .../commit/{sha}`; names → `refs/branches/`, then `refs/tags/`.
- Pull requests: `state=OPEN` default (closed = MERGED+DECLINED+SUPERSEDED); labels →
  `ErrLabelsUnsupported`; `HeadSHA` is the API's abbreviated (12-char) hash, as upstream.
- Webhooks: `repo:push` (first change, `TouchedDefaultBranch=true`,
  `ChangedFilesTruncated=true`, `Repository.APIURL` = `links.self.href`) and
  `pullrequest:created|updated|fulfilled|rejected`. Delivery: `X-Request-UUID`.
- Diffstat callback (REPO-119): ingestion that verified the UUID can derive the API base with
  `APIBaseURLFromRepositoryURL(ev.Repository.APIURL, ev.Repository.FullName())`, build a
  provider with the matching repository credentials (`Options.Username`/`Token`, user
  `x-token-auth` = bearer) and call `ChangedFiles(ctx, ref, ev.Before, ev.After)` (diffstat
  `before..after?renames=true`, every page; old and new paths) and
  `TouchesMainBranch(ctx, ref, ev.Revision)`, then clear `ChangedFilesTruncated`.

### Azure DevOps (ASET-033, ASET-052, REPO-122)

- Organization URL: `<api or https://dev.azure.com>/<Options.Organization>`; with an empty
  `Organization` the last path segment of `APIURL` is the organization (registry factory).
  Names may contain spaces and are path-escaped; separators, control characters and `..`
  are rejected.
- Repositories: `GET {project}/_apis/git/repositories?api-version=7.1`. `RepoQuery.Organization`
  = team project. As upstream, entries missing name/default branch/remote URL/ID are skipped,
  `URL` is always the remote (HTTPS) URL (the clone protocol is validated, then ignored),
  `Labels` are empty; disabled repositories are skipped. Branch heads via
  `.../repositories/{id}/stats/branches[?name=]` (404 → no entries).
  `Repository.Organization` = team project (so `Ref()` works); the generator's
  `organization` parameter is `Provider.Organization()`.
- Paths: `GET .../items?path=&versionDescriptor.version=&versionDescriptor.versionType=&$format=json`
  (branch by default, commit for full SHAs, tag for `refs/tags/`); 404 → false.
- Revisions: full SHA → `.../commits/{sha}`; otherwise `.../refs?filter=heads/<n>` then
  `tags/<n>` (exact match, annotated tags peeled, continuation tokens followed).
- Pull requests: project listing `GET {project}/_apis/git/pullrequests` (status active by
  default; completed/abandoned/all; `$top=100`/`$skip`), filtered to `RepoRef.Name`
  (upstream). PRs missing repository/ID/refs/last merge source commit are skipped; labels
  client-side; `Author` = unique name before `@`; missing project →
  `KindNotFound` (upstream's "The following project does not exist").
- Errors: Azure's `typeKey` is kept in the error text; a `203` or HTML response (the sign-in
  page Azure returns for a bad PAT) maps to `KindUnauthorized`.
- Webhooks: `git.push` (first ref update, `TouchedDefaultBranch` = full ref equals the
  repository default branch, `ChangedFilesTruncated=true`, `Repository.HTMLURL`/`CloneURL` =
  remote URL that upstream matches), `git.pullrequest.created` (opened),
  `.updated`/`.merged` (synchronize, or closed once status is completed/abandoned).
  `Repository.Owner`/`Name` = project/repository names, which upstream's ApplicationSet PR
  matching compares. Delivery ID: payload `id`, falling back to `X-Vss-Activityid`.

### AWS CodeCommit (ASET-035)

- aws-sdk-go-v2 `codecommit` + `resourcegroupstaggingapi` (+ `sts`/`stscreds` for
  `Options.Role`), as upstream. Default AWS configuration (environment, shared files,
  IRSA/IMDS) unless `Options.AWSConfig` is given; `Options.Region` overrides; a region is
  required at construction. `scm.Config.APIURL` must be empty (no custom API URL upstream,
  so the allow-list never applies); custom endpoints go through `AWSConfig.BaseEndpoint`.
- The SDK's HTTP client is Patrol's stack (TLS settings, per-attempt timeout, size cap,
  pacing, observer); SDK retries are bounded by `Retry.MaxAttempts`/`Retry.MaxDelay`, with the
  SDK retry quota disabled.
- Repositories: `ListRepositories` (or Tagging `GetResources` for `codecommit:repository`
  with filters grouped by key, values ORed, key-only = any value; unparsable ARNs skipped),
  then `GetRepository` each (any failure fails the listing, as upstream). Uninitialized
  repositories (no default branch) are skipped. `Organization` = account ID, `RepositoryID` =
  repository ID, `Labels` empty; protocols `ssh` (default), `https`, `https-fips`
  (`FIPSCloneURL`). `RepoQuery.Organization` is ignored.
- Branch SHAs: `GetBranch` per returned branch (upstream leaves `sha` empty).
- Paths: `GetFolder` on the parent folder, matching files, folders, submodules and symbolic
  links by base name; root always exists; RepositoryDoesNotExist/CommitDoesNotExist/
  FolderDoesNotExist → false.
- Errors: AWS error codes → kinds (throttling → `RateLimited` with `RetryAfter` 10s; access
  denied → `Forbidden`; invalid/expired credentials → `Unauthorized`; `*DoesNotExist*` →
  `NotFound`; `Invalid*`/validation → `Invalid`).

### Common error mapping (HTTP providers)

400/422 → `Invalid`; 401 → `Unauthorized`; 403 → `Forbidden` (or `RateLimited` when the
rate-limit headers report zero remaining); 404/410 → `NotFound`; 409 → `Conflict`; 429 →
`RateLimited` (`RetryAfter` from `Retry-After`/`RateLimit-Reset`/`X-RateLimit-Reset`, else
60s); 5xx/network/malformed JSON → `Unavailable` (5xx retried); unfollowed 3xx → `Invalid`;
oversize responses → `Invalid` + `ErrResponseTooLarge`; a failing token function →
`Unauthorized` without sending anything. Error texts carry the provider's message (JSON
bodies only, truncated to 512 characters), never request headers or credentials.

## Adapting CRD types (callers)

| Generator / setting | Construction |
|---|---|
| `scmProvider.gitlab` | `gitlab.NewProvider(ctx, Config{APIURL: api, Token, Insecure, CAData, ProxyURL, NoProxy}, Options{IncludeSubgroups, IncludeSharedProjects})`; `RepoQuery{Organization: group, Topics: [topic], Archived: includeArchivedRepos ? ArchivedInclude : ArchivedDefault, AllBranches, CloneProtocol}`. |
| `pullRequest.gitlab` | `RepoRef` from `project` (`ParseFullName`, or `RepoRef{Name: id}` for numeric IDs); `PRQuery{Labels, State}` with `pullRequestState` `""`→`PRStateAll`, `opened`→open, `closed`, `merged`, `locked`→`gitlab.PRStateLocked`. |
| `scmProvider.gitea` / `pullRequest.gitea` | `gitea.NewProvider(ctx, Config{APIURL: api, Token, Insecure})`; `RepoQuery{Organization: owner, Archived: excludeArchivedRepos ? ArchivedExclude : ArchivedDefault}`; PR `RepoRef{owner, repo}`, `PRQuery{Labels}`. |
| `scmProvider.bitbucketServer` / `pullRequest.bitbucketServer` | bearerToken → `Config.Token`, `Options{}`; basicAuth → `Options{Username}` + `Config.Token` = password (bearer wins when both are set, as upstream); `RepoQuery{Organization: project}`; PR `RepoRef{project, repo}`. |
| `scmProvider.bitbucket` | `bitbucketcloud.NewProvider(ctx, Config{Token: appPassword}, Options{Username: user})`; `RepoQuery{Organization: owner}`. |
| `pullRequest.bitbucket` | `Config{APIURL: api, Token}` with `Options{Username}` (basic) or none (bearer/anonymous); `RepoRef{owner, repo}`. |
| `scmProvider.azureDevOps` | `azuredevops.NewProvider(ctx, Config{APIURL: api, Token: accessToken}, Options{Organization})` (the generator rejects an empty token: `no access token provided`); `RepoQuery{Organization: teamProject}`; template `organization` = `Provider.Organization()`. |
| `pullRequest.azuredevops` | same construction; `RepoRef{Owner: project, Name: repo}`, `PRQuery{Labels}`. |
| `scmProvider.awsCodeCommit` | `awscodecommit.NewProvider(ctx, Config{}, Options{Region, Role, TagFilters})`; protocol `https-fips` → `awscodecommit.CloneProtocolHTTPSFIPS`. |
| Webhook secrets | GitLab `webhook.gitlab.secret`; Gitea/Gogs `webhook.gogs.secret` (upstream verifies Gitea deliveries with the Gogs secret); Bitbucket Cloud `webhook.bitbucket.uuid`; Bitbucket Server `webhook.bitbucketserver.secret`; Azure DevOps `azuredevops.JoinSecret(username, password)`. |
| Webhook dispatch order | Use each verifier's `MatchesWebhook`: Azure DevOps, Gitea, Gogs, GitHub, GitLab, Bitbucket Cloud, Bitbucket Server (the header predicates are mutually exclusive for genuine deliveries). |

`continueOnRepoNotFoundError` → `scm.IsNotFound(err)` (Azure: project missing; Bitbucket
Cloud: HTTP 404, not upstream's error-text match). Template parameters `short_sha`,
`short_sha_7`, `head_short_sha` via `scm.ShortSHA`.

## Invariants

- Capabilities are declared, never implied; undeclared methods do not exist on the types.
- Listings are complete or fail (no partial result on any page error, page cap or cursor
  loop). Bitbucket Cloud `next` links that leave the API host are refused (`KindInvalid`).
- Untrusted names (owner, project, repository, branch, path, revision) are validated
  (`..` rejected) and path-escaped before URL construction.
- Credentials are attached only to requests for the configured API host (`HostGuard`) and
  only on clones of requests; no implicit `GITLAB_TOKEN`/`GITEA_TOKEN` environment fallback.
- Webhook authentication runs on the raw body/headers before the event type is inspected;
  token, UUID and basic-auth comparisons are constant time over SHA-256 digests (no length
  leak). Delivery IDs are validated with `scm.ValidateDeliveryID`.
- Factories perform no network I/O (CodeCommit reads local AWS configuration only).
- Providers are safe for concurrent use; per-repository lookups use bounded parallelism (4)
  and keep API order.

## Failure modes

| Failure | Behavior |
|---|---|
| Rate limited (429 / exhausted rate-limit headers / AWS throttling) | Waited out within `Retry.MaxWait` for idempotent requests, else `KindRateLimited` + `RetryAfter`. |
| Server error / network failure | Idempotent requests retried (HTTP: `scm.Chain`; AWS: SDK retryer), then `KindUnavailable`. |
| Invalid or expired credentials | `KindUnauthorized` (Azure's 203 sign-in page included); token function failure → `KindUnauthorized`, nothing sent. |
| Group/project/workspace/repository missing | `KindNotFound`; `PathExists` returns false. |
| Gitea default branch missing on a non-empty repository | Listing fails (upstream). |
| Bitbucket Server `branches/default` 404 for every repository | All repositories skipped (upstream semantics; see gaps). |
| Pagination beyond `MaxPages` | `ErrPageLimit` (`KindInvalid`). |
| Response larger than `MaxResponseBytes` | `ErrResponseTooLarge` (`KindInvalid`). |
| Webhook secret mismatch / missing | `KindUnauthorized` + `ErrSignatureMismatch` / `ErrSignatureMissing`. |
| Unsupported webhook event | `ErrUnsupportedEvent` (`KindInvalid`) after authentication. |

## Security notes

- Tokens, app passwords, PATs, AWS keys and webhook secrets are never logged or included in
  errors: authentication headers are set on request clones below the logging and observer
  layers; API URLs in errors are redacted; error messages come from JSON bodies only.
- GitLab tokens, Bitbucket Cloud UUIDs and Azure DevOps basic credentials authenticate the
  sender but do not cover the body (`Verified=true` means "sender presented the secret");
  only Gitea, Gogs and Bitbucket Server sign the body. All matching is idempotent refresh
  work, as upstream.
- `APIBaseURLFromRepositoryURL` derives a URL from webhook payload data: callers must check
  it against the configured Bitbucket API URL (or the ApplicationSet allow-list) before
  sending repository credentials to it (upstream does not check).
- Callers must apply the ApplicationSet API URL allow-list (ASET-101) to `Config.APIURL`.
  CodeCommit refuses an API URL entirely.
- Proxy settings (`ProxyURL`/`NoProxy`) apply to every HTTP provider when set; upstream
  applies `--scm-proxy-url` only to GitHub, GitLab, Gitea and Bitbucket Server (ASET-045), so
  callers keep parity by not setting it for Bitbucket Cloud and Azure DevOps.

## Contract defects found (`pkg/scm` v1, not changed) and workarounds

1. **No provider-specific configuration in `scm.Config`.** GitLab subgroups/shared projects,
   the Azure DevOps organization, CodeCommit region/role/tag filters and the Bitbucket basic
   auth user cannot be passed through `Registry.New`. Workaround: each package's
   `NewProvider(ctx, cfg, Options)`; the registry factory uses zero options (Azure: the
   organization is taken from the `APIURL` path; CodeCommit: the default AWS configuration).
   Suggested contract addition: `Config.ProviderOptions any`.
2. **`Config.Token` is a bearer token only.** Basic auth needs a user name (Bitbucket Server
   and Cloud). Workaround: `Options.Username` + `Token` as password. Suggested: a
   `BasicAuth{Username string; Password TokenFunc}` field.
3. **`PRQuery.State` defaults to open**, while upstream's GitLab default (empty
   `pullRequestState`) is every state, and GitLab has a `locked` state. Workaround: callers map
   `""` to `PRStateAll`; `gitlab.PRStateLocked`.
4. **`Repository.Organization` doubles as the `RepoRef` owner.** Azure DevOps needs the team
   project for API calls but upstream's `organization` template parameter is the Azure
   organization. Workaround: `Organization` = team project, `Provider.Organization()` for the
   template. Suggested: a separate `Repository.Owner` / template organization field.
5. **`WebhookVerifier` takes one secret.** Azure DevOps uses a user name and password.
   Workaround: `"user:password"` (`JoinSecret`/`SplitSecret`).
6. **Conformance assumptions that not every SCM satisfies** (skips are listed per test with
   the reason): `Webhooks/TamperedBody` assumes a body signature (GitLab, Bitbucket Cloud,
   Azure DevOps authenticate the sender only); `Webhooks/ValidSignature` requires a
   changed-file list in push events (Bitbucket Server/Cloud and Azure DevOps payloads have
   none); `Repositories/NotFound` assumes an organization scope (CodeCommit has none);
   `Repositories/CloneProtocol` assumes the protocol selects the URL (Azure DevOps ignores it
   upstream); `PullRequests/Labels` assumes PR labels (Bitbucket has none);
   `Webhooks/PullRequest` assumes PR events (Gogs: push only upstream). `EmptyRepository`
   must be visible to `Match`, but Bitbucket Server, Azure DevOps and CodeCommit skip
   repositories without a default branch before repository-level filters (upstream), so
   their fixtures report none. Each skipped property is covered by a provider test.
7. **Empty `Event.ChangedFiles` is ambiguous** ("no files" vs "unknown"). Providers whose
   payloads carry no file list set `ChangedFilesTruncated=true` so that callers refresh every
   path (upstream treats an empty list the same way).
8. **One ref per push event.** Bitbucket and Azure DevOps pushes can update several refs;
   only the first is reported (upstream). Suggested: `Event.RefUpdates`.
9. **`PRQuery.Labels` is documented as "all of these labels, exact match"**, but upstream's
   GitLab generator delegates label filtering to GitLab, whose `None`/`Any` keywords select
   unlabeled / labeled merge requests. Workaround: the GitLab provider sends the labels to
   the API and keeps every merge request returned (parity over the contract wording).
   Suggested: document label semantics as provider-defined.
10. **`RepoRef.Validate` / `ValidateSegment` reject `~`**, which Bitbucket Server needs for
    personal project keys (`~userslug`). Workaround: the Bitbucket Server provider validates
    the owner with its own project-key check (optional leading `~`, then the same safe set)
    and still path-escapes it.

## Parity feature areas served

- `06-applicationset`: ASET-030 (GitLab SCM), ASET-031 (Gitea SCM), ASET-032 (Bitbucket
  Server SCM), ASET-033 (Azure DevOps SCM), ASET-034 (Bitbucket Cloud SCM), ASET-035 (AWS
  CodeCommit SCM), ASET-038/039 (path checks per provider), ASET-042 (clone protocols incl.
  `https-fips`), ASET-043 (`allBranches`), ASET-044 (TLS `insecure`/CA — tested for GitLab and
  Bitbucket Server), ASET-045 (proxy building block — tested for GitLab), ASET-048..052 (PR
  generators for GitLab, Gitea, Bitbucket Server, Bitbucket Cloud, Azure DevOps), ASET-056
  (label filters), ASET-057 (repository-not-found mapping), ASET-105/106 (normalized GitLab
  and Azure DevOps push/PR events with the fields upstream matches on).
- `03-repositories-clusters-webhooks`: REPO-118 (GitLab), REPO-119 (Bitbucket Cloud incl.
  diffstat/main-branch callback building blocks), REPO-120 (Bitbucket Server), REPO-121
  (Gogs), REPO-122 (Azure DevOps) — verification and normalization; HTTP status codes and
  routing belong to `internal/webhook`.

All of the above are `simulated` (fake servers), not live-verified.

## Known gaps and differences from upstream

- No live verification against any real SCM service (see the banner).
- GitLab: `archived` semantics depend on GitLab's API (`archived=true` = only archived);
  Patrol reproduces upstream's wire parameters rather than the documented intent of
  `includeArchivedRepos`. Merge request webhooks carry no author username (only the actor).
- Gitea: upstream reads only the first page of repositories and branches; Patrol reads all
  pages. Gitea clone URL for `https` is the HTML URL (upstream), and `HTTPSURL` mirrors it.
- Bitbucket Server: personal project keys must be given as `~` + user *slug*; a raw
  user name containing characters outside `[A-Za-z0-9_.-]` (for example `@`) is rejected
  with `KindInvalid` (upstream would send it and get the server's answer).
  `Name` is the slug; upstream uses the display name (identical for
  lower-case names without spaces; upstream's API calls fail for the others). The deprecated
  `branches/default` endpoint is used as upstream; a server that removed it would skip every
  repository. Unsigned `diagnostics:ping` is rejected when a secret is configured (upstream
  accepts it).
- Bitbucket Cloud: webhook HMAC secrets (`X-Hub-Signature`) are not supported — upstream
  authenticates by UUID only. `ChangedFiles` reports old and new paths (upstream new only).
  The diffstat callback itself (repository lookup, credentials) belongs to webhook ingestion.
- Azure DevOps: repositories are addressed by name in `RepoRef`; tags in `PathExists` need
  `refs/tags/`. Upstream reads only the first page of project pull requests; Patrol pages.
- AWS CodeCommit: branch SHAs cost one `GetBranch` per branch (upstream leaves `sha` empty);
  revision resolution not declared (no tag API); `RepoQuery.Organization` ignored.
- Pull request and push event normalization for Bitbucket Server, Bitbucket Cloud and Gitea
  pull requests goes beyond upstream (which ignores those events); ingestion decides whether
  to act on them.
- Provider-specific options are not reachable through `scm.Registry` (contract defect 1).

## Verification

Run on 2026-10-13 (darwin/arm64, Go 1.27.2), all against local fakes:

| Command | Result |
|---------|--------|
| `go build ./...` | exit 0 |
| `go vet ./pkg/scm/gitlab/... ./pkg/scm/gitea/... ./pkg/scm/gogs/... ./pkg/scm/bitbucketcloud/... ./pkg/scm/bitbucketserver/... ./pkg/scm/azuredevops/... ./pkg/scm/awscodecommit/...` | exit 0 |
| `golangci-lint run` on the same packages | 0 issues |
| `go test -race -count=1 -json` on the same packages | exit 0; 97 top-level tests pass; 345 tests+subtests pass, 101 skipped (conformance checks for undeclared capabilities, plus the documented per-provider skips above), 0 failed |
| `go test -race -count=3 ./pkg/scm/gitlab/... ./pkg/scm/bitbucketserver/...` | exit 0 (flakiness check after the review fixes) |

Per package (tests+subtests pass/skip): gitlab 125/14, gitea 43/6, gogs 12/25,
bitbucketcloud 39/10, bitbucketserver 70/16, azuredevops 34/10, awscodecommit 22/20.

Review follow-up (same date): GitLab label keywords (`TestPullRequestLabelKeywords`,
`TestPullRequestLabelsNotRecheckedClientSide`) and Bitbucket Server personal projects
(`TestPersonalProject`, `TestProjectKeyValidation`, rapid `TestProjectKeyPathProperty`) were
added; each new test was confirmed to fail against the pre-fix code via `go test -overlay`.

Conformance (`conformance.Run`) runs: GitLab (direct constructor and registry with a numeric
group), Gitea (registry), Gogs (registry, webhooks only), Bitbucket Server (basic auth and
bearer via registry), Bitbucket Cloud (basic auth), Azure DevOps (registry with organization
URL), AWS CodeCommit (static credentials and `BaseEndpoint` fake). Additional tests cover
group options and wire parameters, archived/topic mapping, error and rate-limit mapping, TLS
CA/insecure, proxy/no-proxy, cross-host redirect credential stripping, page caps, response
size caps, cancellation, tag-filter requests, STS role assumption, the AWS default-config
path, webhook fixtures for every supported event, and property-based tests (rapid) for
signature/token/UUID verification, URL normalization idempotency, secret splitting and the
FIPS rewrite.
