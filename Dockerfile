# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# patrol-cd: the single multi-call image of Patrol CD. It holds every control-plane component
# (`patrolcd <component>`, also linked under each component name), the CLI (`patrol`) and the
# pinned tools the repo-server runs (ADR-0007). Guide: docs/guides/images.md.
#
#   make image                     # local single-platform build: patrol-cd/patrol-cd:dev
#   make image-multiarch           # linux/amd64 + linux/arm64 OCI archive (push needs PUSH=1 REGISTRY=...)
#
# Stages; only "runtime" ships:
#   ui       BUILDPLATFORM   pnpm (corepack, SHA-512 pinned) install --frozen-lockfile + vite build
#   build    BUILDPLATFORM   Go cross-compile for TARGETOS/TARGETARCH (CGO off, -trimpath), UI embedded
#   tools    BUILDPLATFORM   helm, kustomize, git-lfs, license texts, connect-proxy source (SHA-256 pinned)
#   connect  TARGETPLATFORM  connect-proxy compiled from the pinned source (static, musl)
#   runtime  TARGETPLATFORM  Alpine 3.24 + git, openssh-client, gpg, tini, ca-certificates, tzdata
# The Go and UI builds never run under emulation; only the two TARGETPLATFORM stages (package
# installation, one small C file) need QEMU/binfmt when building for a foreign architecture.

# Base images, pinned by multi-arch index digest (verified with `make image-verify-pins`).
ARG ALPINE_IMAGE=docker.io/library/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG GOLANG_IMAGE=docker.io/library/golang:1.27.2-alpine3.24@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d
ARG NODE_IMAGE=docker.io/library/node:22.23.3-alpine3.24@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402
# pnpm as pinned by ui/package.json "packageManager"; the SHA-512 (hex) of the npm tarball is
# checked by corepack before pnpm runs (npm registry dist.integrity of pnpm@10.33.0).
ARG PNPM_VERSION=10.33.0
ARG PNPM_SHA512=10568bb4a6afb58c9eb3630da90cc9516417abebd3fabbe6739f0ae795728da1491e9db5a544c76ad8eb7570f5c4bb3d6c637b2cb41bfdcdb47fa823c8649319

# ---------------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS ui
SHELL ["/bin/sh", "-eu", "-c"]
ARG PNPM_VERSION
ARG PNPM_SHA512
# Optional npm registry mirror (corepack and pnpm); empty: registry.npmjs.org.
ARG NPM_REGISTRY=
ENV COREPACK_HOME=/opt/corepack \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    COREPACK_ENABLE_AUTO_PIN=0 \
    CI=true
RUN if [ -n "$NPM_REGISTRY" ]; then export COREPACK_NPM_REGISTRY="$NPM_REGISTRY"; fi; \
    corepack enable pnpm; \
    corepack install --global "pnpm@${PNPM_VERSION}+sha512.${PNPM_SHA512}"
WORKDIR /src/ui
COPY ui/package.json ui/pnpm-lock.yaml ui/pnpm-workspace.yaml ./
# COREPACK_ENABLE_NETWORK=0: pnpm must come from the verified corepack cache, never a new download.
RUN --mount=type=cache,id=patrol-pnpm-store,target=/var/cache/pnpm-store \
    if [ -n "$NPM_REGISTRY" ]; then export npm_config_registry="$NPM_REGISTRY"; fi; \
    COREPACK_ENABLE_NETWORK=0 pnpm install --frozen-lockfile --store-dir=/var/cache/pnpm-store
COPY ui/ ./
RUN COREPACK_ENABLE_NETWORK=0 pnpm build; \
    test -f dist/index.html

# ---------------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS build
SHELL ["/bin/sh", "-eu", "-c"]
# GOTOOLCHAIN=local: build with exactly the image's Go (go.mod: go 1.27.2), never a download.
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly
# Optional Go module proxy (e.g. a corporate mirror); empty: Go's default. go.sum still verifies
# every module.
ARG GOPROXY=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=patrol-gomod,target=/go/pkg/mod \
    go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
# internal/ui embeds internal/ui/dist (the hack/build.mk ui-build convention).
COPY --from=ui /src/ui/dist/ internal/ui/dist/
COPY hack/images/aliases.txt hack/images/link-aliases.sh /usr/local/share/patrol-image/
ARG TARGETOS
ARG TARGETARCH
# Build information (internal/server/version.go, internal/cli/cmdutil/version.go); hack/image.mk
# passes the git describe version, commit, tag, tree state and the commit date.
ARG VERSION=devel
ARG COMMIT=
ARG GIT_TAG=
ARG GIT_TREE_STATE=
ARG DATE=
RUN --mount=type=cache,id=patrol-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=patrol-gobuild,target=/root/.cache/go-build \
    ldflags="-s -w"; \
    for pkg in github.com/juliosaraiva/patrol-cd/internal/server github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil; do \
      ldflags="$ldflags -X $pkg.version=${VERSION} -X $pkg.gitCommit=${COMMIT} -X $pkg.gitTag=${GIT_TAG}"; \
      ldflags="$ldflags -X $pkg.gitTreeState=${GIT_TREE_STATE} -X $pkg.buildDate=${DATE}"; \
    done; \
    for cmd in patrolcd patrol; do \
      GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath -buildvcs=false -ldflags "$ldflags" \
        -o "/out/bin/$cmd" "./cmd/$cmd"; \
    done; \
    sh /usr/local/share/patrol-image/link-aliases.sh /out/bin /usr/local/share/patrol-image/aliases.txt

# ---------------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${ALPINE_IMAGE} AS tools
SHELL ["/bin/sh", "-eu", "-c"]
RUN apk add --no-cache curl
COPY hack/images/checksums.txt hack/images/fetch-tools.sh /usr/local/share/patrol-image/
ARG TARGETOS
ARG TARGETARCH
# Every download is checked against hack/images/checksums.txt before it is extracted.
RUN --mount=type=cache,id=patrol-tools,target=/var/cache/patrol-tools \
    sh /usr/local/share/patrol-image/fetch-tools.sh --platform "${TARGETOS}/${TARGETARCH}" \
      --out /out --cache /var/cache/patrol-tools

# ---------------------------------------------------------------------------------------------
# connect-proxy: SOCKS5 ProxyCommand used by internal/git for SSH repositories with a proxy
# (Alpine does not package it). Static, so it needs nothing from the runtime image. The gcc
# command is recorded verbatim in hack/images/licenses/connect-proxy-BUILD.txt, which ships next
# to the source; rootfs-check.sh fails when the two differ.
FROM ${ALPINE_IMAGE} AS connect
SHELL ["/bin/sh", "-eu", "-c"]
RUN apk add --no-cache gcc musl-dev
COPY --from=tools /out/src/connect-proxy/connect.c /src/connect.c
RUN mkdir -p /out; \
    gcc -std=gnu17 -O2 -Wall -fstack-protector-strong -DSOCKLEN_T=socklen_t -static -s \
      -o /out/connect-proxy /src/connect.c; \
    test -x /out/connect-proxy

# ---------------------------------------------------------------------------------------------
FROM ${ALPINE_IMAGE} AS runtime
SHELL ["/bin/sh", "-eu", "-c"]
# true keeps /sbin/apk for images derived from this one (removed by default).
ARG KEEP_PACKAGE_MANAGER=false
# Packages (hack/images/apk-packages.txt), user patrol 999:999, mount points, /etc/gitconfig,
# /etc/ssh/ssh_config, setuid/setgid bits stripped, package manager binary removed.
RUN --mount=type=bind,source=hack/images,target=/run/patrol-image \
    apk add --no-cache $(grep -v '^#' /run/patrol-image/apk-packages.txt); \
    keep=; \
    if [ "$KEEP_PACKAGE_MANAGER" = true ]; then keep=--keep-package-manager; fi; \
    sh /run/patrol-image/rootfs-setup.sh --root / --config-dir /run/patrol-image $keep

COPY --from=tools --chmod=0755 /out/bin/ /usr/local/bin/
COPY --from=connect --chmod=0755 /out/connect-proxy /usr/local/bin/connect-proxy
COPY --from=tools /out/licenses/ /usr/share/licenses/
# connect-proxy is GPL-2.0-or-later: the license text and the build command go next to its source
# (/usr/share/licenses/connect-proxy/connect.c).
COPY --chmod=0644 hack/images/licenses/GPL-2.0.txt /usr/share/licenses/connect-proxy/COPYING
COPY --chmod=0644 hack/images/licenses/connect-proxy-BUILD.txt /usr/share/licenses/connect-proxy/BUILD
COPY --from=tools /out/share/tools.lock /usr/share/patrol-cd/tools.lock
COPY --chmod=0644 LICENSE NOTICE /usr/share/licenses/patrol-cd/
# patrolcd, patrol and the component-name links to patrolcd.
COPY --from=build /out/bin/ /usr/local/bin/

ENV HOME=/home/patrol \
    USER=patrol \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TINI_SUBREAPER=1
WORKDIR /home/patrol
# Numeric, so kubelet can enforce runAsNonRoot without a passwd lookup.
USER 999:999
# server 8080 (API/UI) + 8083, repo-server 8081 + 8084, controller 8082 + 8090 (tree API),
# appset-controller 8080 + 8081, commit-server 8086 + 8087, notifications-controller 9001.
EXPOSE 8080 8081 8082 8083 8084 8086 8087 8090 9001
# Kubernetes probes (docs/architecture/components/wiring.md), not a Docker health check.
HEALTHCHECK NONE
STOPSIGNAL SIGTERM
# tini is PID 1: it forwards SIGTERM to the component (graceful drain, wiring.md "Shutdown") and
# reaps orphaned tool processes (git, ssh, gpg, helm, kustomize, plugins). Pods select the
# component through args, e.g. args: [patrolcd, server] or args: [/usr/local/bin/patrol-repo-server].
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["patrolcd", "--help"]

ARG VERSION=devel
ARG COMMIT=
ARG DATE=
# licenses: the software this Dockerfile adds itself (Patrol CD, Helm, Kustomize: Apache-2.0;
# git-lfs: MIT; connect-proxy: GPL-2.0-or-later), plus LicenseRef-Bundled-Dependencies for what it
# does not enumerate: the Alpine packages and the Go standard library, Go modules and npm packages
# compiled into the binaries (docs/guides/images.md "Licenses").
LABEL org.opencontainers.image.title="patrol-cd" \
      org.opencontainers.image.description="Patrol CD: Kubernetes-native continuous delivery (control plane, CLI and pinned tools)" \
      org.opencontainers.image.source="https://github.com/juliosaraiva/patrol-cd" \
      org.opencontainers.image.url="https://github.com/juliosaraiva/patrol-cd" \
      org.opencontainers.image.documentation="https://github.com/juliosaraiva/patrol-cd/blob/main/docs/guides/images.md" \
      org.opencontainers.image.licenses="Apache-2.0 AND MIT AND GPL-2.0-or-later AND LicenseRef-Bundled-Dependencies" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.base.name="docker.io/library/alpine:3.24.2" \
      org.opencontainers.image.base.digest="sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
