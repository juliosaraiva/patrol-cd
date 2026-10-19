# Helm chart targets, included by the root Makefile (`include hack/deploy.mk`). Chart:
# deploy/helm/patrol-cd. Guide: docs/guides/installation.md. Nothing here publishes: helm-package
# writes an archive to dist/ only.
#
#   make helm-docs       # regenerate the chart's generated files (README.md, CRD templates,
#                        # parameter table, schema key sets) from values.yaml, deploy/crds and
#                        # internal/settings
#   make helm-verify     # fail when a generated file is stale (chart drift from deploy/crds or
#                        # internal/settings) or a shipped default differs from the code
#   make helm-lint       # helm-verify + `helm lint --strict` of the defaults and every ci/ profile
#                        # + schema/validation negative tests (ci/invalid)
#   make helm-template   # render every profile twice, fail unless both renders are identical,
#                        # write the renders and SHA256SUMS to dist/helm-template
#   make helm-package    # dist/patrol-cd-<version>.tgz (no push)
#   make manifests-install  # regenerate the static install manifests (deploy/manifests) from the
#                           # chart (guide: docs/guides/manifests.md)
#   make verify-manifests   # fail when deploy/manifests is stale (writes nothing)

HELM ?= helm
HELM_CHART ?= deploy/helm/patrol-cd
# Kubernetes version used for capabilities by lint and template (Argo CD v3.5.5 is tested on
# 1.33-1.36).
HELM_KUBE_VERSION ?= 1.36.0
HELM_RELEASE ?= patrol
HELM_NAMESPACE ?= patrol
HELM_DIST ?= $(CURDIR)/dist
HELM_PROFILES = $(sort $(wildcard $(HELM_CHART)/ci/*.yaml))
HELM_INVALID = $(sort $(wildcard $(HELM_CHART)/ci/invalid/*.yaml))

##@ Helm chart

.PHONY: helm-docs
helm-docs: ## Regenerate the chart's generated files (README, CRD templates, parameter table, schema key sets).
	$(GO) run ./hack/helmdocs -chart $(HELM_CHART) -crds $(CRD_DIR)

# The CRD templates are part of helm-docs (one generator, one drift check).
.PHONY: helm-crds
helm-crds: helm-docs ## Copy deploy/crds into the chart's templates/crds (alias of helm-docs).

.PHONY: helm-verify
helm-verify: ## Fail when the chart's generated files are stale or its shipped defaults drift from internal/settings.
	$(GO) run ./hack/helmdocs -chart $(HELM_CHART) -crds $(CRD_DIR) -check

.PHONY: helm-lint
helm-lint: helm-verify ## helm lint --strict of the defaults and every ci/ profile, plus the negative value tests.
	$(HELM) lint --strict $(HELM_CHART) --kube-version $(HELM_KUBE_VERSION)
	@for f in $(HELM_PROFILES); do \
		echo "helm lint --strict -f $$f"; \
		$(HELM) lint --strict $(HELM_CHART) --kube-version $(HELM_KUBE_VERSION) -f "$$f" >/dev/null \
			|| { $(HELM) lint --strict $(HELM_CHART) --kube-version $(HELM_KUBE_VERSION) -f "$$f"; exit 1; }; \
	done
	@status=0; for f in $(HELM_INVALID); do \
		want=$$(sed -n 's/^# expect: //p' "$$f" | head -n 1); \
		if [ -z "$$want" ]; then echo "$$f: missing '# expect: <error substring>' line"; status=1; continue; fi; \
		if out=$$($(HELM) template $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) --kube-version $(HELM_KUBE_VERSION) -f "$$f" 2>&1); then \
			echo "FAIL $$f: rendered, expected an error containing: $$want"; status=1; \
		elif printf '%s' "$$out" | grep -qF -- "$$want"; then \
			echo "ok   $$f (rejected: $$want)"; \
		else \
			echo "FAIL $$f: error does not contain \"$$want\":"; printf '%s\n' "$$out" | tail -n 5; status=1; \
		fi; \
	done; exit $$status

.PHONY: helm-template
helm-template: ## Render every ci/ profile twice into dist/helm-template and fail unless the renders are identical.
	@mkdir -p $(HELM_DIST)/helm-template
	@rm -f $(HELM_DIST)/helm-template/*.yaml $(HELM_DIST)/helm-template/SHA256SUMS
	@for f in $(HELM_PROFILES); do \
		n=$$(basename "$$f" .yaml); \
		for run in 1 2; do \
			$(HELM) template $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) --kube-version $(HELM_KUBE_VERSION) \
				-f "$$f" > "$(HELM_DIST)/helm-template/$$n.run$$run.yaml" || exit 1; \
		done; \
		if ! cmp -s "$(HELM_DIST)/helm-template/$$n.run1.yaml" "$(HELM_DIST)/helm-template/$$n.run2.yaml"; then \
			echo "non-deterministic render: $$f"; exit 1; \
		fi; \
		mv -f "$(HELM_DIST)/helm-template/$$n.run1.yaml" "$(HELM_DIST)/helm-template/$$n.yaml"; \
		rm -f "$(HELM_DIST)/helm-template/$$n.run2.yaml"; \
	done
	@cd $(HELM_DIST)/helm-template && if command -v sha256sum >/dev/null 2>&1; then sha256sum *.yaml; else shasum -a 256 *.yaml; fi > SHA256SUMS
	@cat $(HELM_DIST)/helm-template/SHA256SUMS

.PHONY: helm-package
helm-package: helm-verify ## Package the chart into dist/ (no push).
	@mkdir -p $(HELM_DIST)
	$(HELM) package $(HELM_CHART) --destination $(HELM_DIST)

##@ Static install manifests

# deploy/manifests: install.yaml, namespace-install.yaml, core-install.yaml, ha/*.yaml, the
# *-with-hydrator variants, crds.yaml and their kustomize bases, rendered from the chart with the
# value files in deploy/manifests/profiles (release and namespace "patrol"). The generator needs
# helm 4 and the pinned kustomize (bin/kustomize, `make tools`).
MANIFESTS_DIR ?= deploy/manifests
GENMANIFESTS_FLAGS = -chart $(HELM_CHART) -out $(MANIFESTS_DIR) -helm $(HELM) -kustomize $(KUSTOMIZE) \
	-kube-version $(HELM_KUBE_VERSION)

.PHONY: manifests-install
manifests-install: $(KUSTOMIZE) ## Regenerate the static install manifests in deploy/manifests from the Helm chart.
	$(GO) run ./hack/genmanifests $(GENMANIFESTS_FLAGS)

.PHONY: verify-manifests
verify-manifests: $(KUSTOMIZE) ## Fail when deploy/manifests is stale (renders the chart again; writes nothing).
	$(GO) run ./hack/genmanifests $(GENMANIFESTS_FLAGS) -check
