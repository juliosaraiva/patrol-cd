# Component: `patrol app` and `patrol appset` (`internal/cli/app`)

Status: implemented (every `argocd app` and `argocd appset` subcommand of the v3.5.5 baseline);
not yet registered in `cmd/patrol` (see "Requests to other components"). Framework:
`internal/cli/cmdutil` (factory, global flags, exit codes, prompts, editor) and
`internal/cli/output` (tables, sanitizing). API: `pkg/apiclient` (applications, resource-level
application routes, ApplicationSets). Upstream baseline: Argo CD v3.5.5
`cmd/argocd/commands/app.go`, `app_actions.go`, `app_resources.go`, `applicationset.go`,
`cmd/util/app.go`, `cmd/util/applicationset.go` (behavior re-implemented, no code copied).

Wiring: `app.Register(root, f)` adds both groups (with the kubectl client flags, as upstream
`initialize.InitCommand`, but persistent so that every subcommand accepts them); it is a
`cli.Registrar`.

| File | Contents |
|------|----------|
| `factory.go` | `Register`, the per-command `env` (client, prompts, editor, clock, poll/retry intervals, URL fetcher, local renderer) built from `*cmdutil.Factory` |
| `client.go`, `apiclient.go` | `Client`: the narrow interface over `*apiclient.Client` the commands use (streams narrowed to `Next`/`Close`); `NewAPIClient` |
| `errors.go` | exit codes (1 usage/`log.Fatal`, 20 generic, `diff` 2/1), helpers over `cmdutil` |
| `appopts.go`, `specmut.go` | the shared application flags (`create`, `set`, `add-source`) and their mapping to the spec in pflag's lexical `Visit` order (upstream `SetAppSpecOptions`), helpers re-implementing the v1alpha1 source methods |
| `app.go` | command group, name/namespace/source-position resolution, refresh flags, listing with pagination |
| `create.go`, `list.go`, `get.go`, `set.go`, `unset.go`, `edit.go`, `delete.go`, `sources.go`, `ops.go` | lifecycle commands (`edit`/`patch` in `edit.go`; `history`, `rollback`, `terminate-op`, `confirm-deletion` in `ops.go`) |
| `sync.go`, `selector.go`, `wait.go`, `watch.go` | sync (selectors, local manifests, preview), wait (conditions, operations, progress table), watch stream with re-open and polling fallback |
| `diff.go`, `difftext.go` | diff targets (live, revision(s), local, server-side generate), client-side and server-side diff, output and external diff programs |
| `localtar.go`, `localrender.go` | archive of a local directory (ignore rules, symlink containment, limits) and client-side rendering with `internal/render` |
| `manifests.go`, `resources.go`, `actions.go`, `logs.go` | manifests, resource tree/list, get/patch/delete-resource, actions, pod logs |
| `appset.go` | `appset get|list|create|generate|delete` |
| `print.go`, `tree.go` | summaries, tables, resource tree rendering |

## Behavior

- **Names.** Every command accepts `ns/name` (the qualified name wins over `-N`). Missing and
  forbidden Applications/ApplicationSets both answer 403 (`permission denied`, no existence
  oracle, upstream behavior); commands that must tell "absent" from "present" (create, delete
  waits, appset create) treat both as absent.
- **Exit codes.** Usage errors and upstream `log.Fatal` paths exit 1, API and other errors 20
  (`errors.CheckError`); `app diff` exits 2 on errors, `--diff-exit-code` (default 1) when a
  difference is found, 0 otherwise (`--exit-code=false` always 0). Messages are the upstream
  ones (e.g. `existing application spec is different, use upsert flag to force update` comes
  from the server).
- **Spec flags.** All upstream source, destination, sync-policy, Helm, Kustomize, Jsonnet,
  plugin and hydrator flags; changed flags are applied in lexical order exactly once, on the
  source at `--source-position`/`--source-name` (1-based); hydrator flags edit
  `spec.sourceHydrator`. `--validate` (default true) is passed to the server; `unset` and
  `confirm-deletion` update without validation as upstream.
- **Source hydrator Applications.** An Application with `spec.sourceHydrator` (existing, from
  `create -f FILE`, or created by hydrator flags) has no plain source. In `create`, `set` and
  `unset`, tool options (Helm, Kustomize, directory/Jsonnet, plugin) and `-p` are stored in
  `spec.sourceHydrator.drySource`. The location flags `--repo`, `--path`, `--revision`,
  `--helm-chart`, `--tag-prefix`, `--ref` and `--source-name` fail with exit 20 and point to
  `--dry-source-*`/`--sync-source-*`, and so do tool options and `-p` with
  `--source-position > 0`. No change is sent then. Upstream edits a detached copy and reports
  success without changing anything. `-p` on a dry source without a tool block counts as Helm
  from the `name=value` syntax. `status.sourceType` is not used, because it describes the
  hydrated sync source.
- **Operations (ADR-0011).** `sync` and `rollback` receive the created Operation and wait for
  that Operation by name; `wait` inspects the Application's Operations. An Operation counts as
  in progress while it is not terminal and, for non-dry-run syncs, until the Application was
  reconciled after it finished (upstream waits for `reconciledAt >= finishedAt`). The final
  summary prints the observed Operation (state and per-resource results from
  `Operation.status.syncResult`) because the Application status may lag behind.
  `terminate-op` sets `spec.cancel` through the API.
- **Resource selectors.** `--resource GROUP:KIND:[NAMESPACE/]NAME` (repeatable, `*` wildcards,
  `!` exclusions; exclusions win, only exclusions select the rest) and `--label` are expanded
  on the client against `status.resources` (labels through the target manifests), because the
  Operation API takes explicit resources only.
- **Watching.** `wait`, `sync`, `rollback`, `delete --wait` and `appset create|delete --wait`
  follow the watch stream; a stream that fails is re-opened (3 attempts), unsupported or
  failing streams fall back to polling. Deletion waits read the object once after each stream
  opened, so a deletion that happened before the stream started is not missed.
- **Diff.** Client-side diff uses `internal/diff` with the Application's
  `ignoreDifferences`, the system rules of `resource.customizations` and the tracking settings;
  server-side diff (`--server-side-diff` or the `ServerSideDiff=true` compare option) batches
  by size with bounded concurrency. Targets from a revision, several revisions, `--local`
  or `--server-side-generate` exclude Secrets (the server masks Secret data). Without an
  external program the output is the default ("normal") format of `diff(1)` computed
  in-process, as upstream prints it through `diff`; `KUBECTL_EXTERNAL_DIFF` (or
  `PATROL_DIFF_PROGRAM`, which wins) runs a program on two private files (0600, private
  temporary directory), split like a command line without a shell.
- **Local sources.** `--local DIR` renders on the client with `internal/render` (Directory,
  Kustomize, Helm path, Jsonnet) confined to `--local-repo-root`; `--server-side-generate`
  uploads a gzipped tar of DIR (`--local-include` patterns on file names, default
  `*.yaml,*.yml,*.json`; `.gitignore`/`.patrolignore` honored per directory with negation and
  anchoring; `.git/.hg/.svn` skipped; symlinks kept only when they stay inside DIR; owner
  names and IDs stripped; at most 32 MiB compressed, 256 MiB uncompressed, 20000 files) with
  its SHA-256 checksum and lets the server render. Local sync manifests are bounded to 1024
  objects and 1 MiB; a server-rendered local sync refuses core Secrets (their data comes back
  masked).
- **Untrusted input.** Files and URLs (`-f`, `--apply-spec-patch`, `appset create`) are read
  with a 16 MiB bound (URLs: http(s) only, 60 s timeout, 2xx only, credentials redacted in
  messages) and decoded strictly (unknown fields rejected, at most 10000 documents). Text from
  the server (names, messages, log lines) goes through `output.Sanitize`/`SanitizeMultiline`
  before reaching the terminal. This includes rendered repository content that Kubernetes has
  not validated yet: the `diff` / `sync --preview-changes` resource headers and the
  `RepeatedResourceWarning`/Secret warnings are sanitized, and the built-in diff body passes
  through `SanitizeMultiline` (YAML escapes control characters, and this step also removes
  bidirectional overrides). An external diff program gets the files unchanged. No token or
  Secret value is printed or logged.

## Differences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| `Application.operation` / `status.operationState` drive sync and wait | Operation objects followed by name; in-progress also until reconciled | ADR-0011 |
| `--resource` exclusions sent to the server | expanded on the client against `status.resources` | the Operation API takes explicit resources |
| Default diff output by executing `diff` | `diff(1)` normal format computed in-process; `PATROL_DIFF_PROGRAM` added | no dependency on a `diff` binary |
| `GetManifestsWithFiles` gRPC stream | gzipped tar upload with checksum (`--server-side-generate`) | no gRPC (api-contract) |
| `actions run` falls back to `RunResourceAction` V1 | single action route | one server version |
| — | `create --apply-spec-patch FILE|URL` (JSON merge patch on the spec of every constructed Application) | Patrol addition |
| — | `sync --server-side-generate`, `sync --local-include`, `manifests --server-side-generate/--local-include` | Patrol addition (server rendering of local files outside `diff`) |
| — | `rollback --dry-run` | Patrol addition (the rollback route accepts it) |
| — | `actions run --param name=value` | Patrol addition (parameterized actions such as `scale`) |
| — | `appset delete --cascade`, `--propagation-policy` | Patrol addition (the delete route accepts them) |
| Source flags and `-p` on a source hydrator Application edit a detached copy (silently lost); with hydrator flags, plain source flags are ignored | tool options and `-p` go to `drySource`; location flags and `--source-position > 0` fail (exit 20) | no false success; the CRD forbids `source` next to `sourceHydrator` |
| Resource headers in `diff` output printed raw | sanitized (`output.Sanitize`) | rendered names are untrusted; `--preview-changes` asks for confirmation after the output |
| `confirm-deletion` annotation `argocd.argoproj.io/deletion-approved` | `cd.patrol.example/deletion-approved` | API group (ADR-0002) |

## Requests to other components

- **cli-core (`cmd/patrol/main.go`)**: register the group: `cli.Main(os.Args[1:], app.Register)`
  (with the other registrars). Until then the binary has no `app`/`appset` commands; the tests
  register it through `clitest.Options.Registrars`.
- **cli-core (`cmd/patrol/main.go`)**: call `sandbox.MaybeHelperMain()` and
  `jsonnet.MaybeJsonnetMain()` first in `main`, so client-side `--local` rendering of Jsonnet
  can re-execute the binary as the evaluator child. Without it a Jsonnet directory fails with
  `jsonnet.MaybeJsonnetMain was not called in this binary` (other source types are
  unaffected); `--server-side-generate` works either way.

## Gaps

- Client-side `--local` rendering refuses Config Management Plugin sources and Helm chart
  repository sources (`use --server-side-generate`).
- `app logs` is exercised by unit tests with a fake stream only: envtest has no kubelet to
  serve pod logs.
- The end-to-end tests run a controller stand-in (it compares target manifests with envtest
  objects and applies them on sync); the real controller and sync engine are other tasks'.

## Parity IDs

Recorded in [`docs/parity/status/cli-app.yaml`](../../parity/status/cli-app.yaml).

## Verification

Tests (`internal/cli/app`):

- Unit tests: flag→spec mapping for every flag family (`appopts_test.go`), resource selector
  parsing and expansion (`selector_test.go`), local archives with include/ignore rules and
  symlinks (`localtar_test.go`), diff rendering, external diff programs, client-side and
  server-side diff, exit codes (`diff_test.go`), logs query/reconnect, wait conditions,
  action parameters, history tables, change detection (`units_test.go`).
- End-to-end tests (`e2e_test.go`, harness `e2e_harness_test.go`): the real CLI root command
  (`internal/cli/clitest`) against the real API server with the applications and
  applicationsets services, auth (login session tokens), RBAC (admin, read-only alice, bob
  limited to project team-a, nobody), settings, the real controller tree API handler and
  client, envtest kube-apiserver 1.36 with the generated CRDs, an in-process repo-server fake
  and a controller stand-in. Flows: create (flags, files, stdin, upsert, strict decoding,
  finalizer), list/get (filters, refresh, outputs, operation), set/unset/edit/patch,
  add/remove-source with all source-addressing errors, hydrator flags and source options on
  hydrator Applications (set/unset/create -f, rejected location flags), diff (live, revision,
  server-side, local client-side and server-side generate with ignore rules, exit codes,
  rendered names with terminal escapes in headers, warnings and previews),
  sync (resources, exclusions, options, retry, info, dry-run, async, selector, preview,
  local), wait (sync/health/operation/delete/selector/timeout), history, rollback, manifests,
  resources, get/patch/delete-resource, actions list/run (ambiguous match, `--all`,
  parameters), terminate-op, confirm-deletion, delete (cascade, non-cascade, wait), appset
  create (dry-run, wait, upsert, conflict)/get/list/generate/delete, authorization denials
  and hidden existence.

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0):

```bash
go build ./...                                    # exit 0
go vet ./internal/cli/app/...                     # exit 0
golangci-lint run ./internal/cli/app/...          # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 -v ./internal/cli/app/...
# ok (39 top-level tests incl. 9 TestE2E*, none skipped, no race reports)
go test -count=1 ./internal/cli/app/...           # ok (without envtest the TestE2E* tests skip)
go run ./hack/parity && go run ./hack/parity -check   # exit 0
```
