# Patrol CD build entry points. Tools are pinned in tools/go.mod and installed into ./bin.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
KUSTOMIZE ?= $(LOCALBIN)/kustomize
TOOLS_MODFILE := $(CURDIR)/tools/go.mod

ENVTEST_K8S_VERSION ?= 1.36.0
ENVTEST_DIR ?= $(LOCALBIN)/envtest

CRD_DIR := deploy/crds
API_PATHS := ./api/...
BOILERPLATE := hack/boilerplate.go.txt
# Generated files compared by verify-generated.
GENERATED := api/v1alpha1/zz_generated.deepcopy.go $(CRD_DIR)

GO ?= go
GO_TEST_FLAGS ?= -race -count=1
# Packages for vet/lint/test, e.g. `make test PKGS=./api/...`.
PKGS ?= ./...

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Tools

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

$(CONTROLLER_GEN): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install -modfile=$(TOOLS_MODFILE) sigs.k8s.io/controller-tools/cmd/controller-gen

$(SETUP_ENVTEST): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install -modfile=$(TOOLS_MODFILE) sigs.k8s.io/controller-runtime/tools/setup-envtest

$(KUSTOMIZE): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install -modfile=$(TOOLS_MODFILE) sigs.k8s.io/kustomize/kustomize/v5

.PHONY: tools
tools: $(CONTROLLER_GEN) $(SETUP_ENVTEST) $(KUSTOMIZE) ## Install pinned tools into ./bin (skips installed ones).

# Absolute path of the envtest binaries. Uses installed binaries when present (offline), otherwise
# downloads them.
.PHONY: envtest
envtest: $(SETUP_ENVTEST) ## Print the envtest assets directory (KUBEBUILDER_ASSETS).
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -i -p path 2>/dev/null \
		|| $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path

##@ Generation

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy code and CRD manifests (RBAC generation will be added here).
	$(CONTROLLER_GEN) object:headerFile="$(BOILERPLATE)" paths="$(API_PATHS)"
	$(MAKE) --no-print-directory manifests

# The ApplicationSet CRD embeds the Application spec in every generator template. With field
# descriptions it is about 1 MB as JSON, uncomfortably close to etcd's 1.5 MiB object limit, so, as
# upstream does, its descriptions are dropped: it is generated a second time with maxDescLen=0 and that
# copy replaces the first. deploy/crds also holds a hand-written admission policy that controller-gen
# leaves untouched.
.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD manifests into deploy/crds.
	$(CONTROLLER_GEN) crd paths="$(API_PATHS)" output:crd:artifacts:config=$(CRD_DIR)
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		$(CONTROLLER_GEN) crd:maxDescLen=0 paths="$(API_PATHS)" output:crd:artifacts:config="$$tmp"; \
		cp "$$tmp/cd.patrol.example_applicationsets.yaml" $(CRD_DIR)/cd.patrol.example_applicationsets.yaml

# Works without git history: snapshots the generated files, regenerates, and diffs.
.PHONY: verify-generated
verify-generated: ## Fail if generated files are stale.
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		for f in $(GENERATED); do \
			mkdir -p "$$tmp/before/$$(dirname $$f)"; \
			if [ -e "$$f" ]; then cp -R "$$f" "$$tmp/before/$$f"; fi; \
		done; \
		$(MAKE) --no-print-directory generate >/dev/null; \
		status=0; \
		for f in $(GENERATED); do \
			if [ -e "$$tmp/before/$$f" ]; then \
				diff -ru "$$tmp/before/$$f" "$$f" || status=1; \
			else \
				echo "missing before generate: $$f"; status=1; \
			fi; \
		done; \
		if [ $$status -ne 0 ]; then echo "generated files are stale: run 'make generate' and commit the result"; exit 1; fi; \
		echo "generated files are up to date"

##@ Verification

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet $(PKGS)

.PHONY: lint
lint: vet ## Run go vet and golangci-lint (when installed); UI lint when ui/ exists.
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run $(PKGS); \
		else echo "golangci-lint not installed; skipped"; fi
	@if [ -f ui/package.json ]; then pnpm -C ui typecheck && pnpm -C ui lint; fi

# PATROL_REQUIRE_ENVTEST makes envtest-backed tests fail instead of skip when KUBEBUILDER_ASSETS is
# missing, so `make test` cannot pass without running them.
.PHONY: test
test: $(SETUP_ENVTEST) ## Run Go tests, including envtest-backed admission tests.
	KUBEBUILDER_ASSETS="$$($(MAKE) --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 \
		$(GO) test $(GO_TEST_FLAGS) $(PKGS)

##@ Build

.PHONY: build
build: ## Build all packages; binaries under cmd/ are written to ./bin.
	$(GO) build ./...
	@if [ -d cmd ]; then $(GO) build -o $(LOCALBIN)/ ./cmd/...; fi
