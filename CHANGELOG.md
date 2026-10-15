# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.55.0] - 2026-10-12

### Security

- Through the plugin releases this engine pins: a flow acts as the account that published it. Its content nodes, its content event trigger and the nodes other plugins contribute read and write only what that account may, with its field mask, and stop when the account is demoted, disabled or deleted. Run records show their values only to readers whose access covers them, and a flow published through an admin token acts as no account.
- Through the license library release this engine pins: a license's expiry, not-before time and grace period are judged by a clock that never runs backward, so setting the server clock back no longer keeps an expired license active.
- A draft or archived entry is no longer readable on the public content reads. The list, the read by id, the cursor list, relations, populate, revisions and the change stream serve only published entries to a caller who may not read drafts, a status filter other than `published` answers `403` for that caller, and a response that can carry drafts is sent `private, no-store`. Reading drafts takes both the read and the update permission on the content type, and an API key without an admin role also needs the read and update scopes on it. A change event that carries no status on a content type with draft and publish is held back from readers.
- Reading entries with their related entries populated no longer writes into the copy of an entry the engine caches. Two readers populating the same entry at once could stop the engine, and a relation one reader populated could appear in another reader's plain read of that entry, past that reader's own access rules and field masks. Every reader now gets its own copy.
- An audit entry written for a data subject's export or erasure names the subject by a SHA-256 digest of the identifier the request named, usually an email address, instead of the identifier itself. The log still proves each request happened, and anyone holding the identifier can find its entry. Entries written before this release are left as they are.
- An API key limited to some content types, or bound by access rules, is held to them on every transport. The schema listing shows only the content types the caller may read, a filter on a field the caller's rules hide is refused, and a flow carries the caller that started it, where its nodes used to run with no caller.
- An access rule that hides a relation field hides both forms of it on every read, the key column and the populated record. A filter on either form is refused, and a read whose content type cannot be loaded fails instead of serving unmasked data.
- Listing the records linked through a relation is refused when the caller's rule hides that relation field on the record the listing starts from.
- A content read that a session cookie may have authenticated is marked private and never stored, so a shared cache in front of the engine can no longer hand one signed-in user's view to another caller. Reads authenticated by a token or a key stay cacheable and vary on the header that carries the credential.
- An open content stream checks every event against the caller's current access, field mask and draft rules, and ends with a short reason when access is withdrawn. It used to check once, when it opened.
- An open content stream ends when its token expires, and before each event it checks that the session or API key still holds. A sign-out, a password an administrator set, a disabled or deleted account, an erasure, or a revoked or narrowed key ends it. Clients reconnect and authenticate again.
- A relation can no longer point at an entry that belongs to another tenant. Every write of a relation checks, inside the same transaction, that the target belongs to the caller's tenant, and refuses another tenant's entry exactly as it refuses one that does not exist, so the answer no longer tells a caller which ids exist elsewhere.
- A plugin can no longer replace or remove a service another plugin supplies to the whole install, such as the permission checker or the schema engine, whatever capabilities it holds. Each such registration now needs the capability of its role, the build grants each role to the one plugin that supplies it, and only the plugin that registered a role can replace or clear it. A refused registration is returned to the plugin and logged.
- Through the plugin releases this engine pins, the relation check holds on every other path that writes a relation: GraphQL and gRPC writes, bulk imports, incoming webhooks, entries saved in the admin console, tenant clones and backup imports.
- Through the GraphQL plugin release this engine pins: queries, mutations and subscriptions answer to the access rules and field masks the Content API applies. A subscription socket is held to the credential that opened it and ends when that credential expires or is revoked, and a subscriber receives only its own tenant's events. The persisted query lockdown can no longer be passed by sending a query with its own hash, and it governs the socket too. Introspection is refused to a key limited to some content types, and errors no longer carry the database's text or guesses at field names.
- Through the gRPC plugin release this engine pins: content and schema calls answer to the access rules and field masks the Content API applies. A caller whose token names no tenant is refused on a multi-tenant install, server reflection takes the bearer token every other service takes, a call with no bearer token is refused before its message is read, a request message is capped at 4 MiB, and a database failure answers `Unavailable` without the driver's text.
- Through the audit plugin release this engine pins: the audit hash chain stays verifiable through prune, retention, tenant deletes and subject erasure, each of which now leaves a signed seal, and prune and retention keep every entry a legal hold covers. `GET /api/admin/audit-log/verify` checks the chain, and `POST /api/admin/audit-log/rehash` re-signs it after a deliberate reset, such as a new audit key.
- Through the content plugin release this engine pins: escaped text in a rich text field is no longer stored as live markup, four other ways past the sanitizer are closed, and a rollback sanitizes the revision it restores.
- Through the multitenant plugin release this engine pins: a tenant clone creates its target the way tenant create does, so it can no longer take a slug whose deleted tenant left rows behind.
- Through the multitenant plugin release this engine pins: a row a backup import cannot restore is reported as "the row could not be restored". The database's own text, which names its tables, keys and values, goes to the log instead of the answer.
- Through the schema plugin release this engine pins: a tenant that defines a content type another tenant also defines can no longer store a field in a column the other definition uses for something else, such as a plain field on the other tenant's relation key, through which writes skipped the relation check. A field can no longer be stored in a column the engine keeps for itself, such as `tenant_id`.
- On an install with several tenants, a session token that names its tenant in a spelling the engine never issues, such as one with a capital letter or a hyphen, is refused with `401` before any handler runs, whatever tenant header a super admin sends. A request that read the database was already refused when its connection was scoped to that tenant, and a stream that read nothing was not. A single-tenant install is unchanged.
- Through the GraphQL and realtime plugin releases this engine pins: a GraphQL subscription socket, and a realtime socket that runs flows, no longer hold a database connection for their whole life on a multi-tenant install. A few idle sockets in different tenants held every pooled connection, so other tenants' requests waited, and a GraphQL socket's subscriptions shared one connection, which dropped events under load.
- Through the plugin releases this engine pins: an id a caller sends no longer tells it which ids another tenant holds. A flow's upsert, an incoming webhook, a translation import, a PII masking rule, a search ranking config and an admin's tenant import each give a new record an id of its own, or refuse an id the caller's tenant does not hold, and an incoming webhook's field map can no longer set the id or the tenant.
- Through the SCIM plugin release this engine pins: a directory acts only on the accounts it created or took over, and never on a super admin. A directory's token could deactivate or delete any account in its tenant by naming its address.
- Through the storage plugin release this engine pins: a tenant admin can no longer copy or delete another tenant's object through a GCS multipart upload, and the health route shows a tenant admin only that tenant's providers. No provider test, health check, error or signed URL carries a driver's text, an Azure SAS token or credentials written into an endpoint.
- Through the webhook plugin release this engine pins: a delivery retry through one webhook's path refuses a delivery of another webhook, which it used to send to that webhook's URL with its signature.
- Through the MFA plugin release this engine pins: starting authenticator setup keeps the active authenticator until a code from the new key verifies.
- Through the synthetic monitoring plugin release this engine pins: a probe update that moves the target to another host needs each stored secret sent again, and a run drops its secrets on a redirect to another origin.
- Through the schema plugin release this engine pins: adding a relation to a content type that several tenants define clears dangling references only in the saving tenant's rows, and a definition that would store a field differently from another tenant's definition of the same table is refused.
- Through the plugin releases this engine pins: a bulk import through the content plugin refuses a relation into another tenant's entry or a missing one, as its dry run does. A draft the content status backfill lost to a deadlock on MySQL is projected again instead of staying readable on the public content routes. The ceiling of 50 enabled PII masking rules per tenant holds under concurrent writes.
- On an install with several tenants, an account that a tenant's identity provider, its directory or a self sign-up creates belongs to that tenant alone. It is found by the tenant and its address, so the same address can name a different account in each tenant, and a sign-up, a password reset or a magic link in one tenant no longer reveals whether another tenant holds the address. The sign-in plugins take part through the releases this engine pins.
- A sign-in refused because the account has no password, or signs in only through its identity provider, takes as long as a sign-in for an address nobody holds.
- The tenant a request resolves from its hostname decides which tenant's account a password sign-in or a token request reaches. A tenant named in the request body no longer overrides it.
- Through the plugin releases this engine pins: a SAML sign-in never signs in an account that holds the super admin role, whichever way the account was found, and a user the directory deactivated is refused at SSO sign-in however the directory capitalized the address. A magic link requested with a tenant named in the request body never creates an account, and password reset and magic link requests answer in one timing class whether or not the tenant holds the address.

### Added

- A plugin can give a public route on the general limit a limit of its own at runtime, through `core.RegisterPublicRouteLimitResolver`. The public limiter asks it on each request to such a route, and a limit it answers replaces the general per-address cap there, higher or lower. A route with a limit of its own, such as sign-in or the token endpoint, keeps that limit whatever a resolver answers, and so does every route under `/auth`, which guards a credential: `PUBLIC_RATE_LIMITS` stays the way to move them. A HEAD request follows its GET route's limit and shares its bucket.
- `core.EngineRateLimitsProvider` lists every public route on the general limit that can take a limit of its own, as scope `general`, with the general limit's values. HEAD routes and routes under `/auth` are not listed.
- A plugin route can declare the media types it answers in, such as a stream, CSV or a file, through `MediaTypes` on its `core.RouteDecl`, and the API description shows them. A route that declares nothing is described as before.
- A plugin route can declare the response headers a page on another origin may read and the request headers it accepts, through `ExposeHeaders` and `AllowHeaders` on its `core.RouteDecl`. The engine adds them to the CORS lists every router sends, and the origin check is unchanged.
- The `contentrules` package gives a plugin that writes content the field defaults and validation rules the Content API applies: `PrepareCreate` for a new entry and `ValidatePatch` for a partial update.
- A tenant purge handler can learn whether the tenant delete it ran in committed or rolled back, through `core.OnTenantPurgeOutcome` on the context the delete hands it. Work a handler does outside the database can then wait for the commit. Handlers that do not ask behave as before.
- A free install refused another admin seat gets a link to where that ceiling is lifted, in the refusal body and its upgrade header, when the build's licensing names one. A link that is neither https nor a console path is dropped, and a build whose licensing names none refuses as before.
- Each tenant sets how many days an account its identity provider created may still sign in with a password, a reset or a magic link after its last sign-in through that provider, from 0 to 365 with 30 as the default, through `GET` and `PUT /api/admin/auth/provider-window`. 0 allows sign-in through the provider only. Only a signed-in session can read or change it, a tenant admin changes only their own tenant, and each change is audited with the old and new values.
- Through the plugin releases this engine pins: a tenant's admin can abandon a content type rename that stopped partway and cannot finish, which puts every part it moved back under the old name and frees both names. A refusal at a free ceiling on tenants, a tenant's admin seats, OAuth providers, review workflows or locales carries a link to the pricing page opened on the add-on that lifts it. The content and localization plugins list a tenant's pending rich text repairs.

### Changed

- Creating an entry through the Content API writes the `_status` it is given on a content type with draft and publish, and a value other than `draft`, `published` or `archived` is a validation error. Updating or restoring an entry no longer changes its status: publish and unpublish through their own routes.
- The deprecation `Link` header on an admin-role API key, and the README, point at the documentation's new address, lyeve.com/docs. The old host redirects there.
- A release candidate image is published under its exact tag only, `X.Y.Z-rc.N`, and never as `latest` or the minor line, so an install that follows either tag never receives a candidate.
- A key that holds only schema read access also needs content read access to see a schema, and asking for a content type the caller may not read answers `403` whether or not it exists.
- The MySQL and SQL Server hosts in `plugintest` get each test's database ready ahead of the test, and the MySQL one keeps its data in memory, so a plugin's tests on those dialects wait far less for database setup.
- Every host role registrar returns an error, so a plugin's Start can report a refused registration: `core.PermissionCheckerRegistrar`, `core.SchemaEngineRegistrar`, `core.EmailSenderRegistrar`, `core.FlowDefinitionValidatorRegistrar`, `core.FlowInvokerRegistrar`, `core.ContentLocalizerRegistrar`, `core.ContentEntryWriterRegistrar`, `core.RecordRevisionStoreRegistrar`, `core.StorageSetter` and `core.UserProvisionerSetter`. A host or test double that implements one has to return an error too. Seven capabilities name the roles that had none (`core.CapPermissionChecker`, `core.CapEmailSender`, `core.CapContentLocalizer`, `core.CapContentEntryWriter`, `core.CapRecordRevisionStore`, `core.CapStorage` and `core.CapUserProvisioner`), a plugin's own capability declaration does not narrow them, and `core.ErrHostRoleUnsupported` tells a plugin that the host takes no such role.
- Through the plugin releases this engine pins: each role that reads or writes content through GraphQL or gRPC needs an access rule for those content types, as the Content API does. A key scoped only to `graphql.ws:read` needs `graphql:read` to subscribe. The GraphQL socket speaks only `graphql-transport-ws` and takes its token on the upgrade or in the first `connection_init`. Content fields are nullable on GraphQL output, so regenerate typed clients. A gRPC tool that calls server reflection sends the bearer token.
- Through the audit plugin release this engine pins: stop every replica before you start this version, because an older replica deletes and redacts audit entries without sealing the chain. The first boot enforces retention policies that never ran on schedule before, so review them first. A tenant with an active legal hold can no longer be deleted until its holds are released.
- Through the GraphQL plugin release this engine pins: creates and updates run the Content API's write rules and a create fills each declared `default` first. Input the Content API refuses is refused with `INVALID_INPUT` and the Content API's message, and a required field with a default may be left out of a create.
- Through the plugin releases this engine pins: a plugin whose host role this engine refuses logs the refusal and keeps running without that role. The permissions plugin fails its start instead, because it would otherwise serve rules nothing enforces.
- Through the flow plugin release this engine pins: a datasource update keeps the secret parts it leaves out, `"secret": {}` keeps every part and `"secret": null` clears them. A flow's upsert with an id its tenant does not hold fails the node instead of creating a record.
- Through the usage and webhook plugin releases this engine pins: the quota, quota request, incoming webhook and dead letter lists say whether their paid capability is licensed.
- Through the plugin releases this engine pins: a closed region cannot be the default, and a SCIM connection's deprovision action is `disable` or `delete`.
- A session's email claim is no longer unique on an install with several tenants. An account a tenant's identity provider created carries its address there, and the same address can sign in to another tenant's account, so identify an account by its id. Migration 011 adds the account's provider address and its last provider sign-in, and runs at the next start.

### Fixed

- The content change stream stays open past the server's write timeout. It ended after 15 seconds, so a change made after that point was lost. The stream now moves its deadline forward before every write and sends a heartbeat comment every 10 seconds, which also keeps proxies from closing a quiet stream.
- Through the multitenant plugin release this engine pins: a backup restore test no longer deletes the tenant's data. It restored the newest backup over the tenant itself, so every change since that backup was lost. It now checks the backup without writing anything. Scheduled backups and restore tests also run on their schedules, which nothing did before.
- Through the multitenant plugin release this engine pins: a backup import accepts a dump of up to 500 MiB and a backup validate one of up to 50 MiB, the limits those routes document, and a larger body answers `413` in JSON. A clone whose client disconnects partway removes its target tenant, and the JSON Lines and portable dump exports have ten minutes to finish instead of stopping at the admin listener's 30-second write deadline.
- Through the multitenant plugin release this engine pins: export, import and restore name their operation in an `X-Operation-Id` response header, so a caller can follow its progress, and every caller following one operation gets each progress event.
- Through the plugin releases this engine pins: a draft saved in the admin console is no longer served as published on the Content API, GraphQL or gRPC, and drafts saved before this release are marked as drafts again in the background. A scheduled publish sends the same webhook and flow events a save does. Failed webhook deliveries are retried, the first retry waits the base delay, and a webhook's own retry fields apply when it has no retry policy. A realtime stream no longer ends at the server's write timeout. The IP masking rule masks IPv6 addresses as well as IPv4.
- A `json` field's default is copied into each new entry instead of shared with the schema, so a hook that edits the entry in place no longer edits the default.
- `security.DiscoverOIDC` gives each caller its own copy of the cached issuer configuration.
- An unchanged content entry carries the same `ETag` on every read, so `If-None-Match` answers `304` again. Content responses encode map keys in sorted order, and the entry's tag hashes those bytes. Before, an entry's fields were written in random order and its tag changed on almost every read.
- Every boot logged a `public rate-limit config gap` ERROR for public routes with no limit of their own, although each runs on the general per-address cap, which is the intended limit for a route that guards no credential. Those routes are now listed once at Info. The ERROR remains for the cases it was written for: a route with no limit of its own and no general cap, which runs unlimited, and a route under `/auth` with no limit of its own, which guards a credential with only the general cap.
- The schema import accepts bundles up to its documented 4 MiB. A general 1 MiB limit refused larger ones first. Every other admin route keeps its limit.
- Six guard refusals now answer with the JSON error shape and content type every other refusal uses. Their body was JSON labeled as plain text, so a client that trusted the label could not read it. Status and message are unchanged.
- The API description names the format each route answers in, including the content change stream, which serves Server-Sent Events and was described as newline-delimited JSON. It also names the role or token each admin route requires and the schema import's request body.
- Through the plugin releases this engine pins: GraphQL lists work on SQL Server, GraphQL and gRPC writes can set a `belongs_to` relation, and a GraphQL record has one form on every database. Live log streams stay open past the server's write timeout. The Elasticsearch search provider indexes and searches. A GitHub sign-in provider made from its template signs people in. Scheduled audit retention applies its policies. Tenant backup import and validate accept dumps up to their own limits, and export, import and restore name their operation so its progress can be followed.
- The table helpers in `plugintest` (`AssertTablesExist`, `AssertMigrationTable` and `DBFixture.TableExists`) count a table only in the test's own database. On MySQL `information_schema` lists every database on the server, so a table another test created used to count.
- A plugin that reads content type definitions while no schema engine is registered gets an error instead of a panic. Its reads also go to the engine registered at the time of the read, not the one there was when the plugin started.
- On an install with several tenants, the session check on authenticated API routes no longer takes the request's tenant database connection. It took that connection before any handler ran, and the handler kept it until it returned, so every stream opened with a session token held one connection for as long as it stayed open, whether or not it read anything, and 25 idle streams in any tenants held the whole default pool. The check now reads on a connection it gives back at once, so a stream whose handler reads nothing holds none, and a revoked session is refused as before.
- On an install with several tenants, an open content change stream no longer holds a database connection between changes. Before each change it checks again that the caller's credential, access rules, draft access and field mask still admit it, and those reads took the connection the request was scoped to and kept it until the client left. Each check now borrows a connection and gives it back, and a revoked credential or withdrawn access still ends the stream at its next change.
- Through the GraphQL plugin release this engine pins: a plugin restart ends each open subscription socket with close code 1012 and the reason `Service restart`, which `graphql-ws` retries. Its subscriptions used to go silent until the client reconnected for another reason.
- A content update that runs inside a transaction a plugin holds, as the content plugin's save does, no longer retries a deadlock it lost. On MySQL the deadlock had already rolled the transaction back, so the retried update committed on its own while the rest of the save was lost, and the save was answered as saved. On PostgreSQL and SQL Server the retry failed with an error that did not read as a deadlock. The update now returns the deadlock to the plugin, which can start its transaction over. An update outside such a transaction is retried as before.
- A content write that runs inside a transaction a plugin holds drops the cached reads of its content type when that transaction commits, rather than when its own statement completes. A read in between used to cache the row as it was before the write, and serve it after the commit until the cache expired. A read inside the transaction now goes to the database, so it sees the write and never caches a row that has not committed.
- A many_to_many between two content types whose pivot table name would pass 63 characters keeps its pairs in a table named by `core.BoundedPivotTableName`: the name's first 54 characters, an underscore and 8 characters of its SHA-256. MySQL refused the full name, and PostgreSQL cut it to 63 bytes, so two such relations could share one table. The content store resolves the pivot through `core.ResolvePivotTable`, the same rule the schema engine creates it by, and a pivot an earlier install created under its full name keeps that name.
- Through the plugin releases this engine pins: a tenant delete that rolls back after the search purge ran no longer leaves the tenant without its Elasticsearch or Meilisearch index on every replica, with its writes refused for ten minutes. The index is dropped, and the other replicas told, only once the delete has committed.
- Through the rate limit and usage plugin releases this engine pins: on an install with several tenants, a GET, HEAD or OPTIONS request reads its rules, limits and quotas on a connection given back at once, so idle streams and sockets no longer hold the whole pool.
- Through the schema plugin release this engine pins: a relation saved with a new target, kind or key column moves its key, and deleting a content type or a tenant takes off shared tables the keys and indexes only it asked for. Pivot tables are dropped once nothing declares them, get their foreign keys on MySQL 8, and a tenant delete clears a long PostgreSQL pivot stored under its 63-byte name. Making an indexed field required or changing its type works on SQL Server and MySQL, an email field can be retyped on PostgreSQL, and generated index and key names stay within each database's limit.
- Through the multitenant plugin release this engine pins: a tenant moved through JSON Lines or the portable dump keeps its relations, including a move split across several files, and a tenant clone copies many_to_many links. A SQL Server backup restores each row under its own id. Import, validate and restore outlast the admin listener's timeouts.
- Through the plugin releases this engine pins: a content save the content table refuses answers 409 or 422 instead of saving an entry no read serves. gRPC writes and bulk imports take a relation under its declared name. A webhook retry the receiver accepted is sent once. On SQL Server, concurrent API key counts and quota writes no longer deadlock and drop the count.
- Through the plugin releases this engine pins: a password reset longer than 72 bytes is refused before it spends the link, a first passkey registers on an account with no authenticator, a PII rule keeps priority 0, a region can be created closed and its coordinates cleared, a rate limit rule for every tenant can be saved, a media upload into a folder with dot segments works, and a storage provider whose keys will not decrypt is marked as such.
- Through the flow plugin release this engine pins: a flow reads a relation whose pivot table has a bounded name instead of failing with `invalid pivot table name`.
- Through the schema plugin release this engine pins: renaming a content type moves its pivot tables, their key columns, indexes and keys with it, so its relations and existing pairs work under the new name. A rename is refused with 409 when another tenant's definition relates to the type or defines the new name.
- Through the schema plugin release this engine pins: a content type rename that stops partway on MySQL holds both names until it finishes, so another tenant can no longer take over the moved table, and the plugin's start finishes a pending rename. Renaming a type also renames the indexes and keys of its own table.
- An engine pointed at a MySQL database created the way the documentation describes starts and completes setup. It turns on the driver's time parsing even when the URL leaves it out or turns it off, gives a database it has not yet installed into the collation its tables need, and takes its migration lock on MySQL 9, which has no MD5 function. A database that already holds the engine's tables under another collation is left as it is, with a warning.
- An erasure reaches accounts that have no password, such as those a SAML sign-up creates. They used to be skipped and kept their address.
- The token endpoint refuses a disabled or expired account, as the password sign-in does.
- Through the plugin releases this engine pins: when a field turns rich text, the values stored before, in entries and in translations, are sanitized in the background, including a change made while a plugin was stopped. Rows a tenant migration import or a backup restore writes appear in the console, and after a restore that cleared tables the console drops entries whose rows are gone. An erasure by address reaches what the sign-in plugins keep about accounts a tenant's identity provider created, however the request capitalizes the address, clears the directory's stored copy of the person, and no longer fails on the second person erased from one directory.
- A data subject export by address returns only the accounts of the tenants it covers. One address can hold an account in each of several tenants, and an export for one tenant also listed the other tenants' accounts, while an export of every tenant reported each tenant as matched and counted each account once per tenant. An account that names no tenant is exported with the default tenant, and an export of every tenant covers the default tenant on an install that turned tenants on after setup.
- On SQL Server a data subject export names each account by its id in lower case, the form every other answer uses. It used the upper case form, so a client matching ids as text did not find the account.

## [0.54.1] - 2026-10-11

### Fixed

- The admin console no longer reads a content type's whole table again after every engine restart, through the content plugin release this engine pins. Picking up entries written through the Content API, GraphQL, gRPC or a bulk import stores its progress, runs once per content type, and spends at most two seconds per list. Entries a flow writes through its content node appear in the console as soon as they are written.

## [0.54.0] - 2026-10-10

### Added

- `core.ContentReader`: a plugin reads a schema's generated content table one page at a time, every status included, ordered by id, through `ContentPage`. The engine host implements it and `ScopedHost` forwards it.

### Fixed

- Entries created, edited or deleted through the Content API, GraphQL or gRPC appear in the admin console, through the content plugin release this engine pins.

## [0.53.0] - 2026-10-10

### Security

- Requires Go 1.27.2, which fixes standard library advisories in crypto/tls, net/http, its bundled http2 and net/textproto (GO-2026-6603, GO-2026-6605, GO-2026-6607, GO-2026-6608, GO-2026-6610, GO-2026-6611, GO-2026-6613, GO-2026-6617).
- golang.org/x/net 0.60.0, which fixes the same http2 advisories in the module copy.

### Added

- The goroutine tunables carry a lifecycle switch, `ParallelLifecycleSetter`,
  that decides whether plugin caches warm and plugins stop through the
  parallel engine.

### Changed

- The engine boots a 100-worker pool, runs hooks in order, and warms and
  stops plugins one at a time. It no longer reads `GOROUTINE_ENGINE_ENABLED`,
  `GOROUTINE_ENGINE_POOL_SIZE`, `ASYNC_HOOKS_ENABLED` or `ASYNC_HOOK_TIMEOUT`.
  The goroutine engine plugin reads them and changes the primitives from its
  Start onward.
- Every goroutine the engine starts recovers from a panic and logs it under a
  name, instead of ending the process. The admin, API and setup servers stay
  the exception, because a server that died quietly would leave the process
  running with no listener. `make verify` refuses any other go statement that
  does not recover.
- golangci-lint 2.14.0. 2.13.2 cannot read what Go 1.27.2 compiles.

### Fixed

- `Host.Version` returns the build version. It returned the service name, so
  plugins reported `lyeve-core` as the engine version.
- The boot warning for a plugin with no capability grant says the grant can
  come from the host capability policy as well as from
  `RegisterPluginWithCaps`.
- `make bench-compare` compares the newest benchmark run against the one
  before it. It only ever reported one run.

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

