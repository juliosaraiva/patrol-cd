# Public API contract

Normative for `internal/apiserver`, `pkg/apiclient`, `internal/cli` and `ui/`. The generated
`api/openapi/openapi.json` must match this table; differences are bugs.

## Conventions

- Base path `/api/v1` (plus `--rootpath` prefix when served under a subpath).
- JSON bodies; CRD objects use their Kubernetes JSON form (`apiVersion`, `kind`, `metadata`,
  `spec`, `status`).
- Authentication: `Authorization: Bearer <jwt>` or `patrol.token` cookie (UI). Cookie-based
  state-changing requests must send `X-Requested-With: patrol`.
- Errors: `application/problem+json` `{type, title, status, detail, code}` where `code` is a
  stable identifier (`not_found`, `permission_denied`, `invalid_argument`, `conflict`,
  `already_exists`, `failed_precondition`, `unauthenticated`, `unavailable`, `internal`,
  `resource_exhausted`). Permission denials on existing-but-forbidden objects return 403 with
  the same body as 404-forbidden-by-project would leak nothing extra (upstream returns
  `PermissionDenied` for both "not found" and "forbidden" on application gets; Patrol keeps
  that to avoid existence oracles).
- Optimistic concurrency: updates accept `metadata.resourceVersion`; stale → 409 `conflict`.
- Path parameters: a single-segment value is escaped as a whole (`/` → `%2F`), so it may contain
  slashes (repository and cluster URLs). A value of exactly `/` is never routed: `http.ServeMux`
  takes the lone `/` for a trailing slash. Under `/api/` the router answers 404 `not_found`
  (`TestSlashOnlyPathValueIsNotRouted`, CI-F20); outside it (`/extensions/{extension}/...`) the
  request falls through to the UI handler when the UI is served.
- Lists: `?limit=&continue=` pagination (opaque token), `?selector=` label selector; response
  `{items: [...], metadata: {continue, resourceVersion}}`.
- Applications in any namespace: `?appNamespace=` on application routes (default: control
  plane namespace); ApplicationSets: `?appsetNamespace=`. Object references in RBAC become
  `<project>/<namespace>/<name>` when namespace ≠ control-plane namespace.
- Streams (`/api/v1/stream/...`): `text/event-stream`; events `{type: ADDED|MODIFIED|DELETED,
  object}`; heartbeat comment every 15s; authorization re-evaluated per event (objects the
  caller may no longer see are not sent).
- Every route declares its RBAC check `(resource, action, object)` in the route table.

## Routes

### Session, account, settings, version

| Method | Path | RBAC | Notes |
|--------|------|------|-------|
| POST | `/session` | — (rate limited) | local login `{username,password}` → `{token}`; sets cookie when `?cookie=true` |
| DELETE | `/session` | authenticated | logout; revokes `jti`; clears cookie; returns OIDC logout URL if configured |
| GET | `/session/userinfo` | — | `{loggedIn, username, iss, groups}` |
| GET | `/auth/login` | — | starts OIDC flow (`?return_url=`) |
| GET | `/auth/callback` | — | OIDC callback; validates state/nonce/PKCE |
| POST | `/session/oidc-exchange` | — | CLI SSO: exchange ID token (from PKCE flow against `cliClientID`) for Patrol session |
| GET | `/account` | accounts,get | list accounts |
| GET | `/account/{name}` | accounts,get | |
| GET | `/account/can-i/{resource}/{action}/{subresource...}` | — | `{value: "yes"|"no"}` for caller |
| PUT | `/account/password` | self or accounts,update | `{name, currentPassword, newPassword}` |
| POST | `/account/{name}/token` | accounts,update (self allowed) | `{expiresIn, id}` → `{token}` |
| DELETE | `/account/{name}/token/{id}` | accounts,update | |
| GET | `/settings` | — (public subset unauthenticated) | UI/CLI settings: url, OIDC config (no secrets), banners, links, extensions, kustomize options, resource overrides (authenticated) |
| GET | `/settings/plugins` | authenticated | CMP plugins with parameters announce |
| GET | `/version` (also `/api/version`) | — | build info, tool versions |

### Applications

| Method | Path | RBAC (object `<project>/<app>`) | Notes |
|--------|------|------|-------|
| GET | `/applications` | applications,get (filtered per item) | `?projects=&selector=&repo=&appNamespace=&name=` |
| GET | `/stream/applications` | per item | SSE watch |
| POST | `/applications` | applications,create | `?upsert=&validate=` |
| GET | `/applications/{name}` | applications,get | `?refresh=normal|hard&project=` (refresh sets annotation then waits up to 15s for reconciliation) |
| PUT | `/applications/{name}` | applications,update | full object; `?validate=&project=` |
| PUT | `/applications/{name}/spec` | applications,update | |
| PATCH | `/applications/{name}` | applications,update | `{patch, patchType: json|merge}` |
| DELETE | `/applications/{name}` | applications,delete | `?cascade=&propagationPolicy=foreground|background` |
| POST | `/applications/{name}/sync` | applications,sync (+override when `sources`/`manifests`/revision override) | creates Operation; body mirrors `Operation.spec.sync` + `retryStrategy`, `infos`, `dryRun` |
| POST | `/applications/{name}/rollback` | applications,sync | `{id, prune, dryRun}` |
| DELETE | `/applications/{name}/operation` | applications,sync | sets `spec.cancel` on running Operation |
| GET | `/applications/{name}/operations` | applications,get | Patrol addition: Operation list |
| GET | `/applications/{name}/operations/{op}` | applications,get | |
| GET | `/applications/{name}/syncwindows` | applications,get | |
| GET | `/applications/{name}/revisions/{revision}/metadata` | applications,get | `?sourceIndex=&versionId=` |
| GET | `/applications/{name}/revisions/{revision}/chartdetails` | applications,get | |
| GET | `/applications/{name}/revisions/{revision}/ocimetadata` | applications,get | |
| GET | `/applications/{name}/manifests` | applications,get | `?revision=&sourcePositions=&revisions=` |
| POST | `/applications/{name}/manifests-with-files` | applications,get | tar.gz body (local sources for server-side generate) |
| GET | `/applications/{name}/managed-resources` | applications,get | diff data via controller tree API; secrets masked |
| POST | `/applications/{name}/server-side-diff` | applications,get | |
| GET | `/applications/{name}/resource-tree` | applications,get | |
| GET | `/stream/applications/{name}/resource-tree` | applications,get | SSE |
| GET | `/applications/{name}/events` | applications,get | `?resourceNamespace=&resourceName=&resourceUID=` |
| GET | `/applications/{name}/resource` | applications,get | live manifest (secrets masked) |
| POST | `/applications/{name}/resource` | applications,update/<group>/<kind>/<ns>/<name> | patch live resource |
| DELETE | `/applications/{name}/resource` | applications,delete/<group>/<kind>/<ns>/<name> | `?force=&orphan=` |
| GET | `/applications/{name}/resource/actions` | applications,get | |
| POST | `/applications/{name}/resource/actions` | applications,action/<group>/<kind>/<action> | `{action, resourceActionParameters}` |
| GET | `/applications/{name}/logs` and `/applications/{name}/pods/{pod}/logs` | logs,get | SSE; `?container=&sinceSeconds=&tailLines=&follow=&filter=&previous=&kind=&group=&resourceName=` |
| GET | `/applications/{name}/links` | applications,get | deep links |
| GET | `/applications/{name}/resource/links` | applications,get | |
| GET (WS) | `/terminal` | exec,create | `?appName=&appNamespace=&projectName=&namespace=&pod=&container=&shell=` |

### ApplicationSets

| Method | Path | RBAC (`<project>/<name>`) |
|--------|------|------|
| GET | `/applicationsets` | applicationsets,get (filtered) |
| GET | `/stream/applicationsets` | per item |
| POST | `/applicationsets` | applicationsets,create (`?upsert=&dryRun=`) |
| GET | `/applicationsets/{name}` | applicationsets,get |
| DELETE | `/applicationsets/{name}` | applicationsets,delete |
| POST | `/applicationsets/generate` | applicationsets,create (dry-run render of generated Applications) |
| GET | `/applicationsets/{name}/resource-tree` | applicationsets,get |
| GET | `/applicationsets/{name}/events` | applicationsets,get |

### Projects

| Method | Path | RBAC |
|--------|------|------|
| GET/POST | `/projects` | projects,get / projects,create |
| GET/PUT/DELETE | `/projects/{name}` | projects,get/update/delete |
| GET | `/projects/{name}/detailed` | projects,get (includes global projects, scoped repos/clusters) |
| GET | `/projects/{name}/globalprojects` | projects,get |
| GET | `/projects/{name}/events` | projects,get |
| GET | `/projects/{name}/syncwindows` | projects,get |
| GET | `/projects/{name}/links` | projects,get |
| POST | `/projects/{project}/roles/{role}/token` | projects,update |
| DELETE | `/projects/{project}/roles/{role}/token/{iat}` | projects,update (`?id=`) |

### Repositories, credentials, certificates, GPG keys, SCM providers

Repository routes accept `{repo}` as the URL-encoded repository URL (Argo-compatible) and
resolve the `Repository` object by `spec.url` (+ `?appProject=`). Write variants use
`/write-repositories` and `/write-repocreds` (purpose `write`).

| Method | Path | RBAC (`repositories` / `write-repositories`, object `<project>/<url>`) |
|--------|------|------|
| GET/POST | `/repositories` | get / create (`?upsert=&credsOnly=`) |
| GET/PUT/DELETE | `/repositories/{repo}` | get/update/delete (`?forceRefresh=` on GET) |
| GET | `/repositories/{repo}/refs` | get |
| GET | `/repositories/{repo}/oci-tags` | get |
| GET | `/repositories/{repo}/apps` | get (`?revision=&appName=&appProject=`) |
| POST | `/repositories/{repo}/appdetails` | get |
| GET | `/repositories/{repo}/helmcharts` | get |
| POST | `/repositories/{repo}/validate` | create (test connection without saving) |
| GET/POST | `/repocreds` | repositories,get/create |
| PUT/DELETE | `/repocreds/{url}` | repositories,update/delete |
| GET/POST/DELETE | `/certificates` | certificates,get/create/delete (`?hostNamePattern=&certType=&certSubType=`) |
| GET/POST | `/gpgkeys` | gpgkeys,get/create |
| GET/DELETE | `/gpgkeys/{keyID}` | gpgkeys,get/delete |
| GET/POST | `/scmproviders` | scmproviders,get/create (Patrol addition) |
| GET/PUT/DELETE | `/scmproviders/{name}` | scmproviders,get/update/delete |

### Clusters

`{server}` is the URL-encoded server URL, or the cluster name with `?id.type=name`.

| Method | Path | RBAC (`clusters`, object = server URL) |
|--------|------|------|
| GET/POST | `/clusters` | get / create (`?upsert=`) |
| GET/PUT/DELETE | `/clusters/{server}` | get/update/delete (`?updatedFields=`) |
| POST | `/clusters/{server}/rotate-auth` | update |
| POST | `/clusters/{server}/invalidate-cache` | update |

### Notifications, webhooks, badges, extensions

| Method | Path | Auth |
|--------|------|------|
| GET | `/notifications/services`, `/notifications/triggers`, `/notifications/templates` | authenticated |
| POST | `/api/webhook` (and `/api/webhook/{provider}`) | provider signature/secret |
| GET | `/api/badge` | public when `statusbadge.enabled`; `?name=&appNamespace=&project=&revision=&showAppName=&width=&keepFullRevision=` |
| ANY | `/extensions/{extension}/...` | extensions,invoke,`<extension>` + application access via `Argocd-Application-Name`-equivalent headers `Patrol-Application-Name`, `Patrol-Project-Name` |

### Internal (not public)

| Service | Path prefix | Auth |
|---------|-------------|------|
| controller tree API | `/internal/v1/tree`, `/internal/v1/managed-resources`, `/internal/v1/server-side-diff` | internal token |
| repo-server | `/repo/v1/*` | internal token |
| commit-server | `/commit/v1/*` | internal token |
| cmp-server | unix socket `/plugin/v1/*` | socket permissions |
