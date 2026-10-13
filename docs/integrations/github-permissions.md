# GitHub permissions for Patrol CD

This page lists the least privileges Patrol CD needs on GitHub (github.com, GitHub
Enterprise Server, GitHub Enterprise Cloud) for each capability of the `pkg/scm/github`
provider, the webhook events to subscribe to, and the equivalent personal access token (PAT)
scopes. Grant only the rows for features you enable.

A GitHub App is the recommended identity: tokens are short-lived (1 hour, refreshed by
Patrol 5 minutes before expiry), scoped to the repositories the App is installed on, and
the only identity that can create check runs.

## GitHub App permissions per Patrol capability

| Patrol capability (feature) | GitHub API used | GitHub App permission | Needed when |
|---|---|---|---|
| Always | repository metadata in every response | **Metadata: Read-only** (mandatory for every App) | always |
| `repositories` (SCM Provider generator) | `GET /orgs/{org}/repos`, `GET /users/{user}/repos`, `GET /repos/{o}/{r}` | Metadata: Read-only | SCM Provider generator |
| `branches` (`allBranches`, default-branch SHA) | `GET /repos/{o}/{r}/branches`, `GET /repos/{o}/{r}/branches/{b}` | **Contents: Read-only** | SCM Provider generator |
| `paths` (`pathsExist` / `pathsDoNotExist`) | `HEAD`/`GET /repos/{o}/{r}/contents/{path}?ref=` | Contents: Read-only | SCM generator path filters |
| `revisions` (resolve branch/tag to SHA) | `GET /repos/{o}/{r}/commits/{ref}` | Contents: Read-only | revision resolution through the API |
| `credentials` (Git over HTTPS clone/fetch) | Git smart HTTP with user `x-access-token` | Contents: Read-only (**Read and write** only for the source hydrator / commit-server push) | private repositories |
| `pullRequests` (Pull Request generator) | `GET /repos/{o}/{r}/pulls` | **Pull requests: Read-only** | Pull Request generator |
| `commitStatuses` (notifications `github.status`) | `POST /repos/{o}/{r}/statuses/{sha}` | **Commit statuses: Read and write** | only when status reporting is enabled |
| `checkRuns` (notifications `github.checkRun`) | `GET /repos/{o}/{r}/commits/{sha}/check-runs`, `POST`/`PATCH /repos/{o}/{r}/check-runs` | **Checks: Read and write** | only when check-run reporting is enabled |
| Notifications `github.deployment` (via `Provider.Client()`) | `POST /repos/{o}/{r}/deployments`, `.../statuses` | Deployments: Read and write | only when deployment reporting is enabled |
| Notifications `github.pullRequestComment` (via `Provider.Client()`) | `GET`/`POST`/`PATCH /repos/{o}/{r}/issues/{n}/comments` | Pull requests: Read and write | only when PR comments are enabled |
| `installations` (installation discovery, listing) | `GET /app/installations`, `GET /orgs/{org}/installation`, `GET /users/{user}/installation`, `POST /app/installations/{id}/access_tokens` | none (authenticated with the App JWT) | App authentication |
| Hydrator commit author identity (`BotIdentity`) | `GET /app`, `GET /users/{slug}[bot]` | none beyond Metadata | source hydrator |

Organization permissions: none are required. Account permissions: none.

Installation scope: install the App on "Only select repositories" whenever possible. The SCM
Provider generator only sees repositories the installation can access, which is the desired
least-privilege behavior.

### Minimal permission sets

| Use case | App permissions |
|---|---|
| Sync from private repositories only | Metadata: Read, Contents: Read |
| + SCM Provider generator | Metadata: Read, Contents: Read |
| + Pull Request generator | + Pull requests: Read |
| + Commit status notifications | + Commit statuses: Read and write |
| + Check run notifications | + Checks: Read and write |
| + Source hydrator pushes | Contents: Read and write (write credentials belong to the commit-server only) |

## Webhook events

Configure the webhook URL `https://<patrol-server>/api/webhook` (Application refresh) and,
for ApplicationSets, the ApplicationSet webhook endpoint. Use content type
`application/json` and set a secret (`webhook.github.secret`); Patrol verifies
`X-Hub-Signature-256` (HMAC-SHA256) and rejects deliveries with a missing or wrong signature
when a secret is configured.

| Event | Used for | Requires App permission to subscribe |
|---|---|---|
| `push` | Application refresh (with changed-path filtering), Git generator refresh | Contents: Read |
| `pull_request` (`opened`, `synchronize`, `reopened`, `closed`, `labeled`, `unlabeled`) | Pull Request generator refresh | Pull requests: Read |
| `repository` (`created`, `archived`, `renamed`, …) | SCM Provider generator refresh (optional) | Metadata: Read |
| `installation`, `installation_repositories` | Installation cache invalidation, SCM generator refresh (delivered to Apps automatically) | none |
| `ping` | Delivery check (acknowledged, no action) | none |

`package` events are handled by the GHCR registry webhook parser, not by the SCM provider.

## Personal access token equivalents

Check runs cannot be created with personal access tokens (GitHub restricts check-run writes
to GitHub Apps); Patrol does not declare the `checkRuns` capability for token
authentication.

| Patrol capability | Fine-grained PAT (repository permissions) | Classic PAT scope |
|---|---|---|
| Always | Metadata: Read-only (added automatically) | none for public repositories |
| `repositories`, `branches`, `paths`, `revisions`, `credentials` (read) | Contents: Read-only | `repo` for private repositories (classic tokens have no read-only private scope); public repositories need no scope |
| `pullRequests` | Pull requests: Read-only | `repo` (private) / none (public) |
| `commitStatuses` | Commit statuses: Read and write | `repo:status` (private and public) |
| Hydrator push | Contents: Read and write | `repo` (private) / `public_repo` (public) |
| Deployments (notifications) | Deployments: Read and write | `repo_deployment` |
| PR comments (notifications) | Pull requests: Read and write | `repo` (private) / `public_repo` (public) |
| `checkRuns` | not supported | not supported |
| `installations` | not applicable | not applicable |

Fine-grained PAT notes: the token's resource owner must be the organization being scanned,
and the repository access list limits what the SCM Provider generator can discover.
Anonymous access works for public repositories only and is limited to 60 requests per hour
per IP address; Patrol then declares only read capabilities.

## Rate limits

Patrol paces requests client-side (default 10 requests/s, burst 20, per provider instance),
waits out short rate-limit resets (up to 60 seconds by default), and otherwise reports a
`RateLimited` error with the reset time so controllers requeue instead of hammering the API.
App installation tokens have a higher rate limit than PATs on large organizations.
