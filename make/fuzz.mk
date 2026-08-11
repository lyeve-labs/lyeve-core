# Fuzz harnesses. The -short variants are the CI budget. The plain ones run
# twice as long for a deeper local sweep.

.PHONY: fuzz-short fuzz-auth fuzz-schema-short fuzz-schema fuzz-rewrite-short

fuzz-short: ## Run auth fuzz harnesses for 30s each (CI-friendly)
	@echo "$(BOLD)Fuzz: auth/crypto parsers - 30s per target$(RESET)"
	@for target in FuzzJWTParse FuzzDecryptWireFormat FuzzJWKSParse FuzzAPIKeyDecode; do \
		echo "$(CYAN)$$target$(RESET)"; \
		go test -fuzz=$$target -fuzztime=30s ./internal/auth/ || exit 1; \
		echo ""; \
	done
	@echo "$(GREEN)All fuzz targets passed.$(RESET)"

fuzz-auth: ## Run auth fuzz harnesses for 60s each (deeper coverage)
	@echo "$(BOLD)Fuzz: auth/crypto parsers - 60s per target$(RESET)"
	@for target in FuzzJWTParse FuzzDecryptWireFormat FuzzJWKSParse FuzzAPIKeyDecode; do \
		echo "$(CYAN)$$target$(RESET)"; \
		go test -fuzz=$$target -fuzztime=60s ./internal/auth/ || exit 1; \
		echo ""; \
	done
	@echo "$(GREEN)All fuzz targets passed.$(RESET)"

fuzz-schema-short: ## Run schema + slug fuzz harnesses for 30s each (CI-friendly)
	@echo "$(BOLD)Fuzz: schema JSON decode + DDL identifiers - 30s per target$(RESET)"
	@for target in FuzzSchemaJSONDecode FuzzDDLIdentifier; do \
		echo "$(CYAN)$$target$(RESET)"; \
		go test -fuzz=$$target -fuzztime=30s ./internal/schema/ || exit 1; \
		echo ""; \
	done
	@echo "$(BOLD)Fuzz: tenant slug validation - 30s$(RESET)"
	@echo "$(CYAN)FuzzSlugValidate$(RESET)"
	@go test -run='^$$' -fuzz=FuzzSlugValidate -fuzztime=30s ./internal/db/ || exit 1
	@echo "$(GREEN)All schema fuzz targets passed.$(RESET)"

fuzz-schema: ## Run schema + slug fuzz harnesses for 60s each (deeper coverage)
	@echo "$(BOLD)Fuzz: schema JSON decode + DDL identifiers - 60s per target$(RESET)"
	@for target in FuzzSchemaJSONDecode FuzzDDLIdentifier; do \
		echo "$(CYAN)$$target$(RESET)"; \
		go test -fuzz=$$target -fuzztime=60s ./internal/schema/ || exit 1; \
		echo ""; \
	done
	@echo "$(BOLD)Fuzz: tenant slug validation - 60s$(RESET)"
	@echo "$(CYAN)FuzzSlugValidate$(RESET)"
	@go test -run='^$$' -fuzz=FuzzSlugValidate -fuzztime=60s ./internal/db/ || exit 1
	@echo "$(GREEN)All schema fuzz targets passed.$(RESET)"

fuzz-rewrite-short: ## Run SQL placeholder rewrite fuzz for 30s (CI-friendly)
	@echo "$(BOLD)Fuzz: SQL placeholder rewrite - 30s$(RESET)"
	@echo "$(CYAN)FuzzRewritePlaceholders$(RESET)"
	@go test -run='^$$' -fuzz=FuzzRewritePlaceholders -fuzztime=30s ./internal/db/ || exit 1
	@echo "$(GREEN)Rewrite fuzz target passed.$(RESET)"
