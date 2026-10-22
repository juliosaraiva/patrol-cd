# e2e fixtures

In-cluster services the end-to-end tests run against. They are deployed into namespace
`patrol-e2e` of the `patrol-control` kind cluster (`hack/kind/up.sh`) and use only images built
locally from official base images (`alpine`, `oras`). Building them the first time needs network
access (see [Network and offline use](#network-and-offline-use)). The full walkthrough is in
[docs/guides/local-development.md](../../../docs/guides/local-development.md).

```bash
hack/kind/up.sh                      # clusters + registry
test/e2e/fixtures/deploy.sh          # build, load, deploy, seed, push OCI artifacts
test/e2e/fixtures/smoke.sh           # in-cluster checks (add --with-registry-image)
test/e2e/fixtures/validate.sh        # offline: every manifest against envtest + deploy/crds
test/e2e/fixtures/test-scripts.sh    # offline: guards, register-target.sh, patrol/register.sh on envtest
```

| Path | Contents |
|------|----------|
| `namespace.yaml` | Namespace `patrol-e2e` |
| `git-server/` | Image (`patrol-cd/e2e-git-server:dev`): nginx + fcgiwrap + `git-http-backend` (smart HTTP) and OpenSSH + `git-shell`, one pod, shared `/git`. `manifests/` (Deployment, Service, **TEST-ONLY** SSH host key Secret), `seed-job.yaml`, `known_hosts` |
| `seed/repos/` | Minimal apps seeded as repositories (always): `guestbook` (plain YAML), `helm-guestbook` (chart), `kustomize-guestbook` (base + `staging`/`production` overlays), `helm-values` (values for multi-source) |
| `seed/updates/` | Per-repository overlay applied for the second seeded revision (`v1.1.0`/`main`) |
| `chart-repo/` | Image (`patrol-cd/e2e-chart-repo:dev`): nginx serving `helm package` output + `index.yaml`; `build.sh` is the packaging step |
| `oci/push.sh` | Pushes the chart (`helm push`) and a plain-manifest bundle (`oras push`) into `patrol-registry` |
| `toolbox/` | Image (`patrol-cd/e2e-toolbox:dev`: git, ssh, helm, oras, kubectl, curl, jq) and a long-running `toolbox` Pod |
| `patrol/` | Patrol CD objects for the fixtures: `repositories.yaml`, `applications.yaml`, `register.sh` |
| `webhook/send-github-push.sh` | Signed GitHub webhook sender (bash + curl + openssl) |
| `fake-github/` | Placeholder: plan for an in-cluster GitHub API double |
| `build-images.sh`, `deploy.sh`, `smoke.sh`, `validate.sh`, `test-scripts.sh`, `lib/envtest.sh` | Tooling |

## Network and offline use

These steps need network access:

| Step | Downloads |
|------|-----------|
| Pulling the base images below (once) | from Docker Hub and ghcr.io |
| First `build-images.sh` run (also run by `deploy.sh`), or any rebuild after the build cache was pruned | Alpine v3.24 packages: `apk add` in all three Dockerfiles (`main` and `community`) |

Base images to pull while online (otherwise `up.sh` and `docker build` pull them on first use):

```bash
docker pull kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed  # kind nodes
docker pull registry:3.0.0@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e         # patrol-registry
docker pull alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6       # fixture images
docker pull ghcr.io/oras-project/oras:v1.3.4@sha256:f7bc056d54d97baa399414ed5048ecc67c3371b750d4bbce1d871827a5758179  # toolbox build stage
```

After the fixture images have been built once, the fixtures work offline:

- `deploy.sh --skip-build` loads the existing `patrol-cd/e2e-*:dev` images into `patrol-control`
  and pulls nothing. The images stay in the local Docker image store until removed; note that
  `docker image prune -a` removes them, because no host container uses them.
- A rebuild (`build-images.sh`, or `deploy.sh` without `--skip-build`) reuses the `apk add`
  layer from the build cache: in each Dockerfile it comes right after `FROM` and before any
  `COPY` of local content, so changing seeds, scripts or configuration does not invalidate it.
  A rebuild fails offline once that cache entry is gone (`docker builder prune`, or Docker
  Desktop's automatic build-cache garbage collection).
- Seeded workloads run `registry.k8s.io/pause:3.10`, the containerd sandbox image of kind's node
  image, so they need no pull.

None of this has been exercised on a live cluster yet: see
[docs/verification/runs/kind-environment.md](../../../docs/verification/runs/kind-environment.md).

## Endpoints (from pods in patrol-control)

| Service | URL |
|---------|-----|
| git, basic auth (read/write, push creates missing repositories) | `http://git-server.patrol-e2e.svc.cluster.local/git/<repo>.git` |
| git, anonymous read-only | `http://git-server.patrol-e2e.svc.cluster.local/public/<repo>.git` |
| git over SSH (user `git`, key auth) | `ssh://git@git-server.patrol-e2e.svc.cluster.local/git/<repo>.git` |
| Helm repository | `http://chart-repo.patrol-e2e.svc.cluster.local` (anonymous), `.../private` (basic auth) |
| OCI chart | `oci://patrol-registry:5000/patrol-e2e/charts/helm-guestbook` (`0.1.0`, `0.2.0`, plain HTTP) |
| OCI manifest bundle | `oci://patrol-registry:5000/patrol-e2e/manifests/guestbook` (`v1.0.0`, `v1.1.0`, plain HTTP) |
| patrol-target API | `https://patrol-target-control-plane:6443` (kubeconfig in Secret `patrol-target-kubeconfig`) |

## Seeded repositories

Always: `guestbook`, `helm-guestbook`, `kustomize-guestbook`, `helm-values` and the monorepo
`apps` (all four as subdirectories). These are what `smoke.sh` and `patrol/applications.yaml`
use; `PATROL_E2E_SEED_DIR` replaces their source directory.

When the repository's `examples/` exists at image build time, it is also seeded as
`patrol-examples.git` (the example Applications use paths such as `apps/plain-yaml/manifests`
relative to that root), and `examples/apps/multi-source-helm-values/values-repo` as
`patrol-examples-values.git`. `PATROL_E2E_EXTRA_REPOS="name=dir ..."` replaces this list (empty
disables it). Extra repositories get the same refs, without an updates overlay, and are not part
of `apps`.

| Ref | Commit | Content |
|-----|--------|---------|
| `v1.0.0` (annotated), `release-1.0` | c1 | seed content, `e2e-revision.txt` = `1` |
| `v1.1.0` (lightweight), `main` (HEAD) | c2 | + `seed/updates/<repo>` overlay, `e2e-revision.txt` = `2` |
| `feature/e2e` | c3 | + `e2e-feature.txt` |

Author, committer and dates are fixed, so commit SHAs depend only on the seed content.
Re-running the `git-seed` Job (`deploy.sh` does) force-pushes and prunes: every seeded
repository returns to exactly this state.

Overlay effects: guestbook and helm-guestbook `replicas`/`replicaCount` 1 → 2, kustomize
`staging` replicas 1 → 2, helm-values `values/dev.yaml` `replicaCount` 1 → 2. Workloads run
`registry.k8s.io/pause:3.10`, containerd's sandbox image on kind nodes, so they start offline.

## Credentials

Created by `deploy.sh` in `patrol-e2e` when missing; values are random, never printed and only
stored in the cluster:

| Secret | Keys | Used by |
|--------|------|---------|
| `git-server-http-auth` | `username`, `password` | nginx htpasswd (generated at container start), seed Job, toolbox |
| `git-server-ssh-authorized-keys` | `authorized_keys` | sshd |
| `git-server-ssh-client` | `sshPrivateKey`, `known_hosts` | tests, toolbox (same key names as a Patrol repository Secret) |
| `chart-repo-auth` | `username`, `password` | chart repo `/private`, toolbox |
| `patrol-target-kubeconfig` | `kubeconfig` | toolbox (cross-cluster checks) |

`git-server/manifests/ssh-host-key.secret.yaml` holds a **TEST-ONLY** SSH host key committed on
purpose so `git-server/known_hosts` is stable. It protects nothing; never reuse it.
