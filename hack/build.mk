# Binary and web UI build targets, included by the root Makefile (after LOCALBIN and GO are
# defined). The web UI is built with pnpm and copied into internal/ui/dist, which
# internal/ui embeds; without that copy `go build` embeds a "UI not built" page.

UI_DIR ?= ui
UI_EMBED_DIR := internal/ui/dist
VERSION_PKG := github.com/juliosaraiva/patrol-cd/internal/server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
GIT_TAG ?= $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_TREE_STATE ?= $(shell if git diff --quiet HEAD 2>/dev/null; then echo clean; else echo dirty; fi)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CGO_ENABLED ?= 0
GO_BUILD_FLAGS ?= -trimpath
GO_LDFLAGS ?= -s -w \
	-X $(VERSION_PKG).version=$(VERSION) \
	-X $(VERSION_PKG).gitCommit=$(GIT_COMMIT) \
	-X $(VERSION_PKG).gitTag=$(GIT_TAG) \
	-X $(VERSION_PKG).gitTreeState=$(GIT_TREE_STATE) \
	-X $(VERSION_PKG).buildDate=$(BUILD_DATE)

##@ Binaries

.PHONY: ui-build
ui-build: ## Build the web UI (pnpm) and copy ui/dist into internal/ui/dist for embedding.
	pnpm -C $(UI_DIR) install --frozen-lockfile
	pnpm -C $(UI_DIR) build
	find $(UI_EMBED_DIR) -mindepth 1 ! -name .keep ! -name .gitignore -exec rm -rf {} +
	cp -R $(UI_DIR)/dist/. $(UI_EMBED_DIR)/

.PHONY: ui-clean
ui-clean: ## Remove the copied web UI (go build then embeds the "UI not built" page).
	find $(UI_EMBED_DIR) -mindepth 1 ! -name .keep ! -name .gitignore -exec rm -rf {} +

.PHONY: build-bin
build-bin: | $(LOCALBIN) ## Build bin/patrolcd and bin/patrol with version information (run ui-build first to embed the UI).
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o $(LOCALBIN)/patrolcd ./cmd/patrolcd
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o $(LOCALBIN)/patrol ./cmd/patrol

.PHONY: build-all
build-all: ui-build build-bin ## Build the web UI and both binaries.
