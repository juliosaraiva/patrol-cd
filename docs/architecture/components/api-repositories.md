# Component: repository API (`internal/apiserver/services/repositories`)

Status: implemented (routes, Go client, tests against envtest). Not yet registered by the
patrol-server command (`cmd/patrolcd`), which is wired separately. Decision records:
[ADR-0003](../adr/0003-resource-model.md) (typed Repository/RepositoryCredential/SCMProvider
objects referencing Secrets), [ADR-0005](../adr/0005-public-api.md),
[ADR-0006](../adr/0006-internal-apis.md) (the repo-server never reads Secrets),
[ADR-0009](../adr/0009-authn-authz.md), [ADR-0010](../adr/0010-scm-provider-contract.md).
Normative contract: [`api-contract.md`](../api-contract.md), "Repositories, credentials,
certificates, GPG keys, SCM providers". Upstream behavior researched in Argo CD v3.5.5
`server/repository`, `server/repocreds`, `server/certificate`, `server/gpgkey`,
`util/db/{repository,repository_secrets,certificate,gpgkeys}.go`; nothing is copied.

| Package | Contents |
|---------|----------|
| `internal/apiserver/services/repositories` | `New(Deps) *Service` (implements `apiserver.Service`), route registration, handlers, input validation, managed credentials Secrets, trust ConfigMaps |
| `pkg/apiclient/repositories.go` | Route patterns, request/response types shared with the server, Go client methods |

## Wiring

```go
svc := repositories.New(repositories.Deps{
	Config: repositories.Config{Namespace: ns, HydratorEnabled: params.Bool("hydrator.enabled")},
	Client:       directClient,                 // controller-runtime client.New (no cache): read-after-write
	Resolver:     resolver,                     // *repository.Resolver over a started *repository.Lister
	StatusWriter: repository.NewStatusWriter(dyn),
	RepoServer:   repoServerClient,             // internal/reposerver/apiclient HTTP client
	Settings:     settingsManager,              // globalProjects, kustomize/Helm options, source types
})
apiserver.New(cfg, apiserver.Deps{Services: []apiserver.Service{svc, ...}})
```

`Config`: `ConnectionTimeout` (30 s per connection check), `ConnectionStateTTL` (1 h, the
upstream connection status cache expiration), `MaxParallelChecks` (8 per list request),
`RepoServerTimeout` (2 min for refs/tags/apps/details/charts), the three trust ConfigMap
names (default `patrol-tls-certs-cm`, `patrol-ssh-known-hosts-cm`, `patrol-gpg-keys-cm`). A
`Service` with zero `Deps` registers its routes (OpenAPI generation) and answers 503.

The server's service account needs, in the control-plane namespace: get/list/create/update/
delete on `repositories`, `repositorycredentials`, `scmproviders`, Secrets and ConfigMaps;
update on `repositories/status`; get/list on `appprojects` and get on `applications`.

## Routes

`{repo}`/`{url}` are the URL-encoded repository URL or template prefix (one path segment;
`%2F` decodes to `/`). Write variants use resource `write-repositories`. RBAC objects follow
upstream `createRBACObject`: `<project>/<url>` for project-scoped repositories, `<url>`
otherwise; templates use the URL prefix; certificates and GnuPG keys use `""`.

| Method | Path | RBAC | Enforced | Notes |
|--------|------|------|----------|-------|
| GET | `/repositories` | repositories, get, `<project>/<url>` per item | handler (`rest.Can`) | `?forceRefresh&appProject&selector&limit&continue`; sorted `<project>/<url>`; connection checked when missing, for another generation, older than the TTL, or `forceRefresh` |
| POST | `/repositories` | repositories, create, `<project>/<url>` (+ update on upsert; + `scmproviders, get, <name>` for a new `spec.scmProviderRef`) | handler | `?upsert&credsOnly` |
| GET | `/repositories/{repo}` | repositories, get | handler | `?appProject&forceRefresh`; missing and forbidden both 403 `permission denied` |
| PUT | `/repositories/{repo}` | repositories, update on current **and** new `<project>/<url>` | handler | `?appProject` = current project when moving; `metadata.resourceVersion` → 409 when stale |
| DELETE | `/repositories/{repo}` | repositories, delete | handler | `?appProject`; deletes the managed Secret |
| POST | `/repositories/{repo}/validate` | repositories, create | handler | nothing stored; failure 400 with the connection message |
| GET | `/repositories/{repo}/refs` | repositories, get on the repository used for `?appProject` | handler | branches/tags |
| GET | `/repositories/{repo}/oci-tags` | same | handler | type hint `oci` |
| GET | `/repositories/{repo}/helmcharts` | same | handler | type hint `helm` |
| GET | `/repositories/{repo}/apps` | same + applications, create **or** update, `<appProject>/<appName>` + sourceRepos | handler | `?revision&appName&appProject` (required); sorted by path |
| POST | `/repositories/{repo}/appdetails` | same + applications, get (+ create for a new Application); existing Application: same project, source current or in history | handler | body `{source, appName, appProject, sourceIndex, versionId}` |
| * | `/write-repositories[...]` | write-repositories, ... | handler | 501 `hydrator is disabled` unless `HydratorEnabled`; create needs credentials and always tests |
| GET | `/repocreds`, `/write-repocreds` | repositories / write-repositories, get, `<urlPrefix>` per item | handler | |
| POST | `/repocreds`, `/write-repocreds` | create, `<urlPrefix>` (+ update on upsert) | handler | `?upsert` |
| PUT | `/repocreds/{url}`, `/write-repocreds/{url}` | update, `{url}` | middleware | exact prefix |
| DELETE | `/repocreds/{url}`, `/write-repocreds/{url}` | delete, `{url}` | middleware | exact prefix |
| GET/POST/DELETE | `/certificates` | certificates, get/create/delete, `""` | middleware | `?hostNamePattern&certType&certSubType`; POST `?upsert` |
| GET/POST | `/gpgkeys` | gpgkeys, get/create, `""` | middleware | POST body `{keyData}` (one or more armored keys) |
| GET/DELETE | `/gpgkeys/{keyID}` | gpgkeys, get/delete, `""` | middleware | key ID or fingerprint |
| GET | `/scmproviders` | scmproviders, get, `<name>` per item | handler | |
| POST | `/scmproviders` | scmproviders, create, `<metadata.name>` | handler | |
| GET/PUT/DELETE | `/scmproviders/{name}` | scmproviders, get/update/delete, `{name}` | middleware | PUT replaces spec, labels, annotations |

Creating, updating or validating a repository that has no credentials of its own and would
inherit a credential template's, with a `spec.proxy` other than the template's or
`spec.insecure`/`insecureIgnoreHostKey`/`insecureOCIForceHTTP` the template does not set,
additionally needs `<resource>, update, <template urlPrefix>` (see "Inherited template
credentials" below).

`scmproviders` is not yet a constant of `internal/rbac` (`ResourceSCMProviders` is declared
here): its owner was asked to add it with the strict-mode action table entry and the
built-in grants (`role:readonly` get, `role:admin` create/update/delete). Until then
policies grant it explicitly and the policy loader warns that it is unknown; with the
built-in policy alone, even the local `admin` account can neither manage SCM providers nor
reference one from a repository (`TestSCMProvidersBuiltinRoles` asserts this today and
asserts full access once `internal/rbac` declares the resource).

## Request and response shapes

- `RepositoryWriteRequest` / `RepositoryCredentialWriteRequest`: the Kubernetes object form
  (`apiVersion`, `kind`, `metadata`, `spec`, ignored `status`) plus `credentials`
  (`RepositoryCredentials`: `username`, `password`, `bearerToken`, `sshPrivateKey`,
  `tlsClientCertData`, `tlsClientCertKey`, `githubAppPrivateKey`, `gcpServiceAccountKey`,
  `azureServicePrincipalClientSecret`). Unknown fields are refused. `String`/`GoString`/
  `LogValue` of `RepositoryCredentials` print `[redacted]`.
- `RepositoryInfo`: the stored Repository (managed fields and the last-applied annotation
  removed) plus `connectionState` (Unknown when never checked), `hasCredentials` (Secret,
  SCM provider or workload identity) and `inheritedCredentials` (a template of the same
  purpose matches). `RepositoryCredentialInfo` adds `hasCredentials` and `username` (listed
  upstream; not a secret). Credential values are never part of any response.
- SCM providers are returned as `SCMProvider` objects; their write request has no
  credentials field: Secrets are referenced by name only (`caSecretRef`,
  `auth.tokenSecretRef`, `auth.basicAuthSecretRef`, `auth.githubApp.privateKeySecretRef`,
  `webhook.secretRef`) and created by operators (labelled
  `cd.patrol.example/secret-type: scm-creds`).
- `RepositoryCertificate{serverName, certType, certSubType, certData, certInfo}`: `certData`
  is plain text (PEM, or the base64 SSH key), returned by create and delete, never by list.
- `GnuPGPublicKey{keyID, fingerprint, owner, trust, subType, keyData}`; `keyData` only on get.

## Behavior and invariants

- **Managed credentials Secret.** Credentials go to a Secret named `<object>-creds`,
  labelled with the object's `cd.patrol.example/secret-type` (`repository`,
  `repository-write`, `repo-creds`, `repo-write-creds`), annotated
  `cd.patrol.example/managed-by: patrol-cd` and owned (controller ownerReference) by the
  object, which also binds it to a project-scoped Repository as `internal/repository`
  requires. `spec.credentialsRef` is server-managed: empty on create, unchanged on update.
  A `credentials` field replaces every recognized key (empty values remove it) and keeps
  unknown keys; no `credentials` keeps the stored ones. A Secret the API does not manage
  (declarative setups) is never modified: its data seeds a new managed Secret and the
  reference moves to it. When no key remains the managed Secret is deleted and the
  reference cleared. A Repository is created before its Secret (the ownerReference needs the
  UID) and deleted again when the Secret cannot be written.
- **Credentials follow the object update.** An update (PUT, upsert, template PUT) first
  reads what is stored and computes the Secret change, then writes the object with the
  `credentialsRef` it will need under the `resourceVersion` it read, and only then creates,
  updates or deletes the managed Secret. An update the API server refuses (409 from a
  concurrent writer, or any other error) therefore changes no credential and never deletes a
  Secret the object still references. When the Secret cannot be written after the object
  update, the previous spec is written back (best effort, under the new `resourceVersion`;
  a newer write wins) and the error is returned. Residual: the object may reference its new
  managed Secret a moment before it exists (the repository library reports
  `SecretNotFound` meanwhile); a credentials-only update leaves the object unchanged, so its
  `resourceVersion` does not move and two concurrent credentials-only updates are
  last-writer-wins on the Secret, as are the Secret writes of two updates that both
  succeeded under consecutive `resourceVersion`s if the first one's Secret write is delayed
  past the second request.
- **Inherited template credentials.** The repository library gives a repository without
  credentials of its own (no username, password, bearer token, SSH key, TLS client
  certificate, GitHub App key or service principal secret, no workload identity, no
  SCMProvider with authentication) the longest-prefix template's credentials but keeps the
  repository's own `proxy`, `insecure` and `insecureIgnoreHostKey` (upstream
  `CopyCredentialsFrom`). Create, update and validate refuse such a repository with 403
  `permission denied: the repository would use the credentials of a credential template with
  its own spec.proxy, ...` when it sets a proxy other than the template's or an insecure flag
  the template does not set, unless the caller may update that template (it could change the
  template's settings anyway). The check runs before any connection test, so a refused
  request sends nothing; the message does not name the template; the denial is audited with
  `update` on the template prefix. Own credentials, URLs outside every template, the
  template's own proxy and no proxy at all are accepted.
- **Object names** are `repo-`, `repo-write-`, `creds-`, `creds-write-` plus the FNV-64a of
  the normalized URL and the project (REPO-028; never used for lookups). Lookups match
  `spec.url` with upstream `SameURL` and the exact project. A create for a URL and project
  already registered (under any name) is idempotent when identical (spec after defaults and
  stored credential keys), an update with `upsert`, otherwise upstream's
  `existing repository spec is different; use upsert flag to force update; difference in
  keys "..."` (400).
- **Connection checks** use `internal/repository`: `ValidateWithSecretData` for unsaved input
  (create, validate, and right after credentials were written) and `Validate` for stored
  repositories; the result is written to `Repository.status` by the `StatusWriter` (stale
  results are skipped). Unscoped read and all write repositories must pass before they are
  stored (`error testing repository connectivity: <message>`, 400); project-scoped read
  repositories are stored either way with their state recorded. Configuration errors
  report `Configuration error - please check the server logs (<Reason>)`.
- **URL passwords of declarative objects** (`spec.url`, `spec.urlPrefix`, `spec.proxy`,
  `githubApp.enterpriseBaseURL`, `azureServicePrincipal.activeDirectoryEndpoint`, SCM
  provider `apiURL`/`webURL`): the API refuses them on writes, but objects created with
  kubectl may carry them. Responses mask the password (`https://bot:******@host/...`; the
  user name and every other byte are kept), and 403 details and audit records of RBAC checks
  on such objects show the masked object. Lookups still match the stored URL (upstream
  `SameURL`), so these objects are addressed by their stored URL; fixing them declaratively
  is the remedy.
- **No existence oracle**: get, update and delete look among the repositories of the URL the
  caller may get; none is the generic 403 `permission denied` (audited with the request's
  coordinates). Several visible ones and no matching project is 404
  `repository not found for url "<u>" and project "<p>"` (only visible objects involved).
  Browsing endpoints authorize before credentials are resolved or tokens minted; a missing
  project in apps/appdetails is reported as `repository '<r>' not permitted in project
  '<p>'`.
- **Inputs**: repository URLs and prefixes: http(s), ssh, git or oci URLs with a host and no
  password, scp-style `[user@]host:path`, or (Helm/OCI) `host[:port]/path`; local paths,
  `file://`, git remote helpers (`ext::`, `fd::`), option-like values, backslashes,
  whitespace and control characters are refused (threat model T8). Proxies: http, https,
  socks5(h), no user information. GitHub Enterprise and Entra endpoints: https. Credentials
  are parsed before they are stored (TLS pair, SSH key without passphrase, RSA GitHub App
  key, Google `service_account` key), bounded (256 KiB each, 768 KiB together) and never
  echoed in errors. Projects must exist. Bodies are capped at 1 MiB.
- **Trust ConfigMaps**: read-modify-write with the read `resourceVersion` (conflicts retried
  with `retry.DefaultRetry`); created with `app.kubernetes.io/part-of: patrol-cd` when
  missing; an existing unlabelled ConfigMap (ignored by `internal/settings`) is refused with
  412. Certificate batches are validated completely before anything is written. Only
  certificate PEM blocks are stored (text around them is dropped); private keys are refused;
  TLS host names must be valid ConfigMap keys (no port, no IPv6 literal). Known-host lines
  that are comments, blank or unparsable are kept verbatim on rewrite. GnuPG keys are
  re-armored one key per entry, keyed by upper-case key ID; private key blocks are refused.
- **Informer lag**: stored repositories, templates and SCM providers are resolved by the
  `internal/repository` informers. A repository created (or validated) milliseconds after
  the template or SCM provider it relies on may fail its test with a configuration error
  (`SecretNotFound`, `SCMProviderNotFound`) until the informers caught up; retrying
  succeeds. Lookups, existence checks and RBAC decisions of the handlers read the API
  server directly and are not affected.
- **Audit**: every mutation records its target (Repository, RepositoryCredential,
  SCMProvider or ConfigMap) and a message without secrets; generic denials of reads are
  recorded too.

## Differences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| Repository Secrets hold the settings and credentials | Repository/RepositoryCredential objects plus a managed Secret | ADR-0003 |
| Request/response is Argo's flat `Repository` (`repo`, `username`, ...) | Kubernetes object form plus write-only `credentials`; responses add `hasCredentials`, `inheritedCredentials`, `connectionState` | api-contract.md conventions |
| Connection state cached in Redis/memory | recorded in `Repository.status`, re-checked after `ConnectionStateTTL` | ADR-0004, Kubernetes is the source of truth |
| Create response `{repo, type, name}` | the full sanitized object | |
| `credsOnly` unused | creates a credential template from the body | api-contract.md |
| Upsert checks only create | upsert also needs update | an upsert replaces the stored object |
| Update cannot move a repository between projects | moves it (update in both projects, target project must exist, no duplicate) | |
| Repocreds update/delete resolve the longest matching prefix | exact prefix | the request's RBAC object must be the object acted on |
| Write repocreds list: only password templates whose URL is a read template URL | every write template the caller may get | upstream quirk hides templates |
| Deleting an unmanaged Secret strips its label | the Secret is left untouched; only managed Secrets are deleted | declarative Secrets may be shared |
| `certData` base64 bytes in JSON | plain text | |
| Raw certificate input stored; IPv6 host keys accepted | only certificate blocks stored; private keys refused; host must be a ConfigMap key | no secrets in ConfigMaps; IPv6 cannot be a key |
| A corrupt TLS entry aborts delete | removed and reported as `invalid` | lets the API repair it |
| GPG delete does not normalize fingerprints; one invalid ConfigMap entry fails the list | normalized like get; invalid entries skipped (as `internal/settings` does) | |
| Proxy URLs may carry credentials; `spec.credentialsRef` n/a | refused (spec fields are not secret); server-managed | |
| `spec.scmProviderRef` n/a | needs `scmproviders, get, <name>` when set or changed | using a provider's credentials |
| Hydrator-disabled write routes: gRPC Unimplemented | 501, code `failed_precondition` | the problem code set has no `unimplemented` |
| `ValidateAccess`/`Create`/`Update` send inherited template credentials with the request's proxy and insecure flags | refused (403) unless the caller may update the template | a project-scoped `create` grant must not route an organization-wide password through a proxy of the caller's choice with TLS verification off |
| `Sanitized()` keeps the repository URL, password included | URL passwords masked in responses, denials and audit records | responses never contain credential values |
| Secret written in one step | object written first under its `resourceVersion`, managed Secret second | a 409 must not change the stored credentials |

## Security notes and residual risks

- Responses, errors, audit records and logs never contain credential values: responses are
  built from the objects, whose credential fields are references, with URL passwords of
  declaratively created objects masked; repo-server and resolver errors are redacted
  against the forwarded material before they are returned or logged.
- Template credentials and the receiving repository's transport: the API check above covers
  objects written through it. A repository created declaratively, or created through the
  API before a matching template existed, still receives a template's credentials with its
  own proxy and insecure flags (repository library behavior, see the repository
  component's residual risks). Pinning template transport in `internal/repository` (as
  SCMProvider credentials already are) is the complete fix and was requested from that
  package's owner.
- Credentials are only ever written by the handlers; callers cannot point
  `spec.credentialsRef` at an existing Secret (that would let them use another
  repository's credentials against a URL they choose).
- Granting `scmproviders, create/update` lets the caller send the referenced `scm-creds`
  Secrets to provider URLs of its choice: grant it to administrators only.
- RBAC objects of unscoped repositories are their URL, so a scheme-less registry URL whose
  first segment equals a project name (`registry.example.com/charts` vs a project named
  `registry.example.com`) is matched by that project's `<project>/*` policies (upstream
  object scheme). Prefer `oci://` URLs.
- Unregistered URLs can be browsed by callers with `repositories, get, <url>`; credential
  templates whose prefix matches are applied (upstream behavior; see the repository
  component's residual risks about prefixes that end inside a host name).
- `forceRefresh` lists trigger one repo-server check per visible repository (bounded to
  `MaxParallelChecks` at a time and `ConnectionTimeout` each).

## Gaps

- Not wired into `cmd/patrolcd` (no server registration, no RBAC manifest update for the
  server service account).
- `scmproviders` RBAC constant, strict-mode actions and built-in grants pending in
  `internal/rbac` (requested; needed: `ResourceSCMProviders = "scmproviders"` in the resource
  list, `crudActions` in the strict table, `p, role:readonly, scmproviders, get, *, allow` and
  `p, role:admin, scmproviders, create|update|delete, *, allow` in `builtin-policy.csv`). Until
  then the built-in roles cannot use the SCM provider routes or `spec.scmProviderRef`.
- Template transport pinning for declarative repositories belongs to `internal/repository`
  (requested).
- No deprecated alias routes (`List`, `Create`, `Update`, `Delete` share paths upstream).
- No periodic re-check of repositories (a reconciler's job); states are refreshed on reads.
- Verified only against a fake repo-server; no real git/Helm/OCI servers.

## Parity IDs covered

See `docs/parity/status/api-repositories.yaml`: API-009 (repository paths), API-029,
API-031, API-036, API-037, API-038, API-039, REPO-027, REPO-028, REPO-035..043, REPO-045,
REPO-046, REPO-059..061, REPO-065 (implemented).

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0), after the review
follow-up (credentials ordering, inherited template transport, URL password masking, SCM
provider built-in roles):

```bash
go build ./...                                                                   # ok
go vet ./internal/apiserver/services/repositories/... ./pkg/apiclient/...        # ok
golangci-lint run ./internal/apiserver/services/repositories/... ./pkg/apiclient/...  # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=3 ./internal/apiserver/services/repositories/ ./pkg/apiclient/
# ok, ok; with -count=1 -v: 41 top-level tests, 118 tests/subtests pass, none skipped
go run ./hack/parity && go run ./hack/parity -check                              # ok, ok
```

Full output: [`docs/verification/runs/2026-10-16-api-repositories-review.txt`](../../verification/runs/2026-10-16-api-repositories-review.txt).
The regression tests `TestCredentialsFollowObjectUpdate`, `TestInheritedTemplateTransport`
and `TestDeclarativeURLPasswordsMasked` were also run against the handlers as they were
before the follow-up (`go test -overlay` mapping the previous files from the git index):
all three fail there (`expected "old-pass", actual "new-pass"` after a 409; the template
password sent with the caller's proxy; the declarative URL password in the list response).

`internal/apiserver` `TestOpenAPIUpToDate` fails until `api/openapi/openapi.json` is
regenerated with the new routes (regeneration is done once every service has landed).

Tests: route table with zero dependencies (every contract route present, none public, 503
when not configured); OpenAPI generation of the area's types with the CRD schemas (no
credential field in response schemas); validators (URLs, proxies, endpoints, credentials,
specs, SCM providers), naming, spec diffs, known-hosts round trips, certificate selectors
and batches, history matching, GnuPG splitting, error redaction; client request building
(path escaping, queries, bodies). Through `pkg/apiclient` against envtest with the real
`rbac.Enforcer`, `auth.TokenService`, `repository.Lister`/`Resolver`/`StatusWriter` and a fake
repo-server: lifecycle (managed Secret labels, annotation and owner, sanitized responses,
encoded and malformed paths, idempotent and differing creates, upsert, 409 on a stale
resourceVersion, credential replacement keeping unknown keys, server-managed
credentialsRef, forced and TTL-driven refresh recorded in status, delete with the Secret),
unreachable unscoped versus scoped creates with redaction, input rejection table, project
isolation (per-item lists, ambiguous URL, identical 403 for forbidden and missing,
project-role token confinement, moves between and into projects), unauthenticated and
anonymous callers with and without `policy.default`, validate without saving and with
template credentials, write repositories (501 gating, mandatory credentials, separate RBAC
and lists), SCM provider references, `credsOnly`, credential templates (read and write,
exact-prefix delete, upstream repository-URL quirk), browsing (server-side credentials,
other tenants, SSH key refs, repo-server errors redacted and outages as 503), apps and app
details authorization (create/update gate, sourceRepos and scoped repositories, project
mismatch, history, new Applications), certificates and GnuPG keys (filters, brackets,
duplicates, upsert, private keys, unlabelled ConfigMap, comments kept, deletes), SCM
providers (validation, unknown credential fields refused, per-name RBAC, concurrency,
built-in roles), credentials ordering under injected conflicts and Secret write failures
(repositories, upserts, templates), inherited template transport (proxy and each insecure
flag on validate/create/update, own credentials, username-only, outside URLs, template's own
proxy, credential removal, template updaters), URL password masking of declarative
Repository, RepositoryCredential and SCMProvider objects in lists, gets, denials and audit
records.
