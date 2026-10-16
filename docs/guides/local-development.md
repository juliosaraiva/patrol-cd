# Local development environment (kind)

Two disposable kind clusters, a local OCI registry and in-cluster e2e fixtures (git server,
Helm chart repository, OCI artifacts) for developing and testing Patrol CD on a workstation.

```
 host (macOS/Linux)                       docker network "kind"
 ─────────────────                        ───────────────────────────────────────────────────────────
 127.0.0.1:8443 ──────────────▶ patrol-control-control-plane :30443 (NodePort, Patrol UI/API)
 127.0.0.1:<random> ──────────▶ patrol-control-control-plane :6443   ┐ patrol-control
                                patrol-control-worker               ┘ (fixtures in ns patrol-e2e)
 127.0.0.1:<random> ──────────▶ patrol-target-control-plane  :6443   ◀── pods in patrol-control
                                                                          https://patrol-target-control-plane:6443
 127.0.0.1:5001 ──────────────▶ patrol-registry              :5000   ◀── pods: patrol-registry:5000
                                                                     ◀── node containerd: localhost:5001
```

Everything here is disposable: the scripts only create, change or delete kind clusters named
`patrol-*`, the `patrol-registry` container and local images tagged `patrol-cd/*:dev`.

## Requirements

| Tool | Tested version |
|------|----------------|
| Docker (Docker Desktop on macOS) | 29.8 |
| kind | v0.33.0 |
| kubectl | v1.36 |
| helm | v4.3 (`oci/push.sh`, `chart-repo/build.sh`) |
| git, openssl, ssh-keygen, curl | any recent |

Resources: three kind nodes (Kubernetes 1.36.4, `kindest/node:v1.36.4@sha256:099e…`, pinned in
`hack/kind/lib.sh`), the registry and the fixtures. As an estimate (not yet measured), plan for
about 4 GiB of free memory in the Docker VM and at least 5 GB of free disk in it (node image,
three node containers, fixture images). Check the disk with `docker system df`: when Docker's disk is full every container
start fails with `no space left on device` (see [Troubleshooting](#troubleshooting)).

Network: needed to pull the node, registry, `alpine` and `oras` base images and, on the first
fixture image build, for the `apk add` step of each fixture Dockerfile. After one successful
build, `test/e2e/fixtures/deploy.sh --skip-build` works offline, and rebuilds do too while the
build cache keeps the `apk add` layers. The exact image references and `docker pull` commands
are in [test/e2e/fixtures/README.md](../../test/e2e/fixtures/README.md#network-and-offline-use).

The scripts are written for bash 3.2 (macOS `/bin/bash`) and POSIX `sh` (in-container scripts).

## Create the clusters

```bash
hack/kind/up.sh
# or, until the root Makefile includes hack/kind/kind.mk:
make -f Makefile -f hack/kind/kind.mk kind-up
```

`up.sh` is idempotent. It:

1. creates (or starts) the registry container `patrol-registry` (`registry:3.0.0`) publishing
   `127.0.0.1:5001`, labelled `cd.patrol.example.kind-role=registry`;
2. creates `patrol-control` (control plane + worker, `hack/kind/patrol-control.yaml`) and
   `patrol-target` (one node, `hack/kind/patrol-target.yaml`), or starts their stopped node
   containers;
3. attaches the registry to the `kind` network and writes containerd `hosts.toml` files on every
   node;
4. waits for nodes `Ready` and CoreDNS, and publishes the KEP-1755 `local-registry-hosting`
   ConfigMap in `kube-public`;
5. writes kubeconfigs to `.cache/kind/` and checks that the `patrol-control` control-plane
   container reaches `https://patrol-target-control-plane:6443/version` with TLS verified.

Override the node or registry image with `PATROL_KIND_NODE_IMAGE` / `PATROL_REGISTRY_IMAGE`.

Other targets in `hack/kind/kind.mk`: `kind-down`, `kind-load`, `kind-status`, `kind-fixtures`.

## Kubeconfigs and the context guard

kind is always called with `--kubeconfig`, so `~/.kube/config` is never read or modified.

| File (`.cache/kind/`) | Context | Server |
|-----------------------|---------|--------|
| `patrol-control.kubeconfig` | `kind-patrol-control` | `https://127.0.0.1:<port>` |
| `patrol-target.kubeconfig` | `kind-patrol-target` | `https://127.0.0.1:<port>` |
| `patrol-target-internal.kubeconfig` | `kind-patrol-target` | `https://patrol-target-control-plane:6443` |

```bash
eval "$(hack/kind/env.sh)"
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" get nodes
```

`env.sh` exports `PATROL_CONTROL_KUBECONFIG`, `PATROL_CONTROL_CONTEXT`, `PATROL_TARGET_KUBECONFIG`,
`PATROL_TARGET_CONTEXT`, `PATROL_TARGET_INTERNAL_KUBECONFIG`, `PATROL_TARGET_INTERNAL_SERVER`,
`PATROL_REGISTRY` (`localhost:5001`), `PATROL_REGISTRY_INTERNAL` (`patrol-registry:5000`) and
`PATROL_UI_URL`. It does not export `KUBECONFIG`.

Every script goes through `hack/kind/lib.sh`, which refuses to run:

- kind against a cluster whose name is not `patrol-<name>`;
- kubectl without an explicit `--kubeconfig` file and `--context`, with a context not named
  `kind-patrol-<name>`, or with a context whose server is not a local kind endpoint
  (`https://127.0.0.1:*`, `https://localhost:*`, `https://[::1]:*` or
  `https://patrol-*-control-plane:6443`).

Scripts that need these checks source the library and call `patrol::kubectl <cluster> ...`.

## How patrol-control reaches patrol-target

Both clusters are kind clusters on the same Docker bridge network, `kind` (created by kind; all
kind clusters on the machine share it). Each node is a container whose name doubles as its DNS
name on that network, served by Docker's embedded DNS. The approach relies on kind's network
setup as follows:

- **Name resolution.** kind configures each node's resolver to use Docker's embedded DNS, and
  CoreDNS forwards non-cluster names to the node's resolver, so a pod in `patrol-control` resolves
  `patrol-target-control-plane` to that container's address on the `kind` network.
- **Routing.** Pod traffic leaves the `patrol-control` node container and enters the `kind`
  bridge; `patrol-target`'s API server listens on `:6443` on all interfaces inside its container.
  (The host reaches the same API server through a random `127.0.0.1` port mapping.)
- **TLS.** `kind get kubeconfig --internal` emits `https://patrol-target-control-plane:6443` with
  the cluster CA; kind's kubeadm configuration includes the control-plane container name in the
  API server certificate, so the connection verifies against that CA. `up.sh` writes this
  kubeconfig to `.cache/kind/patrol-target-internal.kubeconfig`.
- **Address.** The container's IP on the `kind` network is written to
  `.cache/kind/patrol-target-control-plane.ip` for diagnostics. Prefer the name: Docker may assign
  a different IP when the container restarts.
- **Distinct CIDRs.** `patrol-target` uses pod/service subnets `10.245.0.0/16`/`10.97.0.0/16`,
  `patrol-control` kind's defaults (`10.244.0.0/16`/`10.96.0.0/16`), so addresses in logs and
  resource status identify the cluster.

Checks: `up.sh` verifies the path from the `patrol-control` control-plane container;
`test/e2e/fixtures/smoke.sh` verifies it from a pod (`cross-cluster` check: DNS, TLS and an
authenticated `kubectl get nodes`). Status of these checks on this repository's reference
machine: [docs/verification/runs/kind-environment.md](../verification/runs/kind-environment.md).

### Register patrol-target with Patrol CD

Patrol CD reads destination clusters from `Cluster` objects (see
[CRD reference](../reference/crds.md#cluster)). With the CRDs installed in `patrol-control`:

```bash
hack/kind/register-target.sh --namespace <patrol-namespace> [--install-crds]
```

creates, in `patrol-target`, ServiceAccount `kube-system/patrol-manager` bound to
`cluster-admin` and a TokenRequest token for it (default validity 8760h, `--token-duration`),
and, in `patrol-control`, Secret `cluster-patrol-target` (keys `bearerToken`, `caData`) plus:

```yaml
apiVersion: cd.patrol.example/v1alpha1
kind: Cluster
metadata:
  name: patrol-target
spec:
  name: patrol-target
  server: https://patrol-target-control-plane:6443
  config:
    credentialsRef:
      name: cluster-patrol-target
```

(`hack/kind/manifests/cluster-patrol-target.yaml`). `--install-crds` applies `deploy/crds`
server-side only when they are not installed yet; otherwise the existing CRDs are left to their
owner. Applications then target it with `destination.name: patrol-target`.

## Local registry

| From | Address | Notes |
|------|---------|-------|
| host | `localhost:5001` | `docker push`, `helm push --plain-http`, `oras push --plain-http` |
| pod image references | `localhost:5001/<image>` | containerd on every node maps it to the registry (`/etc/containerd/certs.d/localhost:5001/hosts.toml`) |
| pod image references | `patrol-registry:5000/<image>` | also mapped, plain HTTP |
| processes in pods (repo-server, helm, oras) | `patrol-registry:5000` | plain HTTP: `insecureOCIForceHTTP: true` on Repository objects, `--plain-http` for CLIs |

The registry is a container, not a kind port mapping: a host port can be bound once, and
`127.0.0.1:5001` belongs to `patrol-registry`. `patrol-control` maps only `127.0.0.1:8443` to
NodePort `30443`, reserved for the Patrol UI/API Service.

## Images

```bash
hack/kind/load-image.sh                          # every local patrol-cd/*:dev image -> both clusters
hack/kind/load-image.sh --cluster patrol-control patrol-cd/e2e-toolbox:dev
hack/kind/load-image.sh --registry patrol-cd/e2e-toolbox:dev   # -> localhost:5001/patrol-cd/e2e-toolbox:dev
```

Loaded images need `imagePullPolicy: IfNotPresent` or `Never` (the fixtures use `Never`, so a
missing image fails fast instead of being looked up on Docker Hub). When `kind load
docker-image` fails on Docker's containerd image store (multi-platform index with absent
platforms), the script retries with a single-platform `docker save --platform` archive.

## e2e fixtures

```bash
test/e2e/fixtures/deploy.sh        # build + load images, Secrets, git server, seed Job,
                                   # chart repo, toolbox, OCI artifacts (idempotent)
test/e2e/fixtures/smoke.sh         # in-cluster checks; --with-registry-image also pulls
                                   # localhost:5001/patrol-cd/e2e-toolbox:dev in both clusters
```

Endpoints, seeded repositories and refs, and credentials are listed in
[test/e2e/fixtures/README.md](../../test/e2e/fixtures/README.md). In short:

- git over smart HTTP with basic auth (`/git/`, push creates missing repositories), anonymous
  read-only (`/public/`), and SSH (`ssh://git@git-server.patrol-e2e.svc.cluster.local/git/…`) over
  the same repositories; the SSH host key is a committed **TEST-ONLY** key and
  `test/e2e/fixtures/git-server/known_hosts` lists it;
- repositories `guestbook`, `helm-guestbook`, `kustomize-guestbook`, `helm-values` and the
  monorepo `apps`, each with deterministic `v1.0.0`/`release-1.0`, `v1.1.0`/`main` and
  `feature/e2e` refs; re-running the seed Job resets them. When `examples/` exists, it is seeded
  too, as `patrol-examples.git` (plus `patrol-examples-values.git`), so the example Applications
  work after replacing their placeholder `repoURL` with
  `http://git-server.patrol-e2e.svc.cluster.local/git/patrol-examples.git`;
- a Helm repository (`helm-guestbook` 0.1.0 and 0.2.0, anonymous and basic auth);
- the same chart as an OCI artifact and the guestbook manifests as an OCI bundle in
  `patrol-registry`.

Credentials are generated per environment and stay in the cluster. To use them from the host:

```bash
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e get secret git-server-http-auth -o go-template='{{.data.password | base64decode}}'
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e port-forward svc/git-server 8080:80   # http://localhost:8080/git/<repo>.git
```

Debug from inside the cluster with the toolbox pod (`git`, `ssh`, `helm`, `oras`, `kubectl`):

```bash
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e exec -it toolbox -- sh
```

### Register the fixtures with Patrol CD

```bash
test/e2e/fixtures/patrol/register.sh --namespace <patrol-namespace> [--with-applications]
```

copies the fixture credentials into Secrets `e2e-git-http-creds`, `e2e-git-ssh-creds` and
`e2e-chart-repo-creds` in the Patrol namespace and applies `patrol/repositories.yaml`
(credential templates for `/git/` over HTTP and SSH, the Helm repositories and the two OCI
repositories). `--with-applications` also applies `patrol/applications.yaml`: one manually
synced Application per source type (plain directory over HTTP and SSH, Helm from git, Kustomize
overlay, multi-source chart + `$values`, OCI chart, OCI manifests), all with
`destination.name: patrol-target`. For SSH, add the lines of
`test/e2e/fixtures/git-server/known_hosts` to the `ssh_known_hosts` key of
`patrol-ssh-known-hosts-cm`.

### Webhooks

```bash
printf '%s' "$SECRET" > /tmp/webhook-secret   # the GitHub webhook secret configured in Patrol
test/e2e/fixtures/webhook/send-github-push.sh --secret-file /tmp/webhook-secret --insecure \
  --repo http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git \
  --ref refs/heads/main --changed guestbook-ui-deployment.yaml
```

The script signs the exact body it sends (`X-Hub-Signature-256: sha256=<HMAC-SHA256>`), posts
to `https://localhost:8443/api/webhook` by default (`--url`), and supports `--dry-run`,
`--payload FILE --event NAME` for other events and `--self-test` (GitHub's documented example).

### Fake GitHub API

Not available yet; see [test/e2e/fixtures/fake-github/README.md](../../test/e2e/fixtures/fake-github/README.md).

## Offline checks

```bash
test/e2e/fixtures/validate.sh
test/e2e/fixtures/test-scripts.sh
```

starts the envtest `etcd` and `kube-apiserver` (Kubernetes 1.36, `bin/envtest`, `make envtest`),
installs `deploy/crds`, and dry-run creates every fixture manifest, the Patrol objects in
`test/e2e/fixtures/patrol/` and `hack/kind/manifests/`, and the rendered seed applications
(`kustomize build`, `helm lint --strict` + `helm template` with each values file).
`test-scripts.sh` runs two such API servers with kubeconfig contexts named
`kind-patrol-control`/`kind-patrol-target` and exercises the context guards,
`hack/kind/register-target.sh` and `test/e2e/fixtures/patrol/register.sh` against them. Neither
needs Docker or network access.

## Tear down

```bash
hack/kind/down.sh        # or: make -f Makefile -f hack/kind/kind.mk kind-down
```

deletes every kind cluster named `patrol-*`, the `patrol-registry` container (only if it carries
the hack/kind label) with its storage volume, and the kubeconfigs in `.cache/kind/`, then
verifies that none remain. The `kind` network, other kind clusters and images are kept;
`docker image rm` the `patrol-cd/*:dev` images yourself if you want the space back.

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `no space left on device` from `docker run`, `kind create cluster` or `docker build` | The Docker VM disk is full. Inspect with `docker system df`; free space (unused images, build cache, volumes) or enlarge the Docker Desktop disk. The scripts never prune anything. |
| `apk add` fails during `build-images.sh` / `deploy.sh` (e.g. `temporary error`, `unable to select packages`) | The fixture build needs the Alpine package mirrors on its first run or after the build cache was pruned. Build once with network access, or reuse built images with `deploy.sh --skip-build`. |
| `refusing to operate on kube context ...` | A script was pointed at a non-`kind-patrol-*` context or a kubeconfig whose server is not local. Use the files in `.cache/kind/`. |
| `a container named patrol-registry exists but was not created by hack/kind` | Remove or rename that container; `up.sh` and `down.sh` do not touch unlabelled containers. |
| `ErrImageNeverPull` on fixture pods | Images were not loaded into `patrol-control`: run `test/e2e/fixtures/deploy.sh` (or `hack/kind/load-image.sh --cluster patrol-control ...`). |
| `cross-cluster` smoke check fails on name resolution | Check `getent hosts patrol-target-control-plane` in the toolbox pod and `docker network inspect kind`; both node containers must be on the `kind` network. |
| Port 8443 or 5001 already in use | Stop the other listener; the ports are fixed by the cluster config and the registry. |
| Seeded repositories gone after the git server pod restarted | Repositories live in an `emptyDir`: re-run `test/e2e/fixtures/deploy.sh --skip-build`. |
