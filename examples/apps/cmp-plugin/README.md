# Config Management Plugin (sidecar)

`tmpl-render` renders `*.yaml.tmpl` files by replacing `@@NAME@@` placeholders. It is a
POSIX shell + awk script, so the sidecar needs no custom image build.

| Path | Purpose |
|------|---------|
| `plugin/plugin.yaml` | `ConfigManagementPlugin`: generate command, discovery by `fileName`, static parameter announcements. |
| `plugin/generate.sh` | The generate command. Reads `PATROL_APP_NAME`, `ARGOCD_ENV_ENVIRONMENT`, `PARAM_*`. |
| `plugin/kustomization.yaml` | Packages both files into ConfigMap `patrol-cmp-tmpl-render` (`kustomize build plugin`). |
| `app/` | Application content (`*.yaml.tmpl`) — what a Git repository using the plugin contains. |
| `application.yaml` | Application selecting the plugin with parameters and env. |

## Running the sidecar

The plugin runs in its own container of the repo-server pod, next to the repo-server and
sharing the plugin socket directory with it:

- run `patrolcd cmp-server` (the `cmd/patrolcd` wiring and the Helm chart are pending; see
  `docs/architecture/components/cmp.md` for the server's settings and defaults);
- mount the ConfigMap `patrol-cmp-tmpl-render` at `/home/patrol/cmp-server/config`
  (`plugin.yaml` is read from there; `generate.sh` is executed from there);
- share the socket directory `/home/patrol/cmp-server/plugins` (an `emptyDir`) with the
  repo-server container;
- the container needs `sh`, `awk`, `sed`, `tr`, `sort` and `env` (any busybox-based image).

The plugin never receives repository credentials or a Kubernetes token, and only `PATH`,
`HOME`, `TMPDIR`, `LANG` of the sidecar environment plus the request variables reach the
script (`PATROL_CMP_PASS_ENV` widens that).

## Inputs

| Source | Variable |
|--------|----------|
| Application name | `PATROL_APP_NAME` / `ARGOCD_APP_NAME` (plus the other `*_APP_*` build variables) |
| `spec.source.plugin.env[ENVIRONMENT]` | `ARGOCD_ENV_ENVIRONMENT` |
| parameter `image` (string) | `PARAM_IMAGE` |
| parameter `replicas` (string) | `PARAM_REPLICAS` |
| parameter `extra-labels` (map) | `PARAM_EXTRA_LABELS_<KEY>` (upper-cased, `-` → `_`) |
| all parameters as JSON | `ARGOCD_APP_PARAMETERS` |

Try it locally:

```bash
cd examples/apps/cmp-plugin/app
env -i PATH="$PATH" PATROL_APP_NAME=guestbook-cmp ARGOCD_ENV_ENVIRONMENT=staging \
  PARAM_IMAGE=ghcr.io/stefanprodan/podinfo:6.7.1 PARAM_REPLICAS=2 PARAM_EXTRA_LABELS_TEAM=team-a \
  sh ../plugin/generate.sh
```
