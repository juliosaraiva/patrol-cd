# 2026-10-19 — first image build and checks of the built image

Environment: macOS arm64 host, Docker Desktop 29.8.2 (BuildKit 0.33.1, builder `desktop-linux`),
repository at commit `3ea2bd3` with a clean tree (the image's `GitTreeState` label is `clean`).
Everything below ran locally; nothing was pushed to a registry, loaded into a cluster or scanned.

## Freeing the Docker disk (user request: "free docker disk space")

| Step | Command | Result |
|------|---------|--------|
| before | `docker system df` | images 48.6 GB, volumes 21.26 GB, build cache 30.02 GB (18.7 GB reclaimable); 28 running containers of other projects |
| build cache | `docker builder prune --force` | 18.54 GB reclaimed |
| dangling images | `docker image prune --force` | 5.183 GB reclaimed |
| after | `docker system df`; `docker run --rm alpine:3.24.2 df -h /` | build cache 11.48 GB; **152.1 GB available** of 235.7 GB inside the VM |

Not touched: running containers, stopped containers, named volumes (4 GB reclaimable), tagged
unused images (about 17 GB reclaimable); they may belong to other projects.

## Build

| Step | Command | Result |
|------|---------|--------|
| Dockerfile checks | `make image-check` (`docker buildx build --check`) | exit 0, "Check complete, no warnings found"; the three pinned base digests resolve |
| build | `make image` (`docker build … --tag patrol-cd/patrol-cd:dev`) | exit 0 in 89 s (11:48:05Z–11:49:34Z), no warnings in the log, 2 cached steps; image `sha256:646810be1069…`, 386 MB, 11 layers, `linux/arm64` |

Image metadata (`docker image inspect`): user `999:999`, entrypoint `["/sbin/tini","--"]`, cmd
`["patrolcd","--help"]`; labels `version=3ea2bd3`, `revision=3ea2bd32d3f0…`,
`created=2026-10-19T11:41:02Z` (the commit time), `base.name=docker.io/library/alpine:3.24.2` with
its digest, `licenses=Apache-2.0 AND MIT AND GPL-2.0-or-later AND LicenseRef-Bundled-Dependencies`.

## Checks of the built image

All run under the chart's restricted runtime: `--read-only --cap-drop ALL
--security-opt no-new-privileges --user 999:999`, writable emptyDirs as tmpfs.

| # | Check | Command | Result |
|---|-------|---------|--------|
| 1 | Filesystem layout equals the promised layout | `make image-layout-check` (`go run ./hack/imagelayout patrol-cd/patrol-cd:dev`, new) | 52 expectations of `hack/images/rootfs-expected.txt` checked against 1954 image paths, 0 failed. Negative control: an expected file with a wrong mode and a wrong owner reported exactly those two failures (exit 1); `hack/imagelayout` also has unit tests for every failure kind |
| 2 | Runtime smoke test | `make image-smoke` (`hack/images/smoke.sh`, new) | 25 checks pass: runs as `uid=999(patrol)`; `patrolcd` and `patrol` report `3ea2bd3`; `helm` v4.2.1, `kustomize` v5.8.1, `git-lfs` 3.7.1 equal the pins in `checksums.txt`; git 2.54.0; `connect-proxy` 1.106 runs; all 11 aliases of `hack/images/aliases.txt` print their own subcommand's help; the repo-server answers `/readyz` and `/healthz?full=true` with 200 (ready after 2 s in the manual run that preceded the script), PID 1 is `tini`, 0 restarts, SIGTERM gives exit status 0 and `msg=stopped` |
| 3 | Smoke test can fail | `hack/images/smoke.sh alpine:3.24.2` | FAIL lines (wrong identity, no `patrolcd`), non-zero exit; no container left behind |
| 4 | Read-only root | `touch /usr/local/bin/x`, `touch /etc/x` | both `Read-only file system` |
| 5 | Bundled tools on real inputs | `docker run … -v examples:/ex:ro` | `helm template` of `examples/apps/app-of-apps/chart`: 3 objects; `kustomize build` of the first five non-Helm kustomizations found under `examples/apps` (base, cmp-plugin/plugin, overlays prod, staging, dev): 3, 1, 4, 3, 3 objects. The Helm-inflation example fails with `trouble configuring builtin HelmChartInflationGenerator`: it needs `--enable-helm` and network access (observed, not asserted) |
| 6 | GnuPG import as the repo-server does it | `gpg --batch --no-autostart --import` of `examples/platform/settings/patrol-gpg-keys-cm.yaml`'s public key into a fresh `GNUPGHOME` on the read-only root | 1 key imported, 1 public key listed. The image has `gpg` but no `gpg-agent` (Alpine's `gpg` package; verification and import only, no key generation or signing) |
| 7 | OpenSSH, git hardening | `ssh -V`; `git config --system --list` | OpenSSH_10.3p1; system gitconfig sets `core.hookspath=/dev/null`, `core.fsmonitor=false`, `protocol.ext.allow=never`, `safe.barerepository=explicit`, `credential.helper=` |
| 8 | Web UI embedded | `grep -a -o 'assets/index-[A-Za-z0-9_-]*\.js' /usr/local/bin/patrolcd` | `assets/index-i7JZL4_U.js` (the UI bundle's hashed asset is in the binary) |
| 9 | Binary sizes | `ls -l` | `patrolcd` 84,410,528 bytes, `patrol` 80,478,368 bytes |

Defects found in my own new tooling while doing this (fixed before the final runs): the first
`smoke.sh` tried to drop `--network none` with an array substitution that cannot remove two
elements, so the published port was unreachable; the extended version aborted mid-loop because
`| head -1` closed a pipe and the Go binary reported `broken pipe` under `pipefail` (now
`first_line`). Neither was an image defect.

## NOT done

- `linux/amd64` was not built (the host and daemon are arm64); `make image-multiarch` was not run.
  The amd64 pins, the amd64 `rootfs-check` and the cross-platform build remain statically checked only.
- `make image-sbom`, vulnerability scanning, license scanning: not run (release phase, TASKS T5.10).
  Whether the SBOM lists connect-proxy and what apk's database records for each package's license
  (finding R2-8) remain unchecked.
- The image was not pushed, not loaded into kind and not run in a cluster; the server, controller,
  appset, notifications, commit-server and cmp-server components were only started as far as
  `--help`; only the repo-server ran (it needs no Kubernetes API).
- No git clone, helm repository or OCI pull through the repo-server, no SSH-through-SOCKS5
  connection (REPO-023), no signature verification of a signed commit.
- `make image-verify-pins` online mode was not re-run (no pin changed).
