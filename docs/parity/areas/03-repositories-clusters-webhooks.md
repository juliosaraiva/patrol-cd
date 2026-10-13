# Repositories, credentials, clusters and webhooks — parity inventory (Argo CD v3.5.5)

Scope: this area covers how Argo CD v3.5.5 (commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`) stores, resolves, validates and exposes **repository connection settings** and **credentials**. That includes repository Secrets, credential templates (`repo-creds`), the write credentials used by the source hydrator (`repository-write` / `repo-write-creds`), every field of `v1alpha1.Repository` / `v1alpha1.RepoCreds`, Git transport settings (proxies, the `askpass` credential injection, built-in git config, retries), SSH known hosts (`argocd-ssh-known-hosts-cm`), TLS certificates (`argocd-tls-certs-cm`), the GnuPG keyring (`argocd-gpg-keys-cm`), and GPG signature verification / source integrity (`AppProject.spec.sourceIntegrity` and the deprecated `signatureKeys`). It also covers **clusters**: cluster Secrets, every `ClusterConfig` field, how auth precedence is resolved, the in-cluster cluster and `cluster.inClusterEnabled`, cluster info, connection and cache state, `argocd-k8s-auth` (aws/gcp/azure), `argocd cluster add` service-account bootstrapping, `rotate-auth`, and the admin cluster tooling. Finally it covers the **API-server webhook endpoint** `/api/webhook`: GitHub, GitLab, Bitbucket Cloud, Bitbucket Server, Gogs, Azure DevOps and the GHCR registry. For each provider it records the secrets, signature verification, which events refresh apps, how apps are matched, path filtering, jitter, payload limits and cache warming.

Out of scope, covered by other areas: manifest generation per tool (Helm/Kustomize/OCI rendering, chart dependency resolution internals), the ApplicationSet webhook and SCM-provider credentials (`scm-creds`), controller sharding algorithms, the application destination validation pipeline beyond cluster lookup, the source-hydrator commit flow (only its credential lookup is covered here), generic RBAC policy syntax, and the AppProject CRUD API (only the source-integrity/signature-key fields and their CLI are covered here).

Permalink base: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.

---

## A. Repository Secrets and the `Repository` data model

### REPO-001 Repository Secret declarative storage
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L28 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L26 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1461 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L198 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L173 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-repositories.yaml#L1
- **Maturity**: GA
- **Semantics**: Each repository is a Kubernetes `Secret` in the Argo CD control-plane namespace, labelled `argocd.argoproj.io/secret-type: repository`. Secrets are discovered through a shared secrets informer that watches every Secret whose `secret-type` label is not `cluster`. It uses a 3-minute resync and does **not** require the `app.kubernetes.io/part-of: argocd` label, unlike ConfigMaps. Field values are read from `data`; `stringData` works because Kubernetes merges it into `data`. The secret's name is irrelevant for lookup; matching is by the `url` key, compared with `git.SameURL`. Every write through the API calls `ResyncInformers()`, so later reads see the change immediately.
- **Edge cases**: Secrets in any other namespace are ignored. Secrets without the label are invisible. An `argocd.argoproj.io/secret-type: repository` secret with no `url` key still parses but matches nothing. If two secrets share the same normalized URL and project, the first match wins and the log reports `Found multiple credentials for repoURL: <url>`.
- **Configuration surface**: Label `argocd.argoproj.io/secret-type=repository`. Data keys: `url`, `name`, `type`, `project`, `username`, `password`, `bearerToken`, `sshPrivateKey`, `tlsClientCertData`, `tlsClientCertKey`, `insecure`, `insecureIgnoreHostKey`, `enableLfs`, `enableOCI`, `insecureOCIForceHttp`, `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppEnterpriseBaseUrl`, `gcpServiceAccountKey`, `proxy`, `noProxy`, `forceHttpBasicAuth`, `useAzureWorkloadIdentity`, `azureServicePrincipalClientId`, `azureServicePrincipalClientSecret`, `azureServicePrincipalTenantId`, `azureActiveDirectoryEndpoint`, `depth`, `webhookManifestCacheWarmDisabled`.
- **Exposure**: Declarative Secret. The API (RepositoryService), the CLI (`argocd repo`) and the UI (Settings > Repositories) all read and write the same Secrets.
- **Tests required**:
  - Positive: a labelled Secret created with `kubectl apply` appears in `GET /api/v1/repositories` without a restart.
  - Negative: the same Secret without the label, or in another namespace, is not listed.
  - Negative: a Secret labelled `cluster` is never treated as a repository.
  - Recovery: after the Secret is deleted out-of-band, the repo disappears from list and get once the informer catches up.

### REPO-002 Repository `type` (git / helm / oci)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L94 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L424 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L70 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3145
- **Maturity**: GA
- **Semantics**: `type` is `git` (the default when empty), `helm` or `oci`. `Normalize()` sets `git` when empty, and API list/get responses always carry a non-empty type. The type selects the connectivity check in REPO-044 and which credentials object is built: `GetGitCreds`, `GetHelmCreds` or `GetOCICreds`. `ListHelmRepositories` returns only `type=helm` repos with a non-empty `name`. `ListOCIRepositories` returns `type=oci` repos.
- **Edge cases**: Any other type fails `TestRepository` with `unsupported repository type "<t>"`. The `type` key is matched case-sensitively, except in `GetAllHelmRepoCreds` / `GetAllOCIRepoCreds`, which use `strings.EqualFold` on repo-creds.
- **Configuration surface**: Secret key `type`. CLI `--type` (default `git`) on `repo add`, `repocreds add` and `admin repo generate-spec`.
- **Exposure**: Secret / API (`Repository.type`) / CLI / UI (type selector in "Connect repo" via HTTPS).
- **Tests required**:
  - An empty type reads back as `git`.
  - A `helm` repo without a name is excluded from the Helm repository list.
  - An unknown type returns an error from validate-access.
  - Type matching on repo-creds is case-insensitive for helm and oci.

### REPO-003 Repository `name`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L95 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/helmrepository.go#L11 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repo.go#L207
- **Maturity**: GA
- **Semantics**: A free-form display name, used mainly for Helm repositories; it is the alias Helm dependency resolution uses. Only Helm repos with a non-empty name are passed to the repo-server as known Helm repos.
- **Edge cases**: `argocd repo add --type helm` without `--name` fails client-side with `Must specify --name for repos of type 'helm'`. The server does not enforce this.
- **Configuration surface**: Secret key `name`. CLI `--name`.
- **Exposure**: Secret / API / CLI / UI (Name field for Helm).
- **Tests required**:
  - The CLI rejects a Helm repo without a name.
  - An API create of a Helm repo without a name succeeds but the repo is excluded from the Helm repo list.

### REPO-004 Project-scoped repositories (`project`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L112 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L564 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L258 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L763 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L87 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L320
- **Maturity**: GA
- **Semantics**: The `project` key binds a repository Secret to one AppProject.
  - **Lookup** (`getRepositorySecret`): for `(url, project)` the first Secret whose URL matches with `SameURL` **and** whose `project` equals the requested project wins. If none matches and fallback is allowed, an unscoped Secret (empty `project`) with the same URL is used. Read paths such as `GetRepository` and `RepositoryExists` allow the fallback; create, update and delete do not.
  - **Project membership**: `GetAppProjectByName` appends every project-scoped repo URL to the project's effective `sourceRepos`, using an informer index on `data.project`.
  - **RBAC**: the object is `<project>/<repoURL>` when the project is set, otherwise `<repoURL>`.
  - **Coexistence**: the same URL can exist both scoped and unscoped, or in several projects, because the secret name hash includes the project.
- **Edge cases**:
  - An app in project A cannot use a repo scoped to project B; it falls back to an unscoped secret or to no credentials.
  - ApplicationSets that template `project` can only use unscoped repos (per the docs).
  - `CreateRepository` skips the connectivity test when `project` is set (see REPO-037).
- **Configuration surface**: Secret key `project`. CLI `--project` on `repo add`, `repo get` and `repo rm`. API `RepoQuery.appProject`.
- **Exposure**: Secret / API / CLI / UI ("Project" dropdown when connecting a repo).
- **Tests required**:
  - Scoped and unscoped secrets with the same URL: a get with `appProject=X` returns the scoped one; a get without a project returns the unscoped one.
  - With only a scoped secret, a lookup for another project falls back to no credentials.
  - The scoped repo appears in the project's effective sourceRepos.
  - RBAC `repositories, create, my-project/*` allows creating a scoped repo but denies an unscoped one.
  - Deleting the scoped repo leaves the unscoped secret intact.

### REPO-005 HTTPS username/password authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L280 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L268 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L465 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L13
- **Maturity**: GA
- **Semantics**: If `password` or `bearerToken` is non-empty, Git uses `HTTPSCreds`.
  - **git CLI**: the username and password are registered in the askpass store (REPO-051). The username defaults to `x-access-token` when empty.
  - **go-git HTTP** (used by `ls-remote` / refs): `BasicAuth{username or "x-access-token", password}`.
  - **Helm and OCI**: the username and password are passed through directly.
  - **Tokens**: personal access tokens are used as the password with any non-empty username. For Bitbucket Cloud and Data Center the docs say to use username `x-token-auth`.
- **Edge cases**:
  - The CLI prompts for a password when `--username` is set without `--password`.
  - Setting a username without a password is not treated as credentials for HTTPS creds selection, although `HasCredentials()` is true when only a username is set.
  - Git hosts that redirect (for example GitLab's `.git` 301) are not followed.
- **Configuration surface**: Secret keys `username`, `password`. CLI `--username`, `--password`.
- **Exposure**: Secret / API / CLI / UI (HTTPS method).
- **Tests required**:
  - A private repo validates with a correct username and password.
  - A wrong password fails with `authentication required`.
  - An empty username falls back to `x-access-token`.
  - Credentials never appear in API responses (they are sanitized).

### REPO-006 Bearer token authentication (Bitbucket Data Center)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L121 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L327 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1787 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/common.go#L12
- **Maturity**: GA
- **Semantics**:
  - **git CLI**: the header `Authorization: Bearer <token>` goes into env `ARGOCD_GIT_BEARER_AUTH_HEADER`, and git runs with `--config-env http.extraHeader=ARGOCD_GIT_BEARER_AUTH_HEADER`.
  - **go-git**: `TokenAuth{token}`.
  - **Password + forceHttpBasicAuth**: the basic header takes precedence over the bearer header in the git CLI environment, but go-git still prefers the bearer token.
  - **Other uses**: the Bitbucket Cloud webhook diffstat callback also uses the bearer token (REPO-119).
- **Edge cases**: The CLI rejects `--bearer-token` combined with `--password` (`only --bearer-token or --password is allowed, not both`), with a non-git type (`--bearer-token is only supported for Git repositories`), and with a non-HTTPS URL (`--bearer-token is only supported for HTTPS repositories`). Declarative Secrets are not validated, so both fields can be set.
- **Configuration surface**: Secret key `bearerToken`. CLI `--bearer-token` (repo add, repocreds add, admin repo generate-spec).
- **Exposure**: Secret / API / CLI / UI (HTTPS form).
- **Tests required**:
  - The CLI rejects each of the three invalid combinations.
  - A declarative Secret with a bearer token causes the git CLI to send the bearer header.
  - The go-git ls-remote path sends `Authorization: Bearer`.

### REPO-007 `forceHttpBasicAuth`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L116 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L325 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1784
- **Maturity**: GA
- **Semantics**: When this is true **and** a password is set, the git CLI gets `ARGOCD_GIT_AUTH_HEADER=Authorization: Basic base64(user:pass)` plus `--config-env http.extraHeader=ARGOCD_GIT_AUTH_HEADER`. This skips the server's auth-scheme negotiation. Repo-creds templates propagate it: `CopyCredentialsFrom` always overwrites it from the template.
- **Edge cases**: It has no effect without a password. It does not affect the go-git `ls-remote`, which always uses BasicAuth anyway.
- **Configuration surface**: Secret key `forceHttpBasicAuth` (a bool string). CLI `--force-http-basic-auth`.
- **Exposure**: Secret / API / CLI / UI (HTTPS checkbox).
- **Tests required**:
  - With the flag set, fetch sends the basic header without first receiving a 401.
  - The flag is ignored when the password is empty.
  - An invalid bool such as `"yes"` is a configuration error (REPO-027).

### REPO-008 SSH private key authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L361 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L403 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L409 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/ssh.go#L14 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L74
- **Maturity**: GA
- **Semantics**: It is used when `sshPrivateKey` is set and neither `password` nor `bearerToken` is set.
  - **git CLI**: the key is written (with a trailing newline) to a temp file under the shared-memory temp dir, and git runs with `GIT_SSH_COMMAND="ssh -i <file> -o StrictHostKeyChecking=yes -o UserKnownHostsFile=<known_hosts>"`. In insecure mode it uses `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null`.
  - **go-git**: authenticates with the parsed key, checks host keys against Argo CD's `ssh_known_hosts`, and populates host key algorithms from that file.
  - **Key exchange**: algorithms are limited to curve25519, ecdh-sha2-nistp* and dh-group-exchange/group14. Under FIPS mode only the FIPS-compliant subset is used.
  - **SSH user**: taken from the URL (`git@...`).
  - **URL forms**: scp-style `git@host:path` cannot carry a port, so non-default ports need `ssh://` URLs.
- **Edge cases**:
  - The CLI `--ssh-private-key-path` is accepted only for SSH URLs (`--ssh-private-key-path is only supported for SSH repositories.`).
  - An unparseable key fails at connect time.
  - The temp key file is removed after every command.
  - An SSH URL with no credentials uses the ssh-agent with Argo CD's known hosts, or go-git's default when that fails.
- **Configuration surface**: Secret key `sshPrivateKey`. CLI `--ssh-private-key-path`.
- **Exposure**: Secret / API / CLI / UI (SSH method).
- **Tests required**:
  - SSH clone succeeds when the host is in known hosts and fails when it is not.
  - Clone succeeds with `insecure` even when the host is unknown.
  - A non-default port works with an `ssh://` URL.
  - No key material remains on disk after an operation.

### REPO-009 TLS client certificate authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L89 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L282 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L338 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L56
- **Maturity**: GA
- **Semantics**: PEM cert and key data. A client cert is used only when **both** the cert and the key are non-empty.
  - **git CLI**: written to temp files and passed as `GIT_SSL_CERT` / `GIT_SSL_KEY`.
  - **go-git**: supplied via `GetClientCertificate`.
  - **Helm and OCI**: passed as `CertData` / `KeyData`.
  - **Other credential types**: also honoured by GitHub App creds and Azure Service Principal creds.
  - **Format limits**: only PEM, and the key must not be password-protected.
- **Edge cases**:
  - The CLI requires both paths together (`--tls-client-cert-path and --tls-client-cert-key-path must be specified together`) and only for HTTPS URLs.
  - A cert alone, without a password, does **not** select HTTPS creds; `GetGitCreds` returns NopCreds unless another credential exists. Even so, `HasCredentials()` is true, so repo-creds are not inherited.
- **Configuration surface**: Secret keys `tlsClientCertData`, `tlsClientCertKey`. CLI `--tls-client-cert-path`, `--tls-client-cert-key-path`.
- **Exposure**: Secret / API / CLI / UI (HTTPS and GitHub App forms).
- **Tests required**:
  - mTLS-protected git and Helm endpoints succeed with the cert.
  - They fail without it.
  - The CLI rejects a single path.
  - A cert-only repo does not inherit repo-creds.

### REPO-010 `insecure` (skip TLS verification / SSH host key check)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L142 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L275 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L432 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L367 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1814
- **Maturity**: GA
- **Semantics**: `IsInsecure()` returns `insecure || insecureIgnoreHostKey || insecureOCIForceHttp`.
  - **Git HTTPS**: `GIT_SSL_NO_VERIFY=true`, and the go-git client sets `InsecureSkipVerify`.
  - **Git SSH**: strict host key checking is disabled and the warning `temporarily disabling strict host key checking ...` is logged.
  - **Helm and OCI**: `InsecureSkipVerify` uses **only** the `insecure` field, not `insecureIgnoreHostKey`.
  - **API responses**: `Sanitized()` reports `insecure: IsInsecure()`, so any of the three flags shows as `insecure: true`.
- **Edge cases**: For HTTPS, `insecure` overrides any configured TLS certificate (REPO-058). The CLI flag `--insecure-skip-server-verification` maps to `insecure`.
- **Configuration surface**: Secret key `insecure`. CLI `--insecure-skip-server-verification`.
- **Exposure**: Secret / API / CLI / UI ("Skip server verification").
- **Tests required**:
  - A self-signed HTTPS repo fails without the flag and passes with it.
  - An unknown SSH host fails without the flag and passes with it.
  - API GET reports insecure=true when only `insecureIgnoreHostKey` is set.

### REPO-011 `insecureIgnoreHostKey` (deprecated alias)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L82 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/repo.go#L48
- **Maturity**: Deprecated
- **Semantics**: Legacy flag: "should not be used anymore, Insecure is favoured". It contributes to `IsInsecure()` for Git only and is persisted under its own key.
- **Edge cases**: It is not honoured by Helm or OCI credentials. It is not shown in the UI.
- **Configuration surface**: Secret key `insecureIgnoreHostKey`. CLI `--insecure-ignore-host-key` (marked deprecated in the help text).
- **Exposure**: Secret / API / CLI.
- **Tests required**:
  - An SSH repo with the flag connects to an unknown host.
  - A Helm repo with only this flag still verifies TLS.

### REPO-012 `enableLfs` (Git LFS)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L88 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L689 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L782 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1806
- **Maturity**: GA
- **Semantics**: All git commands run with `GIT_LFS_SKIP_SMUDGE=1`. When LFS is enabled:
  - after fetch, `git lfs fetch --all` runs with credentials;
  - after checkout, if `git lfs ls-files -n` lists files, `git lfs checkout` populates them.
- **Edge cases**:
  - LFS failures surface as `failed to list LFS files` or `failed to checkout LFS files`.
  - Git repos only.
  - It is a repository setting, copied by `CopySettingsFrom` but **not** inherited from repo-creds, because RepoCreds has no LFS field.
- **Configuration surface**: Secret key `enableLfs`. CLI `--enable-lfs`.
- **Exposure**: Secret / API / CLI / UI.
- **Tests required**:
  - A repo with LFS pointers produces real file contents only when `enableLfs` is true.
  - The LFS fetch uses the repo credentials.

### REPO-013 `enableOCI` (Helm charts from OCI registries)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L99 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3165 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L503
- **Maturity**: GA
- **Semantics**: For `type: helm`, this marks the URL as an OCI registry: host[:port]/path without the `oci://` prefix. The connectivity test calls `TestHelmOCI`. Credential templates (`CopyCredentialsFrom`) always overwrite `enableOCI` with the template's value.
- **Edge cases**: A connectivity test with `enableOCI` and a URL that is not a Helm OCI repo fails with `OCI Helm repository URL should include hostname and port only`. The docs state the `oci://` prefix must be omitted for Helm OCI.
- **Configuration surface**: Secret key `enableOCI`. CLI `--enable-oci` (repo add and repocreds add).
- **Exposure**: Secret / API / CLI / UI ("Enable OCI").
- **Tests required**:
  - A Helm OCI registry validates.
  - An `oci://` URL with enableOCI fails validation with the documented message.
  - A repo-creds template with enableOCI applies to a matching repo URL.

### REPO-014 `insecureOCIForceHttp`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L123 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L330 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/common.go#L44 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3170
- **Maturity**: GA
- **Semantics**: Uses plain HTTP, with no TLS at all, for OCI repos (`type: oci`) or Helm OCI repos (`type: helm` + `enableOCI`). It maps to `oci.Creds.InsecureHTTPOnly` or `helm.WithPlainHTTP()`. It also contributes to `IsInsecure()`, which affects git. Templates always overwrite it.
- **Edge cases**: The CLI rejects it unless the type is `oci`, or `helm` with `--enable-oci` (`--insecure-oci-force-http requires --type oci or --enable-oci`). Declarative Secrets are not validated. It is omitted from sanitized API output.
- **Configuration surface**: Secret key `insecureOCIForceHttp`. CLI `--insecure-oci-force-http`.
- **Exposure**: Secret / API / CLI / UI (checkbox, effective only for oci or enableOCI).
- **Tests required**:
  - An HTTP-only OCI registry works with the flag.
  - The CLI rejects the flag for a plain git repo.

### REPO-015 GitHub App authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L286 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L540 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L553 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L688 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L619 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L110
- **Maturity**: GA
- **Semantics**: It is selected when `githubAppPrivateKey` is non-empty **and** `githubAppID != 0`, and neither password, bearer token nor SSH key is set.
  - **Token flow**: an installation token is obtained through `ghinstallation` from `https://api.github.com`, or from the enterprise base URL. The git CLI receives username `x-access-token` and the token as password via askpass. go-git uses `BasicAuth{x-access-token, token}`.
  - **Caching**: transports are cached per hash of (key, appID, installationID, baseURL), with a TTL from `ARGOCD_GITHUB_APP_CREDS_EXPIRATION_DURATION` minutes (default 60).
  - **Other inputs**: TLS client certs, `insecure`, `proxy` and `noProxy` apply to API calls.
  - **Author identity** (used by the hydrator): `GetUserInfo` returns `<slug>[bot]` and `<id>+<login>@users.noreply.github.com`.
  - **Token timeout**: `ARGOCD_GIT_REQUEST_TIMEOUT` (default 15s).
- **Edge cases**:
  - An app ID of 0 with a key set falls through to the other credential types, ending in NopCreds.
  - An invalid key fails with `failed to initialize GitHub installation transport`.
  - A proxy failing `ValidateHTTPProxyURL` is dropped with a security log (REPO-021).
  - The CLI requires an HTTPS URL for `--github-app-private-key-path`.
- **Configuration surface**: Secret keys `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppEnterpriseBaseUrl`. CLI `--github-app-id`, `--github-app-installation-id`, `--github-app-private-key-path`, `--github-app-enterprise-base-url`. Env `ARGOCD_GITHUB_APP_CREDS_EXPIRATION_DURATION`.
- **Exposure**: Secret / API / CLI / UI ("via GitHub App").
- **Tests required**:
  - A private repo validates with an app; the token is reused within the cache TTL.
  - An invalid key returns an error.
  - The key never appears in API responses (the app ID and installation ID do).
  - Recovery: after the cache expires, a new token is minted.

### REPO-016 GitHub App installation ID auto-discovery
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L692 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L771 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L902 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L129
- **Maturity**: GA
- **Semantics**: When `githubAppInstallationID` is 0:
  1. The org is extracted from the repo URL: the first path segment, lowercased. HTTPS, scp-style SSH, `ssh://` with a port, and the malformed form `ssh://git@host:org/repo` are all supported.
  2. All app installations are listed with a 15s timeout and pagination of 100.
  3. Every `org:domain:appID` mapping is cached for 60 minutes, under a mutex with double-checked caching.
  4. The cached ID for the org is used.
- **Edge cases**:
  - No org in the URL: `could not extract organization from repository URL ...`.
  - App not installed: `failed to discover GitHub App installation ID for organization <org>: ensure the GitHub App (ID: <id>) is installed for this organization: installation not found for org: <org>`.
  - The domain comes from the enterprise base URL (`github.com` when empty).
- **Configuration surface**: Omit `githubAppInstallationID`.
- **Exposure**: Secret / API / CLI (the flag is optional) / UI (Installation ID field optional).
- **Tests required**:
  - Discovery finds the right installation among several orgs.
  - Org name matching is case-insensitive.
  - A second call within 60 minutes uses the cache and makes no API call.
  - A missing installation produces the documented error.

### REPO-017 GitHub Enterprise base URL
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L107 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L665 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L858
- **Maturity**: GA
- **Semantics**: This is the API base URL, for example `https://ghe.example.com/api/v3`, with any trailing `/` trimmed. It defaults to `https://api.github.com`. It is used for token minting and installation discovery.
- **Edge cases**: The domain for the discovery cache key is parsed from this URL, and SSH-style or scheme-less values are tolerated.
- **Configuration surface**: Secret key `githubAppEnterpriseBaseUrl`. CLI `--github-app-enterprise-base-url`.
- **Exposure**: Secret / API / CLI / UI ("GitHub Enterprise" type).
- **Tests required**:
  - A GHE instance mock receives the token request at `<base>/app/installations/<id>/access_tokens`.
  - A trailing slash is tolerated.

### REPO-018 Google Cloud Source service-account key
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L967 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L991 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L1276 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L148
- **Maturity**: GA
- **Semantics**: The JSON credentials are parsed with the type taken from the JSON `type` field: `service_account` (the default), `external_account`, `impersonated_service_account` or `authorized_user`. The scope is `https://www.googleapis.com/auth/cloud-platform`. The git username is the `client_email`, and the password is an OAuth access token from a token source cached forever per JSON hash; the token source refreshes itself.
- **Edge cases**: Invalid JSON is logged at construction time, and later operations fail with `credentials for Google Cloud Source repositories are invalid`. The CLI requires an HTTPS URL for `--gcp-service-account-key-path`.
- **Configuration surface**: Secret key `gcpServiceAccountKey`. CLI `--gcp-service-account-key-path`.
- **Exposure**: Secret / API / CLI / UI ("via Google Cloud Source").
- **Tests required**:
  - A mock token endpoint is called once per expiry.
  - Invalid JSON yields the documented error.
  - The username equals client_email.

### REPO-019 Azure Workload Identity (Azure Repos and ACR Helm OCI)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L1073 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L1130 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/creds.go#L126 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/creds.go#L184 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L176
- **Maturity**: GA
- **Semantics**:
  - **Git**: a workload-identity token is requested for scope `499b84ac-1321-427f-aa17-267ca6975798/.default` (Azure DevOps). It is cached until shortly before expiry and sent as a bearer header (`ARGOCD_GIT_BEARER_AUTH_HEADER`) and as go-git `TokenAuth`. The username is the empty GUID.
  - **Helm (ACR)**:
    1. Challenge `https://<registryHost>/v2/`, expecting a 401 with a `Www-Authenticate: Bearer realm=...,service=...` header.
    2. Get an ARM token for `${AZURE_ARM_TOKEN_RESOURCE:-https://management.core.windows.net}/.default`.
    3. POST it to `<realm host>/oauth2/exchange` (`grant_type=access_token`) to obtain a refresh token, which is used as the Helm password and cached until JWT expiry.
  - **Prerequisites**: the docs require a pod label and SA annotation, plus `AZURE_ARM_TOKEN_RESOURCE=https://containerregistry.azure.net` for ACR.
- **Edge cases**:
  - A registry without a challenge fails with `registry '<h>' did not issue a challenge`.
  - Non-bearer auth fails with `registry does not allow 'Bearer' authentication`.
  - A missing realm or service fails with `realm parameter not found in challenge` or `service parameter not found in challenge`.
  - Templates always overwrite `useAzureWorkloadIdentity`.
- **Configuration surface**: Secret key `useAzureWorkloadIdentity`. CLI `--use-azure-workload-identity`. Env `AZURE_ARM_TOKEN_RESOURCE` plus standard Azure WI env/pod setup.
- **Exposure**: Secret / API / CLI / UI ("Use Azure Workload Identity").
- **Tests required**:
  - A mock ACR challenge/exchange flow yields a password.
  - Each challenge error case produces its documented message.
  - Git requests carry the bearer header.

### REPO-020 Azure DevOps Service Principal
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L295 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L1138 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L1217 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L299 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L245
- **Maturity**: GA
- **Semantics**: It is selected only when the client ID, client secret and tenant ID are **all** non-empty, and none of the earlier credential types applies (REPO-026).
  - **Token**: a client-secret credential obtains a token for the Azure DevOps scope. The authority defaults to `https://login.microsoftonline.com`. A custom `azureActiveDirectoryEndpoint` also disables instance discovery.
  - **Caching**: tokens are cached per hash of (tenant, client, secret, endpoint) for `ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION` minutes (default 59; capped at 59 with a warning).
  - **Transport**: sent as a bearer header and go-git `TokenAuth`. Proxy, noProxy and client certs apply to the token HTTP client.
- **Edge cases**:
  - A partial set (for example a missing tenant) silently falls through to NopCreds.
  - The **repo-creds Secret uses different key casing**: `azureServicePrincipalClientID` and `azureServicePrincipalTenantID`, whereas repository Secrets use `...ClientId` and `...TenantId` (see the Doc/implementation differences section).
  - The client secret is never returned by the API; the client ID, tenant ID and endpoint are.
- **Configuration surface**:
  - Repository Secret keys: `azureServicePrincipalClientId`, `azureServicePrincipalClientSecret`, `azureServicePrincipalTenantId`, `azureActiveDirectoryEndpoint`.
  - repo-creds keys: `azureServicePrincipalClientID`, `azureServicePrincipalClientSecret`, `azureServicePrincipalTenantID`, `azureActiveDirectoryEndpoint`.
  - CLI `--azure-service-principal-client-id`, `--azure-service-principal-client-secret`, `--azure-service-principal-tenant-id`, `--azure-active-directory-endpoint`.
  - Env `ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION`.
- **Exposure**: Secret / API / CLI / UI ("Via Azure Service Principal").
- **Tests required**:
  - A mock AAD token endpoint is used.
  - A custom endpoint disables instance discovery.
  - A partial config gives no credentials.
  - The expiration env is capped at 59.
  - A repo-creds round-trip uses the `...ID` key casing.

### REPO-021 Repository HTTP(S) proxy (`proxy`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/proxy/proxy.go#L26 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/proxy/proxy.go#L43 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1831 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L111 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L517
- **Maturity**: GA
- **Semantics**:
  - **git CLI**: when `proxy` is set, any existing `http_proxy`, `https_proxy` and `no_proxy` env vars (case-insensitive) are removed, and `http_proxy=<proxy>`, `https_proxy=<proxy>` and `no_proxy=<noProxy>` are added.
  - **Go HTTP clients** (go-git, Helm, OCI, GitHub API): use an `httpproxy.Config` built from proxy and noProxy.
  - **No proxy set**: the standard environment variables of the repo-server apply.
  - **Templates**: `CopyCredentialsFrom` fills `proxy` and `noProxy` from the template only if they are empty.
  - **Validation**:
    - GitHub App and Azure SP creds validate the proxy with `ValidateHTTPProxyURL`: scheme `http|https|socks5|socks5h`, hostname limited to `[a-zA-Z0-9.-]` or a valid IPv6 address, port required (1–65535).
    - An invalid proxy is **dropped** with a high-severity security log (CWE-78) rather than causing an error.
    - Plain HTTPS creds do not validate the proxy.
- **Edge cases**:
  - A proxy URL without a port is dropped for GitHub App and Azure SP creds (`proxy URL has no port`), but used as-is for plain HTTPS git.
  - The CLI `repo add` passes `--proxy` to ValidateAccess, but not `--no-proxy`.
  - `repocreds add` uses `--proxy-url` and has no no-proxy flag.
- **Configuration surface**: Secret key `proxy`. CLI `repo add --proxy`, `repocreds add --proxy-url`.
- **Exposure**: Secret / API / CLI / UI ("Proxy").
- **Tests required**:
  - Traffic goes through a configured HTTP proxy for clone, ls-remote and Helm index fetch.
  - Without a proxy, the env proxy is used.
  - For a GitHub App, an unsafe proxy (`http://a;b:80`) is ignored and a security log is emitted.

### REPO-022 `noProxy`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L118 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/proxy/proxy.go#L39 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L540
- **Maturity**: GA
- **Semantics**: A comma-separated exclusion list, effective only when `proxy` is set. Go clients use golang `httpproxy` semantics (domains, `.suffix`, CIDR). Exec'ed tools receive `no_proxy=<value>` and may interpret it differently.
- **Edge cases**: With an empty `proxy`, `noProxy` is ignored and the env `NO_PROXY` applies. CIDR or wildcard entries may not be honoured by helm or kustomize.
- **Configuration surface**: Secret key `noProxy`. CLI `repo add --no-proxy`.
- **Exposure**: Secret / API / CLI / UI.
- **Tests required**:
  - A host in noProxy bypasses the proxy; others use it.
  - With `proxy` empty, noProxy has no effect.

### REPO-023 SSH over SOCKS5 proxy
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L104 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L446 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repo.go#L67
- **Maturity**: GA
- **Semantics**: For SSH creds, `proxy` must be `socks5://` or `socks5h://` with a safe host and an explicit port. A temporary 0700 wrapper script `exec connect-proxy -5 -S <host>:<port> "$@"` is written and passed as `-o ProxyCommand=<script> %h %p`. Any user and password in the URL become `SOCKS5_USER` and `SOCKS5_PASSWD`. The script is deleted together with the key file.
- **Edge cases**: An invalid scheme or host is rejected at construction (the proxy is cleared, so the connection goes direct with a warning). At `Environ()` time an unsafe proxy errors with `unsafe proxy URL rejected: ...`. The `connect-proxy` binary must exist in the image.
- **Configuration surface**: Secret key `proxy` on an SSH repo. CLI `--proxy socks5://...`.
- **Exposure**: Secret / API / CLI / UI.
- **Tests required**:
  - An SSH clone goes through a SOCKS5 proxy, with and without credentials.
  - An `http://` proxy on an SSH repo is ignored.
  - No script remains after the operation.

### REPO-024 Shallow clone `depth`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L125 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L572 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L381 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L709
- **Maturity**: GA
- **Semantics**: `depth > 0` makes fetches run `git fetch origin <rev> --depth <n> --force --prune`, without `--tags`. A value of 0 or an absent key means a full fetch with `--tags`. It is a repository *setting*: copied by `CopySettingsFrom`, not by credential templates (RepoCreds has no depth). With shallow clones the non-webhook manifest-generate-paths git-history comparison is skipped.
- **Edge cases**:
  - A non-integer value is a parse error (REPO-027).
  - A negative value is not rejected server-side (only the UI validates "Depth must be a non-negative number").
  - It is not available on `ValidateAccess`.
- **Configuration surface**: Secret key `depth`. CLI `repo add --depth`.
- **Exposure**: Secret / API / CLI / UI (Depth field).
- **Tests required**:
  - With `depth: 1` the repo-server fetch uses `--depth 1`.
  - With depth 0, tags are fetched.
  - A shallow repo still syncs a specific SHA.

### REPO-025 `webhookManifestCacheWarmDisabled` (per-repository)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L127 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L477 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/repo.go#L63 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L474
- **Maturity**: GA
- **Semantics**: When true on the repository whose URL matches the webhook payload (`lookupRepository`, which uses `SameURL` over unscoped/global listing), the webhook handler skips manifest-cache warming (REPO-128) for unaffected apps and only triggers refreshes. It is ORed with the global env `ARGOCD_WEBHOOK_MANIFEST_CACHE_WARM_DISABLED=true`.
- **Edge cases**: It is not exposed in the UI or in sanitized API output. A repository lookup error is logged and treated as "not disabled".
- **Configuration surface**: Secret key `webhookManifestCacheWarmDisabled`. CLI `repo add --webhook-manifest-cache-warm-disabled`.
- **Exposure**: Secret / API (field on create/update) / CLI.
- **Tests required**:
  - With the flag, a webhook for a repo with 100 unaffected apps performs zero cache operations.
  - Without it, `argocd_webhook_store_cache_attempts_total` increments.

### REPO-026 Git credential type precedence
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L276 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L152
- **Maturity**: GA
- **Semantics**: `GetGitCreds` checks credential types in this fixed order:
  1. `password` or `bearerToken`: HTTPS
  2. `sshPrivateKey`: SSH
  3. `githubAppPrivateKey` + `githubAppID != 0`: GitHub App
  4. `gcpServiceAccountKey`: GCP
  5. `useAzureWorkloadIdentity`: Azure WI
  6. all three Azure SP fields: Azure SP
  7. otherwise: NopCreds

  `HasCredentials()` (which decides template inheritance) is true when any of username, password, bearerToken, sshPrivateKey, tlsClientCertData, githubAppPrivateKey or azureServicePrincipalClientSecret is non-empty, or `useAzureWorkloadIdentity` is set. **It ignores `gcpServiceAccountKey`.**
- **Edge cases**:
  - A repo with only a GCP key still inherits repo-creds, but the repo's own GCP key remains, because inheritance only fills empty fields.
  - A username alone counts as credentials for inheritance but yields NopCreds.
- **Configuration surface**: n/a (derived).
- **Exposure**: internal (affects all paths).
- **Tests required**:
  - A table test of every combination against the expected credential type.
  - Username only gives NopCreds and no inheritance.
  - A GCP key plus a matching repo-creds keeps the GCP creds.

### REPO-027 Secret field encoding, parsing and write/delete semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L49 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L67 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L89 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L101 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L104 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L425
- **Maturity**: GA
- **Semantics**:
  - **Parsing**: bools are parsed with Go `strconv.ParseBool` (`1,t,T,TRUE,true,True,0,f,F,FALSE,false,False`) and ints with `ParseInt` base 10. An absent key means false or 0.
  - **Writing**: a key is written only if it already exists **or** the value is non-zero/non-empty. Unknown keys are preserved on update, and existing keys can be blanked but are never removed.
  - **Metadata**: every API write adds the annotation `managed-by: argocd.argoproj.io` and sets the `secret-type` label.
  - **Delete**: if the secret carries `managed-by: argocd.argoproj.io`, it is deleted. Otherwise the `argocd.argoproj.io/secret-type` label is **removed** and the Secret is kept, which protects declaratively managed secrets.
- **Edge cases**:
  - A malformed bool or int in a repository secret means `ListRepositories` still returns the entry, with `connectionState.status=Failed` and `message="Configuration error - please check the server logs"`, plus the warning `Error while parsing repository secret '<name>'`.
  - `GetRepository` for that URL returns the parse error.
- **Configuration surface**: Annotation `managed-by`. Label `argocd.argoproj.io/secret-type`.
- **Exposure**: Secret / API.
- **Tests required**:
  - `insecure: "yes"` produces the Failed/Configuration error state in the list.
  - An update preserves an extra custom key.
  - Deleting an unmanaged secret only strips the label.
  - Deleting an API-created secret removes it.

### REPO-028 Secret naming for API-created repositories and creds
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L18 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L472 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L45
- **Maturity**: GA
- **Semantics**: Names are `<prefix>-<fnv32a(url + project)>`, with prefixes `repo`, `repo-write`, `creds`, `creds-write`. The project is empty for creds. The formula is documented as unstable and must not be used for lookup.
- **Edge cases**: If the computed name already exists but lacks the `secret-type` label, create fails with InvalidArgument: `secret "<name>" doesn't have the proper "argocd.argoproj.io/secret-type" label: please fix the secret or delete it`. If the name exists with the label, it fails with AlreadyExists `repository "<url>" already exists`.
- **Configuration surface**: n/a.
- **Exposure**: API / CLI (`admin repo generate-spec` prints the Secret using this name).
- **Tests required**:
  - Creating the same URL in two projects produces two distinct secret names.
  - A pre-existing unlabelled secret with the colliding name yields the InvalidArgument message.

### REPO-029 Repository URL normalization and equality (`SameURL`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/git.go#L43 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/git.go#L63 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/git.go#L83
- **Maturity**: GA
- **Semantics**: `NormalizeGitURL`:
  1. Lowercases and trims the whole URL, including the path.
  2. If it is an SSH URL (`^(ssh://)?([^/:]*?)@[^@]+$`) without `ssh://`, replaces the first `:` with `/` and prefixes `ssh://`.
  3. Strips a trailing `.git`.
  4. Re-serializes through `url.Parse`, then strips `ssh://`.

  `SameURL` compares normalized forms, falling back to the raw string when normalization fails. This lets repos with invalid URLs still be matched for deletion. Both normalized values must be non-empty.
- **Edge cases**:
  - `https://GitHub.com/Org/Repo.git` equals `https://github.com/org/repo`.
  - `git@github.com:org/repo` equals `ssh://git@github.com/org/repo`.
  - `https` vs `ssh` forms of the same repo are **not** equal.
  - A trailing slash is not stripped.
- **Configuration surface**: n/a.
- **Exposure**: internal (repo lookup, credential matching, webhook metrics labels).
- **Tests required**:
  - A table test of equivalences and non-equivalences.
  - An invalid URL can still be deleted.

## B. Repository credential templates

### REPO-030 Credential template Secret (`repo-creds`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L21 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L467 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L529 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L332 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-repo-creds.yaml#L1
- **Maturity**: GA
- **Semantics**: A Secret labelled `argocd.argoproj.io/secret-type: repo-creds` holds a URL prefix plus credentials.
  - **Supported keys**: `url`, `username`, `password`, `bearerToken`, `sshPrivateKey`, `tlsClientCertData`, `tlsClientCertKey`, `type`, `githubAppPrivateKey`, `githubAppID`, `githubAppInstallationID`, `githubAppEnterpriseBaseUrl`, `gcpServiceAccountKey`, `proxy`, `noProxy`, `enableOCI`, `insecureOCIForceHttp`, `forceHttpBasicAuth`, `useAzureWorkloadIdentity`, `azureServicePrincipalClientID`, `azureServicePrincipalClientSecret`, `azureServicePrincipalTenantID`, `azureActiveDirectoryEndpoint`.
  - **Not supported**: `project`, `name`, `insecure`, `enableLfs`, `depth` (no such fields on RepoCreds).
- **Edge cases**:
  - A `project` key on repo-creds is silently ignored (the sample file shows one).
  - Malformed bools or ints make `GetRepositoryCredentials` return an error, which fails `GetRepository` for every matching URL.
- **Configuration surface**: Label `argocd.argoproj.io/secret-type=repo-creds`. The keys above.
- **Exposure**: Secret / API (RepoCredsService) / CLI (`argocd repocreds`) / UI ("Save as credentials template").
- **Tests required**:
  - A declarative repo-creds round-trips through the API create/list.
  - Unsupported keys are ignored.
  - A malformed `enableOCI` value surfaces as an error.

### REPO-031 Longest-prefix template matching and credential inheritance
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L615 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L434 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L150 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L375 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L275
- **Maturity**: GA
- **Semantics**:
  - **Matching**: both the repo URL and each template URL are normalized (REPO-029), and the template with the **longest** normalized URL that is a plain string prefix of the repo URL wins. Ties keep the first one and log `Found multiple credentials`.
  - **When inheritance applies**: a repository inherits only when `HasCredentials()` is false. Inherited repos get `inheritedCreds: true`, which the CLI shows as `CREDS=inherited`.
  - **Unconfigured repos**: `GetRepository` for a URL with no repository secret returns a stub `{repo: url}` enriched with template credentials. Apps can therefore use templates without any repository Secret.
- **Edge cases**:
  - The prefix is not path-segment aware: template `https://github.com/argo` matches `https://github.com/argoproj/x`.
  - Matching ignores case and `.git`.
  - A repo with only a username set does not inherit.
- **Configuration surface**: n/a (behaviour).
- **Exposure**: all repo consumers; `inheritedCreds` in API output.
- **Tests required**:
  - Two templates `https://github.com/org` and `https://github.com/org/team`: a repo under team uses the longer one.
  - A repo with its own password ignores the templates.
  - An unconfigured repo URL is cloneable via a template.
  - `inheritedCreds=true` appears in the list.

### REPO-032 Template field copy rules (`CopyCredentialsFrom`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L211 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L157
- **Maturity**: GA
- **Semantics**:
  - **Filled only if empty on the repo**: username, password, bearerToken, sshPrivateKey, TLS cert data and key, GitHub App key/ID/installation ID/base URL, GCP key, the four Azure SP fields, proxy, noProxy, and type.
  - **Always overwritten from the template**: `enableOCI`, `insecureOCIForceHttp`, `forceHttpBasicAuth`, `useAzureWorkloadIdentity`.
  - **`CopyCredentialsFromRepo`** (repo to repo) is the same except it does not touch proxy, noProxy, type or enableOCI.
- **Edge cases**: A repository with `enableOCI: true` that inherits from a template without it ends up with `enableOCI=false`.
- **Configuration surface**: n/a.
- **Exposure**: internal.
- **Tests required**:
  - A unit test per field for empty vs non-empty receivers.
  - The always-overwrite fields are verified.

## C. Write credentials (source hydrator)

### REPO-033 Write repositories (`repository-write`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L208 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L72 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L99 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L485 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L53
- **Maturity**: Beta (part of Source Hydrator)
- **Semantics**: These Secrets use the same schema as repository Secrets but are labelled `argocd.argoproj.io/secret-type: repository-write`, and are used only for pushing hydrated manifests. Project scoping and fallback work exactly as for read repos (REPO-004), through a separate `byProjectRepoWrite` index. The hydrator calls `GetWriteRepository(destRepoURL, appProject)`. If nothing is configured it receives a `{repo: url}` stub, possibly enriched with `repo-write-creds`. Read and write secrets are fully separate, so a read secret never grants push.
- **Edge cases**:
  - Several matching write secrets: the first wins and a warning is logged (documented as non-deterministic).
  - Project-scoped write secrets only work when all apps writing to the same repo and branch are in that project.
- **Configuration surface**: Label `argocd.argoproj.io/secret-type=repository-write`. Same keys as REPO-001.
- **Exposure**: Secret / API (`/api/v1/write-repositories`) / UI ("write" toggle in connect forms). No CLI.
- **Tests required**:
  - The hydrator pushes using the write secret.
  - A read-only secret is never used for push.
  - Scoped write secret selection follows the app project.

### REPO-034 Write credential templates (`repo-write-creds`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L210 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L295 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L451 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L522
- **Maturity**: Beta
- **Semantics**: The same longest-prefix logic as REPO-031, applied to `repo-write-creds` Secrets when the write repository has no credentials of its own.
- **Edge cases**: `ListRepoCreds` and `GetAllHelm/OCIRepoCreds` always read **read** repo-creds (`repo-creds`), even on the write backend (see REPO-046).
- **Configuration surface**: Label `argocd.argoproj.io/secret-type=repo-write-creds`.
- **Exposure**: Secret / API (`/api/v1/write-repocreds`) / UI (credentials template plus write).
- **Tests required**:
  - The hydrator pushes to a repo with no write secret when a matching repo-write-creds exists.
  - The longest prefix wins.

## D. Repository and credential APIs, CLI and UI

### REPO-035 RepositoryService List / ListRepositories
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L132 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L185 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L218 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L400 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L128
- **Maturity**: GA (`List` is a deprecated alias of `ListRepositories`)
- **Semantics**: `GET /api/v1/repositories?forceRefresh=<bool>`. The handler:
  1. Lists all repositories, with template credentials applied.
  2. Normalizes and sanitizes each one. `Sanitized()` keeps only repo, type, name, insecure (=IsInsecure), enableLfs, enableOCI, proxy, noProxy, project, forceHttpBasicAuth, inheritedCreds, githubAppID, githubAppInstallationID, githubAppEnterpriseBaseUrl, useAzureWorkloadIdentity, azureActiveDirectoryEndpoint, azureServicePrincipalClientId, azureServicePrincipalTenantId and depth.
  3. Filters by RBAC `repositories, get, <project>/<url>|<url>`.
  4. Computes each connection state in parallel (REPO-043).
  5. Sorts by `"<project>/<repo>"`.
- **Edge cases**:
  - Secrets (password, keys, tokens, client secret) are never returned.
  - `insecureOCIForceHttp` and `webhookManifestCacheWarmDisabled` are not returned.
  - Users without get permission receive an empty list, not a 403.
- **Configuration surface**: Query `forceRefresh`.
- **Exposure**: API `RepositoryService.ListRepositories` (`GET /api/v1/repositories`) / CLI `argocd repo list` / UI Settings > Repositories.
- **Tests required**:
  - No secret fields appear in the JSON output.
  - The RBAC filter hides repos per project.
  - The sort order is stable.
  - `forceRefresh` bypasses the cache.

### REPO-036 RepositoryService Get
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L137 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L645 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L134
- **Maturity**: GA
- **Semantics**: `GET /api/v1/repositories/{repo}?appProject=&forceRefresh=`. It searches the sanitized, RBAC-filtered list for `SameURL` matches.
  - **Choosing a match**: if exactly one matches and no `appProject` was given, that one is returned. Otherwise the match whose project equals `appProject` is returned.
  - **Checks**: the caller must pass RBAC `repositories, get`, and the repository must exist (fallback allowed).
- **Edge cases**:
  - No accessible match returns PermissionDenied (`common.PermissionDeniedAPIError`), so the API does not reveal whether the repo exists.
  - Several matches with a non-matching project return `repository not found for url "<u>" and project "<p>"`.
  - A non-existent repo after the checks returns NotFound `repo '<u>' not found`.
  - The URL in the path must be URL-encoded.
- **Configuration surface**: Query `appProject`, `forceRefresh`.
- **Exposure**: API / CLI `argocd repo get REPO [--project] [--refresh hard] [-o json|yaml|wide|url]` / UI repo details.
- **Tests required**:
  - Get a scoped repo by project.
  - Two scoped repos with the same URL and no project gives an error.
  - Unauthorized users get 403 regardless of existence.

### REPO-037 RepositoryService Create / CreateRepository
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L436 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L57 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L111
- **Maturity**: GA (`Create` is a deprecated alias)
- **Semantics**: `POST /api/v1/repositories?upsert=&credsOnly=`, with body = Repository.
  1. Missing payload returns InvalidArgument `missing payload in request`.
  2. RBAC `repositories, create, <project>/<url>|<url>`.
  3. If `project` is empty, the repo-server connectivity test runs first; credentials are copied from a matching template when the repo has none. Project-scoped repos skip the test.
  4. The Secret is created with `connectionState=Successful`.
  5. On AlreadyExists: if the existing spec, after defaulting type to `git` and ignoring connection state, is DeepEqual to the request, the call is idempotent. With `upsert=true` it updates. Otherwise it returns InvalidArgument with the "existing spec is different" diff message.
  6. The response contains only `{repo, type, name}`.
- **Edge cases**:
  - `credsOnly` exists in the proto but is not used by the server.
  - DB-level create fails with AlreadyExists `repository "<url>" already exists` when an exact (url, project) secret exists.
  - A connection failure returns the repo-server error (`error testing repository connectivity: ...`).
- **Configuration surface**: Query `upsert`.
- **Exposure**: API / CLI `argocd repo add` / UI "Connect repo".
- **Tests required**:
  - An identical create is idempotent.
  - A differing create without upsert fails.
  - Upsert updates the secret.
  - A scoped repo is created even when unreachable.
  - An unscoped unreachable repo is rejected.
  - RBAC deny gives 403.

### REPO-038 RepositoryService Update / UpdateRepository
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L547 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L206 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L136
- **Maturity**: GA (`Update` is a deprecated alias)
- **Semantics**: `PUT /api/v1/repositories/{repo.repo}`, with body = Repository.
  - **Lookup**: the existing repo is fetched by (url, project) with fallback. A fetch failure returns PermissionDenied.
  - **RBAC**: `repositories, update` is enforced on both the existing object and the target `<project>/<url>`.
  - **Write**: the secret for the exact (url, project) is updated (NotFound `repo '<u>' not found` otherwise). Fields are merged per REPO-027.
  - **No connectivity test** is run.
  - **Response**: `{repo, type, name}`.
- **Edge cases**:
  - Moving a repo between projects requires update permission in both, and requires an exact (url, newProject) secret to exist.
  - Sending empty secret fields blanks them only if the keys already exist.
- **Configuration surface**: n/a.
- **Exposure**: API / UI (repo edit). The CLI has no `repo edit` command.
- **Tests required**:
  - Updating the proxy persists it.
  - Updating a non-existent repo returns NotFound.
  - The RBAC check on the destination project is enforced.

### REPO-039 RepositoryService Delete / DeleteRepository
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L604 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L232
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/repositories/{repo}?appProject=`. The handler:
  1. Resolves the repo as in Get.
  2. Enforces RBAC `repositories, delete`.
  3. Invalidates the cached connection state.
  4. Deletes, or un-labels, the secret for the exact (url, project) per REPO-027.
- **Edge cases**: An unresolvable repo returns PermissionDenied. An unmanaged declarative secret keeps existing with the label removed.
- **Configuration surface**: Query `appProject`.
- **Exposure**: API / CLI `argocd repo rm REPO... [--project]` (prompts unless prompts are disabled) / UI.
- **Tests required**:
  - Delete the scoped repo only.
  - An unmanaged secret survives without its label.
  - The connection-state cache is cleared.

### REPO-040 RepositoryService ValidateAccess
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L683 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L55
- **Maturity**: GA
- **Semantics**: `POST /api/v1/repositories/{repo}/validate`. RBAC is `repositories, create, <project>/<url>`. A Repository is built from the `RepoAccessQuery` fields (repo, type, name, username, password, bearerToken, sshPrivateKey, insecure, TLS cert and key, enableOci, GitHub App fields, proxy, gcpServiceAccountKey, insecureOciForceHttp, useAzureWorkloadIdentity, the Azure SP fields). If it has no credentials, a matching template is applied. Then the repo-server `TestRepository` runs (REPO-044).
- **Edge cases**: `RepoAccessQuery` has **no** noProxy, enableLfs, forceHttpBasicAuth-to-repo mapping, or depth. `forceHttpBasicAuth` is in the proto but is not copied by the server.
- **Configuration surface**: Request body.
- **Exposure**: API / CLI (`repo add` calls it before create) / UI (Connect).
- **Tests required**:
  - Valid credentials return 200.
  - Invalid credentials return an error.
  - Template credentials are used when none are given.
  - RBAC create is required.

### REPO-041 Write-repository endpoints (gated by hydrator)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L159 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L199 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L495 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L570 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L624 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L734 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L65
- **Maturity**: Beta
- **Semantics**: The endpoints are `GetWrite` (`GET /api/v1/write-repositories/{repo}`), `ListWriteRepositories` (`GET /api/v1/write-repositories`), `CreateWriteRepository` (`POST /api/v1/write-repositories`), `UpdateWriteRepository` (`PUT /api/v1/write-repositories/{repo.repo}`), `DeleteWriteRepository` (`DELETE /api/v1/write-repositories/{repo}`) and `ValidateWriteAccess` (`POST /api/v1/write-repositories/{repo}/validate`).
  - **Gate**: every endpoint returns `Unimplemented: hydrator is disabled` unless the server runs with the hydrator enabled.
  - **RBAC**: resource `write-repositories`, actions get/create/update/delete.
  - **Create**: requires credentials (`missing credentials in request`) and always runs the connectivity test, including for scoped repos.
- **Edge cases**:
  - `ValidateWriteAccess` does not apply credential templates.
  - Write create's idempotency compares the raw request with an enriched existing repo, without type defaulting. This differs from read create.
- **Configuration surface**: `hydrator.enabled` (`ARGOCD_HYDRATOR_ENABLED`) on argocd-server.
- **Exposure**: API / UI.
- **Tests required**:
  - With the hydrator disabled, all endpoints return Unimplemented.
  - Create without credentials returns InvalidArgument.
  - RBAC on `write-repositories` is enforced separately from `repositories`.

### REPO-042 Repository browsing endpoints (refs, OCI tags, apps, app details, Helm charts)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L241 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L262 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L285 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L333 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L412 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L801
- **Maturity**: GA
- **Semantics**:
  - **`ListRefs`** (`GET /api/v1/repositories/{repo}/refs`) and **`ListOCITags`** (`.../oci-tags`): RBAC `repositories, get`; return branches and tags, or OCI tags, from the repo-server.
  - **`ListApps`** (`GET .../apps?revision=&appName=&appProject=`): additionally requires `applications, create` **or** `update` on `<appProject>/<appName>`, and the repo must be permitted in the project's sourceRepos. Otherwise `repository '<r>' not permitted in project '<p>'`.
  - **`GetAppDetails`** (`POST /api/v1/repositories/{source.repoURL}/appdetails`): requires `applications, get`.
    - For a non-existent app it also requires `applications, create`.
    - For an existing app, the project must match and the source must be the current source or appear in history; for multi-source apps the comparison uses the `sourceIndex`/`versionId` revision. Otherwise PermissionDenied.
  - **`GetHelmCharts`** (`GET .../helmcharts`): RBAC `repositories, get`.
  - **Repository lookup**: all endpoints resolve the repo with `appProject`, and a lookup failure returns PermissionDenied.
- **Edge cases**:
  - `GetAppDetails` with a nil source returns InvalidArgument `missing payload in request`.
  - When the source hydrator is configured, only its dry and sync sources are accepted.
- **Configuration surface**: Query params as listed.
- **Exposure**: API / UI (app creation wizard, revision dropdowns).
- **Tests required**:
  - ListApps without app create/update permission is denied.
  - GetAppDetails with an arbitrary non-historical source is denied.
  - Refs for an SSH repo with credentials succeed.

### REPO-043 Repository connection state and cache
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L97 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cache/cache.go#L38 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2314
- **Maturity**: GA
- **Semantics**: `connectionState` is `{status: Successful|Failed|Unknown, message, attemptedAt}`. For each listed repo:
  - Unless `forceRefresh` is set, the cached state keyed by (url, project) is returned. The cache TTL comes from `--connection-status-cache-expiration` / `ARGOCD_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION` (default 1h).
  - Otherwise the repo is fetched with credentials and tested on the repo-server.
  - On failure the status is Failed, with message `Unable to connect to repository: <err>`, or `Configuration error - please check the server logs` for credential configuration errors.
  - The result is written to the cache.
- **Edge cases**: The JSON field name for `ModifiedAt` is `attemptedAt`. Deleting a repo clears the cache entry. A cache write failure is only logged.
- **Configuration surface**: Flag `--connection-status-cache-expiration`, env `ARGOCD_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION`.
- **Exposure**: API field / CLI `STATUS`/`MESSAGE` columns and `--refresh hard` / UI connection status icon.
- **Tests required**:
  - Breaking credentials makes the status flip to Failed only after `forceRefresh` or cache expiry.
  - The message format is checked.

### REPO-044 Repo-server connectivity test (`TestRepository`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3145 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/git.go#L124
- **Maturity**: GA
- **Semantics**: An empty type means git. The test per type is:
  - **git**: `git.TestRepo`, which builds a client with the credentials, insecure, LFS, proxy and noProxy settings and runs `LsRemote("HEAD")`.
  - **oci**: an OCI client `TestRepo`.
  - **helm with enableOCI**: requires a Helm-OCI-shaped URL, then calls `TestHelmOCI`, using plain HTTP if `insecureOCIForceHttp`.
  - **helm otherwise**: `GetIndex` limited to the configured maximum index size.

  Errors are wrapped as `error testing repository connectivity: <err>`.
- **Edge cases**:
  - An unsupported type returns `unsupported repository type "<t>"`.
  - A git init error returns `unable to initialize git client: ...`.
  - An ls-remote failure returns `unable to ls-remote HEAD on repository: ...`.
- **Configuration surface**: n/a.
- **Exposure**: Internal gRPC (repo-server), reached through ValidateAccess, CreateRepository and connection state.
- **Tests required**: Each type succeeds and fails against fixture servers, and each produces the expected error strings.

### REPO-045 RepoCredsService (read templates)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L36 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L84 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L154 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L178 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L332 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.proto#L42
- **Maturity**: GA
- **Semantics**:
  - **`ListRepositoryCredentials`** (`GET /api/v1/repocreds`): for each template URL passing RBAC `repositories, get, <url>`, returns `{url, username}` only. The username comes from the longest-prefix match for that URL.
  - **`CreateRepositoryCredentials`** (`POST /api/v1/repocreds?upsert=`): RBAC `repositories, create, <url>`; an empty URL returns `must specify URL`. Same idempotent/upsert/diff semantics as repositories. Returns `{url}`.
  - **`UpdateRepositoryCredentials`** (`PUT /api/v1/repocreds/{creds.url}`): RBAC update. NotFound returns `repository credentials '<u>' not found`.
  - **`DeleteRepositoryCredentials`** (`DELETE /api/v1/repocreds/{url}`): RBAC delete. Uses REPO-027 delete semantics.
  - **RBAC objects** are plain URLs; there is no project.
- **Edge cases**:
  - DB create first checks whether a **repository** (not a template) with the same URL and no project exists, and returns AlreadyExists `repository credentials "<url>" already exists` in that case.
  - Update and Delete resolve the target with longest-prefix matching, so deleting `https://github.com/org/team` when only `https://github.com/org` exists deletes the `org` template.
- **Configuration surface**: Query `upsert`.
- **Exposure**: API / CLI `argocd repocreds` / UI (credentials template).
- **Tests required**:
  - Create, list, delete round-trip.
  - Upsert behaviour.
  - Creating a template whose URL equals an existing repository URL fails.
  - The prefix-resolution quirk on delete.
  - The list never returns secrets.

### REPO-046 Write RepoCreds endpoints
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L60 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L119 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L166 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repocreds/repocreds.go#L188 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L348
- **Maturity**: Beta
- **Semantics**: The endpoints are `GET/POST /api/v1/write-repocreds`, `PUT /api/v1/write-repocreds/{creds.url}` and `DELETE /api/v1/write-repocreds/{url}`, with RBAC resource `write-repositories`. Create checks for an existing write template; AlreadyExists returns `write repository credentials "<url>" already exists`.
- **Edge cases**:
  - **List quirk**: the list iterates over the **read** template URLs (`db.ListRepositoryCredentials`). For each one it looks up the write template by longest prefix and includes it only if the write template has a non-empty **password**. Write templates without a matching read template URL, or that use SSH, GitHub App or other non-password auth, are not listed.
  - These endpoints are **not** gated by the hydrator flag, unlike write-repositories.
- **Configuration surface**: n/a.
- **Exposure**: API / UI.
- **Tests required**:
  - Reproduce the list quirk: a write template with a GitHub App and no read template is not listed; a password-based write template whose URL is shared with a read template is listed.
  - Creating a duplicate fails.

### REPO-047 CLI `argocd repo` (add / get / list / rm)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repo.go#L26 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repo.go#L57 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/repo.go#L38 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repo.go#L332 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_repo_add.md#L67
- **Maturity**: GA
- **Semantics**:
  - **`add REPOURL`**:
    - **Flags**: `--type`, `--name`, `--project`, `--username`, `--password`, `--bearer-token`, `--ssh-private-key-path`, `--tls-client-cert-path`, `--tls-client-cert-key-path`, `--insecure-ignore-host-key`, `--insecure-skip-server-verification`, `--enable-lfs`, `--enable-oci`, `--github-app-id`, `--github-app-installation-id`, `--github-app-private-key-path`, `--github-app-enterprise-base-url`, `--proxy`, `--no-proxy`, `--gcp-service-account-key-path`, `--force-http-basic-auth`, `--use-azure-workload-identity`, `--insecure-oci-force-http`, `--depth`, `--webhook-manifest-cache-warm-disabled`, the four `--azure-*` flags, and `--upsert`.
    - **Behaviour**: reads key and cert files, validates flag combinations client-side, prompts for a password when only a username is given, calls ValidateAccess and then CreateRepository, and prints `Repository '<url>' added`.
  - **`get REPO`**: `--project`, `-o json|yaml|wide|url` (default wide), `--refresh hard`.
  - **`list`**: `-o yaml|json|url|wide`, `--refresh hard`. Table columns: TYPE, NAME, REPO, INSECURE, OCI, LFS, CREDS (`true|false|inherited`), STATUS, MESSAGE, PROJECT.
  - **`rm REPO...`**: `--project`; asks for confirmation per repo.
- **Edge cases**:
  - Unknown `--refresh` values: `unknown refresh value: <v>. Supported values: hard`.
  - Unknown output format: `unknown output format: <f>. Supported formats: yaml|json|url|wide`.
  - There is no `repo edit` or `repo update` command, and no CLI for write repositories.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Golden output for list and get.
  - Every client-side validation error string.
  - `--upsert` path.
  - Password prompt behaviour (mocked TTY).

### REPO-048 CLI `argocd repocreds` (add / list / rm)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repocreds.go#L54 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repocreds.go#L196 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repocreds.go#L222 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/repocreds.go#L278 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_repocreds_add.md#L43
- **Maturity**: GA
- **Semantics**:
  - **`add REPOURL`**: `--username`, `--password`, `--bearer-token`, `--ssh-private-key-path`, `--tls-client-cert-path`/`--tls-client-cert-key-path`, the GitHub App flags, `--upsert`, `--enable-oci`, `--insecure-oci-force-http`, `--type` (default git), `--gcp-service-account-key-path`, `--force-http-basic-auth`, `--use-azure-workload-identity`, `--proxy-url`, and the `--azure-*` flags. It uses the same client-side validations as `repo add`, plus the insecure-oci check. There is **no** connectivity test.
  - **`list`**: `-o json|yaml|wide|url`. Columns: URL PATTERN, USERNAME (`-` if empty), SSH_CREDS, TLS_CREDS. Because the API returns only url and username, SSH_CREDS and TLS_CREDS are always `false`.
  - **`rm CREDSURL...`**: with confirmation.
- **Edge cases**: The proxy flag is named `--proxy-url` here (`--proxy` on `repo add`). There is no `--no-proxy` and no `--insecure-skip-server-verification`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - add, list and rm golden outputs.
  - SSH_CREDS and TLS_CREDS are always false in the list.
  - Validation errors.

### REPO-049 CLI `argocd admin repo generate-spec`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/repo.go#L40 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/repo.go#L146 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_repo_generate-spec.md#L59
- **Maturity**: GA
- **Semantics**: Offline. It creates the repository in a fake clientset in namespace `argocd` and prints the resulting Secret (`-o yaml|json`, default yaml), named per REPO-028. It accepts the same flag set as `repo add`.
- **Edge cases**:
  - Only these options are mapped onto the generated secret: type, name, project, username, password, bearerToken, the SSH key file, the TLS cert files, insecureIgnoreHostKey, insecure, enableLfs, enableOCI, useAzureWorkloadIdentity, insecureOCIForceHttp and webhookManifestCacheWarmDisabled.
  - The GitHub App, GCP key file, proxy, noProxy, forceHttpBasicAuth, depth and Azure SP flags are **accepted but silently ignored**, even though the examples show GitHub App and GCP usage.
  - A Helm repo without a name fails with `must specify --name for repos of type 'helm'`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - The generated secret matches the expected YAML.
  - Ignored flags do not appear in the output (parity with upstream).

### REPO-050 UI Settings > Repositories
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/repos-list/repos-list.tsx#L170 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/repos-list/repos-list.tsx#L215 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/repos-list/repos-list.tsx#L320 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L25
- **Maturity**: GA
- **Semantics**:
  - **Connection methods**: VIA SSH, VIA HTTP/HTTPS, VIA GitHub App, VIA Google Cloud Source, VIA Azure Service Principal.
  - **Buttons**: "Connect" creates a repository; "Save as credentials template" creates repo-creds.
  - **Write**: a write toggle routes to the write endpoints.
  - **Fields**: depth (must be non-negative), noProxy, project, bearerToken, TLS cert, forceHttpBasicAuth, enableOCI, insecureOCIForceHttp (applied only for oci or enableOCI), useAzureWorkloadIdentity.
  - **List**: shows connection state and supports refresh, disconnect and edit (HTTPS).
- **Edge cases**:
  - For templates, URL validation requires HTTP(S) for the GitHub App, GCS and Azure SP methods.
  - `webhookManifestCacheWarmDisabled` and `insecureIgnoreHostKey` are not exposed.
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**:
  - E2E: connect via each method.
  - Save as a template.
  - Invalid depth validation.
  - A failed connection shows the error notification.

## E. Git transport and credential plumbing

### REPO-051 Git CLI credential injection (askpass) and auth headers
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1775 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/askpass/server.go#L111 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L66
- **Maturity**: GA
- **Semantics**:
  - **Askpass**: credentialed git commands (fetch, ls-remote, push, submodule, LFS) store the username and token in an in-process askpass server and run git with `GIT_ASKPASS=argocd` and `ARGOCD_GIT_ASKPASS_NONCE=<id>`. The nonce is removed after the command.
  - **Auth headers**: if the env contains `ARGOCD_GIT_AUTH_HEADER` or `ARGOCD_GIT_BEARER_AUTH_HEADER`, git is invoked with `--config-env http.extraHeader=<VAR>`.
  - **Base environment**: every git invocation adds `HOME=/dev/null`, `GIT_TERMINAL_PROMPT=false` and `GIT_LFS_SKIP_SMUDGE=1`.
- **Edge cases**: Credentials never appear on the command line or in persistent files. The global `~/.gitconfig` is not honoured.
- **Configuration surface**: n/a.
- **Exposure**: internal.
- **Tests required**:
  - Inspect the child process env and args for the expected variables.
  - The nonce is removed after the command.
  - No secrets appear in the process args.

### REPO-052 Built-in git configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L59 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L231 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L279 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/git_configuration.md#L26
- **Maturity**: GA
- **Semantics**: `maintenance.autoDetach=false` and `gc.autoDetach=false` are injected through `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n`, overriding the system gitconfig. The system `/etc/gitconfig` is honoured; global config is not (HOME is `/dev/null`).
- **Edge cases**: Disabling this is documented as unsupported.
- **Configuration surface**: `argocd-cmd-params-cm: reposerver.enable.builtin.git.config` (default `"true"`), env `ARGOCD_REPO_SERVER_ENABLE_BUILTIN_GIT_CONFIG`, flag `--enable-builtin-git-config`.
- **Exposure**: repo-server config.
- **Tests required**:
  - When enabled, the env contains `GIT_CONFIG_COUNT=2` and both keys.
  - When disabled, they are absent.

### REPO-053 Git remote retry and timeout settings
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L218 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L305 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L268
- **Maturity**: GA
- **Semantics**:
  - **Attempts**: `ARGOCD_GIT_ATTEMPTS_COUNT` sets the attempt count for remote operations (default 1, minimum 1; non-integer values **panic** at start-up).
  - **Backoff**: `ARGOCD_GIT_RETRY_MAX_DURATION` (default 5s), `ARGOCD_GIT_RETRY_DURATION` (default 250ms) and `ARGOCD_GIT_RETRY_FACTOR` (default 2).
  - **HTTP timeout**: `ARGOCD_GIT_REQUEST_TIMEOUT` (default 15s) applies to the go-git HTTP client and the GitHub App token fetch. This client never follows redirects.
- **Edge cases**: An invalid `ARGOCD_GIT_ATTEMPTS_COUNT` value panics the process with `Invalid value in ARGOCD_GIT_ATTEMPTS_COUNT env variable`.
- **Configuration surface**: The env vars above.
- **Exposure**: repo-server / controller env.
- **Tests required**:
  - A retry count of 3 makes 3 attempts against a flaky server.
  - The panic on an invalid value.
  - A redirect is not followed.

### REPO-054 Git submodules and credential reuse
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L763 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L276 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L543
- **Maturity**: GA
- **Semantics**: When submodules are enabled (the default), checkout runs `git submodule sync --recursive` and then `git submodule update --init --recursive`, both with the **parent** repository's credentials.
- **Edge cases**: A submodule that needs different credentials fails. `ARGOCD_GIT_MODULES_ENABLED=false` disables submodule handling.
- **Configuration surface**: Env `ARGOCD_GIT_MODULES_ENABLED`.
- **Exposure**: repo-server env.
- **Tests required**:
  - A private submodule on the same host with the same credentials resolves.
  - With the env set to false, the submodule directory stays empty.

### REPO-055 Credential and token caches
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L75 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L139 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L302
- **Maturity**: GA
- **Semantics**: All caches are in-memory and per process:
  - **GitHub App transports**: default TTL 60 minutes (`ARGOCD_GITHUB_APP_CREDS_EXPIRATION_DURATION`, in minutes); the token is refreshed by the transport.
  - **GitHub installation IDs**: 60 minutes.
  - **GCP token sources**: no expiry.
  - **Azure WI tokens**: expiry derived from the token.
  - **Azure SP tokens**: 59 minutes (`ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION`, capped at 59).
  - **Helm ACR tokens**: until JWT expiry.
- **Edge cases**: Non-integer env values are ignored and the defaults apply.
- **Configuration surface**: The env vars above.
- **Exposure**: internal.
- **Tests required**:
  - Token minting count over time with mocked endpoints.
  - The cap at 59 minutes.

### REPO-056 Helm and OCI credentials for dependencies and chart sources
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/helmrepository.go#L11 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L285 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L307 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L307 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/repository_types.go#L342
- **Maturity**: GA
- **Semantics**:
  - **What the server passes to the repo-server**: named Helm repositories (`type: helm`, non-empty `name`) and all `repo-creds` whose `type` is `helm` (case-insensitive) or `oci`. These are used to authenticate chart sources and chart dependencies.
  - **Helm credentials**: username, password, a CA path from `argocd-tls-certs-cm` (empty for SSH URLs, otherwise `<tls dir>/<hostname>` when valid certs exist), client cert and key, and `InsecureSkipVerify = insecure`. With Azure WI, the ACR flow from REPO-019 is used instead.
  - **OCI credentials**: the same fields plus `InsecureHTTPOnly`.
- **Edge cases**: If the URL cannot be parsed for its hostname, a warning is logged and no CA path is used. Unnamed Helm repos are not passed as dependency repos.
- **Configuration surface**: Repository and repo-creds Secrets with `type: helm|oci`.
- **Exposure**: Secret / internal.
- **Tests required**:
  - A chart with a dependency on a private Helm repo resolves via a named repo.
  - The same works via a helm-typed repo-creds.
  - A custom CA is picked up for the Helm host.

## F. SSH known hosts and TLS certificates

### REPO-057 SSH known hosts (`argocd-ssh-known-hosts-cm`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L58 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L100 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L438 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L391 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/certificate.go#L456 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L465
- **Maturity**: GA
- **Semantics**:
  - **Storage**: the ConfigMap holds a single key, `ssh_known_hosts`, in standard known_hosts format (`<host> <keytype> <base64>`, one entry per line; `[host]:port` is allowed; `#` comments and blank lines are ignored).
  - **Mount**: the ConfigMap is mounted at `/app/config/ssh/ssh_known_hosts`; `ARGOCD_SSH_DATA_PATH` overrides the directory.
  - **Use**: the git CLI uses `UserKnownHostsFile=<path>` with strict checking. go-git uses the same file and passes the host-key algorithms known for `host:port` (port 22 by default).
  - **API reads**: the ConfigMap must carry `app.kubernetes.io/part-of: argocd` to be read by the settings manager.
- **Edge cases**:
  - Hashed hostnames are not supported for import.
  - An unknown host fails the connection unless `insecure` is set.
  - Propagation into pods depends on kubelet ConfigMap sync delay.
  - Parsing is limited to 256 entries per stream (`CertificateMaxEntriesPerStream`).
- **Configuration surface**: ConfigMap `argocd-ssh-known-hosts-cm`, key `ssh_known_hosts`. Env `ARGOCD_SSH_DATA_PATH`.
- **Exposure**: ConfigMap / API (CertificateService) / CLI (`argocd cert add-ssh|list|rm`) / UI (Settings > Certificates).
- **Tests required**:
  - An SSH repo works after its host key is added and fails before.
  - A `[host]:2222` entry works for a non-default port.
  - Commented lines are ignored.

### REPO-058 TLS certificates for repository servers (`argocd-tls-certs-cm`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L60 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L90 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L318 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L345 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L375 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L406
- **Maturity**: GA
- **Semantics**:
  - **Storage**: each data key is a server **hostname** (no scheme or port; the port is stripped on lookup) and its value is one or more PEM certificates, either the server cert or a CA.
  - **Mount**: mounted at `/app/config/tls/<hostname>`; `ARGOCD_TLS_DATA_PATH` overrides the directory.
  - **Use**: for HTTPS git the CLI gets `GIT_SSL_CAINFO=<file>`, but only when the file exists and contains only valid certs. go-git, Helm and OCI use the file as the **only** root CAs for that host. Without a file, the system trust store is used.
  - **Precedence**: `insecure` overrides it entirely.
  - **Path safety**: path traversal is rejected (`could not get certificate for host ...`).
- **Edge cases**:
  - A file with no parseable certs gives `no certificates found in existing file`, and the bundle is ignored.
  - Because a configured bundle **replaces** the system roots for that host, a wrong certificate breaks access even to publicly trusted hosts.
  - Certs are configured per server, not per repo.
- **Configuration surface**: ConfigMap `argocd-tls-certs-cm`, one key per hostname. Env `ARGOCD_TLS_DATA_PATH`.
- **Exposure**: ConfigMap / API / CLI (`argocd cert add-tls|list|rm`) / UI.
- **Tests required**:
  - A self-signed git server works once its CA is added.
  - Adding a wrong CA for a public host breaks access.
  - A port in the hostname is stripped.

### REPO-059 CertificateService ListCertificates
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/certificate/certificate.go#L31 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/certificate.go#L59 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L283 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/certificate/certificate.proto#L14
- **Maturity**: GA
- **Semantics**: `GET /api/v1/certificates?hostNamePattern=&certType=&certSubType=`, with RBAC `certificates, get, ""`.
  - **Filters**: `certType` is empty, `*`, `ssh`, or `https`/`tls`. `hostNamePattern` is a file glob (`filepath.Match`, with `[` and `]` escaped); empty matches everything. `certSubType` filters SSH entries only.
  - **SSH items**: `{serverName, certType: ssh, certSubType: <keytype>, certInfo: "SHA256:<fingerprint>"}`.
  - **HTTPS items**: one item per PEM entry, with `certSubType` = lowercased public key algorithm (`rsa`, `ecdsa`, ...) and `certInfo` = subject DN. A decode error gives subtype `invalid` and certInfo = the error.
  - **Never returned**: `certData`.
- **Edge cases**: A corrupt PEM file for a host is skipped silently in the list.
- **Configuration surface**: Query params.
- **Exposure**: API / CLI `argocd cert list [--cert-type] [--hostname-pattern] [--sort hostname|type] [-o json|yaml|wide]` / UI.
- **Tests required**:
  - Filter combinations.
  - Glob patterns with brackets.
  - The fingerprint format.
  - RBAC deny.

### REPO-060 CertificateService CreateCertificate
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/certificate/certificate.go#L47 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/certificate.go#L152 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L80 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1841
- **Maturity**: GA
- **Semantics**: `POST /api/v1/certificates?upsert=`, with body = RepositoryCertificateList. RBAC is `certificates, create`.
  - **https**:
    1. The hostname must match the hostname regex or be IPv6.
    2. The data must contain at least one valid PEM X.509 cert.
    3. An existing entry with different data requires `upsert`; upsert replaces the whole PEM bundle.
    4. Each PEM is returned in the created list.
  - **ssh**:
    1. The hostname, or the host inside `[host]:port`, is validated.
    2. The line is parsed with `ssh.ParseKnownHosts`.
    3. An existing (host, subtype) entry with different data requires `upsert`.
    4. Identical data is a no-op and is **not** included in the response.
  - **Saving**: the ConfigMaps are rewritten (known hosts joined with `\n` plus a trailing newline; TLS data map replaced) and the informers are resynced.
- **Edge cases**:
  - `invalid hostname in request: <h>`
  - `key for '<h>' (subtype: '<t>') already exists, and upsert was not specified`
  - `TLS certificate for server '<h>' already exists, and upsert was not specified`
  - `no valid PEM data received`
  - `unknown certificate type: <t>`
  - The whole request fails on the first error, and nothing is saved.
- **Configuration surface**: Query `upsert`.
- **Exposure**: API / CLI `cert add-tls`, `cert add-ssh` / UI.
- **Tests required**:
  - Each error string.
  - Upsert replaces data.
  - Identical re-add reports 0 created.
  - Mixed-type batch.

### REPO-061 CertificateService DeleteCertificate
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/certificate/certificate.go#L60 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/certificate.go#L336
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/certificates?hostNamePattern=&certType=&certSubType=`, with RBAC `certificates, delete`. It removes every matching SSH entry (glob plus optional subtype) and every matching TLS host, and returns the removed items, one per PEM for TLS. ConfigMaps are rewritten only if something was removed.
- **Edge cases**:
  - An empty selector removes **everything**; the CLI forbids a lone `*` pattern, but the API does not.
  - A TLS entry with corrupt PEM data aborts the delete with a parse error, so it must be fixed through kubectl.
- **Configuration surface**: Query params.
- **Exposure**: API / CLI `argocd cert rm REPOSERVER [--cert-type] [--cert-sub-type]` / UI (Remove).
- **Tests required**:
  - Delete one SSH subtype only.
  - Delete a TLS host.
  - An empty selector via the API wipes all entries.
  - The CLI rejects `*`.

### REPO-062 CLI `argocd cert` (add-tls / add-ssh / list / rm)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cert.go#L69 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cert.go#L148 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cert.go#L186 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cert.go#L256 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cert.go#L313
- **Maturity**: GA
- **Semantics**:
  - **`add-tls SERVERNAME [--from file] [--upsert]`**: reads PEM from a file or stdin and deduplicates by SHA-256 fingerprint, warning `WARNING: Duplicate certificate detected ...`. It sends one https entry with the PEMs joined by newlines and prints `Created/updated TLS certificate entry for repository server <s> with <n> unique PEM certificates`.
  - **`add-ssh --batch [--from file] [--upsert]`**: `--batch` is mandatory. It parses known_hosts lines, expands multi-host lines into one entry per host, and prints `Successfully created <n> SSH known host entries`.
  - **`list`**: see REPO-059.
  - **`rm REPOSERVER`**: asks for confirmation; `*` alone gives `A single wildcard is not allowed as REPOSERVER name.`
- **Edge cases**:
  - add-tls with no valid certs prints `No valid certificates have been detected in the stream.` and exits 0.
  - add-ssh without `--batch` errors with `you need to specify --batch or specify --help for usage instructions`.
  - Empty known-hosts input is fatal: `No valid SSH known hosts data found.`
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**: Golden outputs and each error.

### REPO-063 UI Settings > Certificates
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/certs-list/certs-list.tsx#L134 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/certs-list/certs-list.tsx#L97 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/private-repositories.md#L373
- **Maturity**: GA
- **Semantics**: Lists all certificates, and supports "Add TLS certificate" (server name plus PEM), "Add SSH known hosts" (paste lines) and Remove with confirmation.
- **Edge cases**: Pasted key data must have no line breaks.
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**: E2E add and remove for both types.

## G. GnuPG keyring and source integrity

### REPO-064 GnuPG public keys ConfigMap (`argocd-gpg-keys-cm`) and repo-server keyring sync
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/gpgkeys.go#L59 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L216 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L48 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L104 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L116 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L335
- **Maturity**: GA
- **Semantics**:
  - **Storage**: each ConfigMap key is a 16-hex-character short key ID, and its value is the ASCII-armored public key.
  - **Validation**: on read, every entry must parse to exactly one key whose ID equals the map key.
  - **Repo-server keyring**: when `ARGOCD_GPG_ENABLED` is not false, the repo-server initializes a GnuPG home at `ARGOCD_GNUPGHOME` (default `/app/config/gpg/keys`; this is a transient keyring with a generated private key used to build the trust DB). It then one-way syncs it from the mounted ConfigMap directory (`ARGOCD_GPG_DATA_PATH`, default `/app/config/gpg/source`), and a file watcher re-syncs on changes. All imported keys are trusted ultimately.
- **Edge cases**:
  - One invalid entry makes the whole list fail: `error validating key ID "<k>" in GPG keys configmap`, `error parsing GPG key for entry ...`, or `key parsed for entry with key ID '<a>' had different key ID '<b>'`.
  - Manual keyring changes are lost on restart.
- **Configuration surface**: ConfigMap `argocd-gpg-keys-cm`. Env `ARGOCD_GPG_ENABLED`, `ARGOCD_GNUPGHOME`, `ARGOCD_GPG_DATA_PATH`.
- **Exposure**: ConfigMap / API (GPGKeyService) / CLI (`argocd gpg`) / UI (Settings > GnuPG keys).
- **Tests required**:
  - Adding a key to the ConfigMap makes it available for verification without a restart.
  - Removing it revokes it.
  - A mismatched key ID entry breaks the list call.

### REPO-065 GPGKeyService (List / Get / Create / Delete)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/gpgkey/gpgkey.go#L31 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/gpgkey/gpgkey.go#L49 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/gpgkey/gpgkey.go#L72 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/gpgkey/gpgkey.go#L101 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/gpgkeys.go#L90 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/gpg.go#L65 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/gpgkey/gpgkey.proto#L40
- **Maturity**: GA
- **Semantics**: RBAC resource `gpgkeys`, object `""`.
  - **List** (`GET /api/v1/gpgkeys`): returns keyID, fingerprint, owner, trust and subType, with **KeyData stripped**.
  - **Get** (`GET /api/v1/gpgkeys/{keyID}`): accepts a 16-hex short ID or a 40-hex fingerprint (the last 16 characters are used) and returns the key including KeyData.
  - **Create** (`POST /api/v1/gpgkeys`, body `publickey`): trims and validates the key data, which may contain several keys. Keys already present are reported in `skipped`; new ones in `created`. The ConfigMap is saved.
  - **Delete** (`DELETE /api/v1/gpgkeys?keyID=`): removes the exact ConfigMap key.
- **Edge cases**:
  - Empty data: `submitted key data is empty`.
  - An invalid ID on Get: `'<k>' is not a valid GnuPG key ID`. A missing key: `no such key: <id>`.
  - Delete does **not** normalize fingerprints, so deleting by fingerprint fails with `no such key configured: <fp>`.
  - The request's `upsert` field is ignored; existing keys are always skipped.
- **Configuration surface**: n/a.
- **Exposure**: API / CLI / UI.
- **Tests required**:
  - Add two keys, one of them already existing, and check created and skipped.
  - Get by fingerprint.
  - Delete by short ID succeeds and by fingerprint fails.
  - RBAC.

### REPO-066 CLI `argocd gpg` (list / get / add / rm)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/gpg.go#L25 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/gpg.go#L129 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_gpg_add.md#L18
- **Maturity**: GA
- **Semantics**:
  - **`list [-o json|yaml|wide]`**
  - **`get KEYID [-o json|yaml|wide]`**
  - **`add --from <file>`** (`-f`): binary or ASCII-armored, per the docs.
  - **`rm KEYID`**
- **Edge cases**:
  - `--from is mandatory`.
  - A missing argument gives `Missing KEYID argument`.
  - An unknown output format gives `unknown output format: <f>`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**: Golden outputs and error messages.

### REPO-067 UI Settings > GnuPG keys
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/gpgkeys-list/gpgkeys-list.tsx#L93 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/gpgkeys-list/gpgkeys-list.tsx#L68 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L107
- **Maturity**: GA
- **Semantics**: List, import (ASCII-armored only in the UI) and remove keys.
- **Edge cases**: Binary keys are not supported in the UI.
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**: E2E import and remove.

### REPO-068 AppProject `sourceIntegrity.git.policies` (policy matching)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/source_integrity.go#L8 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L21 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L52 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L91 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity.md#L1 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L144
- **Maturity**: GA
- **Semantics**: Each policy is `{repos: [{url: <glob>}], gpg: {mode, keys}}`.
  - **Matching a policy**: a policy matches a source repo URL when at least one positive glob matches **and** no `!`-prefixed glob matches. Negative matches abort evaluation of that policy immediately.
  - **Multiple matches**: exactly one policy may match a repo. If several match, verification returns the error `multiple (<n>) git source integrity policies found for repo URL: <url>`, which fails closed.
  - **No match**: the source is not verified.
  - **Scope**: only Git sources are checked; Helm, OCI and empty sources are skipped. In multi-source apps each source is checked independently.
  - **Failure effect**: failures produce a ComparisonError and block sync.
- **Edge cases**:
  - A policy with `gpg: null` is logged as "No verification configured" and is not checked.
  - A policy with no repos never matches.
  - Matching uses the raw source `repoURL` with Argo CD glob semantics.
- **Configuration surface**: CRD `AppProject.spec.sourceIntegrity.git.policies[].repos[].url`, `.gpg.mode`, `.gpg.keys[]`.
- **Exposure**: CRD / API (Project update) / CLI (`argocd proj source-integrity ...`). There is no UI ("UI is not supported yet").
- **Tests required**:
  - A negative glob excludes a repo.
  - Two overlapping policies produce the multiple-policies error.
  - A Helm source is unaffected.
  - Multi-source with different policies per source.

### REPO-069 GPG mode `none`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/source_integrity.go#L33 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L71
- **Maturity**: GA
- **Semantics**: The policy matches but performs no verification. Unsigned or invalid commits are accepted. Because the matching policy "wins", no other policy is tried.
- **Edge cases**: `HasCriteria` still reports true for the repo, so local-manifest sync stays blocked (REPO-073).
- **Configuration surface**: `gpg.mode: none`.
- **Exposure**: CRD / CLI.
- **Tests required**:
  - An unsigned commit syncs under `none`.
  - A local sync is still rejected.

### REPO-070 GPG mode `head`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L124 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L214 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L189
- **Maturity**: GA
- **Semantics**: Verifies only the target revision. If it is an annotated tag, the **tag** signature is verified; otherwise the commit signature is. A signature passes only if its result is `Good` **and** its key ID is in `keys`. Entries in `keys` may be short IDs or fingerprints; invalid entries are logged and skipped.
- **Edge cases**:
  - The problem messages are `Failed verifying revision <rev> by '<author>': <result> (key_id=<id>)` and `Failed verifying revision <rev> by '<author>': signed with unallowed key (key_id=<id>)`.
  - The check name is `GIT/GPG`.
  - An unknown mode gives the error `unknown GPG mode "<m>" configured for GIT source integrity`.
- **Configuration surface**: `gpg.mode: head`, `gpg.keys`.
- **Exposure**: CRD / CLI.
- **Tests required**:
  - Signed with an allowed key: syncs.
  - Signed with a key that is in the keyring but not allowed: blocked with the unallowed-key message.
  - Unsigned: blocked.
  - A signed annotated tag on an unsigned commit: syncs.

### REPO-071 GPG mode `strict` and seal commits
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/source_integrity.go#L36 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L168 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1302 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L195
- **Maturity**: GA
- **Semantics**: Verifies the target revision and **all ancestors**. Each ancestral branch stops at the most recent "seal commit": a GPG-signed commit with the trailer `Argocd-gpg-seal: <justification>`. If the target is an annotated tag, both the tag and the history are verified.
  - **Reporting**: at most 10 problems are reported, newest first.
  - **Deduplication**: repeated problems for the same signing key are squashed, except for unsigned commits and tags.
- **Edge cases**:
  - The problem list may be incomplete; seal commits signed by keys that are in the keyring but not in `keys` can surface on later runs.
  - No signatures found: `git LsSignatures found no signatures for <rev>`.
- **Configuration surface**: `gpg.mode: strict`.
- **Exposure**: CRD / CLI.
- **Tests required**:
  - A history containing one unsigned commit is blocked.
  - Adding a signed seal commit with an allowed key unblocks it.
  - The 10-problem cap.
  - A tag with valid history passes.

### REPO-072 Legacy `signatureKeys` and the effective policy conversion
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2828 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2857 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2874 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/gpg.go#L675 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/gpg-verification.md#L3 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L221
- **Maturity**: Deprecated
- **Semantics**: `spec.signatureKeys[].keyID` is converted by `EffectiveSourceIntegrity()`:
  - **No `sourceIntegrity`**: a single policy `{repos: [{url: "*"}], gpg: {mode: head, keys: <keyIDs>}}` is created, with a warning.
  - **`sourceIntegrity.git` set**: the signature keys are **ignored**, and an error is logged.
  - **`sourceIntegrity` set without `git`**: the converted git policy is merged into a copy.
  - **Legacy outputs** (`SignatureInfo`, revision metadata) are still populated for API compatibility: `Good signature from <author> key <id>` or `Revision is not signed.`
- **Edge cases**: Downgrading requires moving the keys back manually.
- **Configuration surface**: CRD `AppProject.spec.signatureKeys`.
- **Exposure**: CRD / API / CLI (`proj add-signature-key`, `remove-signature-key`, `--signature-keys`) / UI (Project "Required signature keys").
- **Tests required**:
  - signatureKeys only: behaves like head mode for `*`.
  - Both set: the signature keys are ignored.
  - Merge with a non-git sourceIntegrity.

### REPO-073 Source integrity blocks local-manifest sync
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2138 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L719 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity.md#L17
- **Maturity**: GA
- **Semantics**: If any source of the app has applicable criteria (`HasCriteria`), a sync request with `manifests` (`argocd app sync --local`) is rejected with FailedPrecondition `Cannot use local manifests when source integrity is enforced`. The controller enforces the same as a ComparisonError.
- **Edge cases**: Mode `none` still counts as criteria.
- **Configuration surface**: n/a.
- **Exposure**: API (ApplicationService.Sync) / CLI.
- **Tests required**:
  - `--local` sync is rejected under any matching policy.
  - It is allowed when no policy matches.

### REPO-074 CLI `argocd proj source-integrity git policies` (list / add / update / delete)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_source_integrity.go#L55 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_source_integrity.go#L264 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_source_integrity.go#L361 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_source_integrity.go#L533 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_proj_source-integrity_git_policies_add.md#L21
- **Maturity**: GA
- **Semantics**:
  - **`list PROJECT`**
  - **`add PROJECT --repo-url <glob>... --gpg-mode strict|head|none --gpg-key <id>...`**: key IDs are normalized to short IDs. Warnings are printed for no repos, no keys, or keys absent from the keyring (fetched through the GPG key API).
  - **`update PROJECT POLICY_ID`**: `--gpg-mode`, `--repo-url`, `--add-repo-url`, `--delete-repo-url`, `--gpg-key`, `--add-gpg-key`, `--delete-gpg-key`, `-y`.
  - **`delete PROJECT POLICY_ID... [-y]`**
  - **Update mechanism**: changes are applied by reading the project and calling the ProjectService update.
- **Edge cases**:
  - `gpg-mode must be set` / `gpg-mode must be one of: strict, head, none`.
  - `invalid POLICY_ID '<x>'` / `the POLICY_ID <n> is out of range (0-<m>)`.
  - Combining set and add/delete flags is rejected (`option --gpg-key, cannot be combined with --add-gpg-key or --delete-gpg-key`).
  - Invalid key: `invalid GPG key ID '<k>': ...`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI (backed by the Project API).
- **Tests required**:
  - CRUD round-trip.
  - Each error.
  - The warning for a key not in the keyring.

### REPO-075 CLI legacy signature key commands
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L187 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L42 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L256
- **Maturity**: Deprecated
- **Semantics**: `argocd proj add-signature-key PROJECT KEY-ID`, `argocd proj remove-signature-key PROJECT KEY-ID`, and `--signature-keys k1,k2` on `proj create` / `proj set` manage `spec.signatureKeys`. Keys are normalized with `KeyID`.
- **Edge cases**: Invalid key IDs are rejected client-side.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**: Add, remove and set round-trips.

### REPO-076 `ARGOCD_GPG_ENABLED` global switch
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L240 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L76 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/gpg.go#L819 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-integrity-git-gpg.md#L326
- **Maturity**: GA
- **Semantics**: The feature is disabled when the value is `false` or `no` (case-insensitive); any other value, including empty, means enabled. The docs say to set it on argocd-server, repo-server, application-controller and applicationset-controller. When disabled, the repo-server does not initialize the keyring, and matching policies are skipped with a one-time warning: `SourceIntegrity criteria for git+gpg declared, but it is turned off by ARGOCD_GPG_ENABLED`.
- **Edge cases**: Code behaviour at the pinned commit: the "skip when disabled" branch is guarded by a process-wide "already logged" flag. Only the **first** lookup per process returns no check; later lookups return a verifier even when GPG is disabled. See the Doc/implementation differences section.
- **Configuration surface**: Env `ARGOCD_GPG_ENABLED`.
- **Exposure**: Deployment env.
- **Tests required**:
  - With the switch disabled, the first evaluation is skipped and the warning is logged once.
  - Parity test for the second-evaluation behaviour.
  - With the switch disabled, `CommitSignatureError` performs no verification.

## H. Cluster Secrets and the `Cluster` data model

### REPO-077 Cluster Secret declarative storage
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L375 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L422 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/cluster_informer.go#L51 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2324 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L542
- **Maturity**: GA
- **Semantics**: A cluster is a Secret in the Argo CD namespace labelled `argocd.argoproj.io/secret-type: cluster`, with data keys `name`, `server`, `namespaces`, `clusterResources`, `project`, `shard` and `config` (JSON `ClusterConfig`).
  - **Informer**: a dedicated cluster informer (3-minute resync) transforms each Secret into a `Cluster` once, and indexes by server URL (trailing `/` trimmed), name and project.
  - **Cluster ID**: the Secret UID; it is not exposed in the API (`json:"-"`).
- **Edge cases**:
  - Invalid `config` JSON leaves the item untransformed. It is skipped by list/get, with the warnings `Failed to convert secret <n> to cluster` and `Expected *appv1.Cluster in cache, got *v1.Secret (skipping)`, and lookups by URL return NotFound.
  - The server URL is compared after trimming the trailing slash.
- **Configuration surface**: Label `argocd.argoproj.io/secret-type=cluster`. Keys as above.
- **Exposure**: Secret / API (ClusterService) / CLI (`argocd cluster`) / UI (Settings > Clusters).
- **Tests required**:
  - A declarative cluster appears in the list.
  - Malformed config JSON is skipped and does not break the list.
  - A trailing slash in `server` is normalized.

### REPO-078 Cluster `server` and `name`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L377 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L295 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/cluster_informer.go#L161
- **Maturity**: GA
- **Semantics**: `server` is the API server URL and the primary key; it is stored with any trailing `/` trimmed. `name` defaults to the server URL when empty on API create or update. Name lookups (`GetClusterServersByName`) can return several servers. `in-cluster` is special-cased: it maps to `https://kubernetes.default.svc` when no secret exists and in-cluster is enabled.
- **Edge cases**: Duplicate names are allowed; destination resolution by name is handled elsewhere. When in-cluster is disabled, name lookups filter out `https://kubernetes.default.svc`.
- **Configuration surface**: Keys `server`, `name`.
- **Exposure**: Secret / API / CLI / UI.
- **Tests required**:
  - Create without a name: name = server.
  - Two clusters with the same name: name lookup returns both.
  - `in-cluster` resolves without a secret.

### REPO-079 Cluster `namespaces` (namespaced mode)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2340 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L383 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L871 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L551
- **Maturity**: GA
- **Semantics**: A comma-separated list; entries are trimmed and empty ones dropped. When non-empty, the controller watches only those namespaces (one list/watch per namespace) and ignores cluster-scoped resources unless `clusterResources=true`. A change triggers cluster-cache invalidation and re-sync. Project-scoped clusters contribute one destination per namespace to the project (`*` when the list is empty).
- **Edge cases**:
  - Many namespaces can exhaust idle connections (tune `ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS`).
  - The CLI `cluster set --namespace '*'` clears the list.
  - On create, `argocd cluster add --namespace` installs a Role and RoleBinding per namespace instead of a ClusterRole.
- **Configuration surface**: Key `namespaces`. CLI `--namespace` (repeatable).
- **Exposure**: Secret / API / CLI / UI (Namespaces editor).
- **Tests required**:
  - Resources outside the listed namespaces are not visible.
  - Updating the namespaces re-syncs the cache.
  - `" a , ,b "` parses to `[a,b]`.

### REPO-080 Cluster `clusterResources`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2348 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L394 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L874
- **Maturity**: GA
- **Semantics**: Meaningful only in namespaced mode: it allows managing cluster-scoped resources. Stored as `"true"` only; the key is absent when false. Read as `== "true"`, so it is case-sensitive.
- **Edge cases**: `"True"` in a declarative secret is read as false.
- **Configuration surface**: Key `clusterResources`. CLI `--cluster-resources`.
- **Exposure**: Secret / API / CLI.
- **Tests required**:
  - A ClusterRole can be synced in namespaced mode only when the flag is true.
  - The `"True"` casing quirk.

### REPO-081 Project-scoped clusters and `permitOnlyProjectScopedClusters`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2350 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L483 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L776 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L45 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L372
- **Maturity**: GA
- **Semantics**: `project` binds a cluster to an AppProject. Its server, and each of its namespaces or `*`, are appended to the project's effective destinations. The cluster RBAC object is `<project>/<server>`. When the project sets `permitOnlyProjectScopedClusters: true`, a destination that matches the project's destinations is permitted only if the cluster's name or server equals one of the project-scoped clusters.
- **Edge cases**: A non-scoped cluster listed explicitly in the project's destinations is rejected when the flag is true.
- **Configuration surface**: Cluster key `project`. CLI `--project`. CRD `AppProject.spec.permitOnlyProjectScopedClusters`.
- **Exposure**: Secret / CRD / API / CLI / UI.
- **Tests required**:
  - A scoped cluster auto-permits a destination.
  - The flag rejects a global cluster.
  - RBAC `clusters, create, proj/*`.

### REPO-082 Cluster `shard`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2346 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L391 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L446
- **Maturity**: GA (the sharding algorithms themselves are Alpha and live in the controller area)
- **Semantics**: An optional explicit controller shard number. Absent means the controller computes one. Stored as a decimal string.
- **Edge cases**: A non-integer is logged (`Error while parsing shard in cluster secret`) and treated as unset. The CLI default `-1` means unset.
- **Configuration surface**: Key `shard`. CLI `--shard`.
- **Exposure**: Secret / API / CLI.
- **Tests required**: `shard: "abc"` is treated as unset; `shard: "2"` round-trips.

### REPO-083 Cluster labels and annotations
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L402 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L455 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2352
- **Maturity**: GA
- **Semantics**: The `Cluster.labels` and `Cluster.annotations` fields map to the Secret's metadata.
  - **On read**: the `argocd.argoproj.io/secret-type` label and the `kubectl.kubernetes.io/last-applied-configuration` and `managed-by` annotations are stripped.
  - **On write**: the labels **replace** the Secret's labels (plus secret-type), and the annotations replace its annotations, plus `managed-by` and the refresh annotation.
- **Edge cases**: Setting the last-applied annotation via the API returns InvalidArgument `annotation kubectl.kubernetes.io/last-applied-configuration cannot be set`. An API update drops any Secret labels or annotations not in the request.
- **Configuration surface**: Secret metadata. CLI `--label k=v`, `--annotation k=v`.
- **Exposure**: Secret / API / CLI / UI (LABELS, ANNOTATIONS).
- **Tests required**:
  - Labels round-trip without system keys.
  - The last-applied annotation is rejected.
  - An update replaces the labels.

### REPO-084 `ClusterConfig` basic auth (`username` / `password`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2518 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4053
- **Maturity**: GA
- **Semantics**: Used when neither `awsAuthConfig` nor `execProviderConfig` is set. Passed into the REST config's Username and Password.
- **Edge cases**: Not exposed in API responses (sanitized).
- **Configuration surface**: `config.username`, `config.password`.
- **Exposure**: Secret / API (write only).
- **Tests required**: Basic auth against a mock API server; the secret is not returned.

### REPO-085 `ClusterConfig.bearerToken`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2524 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L119
- **Maturity**: GA
- **Semantics**: Bearer authentication. `argocd cluster add` stores the argocd-manager SA token here, but only when the kubeconfig context has no client cert **and** key; with both present, the cert is used and no token is stored. Rotate-auth requires it (REPO-103).
- **Edge cases**: Sanitized out of responses.
- **Configuration surface**: `config.bearerToken`.
- **Exposure**: Secret / API / CLI (`cluster add`, `admin cluster generate-spec --bearer-token|--generate-bearer-token`).
- **Tests required**:
  - Token auth works.
  - A cert-based kubeconfig context yields no token.

### REPO-086 `ClusterConfig.tlsClientConfig`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2543 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3994 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L71
- **Maturity**: GA
- **Semantics**: The fields are `insecure` (bool, always serialized), `serverName` (SNI and verification name), `caData`, `certData` and `keyData` (PEM bytes, base64 in JSON). They apply to every non-in-cluster auth mode, including AWS and exec.
  - **CLI capture**: `cluster add` reads the kubeconfig's CA, cert and key, either inline or from files.
  - **Sanitizing**: only `insecure` and `serverName` are returned by the API.
- **Edge cases**: They are ignored for the in-cluster server (REPO-092).
- **Configuration surface**: `config.tlsClientConfig.{insecure,serverName,caData,certData,keyData}`.
- **Exposure**: Secret / API / CLI.
- **Tests required**:
  - A custom CA, mTLS and serverName override each work.
  - Insecure skips verification.
  - The data is not returned by GET.

### REPO-087 `ClusterConfig.awsAuthConfig` (EKS IAM)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2469 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4002 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L674
- **Maturity**: GA
- **Semantics**: When set (it takes precedence over exec and token), the REST config uses an exec provider: command `argocd-k8s-auth`, args `aws --cluster-name <clusterName> [--role-arn <roleARN>] [--profile <profile>]`, apiVersion `client.authentication.k8s.io/v1beta1`, never interactive. It is returned in API responses because it is not secret.
- **Edge cases**: An empty `clusterName` is passed through as `--cluster-name ""`. Requires the `argocd-k8s-auth` binary and AWS credentials (IRSA or Pod Identity).
- **Configuration surface**: `config.awsAuthConfig.{clusterName,roleARN,profile}`. CLI `--aws-cluster-name`, `--aws-role-arn`, `--aws-profile`.
- **Exposure**: Secret / API / CLI.
- **Tests required**: The exec args are built exactly; a mock exec plugin yields a token.

### REPO-088 `ClusterConfig.execProviderConfig`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2482 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4020 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L567
- **Maturity**: GA
- **Semantics**: The fields are `command`, `args`, `env` (map, converted to an ExecEnvVar list), `apiVersion`, `installHint`, `provideClusterInfo` and `config` (raw JSON passed as `ExecCredential.Spec.Cluster.Config` when non-empty). The exec provider always runs with `InteractiveMode: Never`. It is used when `awsAuthConfig` is nil.
- **Edge cases**:
  - The command must exist in the image.
  - The env var order is non-deterministic (Go map iteration).
  - `execProviderConfig` is **removed** from API responses because args and env may contain secrets.
- **Configuration surface**: `config.execProviderConfig.*`. CLI `--exec-command`, `--exec-command-args`, `--exec-command-env`, `--exec-command-api-version`, `--exec-command-install-hint`.
- **Exposure**: Secret / API (write only) / CLI.
- **Tests required**:
  - A mock plugin receives the env and args.
  - `provideClusterInfo` together with `config` delivers the config to the plugin.
  - GET omits execProviderConfig.

### REPO-089 `ClusterConfig.disableCompression`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2536 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4073 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L104
- **Maturity**: GA
- **Semantics**: Disables automatic gzip on requests to this cluster (REST config and transport).
- **Edge cases**: `argocd cluster add --disable-compression` is parsed but **not applied**. The value is taken from the kubeconfig context's REST config instead (see the Doc/implementation differences section).
- **Configuration surface**: `config.disableCompression`. CLI `--disable-compression` (no effect).
- **Exposure**: Secret / API / CLI.
- **Tests required**:
  - Declarative true disables `Accept-Encoding: gzip`.
  - Parity: the CLI flag does not change the stored value.

### REPO-090 `ClusterConfig.proxyUrl`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2539 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3952 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4066
- **Maturity**: GA
- **Semantics**: Every request to the cluster API server goes through this proxy; it overrides the environment proxy. Schemes `http`, `https` and `socks5` are allowed. Without it, the transport uses `http.ProxyFromEnvironment`.
- **Edge cases**: Another scheme gives `unable to create K8s REST config, can't parse proxy url: failed to parse proxy url, unsupported scheme "<s>", must be http, https, or socks5`. `socks5h` is **not** accepted for clusters, unlike for repos.
- **Configuration surface**: `config.proxyUrl`. CLI `cluster add --proxy-url`.
- **Exposure**: Secret / API / CLI (`cluster add --proxy-url`; `cluster get -o wide` shows "Use proxy").
- **Tests required**:
  - An HTTP proxy is used.
  - An invalid scheme produces the error message.

### REPO-091 Cluster auth precedence (`RawRestConfig`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3966 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4081
- **Maturity**: GA
- **Semantics**: Evaluated in order:
  1. in-cluster server with `ARGOCD_FAKE_IN_CLUSTER=true`: local kubeconfig;
  2. in-cluster server without username, password or token: pod in-cluster config;
  3. in-cluster server with username, password or token: in-cluster config with those overriding, and the token file cleared;
  4. otherwise `awsAuthConfig`, then `execProviderConfig`, then basic and bearer, each with the TLS config.

  Then `proxyUrl`, `disableCompression`, server-side timeout, QPS and burst are applied. `RESTConfig()` additionally wraps the transport with the tuned defaults (REPO-105).
- **Edge cases**: For the in-cluster server, the TLS, AWS and exec settings in the secret are ignored.
- **Configuration surface**: n/a.
- **Exposure**: internal.
- **Tests required**: A table test of the precedence; for in-cluster, a token in the secret overrides the SA token.

### REPO-092 In-cluster cluster (`https://kubernetes.default.svc`, `in-cluster`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L31 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L32 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L243 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L148 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/cluster-management.md#L39
- **Maturity**: GA
- **Semantics**: When in-cluster is enabled and no secret exists for `https://kubernetes.default.svc`, a synthetic cluster `{name: in-cluster, server: https://kubernetes.default.svc}` is listed and returned. Its server version and connection state come from one discovery call made once per process; `attemptedAt` is now. A secret for that server overrides the synthetic one, for example to rename it or set namespaces.
  - **Watch**: an added in-cluster secret is delivered as a *modify* of the local cluster; deleting it reverts to the synthetic cluster, also as a modify.
- **Edge cases**:
  - The in-cluster cluster cannot really be removed: the API delete of the synthetic entry returns NotFound `cluster "https://kubernetes.default.svc" not found`.
  - A discovery failure marks the synthetic cluster Failed, with the error message.
- **Configuration surface**: Optional cluster Secret with `server: https://kubernetes.default.svc`.
- **Exposure**: Secret / API / CLI (`cluster add --in-cluster` / `--cluster-endpoint internal`) / UI.
- **Tests required**:
  - Default list contains in-cluster.
  - A secret override renames it.
  - Deleting the override restores the default.
  - Delete with no secret returns NotFound.

### REPO-093 `cluster.inClusterEnabled`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2786 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L101 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L67 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L364 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L319
- **Maturity**: GA
- **Semantics**: An `argocd-cm` key. It is enabled unless the value is exactly `"false"`; it is also enabled when absent or when the ConfigMap is unreadable (a warning is logged). When disabled:
  - the in-cluster cluster, synthetic or secret, is excluded from the list;
  - `GetCluster` returns NotFound `cluster "https://kubernetes.default.svc" is disabled`;
  - creating it returns InvalidArgument `cannot register cluster: in-cluster has been disabled`;
  - name lookups drop it.
- **Edge cases**:
  - Apps targeting in-cluster go to Unknown and cannot sync, and deleting them does not delete their resources (per the upgrade notes).
  - Values other than `"false"` (for example `"no"`) mean enabled.
- **Configuration surface**: `argocd-cm: cluster.inClusterEnabled`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Disabled: the list excludes it, get returns the documented NotFound, create returns the documented error.
  - `"no"` still means enabled.

### REPO-094 Cluster info, connection state and cache info
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2431 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/clusterinfoupdater.go#L126 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/clusterinfoupdater.go#L27 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L473 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L120
- **Maturity**: GA
- **Semantics**: `Cluster.info` contains `connectionState`, `serverVersion`, `cacheInfo` (`resourcesCount`, `apisCount`, `lastCacheSyncTime`), `applicationsCount` and `apiVersions`.
  - **Writer**: the application controller writes it to Redis every `ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT` (default 10s, minimum 10s, maximum 1m), only for clusters of its shard.
  - **Application count**: apps whose destination resolves to the server, excluding apps not permitted by their project's namespace rules.
  - **Status mapping**:
    - no cache info: `Unknown`, with message `Cluster has no applications and is not being monitored.` when the count is 0;
    - no `lastCacheSyncTime`: `Unknown`;
    - a sync error: `Failed`, with the error;
    - otherwise: `Successful`, with the cache info.
  - **API**: the server attaches the cached info to every response and also fills the deprecated top-level `serverVersion` and `connectionState`. API create, update and rotate-auth seed the cache with `Successful` and the server version.
- **Edge cases**: The deprecated fields mirror `info.*`. Missing cache data leaves `info` empty.
- **Configuration surface**: Env `ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT` (controller).
- **Exposure**: API / CLI (`cluster list` VERSION/STATUS/MESSAGE, `cluster get`) / UI.
- **Tests required**:
  - Status transitions.
  - The application count.
  - The deprecated mirror fields.
  - The no-apps message.

### REPO-095 Auto-label cluster with Kubernetes version
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/clusterinfoupdater.go#L173 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L194 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster.md#L144
- **Maturity**: GA
- **Semantics**: If the cluster Secret has the label `argocd.argoproj.io/auto-label-cluster-info: "true"`, the controller writes `argocd.argoproj.io/kubernetes-version: <K8SVersion>` whenever the value differs. The format is `vMajor.Minor.Patch` per the 3.4 upgrade notes.
- **Edge cases**: The update goes through `UpdateCluster`, which adds `managed-by` and replaces the labels and data.
- **Configuration surface**: Cluster Secret labels.
- **Exposure**: Secret.
- **Tests required**: The label appears and is updated after a version change.

### REPO-096 Cluster cache invalidation (`refreshRequestedAt`, InvalidateCache)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L485 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L412 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L878 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.proto#L80
- **Maturity**: GA
- **Semantics**: `POST /api/v1/clusters/{id.value}/invalidate-cache` (RBAC `clusters, update`) sets `refreshRequestedAt=now`, stored as the Secret annotation `argocd.argoproj.io/refresh` in RFC3339. The controller invalidates and re-warms its cache for that cluster if the last sync time is earlier than the request time. Changes to config, namespaces or clusterResources also invalidate it.
- **Edge cases**: An unparseable annotation is logged and ignored.
- **Configuration surface**: Annotation `argocd.argoproj.io/refresh` on the cluster Secret.
- **Exposure**: API / UI ("Invalidate Cache" button on cluster details).
- **Tests required**:
  - Calling invalidate causes a cache resync, with a new `lastCacheSyncTime`.
  - A malformed annotation is ignored.

### REPO-097 Cluster skip-reconcile annotation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/cache.go#L66 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L249 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L631 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/cluster-management.md#L16
- **Maturity**: GA
- **Semantics**: When the cluster Secret has the annotation `argocd.argoproj.io/skip-reconcile` set to a `ParseBool`-true value, the controller treats the cluster as unmanaged by every shard. The cluster is still listed by the API.
- **Edge cases**: An invalid bool means not skipped. The synthetic in-cluster cluster (nil) is always managed.
- **Configuration surface**: Annotation `argocd.argoproj.io/skip-reconcile`.
- **Exposure**: Secret.
- **Tests required**:
  - Apps on an annotated cluster are not reconciled.
  - Removing the annotation resumes reconciliation.

## I. Cluster APIs, CLI and UI

### REPO-098 ClusterService List
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L53 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L92 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2362 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.proto#L44
- **Maturity**: GA
- **Semantics**: `GET /api/v1/clusters?server=&name=&id.type=&id.value=`.
  - **Filtering**: optionally by id (`name`, `name_escaped` (URL-unescaped), or server by default), then by name, then by server. **Each filter returns only the first match.**
  - **RBAC**: `clusters, get, <project>/<server>|<server>`.
  - **Output**: sanitized items (REPO-086 and REPO-088) with info attached.
- **Edge cases**: A name shared by several clusters returns only the first. Unauthorized clusters are silently omitted.
- **Configuration surface**: Query params.
- **Exposure**: API / CLI `argocd cluster list [-o json|yaml|wide|server]` (columns SERVER, NAME, VERSION, STATUS, MESSAGE, PROJECT; the server is shown as `<server> (<n> namespaces)` when namespaced) / UI.
- **Tests required**:
  - Filters.
  - Sanitization.
  - RBAC omission.
  - name_escaped decoding.

### REPO-099 ClusterService Create
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L144 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L101 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2393
- **Maturity**: GA
- **Semantics**: `POST /api/v1/clusters?upsert=`, with body = Cluster.
  1. RBAC `clusters, create, <project>/<server>`.
  2. Build the REST config and **test it with a GetServerVersion call**.
  3. Create the Secret (name per REPO-104).
  4. On AlreadyExists: if the existing cluster `Equals` the request (server, name, namespaces, shard, clusterResources, annotations, labels, config), the call is idempotent; with upsert it updates; otherwise it returns InvalidArgument with the spec diff.
  5. Seed the cluster info cache.
- **Edge cases**:
  - An unreachable cluster fails with `error getting server version: ...`.
  - An RBAC failure is wrapped as `permission denied while creating cluster: ...`.
  - DB AlreadyExists: `cluster "<server>" already exists`.
- **Configuration surface**: Query `upsert`.
- **Exposure**: API / CLI `cluster add` / UI (none for create).
- **Tests required**:
  - Idempotent create.
  - Upsert.
  - A diff error.
  - An unreachable server is rejected.
  - In-cluster disabled is rejected.

### REPO-100 ClusterService Get
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L194 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L226 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.proto#L13
- **Maturity**: GA
- **Semantics**: `GET /api/v1/clusters/{id.value}?id.type=server|name|name_escaped`, or the legacy `server`/`name` query. A server lookup uses the DB; a name lookup scans the list for the first exact name. RBAC `clusters, get`. Returns the sanitized cluster with info.
- **Edge cases**: A missing cluster or RBAC denial returns PermissionDenied (common error), so the API does not reveal existence. A disabled in-cluster cluster also returns PermissionDenied.
- **Configuration surface**: Query params.
- **Exposure**: API / CLI `argocd cluster get SERVER/NAME... [-o json|yaml|wide|server]` (default yaml; an arg matching `^https?://` is a server, otherwise a name) / UI cluster details.
- **Tests required**:
  - Get by server, name and name_escaped.
  - Not-found returns 403.
  - The CLI selector heuristic.

### REPO-101 ClusterService Update (with `updatedFields`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L297 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L269 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L335
- **Maturity**: GA
- **Semantics**: `PUT /api/v1/clusters/{id.value}?updatedFields=...`, with body = Cluster.
  1. Find the existing cluster by id, server or name and check `clusters, update` on it.
  2. If `updatedFields` is empty or contains `project`, also check `update` on the new `<project>/<server>`.
  3. With `updatedFields`, copy only those paths from the request onto the existing cluster. Supported paths: `name`, `namespaces`, `config`, `shard`, `clusterResources`, `labels`, `annotations`, `project`. Unknown paths are ignored. Without `updatedFields`, the request fully replaces the cluster.
  4. Test connectivity with GetServerVersion.
  5. Update the Secret: data and labels are replaced, and if the Secret is missing it is created.
  6. Seed the cluster info.
- **Edge cases**: A full replace without `config` wipes credentials. Renaming via the CLI uses an id of type name.
- **Configuration surface**: Query `updatedFields`.
- **Exposure**: API / CLI `argocd cluster set NAME [--name] [--namespace]... [--label] [--annotation]` (`--namespace '*'` means all) / UI (edit name, namespaces, labels, annotations).
- **Tests required**:
  - A partial update of the namespaces keeps the config.
  - Changing the project requires RBAC in the target project.
  - An unreachable cluster blocks the update.

### REPO-102 ClusterService Delete
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L351 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L377 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L360
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/clusters/{id.value}`. The cluster is resolved (PermissionDenied if missing).
  - **By name**: **every** server with that name is deleted, each checked against `clusters, delete` with the project of the first resolved cluster.
  - **By server**: only that server.
  - **Secret handling**: REPO-027 delete semantics apply; a secret without `managed-by` is un-labelled.
- **Edge cases**: Deleting by a name shared by several clusters deletes all of them. Apps targeting the cluster go to Unknown health.
- **Configuration surface**: n/a.
- **Exposure**: API / CLI `argocd cluster rm SERVER/NAME... [-y]` (y/n/A prompt; afterwards it also uninstalls the argocd-manager RBAC from the cluster using the local kubeconfig context named after the cluster) / UI (Delete).
- **Tests required**:
  - Delete by name with duplicates.
  - An unmanaged secret is kept.
  - RBAC.
  - The CLI RBAC-uninstall step fails gracefully when the context is missing; upstream exits non-zero after the cluster was already removed.

### REPO-103 ClusterService RotateAuth
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L386 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L331 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L343 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L382 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.proto#L75
- **Maturity**: GA
- **Semantics**: `POST /api/v1/clusters/{id.value}/rotate-auth`, with RBAC `clusters, update` on each target server. The handler:
  1. Requires a bearer token (`Cluster '<s>' does not use bearer token authentication`).
  2. Parses it unverified as a legacy SA token JWT to read its namespace, secret name and SA name.
  3. Creates a new `kubernetes.io/service-account-token` Secret, with GenerateName derived from the old name minus its last `-` segment and the old annotations copied, and waits up to 30s for the token (`timed out waiting for secret to generate new token`).
  4. Clears the cert and key data, stores the new token, tests GetServerVersion, saves the cluster and seeds the info.
  5. Updates the SA's `secrets` list and deletes the old token secret.
- **Edge cases**:
  - Bound or projected tokens without a secret-name claim fail at the secret Get.
  - The rotation only works for SA-token-secret based auth.
- **Configuration surface**: n/a.
- **Exposure**: API / CLI `argocd cluster rotate-auth SERVER/NAME`.
- **Tests required**:
  - Against a kind cluster with a legacy token secret: the new token works and the old secret is deleted.
  - A non-token cluster is rejected.

### REPO-104 Cluster Secret naming and API write semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/secrets.go#L162 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L375
- **Maturity**: GA
- **Semantics**:
  - **Naming**: secrets created by the API are named `cluster-<host>-<fnv32a(serverURL)>`. The host is lowercased and the port stripped; IPv6 hosts are unbracketed with `:` replaced by `-`.
  - **Writes**: every API write **replaces** the Secret's `data` with only the known keys, so unknown keys are dropped. Labels become `Cluster.labels` plus secret-type. Annotations become `Cluster.annotations` plus `managed-by` (and refresh when set).
- **Edge cases**: A server URL that fails `url.ParseRequestURI` returns an error on create. Updating a declarative secret through the API marks it `managed-by`, so a later API delete removes it.
- **Configuration surface**: n/a.
- **Exposure**: API / `admin cluster generate-spec` (prints a secret with this name).
- **Tests required**:
  - The name format for IPv4 with a port and for IPv6.
  - Extra data keys are dropped on update.

### REPO-105 Kubernetes client tuning for managed clusters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/cluster_constants.go#L14 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/cluster_constants.go#L45 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3902
- **Maturity**: GA
- **Semantics**: Environment variables and their defaults:
  - `ARGOCD_K8S_CLIENT_QPS`: 50
  - `ARGOCD_K8S_CLIENT_BURST`: 2×QPS
  - `ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS`: 500; also used for max conns per host
  - `ARGOCD_K8S_TCP_TIMEOUT`: 30s
  - `ARGOCD_K8S_TCP_KEEPALIVE`: 30s
  - `ARGOCD_K8S_TLS_HANDSHAKE_TIMEOUT`: 10s
  - `ARGOCD_K8S_TCP_IDLE_TIMEOUT`: 5m
  - `ARGOCD_K8S_SERVER_SIDE_TIMEOUT`: 0
  - `ARGOCD_K8SCLIENT_RETRY_MAX`: 0, which disables retries
  - `ARGOCD_K8SCLIENT_RETRY_BASE_BACKOFF`: 100ms

  `SetK8SConfigDefaults` builds a custom transport that honours the cluster proxy and then clears the TLS, auth and exec fields on the config, because they are baked into the transport.
- **Edge cases**: Values outside the allowed range fall back to the defaults (env parser bounds).
- **Configuration surface**: The env vars above.
- **Exposure**: Deployment env.
- **Tests required**: Configured values are reflected in the generated transport; retries happen when `RETRY_MAX` > 0.

### REPO-106 CLI `argocd cluster add`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L85 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L71 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L128 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/cluster.go#L181 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_cluster_add.md#L11 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/cluster-management.md#L7
- **Maturity**: GA
- **Semantics**: `argocd cluster add CONTEXT` (with no args it prints the kubeconfig contexts and exits 1).
  - **Flags**: `--kubeconfig`, `--upsert`, `--service-account`, `--system-namespace` (default `kube-system`), `-y/--yes`, `--label`, `--annotation`, `--proxy-url`, `--in-cluster`, the `--aws-*` flags, `--namespace`, `--cluster-resources`, `--name`, `--project`, `--shard` (default -1), the `--exec-command*` flags, `--cluster-endpoint kubeconfig|kube-public|internal`, and `--disable-compression`.
  - **Auth selection**:
    1. With `--aws-cluster-name`: awsAuthConfig.
    2. With `--exec-command`: execProviderConfig.
    3. Otherwise, with `--service-account`: read that SA's token (create a `<sa>-long-lived-token` secret if needed).
    4. Otherwise, after a confirmation prompt on a TTY (`WARNING: This will create a service account argocd-manager ... with full <cluster|namespace> level privileges`), install the argocd-manager RBAC (REPO-107).
  - **Endpoint**: `--in-cluster` or `internal` sets the server to `https://kubernetes.default.svc`. `kube-public` reads the `kube-public/cluster-info` ConfigMap for the endpoint and CA, falling back to the kubeconfig server with a warning.
  - **Request**: calls ClusterService.Create with upsert and prints `Cluster '<server>' added`.
- **Edge cases**:
  - `--in-cluster` combined with `--cluster-endpoint` is fatal: `Can only use one of --in-cluster or --cluster-endpoint`.
  - An unknown context gives `context <c> does not exist in kubeconfig`.
  - The proxy URL is validated with the same schemes as REPO-090.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Against kind: the SA, ClusterRole, binding and token secret are created and the cluster is registered.
  - `--namespace` creates Roles.
  - `--service-account` reuses an SA.
  - The AWS and exec variants store the right config.
  - The kube-public fallback.

### REPO-107 argocd-manager RBAC bootstrap (`clusterauth`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L22 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L179 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L224 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L258 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/clusterauth/clusterauth.go#L287
- **Maturity**: GA
- **Semantics**:
  - **ServiceAccount**: `argocd-manager` is created in the system namespace; already existing is fine.
  - **Without namespaces**: ClusterRole `argocd-manager-role` (`*/*/*` plus nonResourceURLs `*`) and ClusterRoleBinding `argocd-manager-role-binding` are upserted (create, or update on conflict).
  - **With namespaces**: a Role `argocd-manager-role` (`*/*/*`) and RoleBinding are upserted in each namespace.
  - **Token**: `argocd-manager-long-lived-token` (type `kubernetes.io/service-account-token`) is created or reused, then polled every 500ms until `data.token` appears. The timeout is `BearerTokenTimeout` (30s), with a 10s per-request timeout.
  - **Uninstall**: deletes the ClusterRoleBinding, ClusterRole and the SA in `kube-system`, tolerating NotFound.
- **Edge cases**:
  - Uninstall always targets `kube-system`, even if a different `--system-namespace` was used at install, and does not remove namespaced Roles.
  - A token timeout fails with `failed to get token for serviceaccount "argocd-manager": ...`.
- **Configuration surface**: n/a.
- **Exposure**: CLI (`cluster add`, `cluster rm`, `admin cluster generate-spec --generate-bearer-token`).
- **Tests required**:
  - The installed objects match exactly.
  - Re-running is idempotent.
  - Uninstall behaviour with a custom system namespace (parity).

### REPO-108 CLI `argocd cluster` get / list / set / rm / rotate-auth
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L50 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L235 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L324 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L399 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L502 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/cluster.go#L549
- **Maturity**: GA
- **Semantics**: Subcommands: `add` (REPO-106), `get`, `list`, `set`, `rm` and `rotate-auth`.
  - **`get -o wide`** prints: Server URL, Server Name, Server Version, Namespaces ("all namespaces" when empty), Client cert, Cert validation, Basic authentication, oAuth authentication, AWS authentication, Disable compression and Use proxy.
  - **`set`** sends `updatedFields` for name, namespaces, labels and annotations when they are provided. With no fields it prints `Specify the cluster field to be updated.`
- **Edge cases**:
  - A `set` that gets PermissionDenied logs `Ensure that the cluster is present and you have the necessary permissions to update the cluster`.
  - A `rotate-auth` without exactly one argument prints help and exits 1.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Golden outputs for each subcommand.
  - The rm prompt variants (y/n/A).

### REPO-109 CLI `argocd admin cluster` (generate-spec / kubeconfig / namespaces / stats / shards)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L566 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L520 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L309 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L347 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L402 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L178
- **Maturity**: GA (the sharding semantics behind `stats` and `shards` are in the controller area)
- **Semantics**:
  - **`generate-spec CONTEXT`**: offline. It builds the cluster from the kubeconfig and prints the Secret YAML/JSON. Auth is chosen in order: AWS, exec, `--generate-bearer-token` (installs RBAC), `--bearer-token`, or the placeholder token `bearer-token` when none is given. It takes the same cluster flags plus `--service-account` (default `argocd-manager`), `--system-namespace`, `-o`, `--label` and `--annotation`; `kube-public` is not supported and logs a warning.
  - **`kubeconfig CLUSTER_URL OUTPUT_PATH`**: writes a kubeconfig built from the stored cluster's raw REST config.
  - **`namespaces`**: prints, per cluster, the namespaces actually used by apps.
  - **`namespaces enable-namespaced-mode PATTERN`**: for clusters whose server matches the glob and that use between 1 and `--max-namespace-count` namespaces, sets `namespaces` to those used and sets `clusterResources` from the flag. `--dry-run` defaults to true.
  - **`namespaces disable-namespaced-mode PATTERN`**: clears `namespaces`. `--dry-run` defaults to true.
  - **`stats` and `shards`**: per-shard cluster stats.
- **Edge cases**:
  - The `kubeconfig` examples mention `--delete` and listing, which do not exist; exactly 2 args are required.
  - `enable-namespaced-mode` with `--max-namespace-count 0` (the default) matches nothing.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI (requires direct kube access).
- **Tests required**:
  - generate-spec output equals what API create would write.
  - Namespaced-mode enable and disable with dry-run on and off.

### REPO-110 UI Settings > Clusters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/clusters-list/clusters-list.tsx#L46 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/clusters-list/clusters-list.tsx#L97 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/cluster-details/cluster-details.tsx#L40 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/cluster-details/cluster-details.tsx#L58
- **Maturity**: GA
- **Semantics**: The list page shows each cluster's connection state and offers Delete with a confirmation. The details page shows server, credentials type, name, namespaces, applications count, labels and annotations, all editable through partial update with `updatedFields`, and has an "Invalidate Cache" action. There is no "add cluster" in the UI.
- **Edge cases**: Delete calls delete by server URL.
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**: E2E edit of the name and namespaces; invalidate cache; delete.

## J. `argocd-k8s-auth`

### REPO-111 `argocd-k8s-auth aws`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/aws.go#L38 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/aws.go#L65 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/aws.go#L103 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/aws.go#L130 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L998
- **Maturity**: GA
- **Semantics**: Flags: `--cluster-name`, `--role-arn`, `--profile`.
  1. Load the default AWS config, or the named shared-config profile.
  2. If a role ARN is given, assume it via STS.
  3. Presign `sts:GetCallerIdentity` with the headers `x-k8s-aws-id: <clusterName>` and `X-Amz-Expires: 60`.
  4. Build the token `k8s-aws-v1.` + base64url(no padding) of the presigned URL.
  5. Print an `ExecCredential` (`client.authentication.k8s.io/v1beta1`) whose `expirationTimestamp` is now + 14 minutes.

  Presigning is retried every 5s for up to 1 minute.
- **Edge cases**: A timeout fails with `timeout while trying to get signed aws request: last error: ...`. A config load failure gives `error loading AWS configuration: ...`.
- **Configuration surface**: Flags. Standard AWS env, IRSA and Pod Identity.
- **Exposure**: Binary invoked by the exec provider (REPO-087, REPO-088).
- **Tests required**:
  - The token format decodes to the STS URL containing `x-k8s-aws-id`.
  - The role ARN path works with static credentials.
  - The retry and timeout behaviour.

### REPO-112 `argocd-k8s-auth gcp`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/gcp.go#L19 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/gcp.go#L24 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1130
- **Maturity**: GA
- **Semantics**: Uses Google application default credentials (Workload Identity) with the scopes `cloud-platform` and `userinfo.email`, and prints an ExecCredential with the access token and its expiry.
- **Edge cases**: Missing credentials are fatal (CheckError exits non-zero).
- **Configuration surface**: The GCP ADC environment.
- **Exposure**: Binary.
- **Tests required**: With a fake ADC file, the output JSON shape is correct.

### REPO-113 `argocd-k8s-auth azure`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/azure_cgo.go#L15 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/azure_cgo.go#L24 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-k8s-auth/commands/azure_no_cgo.go#L17 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1161
- **Maturity**: GA
- **Semantics**: Uses kubelogin's token provider with options taken from the environment (`AAD_LOGIN_METHOD`, `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_CLIENT_CERTIFICATE_PATH`/`_PASSWORD`, `AAD_USER_PRINCIPAL_NAME`/`_PASSWORD`, `AZURE_AUTHORITY_HOST`, `AZURE_FEDERATED_TOKEN_FILE`). The login method defaults to workload identity when unset. The server ID defaults to `6dae42f8-4368-4678-94ff-3960e28e3630` and can be overridden with `AAD_SERVER_APPLICATION_ID`. `AAD_ENVIRONMENT_NAME` sets the cloud. It prints an ExecCredential.
- **Edge cases**: On darwin builds without cgo, the command fatally exits with the workload-identity CGO error.
- **Configuration surface**: The env vars above (usually set via `execProviderConfig.env`).
- **Exposure**: Binary.
- **Tests required**: With mocked env (an SPN against a mock AAD), the output token is correct; the default login method is workload identity.

## K. API-server webhooks (`/api/webhook`)

### REPO-114 Webhook endpoint and HTTP semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1261 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1263 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L807 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L868 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L60 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L23
- **Maturity**: GA
- **Semantics**: argocd-server serves an unauthenticated handler at `/api/webhook`.
  - **Dispatch**: the first parser whose `CanHandle(headers)` matches handles the request. Parser order: Azure DevOps (`X-Vss-Activityid`), Gogs (`X-Gogs-Event`; must come before GitHub because Gogs also sends GitHub headers), GitHub (`X-GitHub-Event` other than `package`), GitLab (`X-Gitlab-Event`), Bitbucket Cloud (`X-Hook-UUID`), Bitbucket Server (`X-Event-Key`), then GHCR (`X-GitHub-Event: package`).
  - **Responses**:

    | Condition | Status | Body |
    |---|---|---|
    | No parser matches | 400 | `Unknown webhook event` |
    | Registry HMAC failure | 401 | `Unauthorized` |
    | Parse error (too large or invalid JSON) | 400 | `Webhook processing failed: payload must be valid JSON under <N> MB` |
    | Any other parse error, POST | 400 | `Webhook processing failed` |
    | Any other parse error, non-POST | 405 | `Webhook processing failed` |
    | Claimed but skipped (nil payload) | 200 | — |
    | Successfully enqueued | 200 | — |
    | Queue full (50,000 entries) | 503 | `Queue is full, discarding webhook payload` |
  - **Processing**: payloads are processed asynchronously by a worker pool, and each handler run is guarded against panics.
- **Edge cases**:
  - Provider signature failures (GitHub, GitLab, Gogs, Bitbucket, Bitbucket Server, Azure) return 400 and a security-tagged log line. Only the GHCR HMAC failure returns 401.
  - A provider whose library constructor errors is skipped, which yields 400 Unknown event for its requests.
  - On graceful shutdown the queues are drained.
- **Configuration surface**: argocd-server flags `--webhook-parallelism-limit` (env `ARGOCD_SERVER_WEBHOOK_PARALLELISM_LIMIT`, cmd-params `server.webhook.parallelism.limit`, default 50, range 1–1000) and `--webhook-refresh-workers` (REPO-129).
- **Exposure**: HTTP endpoint (not gRPC, not in swagger).
- **Tests required**:
  - Every row of the status code table, using the upstream test cases (`TestInvalidMethod`, `TestInvalidEvent`, `TestUnknownEvent`).
  - Queue overflow returns 503.
  - A panic in a handler does not crash the server.

### REPO-115 `webhook.maxPayloadSizeMB`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2677 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L575 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L808 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L451 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L30
- **Maturity**: GA
- **Semantics**: An `argocd-cm` integer (MB), default 50. The request body is wrapped in `http.MaxBytesReader(limit*1024*1024)`. Exceeding the limit causes a parse error, a 400 with the "payload must be valid JSON under N MB" body, and the log `Webhook processing failed: payload too large or corrupted (limit N MB)`.
- **Edge cases**:
  - A non-integer value is logged and the default is used.
  - The value is read when the webhook handler is constructed at server start.
  - Limits below 1 MB display as `0 MB` because of integer division.
- **Configuration surface**: `argocd-cm: webhook.maxPayloadSizeMB`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - A 100-byte limit rejects a normal GitHub payload with 400.
  - An invalid value falls back to 50.

### REPO-116 Webhook secrets in `argocd-secret`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L450 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1756 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1985 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L812 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L54 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L111
- **Maturity**: GA
- **Semantics**: Keys in `argocd-secret`: `webhook.github.secret`, `webhook.gitlab.secret`, `webhook.bitbucket.uuid`, `webhook.bitbucketserver.secret`, `webhook.gogs.secret`, `webhook.azuredevops.username`, `webhook.azuredevops.password`.
  - **Indirection**: values may be `$<secretName>:<key>` references to another Secret labelled `app.kubernetes.io/part-of: argocd`.
  - **Optional**: secrets are optional. When unset, events are accepted unauthenticated; they are considered safe because they only trigger refreshes.
  - **Changes**: a change to the GitHub, GitLab, Bitbucket UUID, Bitbucket Server or Gogs secret restarts the API server's HTTP stack so that new parsers are built.
- **Edge cases**: A change to the Azure DevOps username or password does **not** trigger the restart, so the new value applies only after the next pod or server restart (see the Doc/implementation differences section).
- **Configuration surface**: The `argocd-secret` keys above, or `$secret:key` references.
- **Exposure**: Secret.
- **Tests required**:
  - Set a GitHub secret: unsigned requests are rejected and signed ones accepted, without a pod restart.
  - A `$ref:key` indirection works.
  - The Azure credential rotation behaviour (parity).

### REPO-117 GitHub webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L69 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L258 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L32
- **Maturity**: GA
- **Semantics**: Accepts `push` and `ping` events. Other events cause a parse error (400).
  - **Push**: `webURL` = `repository.html_url`, revision = last segment of `ref` after splitting `refs/x/` (for example `refs/heads/main` gives `main` and `refs/tags/v1.0` gives `v1.0`), `shaBefore`/`shaAfter` = `before`/`after`, `touchedHead` = (default_branch == revision), and changed files = added + modified + removed across all commits.
  - **Ping**: answered 200 and then ignored (logged `Ignoring webhook event`).
  - **Signature**: verified by the go-playground library with `webhook.github.secret`.
  - **Content type**: must be `application/json`.
- **Edge cases**: A form-encoded payload fails. A push to a tag never touches HEAD.
- **Configuration surface**: `webhook.github.secret`.
- **Exposure**: HTTP.
- **Tests required**:
  - Upstream `TestGitHubCommitEvent`, `TestGitHubTagEvent` and `TestGitHubPingEvent` log lines and status codes.
  - Signature mismatch returns 400.

### REPO-118 GitLab webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L88 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L270 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L282
- **Maturity**: GA
- **Semantics**: Accepts Push Hook, Tag Push Hook and System Hook events; system hooks are routed to push and tag payloads. `webURL` = `project.web_url`. Revision, SHAs, touchedHead (from `project.default_branch`) and changed files work as for GitHub. The token is verified against `webhook.gitlab.secret`, and failures log `GitLab webhook token verification failed`.
- **Edge cases**: Tag event handling is marked "untested" upstream.
- **Configuration surface**: `webhook.gitlab.secret`.
- **Exposure**: HTTP.
- **Tests required**:
  - `TestGitLabPushEvent` and `TestGitLabSystemEvent`.
  - A token mismatch returns 400.

### REPO-119 Bitbucket Cloud webhook (with diffstat callback)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L104 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L295 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L730 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L768 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L795 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L122
- **Maturity**: GA
- **Semantics**: Accepts `repo:push`. `webURL` = `repository.links.html.href`. The revision and SHAs come from the **first** change only. `touchedHead` defaults to true.
  - **Diffstat callback**: only when `webhook.bitbucket.uuid` is set, and therefore the UUID was verified.
    1. Look up the configured repository whose URL matches `webURL` with `SameURL`. If none is configured, use a no-auth client.
    2. Build a Bitbucket client:
       - username `x-token-auth` plus a password: OAuth bearer with the password;
       - any other username plus a password: basic auth;
       - otherwise: OAuth bearer with the repository's `bearerToken`, possibly empty.
    3. Derive the API base from `links.self.href` minus `/repositories/<full_name>`.
    4. Call diffstat `<before>..<after>` with renames to get the changed files.
    5. Get the repository's main branch to compute `touchedHead`; on error, assume true.
  - **Timeout**: 10s.
- **Edge cases**:
  - No UUID configured: no callback, so changed files are empty and every app refreshes.
  - An invalid `full_name` logs `error parsing bitbucket repository full name` and skips the callback.
  - A diffstat failure leaves changed files empty.
- **Configuration surface**: `webhook.bitbucket.uuid`. Repository `username`/`password`/`bearerToken` used for the callback.
- **Exposure**: HTTP.
- **Tests required**:
  - Upstream `Test_affectedRevisionInfo_bitbucket_changed_files`, `TestCreateBitbucketClient`, `TestFetchDiffStatBitbucketClient`, `TestIsHeadTouched`.
  - A UUID mismatch is rejected.

### REPO-120 Bitbucket Server webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L120 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L354
- **Maturity**: GA
- **Semantics**: Accepts `repo:refs_changed` and `diagnostics:ping` (the ping is ignored).
  - **webURLs**: every `links.clone[]` entry named `http` or `ssh`, so several URLs, each matched separately.
  - **Revision**: from the first change's `reference.id`.
  - **touchedHead**: always true.
  - **Changed files**: none, so all matching apps refresh.
  - **Signature**: HMAC with `webhook.bitbucketserver.secret`.
- **Edge cases**: Clone URLs end in `.git`. Matching uses the path including `.git`, so an app `repoURL` without `.git` does not match (REPO-124).
- **Configuration surface**: `webhook.bitbucketserver.secret`.
- **Exposure**: HTTP.
- **Tests required**: `TestBitbucketServerRepositoryReferenceChangedEvent` (both SSH and HTTPS URLs logged) and the ping test.

### REPO-121 Gogs webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L53 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L384
- **Maturity**: GA
- **Semantics**: Accepts `push`. `webURL` = `repo.html_url` (only if `repo` is present). The revision, SHAs, touchedHead (from the default branch) and changed files work as for GitHub. HMAC with `webhook.gogs.secret`. Gogs requests are claimed before the GitHub parser.
- **Edge cases**: A payload without `repo` means no URL, and the event is ignored.
- **Configuration surface**: `webhook.gogs.secret`.
- **Exposure**: HTTP.
- **Tests required**: `TestGogsPushEvent`; a Gogs request carrying GitHub headers is routed to Gogs.

### REPO-122 Azure DevOps webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/scm.go#L35 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L248 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L39
- **Maturity**: GA
- **Semantics**: Accepts `git.push`. `webURL` = `resource.repository.remoteUrl`. The revision and SHAs come from the first `refUpdates` entry. `touchedHead` = (`refUpdates[0].name` == `repository.defaultBranch`). There are no changed files, so all matching apps refresh. Optional basic auth uses `webhook.azuredevops.username` and `webhook.azuredevops.password`.
- **Edge cases**: An empty `refUpdates` yields an empty revision; only apps targeting HEAD are affected if touchedHead is false.
- **Configuration surface**: The two `argocd-secret` keys.
- **Exposure**: HTTP.
- **Tests required**:
  - `TestAzureDevOpsCommitEvent`.
  - Wrong basic auth returns 400.

### REPO-123 GHCR (OCI registry) webhook
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/ghcr.go#L48 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/ghcr.go#L72 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/ghcr.go#L116 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/registry.go#L55 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/registry.go#L133 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L136
- **Maturity**: GA
- **Semantics**: Handles `X-GitHub-Event: package`.
  - **Signature**: when `webhook.github.secret` is set, `X-Hub-Signature-256` must equal `sha256=<hex HMAC-SHA256(body)>`, compared in constant time. A missing or mismatched signature returns 401. Without a secret, a startup warning is logged and events are accepted.
  - **Payload**: only `action: published` with `package_type: container` (case-insensitive) and a non-empty tag is processed. Anything else returns 200 and is skipped.
  - **Event**: a RegistryEvent `{ghcr.io, <owner>/<name>, <tag>}`.
  - **Matching**: apps from all allowed namespaces whose source or hydrator dry source `repoURL` normalizes (strip `oci://`, strip trailing `/`, lowercase) to `ghcr.io/<owner>/<name>`, and whose `targetRevision` equals the tag or is a semver constraint satisfied by it. Matching apps are refreshed synchronously with the normal refresh **and** hydrate annotations.
- **Edge cases**:
  - Invalid JSON: `failed to unmarshal GHCR webhook payload` (400).
  - GitHub does not send events for unknown media types.
  - No cache warming or jitter for registry events.
- **Configuration surface**: `webhook.github.secret`.
- **Exposure**: HTTP.
- **Tests required**:
  - The semver table from the docs (`1.0.0`, `^1.2.0`, `~1.2.0`, `>=1.0.0`).
  - URL normalization variants.
  - Unsigned with a secret returns 401.
  - A non-container package returns 200 with no refresh.

### REPO-124 Webhook repository URL matching (`GetWebURLRegex`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L548 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L571 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L717 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L58
- **Maturity**: GA
- **Semantics**: From the payload's web URL a case-insensitive regex is built: `^((https?|ssh)://)?(<user>@)?((alt)?ssh\.)?<escaped host>(:\d+)?[:/]<escaped path>(\.git)?$`, where the user part is `[\w\.][\w\.-]{0,30}[\w\.\$-]?`. An app source matches when its `repoURL` matches. As a result, https/ssh/scp forms, ports, `ssh.`/`altssh.` subdomains and optional `.git` are all equivalent.
- **Edge cases**:
  - Dots are escaped, so `an.example.com` does not match `an-example.com`.
  - Unknown subdomains do not match.
  - A username starting with `-` is rejected.
  - An unparseable URL gives `failed to parse URL '<u>'`, and that web URL is skipped.
  - An empty URL does not panic.
- **Configuration surface**: n/a.
- **Exposure**: internal.
- **Tests required**: The full upstream `Test_GetWebURLRegex` table.

### REPO-125 Webhook revision matching
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L239 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L680 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L695 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/webhook.md#L18
- **Maturity**: GA
- **Semantics**: For each source:
  - **targetRevision is empty or `HEAD`** (after `ParseRevision`): it changed iff `touchedHead`.
  - **targetRevision starts with `refs/heads/` or `refs/tags/`**: compare the last path segment with the pushed revision.
  - **Otherwise**: compare the full targetRevision with the pushed revision.
  - **Comparison**: equal strings match. Otherwise, if targetRevision is a valid semver constraint and the pushed revision a valid semver version, the constraint check decides.
- **Edge cases**:
  - Branch and tag names are not distinguished: a push to branch `x` refreshes apps on `refs/tags/x`.
  - `three/part/rev` does not match a pushed `rev`.
  - `env/test` matches `env/test`.
- **Configuration surface**: n/a.
- **Exposure**: internal.
- **Tests required**: The upstream `TestAppRevisionHasChanged` table (15 cases).

### REPO-126 Webhook changed-path filtering (`manifest-generate-paths`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L115 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L155 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L499 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L333
- **Maturity**: GA
- **Semantics**: A source that matches on both URL and revision triggers a refresh only if `AppFilesHaveChanged(refreshPaths, changedFiles)`.
  - **Always refresh when**: the payload has no changed files (Bitbucket Server, Azure DevOps, Bitbucket Cloud without a UUID) **or** the app has no refresh paths.
  - **Refresh paths**: the semicolon-separated entries of the app annotation `argocd.argoproj.io/manifest-generate-paths`, each trimmed with empties skipped. An absolute entry (`/x`) is repo-root relative; a relative entry is joined to `source.path` and cleaned.
  - **Hydrator sync source**: its own path is used and the annotation is ignored.
  - **A changed file matches** when it is equal to a refresh path, under it, or matches it as a glob (`filepath.Match`).
  - **Short-circuit**: once one source triggers a refresh, the remaining sources of that app are skipped.
- **Edge cases**: An annotation `.` means the source path itself. The docs claim support only for GitHub, GitLab and Gogs, but the code also supports Bitbucket Cloud through diffstat.
- **Configuration surface**: Application annotation `argocd.argoproj.io/manifest-generate-paths`.
- **Exposure**: CRD annotation.
- **Tests required**:
  - Relative, absolute and glob paths.
  - Multiple paths with spaces.
  - Empty changed files refresh everything.
  - A change outside the paths does not refresh.

### REPO-127 Webhook handling for the source hydrator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L492 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L503 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L220 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L240
- **Maturity**: Beta
- **Semantics**: For apps with `spec.sourceHydrator`, the dry source is added to the list of candidate sources. If the triggering source equals the dry source, the refresh request also sets `argocd.argoproj.io/hydrate: normal`, and the log says `webhook trigger refresh app to hydrate`. Refreshes are a merge patch of `argocd.argoproj.io/refresh: normal`, retried up to 5 times on conflict at 100ms intervals.
- **Edge cases**: A push to the hydrated (sync) branch refreshes but does not hydrate.
- **Configuration surface**: n/a.
- **Exposure**: internal (annotations on the Application CRD).
- **Tests required**: Upstream `TestProcessAppRefresh` ("processes valid hydrate request"); a dry-source push sets both annotations.

### REPO-128 Webhook manifest-cache warming
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L531 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L589 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L65 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L474
- **Maturity**: GA
- **Semantics**: When a source matches on URL and revision but its files did **not** change, and both `before` and `after` SHAs are present and warming is not disabled (globally by `ARGOCD_WEBHOOK_MANIFEST_CACHE_WARM_DISABLED=true` on argocd-server, or per repo by REPO-025), the handler copies the cached manifests from the old revision key to the new one.
  - **Cache key inputs**: destination namespace, tracking method, instance label key, app name, installation ID, the project's effective source integrity, and the destination cluster info from the server cache.
  - **Cache miss**: logged at info level; returns nil.
  - **Errors**: non-fatal, logged as a warning.
- **Edge cases**:
  - Multi-source apps with ref sources are not supported (`moving manifest cache is currently not supported for multi-source applications`).
  - A missing project or cluster info gives an error, logged as a warning.
- **Configuration surface**: Env `ARGOCD_WEBHOOK_MANIFEST_CACHE_WARM_DISABLED`. Repo `webhookManifestCacheWarmDisabled`.
- **Exposure**: internal / Prometheus metric (REPO-131).
- **Tests required**:
  - Upstream `Test_storePreviouslyCachedManifests`.
  - With the global env set, no cache calls are made.

### REPO-129 Webhook refresh jitter and worker pools
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L524 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L188 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L204 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2696 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2716 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L338 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L519
- **Maturity**: GA
- **Semantics**: Refresh requests go into a delaying work queue processed by `server.webhook.refresh.workers` workers (default 20, range 1–1000, env `ARGOCD_SERVER_WEBHOOK_REFRESH_WORKERS`). Within one webhook URL, once more than `webhook.refresh.jitter.threshold` apps (default 10) have been affected, each additional app is delayed by a uniform random duration in `[0, webhook.refresh.jitter)`. Jitter is off by default (0) and is parsed with Argo CD duration parsing. Payload parsing uses `server.webhook.parallelism.limit` workers (default 50).
- **Edge cases**:
  - A negative threshold logs a warning and the default is used.
  - An unparseable jitter means 0.
  - The first `threshold` apps are always immediate.
- **Configuration surface**: `argocd-cm: webhook.refresh.jitter`, `webhook.refresh.jitter.threshold`. `argocd-cmd-params-cm: server.webhook.refresh.workers`, `server.webhook.parallelism.limit`.
- **Exposure**: ConfigMaps / flags.
- **Tests required**: Upstream `TestWebhookRefreshWithJitter` (delayed after the threshold, none below it).

### REPO-130 Webhook app namespace filtering
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L432 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L461
- **Maturity**: GA
- **Semantics**: Apps in the control-plane namespace are always considered. When apps-in-any-namespace is configured, apps from all namespaces are listed and kept only if their namespace matches the configured application namespace patterns (glob or regex list).
- **Edge cases**: Apps in non-allowed namespaces are never refreshed by webhooks.
- **Configuration surface**: argocd-server `--application-namespaces` (cmd-params `application.namespaces`).
- **Exposure**: internal.
- **Tests required**: Upstream `TestGitHubCommitEvent_AppsInOtherNamespaces`.

### REPO-131 Webhook metrics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L66 , https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L455
- **Maturity**: GA
- **Semantics**: The metrics are:
  - `argocd_webhook_requests_total{repo}`: counter per normalized web URL;
  - `argocd_webhook_store_cache_attempts_total{repo,successful}`: cache warming attempts, labelled by the normalized source repoURL;
  - `argocd_webhook_handlers_in_flight`: gauge.
- **Edge cases**: A cache miss counts as `successful="false"`.
- **Configuration surface**: n/a.
- **Exposure**: Prometheus metrics on argocd-server.
- **Tests required**: The counters increment as expected for a push with and without changed paths.

---

## Unconfirmed

- **Library-level signature header and algorithm details** for GitHub (`X-Hub-Signature` sha1 vs `X-Hub-Signature-256`), GitLab (`X-Gitlab-Token`), Gogs, Bitbucket Server HMAC and Bitbucket Cloud UUID. These live in the vendored `github.com/go-playground/webhooks/v6 v6.4.0`, which is not part of the checkout. Checked: `go.mod`, `util/webhook/scm.go`. Patrol must verify against that library version.
- **Whether `webhook.maxPayloadSizeMB`, `webhook.refresh.jitter` and the threshold take effect without a pod restart.** They are read when `webhook.NewHandler` is constructed (`server/server.go#L1261`), and that handler is rebuilt only when the API server's HTTP stack restarts. The restart is triggered by a fixed list of settings changes (`server/server.go#L812-L863`) that does not include these keys. I did not trace the full `Run` restart loop to confirm whether some other path rebuilds the handler.
- **Exact UI behaviour for write credentials, and whether the UI hides the "write" toggle when the hydrator is disabled.** Checked: `ui/src/app/settings/components/repos-list/repos-list.tsx` (fields only).
- **GitLab System Hook payload routing to push/tag types** is done inside go-playground's gitlab parser and was not inspected. It is confirmed only through the upstream test `TestGitLabSystemEvent` (`util/webhook/webhook_test.go`).
- **The `repo-details` UI edit flow**: which fields are editable after creation was not fully traced. Checked: `ui/src/app/settings/components/repo-details/repo-details.tsx` exists; not read in detail.
- **Cluster `Get`/`Update` error wrapping**: the handlers wrap `common.PermissionDeniedAPIError` with `fmt.Errorf` (`server/cluster/cluster.go#L197`, `#L304`). Whether the gRPC status code is preserved end-to-end, so that HTTP returns 403 rather than 500 or 2/Unknown, depends on grpc's unwrap support. This was not verified at runtime.
- **SSH known-hosts format details beyond `[host]:port`**, such as `@cert-authority` and `@revoked` markers: there is no explicit handling in `util/cert/cert.go`, so behaviour depends on `golang.org/x/crypto/ssh.ParseKnownHosts` and on the OpenSSH client.

## Doc/implementation differences

- **repo-creds Azure SP key casing.** Repository Secrets use `azureServicePrincipalClientId` / `azureServicePrincipalTenantId` (`util/db/repository_secrets.go#L348-L350`, `#L458-L460`). repo-creds Secrets use `azureServicePrincipalClientID` / `azureServicePrincipalTenantID` (`#L484`, `#L486`, `#L555-L557`). The docs show only the `...Id` form (`docs/operator-manual/declarative-setup.md#L312-L313`). A declarative repo-creds written with the documented casing does **not** load the client and tenant ID, so the Service Principal is never selected. Code is the baseline.
- **`repo-creds` `project` field.** `docs/operator-manual/argocd-repo-creds.yaml#L75` shows `project:` on a repo-creds Secret, but `RepoCreds` has no project field and the key is ignored.
- **`argocd repo edit` does not exist.** `docs/operator-manual/high_availability.md#L494` says to run `argocd repo edit ... --webhook-manifest-cache-warm-disabled` "via the ArgoCD CLI or UI". There is no `repo edit` command (`cmd/argocd/commands/repo.go#L49-L52`), and the UI does not expose the field. It can only be set with `repo add` (with `--upsert`), the API or the Secret.
- **Webhook path filtering providers.** `docs/operator-manual/high_availability.md#L366-L368` says path filtering works only for GitHub, GitLab and Gogs. The code also provides changed files for Bitbucket Cloud through the diffstat callback when `webhook.bitbucket.uuid` is set (`util/webhook/webhook.go#L312-L349`), which `docs/operator-manual/webhook.md#L122-L134` also documents.
- **OCI registry webhooks.** `docs/operator-manual/webhook.md#L12-L14` speaks generically of "OCI-compliant container registries", but only the GHCR parser is implemented (`util/webhook/webhook.go#L158`, `util/webhook/ghcr.go`).
- **Azure DevOps webhook credential changes.** `docs/operator-manual/webhook.md#L109` says changes "should take effect automatically". The server restart-on-change list covers GitHub, GitLab, Bitbucket UUID, Bitbucket Server and Gogs, but not the Azure DevOps username or password (`server/server.go#L812-L863`).
- **`argocd cluster add --disable-compression` is a no-op.** The flag is parsed (`cmd/util/cluster.go#L197`) but never applied; `NewCluster` uses `conf.DisableCompression` from the kubeconfig (`cmd/util/cluster.go#L104`).
- **`argocd admin repo generate-spec` ignores many flags.** The GitHub App, GCP, proxy, noProxy, forceHttpBasicAuth, depth and Azure SP flags are registered but not mapped (`cmd/argocd/commands/admin/repo.go#L146-L152`), even though its examples show GitHub App and GCP usage (`#L81-L88`).
- **`argocd admin cluster kubeconfig` examples.** The examples list `--delete` and a listing mode with no arguments (`cmd/argocd/commands/admin/cluster.go#L526-L537`), but the command requires exactly 2 args and has no `--delete` flag.
- **`argocd repocreds list` columns.** It shows SSH_CREDS and TLS_CREDS columns, but the API returns only url and username (`server/repocreds/repocreds.go#L49-L52`), so both columns are always `false`.
- **ListWriteRepositoryCredentials.** It enumerates **read** template URLs and filters by a non-empty password (`server/repocreds/repocreds.go#L61-L73`), so write templates are not reliably listed. This is not documented.
- **Cluster uninstall namespace.** `argocd cluster rm` uninstalls RBAC from `kube-system` regardless of the `--system-namespace` used at install time (`util/clusterauth/clusterauth.go#L287-L289`). The docs do not mention this.
- **`ARGOCD_GPG_ENABLED=false` is only honoured once per process.** The docs state that setting it disables GnuPG verification (`docs/user-guide/source-integrity-git-gpg.md#L326-L333`). In code, `lookupGit` skips verification only while the process-wide flag `_gpgDisabledLoggedAlready` is unset; after the first skip it returns a verifier (`util/sourceintegrity/source_integrity.go#L76-L84`). The upstream test asserts only the first call (`util/sourceintegrity/source_integrity_test.go#L35-L58`). The repo-server also does not initialize the keyring when GPG is disabled (`cmd/argocd-repo-server/commands/argocd_repo_server.go#L216`), so later verifications would likely fail rather than pass.
- **Helm OCI Secret example namespace.** The example in `docs/operator-manual/declarative-setup.md#L1333-L1346` uses `namespace: oci-helm-chart`. Repository Secrets are read only from the Argo CD namespace (`util/db/secrets.go#L38`), so that example would be ignored.
- **Credential template conditions.** The docs list the "no credentials" condition as none of `sshPrivateKey`, `username` or `password` (`docs/operator-manual/declarative-setup.md#L377`). The code's `HasCredentials()` also counts `bearerToken`, `tlsClientCertData`, `githubAppPrivateKey`, `azureServicePrincipalClientSecret` and `useAzureWorkloadIdentity`, but **not** `gcpServiceAccountKey` (`pkg/apis/application/v1alpha1/repository_types.go#L152-L154`).
- **GPG key `upsert`.** `GnuPGPublicKeyCreateRequest.upsert` exists in the API (`server/gpgkey/gpgkey.proto`), but the server ignores it and always skips existing keys (`server/gpgkey/gpgkey.go#L82`, `util/db/gpgkeys.go#L104-L113`).
- **`RepoCreateRequest.credsOnly`.** The field exists in the proto (`server/repository/repository.proto#L116-L117`), but the server never reads it (`server/repository/repository.go#L436-L492`).
