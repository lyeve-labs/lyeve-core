# Formatting, the build-time guarantees, and the targets that run them.
#
# Every lint here reads this repository alone.
# `make preflight` is verify plus a build and a race-clean test run.

.PHONY: lint-go lint-go-release tidy \
        lint-no-plugin-source env-parity lint-core-pure \
        lint-no-raw-tenant-header lint-no-raw-error-text lint-no-raw-tx-placeholder \
        lint-no-test-seams lint-no-typography-in-strings \
        lint-mysql-text-default lint-mysql-inline-references lint-mssql-merge-holdlock \
        lint-mysql-index-guard lint-no-insert-ignore lint-sql-bool-literal lint-validate-tags lint-inert-tests \
        build-dev lint-secrets \
        verify verify-ci preflight

# Go files of the root module. cmd/lyeve is a module of its own, which
# lint-go-release covers.
ROOT_GO_FILES = $$(find . -path ./cmd/lyeve -prune -o -name '*.go' -print)

lint-go: ## Check gofmt, vet and golangci-lint on the root module
	@unformatted=$$(gofmt -l $(ROOT_GO_FILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "$(YELLOW)gofmt needed on:$(RESET)"; echo "$$unformatted"; \
		echo "run: make tidy"; exit 1; \
	fi
	@command -v golangci-lint >/dev/null || (echo "$(YELLOW)golangci-lint not found - install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2$(RESET)" && exit 1)
	go vet ./...
	golangci-lint run ./...

# cmd/lyeve is a module of its own, so no pattern in lint-go expands into it,
# and a green lint-go says nothing about it.
lint-go-release: ## Check gofmt, vet and golangci-lint on cmd/lyeve (needs modules that are not published)
	$(require-release)
	@unformatted=$$(gofmt -l $(RELEASE_DIR)); \
	if [ -n "$$unformatted" ]; then \
		echo "$(YELLOW)gofmt needed on:$(RESET)"; echo "$$unformatted"; exit 1; \
	fi
	@command -v golangci-lint >/dev/null || (echo "$(YELLOW)golangci-lint not found - install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2$(RESET)" && exit 1)
	cd $(RELEASE_DIR) && go vet ./... && golangci-lint run ./...

tidy: ## Tidy Go modules and format Go code
	go mod tidy && gofmt -w .

lint-no-plugin-source: ## Fail if engine source imports a plugin module
	@bash tools/verify-no-plugin-source.sh

env-parity: ## Fail if any os.Getenv in config.go is absent from .env.example
	@bash tools/check-env-parity.sh

lint-core-pure: ## Fail if pkg/core (outside enginehost/) imports internal/*
	@bash tools/verify-core-pure.sh

lint-no-raw-tenant-header: ## Fail if any handler reads X-Tenant-ID header directly
	@bash tools/lint-no-raw-tenant-header.sh

lint-no-raw-error-text: ## Fail if any handler leaks err.Error() to HTTP responses
	@bash tools/lint-no-raw-error-text.sh

lint-no-raw-tx-placeholder: ## Fail if a raw *sql.Tx statement hard-codes a $$N placeholder
	@bash tools/lint-no-raw-tx-placeholder.sh

lint-no-typography-in-strings: ## Fail if a Go string literal carries an em dash, curly quote, ellipsis or bullet
	@bash tools/lint-no-typography-in-strings.sh

lint-no-test-seams: ## Fail if this repo can be made to behave differently for a test
	@bash tools/lint-no-test-seams.sh

lint-mssql-merge-holdlock: ## Fail if an MSSQL MERGE upsert does not take a range lock
	@bash tools/lint-mssql-merge-holdlock.sh

lint-mysql-text-default: ## Fail if a MySQL migration puts a literal DEFAULT on a TEXT/BLOB/JSON column
	@bash tools/lint-mysql-text-default.sh

lint-mysql-inline-references: ## Fail if a MySQL migration declares a foreign key on the column
	@bash tools/lint-mysql-inline-references.sh

lint-mysql-index-guard: ## Fail if a MySQL migration creates an index that cannot be created twice
	@bash tools/lint-mysql-index-guard.sh

lint-inert-tests: ## Fail if a test function contains no assertion
	@bash tools/lint-inert-tests.sh

lint-validate-tags: ## Fail if a validate tag sizes a field type that cannot carry a size
	@bash tools/lint-validate-tags.sh

# MySQL's IGNORE downgrades every error to a warning, so a rejected write
# reports zero affected rows, the same answer a duplicate gives.
lint-no-insert-ignore: ## Fail if any Go source emits INSERT IGNORE
	@bash tools/lint-no-insert-ignore.sh

# SQL Server has no TRUE or FALSE keyword, so a boolean literal there is read
# as a column name and the statement fails at run time on that dialect alone.
lint-sql-bool-literal: ## Fail if a boolean literal appears inside SQL text
	@bash tools/lint-sql-bool-literal.sh

# ./... stops at the cmd/lyeve module boundary, so this compiles the root
# module alone.
build-dev: ## Fail if the dev build tag does not compile
	@GOTOOLCHAIN=local go build -tags dev ./...

lint-secrets: ## Fail if a credential reaches the tree or its history
	@command -v gitleaks >/dev/null || (echo "$(YELLOW)gitleaks not found - install: go install github.com/zricethezav/gitleaks/v8@latest$(RESET)" && exit 1)
	@gitleaks detect --source . --config .gitleaks.toml --no-banner --redact --exit-code 1
	@echo "$(GREEN)OK: no secrets in the tree or its history.$(RESET)"

# The build-time guarantees. Every one reads this repository's source, so each
# means the same thing in CI as it does here.
VERIFY_CORE_LINTS := lint-no-plugin-source env-parity lint-core-pure \
                     lint-no-raw-tenant-header lint-no-raw-error-text \
                     lint-no-raw-tx-placeholder lint-mysql-text-default \
                     lint-mysql-inline-references lint-mssql-merge-holdlock \
                     lint-mysql-index-guard lint-no-test-seams lint-no-typography-in-strings \
                     lint-no-insert-ignore lint-sql-bool-literal \
                     lint-validate-tags lint-inert-tests

# What CI enforces: everything verify runs except the secret scan. That shells
# out to whichever gitleaks is on PATH, so the workflow runs it itself against
# a pinned version rather than trusting the runner image.
verify-ci: $(VERIFY_CORE_LINTS) build-dev ## The build-time guarantees CI enforces (verify minus the secret scan)

verify: verify-ci lint-secrets ## Run every build-time guarantee, including the secret scan

preflight: ## Run the full local Go/No-Go gate (build + vet + race tests + verify)
	@echo "$(BOLD)Preflight$(RESET)"
	@echo ""
	@echo "$(CYAN)1/3 Build + vet$(RESET)"
	go build ./... && go vet ./...
	@echo ""
	@echo "$(CYAN)2/3 Race-clean tests$(RESET)"
	go test -race -count=1 ./...
	@echo ""
	@echo "$(CYAN)3/3 Lint guarantees$(RESET)"
	$(MAKE) verify
	@echo ""
	@echo "$(GREEN)Preflight checks complete.$(RESET)"
