# The Claude Code SDLC (docs/guides/claude-code-sdlc.md), included by the root Makefile
# (`include hack/sdlc.mk`, after hack/ci.mk, whose targets it runs).
#
#   make sdlc-check       the deterministic checks of the branch's change, recorded for HEAD's tree
#   make sdlc-gate        whether HEAD may be pushed (check record and, for code, review record)
#   make codeql           CodeQL of Go, the web UI and the workflows, gated by hack/sdlc/sarifgate
#   make sdlc-git-hooks   use .githooks (the same gate for pushes made outside Claude Code)

##@ SDLC

.PHONY: sdlc-check
sdlc-check: ## Deterministic checks of the branch (CI static checks, tests of what changed, CodeQL); records a pass.
	$(if $(filter-out --no-codeql,$(SDLC_CHECK_FLAGS)),$(error SDLC_CHECK_FLAGS may only be --no-codeql))
	hack/sdlc/check.sh $(SDLC_CHECK_FLAGS)

.PHONY: sdlc-gate
sdlc-gate: ## Whether HEAD may be pushed: passing checks and, unless documentation only, a passing agent review.
	hack/sdlc/gate.sh

.PHONY: codeql
codeql: ## CodeQL (security-extended) of Go, web UI and workflows; set CODEQL_LANGUAGES to narrow it.
	$(if $(filter-out go javascript-typescript actions,$(CODEQL_LANGUAGES)),$(error CODEQL_LANGUAGES takes go, javascript-typescript and actions))
	hack/sdlc/codeql.sh analyze $(CODEQL_LANGUAGES)

.PHONY: codeql-install
codeql-install: ## Download the pinned CodeQL bundle into bin/ (checksum verified).
	hack/sdlc/codeql.sh install

.PHONY: sdlc-git-hooks
sdlc-git-hooks: ## Make git use .githooks (pre-push runs the SDLC gate).
	git config core.hooksPath .githooks

.PHONY: ci-sdlc
ci-sdlc: ## Offline tests of the SDLC gate scripts (the CodeQL result gate is tested by ci-unit).
	@echo "==> ci-sdlc"
	hack/sdlc/test-sdlc.sh
