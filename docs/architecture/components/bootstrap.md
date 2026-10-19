# Component: `patrolcd bootstrap` (first-run Secrets and default known hosts)

Status: implemented and tested against a real kube-apiserver (envtest 1.36). The Helm chart
and the static manifests run it as an init container of the primary workload; `test/deploy`
checks that wiring, admits the rendered pods (init container included) under Pod Security
`restricted`, and runs the init container's command as the pod's service account with the
rendered Role, all against envtest (see Verification). It has not been run by a kubelet: no
image was built and no kind cluster started (Docker is blocked on this machine, see
`TASKS.md`). Decision records:
[ADR-0006](../adr/0006-internal-apis.md) (internal token),
[ADR-0009](../adr/0009-authn-authz.md) (local accounts, signing key). Parity IDs: OPS-030
(admin and signing-key part), OPS-031, TEN-092; status overlay:
`docs/parity/status/bootstrap.yaml`. Related: [wiring](wiring.md), [auth](auth.md),
[settings](settings.md), [cli-admin](cli-admin.md) (`patrol admin initial-password`).

`patrolcd bootstrap` creates the Secrets a fresh installation needs before any component
serves, and seeds the default SSH known hosts. It is idempotent (a converged namespace is not
written to), safe when several replicas run it at the same time, and never overwrites
existing material, with one exception: an initial admin Secret older than a `patrol-secret`
without `admin.password` belongs to an earlier installation and is replaced
([Stale initial admin Secret](#stale-initial-admin-secret-dv-6)). The install
manifests run it as an init container of the primary workload (patrol-server; the
controller in a core install); an operator can run it by hand with a kubeconfig.

| Path | Contents |
|------|----------|
| `internal/server/bootstrap_known_hosts.go` | `DefaultSSHKnownHosts` (provider host keys) and the known hosts seeding step |
| `internal/server/bootstrap_known_hosts_test.go` | Fingerprint pins of the defaults; envtest seeding, ownership and RBAC tests |
| `internal/server/bootstrap_secrets.go` | `RunBootstrap(ctx, BootstrapConfig)`, the convergence steps, password and token generation, retry classification; constants `InitialAdminSecretName`, `InitialAdminSecretKey`, `InitialAdminPasswordLength`, `InternalTokenBytes`, `DefaultBootstrapTimeout`, `BootstrapFieldManager` |
| `internal/server/bootstrap_cmd.go` | The cobra command: flags, namespace resolution, logging, kubeconfig loading (client without client-side throttling, `newBootstrapClient`), timeout |
| `internal/server/commands.go` | Registers the command in the `patrolcd` tree |
| `internal/server/bootstrap_secrets_test.go` | Unit, fake-clientset and envtest tests (below) |
| `internal/server/bootstrap_stale_test.go` | Stale initial admin Secret (DV-6): envtest, fake-clientset and precondition tests |
| `internal/server/bootstrap_cmd_test.go` | Command client and deadline reporting against a fake API server (`httptest`) |
| `cmd/patrolcd/testdata/help/bootstrap.golden` | Pinned `patrolcd bootstrap --help` |

## Managed objects

All in the control-plane namespace. Nothing else is read or written.

| Object | Keys | Created when | Shape | Never |
|--------|------|--------------|-------|-------|
| Secret `patrol-internal-token` | `token` | the Secret does not exist (unless `--skip-internal-token`) | `Opaque`; labels `app.kubernetes.io/part-of=patrol-cd`, `app.kubernetes.io/name=patrol-internal-token`; 32 bytes from `crypto/rand`, hex encoded (64 characters) | rotated or modified; an existing Secret without a non-blank `token` is an error |
| Secret `patrol-secret` | `server.secretkey` | the key is missing or empty (`settings.EnsureServerSecretKey`, which also creates the Secret) | 32 random bytes; created Secret labelled `app.kubernetes.io/part-of=patrol-cd` | replaced when present |
| Secret `patrol-secret` | `admin.password`, `admin.passwordMtime` | the admin account is enabled and `admin.password` is absent or empty (unless `--skip-admin`) | bcrypt hash at `auth.DefaultBcryptCost` (`auth.HashPassword`); mtime RFC 3339 UTC, second precision (`auth.SecretWithPassword`) | changed when `admin.password` exists (operator-provided or from an earlier run) |
| Secret `patrol-initial-admin-secret` | `password` | an initial password is needed and the Secret does not exist, or exists but was created in an earlier second than `patrol-secret` (stale: deleted, then created again, see [Stale initial admin Secret](#stale-initial-admin-secret-dv-6)) | `Opaque`; label `app.kubernetes.io/name=patrol-initial-admin-secret` only; 24 characters of `[A-Za-z0-9]` from `crypto/rand` with rejection sampling (bytes 248..255 discarded: no modulo bias), about 143 bits | modified in place; rotated while `admin.password` exists; created when `admin.password` already exists |
| ConfigMap `patrol-ssh-known-hosts-cm` | `ssh_known_hosts` | the ConfigMap exists, carries `app.kubernetes.io/part-of=patrol-cd` and has no `ssh_known_hosts` key (unless `--skip-ssh-known-hosts`) | `DefaultSSHKnownHosts`: the published host keys of GitHub (also `[ssh.github.com]:443`), Bitbucket Cloud, GitLab.com and Azure DevOps (14 entries, fingerprints pinned by `TestDefaultSSHKnownHostsFingerprints`); full update guarded by the resourceVersion, field manager `patrolcd-bootstrap` | created (a missing ConfigMap is left missing); changed when the key exists, even empty (managed through the certificate API or by the operator); written when unlabelled (the settings layer ignores it; a warning is logged) |
| ConfigMap `patrol-cm` | `admin.enabled`, `accounts.admin.enabled` | read only | parsed by `auth.AccountsFromMaps`: admin is enabled unless either key is `false` or not a boolean (fail closed); a missing ConfigMap leaves admin enabled | written |

The initial admin Secret deliberately has no `part-of` label: patrol-server's settings
manager caches part-of Secrets (referenced-secret informer) and settings values may
reference their keys (`$<secret>:<key>`), and the plaintext stays out of both. The internal
token carries the label as required by the task contract; patrol-server holds that token
anyway.

Consumers: `patrol admin initial-password` (`internal/cli/admin/password.go`) prints
`patrol-initial-admin-secret` key `password`; every internal client and server reads the
token (`internal/server/token.go`: mounted file `/app/config/internal-token/token`, or the
Secret directly when the default file is missing and the component has Kubernetes access);
servers compare `sha256(token)` in constant time (`internal/reposerver`,
`internal/commitserver`, `internal/controller/treeapi`), so any header-safe string works and
the hex encoding is never decoded.

## Algorithm

Each run executes the steps in order; every step reads before it writes.

1. **Internal token.** Get `patrol-internal-token`. NotFound: generate, Create;
   AlreadyExists (another run won): Get again. Then require a non-blank `token` (warning
   below 32 characters; the value is kept).
2. **Signing key.** `settings.EnsureServerSecretKey` (creates `patrol-secret` when absent,
   adds `server.secretkey` with a resourceVersion-guarded update otherwise; conflicts mean
   another replica wrote first and are re-read).
3. **Known hosts** (unless `--skip-ssh-known-hosts`). Get `patrol-ssh-known-hosts-cm`.
   NotFound, unlabelled, or key `ssh_known_hosts` present: nothing. Otherwise Update with the
   key set to `DefaultSSHKnownHosts`, carrying the resourceVersion read (a concurrent run or
   API write makes it fail with Conflict; the retry re-reads and finds the key).
4. **Admin enabled?** Get `patrol-cm` (NotFound: enabled). Disabled: stop here.
5. **Admin hash.** Get `patrol-secret`. `admin.password` non-empty: stop (a non-bcrypt value
   is kept and reported). Otherwise obtain the initial password (step 6, given the
   `creationTimestamp` of `patrol-secret`), hash it, and Update `patrol-secret` with
   `admin.password` + `admin.passwordMtime` carrying the resourceVersion just read.
6. **Initial password.** Get `patrol-initial-admin-secret`. Exists and not older than
   `patrol-secret`: use its `password` verbatim (an interrupted earlier run, or a concurrent
   run between steps 6 and 5; empty or longer than 72 bytes is an error). Exists and created
   in an earlier second than `patrol-secret` (stale): Delete it with a UID precondition
   (NotFound or Conflict: another run already deleted or replaced it), then continue as for
   NotFound. NotFound: generate, Create; AlreadyExists: Get and use the winner's password.

Write ordering is crash safe: the plaintext is persisted (Create) before its hash, so every
interruption leaves a state the next run completes without generating a second password. The
stale replacement keeps this: a run interrupted after the delete leaves no initial Secret,
and one interrupted after the Create leaves an initial Secret newer than `patrol-secret`,
which the next run reuses.

| State found | Action |
|-------------|--------|
| nothing | token, signing key, initial Secret, hash created; known hosts seeded when the ConfigMap is shipped without them |
| known hosts ConfigMap with `ssh_known_hosts` (any value, also empty) | key untouched |
| initial Secret exists, created in the same or a later second than `patrol-secret`, no `admin.password` (crash between 6 and 5, or a concurrent run) | hash of the stored password written; initial Secret untouched |
| initial Secret exists, created in an earlier second than `patrol-secret`, no `admin.password` (`patrol-secret` was re-created, e.g. `helm uninstall` + install) | initial Secret deleted (UID precondition) and created again with a new password; its hash written |
| `admin.password` exists, no initial Secret (operator set a password, or installed with one) | nothing |
| `admin.password` exists and initial Secret exists (normal converged state, also after a password change) | nothing (the initial Secret is out of date, as upstream; delete it after changing the password) |
| initial Secret exists without `password`, not older than `patrol-secret` | error: delete it so a new one is generated (an older one is replaced as stale) |
| `patrol-internal-token` exists without a token | error: set one or delete the Secret |
| admin disabled in `patrol-cm` | no initial Secret, no hash |

Concurrency: creations race on Create (exactly one run wins; the others adopt the stored
value); the hash write races on the resourceVersion (exactly one Update succeeds; the
others get Conflict, re-read and find the hash); the delete of a stale initial Secret is
guarded by its UID (exactly one run deletes it; the others get NotFound or Conflict and
adopt the replacement through Create's AlreadyExists). `TestBootstrapConcurrent` and
`TestBootstrapStaleInitialSecretConcurrent` (8 simultaneous runs each) assert exactly one
creation of each Secret, one hash write, one seeding of the known hosts and one deletion of
the stale Secret; how many runs adopt or re-read varies per run and is only logged (example
lines: [run record](../../verification/runs/2026-10-18-bootstrap-dv6.md)).

Retries: the whole sequence is re-run (it is idempotent) after Conflict, server timeouts,
429, 500, 503, unexpected server errors, Forbidden/Unauthorized (RBAC objects applied
together with the workload may not be effective yet), dial failures, network timeouts and
EOFs, with jittered backoff from 200 ms doubling to 5 s, until `--timeout` ends. Everything
else fails at once (NotFound namespace, invalid objects, TLS verification failures, the
two "exists without a value" states). When the deadline ends the error is
`bootstrap did not converge before the deadline: <last retriable error>` (followed by
`(last attempt: …)` when the deadline cut a request in flight short), so a missing
permission still reads as Forbidden (`TestBootstrapDeadline`, `TestBootstrapDeadlineCutsRequest`).
This relies on the command's client having no client-side rate limiter
(`newBootstrapClient`, `TestBootstrapClientNotThrottled`): client-go's token bucket refuses
a request whose wait would pass the deadline with a plain error (`client rate limiter Wait
returned an error: rate: Wait(n=1) would exceed context deadline`) while the context is
still live, and `RunBootstrap` returns that error as is instead of the last retriable one. A
library caller that passes a throttled client can get that error. An attempt makes about a dozen
requests and the backoff spaces attempts, so throttling is not needed; the API server's
priority and fairness still applies, and klog's unstructured "Waited before sending request"
lines cannot occur.

## Stale initial admin Secret (DV-6)

`helm uninstall` deletes `patrol-secret` (rendered by the chart, `configs.secret.create`)
with `admin.password` and keeps `patrol-initial-admin-secret` (created at runtime). Before
this check, the bootstrap of a re-install reused the kept initial Secret (crash recovery)
and re-hashed its password, so an admin password changed before the uninstall silently
reverted to the old initial password. Upstream generates a new password whenever
`admin.password` is missing.

Rule (`initialSecretStale`): when `patrol-secret` has no `admin.password` and the initial
Secret's `metadata.creationTimestamp` is strictly earlier than that of `patrol-secret`, the
initial Secret belongs to an earlier `patrol-secret` and is stale. The bootstrap deletes it
with a UID precondition (NotFound and Conflict mean another run already did, and are
ignored), logs a warning naming both Secrets (never a value), generates a new password and
creates the Secret again with the usual Create arbitration. Every other case reuses it:

- Crash recovery: a run creates (or finds) `patrol-secret` in step 2 before it creates the
  initial Secret in step 6, so an initial Secret left by an interrupted run is never older
  than `patrol-secret`.
- Zero timestamps (fake clientsets; a real API server always sets them) and equal ones (both
  created in the same second) count as not stale, which is the behavior before the check.
- A concurrent run that loses Create reads the winner's Secret, created after the stale one
  was deleted and therefore not older than `patrol-secret`.

Assumptions, stated because the check depends on them:

- Both timestamps are set by the API server from its own clock when it handles the create
  (`FillObjectMetaSystemFields` in `k8s.io/apiserver`), never by the bootstrap, and are kept
  at one-second granularity (`metav1.Time` serializes seconds only); hence the strict
  comparison.
- The two cases are far apart in practice. A stale initial Secret predates the new
  `patrol-secret` by the time between the earlier install's first bootstrap run and the
  re-install (for DV-6 to matter, someone also logged in and changed the password in
  between). A fresh initial Secret follows a chart-rendered
  `patrol-secret` by seconds (the release is applied, then the pod is scheduled, its image
  pulled and the init container started).
- In an HA control plane the two timestamps can come from different API servers, whose
  clocks may differ. Skew only matters for objects created within the skew of each other.
  The narrow case is a `patrol-secret` that the bootstrap creates itself
  (`configs.secret.create: false`, or a manual run in an empty namespace): the same run then
  creates `patrol-secret` and the initial Secret milliseconds apart. If those two requests
  reach API servers whose clocks differ by more than that gap across a second boundary, and
  a second run starts at the same time, that second run can judge the fresh initial Secret
  stale and replace it after the first run stored its hash. `patrol admin initial-password`
  would then print a password that does not log in (recover with the reset procedure under
  "Differences from upstream"). A single API server, or synchronized clocks, rules this out.
  A clock-free test (recording the UID of `patrol-secret` on the initial Secret) would need
  an annotation key in `api/v1alpha1/keys.go`; it is not implemented.
- Not detected: a `patrol-secret` that loses `admin.password` by an in-place update (its
  `creationTimestamp` does not change), e.g. a tool that rewrites the Secret's data. The
  initial Secret is then reused, as before.
- An initial Secret that an operator creates before the install (older than the rendered
  `patrol-secret`) is replaced as stale. Set `admin.password` instead (chart:
  `configs.secret.adminPasswordHash`).

Tests: `TestBootstrapStaleInitialSecret` (envtest: install, password change, `patrol-secret`
deleted and rendered again in a later second; a new password with a new Secret UID, the old
initial password and the changed password both rejected by the server's login path, the
real `patrol admin initial-password` prints the new password, a re-run writes nothing, a
crash after the replacement re-hashes the new Secret), `TestBootstrapStaleInitialSecretConcurrent`
(8 runs: one deletion, one creation, one hash write), `TestBootstrapStaleTimestamps` (fake
clientset, explicit timestamps: zero, equal and newer initial Secrets reused; an older one
replaced with a delete carrying its UID), `TestBootstrapStaleDeleteConflict` (Conflict on
the delete tolerated, the replacement adopted), `TestDeleteStaleInitialSecretPreconditions`
(the envtest API server answers a stale UID with Conflict and a deleted Secret with NotFound;
the replacement is not deleted), `TestBootstrapCrashRecovery` and `TestBootstrapRBACContract`.
Each of these checks its captured logs for the passwords (and, where one was stored, the
hashes) it handled.

## Command

```
patrolcd bootstrap [--namespace NS] [--skip-admin] [--skip-internal-token] [--skip-ssh-known-hosts]
                   [--timeout 60s] [--kubeconfig PATH] [--context NAME] [--loglevel info] [--logformat json|text]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-n`, `--namespace` | `$POD_NAMESPACE`, else `/var/run/secrets/kubernetes.io/serviceaccount/namespace` | Control-plane namespace. There is no kubeconfig-context fallback (writing Secrets into `default` by accident is worse than an error); none found: `no control-plane namespace: pass --namespace (or set POD_NAMESPACE)` |
| `--skip-admin` | false | Leave `admin.password` and `patrol-initial-admin-secret` alone (`server.secretkey` is still ensured) |
| `--skip-internal-token` | false | Leave `patrol-internal-token` alone (for installs that provide it, e.g. from an external secret store) |
| `--skip-ssh-known-hosts` | false | Leave `patrol-ssh-known-hosts-cm` alone (known hosts set through the install's values, or the ConfigMap owned by another tool) |
| `--timeout` | `60s` | Bound of the whole run, retries included; must be positive |
| `--kubeconfig`, `--context` | in-cluster, else `$KUBECONFIG` / `~/.kube/config` | Manual use (client-go loading rules, as the component commands) |
| `--loglevel` | `info` | `debug`, `info`, `warn`, `error` |
| `--logformat` | JSON in a pod (`KUBERNETES_SERVICE_HOST` set), text elsewhere | `json` or `text` |

Logging uses the common setup of the other subcommands (`NewLogger` + `installLogger`, attribute
`component=bootstrap`) on stderr. Logs carry object names, keys and actions only: never the
token, password, hash or signing key (asserted by the tests on captured logs, stdout and
stderr). Nothing is written to stdout. Exit status 0 when converged, 1 on any failure
(errors are printed by `patrolcd` as `Error: …`). SIGTERM/SIGINT cancel the run.

## Kubernetes RBAC (contract for the chart and manifests)

The bootstrap needs exactly these namespaced permissions in the control-plane namespace,
for the service account of the pod that runs it. No list, watch or patch; no cluster-scoped
access. `create` cannot be restricted by `resourceNames` in Kubernetes RBAC (the name is not
known at authorization time), so it is granted on Secrets of the namespace. `delete` is
used only to replace a stale `patrol-initial-admin-secret` (see
[Stale initial admin Secret](#stale-initial-admin-secret-dv-6)); without it that case fails
with Forbidden instead of re-hashing the stale password. See Security notes for what this
contract amounts to.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: patrol-bootstrap            # or merged into the patrol-server Role
rules:
  - apiGroups: [""]
    resources: [secrets]
    verbs: [create]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [patrol-secret, patrol-internal-token, patrol-initial-admin-secret]
    verbs: [get]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [patrol-secret]
    verbs: [update]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [patrol-initial-admin-secret]
    verbs: [delete]
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [patrol-cm]
    verbs: [get]
  # Only when the bootstrap seeds the known hosts (no --skip-ssh-known-hosts).
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [patrol-ssh-known-hosts-cm]
    verbs: [get, update]
```

`TestBootstrapRBACContract` binds exactly these rules (`bootstrapPolicyRules`, keep both in
sync) to a service account and runs the bootstrap as that account (impersonation against the
envtest API server, RBAC authorizer): a fresh install (known hosts seeded), a re-run and a
re-install with a stale initial Secret converge; `list` is denied, and so is `delete` on
`patrol-secret` and `patrol-internal-token` (dry run); without the `update` rule on
`patrol-secret` the run fails with Forbidden, and without the `delete` rule a fresh install
and a re-run still converge but the stale re-install fails with Forbidden (the stale Secret
and the missing hash are left as they were). `TestBootstrapKnownHostsRBAC` does the same
without `update` on the known hosts ConfigMap. `patch` is not used (writes are full updates
guarded by resourceVersion), so it is not part of the contract.

The chart renders this Role as `patrol-bootstrap` (`templates/rbac/_rules.tpl`,
`patrol.rules.bootstrap`) and binds it to the primary workload's service account, which the
init container shares. That account already holds more in every profile checked: in
`default`, `namespaced` and `hardened` the `patrol-server` Role grants
`get, list, watch, create, update, delete` on all Secrets of the namespace; in `core` the
controller's ClusterRole is `*` on `*` (commands in the
[run record](../../verification/runs/2026-10-18-bootstrap-dv6.md); finding BS-1 of the
Phase 4 review covers 12 value combinations). The `delete` rule therefore adds no effective
permission to a chart install; the contract matters to operators who bind the bootstrap to
a dedicated service account with their own RBAC.

## Deployment requirements (for the manifests and Helm chart owner)

- Run `patrolcd bootstrap` as an init container of patrol-server (of the controller in a
  core install, which has no patrol-server), same image, same service account, args
  `[bootstrap]` (plus `--skip-admin` / `--skip-internal-token` / `--skip-ssh-known-hosts` when
  those are provided externally), env `POD_NAMESPACE` from `fieldRef: metadata.namespace`. No volumes; it
  writes nothing to disk, so `readOnlyRootFilesystem: true`, `runAsNonRoot`, all
  capabilities dropped, no privilege escalation and seccomp `RuntimeDefault` (Pod Security
  `restricted`) apply unchanged. The pod needs its service account token (patrol-server and
  the controller mount it anyway).
- Resources: measured with `/usr/bin/time -l` against envtest on macOS arm64
  ([run record](../../verification/runs/2026-10-18-bootstrap-dv6.md); Linux numbers will
  differ): a fresh install took 0.27 s wall, 0.08 s user and 0.06 s system CPU (one bcrypt
  hash at cost 10), a converged re-run 0.03 s, a stale re-install 0.11 s; maximum resident
  set size about 53 to 54 MB, peak memory footprint about 16 MB. Suggested: requests
  `cpu: 10m`, `memory: 32Mi`; limits `memory: 128Mi`.
- **The `patrol-internal-token` volume of the pod that runs the bootstrap must be
  `optional: true`.** Volumes are set up before init containers start, so a required
  volume of a Secret the init container creates would keep the pod from ever starting.
  patrol-server and the controller cope with an empty mount: when the default token file is
  missing they read the Secret through the API (`internal/server/token.go`; do not pass
  `--internal-token-file` explicitly on these, which disables that fallback). Other
  components (repo-server, commit-server, appset and notifications controllers) can mount it
  as required: the kubelet retries the mount until the Secret exists.
- With several patrol-server replicas every pod runs the init container; that is safe
  (see Concurrency). A Helm hook Job is not needed.
- Ship `patrol-cm` (labelled `app.kubernetes.io/part-of: patrol-cd`; empty data is fine):
  patrol-server disables every account while `patrol-cm` is absent (settings), even though
  the bootstrap treats a missing `patrol-cm` as "admin enabled".
- Ship `patrol-ssh-known-hosts-cm` labelled and **without** `ssh_known_hosts` (Helm and
  `kubectl apply --server-side` would otherwise own the key, and the certificate API's updates
  would make the next `helm upgrade` conflict, or be reverted by a re-apply with
  `--force-conflicts`); the bootstrap seeds the defaults, and the API manages the key from then
  on. When the install sets the key itself (values-managed known hosts) or does not ship the
  ConfigMap, pass `--skip-ssh-known-hosts` and drop the ConfigMap rule above. The repo-server
  and commit-server mount the ConfigMap as a directory (no `subPath`), so the kubelet delivers
  the seeded key to running pods.
- If the manifests ship `patrol-secret`, ship it without `server.secretkey` and `admin.*`
  data (upstream ships an empty `argocd-secret`): the bootstrap adds those keys, and
  templating them would let an upgrade overwrite them. Prefer not templating the Secret's
  data at all, or let the bootstrap create it.
- Uninstall: `patrol-internal-token` and `patrol-initial-admin-secret` are created at runtime
  and are not removed by `helm uninstall` / `kubectl delete -f`; their manual removal is
  documented in the installation guide (both carry the `app.kubernetes.io/name` labels
  above). `patrol-secret` is rendered by the chart and goes with the release, taking
  `server.secretkey` and `admin.password` with it. A re-install therefore starts with a new
  signing key (sessions and API tokens of the old release are invalid) and a new initial
  admin password: the kept initial Secret predates the re-rendered `patrol-secret` and is
  replaced (finding DV-6, see [Stale initial admin Secret](#stale-initial-admin-secret-dv-6)).
  This needs the `delete` rule above in the bootstrap Role.

## Differences from upstream

| Upstream (Argo CD v3.5.5) | Patrol CD | Reason |
|---------------------------|-----------|--------|
| argocd-server generates the initial admin password inside the API server at startup | a separate, idempotent `patrolcd bootstrap` step (init container); patrol-server itself only ensures `server.secretkey` | task contract; one place, testable in isolation, runnable by hand |
| 16 characters of `[0-9A-Za-z-]` | 24 characters of `[A-Za-z0-9]` | more entropy; no `-` (shell and flag ambiguities when copied) |
| `argocd-initial-admin-secret` written with create-or-update; a missing hash generates a new password | never updated in place; a missing hash reuses an existing initial Secret unless it was created in an earlier second than `patrol-secret`, in which case it is deleted and created again with a new password | crash recovery without a second password, while a re-created `patrol-secret` (re-install) gets a new password as upstream; to reset the admin password, delete `admin.password`, `admin.passwordMtime` and the initial Secret, then restart the pod (or run the command) |
| a hash without mtime gets `admin.passwordMtime` initialized | an existing `admin.password` is left as is, mtime included | task contract (operator material is never touched); a zero mtime only means sessions are not invalidated by a password-change time |
| no internal token (Redis password bootstrap instead, OPS-013) | `patrol-internal-token` (ADR-0006); no Redis (ADR-0004) | Patrol architecture |
| `argocd-ssh-known-hosts-cm` ships the provider host keys as manifest data | the install ships the ConfigMap without data and the bootstrap seeds the same keys once | the manifest would own the key under server-side apply (Helm 4, `kubectl apply --server-side`), conflicting with the certificate API's updates on every upgrade |

## Security notes

- Entropy: `crypto/rand` only; the password generator rejects biased bytes; the token is 256
  bits.
- No secret value reaches logs, stdout, stderr or error messages (errors name objects and
  keys; Kubernetes API errors do not echo Secret data).
- The plaintext initial password exists only in `patrol-initial-admin-secret` (and in the
  memory of the run that generated or read it). Operators should delete it after the first
  login and password change (`patrol account update-password`).
- The RBAC contract is namespaced, and every rule except `create` on Secrets names its
  objects; that does not make it narrow. With `create` on any Secret and `get` and `delete`
  on `patrol-initial-admin-secret`, a holder can replace that Secret with one of type
  `kubernetes.io/service-account-token` annotated `kubernetes.io/service-account.name` with
  any service account of the namespace. Kubernetes' token controller (kube-controller-manager)
  fills in a token for that account, and the named `get` reads it. `patrol-internal-token`
  allows the same while that name is free (no `delete` is granted on it). Before the `delete`
  rule the same was possible once the initial Secret had been deleted after the first login,
  as the installation guide instructs. Treat the contract as equivalent to acting as any
  service account of the control-plane namespace (in the default chart profile that includes
  `patrol-controller`, bound to cluster-admin), and do not grant it to an identity less
  trusted than that. In a chart install it adds nothing: the bootstrap shares the primary
  workload's service account, which already has unrestricted Secret access in the namespace
  or cluster-admin (see Kubernetes RBAC). To close the window after the first run, set
  `bootstrap.enabled: false` (the chart then drops the init container and the Role), or
  reject Secrets of type `kubernetes.io/service-account-token` with these names with an
  admission policy. Not executed: envtest runs no kube-controller-manager, so this rests on
  the token controller's documented handling of manually created token Secrets.

## Verification

Run on 2026-10-18 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0, golangci-lint
2.15.0), from the repository root; exact commands and results in
[`2026-10-18-bootstrap-dv6.md`](../../verification/runs/2026-10-18-bootstrap-dv6.md):

```bash
KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=1 ./internal/server/...                                # ok
go vet ./internal/server/...                                                  # exit 0
golangci-lint run ./internal/server/...                                       # 0 issues
gofmt -l internal/server                                                      # no output
```

Tests (`internal/server/bootstrap_secrets_test.go` unless noted):

- `TestRandomStringRejectionSampling`, `TestRandomStringMapping`,
  `TestGenerateInitialAdminPassword`: byte-to-character mapping, rejection of 248..255,
  24-character `[A-Za-z0-9]` output, every character reachable, entropy failures surface.
- `TestNewInternalToken`, `TestRetriableBootstrapError`, `TestBootstrapNamespace`,
  `TestRunBootstrapValidatesConfig`.
- `TestBootstrapRetries`, `TestBootstrapDeadline` (fake clientset): an injected Forbidden read
  and a conflicting hash update are retried and the run converges; a persistent 503 ends at
  the deadline with the last error.
- `internal/server/bootstrap_cmd_test.go` (fake API server, `httptest`):
  `TestBootstrapClientNotThrottled` (the command's client, configured with QPS 0.1 and burst
  1, still ends a run that keeps getting 403 with the Forbidden error at the deadline; with
  client-go's token bucket it ended early with the rate limiter's error, finding BS-3);
  `TestBootstrapDeadlineCutsRequest` (a request held past the deadline: the error names the
  last Forbidden and the interrupted attempt).
- envtest: `TestBootstrapFreshInstall` (shapes of all objects, bcrypt cost, mtime, login
  through `settings.Parse` + `auth.Authenticator` as patrol-server does, the real
  `patrol admin initial-password` command prints the same password, second run leaves every
  resourceVersion unchanged, logs free of password/token/hash/signing key);
  `TestBootstrapConcurrent` (8 simultaneous runs, fresh and with an empty shipped
  `patrol-secret` + `patrol-cm`: exactly one creation of each Secret and one hash write);
  `TestBootstrapCrashRecovery` (initial Secret without hash, also strictly newer than a
  `patrol-secret` holding only the signing key: reused; initial Secret without
  `patrol-secret`: stale, replaced; initial Secret without a value; token Secret without a
  token); `TestBootstrapAdminDisabled` (`admin.enabled=false`, `accounts.admin.enabled=false`,
  malformed value); `TestBootstrapKeepsOperatorMaterial` (operator admin password, signing
  key and token untouched, no initial Secret; non-bcrypt value kept with a warning);
  `TestBootstrapSkipFlags`; `TestBootstrapRBACContract` (see Kubernetes RBAC);
  `TestBootstrapCommand` (through `server.Main`: JSON and text logs on stderr only, empty
  stdout, re-run without writes, exit 1 for a missing namespace without waiting for the
  timeout and for invalid flags); `TestBootstrapCommandRegistered`.
- `internal/server/bootstrap_stale_test.go`: the DV-6 tests listed under
  [Stale initial admin Secret](#stale-initial-admin-secret-dv-6).
- `internal/server/bootstrap_known_hosts_test.go`: `TestDefaultSSHKnownHostsFingerprints`
  (every default entry parsed with `x/crypto/ssh` and pinned to its provider's published
  SHA256 fingerprint); envtest `TestBootstrapSeedsKnownHosts` (seeded once into a shipped
  ConfigMap, owner `patrolcd-bootstrap`/Update, re-run writes nothing, a later server-side
  apply of the data-less ConfigMap by another manager neither conflicts nor removes the key;
  a present key, also empty, an unlabelled ConfigMap, a missing ConfigMap and
  `SkipSSHKnownHosts` are left alone); `TestBootstrapKnownHostsRBAC`; `TestBootstrapConcurrent`
  also checks exactly one seeding among 8 runs.
- `cmd/patrolcd/main_test.go`: `TestHelpGolden` (`root` and `bootstrap` goldens),
  `TestBootstrapRegistered` (built binary); `go test -count=1 ./cmd/patrolcd/...` ok after
  the DV-6 change (flags and help text unchanged).

Deployment, checked by `test/deploy` against envtest
([run record](../../verification/runs/2026-10-17-deploy-validation.md), executed before the
DV-6 change; `test/deploy` was not run for it):

- The chart (`deploy/helm/patrol-cd/templates/_bootstrap.tpl`,
  `templates/rbac/bootstrap.yaml`) and the static manifests under `deploy/manifests` render
  the init container and the `patrol-bootstrap` Role and RoleBinding.
- `TestWiring` parses the init container's arguments with the real command tree and checks
  `POD_NAMESPACE`, the mounted service account token, and that the pod's token volume is
  optional and its main container does not pass `--internal-token-file`.
- `TestPodSecurityRestricted` creates (server-side dry run, strict field validation) a pod
  from every rendered pod template, init containers included, in a namespace enforcing,
  auditing and warning `restricted:latest`, and allows no warning (52 pods, 66 containers in
  the recorded run). This is admission only.
- `TestRBACSufficiency` runs the init container's command (the patrolcd binary built on the
  host, not the image) as the pod's service account (TokenRequest token) with exactly the
  rendered RBAC, before the pod's main containers, and fails on any denied request in the
  audit log (8 profiles). These are fresh installs: the `delete` of a stale initial Secret is
  not exercised there.
- `TestHelmLifecycle` runs `patrolcd bootstrap` with the admin kubeconfig after the install
  and after uninstall + re-install.

Not verified: a run by a kubelet (no image was built and no kind cluster started: Docker
disk full), hence the kubelet's late population of the optional token volume and the
runtime effect of `readOnlyRootFilesystem` and non-root settings; the stale-Secret
replacement with the chart's Role during a real re-install; the HA clock-skew case (reasoned
above, not tested).
