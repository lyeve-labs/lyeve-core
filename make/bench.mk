# Go benchmarks, their baseline/compare workflow, and the k6 load suite.

.PHONY: bench bench-auth bench-db bench-middleware bench-pool \
        bench-baseline bench-compare bench-perf-budget test-load

BENCH_PKGS := ./internal/auth/ ./internal/db/ ./internal/middleware/ ./internal/jsonpool/

bench: ## Run all benchmarks (skip integration tests)
	go test -run='^$$' -bench=. -benchmem -count=1 $(BENCH_PKGS)

bench-auth: ## Run auth benchmarks only
	go test -run='^$$' -bench=. -benchmem -count=1 ./internal/auth/

bench-db: ## Run SQL rewrite benchmarks only
	go test -run='^$$' -bench=. -benchmem -count=1 ./internal/db/

bench-middleware: ## Run HTTP middleware benchmarks only
	go test -run='^$$' -bench=. -benchmem -count=1 ./internal/middleware/

bench-pool: ## Run buffer pool benchmarks only
	go test -run='^$$' -bench=. -benchmem -count=1 ./internal/jsonpool/

bench-baseline: ## Run benchmarks 5x and save as baseline (creates bench-results/<sha>-<ts>.txt)
	@mkdir -p bench-results && \
	SHA=$$(git rev-parse --short HEAD) && \
	TS=$$(date -u +%Y%m%d-%H%M%S) && \
	OUT="bench-results/$$SHA-$$TS.txt" && \
	{ \
		echo "# LyEve Core Benchmarks"; \
		echo "# commit: $$(git rev-parse HEAD)"; \
		echo "# date:   $$(date -u -Iseconds)"; \
		echo ""; \
		for pkg in $(BENCH_PKGS); do \
			echo "=== $$pkg ==="; \
			go test -run='^$$' -bench=. -benchmem -count=5 "$$pkg"; \
			echo ""; \
		done; \
	} > "$$OUT" 2>&1 && \
	ln -sf "$$(basename "$$OUT")" bench-results/latest.txt && \
	echo "benchmarks saved: $$OUT ($$(wc -l < "$$OUT") lines)"

# Ensure benchstat is installed (auto-installs if missing)
BENCHSTAT := $(shell go env GOPATH)/bin/benchstat
$(BENCHSTAT):
	go install golang.org/x/perf/cmd/benchstat@latest

bench-compare: $(BENCHSTAT) ## Compare latest benchmarks against baseline (requires benchstat)
	@LATEST=$$(readlink -f bench-results/latest.txt 2>/dev/null || true); \
	if [ -z "$$LATEST" ] || [ ! -f "$$LATEST" ]; then \
		echo "No baseline - run 'make bench-baseline' first"; \
		exit 1; \
	fi; \
	PREV=$$(ls -t bench-results/*.txt 2>/dev/null | grep -v latest | head -1 || true); \
	if [ -z "$$PREV" ]; then \
		echo "=== Latest run ==="; \
		benchstat "$$LATEST"; \
	elif [ "$$PREV" = "$$LATEST" ]; then \
		echo "Only one run available - run again to compare"; \
		benchstat "$$LATEST"; \
	else \
		echo "=== benchstat: $$(basename "$$PREV") vs $$(basename "$$LATEST") ==="; \
		benchstat "$$PREV" "$$LATEST"; \
	fi

bench-perf-budget: ## Run endpoint performance budget tests (requires Docker)
	go test ./internal/api/ -run TestEndpointPerformanceBudget -count=1 -timeout 600s -v

# Load tests (k6)
K6 := $(shell command -v k6 2>/dev/null || echo k6)

test-load: ## Run k6 load tests (requires a running CMS: make run-go-all)
	@command -v k6 >/dev/null 2>&1 || { echo "$(YELLOW)k6 not found - install: https://k6.io/docs/get-started/installation/$(RESET)"; exit 1; }
	@echo "$(BOLD)k6 load tests - requires CMS running on :3001 (admin) and :3002 (API)$(RESET)"
	@echo ""
	@echo "$(CYAN)1/5 Auth flow$(RESET) (100 VUs, 5min)"
	$(K6) run tests/load/auth_flow.k6.js
	@echo ""
	@echo "$(CYAN)2/5 Content CRUD$(RESET) (50 VUs, 5min)"
	$(K6) run tests/load/content_crud.k6.js
	@echo ""
	@echo "$(CYAN)3/5 Schema operations$(RESET) (50 VUs, 5min)"
	$(K6) run tests/load/schema_ops.k6.js
	@echo ""
	@echo "$(CYAN)4/5 Health endpoints$(RESET) (200 VUs, 2min)"
	$(K6) run tests/load/health.k6.js
	@echo ""
	@echo "$(CYAN)5/5 Tenant isolation$(RESET) (10 tenants x 20 VUs, 5min)"
	$(K6) run tests/load/tenant_isolation.k6.js
	@echo ""
	@echo "$(GREEN)All k6 load tests complete.$(RESET)"

test-load-%: ## Run a single k6 load test (e.g., make test-load-auth_flow)
	@command -v k6 >/dev/null 2>&1 || { echo "$(YELLOW)k6 not found$(RESET)"; exit 1; }
	$(K6) run tests/load/$*.k6.js
