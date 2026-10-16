# Parity status overlays

One YAML file per workstream (e.g. `sync-engine.yaml`, `apiserver.yaml`). Each maps feature
IDs from `docs/parity/areas/*.md` to:

```yaml
SYNC-012:
  status: implemented         # see below
  impl: [internal/sync/options.go]
  tests: [internal/sync/options_test.go:TestPruneLast]
  evidence: docs/verification/log.md#V-0042
  notes: optional free text (required for different-by-design and blocked)
```

Status meanings (be conservative):

- `in-progress` — partially implemented.
- `implemented` — the behavior exists in code and unit tests pass, but it has not yet been
  exercised through the integrated system (controller/API/CLI/UI as applicable).
- `verified` — exercised against real components (envtest API server, kind cluster, real
  git/helm/kustomize binaries) through the surfaces the feature is exposed on, tests passed.
- `verified-simulated` — passed only against fakes of an external system (GitHub, Slack, …).
- `blocked` — cannot proceed; notes say why.
- `different-by-design` — intentional difference; notes link the documentation.

A library-level feature whose whole behavior lives in one package (e.g. RBAC policy matching)
may be `verified` by that package's tests when they use real inputs and no fakes.

`go run ./hack/parity` merges all files (least-advanced status wins when several workstreams
contribute to one feature) and regenerates `docs/parity/matrix.md`. `-check` fails CI when
the matrix is stale, an ID is unknown, or a verified entry lists no tests.
