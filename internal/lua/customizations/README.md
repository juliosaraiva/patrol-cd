# Built-in resource customizations (third-party data)

This directory is a verbatim copy of the `resource_customizations/` directory of the Argo CD
project:

- Source: https://github.com/argoproj/argo-cd/tree/v3.5.5/resource_customizations
- Tag: `v3.5.5`, commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`
- License: Apache License, Version 2.0 (the same license as Patrol CD; see the repository
  `LICENSE` file). Attribution is recorded in the repository `NOTICE` file.
- Copyright: The Argo Authors.

Patrol CD reuses these files as **behavioral data** so that health checks and resource
actions of third-party CRDs behave identically to the baseline (ADR-0001). No upstream Go
code is used: the scripts are executed by Patrol's own sandboxed runtime in `internal/lua`.

Layout (unchanged from upstream):

```
<group>/<Kind>/health.lua                       health check
<group>/<Kind>/health_test.yaml                 health test cases (inputPath → healthStatus)
<group>/<Kind>/testdata/*.yaml                  test inputs
<group>/<Kind>/actions/discovery.lua            action discovery
<group>/<Kind>/actions/<action>/action.lua      action script
<group>/<Kind>/actions/action_test.yaml         discovery and action test cases
<group>/<Kind>/actions/testdata/*.yaml          action test inputs and expected outputs
```

A `_` in a group or kind directory name is a wildcard (for example `_.crossplane.io/_`),
consulted only when no exact directory exists.

Differences from the upstream directory:

- `embed.go` (upstream Go code that embeds the directory) is not copied. Patrol embeds only
  the runtime scripts (`health.lua`, `actions/discovery.lua`, `actions/*/action.lua`) from
  `internal/lua/builtin.go`; test YAML stays on disk and is read by the tests.
- No file content was modified. Some scripts therefore still write user-visible text that
  names Argo CD (the Flux `reconcile` actions set `reconcile.fluxcd.io/requestedAt` to
  `By Argo CD at: <time>`, and the `batch/Job` `terminate` action records
  `Job was terminated explicitly through Argo CD`).
- Upstream quirks are preserved as data: `psmdb.percona.com/PerconaServerMongoDB/actions/pause/actions.lua`
  is misnamed upstream (not `action.lua`), so the `pause` action is not resolvable there
  either, and `argoproj.io/Application/actions/actions_test.yaml` uses a format upstream's
  test runner ignores (Patrol runs it in a dedicated test).

Run the data-driven tests with `go test ./internal/lua/ -run 'TestBuiltin|TestApplicationToggleAutoSync|TestEmbedded'`.
