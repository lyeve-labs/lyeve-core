# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.56.0] - 2026-10-26

### Security

- Requires Go 1.27.3, which distrusts compromised intermediate certificates in crypto/x509 certificate verification (GO-2026-6809).
- The content API's token route no longer turns a correct password into a session for an account with a second factor enrolled. It answers with the same challenge as the console's sign-in, which a current code completes at the verification route, and accounts without a second factor receive their token as before.
- A second factor a plugin verifies itself, such as a passkey, now completes a password sign-in through the engine's own record of spent challenges. One challenge gives one session whichever factor completes it, and the session acts in the tenant the password step acted in, with the roles the account holds there now. Before, a passkey could sign only for the account's home tenant, so a member who signed in on another tenant's domain landed at home with home roles.
- A plugin allowed to read secret configuration no longer reads the session signing secret as well. That secret now needs a capability of its own, granted only to the plugins that verify sessions with it or open values sealed under it, and a secret name written with dots, hyphens or spaces is checked in the form the engine resolves it.
- The engine's own heap and allocation profile routes now honor the setting that refuses heap profile exports, which before held only for the profiler's routes. A heap export that is allowed is logged with the account that took it.
- A plugin reaches the engine's account store, which creates an account with any roles in any tenant, only when it holds the database write grant. Before, every plugin could reach it. Every plugin that uses the store already holds the grant.
- The data subject export and erase routes refuse, with a 422, an identifier that is not an email address, an account id or an IP address. Before, a bare word such as a first name was accepted, and erasure redacted that word in the records of anyone whose text held it.
- Subject export and erasure find an address written into a URL or an HTML page with its at sign encoded, which they missed before. An encoded plus, hyphen or underscore just before an address now reads as part of a longer address, so one person's request no longer reaches text about someone whose address ends the same way.
- The route that saves a plugin's settings refuses, with a 422 that names it, a key the plugin does not declare. Before, it stored any key, and because every plugin's saved settings are merged into one configuration, a key saved under one plugin could set another plugin's setting, such as a mail relay host.

### Added

- A plugin that reads a request body itself can answer a body past the limit with the engine's own 413 response.
- A plugin can register a tenant purge step that runs on every tenant delete, including a delete that skips the rest of the purge because the tenant holds no stored rows. A step that drains a buffer or clears state kept outside the database uses it, so that work is no longer lost when the tenant's tables are already empty.
- Plugins can clear a data subject's text through the engine's own erasure helper, which changes only rows whose key and text still match what was read and confirmed, so a row edited between the read and the clear keeps its edit. It splits a large clear into statements under the parameter limit of every supported database.
- A plugin that subscribes to content events learns who made the change: the user behind a session, the owner of an admin token, or the id an API key authenticates as, with the kind of credential. A write with no signed-in caller, such as a scheduled job, names nobody. Webhook, broker and realtime payloads keep their shape.
- A plugin that deletes one kind of data in bulk can ask which legal holds in the tenant cover that kind of data as a whole, such as a hold on the whole tenant, on a resource type or on a time window. A hold provider that does not answer the question reports nothing held. One that cannot answer reports the hold status as unavailable, even where the install lets an erasure proceed past a failed check.
- Through the plugin releases this engine pins: content history is opt-in and stores each version as a commit naming its field values, so a save that changes one field stores that field alone. Each tenant chooses which content types keep history, how long versions are kept and whether an activity log records who changed which entry. The server's configuration sets the defaults and can fix a setting for every tenant. A fresh install starts with history off, and a tenant that already holds content keeps history on.
- Through the plugin releases this engine pins: a super admin reads the content activity log across tenants, and reads it in full with PII masking on, as on the audit log.

### Changed

- Plugins can hash a password with the engine's configured hasher and read its length limit, so a password a plugin sets is stored the way creating a user stores it, under bcrypt or argon2id alike.
- A plugin that writes content rows with its own statements, such as a batch import, can now tell the engine to drop its cached content lists and rows, and the other replicas hear of it too. Before, a list read just before such a write kept serving the old rows until its cache entry expired.
- Plugins can ask the legal-hold check the way the engine's erasure does, including the install's setting for a check that cannot answer, so a plugin's own erasure route answers a failed check as the engine's route does.
- A panic the engine recovers, in a request, a background task, a scheduled job, a flow run or a call into a plugin, is now handed to every running plugin that takes on the panic reporting role, with the tenant, the route or task name, the value and the stack. The request still answers 500 and the panic is still logged.
- Plugins can apply the engine's rule for which tenant a log line about another tenant is filed under, and the plugin test kit can capture log lines through the engine's own handler, with the tenant and level it would apply.

### Fixed

- A session survives a refresh whose answer is lost on the way back. Trading the same refresh token again within a few seconds returns the same new token instead of a conflict, and a further retry in that window answers a conflict rather than signing the user out.
- A refresh that loses a race to a concurrent refresh answers a conflict on every cache backend a plugin provides, as it already did on the built-in one, instead of a service error.
- A password sign-in whose second-factor check cannot be read answers that the service is unavailable instead of rejecting the password, and it still issues no session.
- A refresh token store over a cache backend without compare-and-set no longer reports a rotation as won without checking it. The engine refuses to start when replicas share such a backend, and every backend that ships today supports it.
- The migrate command that marks one migration as applied takes the bookkeeping table to mark, so a version that ships in two of a plugin's migration sets can be marked. Without it the refusal names the option, and the boot failure for a plugin with more than one set prints the full command.
- A console log line no longer repeats the request id, trace fields, tenant or plugin. The value a line names itself wins, and the tenant and plugin always come from the request.
- Booting with an unencrypted database address or with Redis unreachable no longer writes plain text lines between the engine's JSON log lines. The production warnings now also name an unencrypted replica address, and Redis connection failures are logged as structured warnings.
- On SQL Server, a data subject erasure that runs while tables are being created or changed no longer fails when the server picks its catalog read as a deadlock victim.
- A plugin migration blocked on a lock another database session holds fails at once instead of being tried four times, which stalled a boot for about three minutes. A deadlock is still retried, and the last failed try returns without waiting again.
- Boot no longer warns that a tenancy plugin's membership routes are unreachable, and a plugin can be granted access to them again. The engine had listed the four routes as its own while never serving them.
- On SQL Server, deleting a tenant while another transaction creates a table no longer fails with a service error. The check each cleanup step makes for its own tables no longer waits on uncommitted table changes, so the delete is not chosen as a deadlock victim.
- On MySQL, migrations run in a session with the same settings as every other engine connection: in UTC, and counting changed rows rather than matched ones.
- Checking a cluster lock with a context that has run out no longer frees the lock. Before, the check closed the lock's database session, and another replica could take the lock while the first was still working under it.
- On PostgreSQL, the check for the engine's own tables finds them on a connection scoped to a tenant, whose search path names the tenant's schema first. Before, it reported them missing and the purge, erasure and audit steps that rely on it fell back to their defaults.
- Updating a content row through the content API no longer writes into a row that was soft deleted. Plugins also gain a content write that only updates an existing row and answers not found for a missing or deleted one.
- The database handle that refuses data changes, given to plugins for reads and schema work, now reads every statement of a query that holds several separated by semicolons, and refuses the query when any of them opens with a command that changes data. Before, it read only the first statement, so a read followed by a delete in one call ran the delete. The handle catches a plugin's own mistakes and is not a sandbox for SQL written elsewhere.
- The description of the setting that opens the shared address space to outbound requests says the email plugin honors it. An SMTP relay or API provider host in that range is accepted without an entry in the email plugin's own list of allowed private networks. Before, it said the email plugin ignored the setting.
- Through the plugin releases this engine pins: on MySQL, a data export with no field filter no longer fails while writing its file, or writes bytes from another entry into an entry's body or meta. The bulk import's lists of a job's rows and rejected rows no longer show another row's data either.
- Through the plugin releases this engine pins: on SQL Server, a write whose request quota check ran out of time no longer fails with 503. The check stops without closing the connection the write holds, and the write goes through uncounted.
- Through the plugin releases this engine pins: a legal hold whose resource id is a UUID typed in upper case now holds what it names on PostgreSQL, and a privacy erasure honors a hold set in any tenant. While a legal hold covers a tenant's content as a whole, content history retention deletes nothing in that tenant.
- Through the plugin releases this engine pins: deleting a content record removes its stored versions, which a delete used to leave listed.
- Through the plugin releases this engine pins: request capture stores up to a second of captures in one transaction, where it stored each request in a transaction of its own. A request, a read included, no longer costs the database a commit and a log write that waits for the disk. A capture shows in the list up to a second after its request.

## [0.55.0] - 2026-10-20

### Security

- Through the plugin releases this engine pins: a flow acts as the account that published it. Its content nodes, its content event trigger and the nodes other plugins contribute read and write only what that account may, with its field mask, and stop when the account is demoted, disabled or deleted. Run records show their values only to readers whose access covers them, and a flow published through an admin token acts as no account.
- Through the license library release this engine pins: a license's expiry, not-before time and grace period are judged by a clock that never runs backward, so setting the server clock back no longer keeps an expired license active.
- A draft or archived entry is no longer readable on the public content reads. The list, the read by id, the cursor list, relations, populate, revisions and the change stream serve only published entries to a caller who may not read drafts, a status filter other than `published` answers `403` for that caller, and a response that can carry drafts is sent `private, no-store`. Reading drafts takes both the read and the update permission on the content type, and an API key without an admin role also needs the read and update scopes on it. A change event that carries no status on a content type with draft and publish is held back from readers.
- Reading entries with their related entries populated no longer writes into the copy of an entry the engine caches. Two readers populating the same entry at once could stop the engine, and a relation one reader populated could appear in another reader's plain read of that entry, past that reader's own access rules and field masks. Every reader now gets its own copy.
- An audit entry written for a data subject's export or erasure names the subject by a SHA-256 digest of the identifier the request named, usually an email address, instead of the identifier itself. The log still proves each request happened, and anyone holding the identifier can find its entry. Entries written before this release are left as they are.
- A data subject erasure or export reaches only the accounts whose address is the one requested, apart from capitals. On MySQL and SQL Server a request for an address that holds no account reached the account of a spelling that differs only in an accent or in `ß` against `ss`, so an erasure could anonymize another person's account and an export could hand it out. The subject's own account is still found when it is stored with capitals, a trailing space or a decomposed accent.
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
- Through the MFA plugin release this engine pins: completing a sign-in's second factor with a passkey no longer gives the session roles the account does not hold in its own tenant. An account that signed in to another tenant where it is a member used to receive a session for its home tenant carrying that membership's roles, so a viewer at home who administers another tenant became an administrator at home.
- Through the synthetic monitoring plugin release this engine pins: a probe update that moves the target to another host needs each stored secret sent again, and a run drops its secrets on a redirect to another origin.
- Through the schema plugin release this engine pins: adding a relation to a content type that several tenants define clears dangling references only in the saving tenant's rows, and a definition that would store a field differently from another tenant's definition of the same table is refused.
- Through the plugin releases this engine pins: a bulk import through the content plugin refuses a relation into another tenant's entry or a missing one, as its dry run does. A draft the content status backfill lost to a deadlock on MySQL is projected again instead of staying readable on the public content routes. The ceiling of 50 enabled PII masking rules per tenant holds under concurrent writes.
- On an install with several tenants, an account that a tenant's identity provider, its directory or a self sign-up creates belongs to that tenant alone. It is found by the tenant and its address, so the same address can name a different account in each tenant, and a sign-up, a password reset or a magic link in one tenant no longer reveals whether another tenant holds the address. The sign-in plugins take part through the releases this engine pins.
- A sign-in refused because the account has no password, or signs in only through its identity provider, takes as long as a sign-in for an address nobody holds.
- The tenant a request resolves from its hostname decides which tenant's account a password sign-in or a token request reaches. A tenant named in the request body no longer overrides it.
- Through the plugin releases this engine pins: a SAML sign-in never signs in an account that holds the super admin role, whichever way the account was found, and a user the directory deactivated is refused at SSO sign-in however the directory capitalized the address. A magic link requested with a tenant named in the request body never creates an account, and password reset and magic link requests answer in one timing class whether or not the tenant holds the address.
- Through the plugin releases this engine pins: a super admin's SAML provider routes and OAuth linked account routes act in the tenant the X-Tenant-ID header names. A SAML provider created with the header used to land in the super admin's own tenant, so it signed people in there and not in the tenant named. The named tenant's linked sign-in accounts read as missing.
- Through the OAuth, SAML and magic-link plugin releases this engine pins: a sign-in completed on one tenant's domain while the request carries another tenant's session is refused. An OAuth callback or a SAML assertion there answers 403, and a magic link answers as an unknown link does and is not spent. Each used to sign the person in to the session's tenant on that domain.
- Through the OAuth, SAML and magic-link plugin releases this engine pins: a host mapped to the default tenant stays shared by every tenant's users. An OAuth callback, a SAML assertion and a magic link complete there in the tenant of the session or link they carry, as a password sign-in does. Each used to refuse another tenant's session or link on that host. A host mapped to any other tenant still refuses them.
- On PostgreSQL, a content read served a field removed from its content type until the removal was applied, and served, as empty values, the fields another tenant's definition of the same content type holds. Reads now build each entry from its content type's own fields, as they already did on MySQL and SQL Server.
- Through the gRPC plugin release this engine pins: the same holds for gRPC content lists, reads, creates and updates, which answer with the content type's own fields alone. On PostgreSQL and SQL Server their rows also carried every other column of the table the type's tenants share, and on every database they carried `tenant_id`, which no row carries now. A localized type's `_locale` is now served on MySQL too.
- A wrong password typed for an account of one tenant, on a request that carries a session of another tenant, is logged and recorded under the account's own tenant. The admins of the session's tenant used to read in their own log stream that the address held an account elsewhere on the install, and its id.
- On a tenant's own domain, a password sign-in or a token request acts in that domain's tenant. It admits an account whose home is that tenant, a member of the tenant with the roles its membership grants, and a super admin. Any other account is answered exactly as an address nobody holds, whether its password is right or wrong: nothing checks its password, counts the attempt toward a lockout, records its device or writes a log line. Such an account used to sign in to its own tenant on that domain, and wrong passwords typed there could lock it. A host mapped to the default tenant, such as the instance's own host mapped so anonymous reads name a tenant, is not a tenant's own domain. It stays shared by every tenant's users, who sign in, refresh and complete a second factor there as on a host that maps to no tenant.
- A refresh token for a session of another tenant is refused on a tenant's own domain, and that session ends. Earlier releases could leave such a session on a domain when an account of another tenant signed in there.
- A second factor for a sign-in that began in another tenant is refused on a tenant's own domain before the code is checked.
- A session signed for a passkey, single sign-on or magic link sign-in carries the roles the account holds in its own tenant, a membership that changed them included, apart from the super admin role, which only the account grants. Such a session used to carry the roles on the account itself, so an administrator demoted through a membership signed in again as an administrator.
- Through the audit plugin release this engine pins: a retention run whose archive upload fails reports a fixed reason in `archive_error` instead of the storage backend's error, which named the bucket, the endpoint and the request.
- Through the email plugin release this engine pins: a provider's SMTP relay is dialed at the address that passed the private and reserved address check, so a name that resolves elsewhere at send time can no longer reach an internal address.
- Through the email plugin release this engine pins: a provider save with a refused SMTP host or `api_base_url` answers a fixed sentence instead of the resolver's error or the internal address the host resolved to.
- Through the email plugin release this engine pins: a relay host or API base URL in the carrier-grade NAT range (100.64.0.0/10), the rest of 0.0.0.0/8 or the local-use NAT64 prefix (64:ff9b:1::/48) is refused, and a NAT64 (64:ff9b::/96) or 6to4 (2002::/16) address is judged as the IPv4 address it embeds. 100.100.100.200 is Alibaba Cloud's metadata endpoint, and 64:ff9b::a9fe:a9fe reached 169.254.169.254 past the link-local check. A relay behind a NAT64 translator that reaches a public address still saves and sends, and an address inside `SMTP_ALLOWED_PRIVATE_NETWORKS` stays allowed.
- Through the email plugin release this engine pins: an API provider sends through a client that resolves its host once, checks the address the way a save does, under `SMTP_ALLOWED_PRIVATE_NETWORKS`, connects to the address it checked and follows no redirect, so a 3xx answer fails the send. A tenant admin could save a name that resolved publicly, point it at a carrier-grade NAT, NAT64 or 6to4 address such as 100.100.100.200 and have the next send deliver the request they wrote there, with up to 512 bytes of the answer in the error. An API provider inside the allowed networks, which used to save and then fail every send, now sends.
- Through the email plugin release this engine pins: the install relay is checked at every send. Its host is resolved once, a private or reserved address outside `SMTP_ALLOWED_PRIVATE_NETWORKS` is refused, and the send connects to the address it checked. A super admin can set the relay in the console while the private networks are the operator's alone, so a stolen super admin session could point every send at an internal address. An install whose relay listens on loopback or a private network must list every address the relay's name resolves to in `SMTP_ALLOWED_PRIVATE_NETWORKS`, both 127.0.0.1 and ::1 for localhost, or its sends are refused after the upgrade.
- The outbound guard refuses the shared address space 100.64.0.0/10, where some cloud metadata services answer, the local-use NAT64 range 64:ff9b:1::/48, the IPv4-compatible and SIIT forms of IPv6, and a NAT64 (64:ff9b::/96) or 6to4 address whose embedded IPv4 address it refuses. Webhooks, flows, identity providers, JWKS fetches and alert channels can no longer reach those addresses. An install that reaches its own services over Tailscale sets `LYEVE_OUTBOUND_ALLOW_CGNAT=true` to allow 100.64.0.0/10, or those requests are refused after the upgrade. Mail providers keep their own check and need 100.64.0.0/10 in `SMTP_ALLOWED_PRIVATE_NETWORKS` instead.
- Through the query monitor plugin release this engine pins: the SQL Server slow request capture redacts the quoted string values in the cached plan it stores, as the other SQL Server plan capture already does. A cached plan keeps the values it was compiled for, so a string a statement was bound with reached the stored plan, and because the plan is whichever statement the server ran last, the value could belong to another tenant than the entry it was filed under.
- Through the flow plugin release this engine pins: secret variable values are masked in a failed run's error everywhere it is read, including the answer to an HTTP caller and the `flow.run_failed` event.
- Through the flow plugin release this engine pins: a flow made from the "Orders with customers and shipments" template answers only the orders of the `customer_id` the request names. It used to return every customer's orders whatever `customer_id` said, and served one status's cached answer for another. A flow already made from the template keeps the query it was made with, so make it again from the template or add the filter by hand.
- Through the gRPC plugin release this engine pins: a license change that drops the grpc feature refuses every call on the gRPC port at once, and the plugin's health check reports it as not serving. Calls on the port used to be served until the engine restarted.
- A production start refused for a default database login no longer prints the database password, and neither do the two lines a configured read replica logs at each start. Each prints its connection string with the password replaced by `***`, in every form the engine connects with: a URL, the MySQL driver form and a PostgreSQL keyword string.
- The super admin role comes only from the account itself. A membership, in the account's own tenant or in any other, never grants it and never takes it away, and it still decides every other role. A membership that held the role used to make an account a super admin in that tenant while the account held a lesser role, which the plugins that protect super admins could not see, and a super admin given a membership in their own tenant without the role lost it at their next sign-in there. A session signed before this release that carries the role from a membership keeps it until the session refreshes, which reads the roles again, or expires, after 15 minutes by default.
- Plugins can no longer have the engine sign a session unless the build grants them `core.CapSessionSign`, which this engine gives only to the magic link, MFA, OAuth and SAML plugins. A plugin cannot claim it through its own capability declaration. Any plugin used to be able to have a session signed for any active account, a super admin's included, which is a sign-in as that account. A refused call returns `core.ErrCapDenied` and is logged.
- A second factor completes one sign-in. The challenge, and a recovery code sent to answer it, are spent before the session is signed, so two requests that send them at once can no longer both receive a session, and the one that loses answers 401 `AUTH_CHALLENGE_EXPIRED`. A recovery code whose spend cannot be written refuses the sign-in with 503. The sign-in used to go ahead and leave that code usable.
- One credential holds at most 8 content change streams open on an instance, and one tenant's callers together at most 128. The next stream past either cap answers 429 with `Retry-After: 10` before anything is sent. A session counts against its account and an API key against the key. An open stream also checks its credential at every heartbeat, so a sign-out or a revoked key ends an idle stream within 10 seconds and frees its slot.
- Through the MFA plugin release this engine pins: adding a second factor, or removing a passkey, needs proof of a factor the account holds: a current authenticator code, an unused backup code, or a passkey assertion against a challenge from `POST /api/admin/mfa/proof/begin`. A signed-in session alone used to set up an authenticator app over an active one, add one beside a passkey and replace the backup codes, or register a passkey and then sign in with it and no password. An account with no factor still enrolls with its session. A setup or registration without proof answers 400 (`mfa.totp.current_code_required`, `mfa.webauthn.proof_required`), and a wrong proof answers 422 (`mfa.totp.current_code_invalid`, `mfa.webauthn.proof_invalid`) and counts toward the verify lockout. A setup begun without proof never activates once the account holds a factor and answers 409 (`mfa.totp.setup_unproven`, `mfa.webauthn.factor_added`), so a setup left pending across the upgrade has to be started again. An API client that registers or removes a passkey sends the proof itself, and setting up an authenticator app from the admin console on an account that holds a factor needs the console release that sends it.
- Through the SCIM plugin release this engine pins: a directory takes over an existing account, at user create or at SAML sign-in, only when the account holds neither the admin nor the super admin role, in its own roles or in any tenant membership. A refused takeover answers 409 `uniqueness`, as a duplicate user does. A directory can no longer disable or delete an administrator through a link written before this release or for a member promoted later, and such a write answers 409 before the record changes. One account is linked by at most one directory record. On upgrade, migration 007 unlinks duplicate links and keeps the one SAML sign-in reads first.
- Through the storage plugin release this engine pins: an Azure provider whose endpoint carries a SAS token, or any other query, is refused on create and update with 400. The endpoint was stored unencrypted and returned by the provider get and list routes, so a tenant admin could read the token and use it outside the engine. A provider saved before this release is returned without its query and keeps its token for its own requests until it is edited. An update that sends the endpoint back as it was read saves it without the token.
- Through the schema plugin release this engine pins: a content type save refused because another tenant defines the same table differently, keeps a relation the save would move, or uses a key the save would block no longer logs the other tenant's name and how it stores the field under the saving tenant, where that tenant's admins could read it in their log view. The 409 answers are unchanged.
- Through the MFA plugin release this engine pins: a passwordless passkey sign-in, a recovery sign-in and a cross-device sign-in on a tenant's own domain answer 401 when the account's home tenant is another one. They signed the account into its home tenant and set that session on the other tenant's domain.
- Through the analytics plugin release this engine pins: a self-service data subject erasure asks the legal hold checker before it changes anything, as the engine's own erasure does. An account under an active hold gets 409, and a hold check that fails answers 503.
- A super admin's data subject export across every tenant, and the cleanup that runs when a tenant is deleted, log their lines about another tenant under no tenant. They were stamped with the super admin's own tenant, whose admins could read in their log views which tenants hold the subject and the name of the tenant being removed. Only an operator reading the whole log sees them now, and a line about the request's own tenant keeps its stamp.
- Through the plugin releases this engine pins: a super admin's work on a tenant other than their own writes its log lines and audit entries under no tenant. That covers a tenant create, update, delete or clone, an import, validation, restore, migration or archive of that tenant's data, a data residency assignment, replication or move, and the search, A/B testing and schema plugins' lines about a deleted tenant. They were filed under the super admin's own tenant, whose admins could read the other tenant's slug or id, its activity and the database driver's text of its failures in their log view and audit log. Only a super admin who is not scoped to a tenant reads them now, and work on the requester's own tenant keeps its tenant.
- A super admin's request writes its access line under no tenant, so the admins of the tenant the request acted in no longer read it, including a path that names another tenant. Only an operator reading the whole log sees these lines, and that includes a super admin's lines about their own tenant. Other callers' access lines keep their tenant.
- On a host whose tenant differs from the account's, the sign-in lines about the account are filed under the account's own tenant: the missing roster warning, the sign-in window refusal and the second factor failures, which the host tenant's admins used to read. Lines whose subject is another tenant or the whole install carry no tenant: an admin token a super admin issues for another tenant and its audit entry, a token refused on another tenant's host, the session revocation of a password set on another tenant's account, and the erasure of an address across the install.
- Through the plugin releases this engine pins: more of a super admin's work on another tenant, or on the whole install, writes its log lines and audit entries under no tenant. That covers a refresh of another tenant's analytics figures or of every tenant's, another tenant's usage, snapshots and quota, another tenant's log levels and the install's log settings, another tenant's email branding, a data residency region change and the residency report, a search reindex of every tenant, and the remaining tenant backup, domain mapping and schema listing lines and entries. Only a super admin who is not scoped to a tenant reads them now.
- An anonymous device sign-in request on a host mapped to a tenant files its lines about the install's own sign-in table under no tenant: an unset console address, a failed cleanup of expired requests, the install's limit of open requests and a failed insert. When the command line client polls on another tenant's host, the refused exchange line, the failed device history line and the device history row go under the tenant the approval bound. All of them were filed under the host's tenant, whose admins could read them.
- Through the plugin releases this engine pins: the rest of a super admin's work on another tenant, or on the whole install, writes its log lines and audit entries under that tenant or under no tenant, never under the super admin's own. That covers capture replays, rate limits, firewall rules, sign-in providers, model prices, worker tunables, the IP reputation list, storage providers, error codes, telemetry exports, second factor resets, API metrics, event replays, audit exports, cross-tenant erasures, profiler exports and the license routes.
- Through the PII masking plugin release this engine pins: a super admin's read of personal data is recorded in the PII access log under the tenant the request names, on the tenant members route or through the audit log's tenant filter, and under no tenant on every other route. Those reads were recorded under the tenant the request carried, whose admins could read which other tenant's personal data a super admin read.
- Through the plugin releases this engine pins: a password reset requested on one tenant's host for an address another tenant holds logs a failed read under the account's own tenant, and a failed read of the region list on the public region routes logs under no tenant. Both were filed under the tenant of the host the request arrived on, whose admins could read them.
- Through the request capture plugin release this engine pins: a capture replay goes to the engine's own listener for its path and returns a redirect instead of following it. It took its target from the replay request's Host header, so a tenant admin could make the engine fetch an internal address and read part of the answer. A replay also carries the replaying admin's credential only to this engine. A capture recorded from a request line that named an outside host was replayed there with that credential.
- Through the request capture plugin release this engine pins: a super admin's replay of another tenant's capture runs in that tenant. It ran in the super admin's own tenant, so that tenant captured the other tenant's request, and the replay record filed under the other tenant held the super admin's tenant's data.
- Through the rate limit plugin release this engine pins: the warning written when Redis cannot be reached is filed under no tenant. It was filed under the tenant of the request that met the outage, whose admins could read the install's Redis address in it.
- Through the sign-in provider and second factor plugin releases this engine pins: a public sign-in request on a host mapped to a tenant files its lines about the install under no tenant: an unset base URL, a refused provider address, a provider secret that would not open, a provider's open circuit or failed discovery, and a failed cleanup of cross-device sign-in sessions. They were filed under the host's tenant, whose admins could read them.

### Added

- A plugin can give a public route on the general limit a limit of its own at runtime, through `core.RegisterPublicRouteLimitResolver`. The public limiter asks it on each request to such a route, and a limit it answers replaces the general per-address cap there, higher or lower. A route with a limit of its own, such as sign-in or the token endpoint, keeps that limit whatever a resolver answers, and so does every route under `/auth`, which guards a credential: `PUBLIC_RATE_LIMITS` stays the way to move them. A HEAD request follows its GET route's limit and shares its bucket.
- `core.EngineRateLimitsProvider` lists every public route on the general limit that can take a limit of its own, as scope `general`, with the general limit's values. HEAD routes and routes under `/auth` are not listed.
- A plugin route can declare the media types it answers in, such as a stream, CSV or a file, through `MediaTypes` on its `core.RouteDecl`, and the API description shows them. A route that declares nothing is described as before.
- A plugin route can declare the response headers a page on another origin may read and the request headers it accepts, through `ExposeHeaders` and `AllowHeaders` on its `core.RouteDecl`. The engine adds them to the CORS lists every router sends, and the origin check is unchanged.
- The `contentrules` package gives a plugin that writes content the field defaults and validation rules the Content API applies: `PrepareCreate` for a new entry and `ValidatePatch` for a partial update.
- A tenant purge handler can learn whether the tenant delete it ran in committed or rolled back, through `core.OnTenantPurgeOutcome` on the context the delete hands it. Work a handler does outside the database can then wait for the commit. Handlers that do not ask behave as before.
- A free install refused another admin seat gets a link to where that ceiling is lifted, in the refusal body and its upgrade header, when the build's licensing names one. A link that is neither https nor a console path is dropped, and a build whose licensing names none refuses as before.
- Each tenant sets how many days an account its identity provider created may still sign in with a password, a reset or a magic link after its last sign-in through that provider, from 0 to 365 with 30 as the default, through `GET` and `PUT /api/admin/auth/provider-window`. 0 allows sign-in through the provider only. Only a signed-in session can read or change it, a tenant admin changes only their own tenant, and each change is audited with the old and new values.
- A plugin's subject eraser or exporter can match an address the way the engine does, through `compliance.AddressCandidates`, `compliance.AddressForms` and `compliance.SameAddress` for a column that holds an address, and `compliance.AddressTextForms` and `compliance.NamesAddress` for text that names one. SQL selects the candidate rows and Go keeps those that hold the address.
- A host resolver that picks a tenant the host does not name, such as the only tenant of an install, marks it with `core.WithFallbackTenantID`, and `core.IsFallbackTenant` reports the mark. The request acts in that tenant and records no host tenant, so a sign-in on the instance's own host is not refused as one completed on another tenant's domain.
- Through the plugin releases this engine pins: a tenant's admin can abandon a content type rename that stopped partway and cannot finish, which puts every part it moved back under the old name and frees both names. A refusal at a free ceiling on tenants, a tenant's admin seats, OAuth providers, review workflows or locales carries a link to the pricing page opened on the add-on that lifts it. The content and localization plugins list a tenant's pending rich text repairs.
- Through the flow, email and media plugin releases this engine pins: a refusal at the free ceiling on flows, email providers or templates, or media variants carries a link to the pricing page opened on the add-on that lifts it.
- Through the webhook, API key, cache, synthetic monitoring and permissions plugin releases this engine pins: a refusal at the free ceiling on webhook endpoints, API keys, cache rules or probes, or on rules for a fourth role, carries a link to the pricing page opened on the add-on that lifts it.
- A plugin can start a tracked worker meant to run until shutdown, such as a periodic sweeper, through `GoLongLived` on `engine.GoroutineTracker`. Shutdown waits for it and the tracker's snapshot counts it, but the leak check never reports it.
- `core.WithinTransaction` marks a context as carrying a transaction that every query made through `Querier` with that context joins, and `core.InTransaction` reports the mark.
- `core.AuthorizeContent` is the access decision every content transport makes before it reads or writes an entry, and it refuses with `ErrNoCaller`, `ErrSchemaNotAllowed`, `ErrScopeDenied` or `ErrPermissionDenied`. `ErrHiddenFieldQueried` refuses a filter or sort that names a field the caller cannot read. `CanReadDrafts`, `DraftPermissionChecker`, `ContentHiddenFields`, `ContentFieldMask` and `HiddenFields` resolve what a caller may read, and `ContentPublished`, `ContentEventPublished` and `ContentStatusKey` read whether a row or an event payload is public. `FlowInvocation.Publisher` carries the user who published a flow, whom a node that reads or writes content acts as.
- `core.HostRole` names each service one plugin supplies to the engine and to the other plugins: `HostRolePermissionChecker`, `HostRoleSchemaEngine`, `HostRoleEmailSender`, `HostRoleFlowDefinitionValidator`, `HostRoleFlowInvoker`, `HostRoleContentEntryWriter`, `HostRoleContentLocalizer`, `HostRoleRecordRevisionStore`, `HostRoleStorage` and `HostRoleUserProvisioner`. `HostRoleRegistry` and `HostRoleOwnerRegistrar` record which plugin holds each role, and refuse with `ErrHostRoleHeld` when another plugin holds it or `ErrHostRoleNotHeld` when a plugin clears a role it does not hold. `CapRoles` lists the capabilities that only appoint a plugin to a role, `PermissionCheckerOf` reads the checker in force at the call, and `plugintest.WithPermissionChecker` registers a checker on a test host.
- A tenant-scoped sign-in finds, creates and stamps accounts through `core.TenantAccountStore`, with `TenantAccount`, `NewTenantAccount`, `TenantAccountProvider` and the host's `TenantAccounts`, and `TenantAccount.SignInEmail` returns the address a session for the account carries. `TenantLocalEmail` and `IsTenantLocalEmail` build and recognize the address a tenant-local account stores. `HostTenantIDFromCtx` and `WithHostTenantID` carry the tenant a request's host resolved to before any credential was read, and `MembershipRoles` returns the roles a session acting under a membership carries.
- `core.ProviderWindowProvider`, `ProviderWindowAdmits`, `ProviderWindowSettingKey`, `ProviderWindowDefaultDays`, `ProviderWindowMaxDays` and the host's `ProviderWindowDays` and `ProviderWindowAdmitsAccount` state how long after its identity provider last signed an account in that account may still sign in by password, password reset or magic link.
- `core.CheckRelationTargets`, `RelationValues`, `RelationValue` and `RelationTargetError` refuse a relation value that names no row the writing tenant holds in the target schema, and `PivotNameLimit` is the longest pivot table name a new `many_to_many` field is given.
- `core.TenantPurgeOutcome` is the function `OnTenantPurgeOutcome` registers, and `WithTenantPurgeOutcomes` prepares the context a tenant delete hands its purge handlers. `PublicRouteLimitResolver` and `ResolvePublicRouteLimit` answer the limit an operator set on a public route. `RouteMediaType`, `RouteDoc.ResponseMediaTypes` and `ParseResponseMediaType` declare the media types a route's success body is served as.
- `httpx.StreamDeadline` and `httpx.NewStreamDeadline` keep a long-lived response's write deadline ahead of its next write. `licensing.CeilingLinker` and `licensing.CeilingURL` say where an operator lifts a capacity ceiling the license states. `compliance.SubjectDigest` is how an audit entry names the subject of an export or an erasure. `runtime.Version` answers the binary's version flag, and `runtime.Usage` refuses an argument nothing answered.
- `lyeve migrate mark-applied --plugin <name> --version <version>` records a plugin migration as applied without running its script. It repairs an install whose migration record was lost while its schema kept the change, so every boot failed on an object that already exists, and such a boot names the command. It runs from a shell on the engine's host against the database the configuration names, and has no HTTP route. It refuses a plugin that is not compiled in or does not declare its migrations, a version the plugin does not ship for that database, a version already recorded as applied, and a run while an engine holds the plugin migration lock. A plugin declares its migrations through `core.MigrationDeclarer` and `core.MigrationTree`, and `runtime.Migrate` answers the command in a main of its own. Through the plugin releases this engine pins, every plugin that keeps tables declares them, except the cluster, permissions, SCIM, search, storage, synthetic monitoring, telemetry and usage plugins.
- A plugin that sets a password checks it against the policy the engine's own routes apply, through `core.PasswordPolicyValidator`, which answers a refusal as a `core.PasswordViolation` carrying the engine's error code and parameters. A host without the role answers `core.ErrHostRoleUnsupported`. The `plugintest` host applies the engine's default policy, and `mockhost.SetValidatePassword` sets another.
- Through the MFA plugin release this engine pins: `POST /api/admin/mfa/proof/begin` issues a passkey challenge whose only use is to prove a factor the account holds before another factor is added or a passkey is removed. No sign-in or step-up accepts it, the request that uses it spends it, and it answers 404 when the account holds no passkey. It shares the verify rate limit.
- `compliance.WithoutTenant` files an audit entry under no tenant, so only a super admin who is not scoped to a tenant reads it. It is for an action a request takes on a tenant other than its own. `compliance.RecordAuditFromRequest` and `compliance.RecordAuditFromRequestWithState` take audit options.
- Through the analytics plugin release this engine pins: the plugin computes the previous UTC day's figures for every tenant on its own, under a cluster lock, so `GET /api/admin/analytics/daily` and `/summary` fill without an outside scheduler.

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
- Through the webhook plugin release this engine pins: an incoming webhook's field map target that the content type does not define is left out of the row, and the rest of the delivery is stored. That covers a field removed from the type, a column the engine sets such as `created_at`, an unknown name, `id` and `tenant_id`, and a name that is no identifier. The 201 answer names each target it left out in a `left_out` list, absent when there is none, and each one is logged as a warning without its value. An unknown target used to answer 503, so the sender retried a delivery that could never succeed, and a mapped `created_at` used to be written. A target spelled in another case than its field, such as `Title` for `title`, is stored under the field as before. Two targets that name one field answer 422 instead of 503.
- Through the plugin releases this engine pins: a closed region cannot be the default, and a SCIM connection's deprovision action is `disable` or `delete`.
- A session's email claim is no longer unique on an install with several tenants. An account a tenant's identity provider created carries its address there, and the same address can sign in to another tenant's account, so identify an account by its id. Migration 011 adds the account's provider address and its last provider sign-in, and runs at the next start.
- Through the flow plugin release this engine pins: an update of a datasource whose stored secret no longer decrypts answers 409 unless it clears the secret with `"secret": null` or sends a complete new one. It used to answer 503 whatever it sent, so deleting the datasource was the only way out.
- JSON responses escape U+2028 and U+2029, write an invalid UTF-8 sequence as U+FFFD, and compact a stored JSON value, such as a schema definition or a configuration export, before sending it. The cache tag of such a response changes once. A stored value that is not valid JSON is no longer sent as broken JSON, and its response carries the status with an empty body.
- A super admin who signs in on a tenant's own domain acts in that domain's tenant. They used to act in their home tenant there.
- Through the error tracking plugin release this engine pins: each panic site opens an alert of its own after the upgrade, and an alert recorded before it gains no more occurrences. The old alert keeps its status, acknowledgment and assignee and passes none of them on, so a panic that was ignored, acknowledged or assigned there comes back as a new open alert. Resolve or ignore the old alerts once their panics have reappeared.
- Through the GraphQL plugin release this engine pins: lists return the newest rows first, the order the Content API lists in, instead of an arbitrary id order.
- Through the cache plugin release this engine pins: `GET /api/admin/cache/entries` answers with the list envelope the other admin lists use (`data`, `total_count`, `limit`, `offset`), ordered by key and paged, instead of `entries` and `note`.
- Through the flow plugin release this engine pins: a flow expression may no longer use `$env`, and a draft that does cannot be published. Every value a flow reads has a name of its own.
- Through the flow plugin release this engine pins: a datasource read lists `secret_headers`, the names of its stored secret headers and never their values.
- Through the GraphQL plugin release this engine pins: `runFlow` answers an input that is not an object with `INVALID_INPUT`, the code every other input refusal carries, instead of `BAD_USER_INPUT`.
- Through the GraphQL plugin release this engine pins: a subscription socket runs at most 32 subscriptions, and one more is refused with `TOO_MANY_SUBSCRIPTIONS` while the rest carry on. A query or mutation sent over the socket answers `OPERATION_NOT_SUPPORTED`, so send it as a POST.
- Through the MFA plugin release this engine pins: a verify whose authenticator setup changed while its code was checked answers 409 and asks for setup to start again. It used to answer 404.
- Through the audit plugin release this engine pins: `POST /api/admin/audit-log/prune` answers 400 to a cutoff later than now. A retention archive's object key keeps a separator after the policy's `archival_path_prefix` and carries a run id, as `<prefix>/<event type>/<time>/policy-<id>-<run id>.csv`.
- Through the storage plugin release this engine pins: a write whose key holds a `.` segment, such as `tenant_x/./a`, is refused with 400. An object already stored under such a key stays readable and removable.
- Through the synthetic monitoring plugin release this engine pins: a probe create or update that names one header twice answers 400, and so does a create that sends the mask `***` as a token, password or sensitive header.
- Through the localization plugin release this engine pins: `GET` and `POST /api/v1/localization/resolve` run on the engine's general public limit instead of a limit of their own at 30 requests a second per address.
- Through the rate limit plugin release this engine pins: the overview no longer repeats the general limit for each public route on it. Those routes are listed at `/api/admin/rate-limits/public-routes`.
- The engine refuses an argument it does not know, such as `--help` or a mistyped flag, with a usage line and exit status 2 before it starts. It answers only `healthcheck`, `migrate` and the version flag.
- A start stops when the engine cannot open the pool that keeps plugins' raw queries from changing data, or the pool that holds leader locks, within five seconds. It used to log a warning and run on without them for the life of the process, so plugins' raw queries ran unguarded and leader locks held connections on their callers' pools.
- Through the search plugin release this engine pins: a search snippet is HTML. The document text in it is escaped and only the `<mark>` pairs around each match are live, so a consumer renders a snippet as HTML. A consumer that renders it as text shows entities such as `&amp;` in place of the characters they stand for.
- Through the bulk import plugin release this engine pins: on an install whose `DATABASE_MAX_CONNECTIONS` is below 4 (the default is 25), an import for any tenant but the default one is refused with 409 and a message naming that setting. A running import whose tenant lease fails ends failed with "the import could not confirm its tenant".
- `security.MFAStore` spends a recovery code through `ConsumeBackupCode`, which reads and removes it in one step, in place of `UpdateBackupCodes`. A store that implements the interface has to provide the new method.
- The `plugintest` host answers `HasFeature` from the capabilities a test grants, as the engine answers both from one license. It answered yes to every feature, so a gate written against `HasFeature` read as licensed in every test that did not override it. A test that needs a gate open grants its name through `WithCapabilities`.
- Through the password reset plugin release this engine pins: a reset confirm applies the engine's password policy before it reads the reset link. A password the policy refuses answers 422 with the engine's code and parameters (`AUTH_PASSWORD_TOO_SHORT`, `AUTH_PASSWORD_TOO_COMMON`, `AUTH_PASSWORD_NEEDS_COMPLEXITY`, or `AUTH_PASSWORD_TOO_LONG` with the limit in `max`), and the link stays usable for a better password. The plugin checked only its own 12-byte minimum and answered a short or long password with 400, so a client that handled that 400 has to handle 422.
- Through the search plugin release this engine pins: on SQL Server with the native provider, the plugin logs one line at start saying that search has no full-text index and scans every entry's title and body, and that an external `search_provider` indexes content instead.
- Through the analytics plugin release this engine pins: API call figures answer null in the daily, summary and export routes, and so does the dashboard's `api_calls_24h`, because nothing collects them. Every stored count was 0, and a migration turns the stored zeros into null.
- Through the request capture plugin release this engine pins: behind a proxy, a capture replay goes straight to the engine's listener instead of out through the proxy, and a super admin's replay of a capture with no tenant is refused with 422.

### Removed

- Through the cache plugin release this engine pins: `GET /api/admin/cache/metrics` and `POST /api/admin/cache/warm`. No warmer was ever registered, so a warm filled nothing a read uses, and the metrics counted nine per-keyspace query caches that nothing read through, so they stayed at zero. A flushed path fills again on its next read.
- Through the cache plugin release this engine pins: the `cache_invalidation_strategy` and `cache_ttl_<keyspace>` settings of those query caches. The response cache keeps its purge on content writes, and a tenant's flush purges that tenant's cached responses.

### Fixed

- A handler can read which tenant the request's host belongs to even when the request carries another tenant's session. The session's tenant still decides where the request acts. A sign-in completed on one tenant's domain under another tenant's session used to have no way to tell, and the sign-in plugins this engine pins now refuse it.
- Through the multitenant plugin release this engine pins: on an install with one tenant, a sign-in on the instance's own address under a default tenant session is no longer refused. The resolver marks the tenant it picks because the install has only one, so the engine no longer takes that tenant for the owner of the address, and the OAuth, SAML and magic-link sign-ins accept the session again.
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
- Through the content, localization and schema plugin releases this engine pins: renaming a content type moves its console entries, their revisions and its pending rich text repairs to the new name in the background, and an abandoned rename moves them back. The console used to list a renamed type as empty, and a console edit of one of its entries was reported saved while the Content API kept serving the old value. The content plugin's first start indexes entries and their revisions, and on Postgres and SQL Server writes to those tables wait while the indexes build.
- Through the content plugin release this engine pins: a delete that takes other rows with it, through the Content API, the console or a restore that clears tables, removes their console entries too. The console used to keep listing entries the Content API no longer served. A console list that adopts rows from the table stops inside its two second budget.
- Through the schema plugin release this engine pins: the rich text outbox no longer grows without end. Once a day, rows older than seven days that every reader has passed are deleted.
- Through the schema plugin release this engine pins: content types created at the same moment on MySQL or SQL Server no longer deadlock each other or fail with "schema migration failed". A create numbers its log entries from a plain read under the lock it already holds for its table, where it used to read with a lock on the whole log, and a create the database still chooses as a deadlock victim is retried, as it already was on PostgreSQL. On MySQL a create that the retry saved no longer logs some of its statements twice.
- Through the schema plugin release this engine pins: a tenant deleted and created again under the same slug can define its old content types again and store entries in them, on every instance of the install. The new definition used to answer 200 without building a table, so every write into it answered 503. A preview of that definition lists the statements its save runs, where it used to list none.
- An engine pointed at a MySQL database created the way the documentation describes starts and completes setup. It turns on the driver's time parsing even when the URL leaves it out or turns it off, gives a database it has not yet installed into the collation its tables need, and takes its migration lock on MySQL 9, which has no MD5 function. A database that already holds the engine's tables under another collation is left as it is, with a warning.
- An erasure reaches accounts that have no password, such as those a SAML sign-up creates. They used to be skipped and kept their address.
- The token endpoint refuses a disabled or expired account, as the password sign-in does.
- Through the plugin releases this engine pins: when a field turns rich text, the values stored before, in entries and in translations, are sanitized in the background, including a change made while a plugin was stopped. Rows a tenant migration import or a backup restore writes appear in the console, and after a restore that cleared tables the console drops entries whose rows are gone. An erasure by address reaches what the sign-in plugins keep about accounts a tenant's identity provider created, however the request capitalizes the address, clears the directory's stored copy of the person, and no longer fails on the second person erased from one directory.
- A data subject export by address returns only the accounts of the tenants it covers. One address can hold an account in each of several tenants, and an export for one tenant also listed the other tenants' accounts, while an export of every tenant reported each tenant as matched and counted each account once per tenant. An account that names no tenant is exported with the default tenant, and an export of every tenant covers the default tenant on an install that turned tenants on after setup.
- On SQL Server a data subject export names each account by its id in lower case, the form every other answer uses. It used the upper case form, so a client matching ids as text did not find the account.
- Through the plugin releases this engine pins: with an encryption key configured, an OAuth provider signs people in on every tenant. Its client secret used to open only on sign-ins in the tenant of the super admin who saved it, and a sign-in on any other tenant failed. A provider whose secret a super admin outside the default tenant saved still signs people in on that tenant, and its first sign-in there stores the secret so that every tenant opens it from then on.
- Through the plugin releases this engine pins: a data subject export lists only the magic links and their audit entries, the AI transcripts and messages, and the support conversations and feedback of the tenants it covers. Each used to list every tenant's rows for the address, so an export for one tenant carried the other tenants' rows, and an export of every tenant repeated each row once per tenant. Rows stored with no tenant are exported with the default tenant.
- Two saves of different engine settings at the same time both keep their change. The settings save, the provider window save and a configuration bundle's apply each read a tenant's settings, changed their own keys and wrote all of them back, so when two overlapped the later one dropped the earlier one's change without an error. Each now locks the tenant's settings while it reads and writes them, on PostgreSQL, MySQL and SQL Server.
- Through the flow and email plugin releases this engine pins: a data subject export lists only the flow runs and steps and the email records of the tenant it covers, and an email erasure reaches every tenant. An export for one tenant used to list the other tenants' rows, and an email erasure reached only the tenant the request resolved to, usually `default`, while it reported itself complete.
- Through the flow and email plugin releases this engine pins: a data subject export or erasure takes a flow payload or an email record only when it names the subject's address whole and in its own spelling, with case folded. An export for `ann@x.example` used to carry the runs naming `joann@x.example` or `ann@x.example.org`, and on MySQL and SQL Server an accented or `ß` spelling matched another address, so one person's erasure redacted another person's records. An address still matches its own rows in any case and in another Unicode form.
- Through the email plugin release this engine pins: a send to several recipients writes one outbox row per recipient, so an erasure or export reaches each person exactly, a long recipient list is no longer cut at 255 characters, and the dashboard counts sent mail per recipient. A request that names the subject by account id reaches the rows of the account's address.
- Through the email plugin release this engine pins: a tenant the operator withholds `email-pro` from is held to the free tier, with one SMTP provider, five templates, no API transport, no trigger writes and no analytics or bounce routes. The capability used to be read for the whole install, so withholding it from one tenant changed nothing.
- Through the localization plugin release this engine pins: a tenant the operator withholds `localization-pro` from is held to two locales, and other tenants on the install keep it. The capability used to be read for the whole install, so withholding it from one tenant changed nothing.
- Through the flow plugin release this engine pins: a subject export on SQL Server succeeds when the subject is named in more than about 2100 run steps, and an export or erasure finds an address holding `&`, `<`, `>` or a quote, or one inside a JSON body encoded into a JSON string twice.
- Through the plugin releases this engine pins: a data subject erasure or export matches the subject's whole address, with case folded, wherever a plugin keeps it. That holds for the address an account, an author or a recipient is stored under: directory users, region assignments and migrations, schema preset and canvas layout authors, translation authorship, review history, magic links and their audit entries, AI transcripts and support conversations, MFA secrets and passkeys, password reset tokens, OAuth provider links, quota request requesters and reviewers, error alert assignees, log alert recipients, A/B test visitors, and the devices and sign-ins of an account. It holds for text that names the address too: export jobs and schedules, webhook records, logged events, message broker outbox events, import staging rows, log lines, scheduled jobs and their runs, error events, analytics events, persisted query documents, captured slow queries, captured requests, quota request reasons, and the results, alerts and settings of synthetic probes. In that text a request for `ann@x.example` used to reach the records naming `joann@x.example` or `ann@x.example.org`. On MySQL and SQL Server an accented or `ß` spelling matched another address, so one person's erasure cleared another person's records, and an AI or magic link export carried them. An address holding `&` or a quote is reached now where the stored text kept it escaped. On PostgreSQL the subject's own records are reached now when the address was stored decomposed, with capitals or with a trailing space. On MySQL so is an analytics event whose stored address holds capitals or a decomposed accent.
- Through the data export plugin release this engine pins: an erasure that clears a finished export job naming the subject also deletes the job's exported file from storage, under the job's own tenant. The file used to stay fetchable after the erasure reported success. A file that cannot be deleted, or an install with no storage backend, fails the erasure and leaves the job as it was, so a later erasure finds the job and finishes it. A job matched only by the operator who ran it keeps its file, which holds other people's data. A job delivered to an S3 target keeps the key that records where its file went.
- Through the data export plugin release this engine pins: an export canceled while it runs stays canceled. A failure or the export deadline that came after the cancel used to mark it failed. An export canceled before it started no longer reads as running again for a moment, and canceling it a second time answers 409 instead of 200.
- Through the media plugin release this engine pins: on SQL Server a file read by id, in the list, in a search or after a details update names its uploader in `uploaded_by` again.
- Through the OAuth plugin release this engine pins: a client secret an administrator saves while someone signs in through that provider is kept. The sign-in used to write the old secret back over the new one, and the provider's sign-ins then failed until the secret was saved again.
- Through the OAuth plugin release this engine pins: the console's documentation link on every provider template opens the OAuth sign-in page of the documentation. Each link pointed at a domain with no DNS record, so none of them opened.
- Through the flow plugin release this engine pins: a datasource list derives the tenant's key once per request instead of once per row, and after a key change a datasource whose secret no longer decrypts logs one warning instead of one on every read.
- Through the plugin releases this engine pins: on SQL Server a flow's content query node that populates a `belongs_to` relation runs again, and so does the "Orders with customers and shipments" template. Both failed with a conversion error. A new translation needs a title only where its content type declares a required title field, so the console can translate a type with no title field. A boot no longer logs two "plugin route shadowed by engine-owned route" warnings for the public schema reads.
- Through the synthetic monitoring plugin release this engine pins: two updates of one probe at the same moment no longer lose a rotated secret. An update that left a secret out could write back the token another update had just rotated, and an update now reads and writes the probe in one locked step. An update that leaves out the timeout or the alert threshold keeps the stored value, which changes what an omitted field means. Both used to reset to their defaults, while an omitted interval or region list already kept its value. Sending 0 still sets the default.
- Through the webhook plugin release this engine pins: a record an incoming webhook delivery creates shows in the content list at once. A list read before the delivery used to keep answering without it until the cached list lapsed, 60 seconds by default, or another write flushed it.
- Through the MFA plugin release this engine pins: a sign-in with a passkey, a backup passkey or a phone carries the account's roles on PostgreSQL. It used to give a session with no roles there, which every administrator screen refused. A sign-in whose roles cannot be read is refused with a temporary error instead of giving a session.
- Through the email plugin release this engine pins: saving email branding accepts a body that leaves out the logo URL, either color or the footer text. It used to answer 400 for each one missing, so an API client had to send all four. A field left out of an update keeps its stored value, and an empty string still clears it.
- Through the audit and multitenant plugin releases this engine pins: a tenant delete that an active legal hold refuses answers 409 with a message naming the hold. It answered 503, which told an operator the database was down.
- Through the error tracking plugin release this engine pins: a Go panic's stack is filed under the line the panic started on, so panics from different code on one endpoint with one message no longer count as one alert. `POST /api/admin/error-tracking/events/stack` takes a `stack_trace` of up to 8192 characters, where the limit was 1024. An alert that occurs again after it was resolved opens unacknowledged, where acknowledging it again used to answer 404.
- Through the analytics plugin release this engine pins: the dashboard's `api_calls_24h` reports the day's API calls instead of its active users.
- Through the API key plugin release this engine pins: a tenant the operator withholds `apikey-pro` from is held to the free tier of five active keys, thirty days of request log and no new or raised request ceiling. The capability used to be read for the whole install.
- Through the data export plugin release this engine pins: an export canceled through another node stops at its next progress update instead of running to its end.
- Through the data export plugin release this engine pins: an export canceled while it stores its file deletes that file from internal storage. No job named such a file before, so neither a tenant delete nor an erasure found it. A file delivered to an S3 target is kept.
- Through the flow plugin release this engine pins: a run whose answer JSON cannot encode, such as one holding NaN or an infinity, is recorded as failed, publishes `flow.run_failed` and answers 500 instead of 200 with an empty body.
- Through the flow plugin release this engine pins: a Postgres number field that stores NaN or an infinity reads as null.
- Through the flow plugin release this engine pins: the cost ledger template's run on the 1st closes the month that just ended.
- Through the message broker plugin release this engine pins: a Kafka ack level other than `all` is refused at start under the setting's own name.
- Through the message broker plugin release this engine pins: the status route reports Kafka as connected only when a broker answers within two seconds.
- Through the multitenant plugin release this engine pins: a cold archive of a tenant that is still live answers 409, and migrate data that is not JSONL answers 400 on import and validate. Both used to answer 503. A cold archive that fails on the server logs its cause.
- Through the multitenant plugin release this engine pins: a backup import whose CREATE TABLE the database refuses reports "the table could not be created", and the database's own text goes to the log instead of the answer.
- Through the multitenant plugin release this engine pins: on MySQL and SQL Server, a backup import on a connection bound to a tenant database writes and clears its rows in the engine's own content tables, as a tenant clone does, instead of in the tenant database.
- Through the PII masking plugin release this engine pins: SCIM answers reach the identity provider as written. A masked address used to read as a changed user, so the provider's next sync rewrote or removed the account. These reads are not recorded in the PII access log.
- Through the schema plugin release this engine pins: on MySQL and SQL Server, a content type created with a `belongs_to` relation is retried when it loses a deadlock, where it used to answer 503.
- Through the schema plugin release this engine pins: every instance sees a content type that was saved and deleted, or renamed, between two of its cache polls. An instance used to keep serving the deleted definition until an unrelated write, so a tenant created again under a deleted slug found a type that read as existing and reads that reached a dropped table. The release adds a plugin migration that creates a change counter per tenant.
- Through the SCIM plugin release this engine pins: a user's and a group's `meta.location` name the address they are served at under `/api/v1/scim/v2`, where a directory following it used to get 404. A user or group create answers 201 with a `Location` header naming the new resource.
- Through the storage plugin release this engine pins: deleting an object that is already gone succeeds on GCS and Azure, where it used to fail with 404. A missing bucket or container still fails.
- Through the synthetic monitoring plugin release this engine pins: a GDPR subject erasure no longer writes back a probe token that an update rotated while the erasure ran, and a probe update that committed answers 200 with what it wrote instead of 404 or 503.
- Through the usage plugin release this engine pins: a tenant that has issued no API key yet, and the default tenant, can read their usage and snapshots, where both answered 404. A slug in a spelling the engine refuses, such as `ACME`, answers 404 on every database instead of reading `acme` on MySQL and SQL Server.
- Through the MFA plugin release this engine pins: a password sign-in's second factor challenge no longer offers a passkey, which no route could accept for that challenge. An account whose only second factor is a passkey signs in with a discoverable passkey on the passwordless routes.
- Through the gRPC plugin release this engine pins: gRPC serves again when a license change restores the grpc feature after one that dropped it. It used to keep refusing as unlicensed until the engine restarted.
- Through the request capture plugin release this engine pins: a request whose URL is longer than 2048 characters is captured on SQL Server again. Its URL is cut to 2048 characters before it is stored, on every database.
- Through the request capture plugin release this engine pins: GDPR erasures and tenant deletes that run at the same time no longer wait in turn for captures still being stored. Each waits at most five seconds for the captures queued before it, so on a busy SQL Server an erase or a delete answers inside the server's write timeout instead of running past it.
- Through the MFA, OAuth and SAML plugin releases this engine pins: a sign-in that ends while the database cannot read the account answers 503, as the engine's own sign-in does, so a client may retry it. It used to answer 500. A disabled account still gets 401.
- Through the PII masking plugin release this engine pins: with masking licensed, a Prometheus scrape of `/api/admin/metrics` or `/api/admin/telemetry/metrics` that asks for gzip reaches the scraper as a valid body. The masker used to run its rules over the bytes the scrape handler had compressed, so a scrape failed now and then with a bad checksum, and over sample values whose digits read as a card or phone number. A handler that compresses its own text answer is now masked before the engine compresses it.
- Through the bulk import plugin release this engine pins: a cancel that lands while an import writes its last batch, or while the import starts, ends the job canceled. The job used to read running for good, and every later cancel answered 503.
- Through the bulk import plugin release this engine pins: a cancel of an import that no worker on this instance runs answers from the stored job. A job still pending or running is recorded canceled, and one already finished answers 409. An import also stops before its next batch once a cancel recorded on another instance marks it canceled.
- Through the bulk import plugin release this engine pins: a tenant delete stops that tenant's running imports on the instance that deletes it, before their jobs are deleted. An import used to go on writing its remaining rows under the deleted tenant after its content was purged.
- Through the bulk import plugin release this engine pins: a cancel whose read of the job fails in the database answers 503 with "the import job could not be read", where it answered 500. An import that a cancel stops before its first batch ends canceled, and a failed import stores a message of its own, never the database's text.
- Through the query monitor plugin release this engine pins: a GDPR subject erasure reads only the slow query captures that may name the subject, through an index written with each capture. On SQL Server, with thirty thousand captures, an erasure took about nine seconds and now takes about twenty milliseconds. Captures stored before this release are still read in full.
- Through the multitenant plugin release this engine pins: on SQL Server, deleting or creating a tenant no longer slows down as content types are added and changed. With 424 tables and concurrent deletes, a delete takes 40ms at the median instead of 442ms.
- The engine prints its version, commit and build date and exits when run with `--version`, `-version` or `version`. It ignored the argument and started a second engine beside the running one, opening database connections and binding ports.
- On MySQL with `DATABASE_URL` in the documented `mysql://` form, the pool that keeps plugins' raw queries from changing data and the pool that holds leader locks connect again. Both read the scheme as the user name and were refused, so the engine logged a warning and ran on without either.
- Boot stops when the permissions plugin fails to register, as it does for every other plugin the binary carries. The list of plugins the boot check expects left it out.
- A license key that is set but not accepted is logged at boot as "license: key not accepted, running free tier" with the reason, such as the license server's refusal, a server that cannot be reached, a server URL that is not usable https, or a token that does not verify. It used to be logged as "license: no key set", which sent an operator who had set a key looking in the wrong place. A token that does not verify is logged with why, such as expired or not signed by the issuer, and never quoted.
- A refresh retried with the token it already traded, within 10 seconds of the trade, answers 409 and leaves the session in place. A lost answer, or two console replicas renewing one session at once, used to read as a stolen token, so the session ended everywhere. A refresh that fails with a server error after the trade puts back the token the client sent, so its retry renews the session. A token presented after the 10 seconds, or an older one, still ends the session as reuse.
- An account whose only second factor is a passkey is refused with 403 and a message saying so when it confirms issuing or rotating an admin token or approving a device. It used to be asked for a code it cannot hold, and the code it sent answered 503, which read as an outage. The code step of a sign-in answers 503 when the second factor store cannot be read, and `AUTH_MFA_NOT_CONFIGURED` only for an account with no authenticator app.
- A password longer than 72 bytes is refused with 422 and `AUTH_PASSWORD_TOO_LONG` at first-run setup, at user create and when a super admin sets a password, while the engine hashes with bcrypt, its default. bcrypt reads no more than 72 bytes, and such a password used to answer 500. A character outside ASCII takes two bytes or more. With `PASSWORD_HASH_ALGO=argon2id` there is no such limit.
- On MySQL every connection sets its session time zone to UTC, in place of any `time_zone` the database URL names. On a server not set to UTC, the times MySQL fills or compares itself, such as `NOW()`, `CURRENT_TIMESTAMP` and column defaults, were off by the server's offset, so a stored time read back wrong and an expiry or retry check against the current time was off by as much. The setting needs no time zone tables on the server.
- The goroutine tracker reports a stuck task once, with its age, instead of every 30 seconds, and never reports a worker registered as long-lived. A worker meant to run until shutdown used to be logged as a possible leak twice a minute from two minutes after boot, for as long as it ran.
- A boot that cannot write the signing key, such as on a platform disk owned by root while the engine runs as its own user, says how to fix it. The error names the user id the engine runs as and the step that fixes the refusal: create the missing key directory, give the engine the key file alone, or create a directory of its own and point `JWT_KEY_PATH` into it. It never asks for an existing directory, which may be shared, to change owner. The error used to say only "permission denied".
- Through the logging plugin release this engine pins: a tenant purge and a GDPR subject erasure write the log entries still buffered for the database before they delete. Entries logged in the second before a tenant delete used to land after it, stay under the deleted slug and block creating that slug again, and a buffered line naming an erased subject was kept the same way. Closing or draining the database sink waits for a flush already under way, so a drain no longer returns before the last entries reach the table.
- Through the OAuth plugin release this engine pins: on SQL Server a sign-in fails over to the provider's backup, a save that keeps the backup succeeds, and two providers that back each other up can be saved at the same time without a deadlock. On MySQL a backup named in upper case is found by the health view. Deleting a provider that another provider names as its backup clears that reference first, where it failed on SQL Server with 422. An update that names a provider as its own backup answers 422, and an empty `backup_provider_id` removes the backup, which no request could do before.
- Through the SCIM plugin release this engine pins: on SQL Server a group with members, a user in any group and a provider with any membership can be deleted, where the delete failed with 422. A group delete that fails in the database answers 503, as user and provider deletes do, instead of 500.
- Through the multitenant plugin release this engine pins: a tenant delete locks the tenant row on every database, so a write guarded on the tenant can no longer land after the purge on SQL Server and outlive the delete, or deadlock with the delete on MySQL. A second delete of a tenant already deleted answers not found at once, where it ran every purge handler first and held up a create of a neighboring slug on MySQL.
- Through the usage plugin release this engine pins: a usage snapshot the database picks as a deadlock victim beside a tenant delete runs again instead of answering 503, and it still skips a tenant deleted during the run.
- On MySQL, two transactions that inserted the same rows at once could deadlock, and the losing one could still commit part of its writes while both callers saw success. The insert retried its own statement after the database had already rolled the transaction back, so the retry ran outside that transaction, and on Postgres and SQL Server it failed with an error the caller could not retry. An insert inside a caller's transaction, or inside a serialized write, now hands the deadlock back to the caller, which can retry the whole transaction, and an insert that is its own transaction still retries as before.
- A content write from a plugin, such as a webhook delivery, that the content rules refuse answers 400, for an unknown status or a relation to a row the tenant does not hold. It answered 503, so the sender retried a delivery that could never succeed and the operator saw an outage where a request had been refused. A database failure still answers 503.
- Through the query monitor plugin release this engine pins: a subject erasure on an install whose slow query log predates the term index, and whose plugin runs unlicensed, finds the subject's captures by a match over the whole log and deletes them. An unlicensed start never adds the term index, so the erasure failed on its first read and left every capture that names the subject.
- Through the request capture plugin release this engine pins: queued captures are stored in batches of up to 32 per transaction, and on SQL Server one statement plan serves every insert. Each capture used to cost about 25 ms on SQL Server, so under load the queue fell minutes behind, dropped most captures, and outlasted the five seconds an erasure waits for queued captures, which let captures of an erased subject be stored after the erasure answered. When a batch fails, even by missing its two second deadline, each of its captures is stored on its own under its own deadline. Once one of those inserts runs out of time or finds the database unreachable, the rest of the batch is dropped and counted in the queue's `dropped_total`.
- Through the bulk import plugin release this engine pins: a tenant delete stops that tenant's imports on every replica. An import holds a share lock on the tenant's registry row while it writes and the delete locks that row first, so a worker on another replica can no longer go on writing content under the deleted tenant. An import requested while its tenant's delete is open waits for the delete and is refused with 404 once the delete commits, where its job and staged rows used to stay under the deleted tenant.
- A write that `sqlx.RetryOnTxConflict` retries after a lost deadlock or serialization conflict no longer waits once more after its last attempt, and a MySQL lock wait timeout is returned at once instead of retried. Each such timeout arrives after `innodb_lock_wait_timeout`, 50 seconds by default, so a caller waited up to three times that long for the error.
- Through the plugin releases this engine pins: a bulk import job writes its rows on an install that isolates tenants per connection, where it stayed `pending` or `running` with no rows written. On SQL Server, API analytics keeps recording when one flush holds two paths that differ only in case or trailing spaces, which it counts as one endpoint as MySQL does. Every flush used to fail from then on. A schema write that overlaps another no longer leaves the schema list without a content type that was just created.
- Through the data residency plugin release this engine pins: assigning a tenant to a closed region answers 409 with the code `data_residency.region_closed`, where it assigned the tenant. Tenants already in the region stay there, and moving a tenant back in after it left is refused.
- Through the media plugin release this engine pins: a media item answers `updated_at` with the time it last changed, and a download sends that time as `Last-Modified`. Every item answered `0001-01-01T00:00:00Z`, and a download sent no `Last-Modified`. On upgrade, migration 012 adds the column and sets each existing item's `updated_at` to its `created_at`.
- Through the MFA plugin release this engine pins: removing the last passkey turns two-factor authentication off when no authenticator app or backup code is left. It used to stay on with nothing to answer the sign-in challenge. Turning off the authenticator app answers 503 when the backup codes cannot be read, where it answered 200 with an error body, and a passkey delete answers 503 on a store failure, where it answered 404. Two accounts storing their first passkey at once on MySQL, or two passkey registrations for one account, no longer deadlock.
- Through the analytics plugin release this engine pins: the dashboard reports the tenant's own counts in `total_content`, `total_media` and `total_users`, which answered 0, and answers null for content or media where that plugin is not installed instead of 503. A self-service data subject export and erasure work on an install without the content or media plugin, and the erasure runs those plugins' own erasers, so a media file and its thumbnails stay reachable instead of being left in storage with nothing pointing at them.
- Through the data export plugin release this engine pins: a data subject erasure reports each finished export whose file went to an S3 bucket the operator named, which the instance cannot delete, as the audit entry `data_export.job.file_not_erased` with the bucket, region and object key, filed under the job's own tenant.
- Through the AI plugin release this engine pins: a data subject export and erasure find a message, recap or support desk entry that names the subject's address with decomposed accents or escaped inside JSON text.
- Through the cron, data export, error tracking and usage plugin releases this engine pins: a data subject erasure keeps a row someone edited during the erasure to another address the erasure's search still matches, instead of clearing it and losing the edit.
- Through the gRPC plugin release this engine pins: a content entry created, updated or deleted through the gRPC content service shows in the content list routes at once, instead of after the list cache lapsed.
- Through the WAF plugin release this engine pins: a request body that fails to read during inspection answers 413 or 400 with a log line naming the read error, where the handler received an empty body, so an upload cut that way answered 400 "invalid multipart form".
- Through the query monitor plugin release this engine pins: a data subject erasure on an install that upgraded with older captures no longer reads every one of them, because the plugin indexes them in the background (18.6 seconds down to about 5 milliseconds on SQL Server over 30,000 captures), and a stop during the plugin's start no longer waits past the shutdown timeout.
- A request body that fails to read partway is answered at once, with 413 when the body limit was reached and 400 otherwise, and a warning names the request and the bytes read. The engine used to pass on the bytes that had arrived, so a handler parsed a shorter body and blamed the client, and a multipart upload sent whole could answer 400 "invalid multipart form" with nothing logged above debug.
- Through the multitenant plugin release this engine pins: the tenant slug keeps one index, the one its unique constraint creates. A second index on the same column let a write guarded on the tenant read past a tenant delete's lock on SQL Server, and deadlock with the delete on MySQL.
- Through the plugin releases this engine pins: a super admin who is not scoped to a tenant finds the stored log records that carry no tenant, beside the default tenant's, in the log search. The lines this release files under no tenant were readable only while the in-memory log held them.
- Through the request capture plugin release this engine pins: a replay of an API call reaches the API listener. A replay sent through the console reached the admin listener, which answered 404 for every API path.

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

