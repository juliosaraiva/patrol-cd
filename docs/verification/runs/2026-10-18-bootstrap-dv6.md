# Bootstrap: stale initial admin Secret (DV-6) and review findings BS-1, BS-3, BS-4, BS-5, F6, F11 (2026-10-18)

Component: `patrolcd bootstrap` ([bootstrap.md](../../architecture/components/bootstrap.md)).
Findings: DV-6 in [`../phase4-findings.md`](../phase4-findings.md); BS-1, BS-3, BS-4, BS-5,
F6 and F11 from the Phase 4 review.

Environment: macOS arm64, Go 1.27.2, envtest kube-apiserver/etcd 1.36.0
(`bin/envtest/k8s/1.36.0-darwin-arm64`, path printed by `make --no-print-directory -s envtest`),
golangci-lint 2.15.0, Helm v4.3.1, yq (Homebrew). No Docker, kind or kubelet. All commands
from the repository root; `$S` is a directory in the session scratchpad (not committed).
`test/deploy` was **not** run for this change.

## DV-6: a re-install re-hashed the stale initial password

Change: `internal/server/bootstrap_secrets.go`. When `patrol-secret` has no `admin.password`
and `patrol-initial-admin-secret` was created in an earlier second than `patrol-secret`
(`initialSecretStale`), the bootstrap deletes the initial Secret with a UID precondition
(`deleteStaleInitialSecret`, NotFound and Conflict ignored, warning logged with object names
only) and generates a new password through the usual Create arbitration. Zero or equal
timestamps reuse the Secret. RBAC contract (`bootstrapPolicyRules`, bootstrap.md): new rule
`delete` on Secret `patrol-initial-admin-secret` only.

Before the fix (tests of `internal/server/bootstrap_stale_test.go` written first, run against
the unchanged `bootstrap_secrets.go`):

| Command | Exit | Result |
|---------|------|--------|
| `KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 go test -count=1 -run 'TestBootstrapStale' ./internal/server/` | 1 | `FAIL TestBootstrapStaleInitialSecret`: the initial Secret kept its UID and its password after `patrol-secret` was deleted and created again in a later second. `FAIL TestBootstrapStaleInitialSecretConcurrent`: 0 deletions and 0 generated passwords among 8 runs (expected 1 and 1), same UID, stale password still in use. `FAIL TestBootstrapStaleTimestamps/patrol-secret_newer_(re-install)`: stored password reused, no delete action. The other five timestamp cases passed. |

That first version of the tests used testify `NotEqual` on passwords, so the failure output
printed one generated, throwaway test password (envtest namespace, deleted with the test). The
assertions were then changed to boolean comparisons that never print values.

After the fix: every test below passes (see "Final validation").

New and changed tests: `TestBootstrapStaleInitialSecret`,
`TestBootstrapStaleInitialSecretConcurrent`, `TestBootstrapStaleTimestamps`,
`TestBootstrapStaleDeleteConflict`, `TestDeleteStaleInitialSecretPreconditions`
(`bootstrap_stale_test.go`); `TestBootstrapCrashRecovery` (setups now create `patrol-secret`
before the initial Secret, as a run does; new case "initial secret without patrol-secret is
stale"), `TestBootstrapRBACContract` (stale re-install as the service account, dry-run
`delete` of `patrol-secret` and `patrol-internal-token` Forbidden, new case "without delete
on the initial admin Secret": Forbidden, stale Secret and missing hash left as they were)
(`bootstrap_secrets_test.go`).

`TestDeleteStaleInitialSecretPreconditions` records what the envtest API server answers: a
delete with the UID of a Secret that has since been replaced fails with Conflict
(`apierrors.IsConflict`), and the replacement is not deleted.

## BS-3: the deadline branch did not cover client-side throttling

Change: `internal/server/bootstrap_cmd.go` builds the client with `newBootstrapClient`, which
sets `rest.Config.RateLimiter` to `flowcontrol.NewFakeAlwaysRateLimiter()`; the comment of
the deadline branch in `RunBootstrap` and bootstrap.md ("Retries") now describe what the
branch covers (an attempt cut short by the context) and what it does not (a throttled
client's early refusal).

| Step | Command | Exit | Result |
|------|---------|------|--------|
| Before (`newBootstrapClient` returning `kubernetes.NewForConfig(rc)`, identical to the previous inline code) | `go test -count=1 -run 'TestBootstrapClientNotThrottled\|TestBootstrapDeadlineCutsRequest' -v ./internal/server/` | 1 | `TestBootstrapClientNotThrottled` FAIL: `Error "get Secret patrol/patrol-internal-token: client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline" does not contain "bootstrap did not converge before the deadline"`; `TestBootstrapDeadlineCutsRequest` PASS (2.00s) |
| After (client without rate limiter) | same | 0 | both PASS (3.00s, 2.00s) |

The fake API server (`httptest`) answers 403 to every request; the client is configured with
QPS 0.1 and burst 1, so client-go's token bucket refuses the second request immediately.

## BS-1: the bootstrap's service account already holds Secret delete rights

```bash
for p in default core namespaced hardened; do
  helm template patrol deploy/helm/patrol-cd -n patrol --kube-version 1.36.0 \
    -f deploy/helm/patrol-cd/ci/$p.yaml > $S/$p.yaml          # exit 0 for each profile
done
yq 'select(.kind=="RoleBinding" and .metadata.name=="patrol-bootstrap") | .subjects[].name' $S/<p>.yaml
yq 'select(.kind=="Role" and .metadata.name=="patrol-server") | .rules[] | select(.resources[] == "secrets")' -o=json -I=0 $S/<p>.yaml
yq 'select(.kind=="ClusterRole" and .metadata.name=="patrol-patrol-controller") | .rules' -o=json -I=0 $S/core.yaml
```

| Profile | `patrol-bootstrap` bound to | That account's other Secret rights in the namespace |
|---------|-----------------------------|------------------------------------------------------|
| default | patrol-server | Role `patrol-server`: `{"apiGroups":[""],"resources":["secrets"],"verbs":["get","list","watch","create","update","delete"]}` |
| namespaced | patrol-server | same |
| hardened | patrol-server | same |
| core | patrol-controller | ClusterRole `patrol-patrol-controller`: `[{"apiGroups":["*"],"resources":["*"],"verbs":["*"]},{"nonResourceURLs":["*"],"verbs":["*"]}]` |

The rendered `patrol-bootstrap` Role (chart working tree at the time of the run) already
contained the new rule `{"apiGroups":[""],"resources":["secrets"],"resourceNames":["patrol-initial-admin-secret"],"verbs":["delete"]}`
at the same position as in `bootstrapPolicyRules`, in all four profiles. (The chart is owned
by another task; this run only rendered it.)

## F11: measurements and concurrency counts

Resource use of one `patrolcd bootstrap` process against envtest, measured with
`/usr/bin/time -l` by a probe test passed through `go test -overlay` (no repository file
written; probe and overlay in the session scratchpad):

```bash
go build -o $S/patrolcd ./cmd/patrolcd
PROBE_BIN=$S/patrolcd KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 \
  go test -overlay $S/probe/overlay.json -count=1 -run TestZZProbeMeasure -v ./internal/server/
```

The probe creates a namespace with `patrol-cm` and the labelled known hosts ConfigMap, runs
the binary three times (fresh install; converged re-run; after deleting `patrol-secret` and
creating it again in a later second, i.e. a stale initial Secret) and prints only the
`time(1)` lines. Second invocation (the first one, on a freshly built binary, took 1.25 s wall
for the fresh install with the same 0.08 s user CPU; macOS first-launch overhead not
separated):

| Run | real | user | sys | maximum resident set size | peak memory footprint |
|-----|------|------|-----|---------------------------|-----------------------|
| fresh install | 0.27 s | 0.08 s | 0.06 s | 54214656 B | 16057136 B |
| converged re-run | 0.03 s | 0.01 s | 0.01 s | 53231616 B | 16139008 B |
| re-install (stale initial Secret) | 0.11 s | 0.08 s | 0.02 s | 53067776 B | 15794968 B |

Both probe runs: `--- PASS: TestZZProbeMeasure`, `ok`.

Concurrency counts are logged by the tests, not asserted (they vary per run). From the final
validation run below:

```
bootstrap_secrets_test.go:461: runs that adopted another run's Secret: internal token 7, initial password 7; conflicts re-read: 8
bootstrap_secrets_test.go:461: runs that adopted another run's Secret: internal token 7, initial password 7; conflicts re-read: 7
bootstrap_stale_test.go:168: stale initial Secret: deleted 1, already replaced 7, generated 1, adopted 7
```

Earlier runs of the same tests the same day logged `internal token 6, initial password 7;
conflicts re-read: 7` and `stale initial Secret: deleted 1, already replaced 5, generated 1,
adopted 7`.

## Documentation claims (BS-4, BS-5, F6, F11)

| Claim removed or changed in bootstrap.md / bootstrap.yaml | Check |
|-----------------------------------------------------------|-------|
| "install manifests are Phase 4 work", "manifests not written yet", "must run the init container" | `grep -n "Phase 4 work\|manifests not written\|80 ms\|53 MB\|7 adoptions\|name-scoped" docs/architecture/components/bootstrap.md` → no match (exit 1); the init container is rendered by `deploy/helm/patrol-cd/templates/_bootstrap.tpl`, Pod Security and RBAC checks are the `test/deploy` tests named in bootstrap.md "Verification" (run record `2026-10-17-deploy-validation.md`) |
| "RBAC is namespaced and name-scoped except create" | replaced by the service-account-token reasoning in "Security notes" (BS-5); not executed: envtest has no kube-controller-manager (token controller) |
| HA clock skew of creationTimestamp | `grep -n -A3 'func FillObjectMetaSystemFields' ~/go/pkg/mod/k8s.io/apiserver@v0.36.1/pkg/registry/rest/meta.go` → `meta.SetCreationTimestamp(metav1Now())` (the handling server's clock); `metav1.Time.ProtoTime` stores seconds only (`k8s.io/apimachinery` `time_proto.go`, Nanos commented out) |

Parity overlay: `docs/parity/status/bootstrap.yaml` validated on a scratch copy of `docs/`
(`go run ./hack/parity -root <copy>` exit 0, then `-check` exit 0). The repository's
`docs/parity/matrix.md` was not regenerated here (`go run ./hack/parity -check` in the
repository: exit 1, stale).

## Final validation

| Command | Exit | Result |
|---------|------|--------|
| `KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 go test -race -count=1 -v ./internal/server/...` | 0 | `ok github.com/juliosaraiva/patrol-cd/internal/server 40.166s`; 131 PASS lines (tests and subtests), 0 FAIL, 0 SKIP |
| `go vet ./internal/server/...` | 0 | |
| `golangci-lint run ./internal/server/...` | 0 | `0 issues.` |
| `gofmt -l internal/server` | 0 | no output |
| `KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" go test -count=1 ./cmd/patrolcd/...` | 0 | `ok github.com/juliosaraiva/patrol-cd/cmd/patrolcd 13.552s` (help goldens unchanged) |

## NOT EXECUTED

- `go test ./test/deploy/...` (run by the orchestrator after all Phase 4 fixes).
  `TestHelmLifecycle` step 8 asserts that a re-install keeps the initial password; with this
  change it gets a new one and needs updating.
- A kubelet-run init container, the image, a real `helm uninstall` + install with the chart's
  Role (Docker disk full, no kind).
- The HA clock-skew case and the service-account-token minting described in BS-5 (reasoned,
  not reproduced).
