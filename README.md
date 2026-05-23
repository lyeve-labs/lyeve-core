# lyeve-core

The engine of [LyEve](https://lyeve.com), a headless CMS written in Go. One
install serves many tenants from PostgreSQL, MySQL or SQL Server, and your
apps read and write content through a REST API. The admin console is
[lyeve-admin](https://github.com/lyeve-labs/lyeve-admin).

| | |
|---|---|
| **Status** | Stable |
| **License** | MIT |
| **Language** | Go, at the version `mise.toml` pins |
| **Databases** | PostgreSQL, MySQL 8+, SQL Server and Azure SQL |
| **Image** | `ghcr.io/lyeve-labs/lyeve-core` |
| **Go module** | `github.com/lyeve-labs/lyeve-core` |
| **Changelog** | [CHANGELOG.md](CHANGELOG.md) |
| **Documentation** | [docs.lyeve.com](https://docs.lyeve.com) |

## How LyEve fits together

LyEve runs as two services, each with its own repository and image.

| Repository | What it does | Image | Listens on |
|---|---|---|---|
| **lyeve-core**, this one | Stores and serves your content, accounts and tenants | `ghcr.io/lyeve-labs/lyeve-core` | `3001` admin API, `3002` content API |
| [lyeve-admin](https://github.com/lyeve-labs/lyeve-admin) | The console your team uses to manage it in a browser | `ghcr.io/lyeve-labs/lyeve-admin` | `3002` |

Put both behind one address. A proxy sends `/api/admin/*` to the engine's
admin port, `/api/v1/*` to its content API, and every other path to the
console. The [Docker Compose guide](https://docs.lyeve.com/deploy/docker-compose/)
sets up the whole stack, TLS included, on one host.

## Features

- **Content API.** REST endpoints create, read, update, list and delete
  entries, which move between draft, published and archived. An update
  writes and validates only the fields it sends.
- **Tenants.** One install serves many tenants. Every query is scoped to one
  tenant, on all three databases.
- **Databases.** PostgreSQL, MySQL and SQL Server behave the same. The engine
  detects which one it has from the connection string.
- **Accounts and access.** Sign-in issues short-lived tokens. Apps
  authenticate with API keys, every account has roles, and the engine accepts
  tokens from OIDC issuers you configure.
- **Privacy requests.** Export or erase the data an install holds about one
  person.
- **Settings bundles.** An instance's settings export as one bundle with every
  secret sealed, and the bundle applies to another instance.
- **Operations.** Structured logs, OpenTelemetry traces, Prometheus metrics
  and health probes. Migrations run on boot.

Plugins in the official image add content types, translated reads, scheduled
publishing, SAML and more. The [documentation](https://docs.lyeve.com) lists
them.

## Quick start

Run the image with a database and two secrets. `APP_ENV=development` lets a
local trial start without the production settings below.

```bash
docker run -p 3001:3001 -p 3002:3002 \
  -e APP_ENV=development \
  -e DATABASE_URL="postgres://user:pass@host:5432/cms?sslmode=disable" \
  -e JWT_SECRET="$(openssl rand -hex 24)" \
  -e ENCRYPTION_KEY="$(openssl rand -hex 24)" \
  ghcr.io/lyeve-labs/lyeve-core:latest
```

Or, from a clone of this repository, start the engine and a PostgreSQL
database together:

```bash
export JWT_SECRET=$(openssl rand -hex 24) ENCRYPTION_KEY=$(openssl rand -hex 24)
docker compose up -d
```

The admin API answers on `3001`, the content API on `3002`, and `GET /healthz`
on both for probes. Point `DATABASE_URL` at MySQL or SQL Server instead and
nothing else changes.

**First run.** On a database with no accounts, the engine logs a one-time
setup token (`setup: no account exists yet`, field `setup_token`). Open the
console, enter the token and create the first super admin. The token stops
working once that account exists.

A Kubernetes starting point is in [`deploy/kubernetes.yaml`](deploy/kubernetes.yaml).

## Configuration

| Variable | Required | What it does |
|---|---|---|
| `DATABASE_URL` | yes | The database to use. In production its credentials must not be the defaults. Use TLS unless the database is on a private network. |
| `JWT_SECRET` | yes | Signs sign-in tokens. Must differ from `ENCRYPTION_KEY`. |
| `ENCRYPTION_KEY` | yes | Encrypts the secrets the engine stores. |
| `APP_ENV` | no | Unset means `production`. `development` relaxes the settings marked "in production". |
| `SECURE_COOKIE` | in production | `true`. Turns on HSTS and the HTTPS redirect. |
| `RATE_LIMIT_RPS` | in production | Requests per second from one client address, above 0. |
| `LYEVE_AUDIT_HMAC_KEY` | in production | 64 hex characters (`openssl rand -hex 32`). Makes the audit trail tamper-evident. |
| `LYEVE_CONSOLE_URL` | recommended | The console's public https URL. Device sign-in and emailed links need it. |
| `ADMIN_CONSOLE_KEY` | no | A secret shared with the console, so sign-in limits count each person rather than the console. |
| `LYEVE_SETUP_TOKEN` | no | Chooses the first-run token yourself, 16 characters or more. Set it when more than one replica runs. |
| `LYEVE_LICENSE_KEY` | no | A license key, which enables licensed plugins in the official image. |

When a setting is missing, the boot error names it. The
[configuration reference](https://docs.lyeve.com/reference/configuration/)
lists every variable, and the
[production checklist](https://docs.lyeve.com/deploy/production-checklist/)
covers the rest of a deployment.

Two optional modes:

- **Setup mode** (`LYEVE_SETUP_MODE=true`). The engine starts even with a
  missing database URL or secret, and the console shows what is missing,
  with fresh secrets to paste into your configuration.
- **Stateless mode** (`LYEVE_MODE=stateless`). The engine runs with no
  database, for an install that relays webhooks and runs flows. See
  [stateless mode](https://docs.lyeve.com/deploy/stateless-mode/).

## Working on the source

Bring any PostgreSQL, MySQL or SQL Server. The commands below start a
PostgreSQL container for you.

```bash
mise install                    # Go, at the version mise.toml pins
docker run -d --name lyeve-dev-postgres -p 4321:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=lyeve_dev postgres:16-alpine
make env                        # writes .env from .env.example, pointing at that database
go build ./...                  # build every package
make verify lint-go             # lints, secret scan, vet and golangci-lint
make test                       # the tests, against the database above
make build-kernel               # bin/lyeve-core
make run-go                     # run the engine from the working tree
```

`make build-kernel` and `make run-go` build `cmd/lyeve-core`, the engine on
its own. It serves setup, sign-in and the admin API. Content types come from
a plugin, so it serves no content until one is added. `cmd/lyeve` is the
official image's build. It needs modules that are not published, so it does
not build from a clone.

| Path | What is there |
|---|---|
| `pkg/core` | The public API a plugin builds against |
| `pkg/` | Packages other modules may import |
| `internal/` | The engine itself, which no other module can import |
| `migrations/` | The engine's schema, for all three databases |
| `cmd/lyeve-core` | The engine with no plugins |
| `tests/` | Integration tests |
| `deploy/`, `docker-compose.yml` | Starting points for Kubernetes and Compose |

## What is open source

Everything in this repository is MIT licensed.

The official image adds plugins on top. They are licensed under the
[Plugin EULA](https://lyeve.com/legal/eula) and are not published. A license
key enables licensed plugins in the official image without a new image. Enter
it in `LYEVE_LICENSE_KEY` or on the console's license page.
[Licensing](https://docs.lyeve.com/concepts/licensing-and-tiers/) explains how.
[Trust](https://docs.lyeve.com/trust/source-code/) shows how to check what the
image contains.

## Contributing

1. Run `mise install` once. Tool versions come from `mise.toml`.
2. Branch off `dev` and open the pull request against `dev`. Commit messages
   follow [Conventional Commits](https://www.conventionalcommits.org).
3. Run the checks under "Working on the source" before you push.

[CONTRIBUTING.md](CONTRIBUTING.md) has the details.

## Security

Report a vulnerability privately, as [SECURITY.md](SECURITY.md) describes.
Please do not open a public issue for it.

Passwords and API keys are stored hashed, and other secrets are encrypted at
rest with AES-256-GCM. Personal data and secrets never appear in logs or in
error messages. [Data protection](https://docs.lyeve.com/trust/data-protection/)
covers what the engine stores and for how long.

## License

MIT. See [LICENSE](LICENSE). The plugins in the official image are licensed
under the [Plugin EULA](https://lyeve.com/legal/eula), not MIT.
