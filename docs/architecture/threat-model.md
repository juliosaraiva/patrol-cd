# Threat model

Method: STRIDE per trust boundary (see `overview.md`). Each threat lists mitigations and the
tests that verify them. Test IDs refer to `docs/verification/security-tests.md`; a mitigation
without a passing test is tracked as a gap there, never claimed here.

## Assets

A1 destination-cluster credentials · A2 repository credentials (read and write) · A3 SCM
tokens / GitHub App private keys · A4 session signing key and OIDC client secret ·
A5 notification service credentials · A6 workloads in destination clusters (integrity and
availability) · A7 secrets rendered into manifests or present in live state · A8 audit trail.

## Actors

Anonymous internet user · authenticated user with limited project roles · project admin ·
Patrol admin · malicious repository author (can push to a source repo or open a PR) ·
malicious plugin author · compromised destination cluster · compromised SCM webhook sender ·
in-cluster attacker with pod network access.

## Threats and mitigations

| ID | Threat | Mitigations | Verification |
|----|--------|-------------|--------------|
| T1 | Cross-project access: user with role in project A reads/syncs/deletes apps of project B | RBAC object `<project>/<app>` enforced server-side on every route incl. streams, logs, terminal, extensions; project role tokens scoped to their project; per-item filtering of lists/watches; 403 indistinguishable from not-found for apps | SEC-RBAC-* |
| T2 | Tenant escapes project restrictions by editing an Application via kubectl or API to an unpermitted destination/source/resource kind | API validates against project; controller re-validates at reconcile (InvalidSpecError) and before each sync task (project resource allow/deny lists, namespace and cluster checks), apps-in-any-namespace require namespace ∈ `application.namespaces` and project `sourceNamespaces` | SEC-PROJ-* |
| T3 | Privilege escalation through ApplicationSet templating (templated `project`, destination) | appset controller validates generated apps against the appset's project policy; templated project values must match an allowed project for appsets outside the control-plane namespace; SCM providers allow-list (`applicationsetcontroller.allowed.scm.providers`); `policy override` disabled unless configured | SEC-APPSET-* |
| T4 | Malicious repository: path traversal / symlink to read repo-server files or other repos' checkouts | out-of-bounds symlink check before rendering; `SecureJoin` for value files and paths; per-repo checkout dirs; repo-server has no kube token; tmpfs per request | SEC-REPO-* |
| T5 | Malicious repository: arbitrary code execution during rendering (Helm plugins, Kustomize exec plugins/KRM functions, Jsonnet native funcs) | Helm plugins disabled (`HELM_PLUGINS` points to empty dir); Kustomize built without `--enable-alpha-plugins`/`--enable-exec` unless configured via `kustomize.buildOptions`; Jsonnet in child process with no native funcs; sandbox limits (CPU, memory, time, output), sanitized env | SEC-RENDER-* |
| T6 | Oversized input / zip bombs / YAML bombs (billion laughs) | byte caps on fetched archives, extracted size/entry caps, manifest size cap, YAML decoder depth/alias limits, request body caps on all HTTP endpoints | SEC-DOS-* |
| T7 | Malicious CMP plugin exfiltrates credentials | sidecars receive only the source tree + declared env; no repo creds, no kube token (automount disabled); unix socket in pod-local emptyDir; NetworkPolicy optional egress restriction | SEC-CMP-* |
| T8 | SSRF via repository URLs, Helm/OCI URLs, cluster server URLs, proxy URLs, notification webhooks, OIDC issuer, extension backends | URL scheme allow-lists; optional private-network deny for repo/notification egress (`--block-private-egress`); extension backends configured only by admins; redirects not followed to different hosts for credentialed requests | SEC-SSRF-* |
| T9 | Webhook spoofing | HMAC verification over raw bytes (constant-time), per-provider secrets; requests without valid signature rejected when a secret is configured; body size cap | SEC-WH-1..4 |
| T10 | Webhook replay / flood | delivery-ID dedup LRU; webhook effects are idempotent (set refresh annotation); per-source rate limit; polling remains truth | SEC-WH-5..7 |
| T11 | Resource ownership abuse: app A adopts/prunes resources of app B or unmanaged resources | tracking ID includes app namespace+name and resource identity; resources tracked by another app are reported `SharedResourceWarning` and not pruned; `FailOnSharedResource`; never prune untracked resources; prune only resources whose tracking matches exactly | SEC-OWN-* |
| T12 | Destructive sync: unintended mass deletion | `allowEmpty=false` default blocks auto-prune to empty; `Prune=confirm`/`Delete=confirm` options; prune disabled by default for automated sync; cascade delete requires finalizer; destructive warnings in plan and UI confirmation | SEC-DESTR-* |
| T13 | Terminal abuse | disabled by default (`exec.enabled`), requires `exec,create` RBAC on the app's project, only pods belonging to the app's resource tree, allowed shells list, audit log of session start/stop | SEC-TERM-* |
| T14 | Log / diff / manifest disclosure of Secrets | Secret data masked in live/target/diff/manifests API responses; logs require `logs,get`; audit logs never include bodies | SEC-DISC-* |
| T15 | UI injection (XSS via app names, annotations, links, banners, extension content) | React escaping only (no `dangerouslySetInnerHTML` except sanitized banner markdown); deep links restricted to http/https; CSP header (`default-src 'self'`); extensions served from configured bundle only | SEC-UI-* |
| T16 | CSRF | SameSite=Strict cookie, required `X-Requested-With` header for cookie-authenticated mutations, Origin check | SEC-CSRF-* |
| T17 | Session theft / fixation | HttpOnly+Secure cookies, short session lifetime, revocation on logout and password change, OIDC state+nonce+PKCE, redirect allow-list | SEC-SESS-* |
| T18 | Brute force login | per-user and global failure limits with lockout window; constant-time comparisons; uniform timing for unknown users | SEC-AUTH-* |
| T19 | Authorization bypass through internal APIs | internal services require shared token and are not exposed by Service type; NetworkPolicy limits callers | SEC-INT-* |
| T20 | Controller over-privilege | default install uses cluster-admin only on the local cluster for app management (as upstream); `namespace-install` profile uses namespaced Roles; service account impersonation per project (`destinationServiceAccounts`) | SEC-PRIV-* |
| T21 | Supply chain | pinned toolchains, `go.sum`/`pnpm-lock.yaml`, pinned base image digests and tool checksums, SBOM (syft-style CycloneDX via `cyclonedx-gomod`/`@cyclonedx/cyclonedx-npm`), `govulncheck` and `pnpm audit` reports interpreted in `docs/verification/supply-chain.md` | SEC-SUPPLY-* |
| T22 | Secret leakage in logs/errors/events | redaction helpers, credential types implement `slog.LogValuer`, tool stderr scrubbed of credential strings, no request bodies in logs | SEC-LEAK-* |
| T23 | Audit tampering / gaps | audit events emitted for every mutation and denial to stdout (collected by cluster logging) and as Kubernetes Events for app/project targets; actors from verified token claims only | SEC-AUDIT-* |

## Residual risks

- Repo-server rendering of Helm charts executes templates (Sprig/`tpl`) which can be CPU heavy;
  limits bound but do not prevent per-tenant noisy neighbors.
- Cluster-admin credentials for the local cluster are required for the default profile.
- Dex, when deployed, is a separate upstream component with its own threat model.
