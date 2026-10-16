# Component: `internal/repository`

Status: implemented (library only; not yet wired into the controller, the API server or the
hydrator). Decision records: ADR-0003 (resource model), ADR-0006 (internal APIs: the
repo-server and commit-server never read Secrets), ADR-0010 (SCM provider contract).
Upstream baseline: Argo CD v3.5.5 (`util/db` repository and credential lookup,
`pkg/apis/application/v1alpha1/repository_types.go` `GetGitCreds`/`GetHelmCreds`/
`GetOCICreds`/`HasCredentials`/`CopyCredentialsFrom`, `util/git/creds.go` GitHub App, Google
and Azure credentials, `util/helm/creds.go` ACR workload identity, `server/repository`
connection state).

Resolves how a component reaches a repository: which `Repository`, `RepositoryCredential` and
`SCMProvider` objects apply to a URL, which Secret keys hold the credentials, and which
short-lived tokens must be minted. The result is an `apiclient.Repository` that the
controller, the API server and the hydrator forward to the repo-server (and, for write
access, to the commit-server) with each request. Those servers never see Kubernetes Secrets
or GitHub App private keys.

## Responsibilities

- Informer-backed `Lister` over Repository, RepositoryCredential and SCMProvider objects in
  the control-plane namespace and the Secrets they reference (only Secrets labeled
  `cd.patrol.example/secret-type` = `repository`, `repository-write`, `repo-creds`,
  `repo-write-creds` or `scm-creds`). Objects are decoded once at ingestion; Secret
  annotations and managed fields are dropped from the cache.
- Secret references are checked before use, because upstream keeps credentials inside the
  repository Secret itself while here objects name Secrets: the referenced Secret's
  `cd.patrol.example/secret-type` must match the referencing kind and purpose (Repository:
  `repository` / `repository-write`; RepositoryCredential: `repo-creds` /
  `repo-write-creds`; SCMProvider key, token, basic auth and CA: `scm-creds`), and a Secret
  referenced by a project-scoped Repository must have an ownerReference
  (`cd.patrol.example/v1alpha1`, kind `Repository`, matching UID) to that Repository or to
  another Repository of the same project. Unsaved input (`ValidateWithSecretData`) brings
  its own data and dereferences no Secret.
- Lookup precedence (REPO-004/029/031/033/034), exactly as upstream:
  1. a Repository of the requested purpose at the same location (`SameURL`: normalized URL,
     raw URL when it cannot be normalized) whose `spec.project` is the requested project;
  2. otherwise one without `spec.project` (a Repository scoped to another project is never
     used);
  3. otherwise a stub for the URL;
  4. a repository without credentials of its own (`HasCredentials`, which ignores a Google
     key) inherits the RepositoryCredential of the same purpose whose normalized `urlPrefix`
     is the longest plain string prefix of the normalized URL (ties: first by name, logged);
     fields are copied with upstream `CopyCredentialsFrom` rules.
  Read and write objects are separate: a read Secret never grants push.
- Type selection: the query's type hint (helm for chart sources, oci for `oci://`, git
  otherwise; a git hint keeps a configured `oci`), else the configured type, else `oci` for
  `oci://` URLs, else git. Per type only the settings that type honors are forwarded (git:
  `insecure` is upstream `IsInsecure`, LFS, depth, host key; Helm: `insecure`, `enableOCI`
  or a scheme-less registry URL; OCI: `insecure`, `insecureOCIForceHTTP`).
- Credential method selection in upstream `GetGitCreds` order (REPO-026): password or bearer
  token → SSH key → GitHub App → Google service account key → Azure workload identity →
  Azure service principal → anonymous. Helm: Azure workload identity (ACR) or basic; OCI:
  basic. Only the selected method's material is forwarded.
- Short-lived tokens, cached until five minutes (at most half the lifetime) before expiry,
  concurrent mints coalesced, `CredentialsExpireAt` set on the result:
  - GitHub App installation tokens through the `pkg/scm/github` `CredentialSource`, from the
    Repository/RepositoryCredential `githubApp` (+ `githubAppPrivateKey` Secret key) or from
    an SCMProvider `auth.githubApp`; the installation is discovered from the organization in
    the repository URL when `installationID` is unset (REPO-015/016/017). Git user name
    `x-access-token`.
  - Google service account keys → OAuth access token, scope
    `https://www.googleapis.com/auth/cloud-platform`, user name `client_email` (REPO-018).
  - Azure workload identity → Entra token for the Azure DevOps scope, user name
    `00000000-0000-0000-0000-000000000000`, token as password and bearer token; for Helm OCI
    registries the ACR challenge/exchange flow with an ARM token for
    `AZURE_ARM_TOKEN_RESOURCE` (REPO-019).
  - Azure service principal → client secret credential (custom AD endpoint disables
    instance discovery), reused for at most 59 minutes (REPO-020).
- Helm dependency lists for `ManifestRequest.Repos` / `HelmRepoCreds` (REPO-056).
- Connection testing through the repo-server `TestRepository` and recording of
  `Repository.status.connectionState` plus the `Ready`/`ConnectionFailed` conditions
  (REPO-040/043/044).

Out of scope: the git/Helm/OCI transports (`internal/git`, `internal/helm`, `internal/oci`
in the repo-server), TLS certificates and SSH known hosts (`internal/settings`), the
RepositoryService API and CLI, the reconciler that periodically validates Repositories, and
project membership of scoped repositories (`internal/project`, fed by
`Lister.GetProjectRepositoryURLs`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `NewLister(dyn, kube, ListerOptions)`, `(*Lister).Start(ctx)`, `HasSynced`, `Namespace` | Informers over the control-plane namespace; lookups return `ErrNotSynced` until `Start` returns. |
| `Lister.Repositories(purpose)`, `Repository(name)`, `RepositoryCredentials(purpose)`, `SCMProvider(name)` | Deep copies, ordered by name. |
| `Lister.GetProjectRepositoryURLs(ctx, project)` | Implements `project.ScopedRepositoryLister`. |
| `NewResolver(lister, Options)` | `Options`: logger, Prometheus registerer, clock, base transport, repo-server client, Azure workload identity provider, ARM resource, service principal cache TTL, refresh margin, cache size, mint timeout, user agent, and the token endpoint policy (`GoogleTokenHosts`, `AzureAuthorityHosts`, `AzureContainerRegistryHosts`, `AzureDevOpsHosts`, `DeniedTokenNetworks`, `AllowLoopbackTokenEndpoints`). |
| `Resolver.ResolveForURL(ctx, url, project, purpose)`, `Resolve(ctx, Query)` → `*Resolution` | URL lookup. `Resolution` says which object matched (`Source`, `RepositoryName`, `CredentialName`, `Inherited`) and the `Method`; its `LogValue` has no secrets. |
| `Resolver.GetRepository`, `GetWriteRepository`, `ListHelmRepositories`, `ListDependencyRepositories(ctx, proj, includeOCI)` | Adapters matching the hydrator's `RepositoryResolver`, `WriteCredentialResolver` and `HelmRepositoryLister`. `GetWriteRepository` returns a credential-less stub (never nil) when nothing matches. |
| `Resolver.ResolveRepository(ctx, repo, inline, typeHint)` | Resolve one Repository object (its own credentials or its template), optionally with unsaved Secret data. |
| `Resolver.ReposForProject(ctx, proj, includeOCI)`, `HelmCredentialTemplates(ctx, proj, includeOCI)` | Named Helm repositories (plus OCI repositories) and Helm (plus OCI) templates permitted by the project's `sourceRepos`. |
| `Resolver.Validate(ctx, repo)`, `ValidateWithSecretData(ctx, repo, data)`, `TestRepository(ctx, repo)` | Connection test → `v1alpha1.ConnectionState` (upstream messages) and the error. |
| `NewStatusWriter(dyn, opts...)`, `(*StatusWriter).WriteConnectionState(ctx, validatedRepo, state, cause)`, `ErrStaleConnectionState` | Status subresource update with conflict/transient retries; skips no-op writes; refuses results for another generation/UID or older than the recorded `attemptedAt` (one-second precision). |
| `ErrNotSynced`, `ErrSecretNotFound`, `ErrSecretKeyMissing`, `ErrSCMProviderNotFound`, `ErrInvalidConfiguration`, `ErrInvalidCredentials`, `ErrTokenUnavailable`, `ErrRepositoryNotFound`, `ErrNoRepoServer`, `IsConfigurationError`, `ConditionReason` | Error classification; `ConditionReason` maps to the `v1alpha1.Reason*` constants. |
| `AzureTokenProvider` | Injectable workload identity token source (default: azidentity `WorkloadIdentityCredential`, created on first use). |

### Caller integration

- **Controller (manifest generation):** `repo, err := r.GetRepository(ctx, src.RepoURL,
  proj.Name)` (use `Resolve` with `Type: helm` for chart sources); `repos, creds, err :=
  r.ListDependencyRepositories(ctx, effectiveProj, src.IsOCI())`.
- **Hydrator:** pass the Resolver as `Repositories`, `WriteCredentials` and
  `HelmRepositories`.
- **API server / Repository reconciler:** `state, err := r.Validate(ctx, repo)` then
  `w.WriteConnectionState(ctx, repo, state, err)` with the same `repo` object;
  `ErrStaleConnectionState` means the object changed (or a later check was recorded) while
  the check ran and nothing was written. For "validate access" of unsaved input use
  `ValidateWithSecretData`.
- **API server, creating a project-scoped Repository with credentials:** create the
  Repository, then its Secret with an ownerReference to it (also garbage-collects the Secret).
- **Process wiring:** set `Options.DeniedTokenNetworks` to the cluster's pod and service
  CIDRs; extend the endpoint allowlists only for private endpoints that are trusted.
- Re-resolve for every request: tokens are cached here, and `CredentialsExpireAt` tells the
  repo-server how long credential-keyed caches may live.

## Behavior and invariants

- Kubernetes is the source of truth: the informer caches and the token cache are
  discardable; nothing is persisted outside the API server.
- Lookups are deterministic (objects ordered by name) where upstream depends on informer
  order.
- Token cache keys are SHA-256 digests of the credential identity (App ID, installation or
  owner, API URL, key; Google key; tenant/client/secret/endpoint; registry host); values are
  never logged and `mintedToken` formatting redacts them. A mint runs detached from the
  caller's cancellation, bounded by `MintTimeout`, so one cancelled caller does not fail the
  others waiting on it. Errors are not cached.
- Metrics (registry passed in, bounded labels, no URLs or names):
  `patrol_repository_resolutions_total{purpose,source,method}`,
  `patrol_repository_resolution_errors_total{purpose,reason}`,
  `patrol_repository_token_requests_total{kind,result}`,
  `patrol_repository_token_mint_duration_seconds{kind}`,
  `patrol_repository_validations_total{result}`.

## Differences from upstream (by design)

| Upstream | Patrol |
|----------|--------|
| A GitHub App ID without a private key, or Azure service principal IDs without a client secret, silently fall through to anonymous access. | Configuration error (`SecretKeyMissing`). The CRD makes the IDs explicit fields, so the missing key is a mistake. |
| Google `external_account` credentials are accepted. | Rejected (`InvalidCredentials`): their `credential_source` can make the minting component (controller, API server, which hold Kubernetes credentials) read local files, run executables or call arbitrary URLs. A malformed service account key is reported as `InvalidCredentials` at resolution time instead of failing later. |
| The ACR exchange runs for any registry host with any realm, with the repository's `insecure` flag, in the repo-server. | It runs in the controller/API server with their identity, so the registry host must match `AzureContainerRegistryHosts` (default `*.azurecr.io`, `*.azurecr.cn`, `*.azurecr.us`), TLS is always verified and the realm must be on the registry host. The realm check alone does not protect a registry host the attacker owns; the allowlist does (also against a plain-prefix template `myacr.azurecr.io` matching `myacr.azurecr.io.attacker.com`). |
| The workload identity's Azure DevOps token is sent to any git URL. | Only to https URLs on `AzureDevOpsHosts` (default `dev.azure.com`, `*.visualstudio.com`). |
| Google `token_uri` / impersonation URLs, the Entra authority (`azureActiveDirectoryEndpoint`; here `azureServicePrincipal.activeDirectoryEndpoint`) and the GitHub Enterprise URL are used as written; issuer error bodies appear in messages. | Google URLs must be https on Google's hosts (or `GoogleTokenHosts`); the Entra authority must be https on a public/US Gov/China cloud authority (or `AzureAuthorityHosts`); GitHub App API URLs must be https. Every token request is https-only (redirects included) and never connects to link-local, unspecified, multicast, loopback (unless `AllowLoopbackTokenEndpoints`) or `DeniedTokenNetworks` addresses (checked on the dialed address, so DNS rebinding and proxies are covered). Errors keep only the HTTP status and a short error code (`invalid_grant`); a disallowed endpoint is `InvalidConfiguration`. |
| A credential template's GitHub App key is used with the repository's `githubAppEnterpriseBaseURL`, proxy and TLS flags when they are set (`CopyCredentialsFrom` fills only empty fields). | An inherited key brings its template's API URL, proxy and TLS settings; the inheriting repository's are ignored for the JWT requests. |
| Helm dependency lists include repositories scoped to other projects. | Excluded; when a URL is registered both scoped to the project and unscoped, the scoped one is used. |
| An SCMProvider reference does not exist. | SCMProvider credentials (GitHub App, token, basic auth) fill only empty fields and the provider, not the referencing object, decides where they go: only to repositories on the provider's host (`webURL`, else the API host without `api.`, else the provider's public host), only over https (http only when the provider's web/API URL is http), never with the object's `insecure`/`insecureIgnoreHostKey`/`insecureOCIForceHTTP` unless the provider sets `insecure`, and through the object's proxy only over verified https. The App's JWTs always go to the provider's API URL with its CA and `insecure`; the object's `githubApp.enterpriseBaseURL` must be empty or equal to it. The same applies when the provider's credentials are inherited through a template. |
| Credentials live in the repository Secret, so a referencing object cannot borrow another's. | Secret references are checked by `secret-type` per kind and purpose, and project-scoped Repositories need Secrets bound to their project by ownerReference (see Responsibilities). |
| Connection state lives in a server cache. | Persisted in `Repository.status`; stale results (spec changed, object recreated, older check) are not written. |
| Credential template ties are logged at debug level, in informer order. | First by name, logged at warning level. |
| Installation discovery results are cached 60 minutes separately from tokens. | The installation is resolved with each token mint (cached together with the token, keyed by owner). |

## Failure modes

| Condition | Behavior |
|-----------|----------|
| Lookup before `Start` completed | `ErrNotSynced`. |
| Referenced Secret missing or unlabeled | `ErrSecretNotFound` naming the Secret and the required label (`Configuration error - please check the server logs` in `connectionState`). |
| Required key missing (GitHub App key, SP secret, SCMProvider token/password) | `ErrSecretKeyMissing`. |
| `scmProviderRef` names nothing | `ErrSCMProviderNotFound`. |
| SCMProvider credentials for another host, over http, with object-level insecure flags or an unverified proxy; `enterpriseBaseURL` differing from the provider's API; App ID mismatch; invalid URL; unknown purpose | `ErrInvalidConfiguration`. |
| Secret with the wrong `secret-type` for the reference, or not bound to the project of a project-scoped Repository | `ErrInvalidConfiguration` naming the Secret, its label value and the required one. |
| Token endpoint outside the policy (Google/Entra host, non-https, GitHub App API over http, refused address, ACR or Azure DevOps host not allowed) | `ErrInvalidConfiguration`; no request is sent (the dial guard refuses the connection before any byte). |
| Unparsable private key, Google key, client certificate | `ErrInvalidCredentials`. |
| GitHub, Google, Entra ID or ACR refuses or is unreachable | `ErrTokenUnavailable` (`Unable to connect to repository: ...` with the HTTP status and error code only, never the response body). |
| URL longer than 4096 bytes or empty | `ErrInvalidConfiguration`. |
| Unsafe proxy URL on GitHub App / service principal token requests | Dropped with a security warning (`security=3`, `CWE=78`), as upstream; still forwarded to the repo-server, which validates it. |
| Status update conflict or transient API error | Re-read and retried with backoff (`DefaultStatusBackoff`). |
| Status write for a generation/UID that is no longer stored, or older than the recorded check | Skipped, `ErrStaleConnectionState`. |

## Security notes

- Secret values (passwords, tokens, keys, minted tokens) never appear in logs, metrics or
  error messages: errors carrying merged credential material are redacted (`******`), URLs
  are logged through `git.SanitizeURL`, and the repo-server's `TestRepository` error is
  redacted against the forwarded material.
- Inputs from repository objects are bounded (URL length, registry response bodies 1 MiB,
  condition messages 32 KiB, token cache size).
- The repo-server receives only the material of the selected method, never the GitHub App
  private key, the Google key or the service principal secret.
- Token requests run in components that hold Kubernetes credentials, so their destinations
  are bounded by the endpoint policy (see Differences) and their responses are never
  reflected beyond a status code and short error code.
- Residual risks (upstream behavior, kept for parity): credential templates match by plain
  string prefix, so a template prefix that ends inside a host name (`https://git.example`)
  also matches `https://git.example.attacker.com/...` and its password, bearer token or
  minted installation token is forwarded there; template credentials are forwarded with the
  inheriting repository's proxy and TLS flags (only SCMProvider credentials and App JWTs are
  pinned); project-scoped repositories inherit global templates. End every `urlPrefix` with
  `/`. A Repository's own GitHub App URL may name any https host not refused by the dial
  guard (blind requests to private ranges are possible unless `DeniedTokenNetworks` covers
  them). `pkg/scm/github` caches installation tokens process-wide by API URL, App ID and key,
  independent of proxy settings.
