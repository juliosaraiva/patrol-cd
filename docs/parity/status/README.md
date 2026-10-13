# Parity status overlays

One YAML file per workstream (e.g. `sync-engine.yaml`, `apiserver.yaml`). Each maps feature
IDs from `docs/parity/areas/*.md` to:

```yaml
SYNC-012:
  status: verified            # not-started|in-progress|implemented|verified|verified-simulated|blocked|different-by-design
  impl: [internal/sync/options.go]
  tests: [internal/sync/options_test.go:TestPruneLast]
  evidence: docs/verification/log.md#V-0042
  notes: optional free text (required for different-by-design and blocked)
```

`go run ./hack/parity` merges all files (least-advanced status wins when several workstreams
contribute to one feature) and regenerates `docs/parity/matrix.md`. `-check` fails CI when
the matrix is stale, an ID is unknown, or a verified entry lists no tests.
