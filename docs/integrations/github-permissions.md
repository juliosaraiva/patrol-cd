# GitHub permissions for Patrol CD

This page lists the least privileges Patrol CD needs on GitHub (github.com, GitHub
Enterprise Server, GitHub Enterprise Cloud) for each capability of the `pkg/scm/github`
provider and of the notifications `github` service, the webhook events to subscribe to, and
the equivalent personal access token (PAT) scopes. Grant only the rows for features you
enable.

A GitHub App is the recommended identity: tokens are short-lived (1 hour, refreshed by
Patrol 5 minutes before expiry), scoped to the repositories the App is installed on, and
the only identity that can create check runs. The notifications `github` service accepts
only GitHub App credentials.

## GitHub App permissions per Patrol capability

Two clients call the GitHub API. The `pkg/scm/github` provider serves the ApplicationSet
SCM Provider and Pull Request generators and mints the installation tokens used to clone
over HTTPS (`internal/repository`). The notifications `github` service
(`internal/notifications/services/github.go`) has its own GitHub App client; it does not
use `pkg/scm`.

### `pkg/scm/github` provider

| Patrol capability (feature) | GitHub API used | GitHub App permission | Needed when |
|---|---|---|---|
| Always | repository metadata in every response | **Metadata: Read-only** (mandatory for every App) | always |
| `repositories` (SCM Provider generator) | `GET /orgs/{org}/repos`, `GET /users/{user}/repos`, `GET /repos/{o}/{r}` | Metadata: Read-only | SCM Provider generator |
| `branches` (`allBranches`, default-branch SHA) | `GET /repos/{o}/{r}/branches`, `GET /repos/{o}/{r}/branches/{b}` | **Contents: Read-only** | SCM Provider generator |
| `paths` (`pathsExist` / `pathsDoNotExist`) | `HEAD`/`GET /repos/{o}/{r}/contents/{path}?ref=` | Contents: Read-only | SCM generator path filters |
| `credentials` (Git over HTTPS clone/fetch) | Git smart HTTP with user `x-access-token` | Contents: Read-only (**Read and write** only for source hydrator pushes) | private repositories |
| `pullRequests` (Pull Request generator) | `GET /repos/{o}/{r}/pulls` | **Pull requests: Read-only** | Pull Request generator |
| `installations` (installation discovery, listing) | `GET /app/installations`, `GET /orgs/{org}/installation`, `GET /users/{user}/installation`, `POST /app/installations/{id}/access_tokens` | none (authenticated with the App JWT) | App authentication |

The provider also implements capabilities that no Patrol component calls yet, so they need
no permission today:

| Capability | GitHub API it would use | Status |
|---|---|---|
| `revisions` (resolve branch/tag to SHA) | `GET /repos/{o}/{r}/commits/{ref}` | no caller: the repo-server resolves revisions with `git ls-remote` |
| `commitStatuses` | `POST /repos/{o}/{r}/statuses/{sha}` | no caller: commit statuses come from the notifications service (below) |
| `checkRuns` | `GET /repos/{o}/{r}/commits/{sha}/check-runs`, `POST`/`PATCH /repos/{o}/{r}/check-runs` | no caller: check runs come from the notifications service (below); `SCMProvider` `spec.statusReporting` is not wired (#235) |
| Commit author identity (`BotIdentity`) | `GET /app`, `GET /users/{slug}[bot]` | no caller: the source hydrator takes its commit author from `commit.author.name` and `commit.author.email` in `patrol-cm` |

### Notifications `github` service

Configured in `patrol-notifications-cm` (`service.github`) with `appID`, `installationID` and
`privateKey`. It sends only what a notification template asks for.

| Template field | GitHub API used | GitHub App permission |
|---|---|---|
| `github.status` | `POST /repos/{o}/{r}/statuses/{sha}` | **Commit statuses: Read and write** |
| `github.checkRun` | `POST /repos/{o}/{r}/check-runs` (each notification creates a new check run; existing ones are never read or updated) | **Checks: Read and write** |
| `github.deployment` | `GET`/`POST /repos/{o}/{r}/deployments`, `POST /repos/{o}/{r}/deployments/{id}/statuses` | **Deployments: Read and write** |
| `github.pullRequestComment` | `GET /repos/{o}/{r}/commits/{sha}/pulls`, `GET`/`POST /repos/{o}/{r}/issues/{n}/comments`, `PATCH /repos/{o}/{r}/issues/comments/{id}` | **Pull requests: Read and write** |

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
| + Source hydrator pushes | Contents: Read and write, through a `Repository` or `RepositoryCredential` with `purpose: write`. The controller's hydrator resolves those credentials and sends them to the commit-server with each push request; the commit-server keeps none. |

## Webhook events

Configure the webhook URL `https://<patrol-server>/api/webhook` (or `/api/webhook/github`).
It refreshes both Applications and ApplicationSets, so no separate ApplicationSet webhook
is needed. The ApplicationSet controller serves the same handler on its own listener only
when `--webhook-addr` is set (chart `appsetController.webhook.enabled`, off by default). Use
content type `application/json` and set a secret (`webhook.github.secret`, or an
`SCMProvider` `spec.webhook.secretRef`); Patrol verifies `X-Hub-Signature-256`
(HMAC-SHA256) and rejects deliveries with a missing or wrong signature when a secret is
configured.

| Event | Used for | Requires App permission to subscribe |
|---|---|---|
| `push` | Application refresh (with changed-path filtering), Git generator refresh | Contents: Read |
| `pull_request` (`opened`, `synchronize`, `reopened`, `closed`, `labeled`, `unlabeled`) | Pull Request generator refresh | Pull requests: Read |
| `repository`, `installation`, `installation_repositories` | Nothing: verified, parsed and acknowledged with `200`, then ignored. Installation tokens and IDs stay cached until they expire. Subscribing is not needed (Apps receive `installation` events anyway). | none |
| `ping` | Delivery check (acknowledged, no action) | none |

`package` events are handled by the GHCR registry webhook parser, not by the SCM provider.

## Personal access token equivalents

Check runs cannot be created with personal access tokens (GitHub restricts check-run writes
to GitHub Apps); Patrol does not declare the `checkRuns` capability for token
authentication. The notifications `github` service takes no token at all, so commit
statuses, deployments and pull request comments need a GitHub App too.

| Patrol capability | Fine-grained PAT (repository permissions) | Classic PAT scope |
|---|---|---|
| Always | Metadata: Read-only (added automatically) | none for public repositories |
| `repositories`, `branches`, `paths`, `credentials` (read) | Contents: Read-only | `repo` for private repositories (classic tokens have no read-only private scope); public repositories need no scope |
| `pullRequests` | Pull requests: Read-only | `repo` (private) / none (public) |
| Hydrator push | Contents: Read and write | `repo` (private) / `public_repo` (public) |
| Notifications (statuses, check runs, deployments, PR comments) | not supported (GitHub App only) | not supported |
| `installations` | not applicable | not applicable |

Fine-grained PAT notes: the token's resource owner must be the organization being scanned,
and the repository access list limits what the SCM Provider generator can discover.
Anonymous access works for public repositories only and is limited to 60 requests per hour
per IP address; Patrol then declares only read capabilities.

## Rate limits

The `pkg/scm` clients pace requests client-side (10 requests/s, burst 20, per provider
instance), wait out short rate-limit resets (up to 60 seconds by default), and otherwise
report a `RateLimited` error with the reset time so controllers requeue instead of
hammering the API. `SCMProvider` `spec.rateLimit` is validated but not applied yet (#236):
every client uses those defaults. The notifications `github` service does not pace its
requests. App installation tokens have a higher rate limit than PATs on large
organizations.
