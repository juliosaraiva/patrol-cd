# Container image targets, included by the root Makefile (`include hack/image.mk`, after
# hack/build.mk). Guide: docs/guides/images.md. Nothing here pushes unless PUSH=1 and an explicit
# REGISTRY are both given.
#
#   make image                                   # patrol-cd/patrol-cd:dev for the daemon's platform
#   make image PLATFORM=linux/arm64 TAG=test     # another platform (QEMU/binfmt for the two
#                                                # TARGETPLATFORM stages when foreign)
#   make image-multiarch                         # linux/amd64+linux/arm64 OCI archive in dist/image
#   make image-multiarch PUSH=1 REGISTRY=registry.example.com/team TAG=v0.1.0
#   make image-verify-pins image-rootfs-check     # no container runtime needed

DOCKER ?= docker
# Repository (without registry) and tag of the built image.
IMAGE ?= patrol-cd/patrol-cd
TAG ?= dev
# Single-platform build target, e.g. linux/arm64 (empty: the daemon's platform).
PLATFORM ?=
# Platforms of image-multiarch.
PLATFORMS ?= linux/amd64,linux/arm64
# Registry (host[:port][/namespace]) for image-multiarch PUSH=1; there is no default.
REGISTRY ?=
PUSH ?=
# Extra flags for docker build / buildx build (e.g. --no-cache, --builder NAME).
IMAGE_BUILD_FLAGS ?=
# Output directory of image-multiarch (without PUSH) and image-sbom.
IMAGE_DIST ?= $(CURDIR)/dist/image
# Architectures checked by image-rootfs-check.
ROOTFS_ARCHES ?= amd64 arm64
KIND_LOAD_CLUSTER ?= all

# Build information. VERSION, GIT_TAG and GIT_TREE_STATE come from hack/build.mk when it is
# included first; the defaults below are the same expressions. DATE is the commit time, not the
# wall clock, so rebuilding a commit gives the same labels (SOURCE_DATE_EPOCH also sets the image
# timestamps in BuildKit).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
GIT_TAG ?= $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_TREE_STATE ?= $(shell if git diff --quiet HEAD 2>/dev/null; then echo clean; else echo dirty; fi)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
DATE ?= $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)

# The image is stamped with IMAGE_VERSION and IMAGE_GIT_TREE_STATE. `git describe --dirty` and
# `git diff` see only tracked files, but the build context (the .dockerignore allow-list) also
# takes untracked ones, so an untracked file under an allow-listed path makes the image's tree
# state dirty and appends -dirty to its version. Counted: what `git ls-files --others` lists with
# the repository's .gitignore files (personal excludes hide nothing here), plus ignored
# kubeconfig*.go sources, which .dockerignore re-includes; .dockerignore drops every other ignored
# file. Untracked tests count too, although .dockerignore drops them. VERSION or GIT_TREE_STATE
# given on the command line or in the environment are used unchanged.
IMAGE_CONTEXT_PATHS := Dockerfile .dockerignore go.mod go.sum api cmd internal pkg ui hack/images LICENSE NOTICE
IMAGE_UNTRACKED = $(shell { git ls-files --others --exclude-per-directory=.gitignore -- $(IMAGE_CONTEXT_PATHS); \
	git ls-files --others --ignored --exclude-per-directory=.gitignore -- \
		$(foreach d,api cmd internal pkg,':(glob)$(d)/**/kubeconfig*.go'); } 2>/dev/null | head -n 1)
image_given = $(filter command environment,$(firstword $(origin $(1))))
IMAGE_GIT_TREE_STATE ?= $(if $(call image_given,GIT_TREE_STATE),$(GIT_TREE_STATE),$(if $(IMAGE_UNTRACKED),dirty,$(GIT_TREE_STATE)))
IMAGE_VERSION ?= $(if $(call image_given,VERSION),$(VERSION),$(if $(and $(IMAGE_UNTRACKED),$(filter-out %-dirty,$(VERSION))),$(VERSION)-dirty,$(VERSION)))

IMAGE_BUILD_ARGS = \
	--build-arg VERSION=$(IMAGE_VERSION) \
	--build-arg COMMIT=$(COMMIT) \
	--build-arg GIT_TAG=$(GIT_TAG) \
	--build-arg GIT_TREE_STATE=$(IMAGE_GIT_TREE_STATE) \
	--build-arg DATE=$(DATE) \
	--build-arg SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH)

##@ Image

.PHONY: image
image: ## Build the image locally as IMAGE:TAG (default patrol-cd/patrol-cd:dev); honors PLATFORM, VERSION, COMMIT, DATE.
	$(DOCKER) build --file Dockerfile --tag $(IMAGE):$(TAG) $(if $(PLATFORM),--platform $(PLATFORM)) \
		$(IMAGE_BUILD_ARGS) $(IMAGE_BUILD_FLAGS) .

# Without PUSH=1 the multi-platform result is written as an OCI archive (no registry contact);
# with PUSH=1 it is pushed to $(REGISTRY)/$(IMAGE):$(TAG) with SBOM and provenance attestations.
# The default "docker" buildx driver needs Docker's containerd image store for multi-platform
# builds; otherwise create a builder first: docker buildx create --name patrol --use.
.PHONY: image-multiarch
image-multiarch: ## Build PLATFORMS (amd64+arm64) to an OCI archive in dist/image; push only with PUSH=1 REGISTRY=host[/ns].
	@set -euo pipefail; \
	if [[ "$(PUSH)" == 1 ]]; then \
		[[ -n "$(REGISTRY)" ]] || { echo "image-multiarch: PUSH=1 needs an explicit REGISTRY (host[:port][/namespace])" >&2; exit 1; }; \
		[[ "$(REGISTRY)" =~ ^[A-Za-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)*$$ ]] || { echo "image-multiarch: invalid REGISTRY '$(REGISTRY)'" >&2; exit 1; }; \
		ref="$(REGISTRY)/$(IMAGE):$(TAG)"; \
		output=(--push --sbom=true --provenance=mode=max); \
	else \
		[[ -z "$(PUSH)" ]] || { echo "image-multiarch: PUSH must be 1 or empty" >&2; exit 1; }; \
		mkdir -p "$(IMAGE_DIST)"; \
		ref="$(IMAGE):$(TAG)"; \
		dest="$(IMAGE_DIST)/patrol-cd_$(TAG).oci.tar"; \
		output=(--output "type=oci,dest=$$dest,rewrite-timestamp=true" --sbom=true --provenance=mode=max); \
	fi; \
	echo "image-multiarch: $$ref for $(PLATFORMS)"; \
	$(DOCKER) buildx build --file Dockerfile --platform $(PLATFORMS) --tag "$$ref" \
		$(IMAGE_BUILD_ARGS) $(IMAGE_BUILD_FLAGS) "$${output[@]}" .

.PHONY: image-check
image-check: ## Run BuildKit's Dockerfile checks (docker buildx build --check; needs a daemon).
	$(DOCKER) buildx build --check --file Dockerfile $(IMAGE_BUILD_FLAGS) .

.PHONY: image-verify-pins
image-verify-pins: ## Re-download and verify every image pin (tools, base digests, Alpine packages).
	hack/images/verify-pins.sh $(VERIFY_PINS_FLAGS)

.PHONY: image-rootfs-check
image-rootfs-check: ## Assemble the runtime filesystem without Docker and check it (ROOTFS_ARCHES).
	@set -e; for arch in $(ROOTFS_ARCHES); do hack/images/rootfs-check.sh --arch $$arch; done

# SBOM of the runtime image (SPDX JSON) from BuildKit's SBOM generator: the image is exported to a
# temporary directory together with its attestations and only the SBOM is kept.
.PHONY: image-sbom
image-sbom: ## Write the image's SPDX SBOM (BuildKit) for PLATFORM to dist/image/sbom (needs a daemon).
	@set -euo pipefail; \
	platform="$(PLATFORM)"; \
	[[ -n "$$platform" ]] || platform=$$($(DOCKER) version --format '{{.Server.Os}}/{{.Server.Arch}}'); \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(DOCKER) buildx build --file Dockerfile --platform "$$platform" $(IMAGE_BUILD_ARGS) $(IMAGE_BUILD_FLAGS) \
		--sbom=true --output "type=local,dest=$$tmp" .; \
	mkdir -p "$(IMAGE_DIST)/sbom"; \
	found=0; \
	for f in "$$tmp"/sbom*.spdx.json; do \
		[[ -f "$$f" ]] || continue; \
		out="$(IMAGE_DIST)/sbom/patrol-cd_$(TAG)_$${platform//\//-}_$$(basename "$$f")"; \
		cp "$$f" "$$out"; echo "image-sbom: $$out"; found=1; \
	done; \
	[[ $$found -eq 1 ]] || { echo "image-sbom: BuildKit wrote no SBOM file" >&2; exit 1; }

.PHONY: image-load-kind
image-load-kind: ## Load IMAGE:TAG into the patrol-* kind clusters (hack/kind/load-image.sh).
	hack/kind/load-image.sh --cluster $(KIND_LOAD_CLUSTER) $(IMAGE):$(TAG)
