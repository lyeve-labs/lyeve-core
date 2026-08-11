# Plugin version pinning, vulnerability scanning and SBOM generation: the
# things the release build depends on being true.

.PHONY: lock-plugin-versions verify-plugin-versions build-pinned-set scan-deps-go \
        sbom sbom-go sbom-docker

lock-plugin-versions: ## Pin every module cmd/lyeve imports to its default branch in PLUGIN_VERSIONS.txt
	@bash tools/lock-plugin-versions.sh

verify-plugin-versions: ## Fail if PLUGIN_VERSIONS.txt pins a commit the release build cannot clone
	@bash tools/verify-plugin-versions.sh

build-pinned-set: ## Build cmd/lyeve from the commits PLUGIN_VERSIONS.txt pins and name every repo that fails
	@bash tools/build-pinned-set.sh $(PINNED_SET_ARGS)

scan-deps-go: ## Run govulncheck against all Go dependencies
	@echo "$(BOLD)govulncheck - Go vulnerability scan$(RESET)"
	@command -v govulncheck >/dev/null || (echo "$(YELLOW)govulncheck not found - install: go install golang.org/x/vuln/cmd/govulncheck@latest$(RESET)" && exit 1)
	govulncheck -show verbose ./...
	@echo "$(GREEN)govulncheck complete.$(RESET)"

# SBOM
# Written inside the repository, under a gitignored directory.
SBOM_DIR   ?= sboms
SBOM_STAMP  = $(shell date -u +%Y%m%d)

sbom: sbom-go sbom-docker ## Generate SBOM artifacts

sbom-go: ## Generate CycloneDX SBOM for the Go module (via cyclonedx-gomod)
	@echo "$(BOLD)CycloneDX SBOM - Go module$(RESET)"
	@command -v cyclonedx-gomod >/dev/null || (echo "$(YELLOW)cyclonedx-gomod not found - install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest$(RESET)" && exit 1)
	@mkdir -p $(SBOM_DIR)
	cyclonedx-gomod mod -licenses -json -output $(SBOM_DIR)/go-module-$(SBOM_STAMP).cdx.json .
	@echo "$(GREEN)SBOM written to $(SBOM_DIR)/go-module-$(SBOM_STAMP).cdx.json$(RESET)"

sbom-docker: ## Generate an SBOM for the engine image (requires Syft)
	@echo "$(BOLD)Syft SBOM - $(IMAGE):latest$(RESET)"
	@command -v syft >/dev/null || (echo "$(YELLOW)syft not found - install: curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s - -b /usr/local/bin$(RESET)" && exit 1)
	@mkdir -p $(SBOM_DIR)
	@syft $(IMAGE):latest -o cyclonedx-json > $(SBOM_DIR)/$$(echo $(IMAGE) | tr '/' '-')-$(SBOM_STAMP).cdx.json 2>/dev/null \
		|| { echo "$(YELLOW)$(IMAGE):latest not found - build or pull the image first$(RESET)"; exit 1; }
	@echo "$(GREEN)SBOM written to $(SBOM_DIR)/$(RESET)"
