# Contributing to LyEve Core

This guide covers contributing to the LyEve engine and its plugin API (`lyeve-core`).

---

## 1. Dev Environment Setup

**This repository builds on its own.** `go build ./...`, `go test ./...` and
`make verify` need nothing outside it. `cmd/lyeve` is the official image's
build. It needs modules that are not published, so it does not build from a
clone, and no contribution needs it. Build and run `cmd/lyeve-core`, the
engine with no plugins, instead.


### Toolchain (via mise)

```bash
# Install mise (one-time)
curl https://mise.run | sh

# Install pinned tools - Go, at the version mise.toml pins
mise install
```

### Local Postgres

The engine carries no environment of its own: it takes a DSN and detects the
dialect from it. Any server will do, and a throwaway one is one command.

```bash
docker run -d --name lyeve-dev-postgres -p 4321:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=lyeve_dev postgres:16-alpine
```

### Configure .env

```bash
make env          # .env from .env.example, already pointing at the database above
```

Leave `LYEVE_LICENSE_KEY` empty. The engine's own tests need no license. A
signed key only matters when running the image.

### Build and test

```bash
go build ./...           # every package but cmd/lyeve, which is its own module
make verify lint-go      # the gate a pull request has to pass
make test-go             # the engine's tests, against the database above
```

`make run-go` and `make run-go-all` run `cmd/lyeve-core` in a clone of this
repository. `make build-kernel` builds `bin/lyeve-core`, the engine with no
plugin, from this repository alone, and `scripts/kernel-smoke.sh` boots it
against an empty database. To see a plugin running, use the official image and
point a client at it.

### Full Makefile reference

| Target | What it does |
|--------|-------------|
| `make env` | Create `.env` from `.env.example` |
| `make run-go` | Start the engine, admin on :3001 and API on :3002. A clone of this repository runs `cmd/lyeve-core`, the engine with no plugin |
| `make run-go-all` | The same, with `APP_ENV=development` |
| `make build-go` | Compile every package |
| `make build-kernel` | Build `bin/lyeve-core`, the engine with no plugin |
| `make test` | Run the engine's tests with the race detector (same as `make test-go`) |
| `make test-integration` | Run the tagged integration suite |
| `make lint-go` | Check gofmt, vet and golangci-lint on the root module |
| `make lint-go-release` | The same for `cmd/lyeve`, which needs modules that are not published and does not resolve from a clone |
| `make cover` | Test + HTML coverage report |
| `make bench` | Run all benchmarks |
| `make verify-ci` | Every build-time guarantee except the secret scan, as CI runs it |
| `make verify` | Every build-time guarantee, including the gitleaks secret scan |

`make` on its own lists every target grouped by the file it comes from, and
`make print-VAR` shows what a variable expanded to.

---

## 2. Repository Layout

```
lyeve-core/
├── cmd/
│   ├── lyeve/             # The official image's build (a module of its own)
│   └── lyeve-core/        # The engine alone, with no plugin compiled in
├── internal/
│   ├── api/               # HTTP routing, handlers, middleware wiring
│   ├── auth/              # JWT/EdDSA, JWKS, passwords
│   ├── config/            # YAML config parsing, env loading, validation
│   ├── db/                # Multi-dialect SQL, tenancy, pool health
│   ├── middleware/        # Rate-limit, security headers, tenant, API key
│   ├── schema/            # Field defaults and validation
│   └── hooks/             # Plugin lifecycle hook bus
├── pkg/
│   └── core/           # Host interface - THE plugin contract
├── migrations/            # Engine-level DDL (multi-dialect)
├── tools/                 # CI scripts (lints over this repository's source)
└── Makefile
```

**Key rule**: Nothing outside this repo may import `github.com/lyeve-labs/lyeve-core/internal/*`. Go refuses that import across a module boundary.

### The plugins

The plugin contract is `pkg/core`. The plugins themselves are not published.
A change to the contract is reviewed by a maintainer against them.

---

## 3. Branches and Commits

### The two permanent branches

`dev` is the integration branch. `main` is production and carries the release
tags. Both take merges only, and nothing is pushed directly
to either.

**Every change branches off `dev` and goes back into `dev`.** Rebase onto `dev`
rather than merging `dev` into your branch. Only a hotfix branches off `main`,
and a hotfix must be merged back into `dev` afterwards or the next release
reverts it.

A release is its own step: `dev` merges into `main`, and the annotated
`vMAJOR.MINOR.PATCH` tag goes on `main` with the CHANGELOG entry in the same
commit.

### Branch naming

```
feat/<short-slug>     # New functionality
fix/<short-slug>      # Bug fixes
hotfix/<short-slug>   # Urgent production fix, the only type cut from main
chore/<short-slug>    # Repo maintenance, tooling
docs/<short-slug>     # Documentation only
refactor/<short-slug> # Code restructuring (no behavior change)
perf/<short-slug>     # Performance work
test/<short-slug>     # Tests only
ci/<short-slug>       # Pipeline changes
```

### Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<optional scope>): <lowercase subject, no period>

<optional body>

<optional footer(s)>
```

Valid types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`, `perf`, `ci`, `build`, `deps`.

| Type | Version bump | CHANGELOG |
|------|:-----------:|-----------|
| `feat:` | minor | yes (Features) |
| `fix:` | patch | yes (Bug Fixes) |
| `perf:` | patch | yes (Performance) |
| `deps:` | patch | yes (Dependencies) |
| `feat!:` or `BREAKING CHANGE:` footer | **major** | yes (with **BREAKING** marker) |
| `docs:` `refactor:` `test:` `build:` `ci:` `chore:` `style:` | none | hidden |

**Examples that pass:**

```
feat(schema): add field reordering endpoint
fix(auth): clear session cookie on token expiry
perf(db): add covering index to content lookup query
deps: bump pgx to v5.7
feat!: drop /api/v0 endpoint

BREAKING CHANGE: Consumers must migrate to /api/v1. See MIGRATION.md.
```

**Examples that fail:**

```
Add new thing              <- no type
feat: Added new thing       <- subject must not start uppercase
fix: fixed the bug.         <- subject must not end with a period
update                      <- unhelpful, also no type
```

### Developer Certificate of Origin

Contributions are accepted under the repository's MIT license. Every commit
must carry a `Signed-off-by:` line with your name and email, which certifies
that you wrote the change or have the right to submit it under that license, as
set out in the [Developer Certificate of Origin 1.1](https://developercertificate.org).
`git commit -s` adds the line. A pull request with an unsigned commit is not
merged until the commit is amended.

### PR rules

1. Open against `dev`. Never push directly to `dev` or `main`, and never merge your own PR.
2. One logical change per branch. Keep PRs focused and small.
3. PRs land with a merge commit, never a squash, so every commit on the branch
   must be a valid Conventional Commit.
4. All tests must pass, and so must `make verify lint-go`.
5. PRs that change the DB schema must include migration files for ALL three dialects (psql, mysql, mssql).
6. At least one approving review from a maintainer.

---

## 4. Code Style

### Go

- Format with `gofmt` (or `gofumpt`). Run `go vet ./...` before pushing.
- **Error handling**: Always wrap errors with `%w` when propagating across layers.
  ```go
  if err != nil {
      return fmt.Errorf("create content: %w", err)
  }
  ```
- No `log.Fatal` outside `main`. No `panic` in library or plugin code.
- Define sentinel errors for domain boundaries (`ErrNotFound`, `ErrConflict`).
- **Logging**: Use `slog` with structured key-value pairs. Pass logger via `context` or struct field: never use global `slog.Default()` in library code.
  ```go
  slog.Info("content created", "id", content.ID, "type", content.Type)
  slog.Error("db query failed", "err", err, "query", "create_content")
  ```
- **Dependency injection**: Inject all dependencies via constructors. No global variables.
  ```go
  func NewContentService(store ContentStore, hooks HookRunner) *ContentService
  ```
- Define interfaces where consumed, not where implemented. Keep interfaces small (1-3 methods when possible).

### HTTP Handlers (chi router)

Follow the `Decode -> Validate -> Call -> Respond` pattern:

```go
func (h *ContentHandler) Create(w http.ResponseWriter, r *http.Request) {
    var input CreateContentInput
    if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
        respond(w, http.StatusBadRequest, ErrResponse{Message: "invalid JSON"})
        return
    }
    if err := validate.Struct(input); err != nil {
        respond(w, http.StatusUnprocessableEntity, validationError(err))
        return
    }
    result, err := h.svc.Create(r.Context(), &input)
    if err != nil {
        h.handleError(w, err)
        return
    }
    respond(w, http.StatusCreated, result)
}
```

### SQL

- **Always parameterized.** Never string-concatenate SQL.
- Use `$1`, `$2` placeholders (PostgreSQL style) in ALL Go code. The engine's `rewrite()` translates them per dialect at runtime.
- For runtime queries, always use `host.Querier(ctx)` - never raw `*sql.DB`.
  ```go
  row := host.Querier(ctx).QueryRow(ctx,
      "SELECT id, title, data FROM content WHERE id = $1", id)
  ```
- Use transactions for multi-step writes:
  ```go
  tx, err := host.Querier(ctx).Begin(ctx)
  if err != nil { return fmt.Errorf("begin tx: %w", err) }
  defer tx.Rollback(ctx)
  // ... queries ...
  return tx.Commit(ctx)
  ```
- For **dialect-specific SQL in Go code** (upserts, merges), branch on `host.Dialect()`:
  ```go
  switch host.Dialect() {
  case "postgres":
      return `INSERT INTO ... ON CONFLICT ... DO UPDATE ...`
  case "mysql":
      return `INSERT INTO ... ON DUPLICATE KEY UPDATE ...`
  case "mssql":
      return `MERGE INTO ... WHEN MATCHED ...`
  }
  ```

### SQL Translation Reference (for migration files)

| PostgreSQL | MySQL | MSSQL |
|---|---|---|
| `UUID DEFAULT gen_random_uuid()` | `CHAR(36) DEFAULT (UUID())` | `UNIQUEIDENTIFIER DEFAULT NEWID()` |
| `TIMESTAMPTZ` | `DATETIME(6)` | `DATETIME2(7)` |
| `NOW()` | `NOW(6)` | `SYSUTCDATETIME()` |
| `JSONB` | `JSON` | `NVARCHAR(MAX)` |
| `TEXT[]` | `JSON` | `NVARCHAR(MAX)` |
| `BOOLEAN` | `TINYINT(1)` | `BIT` |
| `TEXT` (indexed/UNIQUE) | `VARCHAR(255)` | `NVARCHAR(255)` |
| `TEXT` (unindexed) | `LONGTEXT` | `NVARCHAR(MAX)` |
| `ON CONFLICT DO UPDATE` | `ON DUPLICATE KEY UPDATE` | `MERGE WHEN MATCHED` |
| `CREATE TABLE IF NOT EXISTS X` | Same | `IF OBJECT_ID(N'X', N'U') IS NULL CREATE TABLE X` |
| MySQL reserved words | Backtick `` `key` `` | Bracket `[key]` |

### The plugin contract in short

A plugin implements `Start(ctx, host core.Host)` and `Stop(ctx)`, declares its
routes through `Routes() []core.RouteDecl`, runs its own migrations in all
three dialects through `plugin.PluginMigrate`, and reaches the database only
through the host. The
plugins that implement it are not published. `pkg/core` and `pkg/plugin` are
the contract and carry their own tests.

---

## 5. Testing

### Go

```bash
make test-go             # go test -race -count=1 ./... (requires running Postgres)
make cover               # HTML coverage report (writes coverage.html)
make bench               # Run all benchmarks
make bench-auth          # Auth benchmarks only
make bench-db            # SQL rewrite benchmarks only
make bench-middleware    # HTTP middleware benchmarks only
```

**Testing conventions:**

- **Table-driven tests** with `t.Run()` for all domain logic:
  ```go
  func TestCreateContent(t *testing.T) {
      tests := []struct {
          name    string
          input   CreateContentInput
          wantErr bool
      }{
          {"valid input", CreateContentInput{...}, false},
          {"missing title", CreateContentInput{}, true},
      }
      for _, tt := range tests {
          t.Run(tt.name, func(t *testing.T) { ... })
      }
  }
  ```
- Use `testify/assert` and `testify/require` for assertions.
- Integration tests use `testcontainers-go` for a real PostgreSQL instance.
- Non-database boundaries get handwritten fakes. SQL always runs against a real database.
- Target: test the happy path AND the failure path for every function.

### CI gates

Every PR must pass:
- `make verify lint-go`: the lints, the dev-tag build, the secret scan,
  gofmt, vet and golangci-lint
- `make test-go`: the tests with the race detector
- CI additionally runs govulncheck and the tests on every dialect. `make
  verify` needs `gitleaks` on PATH for the secret scan. CI runs `make
  verify-ci`, which is the same set without the scan, and scans with a pinned
  gitleaks.

### Mutation Testing (go-mutesting)

[go-mutesting](https://github.com/zimmski/go-mutesting) modifies source code
and re-runs tests to measure test quality. Without isolation, Docker-dependent
test files (testcontainers, testdb) spin up real containers per mutant, making
mutation testing infeasible (>30 minutes per file, accumulating goroutines).

**Run mutation tests:**

```bash
# Install
go install github.com/zimmski/go-mutesting/cmd/go-mutesting@latest

# Run on a single package (Docker tests excluded via build tag)
cd lyeve-core
go-mutesting --exec "go test -tags mutest -count=1 -timeout 10s" \
  --exec-timeout 4 \
  internal/api/handlers.go internal/api/router.go
```

The `-tags mutest` flag activates the `!mutest` build constraint on all
Docker-dependent test files, skipping them at compile time. Mutation tests
then run against the remaining unit test suite in <1 second per mutant.

**Adding new Docker-dependent tests:**

Any test file that uses `testdb`, `testcontainers`, `RunContainer`, or otherwise
spins up a real database container MUST be tagged with `!mutest`:

```go
//go:build !mutest

package mypackage
```

If the file already has a build tag, combine them:

```go
//go:build integration && !mutest    // for existing //go:build integration
//go:build !short && !mutest          // for existing //go:build !short
```

This ensures mutation testing remains fast while integration tests run
normally under `go test ./...` and in CI.

Keep the build tag as the very first line: before any package doc comment.
If the file also has a legacy `+build` line, keep it in sync:

```go
//go:build integration && !mutest
// +build integration,!mutest
```

---

## 6. Changing the plugin contract

`pkg/core` is the contract every plugin builds against, and the plugins are
not published. A change there is reviewed by a maintainer against them before
it merges, so say in the pull request what the change asks of a plugin. Every
migration the engine ships comes in all three dialects (psql, mysql, mssql).

---

## 7. Security

### Reporting vulnerabilities

**Do not open a public issue.** Email `security@lyeve.com`. See [`SECURITY.md`](SECURITY.md) for the full policy.

### Secure coding requirements

- All SQL queries parameterized: no string interpolation.
- Passwords hashed with bcrypt (the default) or argon2id, chosen by
  `PASSWORD_HASH_ALGO`. Never MD5/SHA1.
- API keys stored as SHA-256 hashes (HMAC-SHA256 when a server-side pepper is configured). Plaintext keys returned once at creation.
- JWT signing: EdDSA (Ed25519) with `kid` header. JWKS published at `/.well-known/jwks.json`.
- Encrypted data at rest uses AES-256-GCM with PBKDF2-HMAC-SHA256 key derivation (600K iterations, OWASP 2023 minimum).
- Tenant isolation cannot be bypassed: middleware chain is TenantHeader -> TenancyConn -> isolated `*sql.Conn`.

---

## 8. Review Checklist

Before requesting review, ensure:

- [ ] `make test-go` passes locally (with running Postgres)
- [ ] `make lint-go` passes (`go vet ./...`)
- [ ] `make verify lint-go` and `make test-go` pass
- [ ] All three SQL dialect migration files are present and correct (if DDL changed)
- [ ] Commit messages follow Conventional Commits
- [ ] PR title is a valid Conventional Commit
- [ ] Errors are wrapped with `%w`, not replaced or swallowed
- [ ] No `log.Fatal` outside `main()`. No `panic` in library code
- [ ] Queries use `host.Querier(ctx)` - never raw `*sql.DB` for runtime queries
- [ ] SQL uses `$N` placeholders: the rewriter handles per-dialect translation
- [ ] New code has table-driven tests with `t.Run()`
- [ ] No hardcoded secrets, paths, or environment-specific values
- [ ] Plugins communicate with engine only via `core.Host`

As a reviewer, when evaluating PRs, look for:

1. **Security**: SQL injection, auth bypass, tenant boundary violation, secret leakage
2. **Correctness**: Edge cases handled, error states accounted for, nil checks on pointers
3. **Performance**: N+1 queries, missing indexes, unbounded allocations
4. **Consistency**: Patterns match existing codebase conventions (handler pattern, store pattern, error wrapping)
5. **Testing**: Happy path AND failure path covered, edge cases tested

---

## 9. Building the Binary

`cmd/lyeve` is the official image's build. It needs modules that are not
published, so it does not build from a clone, and nothing in a contribution
depends on it. The engine packages and their tests are the whole of what a
pull request exercises.

`cmd/lyeve-core` is the engine with no plugins. It is part of the root
module, and `make build-kernel` builds it from a clone as `bin/lyeve-core`.

---

## 10. Getting Help

- Architecture overview: [`README.md`](README.md)
- Plugin contract: [`pkg/core/`](pkg/core/)
- Security policy: [`SECURITY.md`](SECURITY.md)
- Questions? Open an issue.
