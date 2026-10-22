# Release targets, included by the root Makefile (`include hack/release.mk`, after hack/build.mk,
# hack/image.mk and hack/deploy.mk). Guide: docs/guides/releasing.md. The release workflows
# (.github/workflows/release-pr.yml and release.yml) call these targets; nothing here pushes, tags
# or publishes.
#
#   make release-next                       # the next version from the commits since the last tag
#   make release-preview                    # the CHANGELOG.md section it would add
#   make release-prepare [RELEASE_VERSION=X.Y.Z]
#                                           # CHANGELOG.md + Chart.yaml, then the chart's generated
#                                           # files and the static manifests (needs helm 4, kustomize)
#   make release-lint-commits [RELEASE_RANGE=origin/main..HEAD]
#   make release-artifacts RELEASE_VERSION=X.Y.Z RELEASE_REGISTRY=ghcr.io/owner
#                                           # dist/release: CLI binaries, install manifests, chart
#                                           # archive, notes.md, SHA256SUMS

RELEASE_VERSION ?=
RELEASE_RANGE ?= origin/main..HEAD
# Registry (host[:port][/namespace]) the release image is pushed to; the manifests and the chart in
# dist/release point at $(RELEASE_REGISTRY)/patrol-cd/patrol-cd.
RELEASE_REGISTRY ?=
RELEASE_DIST ?= $(CURDIR)/dist/release
# GOOS/GOARCH pairs of the patrol CLI binaries (patrolcd ships in the image only).
RELEASE_CLI_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
RELEASE = $(GO) run ./hack/release

##@ Release

.PHONY: release-next
release-next: ## Print the next release version (empty: nothing to release, or a release is pending).
	@$(RELEASE) next $(if $(RELEASE_VERSION),-version $(RELEASE_VERSION))

.PHONY: release-preview
release-preview: ## Print the CHANGELOG.md section the next release would add.
	@$(RELEASE) preview $(if $(RELEASE_VERSION),-version $(RELEASE_VERSION))

.PHONY: release-prepare
release-prepare: $(KUSTOMIZE) ## Write the next release into CHANGELOG.md and Chart.yaml and regenerate the chart docs and manifests.
	$(RELEASE) prepare $(if $(RELEASE_VERSION),-version $(RELEASE_VERSION))
	$(MAKE) --no-print-directory helm-docs manifests-install

.PHONY: release-lint-commits
release-lint-commits: ## Fail when a non-merge commit in RELEASE_RANGE is not a conventional commit.
	$(RELEASE) lint $(RELEASE_RANGE)

# The binaries are stamped as the tag (the tag itself is created after the artifacts, by the
# release workflow), so VERSION and GIT_TAG are set here instead of read from git.
.PHONY: release-artifacts
release-artifacts: ## Build dist/release for RELEASE_VERSION: CLI binaries, install manifests, chart, notes, SHA256SUMS.
	@[[ -n "$(RELEASE_VERSION)" ]] || { echo "release-artifacts: RELEASE_VERSION is required" >&2; exit 1; }
	@[[ -n "$(RELEASE_REGISTRY)" ]] || { echo "release-artifacts: RELEASE_REGISTRY is required" >&2; exit 1; }
	rm -rf "$(RELEASE_DIST)"
	mkdir -p "$(RELEASE_DIST)"
	$(RELEASE) stage -version $(RELEASE_VERSION) -registry $(RELEASE_REGISTRY) -out "$(RELEASE_DIST)/stage"
	$(RELEASE) notes -version $(RELEASE_VERSION) >"$(RELEASE_DIST)/notes.md"
	@set -euo pipefail; for p in $(RELEASE_CLI_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [[ $$os == windows ]] && ext=.exe; \
		echo "release-artifacts: patrol $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) \
			-ldflags "$(GO_LDFLAGS)" -o "$(RELEASE_DIST)/patrol-$$os-$$arch$$ext" ./cmd/patrol; \
	done
	mv "$(RELEASE_DIST)"/stage/*.yaml "$(RELEASE_DIST)/"
	$(HELM) package "$(RELEASE_DIST)/stage/chart" --destination "$(RELEASE_DIST)"
	rm -rf "$(RELEASE_DIST)/stage"
	cd "$(RELEASE_DIST)" && find . -maxdepth 1 -type f ! -name notes.md ! -name SHA256SUMS -print | sed 's|^\./||' | LC_ALL=C sort \
		| xargs $$(command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256) >SHA256SUMS
	@cat "$(RELEASE_DIST)/SHA256SUMS"

release-artifacts: VERSION = v$(RELEASE_VERSION)
release-artifacts: GIT_TAG = v$(RELEASE_VERSION)
