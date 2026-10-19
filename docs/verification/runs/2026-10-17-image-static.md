# Image: static verification (no container runtime)

Run on 2026-10-18 by the image workstream (Phase 4, T4.1). Scope: `Dockerfile`, `.dockerignore`,
`hack/image.mk`, `hack/images/*`, `docs/guides/images.md`.

**docker build: NOT EXECUTED (Docker Desktop daemon out of disk).** Every stage was verified by
other means below; nothing has run inside the image. Scratch paths are abbreviated as `$S`
(`/private/tmp/claude-501/<session>/scratchpad/image`).

> **Values re-derived on the final tree, 2026-10-18.** The second review round changed the image
> files (connect-proxy `COPYING`/`BUILD` shipped, licenses label, version stamp from untracked
> files, rootfs-check PASS logic; [`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md)).
> Re-run on the final tree: `make image-rootfs-check` exit 0, `rootfs-check: OK (linux/amd64)`
> and `OK (linux/arm64)`, **52** layout entries per arch (50 in #23 below), **200** PASS lines
> and 0 FAIL (190 in #24), 23 s. Reported by the image fix agent and not re-run for this
> note: the build context lists 1,731 files (#15: 1,728), `hack/images/verify-pins.sh
> --offline` exit 0, `shellcheck hack/images/*.sh` exit 0. All other rows are the original
> runs on the earlier tree. `docker build` remains NOT EXECUTED.

## Environment

| Item | Value |
|------|-------|
| Host | macOS 27.0.1 arm64 |
| Go | go1.27.2 darwin/arm64 |
| Node / pnpm / corepack | v22.22.3 / 10.33.0 / 0.29.4 |
| Shells | `/bin/sh` (bash 3.2.57 POSIX mode), `/bin/dash`, bash 3.2.57 |
| tar | GNU tar 1.35 (PATH default), bsdtar 3.5.3 (`/usr/bin/tar`) |
| Other | shellcheck 0.11.0, Apple clang 21.0.0, GnuPG 2.5.24, OpenSSH 10.6p1, jq, file |
| Docker | client/engine 29.8.2, buildx v0.37.2, BuildKit v0.33.1 (daemon disk full) |

## Results

| # | Check | Command (abridged) | Result |
|---|-------|--------------------|--------|
| 1 | BuildKit lint, one attempt | `docker buildx build --check --file Dockerfile .` | **BLOCKED**, exit 1: `failed to read dockerfile: ... failed to create prepare snapshot dir: ... mkdir /var/lib/desktop-containerd/daemon/io.containerd.snapshotter.v1.overlayfs/snapshots/new-1983036061: no space left on device` |
| 2 | Image build | `make image` | **NOT EXECUTED** (same disk condition; not attempted to avoid writes) |
| 3 | Base image digests | Docker Hub tag API + `registry-1.docker.io/v2/<repo>/manifests/<tag>` with an anonymous pull token (Accept: OCI index / manifest list) | passed: see "Pins" |
| 4 | Tool pins (download + SHA-256) | `curl -o` each URL into its own scratch dir, `shasum -a 256` | passed: all six archives match the publishers' checksum files |
| 5 | PGP signatures (manual, once) | `gpg --verify` with keys from `https://github.com/bk2204.gpg`, `https://github.com/chrisd8088.gpg`, `https://raw.githubusercontent.com/helm/helm/main/KEYS` in a scratch `GNUPGHOME` | passed: git-lfs `sha256sums.asc` good signature, Chris Darroch, primary `86CD3297749375BCF8206715F54FE648088335A9` (subkey `F54FE648088335A9`); helm amd64 and arm64 `.asc` good signature, Scott Rigby, `208DD36ED5BB3745A16743A4C7C6FBB5B91C1155`. Keys were not certified further (trust "unknown") |
| 6 | Archive layout and member safety | `tar -tzvf` on all six archives | passed: helm `linux-<arch>/{helm,LICENSE,README.md}`, kustomize `kustomize`, git-lfs `git-lfs-3.7.1/{git-lfs,install.sh,README.md,CHANGELOG.md,man/...}`; no absolute or `..` entries |
| 7 | Tool binaries without executing them | `file`, `go version -m` | passed: all static ELF for the right arch; helm `mod helm.sh/helm/v4 v4.2.1` (go1.26.4), kustomize `-X sigs.k8s.io/kustomize/api/provenance.version=v5.8.1` (go1.24.0), git-lfs contains `3.7.1` (go1.25.3) |
| 8 | `fetch-tools.sh` | `hack/images/fetch-tools.sh --platform linux/{amd64,arm64} --out ... --cache ...` with `/bin/sh`, with `/bin/dash`, and with `PATH=/usr/bin:/bin` (bsdtar) | passed: 6 files per platform (helm, kustomize, git-lfs, 3 license texts, connect-proxy source); dash and bsdtar outputs identical (`diff -r`) |
| 9 | `fetch-tools.sh` refusals | tampered sum; `..` in member name; missing member; `../` destination; unsupported platform; crafted archives with `../evil`, `/etc/evil`, and a symlink member (served from the content-addressed cache) | passed: all 8 refused with exit 1 (`SHA-256 mismatch`, `unsafe archive member name`, `member ... not found`, `unsafe destination path`, `unsupported platform`, `contains an unsafe entry` ×2, `is not a regular file`) |
| 10 | pnpm pin | `registry.npmjs.org/pnpm/10.33.0` `dist.integrity` → hex; `COREPACK_HOME=$S/... corepack install -g pnpm@10.33.0+sha512.<hex>` | passed: wrong hash rejected (`Mismatch hashes`, exit 1), correct hash installed; `COREPACK_ENABLE_NETWORK=0 corepack pnpm --version` in `ui/` → `10.33.0` (served from the verified cache) |
| 11 | connect-proxy source | raw file at commit `a7443d32e6bf6f00698384b0519adc16fa7656fa` vs `connect.c` in the tag 1.106 tarball | passed: both `96b54390603191150ba2e03b28c318afdf84610904927a825658d3b02617526a`; license header GPL-2.0-or-later |
| 12 | connect-proxy compile on musl (no gcc/Linux available) | Alpine v3.24 `musl-dev-1.2.6-r2` (x86_64, aarch64) extracted to scratch; `clang --target=<arch>-linux-musl -nostdinc -isystem <musl>/usr/include -std={gnu17,gnu23} -O2 -Wall -DSOCKLEN_T=socklen_t -c connect.c` | passed: 4/4 compile, 0 warnings, 0 errors |
| 13 | connect-proxy static link symbols | `nm -u connect.o` vs `nm --defined-only libc.a` | passed: 72/72 undefined symbols defined in musl's `libc.a` for both arches (incl. `res_init`, `__res_state`); `libssp_nonshared.a` and `__stack_chk_fail` present for `-fstack-protector-strong -static`. The real `gcc -static` link was not run |
| 14 | Alpine packages | `verify-pins.sh` (APKINDEX of v3.24 main + community) | passed for x86_64 and aarch64: ca-certificates 20260909-r0, git 2.54.0-r0, gpg 2.4.9-r1, openssh-client → openssh-client-default 10.3_p1-r1, tini 0.19.0-r3, tzdata 2026e-r0; build stages curl 8.22.0-r0, gcc 15.2.0-r5, musl-dev 1.2.6-r2. Package contents read from the `.apk` files: tini installs `/sbin/tini`, apk-tools `/sbin/apk`, `alpine-baselayout-data` has `ping:x:999:` in `/etc/group` (handled by `rootfs-setup.sh`) |
| 15 | Build context (`.dockerignore`) | scratch tool `ctxsim` using `github.com/moby/patternmatcher` v0.6.1 + `ignorefile` (the matcher BuildKit uses) | passed: 1,728 files with the final file (api 17, cmd 2, internal 1,337, pkg 65, ui 292, hack/images 11, go.mod/go.sum, LICENSE, NOTICE); no `*_test.go`, no `ui/node_modules`, `ui/dist`, `bin/`, `.git`, kubeconfig or `.env` files; `internal/ui/dist` only `.keep` and `.gitignore`. Probe tree: `kubeconfig.go` kept; `kubeconfig`, `kubeconfig.yaml`, `kubeconfig_test.go`, `a_test.go`, `testdata/k.go`, `.env` excluded |
| 16 | `ui` stage in the filtered context | copy of the context; `pnpm install --frozen-lockfile`; `pnpm build` | passed: install 2.4 s, vite build 3.0 s; `dist/` 73 files (index.html, favicon.svg, .keep, 70 under assets/; 1.6 MB) |
| 17 | `build` stage cross-compile | in the filtered context with `ui/dist` copied into `internal/ui/dist`: `CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOOS=linux GOARCH={amd64,arm64} go build -trimpath -buildvcs=false -ldflags "<Dockerfile ldflags>"` for `./cmd/patrolcd`, `./cmd/patrol` | passed: linux/amd64 patrolcd 91,447,456 B, patrol 87,253,152 B; linux/arm64 patrolcd 84,410,528 B, patrol 80,478,368 B; `file`: ELF 64-bit, statically linked, stripped; `go version -m`: go1.27.2, `-trimpath=true`, `CGO_ENABLED=0`, `GOOS=linux`, right `GOARCH` |
| 18 | Dockerfile RUN snippets as written | `dfrun.py` (scratch): every RUN body, continuations joined, `--mount` removed, `dash -n` | passed: 10 RUN instructions, 0 syntax errors |
| 19 | `build` stage RUN executed verbatim | body of the Go build RUN, paths `/out/bin` and `/usr/local/share/patrol-image` remapped, run with `dash -eu` and `TARGETOS/TARGETARCH` = linux/arm64 and darwin/arm64 | passed: both binaries and 11 links; darwin build: `patrolcd version -o json` → version `ef4e9cf-dirty`, commit `ef4e9cf3…`, buildDate `2026-10-18T18:51:39Z`, gitTreeState `dirty`; `patrol version --client` → same (both version packages stamped) |
| 20 | Component-name dispatch | the 11 links next to a darwin build, each with `--help` | passed: each prints the usage of its subcommand (`patrol-server` → `patrolcd server`, …, `patrol-k8s-auth` → `patrolcd k8s-auth`) |
| 21 | Embedded UI is served | scratch harness: envtest kube-apiserver 1.36.0 with `deploy/crds`, then the darwin `patrolcd server --insecure ... --kubeconfig <envtest>` built as in #17 | passed: `/healthz` 200; `GET /` 200 text/html = built `index.html` (not the "UI not built" page; same asset references as `ui/dist/index.html`); `/assets/index-i7JZL4_U.js` 200 `text/javascript; charset=utf-8` 414,952 B and `/assets/index-DE0kRzlM.css` 200 `text/css; charset=utf-8` 29,768 B, byte-identical to `ui/dist`; `/api/version` → `{"version":"ef4e9cf-dirty"}`; SIGTERM → exit 0 |
| 22 | `runtime` stage RUN executed | body of the runtime RUN with `apk add` replaced by `echo`, `/run/patrol-image` → `hack/images`, `--root /` → scratch root + `--no-chown`, `KEEP_PACKAGE_MANAGER` = false and true | passed: package list expands to `ca-certificates git gpg openssh-client tini tzdata`; patrol user created, ping group removed; `/sbin/apk` removed only when false |
| 23 | Final layout | `hack/images/rootfs-check.sh --arch {amd64,arm64}` with the binaries of #17 (UI embedded) and the tools of #8 | passed: 0 failures each (50 layout entries: modes, owners from the `--no-chown` log, link targets; idempotent second setup run; accounts; no setuid; aliases = `binaryAliases`; every default path under /app/config, /home/patrol, /shared compiled into internal/ exists except the optional /shared/download; ELF/arch/static; Go build settings; helm/kustomize/git-lfs versions; tools.lock) |
| 24 | Same, self-contained | `make image-rootfs-check` (builds the linux binaries, downloads the tools into `.cache/image-tools`) | passed: `rootfs-check: OK (linux/amd64)`, `OK (linux/arm64)`, 190 PASS lines, 0 FAIL, 31 s |
| 25 | rootfs-check catches mistakes | copies of hack/images with one mutation each: alias removed; internal-token dir 0755; `tini` dropped from apk-packages.txt; ping-group handling disabled; `/app/config/dex/tls` not created | passed: all 5 detected (exit 1): missing link + alias diff; mode 0755 ≠ 0700; `lacks tini`; `gid 999 is already used: ping`; layout + default-path failures. Unmodified copy: exit 0 |
| 26 | All pins, online | `make image-verify-pins` (fresh downloads, no cache) | passed: 47 PASS, 0 failures, 0 warnings: checksums.txt format; Dockerfile pins; Go 1.27.2 = go.mod; pnpm = packageManager; both platforms fetched and verified; SHA-256 = publisher files (helm `.sha256sum`, kustomize `checksums.txt`, git-lfs `sha256sums.asc`); digests of alpine, golang, node, docker/dockerfile exist, cover linux/amd64 + linux/arm64, tags unchanged; Alpine packages for both arches |
| 27 | Config files | `git config --file hack/images/gitconfig --list`; `ssh -G -F hack/images/ssh_config git@example.com` | passed: both parse; ssh resolves `globalknownhostsfile /app/config/ssh/ssh_known_hosts`, `userknownhostsfile /dev/null`, `stricthostkeychecking true`, `batchmode yes`, `updatehostkeys false`; with internal/git's command-line options (`-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null`, or a per-call file) the command line wins |
| 28 | GnuPG fresh-home import (host GnuPG, not the image's 2.4.9) | `GNUPGHOME=<new 0700 dir> gpg --batch --no-tty --no-autostart --no-permission-warning --import <key>` | passed: exit 0, `pubring.kbx` created, no agent or keyboxd needed |
| 29 | Make targets | `make help`; `make -n image`; `make image-multiarch PUSH=1`; `PUSH=yes`; `PUSH=1 REGISTRY=https://evil.example` | passed: targets listed; build args rendered from git; the three push guards stop before any docker command (`PUSH=1 needs an explicit REGISTRY`, `PUSH must be 1 or empty`, `invalid REGISTRY`) |
| 30 | Shell scripts | `shellcheck hack/images/*.sh` | passed: 0 findings (POSIX `sh` for the scripts that run in the image build; bash 3.2-compatible for workstation scripts) |

## Pins (resolved 2026-10-18)

| Pin | Value | Source |
|-----|-------|--------|
| `docker/dockerfile` | `1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e` (published 2026-09-30) | registry index digest |
| `alpine` | `3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6`; amd64 `sha256:d56c381f…`, arm64/v8 `sha256:260479a1…` | registry; same digest as the e2e fixtures |
| `golang` | `1.27.2-alpine3.24@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d`; amd64 `sha256:3b15ba43…`, arm64/v8 `sha256:c2b8d9b0…` | registry |
| `node` | `22.23.3-alpine3.24@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402`; amd64 `sha256:2c752226…`, arm64/v8 `sha256:85cdd100…` | registry |
| pnpm 10.33.0 | sha512 hex `10568bb4…8649319` (integrity `sha512-EFaLtKav…GSTGQ==`) | `https://registry.npmjs.org/pnpm/10.33.0` |
| helm 4.2.1 | amd64 `479dca83…7c17fdae`, arm64 `596b9a73…e72814db` | `https://get.helm.sh/helm-v4.2.1-linux-<arch>.tar.gz(.sha256sum)` |
| kustomize 5.8.1 | amd64 `029a7f0f…b53a819d`, arm64 `0953ea3e…fef2c142` | `https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv5.8.1/` |
| git-lfs 3.7.1 | amd64 `1c0b6ee5…b5a5ec08`, arm64 `73a9c90e…8588dd8d` | `https://github.com/git-lfs/git-lfs/releases/download/v3.7.1/` |
| licenses | kustomize `b40930bb…5ef833e1`, git-lfs `4fae9062…62c366b6` | `raw.githubusercontent.com` at the release tags |
| connect-proxy | `96b54390…2617526a` | `raw.githubusercontent.com/gotoh/ssh-connect/a7443d32…/connect.c` |

Full values: `hack/images/checksums.txt` and the Dockerfile `ARG`s.

## Not verified (needs a container runtime or a cluster)

- Any `docker build`/`buildx` execution: the `ui` stage inside `node:22.23.3-alpine3.24` (corepack
  0.3x there), `go mod download` in the golang image, `apk add` results, the `gcc -static`
  connect-proxy link, `COPY --chmod`/symlink preservation, image size, labels, `--check` lint
  results, `make image-multiarch`, `make image-sbom`, `make image-load-kind`.
- Behavior inside the image: git/ssh/gpg (Alpine builds) with a read-only root filesystem, tini
  signal forwarding and reaping, `helm version`/`kustomize version` from the running repo-server
  (OPS-162 test), SSH through a SOCKS5 proxy (REPO-023).
