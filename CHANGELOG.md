# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.52.2] - 2026-10-07

### Fixed

- Object storage accepts a multipart upload part sent as
  `application/octet-stream`. Every part was refused with `415`, so a
  multipart upload could never complete.
- A data subject export on an install without the audit plugin writes every
  section. It stopped at the missing audit table and returned an empty body.
- The email template starters list the required password-reset and
  magic-link templates first.

## [0.52.1] - 2026-10-07

The v0.52.0 tag has no image: its release build stopped on the release
binary's own checks. This release ships the same engine with the plugin and
license module releases those checks expect.

### Changed

- The image ships the license module release whose release checks know the
  response cache's place in the middleware chain, the capture policy and
  tenant region roles, and the capture set replay route.
- Rate limiting no longer serves a super admin route for another tenant's
  refusal history. A super admin switches into the tenant and reads the
  history route every tenant uses.

## [0.52.0] - 2026-10-07

### Added

- `core.CapturePolicyProvider`, a role one plugin may hold to decide which
  requests the capture middleware stores and how long each is kept. The
  middleware asks it once per request after the tenant is resolved. With no
  provider every request is kept for `core.DefaultCaptureTTL`, 24 hours, as
  before.
- `core.TenantRegionResolverProvider`, a role one plugin may hold to answer
  which region a tenant's data belongs in. Every other plugin reads it
  through its host, which asks the running holder on each call and answers
  nil when no plugin holds the role. Two running plugins holding it refuse
  the boot.
- `DELETE /api/admin/license` in the image removes a license stored on the
  install, so it returns to the free tier without a restart. A license set
  in `LYEVE_LICENSE_KEY` is changed there and the route refuses it with
  `409`.

### Changed

- The image ships the latest release of every plugin and of the license
  module.
- The plugin policy grants a database to the plugins that now store tenant
  settings, the secret configuration to the plugins that seal stored
  addresses with the tenant key, and the hook bus to the plugin that
  rebuilds its state when the license changes.

### Fixed

- A revision store can refuse a read with `core.NotGrantedError`, and the
  record revision routes answer it with the same `402` body a plugin's own
  route sends. A record with no history lists `[]` instead of `null`.
- Publishing and unpublishing through the REST routes fire the after-update
  event every other write fires, so a search index, a webhook or a response
  cache hears that an entry went live or was withdrawn.

## [0.51.2] - 2026-10-05

The first release with a published image. The v0.51.0 and v0.51.1 tags have
none, so install 0.51.2 or later.

### Changed

- The image builds every plugin at its newest release, and the release binary
  requires each one by that version.

### Fixed

- The release workflow sets up Go before it builds the workspace, so the image
  build no longer stops after its tests pass.

## [0.51.1] - 2026-10-05

Its release build stopped before publishing, so this tag has no image either.
Install 0.51.2 or later.

### Changed

- The release image builds every plugin it ships at that plugin's latest
  release.

### Fixed

- When a plugin migration fails because the schema already holds what it
  creates, the error now names the repair that works on MySQL. There the
  failed claim survives as a row marked incomplete, so the repair marks that
  row complete instead of inserting the version again.

## [0.51.0] - 2026-10-05

This is the first public release of the engine. It is a headless content
engine written in Go, and it runs on PostgreSQL, MySQL 8 and SQL Server.

### Added

- `core.ContentStatusProjector`, which moves a content row between draft,
  published and archived as `SetContentStatus` does and publishes no
  lifecycle event. A plugin that moves many entries at once and announces
  each change itself, after the whole change has held, writes through it so
  a subscriber sees every entry once and never sees a move that was undone.
  The engine host and the scoped host a plugin receives implement it.
- `core.ConfigSection`, the contract a plugin implements to offer its
  configuration to a bundle that moves between instances, with
  `ConfigSectionRegistrar` and `ConfigSectionProvider` on the host and a
  `ConfigSectionRegistry` a host embeds. A section exports its resources,
  plans a bundle against the instance and applies it inside a transaction
  the caller holds, sealing every secret with a `ConfigSealer` the caller
  gives. The engine offers a `settings` section of its own: the engine
  settings an operator saved that are right on every instance (token
  lifetime, body limits, CORS headers, rate limits). A setting that names
  where an instance lives, such as its origins, and every credential stay
  out of it. Listing the sections takes `core.CapConfigSectionsRead`, which
  a build grants only to the plugin that assembles a bundle, because a
  section exports its owner's secrets. A section name belongs to the plugin
  that registered it first, and another plugin's registration or removal
  under that name is refused. An applied `settings` section records the user
  who applied it.
- `core.ErrEmailNotConfigured`, which an `EmailSender` returns when there is
  no mail transport for the tenant or the instance. A caller can tell it apart
  from a send that failed and take its own no-mail path.
- A content API under `/api/v1/content/{schema}` with a draft, published and
  archived lifecycle, and locale-aware reads when a plugin registers a
  `core.ContentLocalizer`.
- Two listeners: the admin API on `:3001` and the content API on `:3002`. The
  engine applies its own SQL migrations for all three dialects at boot.
- Accounts, roles and sign-in: password login, session tokens signed with
  EdDSA or HMAC-SHA256, refresh tokens, a JWKS endpoint and trusted OIDC
  issuers mapped through `TRUSTED_ISSUER_POLICIES`.
- API keys held to `resource:action` scopes, and admin tokens
  (`Authorization: Bearer lyat_<secret>`) for machines that call the admin
  API, managed at `/api/admin/admin-tokens`.
- Device sign-in for command line tools at `POST /api/admin/auth/device`.
- Multi-tenancy as a `tenant_id` column on every tenant-owned row. The boot
  refuses to start while any such table has no purge path.
- Setup mode (`LYEVE_SETUP_MODE=true`), which boots without a database and
  reports the missing settings, and stateless mode (`LYEVE_MODE=stateless`),
  which serves API keys declared in the configuration file.
- TLS on both listeners with `TLS_CERT_FILE` and `TLS_KEY_FILE`, reloaded
  from disk without a restart.
- An OpenAPI document in two halves, `GET /api/admin/openapi/public.json` and
  `GET /api/admin/openapi/admin.json`.
- A plugin contract in `pkg/core`. A plugin declares its routes, route group
  and capabilities, and fills host roles the engine reads at run time:
  `core.SchemaSourceProvider` for schemas, `core.PermissionCheckerRegistrar`
  for permission rules, `core.RecordRevisionStoreRegistrar` for revisions,
  `core.MembershipProvider` for cross-tenant membership,
  `core.AdminTokenStoreProvider` for admin token storage and
  `core.ClusterBusProvider` for messages between replicas.
- `cmd/lyeve-core`, the engine with no plugin and no license verifier. It
  builds from this module alone with `make build-kernel`.
- The licensing interface in `pkg/licensing`. A build passes a
  `licensing.Verifier` in `runtime.Options.Licensing`, and the `Manager` it
  returns decides which compiled plugins start and what each tenant is
  entitled to. A build that passes none runs on `licensing.Open`, which
  verifies nothing and starts every compiled plugin with no ceiling.
- `GET /api/admin/entitlements` carries `license_module`, true when the build
  links a licensing implementation.
- `GET /api/admin/plugins/status` lists the routes each running plugin
  serves, with method, pattern and route group.
- An optional ceiling on admin accounts, the accounts that hold `admin` or
  `super_admin`, that a licensing implementation may state. When it is
  reached, creating an admin, promoting an account to admin, or letting a
  trusted issuer's user in as an admin answers `402 cap_exceeded` with the
  cap `admin.seats`.
  An install already past it keeps every account it has, and the first admin
  created at setup is never refused. Plugins read the same check from the
  host as `core.AdminSeatGuardProvider`, and `core.WriteWithAdminSeat` runs a
  role write under it. Writes that race at the ceiling are serialized, so
  exactly the ceiling's worth succeed.
- `core.WriteSerializer`, a host role that runs a write in one transaction
  under an exclusive named lock every replica honors, on all three dialects.
  A plugin keeps a ceiling by counting and inserting inside it.
  A lock not granted in time wraps `core.ErrServiceUnavailable` and answers
  503.
- An API key scope can name one thing under its resource: `content.posts:read`
  reads the posts schema and no other, and a scope on a plugin route whose
  path names something, such as `/api/v1/<resource>/{slug}`, can be held to
  one value of it. A scope on the bare resource still covers every name
  under it, so every key issued before reaches what it reached. A key held
  to named schemas is held to them on every schema a request touches,
  including a relation route and a populated field.
- Scope actions tell creating from updating. `POST` asks `create`, `PUT` and
  `PATCH` ask `update`, and `write` grants both, so a key holding `write`
  keeps its reach.
- `GET /api/admin/api-key-scopes` lists every route an API key can be scoped
  to, with the scope each one asks, for an admin or super admin. The console
  builds its key form from it.
- The admin console can vouch for the host the browser opened, in
  `X-Lyeve-Console-Host` with its own signature bound to the request's
  console signature. `core.RequestHost` returns it ahead of `r.Host`, so
  host based tenant resolution works for pages the console serves before
  sign-in. An unsigned host header is dropped, a host signature that does
  not verify is refused with 401, and a signed host no domain can hold is
  ignored. Only tenant resolution reads it: `ALLOWED_HOSTS`, CORS and the
  HTTPS redirect still read the name the engine was reached at.

### Changed

- The engine records its schema version in its own table,
  `engine_schema_migrations`, apart from golang-migrate's default and from
  each plugin's table. A build can pass `runtime.Options.SchemaVersionSeed` to
  report the version a database already holds the first time that table is
  empty. The official image does so for an install upgraded from an earlier
  image, so its first boot applies only what the database lacks.
- An API key reads content under the access rules of the roles it holds, as
  a user with those roles would, and only the schemas it is held to. A key
  with no role, or whose roles have no rule on a schema, is refused with 403
  on that schema. Before upgrading, give each key that reads content a role
  and a rule on the schemas it needs.

- An entry reads with the same JSON on MySQL and SQL Server as on
  PostgreSQL. This changes what those two databases return:
  - A boolean field is `true` or `false`, where MySQL returned `1` or `0`.
  - A datetime field is RFC 3339 in UTC with an explicit offset, such as
    `2026-10-01T09:30:00.25+00:00`, where MySQL returned
    `2026-10-01 09:30:00.250000` and SQL Server returned no offset.
  - A uid field and a relation key are lower case, where SQL Server returned
    upper case.
  - A json field is the JSON value it holds, where SQL Server returned it as
    an escaped string.

  A datetime stored with no offset is read as UTC, because the offset it was
  written with cannot be recovered. A flow condition or webhook consumer that
  compares a boolean field to `1` has to compare it to `true`.

- A datetime field written as text with no offset, such as
  `2026-10-01 09:30:00`, is stored as UTC on every database. An RFC 3339
  value with a `Z` or an offset is accepted on MySQL too.
  A MySQL `DATABASE_URL` that sets `loc` is held to `loc=UTC`.

### Fixed

- `POST /api/admin/users` refuses with 400 a request that resolves no
  tenant, instead of storing an account with the empty tenant that could
  sign in and was refused everywhere after. A super admin on an install
  with several tenants names one with `X-Tenant-ID`.

- On an install with several tenants, a session or API key whose claim
  names no tenant does not take the tenant of the mapped domain it is used
  on. It is refused with 403 like on any other address. An install with
  tenancy on and a single tenant refuses such an account too. Only a request
  with no credential, and a super admin, take the domain's tenant.

