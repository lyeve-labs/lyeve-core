# Running the engine from a working tree.
#
# There is deliberately no target here that starts a database. The engine takes
# a DSN and nothing else, so the environment is the caller's: point
# DATABASE_URL at any PostgreSQL, MySQL or MSSQL server and the dialect is
# detected from the DSN. A throwaway one is one command:
#
#   docker run -d --name lyeve-dev-postgres -p 4321:5432 \
#     -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=lyeve_dev postgres:16-alpine
#
# That matches the DATABASE_URL already in .env.example, so `make env` needs no
# edit afterwards.

.PHONY: run-go run-go-all migrate-go env

# The program a working tree runs: cmd/lyeve when it resolves, and otherwise
# cmd/lyeve-core, the engine with no plugin compiled in, which builds from a
# clone of this repository alone. RUN_PKG=./cmd/lyeve-core picks the engine
# alone anywhere.
RUN_PKG ?= $(if $(RELEASE_RESOLVES),./$(RELEASE_DIR),./cmd/lyeve-core)

run-go: ## Start the engine, admin on :3001 and API on :3002
	@echo "running $(RUN_PKG)"
	go run $(RUN_PKG)

run-go-all: ## Start the engine in development mode (Ctrl-C stops it)
	@echo "running $(RUN_PKG)"
	APP_ENV=$${APP_ENV:-development} go run $(RUN_PKG)

# The engine applies this tree itself at boot, so this is for a database that
# was dropped between runs. Generated DDL is applied by POST
# /api/admin/migrate/apply, which is the only path that reaches the schema
# engine.
migrate-go: ## Apply the SQL migration tree by hand
	migrate -path ./migrations -database "$$DATABASE_URL" up

env: ## Copy .env.example to .env (skip if already exists)
	@[ -f .env ] || (cp .env.example .env && echo "$(YELLOW)Created .env - set DATABASE_URL and JWT_SECRET$(RESET)")
