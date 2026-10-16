# kind environment and e2e fixtures: verification run

Date: 2026-10-16 (UTC 15:19–15:43; review follow-up 15:57–16:06). Workstream: kind environment
(`hack/kind/`, `test/e2e/fixtures/`, `docs/guides/local-development.md`).

## Result

**The live kind validation is blocked.** The Docker Desktop VM's disk is full, so Docker cannot
create any container or build any image (`no space left on device`). No kind cluster, registry
container or fixture image could be created. Freeing that space means deleting images, build
cache or volumes that other projects on this machine use, or enlarging the Docker Desktop disk;
both are outside this workstream's authorization and were not done.

Everything that does not need Docker was validated. Where envtest API servers or a local CGI
shim stand in for the kind clusters or for nginx, the result is labelled `simulated`. Checks
that only inspect registry metadata, package indexes or upstream files, without running a
container, are labelled `static`.

Re-checked in the review follow-up at 2026-10-16T16:02Z: Docker's disk is still full and
`hack/kind/up.sh` and all three `docker build --check` runs fail the same way (see
[Review follow-up](#review-follow-up)). K-01 to K-05 and K-07 stay
blocked. Container-level behaviour (nginx + fcgiwrap + `git-http-backend`, sshd + `git-shell`,
the kind configs, containerd `hosts.toml`, cross-cluster networking, image builds) has not run.

| # | Check | Status |
|---|-------|--------|
| K-01 | `hack/kind/up.sh`: both clusters, registry, nodes Ready | blocked (Docker disk full; re-tried 16:02Z) |
| K-02 | Cross-cluster: pod in patrol-control → `https://patrol-target-control-plane:6443` | blocked |
| K-03 | git-server fixture + seed Job deployed; clone over HTTP basic auth from a pod | blocked |
| K-04 | Chart repo deployed; `helm repo add` / `helm pull` from a pod | blocked |
| K-05 | OCI chart push (host) and pull (pod) through the local registry | blocked |
| K-06 | `hack/kind/down.sh`; no patrol-* clusters and no registry container remain | passed (nothing had been created; re-run 16:03Z) |
| K-07 | Dockerfile checks (`docker build --check`) | blocked (re-tried 16:03Z) |
| K-08 | All manifests and Patrol objects vs envtest kube-apiserver 1.36 + `deploy/crds` (`validate.sh`) | passed, 49 objects |
| K-09 | Chart build step (`helm lint --strict`, `helm package`, `helm repo index`) | passed |
| K-10 | Guards, `register-target.sh`, `patrol/register.sh`, `env.sh` vs two envtest API servers (`test-scripts.sh`) | passed (simulated), 34/34 |
| K-11 | `git-http-backend-wrapper` + `git-seed` + access rules via git-http-backend behind a CGI shim | passed (simulated), 30/30 |
| K-12 | Webhook sender vs Patrol's `pkg/scm/github.WebhookVerifier` | passed |
| K-13 | `sshd -t` on the fixture's sshd config; host key Secret matches `known_hosts` | passed |
| K-14 | kind config keys vs kind v0.33.0 `v1alpha4` JSON tags | passed (static) |
| K-15 | `shellcheck` (bash + POSIX sh), `bash -n` with bash 3.2, `sh -n` | passed |
| K-16 | `kind.mk` targets via `make -f Makefile -f hack/kind/kind.mk` | passed (`help`, `-n` all targets, real `kind-status`) |
| K-17 | Pinned image references resolve in their registries (kind node digest also vs kind v0.33.0 release notes) | passed (static) |
| K-18 | Alpine v3.24 package names in all three Dockerfiles exist (aarch64, x86_64); file paths used by the Dockerfiles and entrypoints are in those packages; users `sshd` and privsep dir `/var/empty` exist in alpine 3.24.2 | passed (static) |
| K-19 | `ghcr.io/oras-project/oras:v1.3.4` (linux/arm64) contains `/bin/oras` | passed (static) |
| K-20 | containerd patch and `hosts.toml` layout match kind v0.33.0's local-registry example; node containerd config is `version = 2` | passed (static) |

## Review follow-up

2026-10-16, 15:57–16:06 UTC.

### Blocker re-check

```
$ docker system df                                           # 15:57Z, unchanged
Images          99        51        48.6GB    21.88GB (45%)
Containers      79        28        749.4MB   520.3MB (69%)
Local Volumes   107       60        21.48GB   4.011GB (18%)
Build Cache     614       0         30.02GB   18.7GB
$ docker run --rm alpine:3.24.2 true
docker: Error response from daemon: failed to create prepare snapshot dir: … no space left on device
$ /bin/bash hack/kind/up.sh                                  # 16:02:54Z, exit 125
[patrol-kind] creating registry container patrol-registry (registry:3.0.0) on 127.0.0.1:5001
Unable to find image 'registry:3.0.0' locally
docker: Error response from daemon: mkdir /var/lib/desktop-containerd/daemon/io.containerd.content.v1.content/ingest/69cbdafc…: no space left on device
$ docker build --check … git-server | chart-repo | toolbox  # 16:03:01Z, exit 1 each
ERROR: failed to build: failed to solve: failed to read dockerfile: … failed to create prepare snapshot dir: … no space left on device
$ /bin/bash hack/kind/down.sh                                # exit 0
[patrol-kind] no kind clusters named patrol-*
[patrol-kind] no patrol-registry container
$ kind get clusters
No kind clusters found.
$ docker ps -a --filter name=patrol-registry --format '{{.Names}}' | wc -l
0
```

Freeing Docker's disk (pruning images, build cache or volumes that other projects use) or
enlarging it is outside this workstream's authority and was not done. To unblock: free or
enlarge the Docker Desktop disk, then run the sequence in
[To run once Docker has space](#to-run-once-docker-has-space).

### K-17 image references (static)

Resolved anonymously against the registries' HTTP API (`curl` with a pull token, `HEAD`/`GET`
of the manifest; no Docker storage involved):

| Reference | Result |
|-----------|--------|
| `kindest/node:v1.36.4` | `Docker-Content-Digest: sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed`; `shasum -a 256` of the index body is the same; platforms linux/amd64, linux/arm64. kind v0.33.0 release notes (GitHub API) list exactly `kindest/node:v1.36.4@sha256:099e0493…cbfaed`; the release's default is v1.37.0, which is why the kind binary only embeds that one. |
| `alpine:3.24.2` | `sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6`, the digest pinned in the three Dockerfiles; includes linux/arm64 |
| `registry:3.0.0` | `sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e`; includes linux/arm64 |
| `ghcr.io/oras-project/oras:v1.3.4` | index `sha256:f7bc056d54d97baa399414ed5048ecc67c3371b750d4bbce1d871827a5758179`; linux/arm64 manifest `sha256:594d7309fe35…` |

### K-18 Alpine packages (static)

`APKINDEX.tar.gz` of `dl-cdn.alpinelinux.org/alpine/v3.24/{main,community}/{aarch64,x86_64}`:
every name in the three `apk add` lines resolves on both architectures. Versions (aarch64):
apache2-utils 2.4.69-r0, fcgiwrap 1.1.0-r9, git and git-daemon 2.54.0-r0, nginx 1.30.4-r1,
openssh-server 10.3_p1-r1, su-exec 0.3-r0, tini 0.19.0-r3 (community), bash 5.3.9-r1,
ca-certificates 20260909-r0, curl 8.22.0-r0, helm 3.19.0-r9 (community), jq 1.8.2-r0,
kubectl 1.36.1-r2 (community), openssl 3.5.9-r0, tar 1.35-r5. `openssh-client` is a virtual
name provided by `openssh-client-default` (provider priority 2) and `openssh-client-krb5` (1),
so apk selects `openssh-client-default`.

File lists of the downloaded aarch64 `.apk` archives (`tar -tzf`, nothing installed or run):

| Package | Paths used by the fixtures |
|---------|----------------------------|
| git-daemon | `/usr/libexec/git-core/git-http-backend`, `/usr/libexec/git-core/git-shell` (the Dockerfile's `test -x` checks) |
| fcgiwrap | `/usr/bin/fcgiwrap` |
| tini / su-exec | `/sbin/tini` (ENTRYPOINT), `/sbin/su-exec` |
| openssh-server | `/usr/sbin/sshd`, `/usr/lib/ssh/sshd-session`, `/usr/lib/ssh/sshd-auth` |
| nginx | `/usr/sbin/nginx`, `/etc/nginx/mime.types`, `/etc/nginx/fastcgi_params` (both included by the fixture configs); `.pre-install` creates user `nginx` (chart repo `user nginx nginx`) |
| apache2-utils | `/usr/bin/htpasswd` |

`alpine-minirootfs-3.24.2-aarch64.tar.gz` (same release as the image): `/etc/passwd` has
`sshd:x:22:22` (OpenSSH's privilege separation user) and no uid/gid 1000, `/var/empty` exists.
nginx 1.30.4-r1 ships no `/etc/nginx/http.d/default.conf`; the Dockerfiles' `rm -f` of it is a
no-op and the fixture `nginx.conf` files do not include `http.d`.

Not shown by K-18: that the packages install cleanly together, and anything at runtime.

### K-19 oras binary (static)

Config of the linux/arm64 image: `Entrypoint ["/bin/oras"]`, history `COPY /go/bin/oras
/bin/oras`. Layer `sha256:32f0dcca…` (digest re-computed after download) lists exactly `bin/`
and `bin/oras` (12795233 bytes, mode 0755), so `COPY --from=oras /bin/oras` in
`toolbox/Dockerfile` has a source.

### K-20 kind configuration (static)

- `images/base/files/etc/containerd/config.toml` at kind v0.33.0 is `version = 2` and sets
  `sandbox_image = "registry.k8s.io/pause:3.10"`, so the `plugins."io.containerd.grpc.v1.cri"`
  patch key is the right one for that format and the seeded workloads' pause image is the
  node's sandbox image.
- `site/static/examples/kind-with-registry.sh` at v0.33.0 uses the same
  `containerdConfigPatches` entry and writes `/etc/containerd/certs.d/localhost:<port>/hosts.toml`
  with `[host."http://<registry>:5000"]`, the layout `up.sh` writes.
- kind v0.27.0 release notes: containerd 2.x accepts registry configuration only in
  `config_path` mode. The comment in `hack/kind/patrol-control.yaml` that claimed node images
  default to that directory was not supported by any source and was replaced with this.
- Re-run of K-14 after the comment change: every key of both configs (`yq -o=json` paths) is a
  JSON tag in kind v0.33.0 `pkg/apis/config/v1alpha4/types.go`.

### Reruns after the follow-up edits

```
$ /bin/bash test/e2e/fixtures/validate.sh                    # 16:04:12Z, exit 0
validated 49 objects against kube-apiserver https://127.0.0.1:27537 with deploy/crds installed
$ PATH="/bin:$PATH" /bin/bash test/e2e/fixtures/test-scripts.sh   # 16:04:28Z, exit 0
test-scripts: 34 passed, 0 failed
$ shellcheck -x -P SCRIPTDIR hack/kind/*.sh test/e2e/fixtures/*.sh test/e2e/fixtures/*/*.sh   # exit 0
$ shellcheck -s sh test/e2e/fixtures/{git-server,chart-repo}/rootfs/usr/local/bin/*           # exit 0
```

## Environment

| Item | Value |
|------|-------|
| Host | macOS 27.0.1 (26A434), arm64 |
| Docker | Docker Desktop, client/server 29.8.2, 8 CPUs, 11.67 GiB (12526632960 bytes), containerd snapshotter |
| kind / kubectl / helm | v0.33.0 / v1.36.1 / v4.3.0 |
| envtest | kube-apiserver + etcd v1.36.0 (`bin/envtest/k8s/1.36.0-darwin-arm64`) |
| kustomize / git / shellcheck / OpenSSH | v5.8.1 (`bin/kustomize`) / 2.56.0 / 0.11.0 / 10.6p1 |
| bash | `/bin/bash` 3.2.57 (all bash scripts ran under it) |

`docker system df` at the time of the run:

```
TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE
Images          99        51        48.6GB    21.88GB (45%)
Containers      79        28        749.4MB   520.3MB (69%)
Local Volumes   107       60        21.48GB   4.011GB (18%)
Build Cache     616       0         30.02GB   18.7GB
```

`~/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw`: 96 GB allocated.

## Blocked: live kind environment

```
$ docker run --rm alpine:3.24.2 df -h /
docker: Error response from daemon: failed to create prepare snapshot dir: failed to create temp
dir: mkdir /var/lib/desktop-containerd/daemon/io.containerd.snapshotter.v1.overlayfs/snapshots/new-1001217509:
no space left on device

$ hack/kind/up.sh                                            # 2026-10-16T15:25:26Z, exit 125
[patrol-kind] creating registry container patrol-registry (registry:3.0.0) on 127.0.0.1:5001
Unable to find image 'registry:3.0.0' locally
docker: Error response from daemon: mkdir /var/lib/desktop-containerd/daemon/io.containerd.content.v1.content/ingest/69cbdafc…:
no space left on device

$ PATROL_REGISTRY_IMAGE=registry:2 PATROL_KIND_NODE_IMAGE=kindest/node:v1.35.0 hack/kind/up.sh
# 2026-10-16T15:25:54Z, both images already present locally; exit 125
docker: Error response from daemon: failed to create prepare snapshot dir: … no space left on device

$ kind create cluster --name patrol-control --config hack/kind/patrol-control.yaml \
    --image kindest/node:v1.35.0 --kubeconfig .cache/kind/patrol-control.kubeconfig --wait 60s
ERROR: failed to create cluster: command "docker run --name patrol-control-worker … kindest/node:v1.35.0"
failed with error: exit status 125
Command Output: docker: Error response from daemon: failed to create prepare snapshot dir: … no space left on device
(exit 1; kind removed the partial cluster: "No kind clusters found.")

$ docker build --check --build-context seed=seed/repos --build-context seed-updates=seed/updates git-server
ERROR: failed to build: failed to solve: failed to read dockerfile: … no space left on device
(same for chart-repo and toolbox; exit 1)

$ docker run --rm alpine:3.24.2 true                         # 2026-10-16T15:38:41Z, still failing
docker: Error response from daemon: failed to create prepare snapshot dir: … no space left on device
```

The second `up.sh` attempt and the `kind create cluster` call used images already in the local
store (`registry:2`, `kindest/node:v1.35.0`) to rule out network pulls as the cause. The pinned
node image `kindest/node:v1.36.4@sha256:099e…` is not in the local store and was never pulled.

Cleanup (K-06):

```
$ hack/kind/down.sh                                          # exit 0
[patrol-kind] no kind clusters named patrol-*
[patrol-kind] no patrol-registry container
[patrol-kind] done: no patrol-* kind clusters and no hack/kind registry container remain
$ kind get clusters
No kind clusters found.
$ docker ps -a --filter name=patrol -q | wc -l
0
```

### To run once Docker has space

```bash
hack/kind/up.sh
kubectl --kubeconfig .cache/kind/patrol-control.kubeconfig --context kind-patrol-control get nodes
kubectl --kubeconfig .cache/kind/patrol-target.kubeconfig --context kind-patrol-target get nodes
test/e2e/fixtures/deploy.sh                       # build, load, deploy, seed Job, chart repo, OCI push
test/e2e/fixtures/smoke.sh --with-registry-image  # K-02..K-05 from a pod, registry pulls in both clusters
hack/kind/down.sh
kind get clusters; docker ps -a --filter name=patrol-registry
```

`smoke.sh` covers K-02 (DNS, TLS and `kubectl get nodes` against patrol-target from the toolbox
pod), K-03 (HTTP basic-auth clone, ls-remote, push; anonymous refused; anonymous clone; SSH
clone with strict host key checking), K-04 (`helm repo add`/`helm pull`, anonymous and
`/private` with basic auth, 401 without) and K-05 (`helm pull oci://patrol-registry:5000/…`,
`oras pull` of the manifest bundle; `--with-registry-image` also runs
`localhost:5001/patrol-cd/e2e-toolbox:dev` in both clusters through containerd's `hosts.toml`).
None of these has been executed yet.

## Passed offline checks

### K-08 manifests vs envtest + CRDs

```
$ /bin/bash test/e2e/fixtures/validate.sh                    # 2026-10-16T15:37Z, exit 0, ~12 s
starting envtest kube-apiserver from …/bin/envtest/k8s/1.36.0-darwin-arm64
server version: v1.36.0
installing Patrol CRDs (server-side apply of deploy/crds)
ok    test/e2e/fixtures/namespace.yaml   namespace/patrol-e2e (created)
ok    test/e2e/fixtures/git-server/manifests/deployment.yaml   deployment.apps/git-server
…
ok    test/e2e/fixtures/patrol/applications.yaml   application.cd.patrol.example/e2e-multi-source
ok    hack/kind/manifests/cluster-patrol-target.yaml   cluster.cd.patrol.example/patrol-target
…
ok    helm template helm-guestbook-0.2.0.tgz   deployment.apps/e2e-helm-guestbook
validated 49 objects against kube-apiserver https://127.0.0.1:33956 with deploy/crds installed
```

49 objects: 9 fixture objects (git server Deployment, Service, TEST-ONLY host key Secret, seed
Job, chart repo Deployment and Service, toolbox Pod, target ServiceAccount and
ClusterRoleBinding), the `patrol-e2e` Namespace (created), 15 Patrol objects (2
RepositoryCredentials, 5 Repositories, 7 Applications, 1 Cluster), and 24 rendered seed
objects (guestbook plain and with its update overlay, kustomize `staging`/`production`/updated
`staging`, helm-guestbook default/updated/each values file/both packaged charts). Each is a
`kubectl create --dry-run=server --validate=strict`. The inline KEP-1755 ConfigMap of `up.sh` was
dry-run created the same way (extracted verbatim, exit 0). A deliberately invalid Application
(both `source` and `sources`) was rejected by the CRD's CEL rule in K-10, confirming the CRD
validation is active in this setup.

The task description asked for a Go test; this workstream may not add Go code, so the same
envtest binaries are driven from bash (`test/e2e/fixtures/lib/envtest.sh`).

### K-09 chart build step

```
$ test/e2e/fixtures/chart-repo/build.sh --package-only       # exit 0
packaged charts in …/.cache/kind/build/charts:
helm-guestbook-0.1.0.tgz
helm-guestbook-0.2.0.tgz
index.yaml
```

`index.yaml` lists both versions with relative URLs; 0.1.0 has `replicaCount: 1`, 0.2.0
(appVersion 1.1.0) `replicaCount: 2`.

### K-10 script behaviour against envtest (simulated)

```
$ PATH="/bin:$PATH" /bin/bash test/e2e/fixtures/test-scripts.sh   # 2026-10-16T15:42:01Z, exit 0
test-scripts: 34 passed, 0 failed
```

Covers: guard accepts `kind-patrol-control`/`-target` and refuses `kind-eks-worker-1` (a stale
non-patrol context present in this machine's `~/.kube/config`, which was never read),
`docker-desktop`, empty and malformed names; `patrol::kubectl` refuses non-patrol clusters; a
kubeconfig whose `kind-patrol-control` context points at `https://203.0.113.10:6443` is refused;
`register-target.sh` fails without CRDs, installs them with `--install-crds`, is idempotent,
writes Secret `cluster-patrol-target` with exactly `bearerToken`,`caData` and no last-applied
annotation, `caData` equals the target's serving CA, and the stored token authenticates as
`system:serviceaccount:kube-system:patrol-manager` with TLS verified and is cluster-admin;
`patrol/register.sh` copies the credentials byte-for-byte and applies all Repository,
RepositoryCredential and Application objects (idempotent); `env.sh` output evaluates.

### K-11 git server logic (simulated)

A scratch Python HTTP server (session scratchpad, not committed) routed `/git/` (basic auth,
`REMOTE_USER`, `PATROL_ALLOW_CREATE=1`) and `/public/` (anonymous, receive-pack refused) like the
fixture's nginx config and ran the real `git-http-backend-wrapper` as CGI in front of
Homebrew's `git-http-backend` (git 2.56.0); `git-seed` ran under `/bin/sh`. 30/30 checks passed:

- first seeding creates every repository by push (push to create), each with `HEAD`, `main`,
  `release-1.0`, `feature/e2e`, annotated `v1.0.0`, lightweight `v1.1.0`; overlays applied
  (guestbook replicas 1 → 2, kustomize staging 1 → 2, helm-values dev 1 → 2); `apps.git` holds
  all four; with `examples/` present, `patrol-examples.git` (examples tree at its root) and
  `patrol-examples-values.git` are seeded and are not part of `apps.git`;
- anonymous access to `/git/` and a wrong password are refused; anonymous push to `/public/`
  is refused and does not create repositories; an invalid (two-level) repository name is not
  created;
- after a test pushes a drift commit to `main` and an extra branch, re-seeding restores every ref
  to the original SHA and prunes the extra branch; seeding an empty server again yields identical
  SHAs. Observed: `guestbook.git` `main` = `v1.1.0` = `dcd7e5bc6e93`, `v1.0.0` tag object
  `35688930db20` (commit `69253fafba47` = `release-1.0`), `feature/e2e` `2e7ed2d83950`.

Not covered by K-11: nginx itself, fcgiwrap, the entrypoints and sshd in the Alpine image
(container-only; part of K-03 once Docker works).

### K-12 webhook sender

A scratch Go program (session scratchpad, module `replace` onto this repository, offline module
cache; the repository's `go.mod`/`go.sum` were not modified) served
`github.WebhookVerifier{}.VerifyAndParse` on `127.0.0.1`. Results:

```
$ test/e2e/fixtures/webhook/send-github-push.sh --self-test
self-test passed: sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17
$ send-github-push.sh --url http://127.0.0.1:<port>/api/webhook --secret-file secret --changed guestbook-ui-deployment.yaml
{"Verified":true,"Ref":"refs/heads/main","Revision":"main","CloneURL":"http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git","ChangedFiles":["guestbook-ui-deployment.yaml"]}
$ … --secret-file wrong      -> rejected: … webhook signature mismatch (HTTP 401)
$ … (no secret)              -> rejected: … webhook signature missing (HTTP 401)
$ … --event ping --payload ping.json -> accepted (Type ping)
```

Also verified: ssh:// and scp-style `--repo` fill `ssh_url`; `--ref refs/heads/release-1.0`
gives `TouchedDefaultBranch: false` with default branch `main`; file names with quotes,
backslashes and non-ASCII survive JSON encoding; control characters are rejected.

### K-13 to K-16

```
$ /opt/homebrew/sbin/sshd -t -f test/e2e/fixtures/git-server/rootfs/etc/ssh/sshd_config.patrol -h <TEST-ONLY host key>
exit 0 (effective: Port 2222, AllowUsers git, AuthenticationMethods publickey, PasswordAuthentication no,
PermitTTY no, AllowTcpForwarding no, AuthorizedKeysFile /etc/ssh/authorized_keys/%u)
$ ssh-keygen -y on the Secret's key  -> matches all 4 lines of git-server/known_hosts
  (SHA256:4T+nNq7krW/I9lmmvDF6d6Lnwfv1txC7wYZhDO7h6Ns)
kind configs: keys {kind apiVersion name networking nodes containerdConfigPatches apiServerAddress
  role extraPortMappings containerPort hostPort listenAddress protocol podSubnet serviceSubnet}
  are all v1alpha4 JSON tags of kind v0.33.0 (pkg/apis/config/v1alpha4/types.go)
$ shellcheck -x -P SCRIPTDIR <bash scripts>; shellcheck -s sh <in-image scripts>   # exit 0, exit 0
$ /bin/bash -n <each bash script>; /bin/sh -n <each sh script>                  # no errors
$ make -f Makefile -f hack/kind/kind.mk help | grep kind-     # 5 targets listed
$ make -f Makefile -f hack/kind/kind.mk kind-status           # exit 0, "patrol-control: not created" …
```

`htpasswd -i -m -c` (as used by both entrypoints) was checked with Apache 2.4.67's `htpasswd`: the
APR1 hash verifies against the password read from stdin without its trailing newline.

## Defects found and fixed during this run

| Defect | Fix |
|--------|-----|
| `register-target.sh --install-crds` failed with a server-side-apply conflict when the CRDs had been applied by another field manager | installs only when the CRDs are missing; otherwise leaves them to their owner |
| `kubectl wait --for=condition=Established` failed immediately on a just-created CRD with no conditions (`.status.conditions accessor error`) | retry loop (`patrol::wait_established`, and in `validate.sh`) |
| envtest teardown hung: the API server did not exit after etcd was killed at the same time | stop in reverse order with a bounded wait and SIGKILL fallback |
| `git-seed` on a workstation let git's system credential helper (macOS keychain) try to store the throwaway seed credentials (`failed to store: -60008`, nothing stored) | `GIT_CONFIG_NOSYSTEM=1`, `-c credential.helper=` |
| `git-seed` silently skipped invalid extra repository names (die inside a command substitution) | status propagated |
| `load-image.sh` checked images before rejecting an invalid `--cluster` | cluster validated first |
| Webhook sender put ssh URLs into `html_url` and used the pushed branch as `default_branch` | `ssh_url` for ssh URLs; `--default-branch` (default `main`) |
| (review) `test/e2e/fixtures/README.md` and `git-server/Dockerfile` claimed the fixtures work offline once the base images are present, but every fixture Dockerfile runs `apk add` | Claim corrected in the README, the three Dockerfiles and the guide: the first build (or a rebuild after the build cache is pruned) needs network; offline afterwards via `deploy.sh --skip-build` or the cached `apk add` layer. The README lists the four base images with `docker pull` commands (references checked in K-17). |
| (review) `hack/kind/patrol-control.yaml` comment claimed kind node images default to `/etc/containerd/certs.d` | Replaced with the sourced statement (K-20) |

## Deviations from the task description

- **Registry port 5001 is not a kind `extraPortMappings` entry.** The registry is the
  `patrol-registry` container publishing `127.0.0.1:5001`; a host port can be bound only once.
  `patrol-control` maps only `127.0.0.1:8443` → NodePort `30443`.
- **Seed source.** The fixture repositories (`test/e2e/fixtures/seed/repos`) are always seeded,
  because `smoke.sh` and `patrol/applications.yaml` depend on them. `examples/` (created by
  another workstream during this run) is seeded in addition, as `patrol-examples.git` with the
  examples tree at its root, since its Applications use paths like `apps/plain-yaml/manifests`;
  splitting `examples/apps` into one repository per directory would not match those paths.
- **Validation driver.** envtest binaries are driven from bash instead of a Go test (no Go in
  this workstream's paths).
