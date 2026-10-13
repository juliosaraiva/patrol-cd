# Frozen Argo CD parity baseline

| Field | Value |
|-------|-------|
| Upstream project | Argo CD (`github.com/argoproj/argo-cd`) |
| Release tag | `v3.5.5` |
| Source commit | `a5038b0e2bbac1715754380ae0084df8e3d5b48c` |
| Release published | 2026-10-12T10:59:41Z |
| Documentation version | `docs/` tree at the tag above (https://github.com/argoproj/argo-cd/tree/v3.5.5/docs), published as https://argo-cd.readthedocs.io/en/release-3.5/ |
| Research date | 2026-10-12 |
| Go module | `github.com/argoproj/argo-cd/v3` (go 1.26.3) |
| Upstream license | Apache-2.0 |

## Selection evidence

`gh release list -R argoproj/argo-cd --limit 15` on 2026-10-12 returned (excerpt):

```
v3.3.16		v3.3.16	2026-10-12T10:59:23Z
v3.5.5	Latest	v3.5.5	2026-10-12T10:59:41Z
v3.4.11		v3.4.11	2026-10-12T10:45:13Z
v3.6.0-rc2	Pre-release	v3.6.0-rc2	2026-10-06T20:50:11Z
v3.6.0-rc1	Pre-release	v3.6.0-rc1	2026-09-16T00:04:41Z
```

`v3.5.5` is marked `Latest`, `prerelease: false`, `draft: false`
(`gh api repos/argoproj/argo-cd/releases/tags/v3.5.5`). `v3.6.0-rc*` are prereleases and
are out of scope. `gh api repos/argoproj/argo-cd/git/ref/tags/v3.5.5` resolves to commit
`a5038b0e2bbac1715754380ae0084df8e3d5b48c` (lightweight tag → commit). The `VERSION`
file at that commit contains `3.5.5`.

## Bundled tool versions at the baseline

From `hack/tool-versions.sh` at the tag:

| Tool | Version |
|------|---------|
| Helm | 4.2.1 |
| Kustomize | 5.8.1 |
| git-lfs | 3.7.1 |
| oras | 1.2.0 |

Key library versions (`go.mod`): Kubernetes client libraries v0.36.1, controller-runtime
v0.24.1, structured-merge-diff v6.4.0, gopher-lua v1.1.2, expr v1.17.8, go-jsonnet v0.22.0,
gojq v0.12.19, sprig v3.3.0. `gitops-engine` now lives inside the argo-cd repository
(`github.com/argoproj/argo-cd/gitops-engine`).

## Distribution contents (what "core Argo CD" means here)

Components shipped by the baseline manifests (`manifests/install.yaml`,
`manifests/ha/install.yaml`, `manifests/core-install.yaml`,
`manifests/namespace-install.yaml`, and `*-with-hydrator` variants):

- `argocd-server` (API, UI, webhooks, terminal, extensions proxy)
- `argocd-application-controller` (StatefulSet, sharded)
- `argocd-repo-server` (+ config management plugin sidecars)
- `argocd-applicationset-controller`
- `argocd-notifications-controller`
- `argocd-commit-server` (source hydrator; `*-with-hydrator` manifests)
- `argocd-dex-server` (bundled Dex for SSO; an independently deployable upstream project)
- `argocd-redis` / `argocd-redis-ha` (cache)

### Explicitly out of scope (separate ecosystem products)

Argo Rollouts, Argo Workflows, Argo Events, Argo CD Image Updater, Argo CD Autopilot,
Argo CD Agent, Akuity/Codefresh products. Their CRDs are covered only where the core
distribution ships behavior for them (built-in Lua health checks and resource actions under
`resource_customizations/`, e.g. `argoproj.io/Rollout`).

### Feature maturity at the baseline

From `docs/operator-manual/feature-maturity.md`:

| Feature | Introduced | Status |
|---------|-----------|--------|
| AppSet Progressive Syncs | v2.6.0 | Beta |
| Proxy Extensions | v2.7.0 | Beta |
| Skip Application Reconcile | v2.7.0 | Alpha |
| Cluster Sharding: round-robin | v2.8.0 | Alpha |
| Dynamic Cluster Distribution | v2.9.0 | Alpha |
| Cluster Sharding: consistent-hashing | v2.12.0 | Alpha |
| Service Account Impersonation | v2.13.0 | Beta |
| Source Hydrator | v2.14.0 | Beta |
| ApplicationSet Web UI | v3.5.0 | Alpha |

Alpha/Beta features are in scope (they ship in the distribution) and are tracked in the
matrix with their maturity.

## Research method

Each feature area was inventoried from, in order of authority:

1. Source code at the pinned commit (`pkg/apis/application/v1alpha1`, `controller/`,
   `gitops-engine/`, `reposerver/`, `server/`, `applicationset/`, `util/`, `cmd/`).
2. Upstream tests (`*_test.go`, `test/e2e/`).
3. API definitions (`server/*/*.proto`, `assets/swagger.json`) and CRD schemas
   (`manifests/crds/`).
4. Generated CLI reference (`docs/user-guide/commands/`).
5. Documentation (`docs/`) and release notes.

Where documentation and implementation disagree, the implementation is treated as the
baseline and the discrepancy is recorded in the area file under "Doc/implementation
differences".

## Later upstream changes (not in scope)

Recorded in `docs/parity/upstream-changes-after-baseline.md`. The baseline is frozen; later
releases (including v3.6.0 when it ships) are tracked there only.
