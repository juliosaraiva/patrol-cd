# Examples

`examples/` holds small, runnable examples of every Patrol CD kind, source type, ApplicationSet
generator, setting and extension point. Every object is validated against a real
kube-apiserver with the Patrol CD CRDs, and every source is rendered with the real tools, by
`go test ./test/examples` (see [Validation](#validation)).

The examples reference the CRDs (`deploy/crds`, [CRD reference](../reference/crds.md)) and
settings objects only; nothing in them depends on component flags. To run Patrol CD itself, see
the [installation guide](installation.md).

## Conventions

- **`examples/` is the root of a sample Git repository.** Application paths are relative to it
  (`apps/helm-chart/chart`). To deploy the examples, push the directory to a Git server and
  replace the placeholder URLs:

  | Placeholder | Content |
  |-------------|---------|
  | `https://git.example.com/platform/patrol-examples.git` | `examples/` |
  | `https://git.example.com/platform/patrol-examples-values.git` | `examples/apps/multi-source-helm-values/values-repo` |
  | `https://github.com/example-org/guestbook.git` | `examples/apps/helm-chart` (the chart is in `chart/`) |
  | `oci://registry.example.com/charts/guestbook`, `registry.example.com/charts` + chart `guestbook` | `examples/apps/helm-chart/chart`, pushed as described in `apps/helm-oci-notes/README.md` |

  `test/examples` uses the same mapping (`exampleRepos`, `exampleCharts` in
  `test/examples/render_test.go`).
- **Control-plane namespace `patrol`.** Applications, ApplicationSets, projects, repositories,
  clusters, SCM providers and settings live there.
- **Secrets are templates.** Every Secret has `stringData` values `REPLACE_ME` and nothing else
  (enforced by the test). Create real Secrets out of band; never commit credentials. Secrets
  referenced by Patrol kinds carry `cd.patrol.example/secret-type` (`repository`, `repo-creds`,
  `repo-write-creds`, `cluster`, `scm-creds`); Secrets referenced from settings carry
  `app.kubernetes.io/part-of: patrol-cd`.
- **Public keys and certificates** in `platform/settings` (TLS CA, SSH host keys, GnuPG key) were
  generated for the examples, their private halves discarded. Replace them with your own.
- Annotations, labels and finalizers use the `cd.patrol.example/` keys of
  `api/v1alpha1/keys.go`.

## Trying them on a kind cluster

Use the disposable kind clusters of `hack/kind` (`patrol-control`, `patrol-target`; their
kubeconfigs are written to `.cache/kind/`, never to `~/.kube/config`) and always pass the
kubeconfig and context explicitly:

```bash
hack/kind/up.sh                       # or the kind-up make target
eval "$(hack/kind/env.sh)"            # PATROL_CONTROL_KUBECONFIG, PATROL_CONTROL_CONTEXT, ...
k() { kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" "$@"; }

k apply --server-side -f deploy/crds
k create namespace patrol
k apply --server-side -R -f examples/platform
k apply --server-side -f examples/apps/helm-chart/application.yaml
# Anything else: validate without persisting.
k apply --server-side --dry-run=server -f examples/apps/appset-matrix/applicationset.yaml
```

`hack/kind/register-target.sh --namespace patrol` registers `patrol-target` as a destination
with real credentials (`examples/platform/clusters/kind-target.yaml` shows the shape of that
Cluster). `examples/apps/plain-yaml/sync-operation.yaml` uses `generateName`: create it with
`k create -f`. `examples/github/04-notifications.yaml` is merged into
`patrol-notifications-cm` (see `examples/github/README.md`). Until the controllers are
deployed, objects are admitted (schema, CEL rules, admission policy) but not reconciled.

## Index

### Application sources (`examples/apps`)

Each directory holds the repository content and the Application that deploys it.

| Directory | Demonstrates | Render it locally |
|-----------|--------------|-------------------|
| `plain-yaml` | Directory source; sync waves (`cd.patrol.example/sync-wave`); `PreSync`/`PostSync` hook Jobs with delete policies; automated sync, retry, `CreateNamespace`; a manual sync `Operation` (`sync-operation.yaml`) | `cat examples/apps/plain-yaml/manifests/*.yaml` |
| `directory-recurse-include-exclude` | `directory.recurse`, `include`/`exclude` globs (the Application lists which files are rendered) | — |
| `helm-chart` | Chart with `values.yaml`, `values-prod.yaml`, `values.schema.json`, a Helm hook mapped to `PreSync`, optional HPA and Namespace; Application with `valueFiles`, `parameters`, `valuesObject`, `ignoreDifferences` | `helm template guestbook examples/apps/helm-chart/chart -n guestbook-helm -f examples/apps/helm-chart/chart/values-prod.yaml` |
| `helm-oci-notes` | Pushing the chart to an OCI registry; `oci://` source and Helm-over-OCI chart source | `helm package examples/apps/helm-chart/chart` |
| `kustomize-base-overlays` | Base, `dev`/`staging`/`prod` overlays, `images`, `replicas`, strategic-merge and JSON 6902 patches, `configMapGenerator` merge, components (`monitoring`, `high-availability`); `manifest-generate-paths` | `bin/kustomize build examples/apps/kustomize-base-overlays/overlays/prod` |
| `kustomize-helm-inflation` | Kustomize `helmCharts` with a vendored chart, post-render patch (needs `kustomize.buildOptions: --enable-helm`) | `bin/kustomize build --enable-helm examples/apps/kustomize-helm-inflation` |
| `jsonnet` | Directory source with Jsonnet: `extVars` (string and code), `tlas`, `libs` (vendored library) | `jsonnet -J examples/apps/jsonnet/vendor --ext-str environment=staging --ext-code replicas=2 --tla-str imageTag=6.7.1 --tla-code 'features={ networkPolicy: true }' examples/apps/jsonnet/app/main.jsonnet` |
| `cmp-plugin` | Config Management Plugin: `plugin.yaml` (discovery by `fileName`, static parameter announcements), POSIX shell generate script, ConfigMap packaging, Application with plugin `env` and string/map parameters | see `examples/apps/cmp-plugin/README.md` |
| `multi-source-helm-values` | Multiple sources: chart from one repository, values from another through `$values` | `helm template guestbook examples/apps/helm-chart/chart -f examples/apps/helm-chart/chart/values-prod.yaml -f examples/apps/multi-source-helm-values/values-repo/teams/team-a/guestbook/values.yaml` |
| `app-of-apps` | Parent Application rendering child Applications from a chart, ordered by sync waves (each wave waits for the previous one to be healthy only with the Application health customization of `platform/settings/patrol-cm.yaml`) | `helm template platform-apps examples/apps/app-of-apps/chart -n patrol` |
| `source-hydrator` | `spec.sourceHydrator` (dry source, sync source, `hydrateTo`) and write credentials (`purpose: write`) | — |

The jsonnet command needs a `jsonnet` binary (for example
`go run github.com/google/go-jsonnet/cmd/jsonnet@v0.22.0`, run outside this module).

### ApplicationSets (`examples/apps/appset-*`)

All use Go templates with `missingkey=error`. The generated Applications are the ones
`test/examples` expects (`expectedApplications`) against the example clusters, repositories,
a fake GitHub organization and a stub plugin service.

| Directory | Generator | Generates |
|-----------|-----------|-----------|
| `appset-list` | List | `guestbook-list-dev`, `guestbook-list-staging` |
| `appset-cluster` | Clusters with `matchExpressions` on Cluster labels | one per `staging`/`prod` Cluster |
| `appset-git-directories` | Git directories with an exclusion | `guestbook-overlay-dev`, `guestbook-overlay-prod` |
| `appset-git-files` | Git files (`config/*/config.json`) | `guestbook-config-dev`, `guestbook-config-prod` |
| `appset-matrix` | Matrix: Clusters × List | 4 (2 clusters × 2 components), project `team-a` |
| `appset-merge` | Merge on `server`: Clusters + List override | `guestbook-merged-<cluster>` with per-cluster replicas |
| `appset-pull-request-github` | Pull request (GitHub, `tokenRef`, label and branch filters) | `pr-<number>-guestbook` per open PR labelled `preview` |
| `appset-scm-provider-github` | SCM provider (GitHub, `providerRef`, `repositoryMatch`, `pathsExist`) | one per matching repository |
| `appset-progressive-rollingsync` | List + `strategy: RollingSync` (`maxUpdate`, `deletionOrder: Reverse`) | 4 environments in 3 steps |
| `appset-plugin-generator` | Plugin generator, its ConfigMap and token Secret | one per tenant returned by the service |

### Platform (`examples/platform`)

| Path | Contents |
|------|----------|
| `projects/default.yaml` | Permissive `default` project (as Argo CD's built-in one). |
| `projects/team-a.yaml` | Strict tenant: source repositories, destinations (server and name), no cluster resources, namespaced allow/deny lists, `orphanedResources`, roles with groups, sync windows with time zones, `destinationServiceAccounts`, `sourceNamespaces`. |
| `projects/signed-commits.yaml` | `sourceIntegrity` with a GnuPG `head` policy. |
| `scm/github-app.yaml` | `SCMProvider` with a GitHub App (`privateKeySecretRef`), webhook `secretRef`, `statusReporting` (mode `checks`) and `rateLimit` (validated, not wired yet: #235, #236). |
| `scm/github-token.yaml` | `SCMProvider` with a token (`tokenSecretRef`), `statusReporting` mode `statuses` (validated, not wired yet: #235). |
| `scm/github-enterprise.yaml` | GitHub Enterprise Server: `apiURL`, `webURL`, `caSecretRef`. |
| `repositories/github-app.yaml` | `RepositoryCredential` using the SCMProvider's App; `Repository` with `githubApp` and a key in `credentialsRef`. |
| `repositories/github-enterprise.yaml` | GitHub App credential template for GHES (`enterpriseBaseURL`). |
| `repositories/ssh.yaml` | Plain SSH repository. |
| `repositories/https-basic.yaml` | HTTPS credential template for every repository below a URL prefix. |
| `repositories/helm.yaml` | Classic Helm repository. |
| `repositories/oci-registry.yaml` | OCI credential template and a Helm-over-OCI repository. |
| `clusters/kind-target.yaml` | The kind cluster `patrol-target` as a destination (bearer token + CA in the Secret). |
| `clusters/eks-prod.yaml` | EKS with `awsAuthConfig`, shard pinning. |
| `clusters/exec-provider.yaml` | Exec credential plugin, project-scoped cluster limited to namespaces. |
| `settings/patrol-cm.yaml` | URL, users, OIDC with `$secret` references, tracking, Kustomize options and versions, Lua health and actions (from `plugins/lua`), Application health (`lua.ApplicationHealthLua`, needed for health-gated app-of-apps waves), `ignoreDifferences`, exclusions, compare options, deep links, banner, hydrator author. |
| `settings/patrol-secret.yaml` | `patrol-secret` (webhook secrets) and the `patrol-oidc` Secret referenced by `oidc.config`. |
| `settings/patrol-rbac-cm.yaml` | Default role, scopes, team policies with a deny rule, an overlay key (`policy.ci.csv`). |
| `settings/patrol-notifications-cm.yaml`, `patrol-notifications-secret.yaml` | Slack, GitHub, webhook and e-mail services; a webhook template and trigger; default subscriptions to catalog triggers; `defaultTriggers`. |
| `settings/patrol-cmd-params-cm.yaml` | Component parameters (application namespaces, hydrator, SCM providers, progressive syncs, ...). |
| `settings/patrol-tls-certs-cm.yaml`, `patrol-ssh-known-hosts-cm.yaml`, `patrol-gpg-keys-cm.yaml` | Repository TLS CA, SSH known hosts, GnuPG keys. |

### GitHub (`examples/github`)

Pull request preview environments with a GitHub App: SCMProvider and repository credentials,
a `previews` project, a pull request ApplicationSet with `providerRef` (namespace per pull
request, cleanup when the pull request closes), commit status and pull request comment
notifications, the App permission list (from
[github-permissions.md](../integrations/github-permissions.md)) and a webhook registration
checklist. Start with `examples/github/README.md`. Nothing there registers an App or webhook.

### Notifications (`examples/notifications`)

Subscription annotations on an Application (single trigger, default triggers, the
`subscriptions` list) and on an AppProject. Services, templates and triggers are in
`platform/settings/patrol-notifications-cm.yaml` and `github/04-notifications.yaml`.

### Plugins (`examples/plugins`)

| Path | Contents |
|------|----------|
| `lua/` | Health check and actions (`suspend`, `resume`, `set-retention` with a parameter) for the example CRD `ops.example.com/BackupSchedule`, with `health_test.yaml`/`action_test.yaml` fixtures in the format of `internal/lua/customizations`. The same scripts are embedded in `patrol-cm`. |
| `ui-extension/` | TypeScript/React resource tab for `BackupSchedule`, built by esbuild into one script that uses the host's React (`package.json` included, dependencies not installed). |

## Validation

`test/examples` (package `examples_test`) checks everything under `examples/`:

| Test | What it does |
|------|--------------|
| `TestInventory` | Every file has a validation (unknown file types fail), YAML parses strictly (duplicate keys fail), JSON parses, `sh -n` on scripts, Secret templates contain only `REPLACE_ME`, no two control-plane objects share a name. |
| `TestExamplesAgainstAPIServer` | envtest kube-apiserver with `deploy/crds` (+ the operations admission policy) and the example CRD. Server-side dry-run create with strict field validation of every object; `helm lint --strict` and `helm template` of every chart (default values and each `values-*.yaml`); `kustomize build --enable-helm` of every Kustomization (components must be used); every Application validated with `project.ValidateApplication` and each source rendered as Patrol CD would (Helm value files/`$ref`/parameters, Kustomize, Directory recurse/include/exclude, Jsonnet through `github.com/google/go-jsonnet` with the Application's extVars/TLAs/libs, the plugin's generate command with the plugin environment); rendered objects must be permitted by the project and are dry-run created; app-of-apps children are processed recursively; every ApplicationSet is expanded with `appset.NewAPIGenerator` (real generators against the example Clusters/Secrets/SCMProviders, a fake GitHub from `pkg/scm/fake`, a stub plugin service) and compared with `expectedApplications`; Application names (static, generated, app-of-apps children) must be unique in the control-plane namespace; coverage: every `.jsonnet`, `*.yaml.tmpl` and plugin script was exercised. |
| `TestSettings` | `settings.Parse` of the settings ConfigMaps and Secrets reports no issue (a generated `server.secretkey` stands in for the bootstrap); `patrol-cmd-params-cm` keys resolve for every component. |
| `TestRBACPolicy` | `rbac.Validate` of `patrol-rbac-cm` (errors and warnings fail), scopes, and enforcement spot checks. |
| `TestAppProjects` | `project.ValidateProject`, project role policies, sync window schedules; `project.CanSync` on `team-a`: the night deny window blocks automated syncs to `prod-us` only (by name or URL, manual syncs allowed), the business-hours allow window restricts only the guestbook applications. |
| `TestNotifications` | `notifications.ParseConfig` (with the built-in catalog) and `Validate` of the platform config, the GitHub fragment and both merged; the GitHub fragment must not redefine a platform key (a merge patch replaces whole values); every service built, every template rendered (no field may render `<no value>`) and every trigger evaluated against a sample Application; every subscription (annotations of Applications, AppProjects and ApplicationSet templates, plus the default subscriptions matching each Application's labels) resolves to a defined trigger and configured service, each subscribed template is rendered for its destination, and a webhook destination needs a `webhook.<service>` section in every template its trigger sends. |
| `TestLuaCustomizations` | Runs every `health_test.yaml`/`action_test.yaml` case against the customizations as loaded from `patrol-cm`, and fails if `patrol-cm` and `plugins/lua` differ; the `cd.patrol.example_Application` health script must equal `lua.ApplicationHealthLua` and agree with `health.ApplicationHealth`. |
| `TestCMPPluginConfigs` | `cmp.ParsePluginConfig` with no warnings. |

Run it:

```bash
make envtest                      # once: installs kube-apiserver/etcd 1.36 into bin/envtest
make tools                        # once: bin/kustomize
go test -count=1 -v ./test/examples/
```

- The envtest binaries are taken from `KUBEBUILDER_ASSETS` or `bin/envtest/k8s/1.36.0-<os>-<arch>`.
  Without them the API server test is skipped (and fails when `PATROL_REQUIRE_ENVTEST` is set,
  as in `make test`).
- `helm` and `kustomize` are taken from `$HELM`/`$KUSTOMIZE`, then `bin/kustomize`, then `PATH`.
  Without them only the rendering subtests of the API server test (`helm charts`,
  `kustomizations`, `applications`, `applicationsets`, `coverage`) are skipped; the dry-run of
  every static object still runs. They fail instead when `PATROL_REQUIRE_EXAMPLE_TOOLS` or
  `PATROL_REQUIRE_ENVTEST` is set, so `make test` (no `-v`, where a skip is invisible) cannot
  pass without rendering the examples.
- Skips are reported with `SKIPPED:` messages; the summary at the end of the verbose output
  lists what was exercised (files per type, dry-run creates per kind, renders per tool,
  generated Applications, tool invocations) and the API server warnings.

The UI extension is not compiled by the Go test. Verify it with Node.js 22:

```bash
cd examples/plugins/ui-extension
pnpm install && pnpm typecheck && pnpm build && pnpm smoke
```

### Last verification

2026-10-16, darwin/arm64, Go 1.27.2, envtest kube-apiserver 1.36.0, helm v4.3.0, kustomize v5.8.1:

- `PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_EXAMPLE_TOOLS=1 go test -count=1 -race -v ./test/examples/`
  — `ok` (8 tests, 24 tests and subtests pass, none skipped). `make test PKGS=./test/examples/`
  — `ok`; the same with `HELM=/nonexistent/helm` fails (`FAIL`, exit 2) in the five rendering
  subtests, and with `HELM=/nonexistent/helm` and no `PATROL_REQUIRE_*` variable the static
  dry-run (99 objects) and seeding pass while the five rendering subtests report `SKIPPED:`. Summary: 143 files
  (99 YAML, 6 JSON, 3 Jsonnet, 12 Helm template, 5 Lua, 1 shell, 2 plugin templates, 4 TypeScript,
  1 JavaScript, 9 Markdown, 1 .gitignore); 307 server-side dry-run creates (42 Applications,
  11 ApplicationSets, 5 AppProjects, 4 SCMProviders, 4 Repositories, 6 RepositoryCredentials,
  3 Clusters, 1 Operation, 23 Secrets, 54 ConfigMaps and the rendered workloads); 4 helm
  lint+template runs, 6 kustomize builds, 39 Applications rendered (36 static or generated,
  3 app-of-apps children), 11 ApplicationSets expanded into 23 Applications. The API server
  warns 33 times that `resources-finalizer.cd.patrol.example` should be a domain-qualified
  name with a path.
- Mutation check: 15 deliberate defects (unknown field, project not permitting a rendered kind,
  values schema violation, unexpected generated Application, missing Jsonnet extVar, invalid
  plugin parameter, Lua drift between `patrol-cm` and fixtures, typo in an ApplicationSet
  template, missing Kustomize component, missing `$values` file, unknown RBAC resource,
  undefined notification template, non-placeholder Secret value, invalid settings duration,
  two ApplicationSets generating the same Application name) each made the corresponding test
  fail. Restoring the previous versions of `github/04-notifications.yaml` (redefined `context`,
  so `deploy-tracker-event` rendered `"environment": "<no value>"`),
  `notifications/application-subscriptions.yaml` (`on-deployed` → `deploy-tracker` with a
  template that has no webhook body), `platform/projects/team-a.yaml` (deny window matching
  every application, allow window matching every `team-a-*` namespace) and
  `platform/settings/patrol-cm.yaml` (no Application health) made `TestNotifications`,
  `TestAppProjects` and `TestLuaCustomizations` fail.
- UI extension (with typescript 5.9.3, @types/react 19.2.18, esbuild 0.28.2, react/react-dom
  19.2.8): `tsc -p tsconfig.json` clean; the esbuild `build` command produced a 1.5 KB bundle
  without imports; `scripts/smoke.mjs` registered and rendered the tab.

## Adding an example

1. Put repository content under `examples/apps/<name>/` next to the Application or
   ApplicationSet that deploys it; reuse the placeholder URLs above (or add a mapping to
   `exampleRepos`/`exampleCharts`).
2. Objects of the control plane go to namespace `patrol`; Secrets use `stringData` with
   `REPLACE_ME`.
3. New ApplicationSets need an entry in `expectedApplications`
   (`test/examples/generators_test.go`); new file types need a class in `classify`
   (`test/examples/inventory_test.go`).
4. Lua customizations: add the scripts and fixtures under `plugins/lua/<group>/<Kind>` and the
   same scripts to `platform/settings/patrol-cm.yaml`.
5. Run `go test -count=1 -v ./test/examples/` and index the example in this page.
