# Container image (`patrol-cd`)

Patrol CD ships one multi-call image for every control-plane component, the CLI and the tools
the repo-server runs (ADR-0007). It is built from the repository's `Dockerfile`; the pins and
helper scripts live in `hack/images/`, the make targets in `hack/image.mk`.

Status: built and checked locally for `linux/arm64` on 2026-10-19 (`make image`, then
`make image-layout-check image-smoke`; see [Verification](#verification)). Not yet done:
`linux/amd64` and multi-arch builds, the SBOM, vulnerability and license scans, a push, and a run
on a cluster (kind validation).

## Contents

| Path | What | Version / source |
|------|------|------------------|
| `/usr/local/bin/patrolcd` | Control-plane multi-call binary: `server`, `controller`, `repo-server`, `appset-controller`, `notifications-controller`, `cmp-server`, `commit-server`, `k8s-auth`, `bootstrap`, `version`; also the hidden helper modes (sandbox rlimits, git askpass, Jsonnet) | this repository; Go 1.27.2, `CGO_ENABLED=0`, `-trimpath`, static; web UI embedded |
| `/usr/local/bin/patrol-*` | Relative links to `patrolcd`, one per component name (below) | `hack/images/aliases.txt` |
| `/usr/local/bin/patrol` | CLI (also what patrol-server serves at `/download/patrol-linux[-<arch>]`, OPS-112) | this repository |
| `/usr/local/bin/helm` | Helm | 4.2.1 (baseline), get.helm.sh, SHA-256 pinned |
| `/usr/local/bin/kustomize` | Kustomize | 5.8.1 (baseline), GitHub release, SHA-256 pinned |
| `/usr/local/bin/git-lfs` | Git LFS | 3.7.1 (baseline), GitHub release, SHA-256 pinned |
| `/usr/local/bin/connect-proxy` | SOCKS5 `ProxyCommand` for SSH repositories with a proxy (internal/git) | ssh-connect 1.106 (commit `a7443d32e6bf`), compiled static in the image build |
| `/usr/bin/git`, `ssh`, `gpg` | Alpine packages `git`, `openssh-client`, `gpg` | Alpine 3.24 branch (versions float, see [Reproducibility](#tags-versions-and-reproducibility)) |
| `/sbin/tini` (+ `/usr/bin/tini` link) | PID 1 | Alpine package `tini` |
| `ca-certificates`, `tzdata` | Public CA bundle, time zones | Alpine packages |
| `/etc/gitconfig`, `/etc/ssh/ssh_config` | Hardened system git and SSH client defaults | `hack/images/gitconfig`, `hack/images/ssh_config` |
| `/usr/share/licenses/<tool>/` | License texts of Patrol CD (`LICENSE`, `NOTICE`), Helm, Kustomize and git-lfs; for connect-proxy (GPL-2.0-or-later) its source `connect.c`, the GNU GPL v2 text (`COPYING`) and the compiler command (`BUILD`) | `fetch-tools.sh` (SHA-256 pinned) and `hack/images/licenses/` (vendored); see [Licenses](#licenses) |
| `/usr/share/patrol-cd/tools.lock` | Name, version, platform, SHA-256 and URL of every bundled third-party file | written by `fetch-tools.sh` |

Component names linked to `patrolcd` (the binary picks its subcommand from the name it was
started as, or from `PATROL_BINARY_NAME`; wiring.md "Binary"): `patrol-server`,
`patrol-controller`, `patrol-application-controller`, `patrol-repo-server`,
`patrol-applicationset-controller`, `patrol-appset-controller`, `patrol-notifications`,
`patrol-notifications-controller`, `patrol-cmp-server`, `patrol-commit-server`,
`patrol-k8s-auth`. `hack/images/rootfs-check.sh` fails when this list and `binaryAliases` in
`internal/server/commands.go` differ.

Not in the image: Redis (ADR-0004), Dex (the chart uses the upstream `dexidp/dex` image), kubectl,
oras, CLI builds for other operating systems.

### Base image choice

The runtime is `alpine:3.24.2`, pinned by its multi-arch index digest. The components need a
real userland at run time: `git` (with `git-remote-https`), `ssh`, `gpg`, a POSIX shell (git runs
`GIT_SSH_COMMAND` through it; the Kustomize wrappers in internal/render and the connect-proxy
wrapper are `#!/bin/sh` scripts), CA certificates, time zones and an init process.

- **distroless** (static or base): no shell, no git/ssh/gpg packages; they would have to be
  copied in with their shared libraries by hand, which is fragile and hard to patch.
- **debian:trixie-slim**: has every package (including `connect-proxy`), glibc like upstream's
  Ubuntu image, but is several times larger and slower to patch through a full apt layer.
- **alpine:3.24**: smallest image with maintained `git`, `openssh`, `gnupg`, `tini` packages for
  amd64 and arm64, used already by the e2e fixtures. Costs: musl instead of glibc (irrelevant for
  the static Go binaries; git, ssh and gpg are Alpine builds), and `connect-proxy` is not packaged,
  so the image compiles its single C source from a pinned commit.

### Licenses

What the image carries for the third-party software it bundles (NOTICE has the same list):

| Software | License | In the image |
|----------|---------|--------------|
| Patrol CD (`patrolcd`, `patrol`) | Apache-2.0 | `/usr/share/licenses/patrol-cd/LICENSE`, `NOTICE` |
| Helm 4.2.1, Kustomize 5.8.1 | Apache-2.0 | `/usr/share/licenses/{helm,kustomize}/LICENSE` |
| git-lfs 3.7.1 | MIT | `/usr/share/licenses/git-lfs/LICENSE.md` |
| connect-proxy 1.106 | GPL-2.0-or-later | `/usr/share/licenses/connect-proxy/`: `connect.c` (the complete source), `COPYING` (GNU GPL v2 text), `BUILD` (the exact `gcc` command of the `connect` stage) |
| Alpine packages (base image, `apk-packages.txt`, their dependencies) | as each package declares | no license texts are copied; apk's package database (`/lib/apk/db/installed`, kept in the image) records each package and its declared license, and `make image-sbom` lists the packages |

The GPL text and the BUILD note are vendored in `hack/images/licenses/` and copied by the
Dockerfile, so they need no download and no `checksums.txt` line. `hack/images/rootfs-check.sh`
fails when the `gcc` command in `connect-proxy-BUILD.txt` differs from the one in the Dockerfile's
`connect` stage, when `COPYING` is not the GPL v2 text, or when the Dockerfile stops copying either
file. `hack/images/licenses/GPL-2.0.txt` is the FSF's text of the GNU General Public License,
version 2 (June 1991), copied verbatim on 2026-10-18 from a local file, Homebrew's libtool 2.6.2
`COPYING` (byte-identical copies ship with Homebrew's xz 5.8.4, libgcrypt 1.12.4, whois 5.6.6 and
wireshark 4.6.9): 17,984 bytes, SHA-256
`edaef632cbb643e4e7a221717a6c441a4c1a7c918e6e4d56debc3d8739b233f6`. It is the FSF revision that
gives the FSF's web address; compared with the older `51 Franklin Street` revision (SHA-256
`8177f975…`) only the address lines and the name in the sample disclaimer differ.

The OCI label `org.opencontainers.image.licenses` is the SPDX expression
`Apache-2.0 AND MIT AND GPL-2.0-or-later AND LicenseRef-Bundled-Dependencies`. The first three
identifiers cover the software the Dockerfile adds itself (table above).
`LicenseRef-Bundled-Dependencies`, an SPDX user-defined reference, stands for the licenses the
label does not enumerate because they change with every dependency or Alpine update: the Alpine
packages (among them git, under GPL version 2 only, and GnuPG, under GPL version 3) and the Go
standard library, Go modules and npm packages compiled into the binaries. Their inventories are
the SBOM (Alpine packages, Go modules), the package database, `go version -m <binary>` and
`ui/pnpm-lock.yaml`.

## Build

```bash
make image                                    # patrol-cd/patrol-cd:dev for the daemon's platform
make image TAG=test PLATFORM=linux/arm64      # another platform (see "Multi-arch")
make image VERSION=v0.1.0                     # override the version stamped into both binaries
make image-check                              # BuildKit's Dockerfile checks (docker buildx build --check)
```

| Variable | Default | Meaning |
|----------|---------|---------|
| `IMAGE`, `TAG` | `patrol-cd/patrol-cd`, `dev` | Image reference |
| `PLATFORM` | daemon's | Single-platform target, e.g. `linux/arm64` |
| `VERSION` | `git describe --tags --always --dirty`, plus `-dirty` when the build context has untracked files (below) | `patrolcd version` / `patrol version` and the OCI version label |
| `COMMIT`, `GIT_TAG`, `GIT_TREE_STATE` | from git; `GIT_TREE_STATE` is `dirty` for tracked changes and for untracked files in the build context | Build information |
| `SOURCE_DATE_EPOCH`, `DATE` | commit time | Image timestamps and `org.opencontainers.image.created` |
| `IMAGE_BUILD_FLAGS` | | Extra `docker build` flags (`--no-cache`, `--builder`, `--build-arg GOPROXY=...`) |

The build context is the `.dockerignore` allow-list applied to the working tree, so it also holds
untracked files, which `git describe --dirty` and `git diff` do not see. `hack/image.mk`
therefore stamps the image with `IMAGE_VERSION` and `IMAGE_GIT_TREE_STATE`: an untracked file
under an allow-listed path (`git ls-files --others` with the repository's `.gitignore` files, so
personal excludes do not hide it; plus git-ignored `kubeconfig*.go` sources, which `.dockerignore`
re-includes) makes the state `dirty` and appends `-dirty` to the version. `.dockerignore` excludes
everything else the current `.gitignore` files ignore, so no other ignored file reaches the
context (keep the two in sync when either changes). An untracked test file also counts although
`.dockerignore` drops it: the state errs towards `dirty`. `VERSION` or `GIT_TREE_STATE` given on
the command line or in the environment are used unchanged. `hack/build.mk` (`make build`) still
uses the tracked-only expressions.

Build arguments of the Dockerfile that are not set by make: `GOPROXY` (Go module mirror; go.sum
still verifies every module), `NPM_REGISTRY` (npm mirror for corepack and pnpm),
`KEEP_PACKAGE_MANAGER=true` (keep `/sbin/apk` for derived images), and the image pins
`ALPINE_IMAGE`, `GOLANG_IMAGE`, `NODE_IMAGE`, `PNPM_VERSION`, `PNPM_SHA512` (change them in the
Dockerfile, not on the command line, so `make image-verify-pins` checks them).

Stages (only `runtime` ships):

| Stage | Platform | Does |
|-------|----------|------|
| `ui` | build | corepack installs pnpm 10.33.0 verified by SHA-512, `pnpm install --frozen-lockfile`, `pnpm build` (vite) |
| `build` | build | `go build` for `TARGETOS/TARGETARCH` (cross-compiled, never emulated) with `ui/dist` copied into `internal/ui/dist` (the `make ui-build` convention), links the component names |
| `tools` | build | `hack/images/fetch-tools.sh`: downloads helm, kustomize, git-lfs, licenses and the connect-proxy source; every file is checked against `hack/images/checksums.txt` before use |
| `connect` | target | compiles connect-proxy (`gcc -static`, musl) |
| `runtime` | target | `apk add` (`hack/images/apk-packages.txt`), `hack/images/rootfs-setup.sh`, then the binaries |

The build needs network access to: `registry-1.docker.io` (base images, `docker/dockerfile`
frontend), `proxy.golang.org`/`sum.golang.org` (or `GOPROXY`), `registry.npmjs.org` (or
`NPM_REGISTRY`), `dl-cdn.alpinelinux.org`, `get.helm.sh`, `github.com` and
`objects.githubusercontent.com` (release assets), `raw.githubusercontent.com` (license texts,
connect-proxy source). BuildKit cache mounts keep Go modules, the Go build cache, the pnpm store
and tool downloads between builds.

### Multi-arch

```bash
make image-multiarch                                        # OCI archive: dist/image/patrol-cd_<tag>.oci.tar
make image-multiarch PUSH=1 REGISTRY=registry.example.com/team TAG=v0.1.0
#   -> registry.example.com/team/patrol-cd/patrol-cd:v0.1.0 (set IMAGE=patrol-cd for .../team/patrol-cd)
```

`PLATFORMS` defaults to `linux/amd64,linux/arm64`. Nothing is pushed unless `PUSH=1` **and** an
explicit `REGISTRY` are given; there is no default registry. Pushed and archived results carry
SBOM and provenance attestations (`--sbom=true --provenance=mode=max`). The Go and UI stages run
on the build machine's platform and cross-compile; only the `connect` and `runtime` stages
(package installation, one C file) execute target-platform binaries, so building the foreign
architecture needs QEMU binfmt handlers (Docker Desktop has them; on Linux:
`docker run --privileged --rm tonistiigi/binfmt --install arm64,amd64`) or native builder nodes.
Docker's default `docker` driver builds several platforms only with the containerd image store;
otherwise `docker buildx create --name patrol --use` first.

### SBOM

`make image-sbom [PLATFORM=linux/amd64]` builds the image with BuildKit's SBOM generator and keeps
the SPDX JSON under `dist/image/sbom/` (installed Alpine packages, Go modules of every binary from
their build information). The UI's npm dependencies are compiled into `patrolcd` and are not
visible to the scanner; their inventory is `ui/pnpm-lock.yaml`. Release-grade SBOMs, signing and
vulnerability/license reports belong to the release phase (TASKS T5.10).

## Running components

The entrypoint is `/sbin/tini --` and the default command `patrolcd --help`. tini runs as PID 1,
forwards SIGTERM/SIGINT to the component (which drains within `--shutdown-timeout`, default 30 s;
wiring.md "Shutdown") and reaps orphaned processes: the repo-server and commit-server start git,
ssh, gpg, helm, kustomize and plugins, and grandchildren that outlive a killed process group are
re-parented to PID 1. A Go process as PID 1 does not reap them. `TINI_SUBREAPER` is set so tini
still reaps when it is not PID 1 (e.g. `shareProcessNamespace: true`). Helper modes are unaffected:
`patrolcd` re-executes itself through `/proc/self/exe`, whatever name it was started as.

Choose the component with `args` and keep the entrypoint:

```yaml
containers:
  - name: repo-server
    image: patrol-cd/patrol-cd:v0.1.0
    args: [patrolcd, repo-server, --shutdown-timeout=30s]   # or: args: [/usr/local/bin/patrol-repo-server]
```

If a manifest sets `command`, it replaces tini; then include it:
`command: [/sbin/tini, --, /usr/local/bin/patrolcd]` with `args: [repo-server, ...]`. Short-lived
containers (the `patrolcd bootstrap` init container, the CMP copy init container) do not need
tini. Set `terminationGracePeriodSeconds` above `--shutdown-timeout` + 5 s (e.g. 45 s).

Locally (needs a daemon): `docker run --rm patrol-cd/patrol-cd:dev patrolcd version`,
`docker run --rm patrol-cd/patrol-cd:dev patrol version --client`.

Ports (`EXPOSE`, documentation only; wiring.md "Ports"): 8080 server API/UI and appset metrics,
8081 repo-server API and appset probes, 8082 controller metrics, 8083 server metrics, 8084
repo-server metrics/probes, 8086/8087 commit-server, 8090 controller tree API, 9001
notifications-controller metrics. Health is checked by Kubernetes probes; the image has no
`HEALTHCHECK`.

## Filesystem, user and security expectations

- User `patrol`, uid/gid **999** (`USER 999:999`, numeric so `runAsNonRoot` can be enforced),
  home `/home/patrol`, shell `/sbin/nologin`, locked password. Alpine's `ping` group, which owns gid
  999 in the base image, is removed. Run the pods with `runAsUser: 999`, `runAsGroup: 999`,
  `fsGroup: 999`.
- Designed for `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`,
  `capabilities: {drop: [ALL]}`, `seccompProfile: {type: RuntimeDefault}` (Pod Security
  "restricted"). The image has no setuid/setgid files and no package manager binary (`/sbin/apk`
  is removed; the package database stays for scanners).
- Arbitrary UIDs (OpenShift `restricted-v2` without `runAsUser: 999`) are not supported: OpenSSH
  refuses to run for a uid without a passwd entry, so SSH repositories would fail.
- Writable paths must be volumes. The image contains the mount points; ownership is `patrol` for
  the leaves and root for their parents:

| Path | Volume in a pod | Used by |
|------|-----------------|---------|
| `/tmp` | `emptyDir` (size-limited) | every component: `TMPDIR` (repo clones and per-call credential files unless `--root-dir`/`--credentials-dir` point elsewhere, GnuPG homes, Helm/Kustomize homes, `/tmp/extensions`) |
| `/home/patrol/cmp-server/plugins` | `emptyDir` shared by the repo-server and CMP sidecars | plugin sockets |
| `/home/patrol/cmp-server/config` | ConfigMap (`plugin.yaml`) | cmp-server |
| `/home/patrol/params` | ConfigMap item `<component>.profile.enabled` → `profiler.enabled` | profiler toggle |
| `/app/config/internal-token` | Secret `patrol-internal-token` | all but cmp-server |
| `/app/config/ssh` | ConfigMap `patrol-ssh-known-hosts-cm` → `ssh_known_hosts` | repo-server, commit-server, also `GlobalKnownHostsFile` of `/etc/ssh/ssh_config` |
| `/app/config/tls` | ConfigMap `patrol-tls-certs-cm` | repo-server, commit-server |
| `/app/config/gpg/source` | ConfigMap `patrol-gpg-keys-cm` | repo-server |
| `/app/config/gpg/keys` | optional `emptyDir` with `--gnupg-home-parent` | repo-server |
| `/app/config/reposerver/tls`, `/app/config/reposerver/mtls` | Secrets (repo-server serving pair, mTLS client pair) | repo-server and its clients |
| `/app/config/dex/tls` | Secret (strict Dex TLS) | server |
| `/shared/app` | optional ConfigMap/volume of extra static assets (`--staticassets`) | server |

Other mount paths a chart chooses (e.g. `/app/config/server/tls`, `/var/run/patrol/...`,
`/var/lib/patrol/repo`) need no directory in the image; the container runtime creates mount points.

Git and SSH configuration: internal/git runs every git command with `HOME=/dev/null`,
`GIT_CONFIG_GLOBAL=/dev/null`, no credential helper, hooks disabled and `protocol.ext` refused;
`/etc/gitconfig` repeats these settings for git started by other tools (Kustomize remote bases).
`safe.directory` is not set: every working copy belongs to uid 999, so git's ownership check
passes and a repository owned by another uid is refused. `/etc/ssh/ssh_config` makes ssh started
without internal/git's options check host keys strictly against
`/app/config/ssh/ssh_known_hosts`, never prompt (`BatchMode`) and never write `known_hosts`;
internal/git's command-line options always take precedence.

## Config Management Plugin sidecars

Plugin sidecars run the plugin author's image, not this one; they get the (static, libc-free)
`patrolcd` binary through a shared `emptyDir`. The Helm chart does this as follows (values
`repoServer.cmp.copyutil.command` and the plugin `command` default):

```yaml
initContainers:
  - name: copyutil
    image: patrol-cd/patrol-cd:<tag>
    command: [cp, /usr/local/bin/patrolcd, /var/run/patrol/patrolcd]
    volumeMounts: [{name: var-files, mountPath: /var/run/patrol}]
containers:
  - name: my-plugin                      # plugin image
    command: [/var/run/patrol/patrolcd, cmp-server]
    securityContext: {runAsUser: 999, runAsGroup: 999, runAsNonRoot: true}
    volumeMounts:
      - {name: var-files, mountPath: /var/run/patrol}
      - {name: plugins, mountPath: /home/patrol/cmp-server/plugins}   # shared with the repo-server
      - {name: plugin-config, mountPath: /home/patrol/cmp-server/config}
      - {name: cmp-tmp, mountPath: /tmp}
```

Copying to `/var/run/patrol/patrol-cmp-server` instead selects the subcommand by name (then
`command: [/var/run/patrol/patrol-cmp-server]`). The volume must not be mounted `noexec`, and the
sidecar must run as uid 999 so the repo-server can use the socket (`0600`).

## Pins and how to verify them

| Pin | Where | Checked by |
|-----|-------|------------|
| `docker/dockerfile` frontend, base images (`alpine`, `golang`, `node`) | `Dockerfile` (`name:tag@sha256:<index digest>`) | `verify-pins.sh`: digest exists, index has linux/amd64 + linux/arm64, tag drift reported |
| Go toolchain | `GOLANG_IMAGE` tag = `go.mod` `go` line; `GOTOOLCHAIN=local` | `verify-pins.sh` |
| pnpm | `PNPM_VERSION` = `ui/package.json` `packageManager`; `PNPM_SHA512` = npm `dist.integrity` (hex) | corepack refuses a mismatching tarball; `verify-pins.sh` |
| helm, kustomize, git-lfs, licenses, connect-proxy source | `hack/images/checksums.txt` (URL, SHA-256, archive members) | `fetch-tools.sh` (every build), `verify-pins.sh` (re-download, publisher checksum files) |
| GPL v2 text and BUILD note of connect-proxy | vendored in `hack/images/licenses/` (no download; provenance under [Licenses](#licenses)) | `rootfs-check.sh` (BUILD command = Dockerfile `connect` stage, COPYING is the GPL v2 text) |
| Alpine packages | `hack/images/apk-packages.txt` (names only) | `verify-pins.sh` (present for x86_64 and aarch64) |

```bash
make image-verify-pins        # online: re-download + SHA-256, publisher sums, registry digests, Alpine index
hack/images/verify-pins.sh --offline
make image-rootfs-check       # assemble the runtime filesystem without Docker and compare (both arches)
make image-layout-check       # compare the built image's filesystem with rootfs-expected.txt (needs a daemon)
make image-smoke              # run the built image under the chart's restricted settings and probe it
```

To bump a tool: change version, URL and SHA-256 of each of its lines in
`hack/images/checksums.txt` (take the SHA-256 from the publisher's checksum file named in column 7)
and run both targets. To bump a base image: resolve the new index digest, update the `ARG` (and
the `base.digest` label for Alpine), run `make image-verify-pins`.

## Air-gapped installation

Build (or pull) the image where the network is available, then mirror it with every platform.
Examples (documentation only; registry names are placeholders):

```bash
# skopeo: copy all platforms, keep the digest
skopeo copy --all docker://registry.example.com/team/patrol-cd/patrol-cd:v0.1.0 \
  docker://mirror.internal.example/patrol-cd/patrol-cd:v0.1.0

# crane
crane copy registry.example.com/team/patrol-cd/patrol-cd:v0.1.0 mirror.internal.example/patrol-cd/patrol-cd:v0.1.0

# from the OCI archive of `make image-multiarch` (no registry needed to build it)
skopeo copy --all oci-archive:dist/image/patrol-cd_v0.1.0.oci.tar \
  docker://mirror.internal.example/patrol-cd/patrol-cd:v0.1.0

# docker only (one platform per image): save, transfer, load, tag, push
docker save -o patrol-cd_v0.1.0_amd64.tar registry.example.com/team/patrol-cd/patrol-cd:v0.1.0
docker load -i patrol-cd_v0.1.0_amd64.tar
```

Reference the mirrored image by digest in the chart (`image.digest`) or manifests. Mirror the Dex
image (`dexidp/dex`) too when SSO through the bundled Dex is used, and any CMP plugin images.

## Tags, versions and reproducibility

- Release tags are the git tags (`vX.Y.Z`), which `git describe` turns into the binaries'
  version; `make image` defaults to `dev`. Prefer digests in deployments; tags are mutable.
- `org.opencontainers.image.*` labels: `source` https://github.com/juliosaraiva/patrol-cd,
  `licenses` (see [Licenses](#licenses)), `version`, `revision` (commit), `created` (commit time),
  `base.name` and `base.digest`.
- Same inputs, same binaries: Go builds are reproducible (`-trimpath`, `-buildvcs=false`, version
  data from build arguments, commit time instead of wall-clock time; a version without `-dirty`
  means no tracked change and no untracked file in the build context), and the bundled tools are
  fixed by SHA-256. The Alpine packages are not: `apk add` installs the current 3.24 package
  versions, so a rebuild picks up Alpine security fixes and can differ from an earlier build; the
  SBOM records what was installed. `make image-multiarch` additionally rewrites file timestamps to
  `SOURCE_DATE_EPOCH`.

## Known limits

- `make image` and `make image-check` have run (arm64 only). `make image-multiarch` (amd64 and
  arm64) and `make image-sbom` have not; the amd64 half of the pins and of
  `make image-rootfs-check` is checked statically only.
- connect-proxy is built by the Dockerfile's `gcc -static` command (the one recorded in `BUILD`)
  and runs in the image (`Version 1.106`, asserted by `make image-smoke`). No SSH connection
  through a SOCKS5 proxy has been made with it.
- GnuPG, SSH and git were exercised in the arm64 image on a read-only root filesystem: the
  repo-server's key import (`gpg --batch --no-autostart --import` into a fresh home) works with
  the image's GnuPG 2.4.9, which ships without `gpg-agent` (import and verification only, no key
  generation or signing); `ssh -V` and the system gitconfig hardening were checked. Cloning,
  signature verification of a real signed commit, and SSH to a host are not exercised until the
  kind e2e run.
- The prebuilt Kustomize 5.8.1 and git-lfs 3.7.1 release binaries were built with Go 1.24.0 and
  1.25.3; scanners may report Go standard library CVEs for them. Rebuilding them from source would
  diverge from the baseline's binaries.
- Arbitrary UIDs are not supported (see above). `/shared/download` is not populated: only the
  Linux CLI of the server's architecture is downloadable (from `PATH`).

## Verification

Static verification on 2026-10-18 (macOS arm64, no container runtime), with commands and
results: [`docs/verification/runs/2026-10-17-image-static.md`](../verification/runs/2026-10-17-image-static.md).

Build and checks of the built image on 2026-10-19 (`linux/arm64`, 386 MB, 11 layers; commands and
results: [`docs/verification/runs/2026-10-19-image-build.md`](../verification/runs/2026-10-19-image-build.md)):

- `make image-check` (BuildKit's Dockerfile checks): no warnings; `make image`: built in 89 s.
- `make image-layout-check` (`hack/imagelayout`): the 52 expectations of `rootfs-expected.txt`
  hold in the real image (users, modes, mount points, links, files that must be absent).
- `make image-smoke` (`hack/images/smoke.sh`): 25 checks under `--read-only --cap-drop ALL
  --security-opt no-new-privileges --user 999:999`: identity, version stamps of `patrolcd` and
  `patrol`, the pinned versions of helm, kustomize and git-lfs, `connect-proxy`, all 11 component
  aliases, and a repo-server that is ready in seconds, has `tini` as PID 1 and exits 0 on SIGTERM.
- Not done: amd64, multi-arch, SBOM, scans, push, a run in a cluster.
