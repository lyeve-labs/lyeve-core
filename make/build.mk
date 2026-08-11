# Toolchain setup and the two binaries.

.PHONY: setup deps deps-go build build-go build-kernel build-release

setup: ## Install all tool versions via mise + install all deps
	MISE_LOG_LEVEL=error mise install
	$(MAKE) deps

deps: deps-go ## Install Go dependencies

deps-go: ## Download Go module dependencies
	go mod tidy

build: build-go ## Build the engine

build-go: ## Compile every package in the module
	go build ./...

# The engine with no plugin compiled in and no license verifier linked.
# GOWORK=off resolves it from this module's go.mod alone, so a dependency that
# go.mod does not declare fails the build.
KERNEL_LDFLAGS := -s -w \
	-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

build-kernel: ## Build bin/lyeve-core, the engine alone, from this module
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags "$(KERNEL_LDFLAGS)" -o bin/lyeve-core ./cmd/lyeve-core
	@echo "OK: bin/lyeve-core built ($(VERSION))"

build-release: ## Build cmd/lyeve, the release binary (needs modules that are not published)
	$(require-release)
	cd $(RELEASE_DIR) && go build -o ../../bin/lyeve .
	@echo "OK: bin/lyeve built"
