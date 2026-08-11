# Settings every other file relies on: colors, build metadata, shared paths,
# and the two entry points for finding your way around (help and print-%).

RESET  := \033[0m
BOLD   := \033[1m
GREEN  := \033[32m
YELLOW := \033[33m
CYAN   := \033[36m

# The release binary: a nested module under cmd/lyeve with its own go.mod. It
# needs modules that are not published, so it does not resolve from a clone of
# this repository. RELEASE_LIST_ERR says why, and RELEASE_RESOLVES is empty
# when it does not resolve.
RELEASE_DIR      ?= cmd/lyeve
RELEASE_LIST_ERR  = $(shell go list ./$(RELEASE_DIR) 2>&1 >/dev/null)
RELEASE_RESOLVES  = $(if $(RELEASE_LIST_ERR),,yes)

# The first line of every target that needs cmd/lyeve.
define require-release
@[ -n "$(RELEASE_RESOLVES)" ] || { \
	echo "$(RELEASE_LIST_ERR)"; \
	echo "$(YELLOW)$@ needs cmd/lyeve, the release binary, which needs modules that are not published.$(RESET)"; \
	echo "A clone of this repository builds and runs the engine alone: make build-kernel, make run-go, make test."; \
	exit 1; }
endef

IMAGE ?= ghcr.io/lyeve-labs/lyeve-core

# Baked into the binary by the release ldflags, and overridable in CI.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: help

help: ## Show this help, grouped by the file each target lives in
	@printf '$(BOLD)LyEve Core$(RESET) $(VERSION)\n\n'
	@for f in $(MAKEFILE_LIST); do \
		rules=$$(grep -hE '^[a-zA-Z_%-]+:.*?## .*$$' "$$f" || true); \
		[ -n "$$rules" ] || continue; \
		printf '$(BOLD)%s$(RESET)\n' "$$(basename "$$f" .mk)"; \
		printf '%s\n' "$$rules" | \
			awk 'BEGIN {FS = ":.*?## "}; {printf "  $(CYAN)%-24s$(RESET) %s\n", $$1, $$2}'; \
		printf '\n'; \
	done

# Not phony: GNU make ignores pattern rules in .PHONY.
print-%: ## Show what a variable expanded to (make print-VERSION)
	@echo '$* = $($*)'
