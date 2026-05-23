package enginehost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func (h *engineHost) WithStorage(s core.Storage) { h.storage = s }

// WithQueryCache wires a per-dialect query result cache into the host.
// Plugins that read through QuerierRO will see cached results.
// When nil, caching is disabled (passthrough to pool).
func (h *engineHost) WithQueryCache(c *db.QueryCache) { h.queryCache = c }

// WithStorageConnected wires an active storage provider connection into the host.
func (h *engineHost) WithStorageConnected(c any) { h.storageConnected = c }

// StorageConnected implements StorageConnectedProvider.
func (h *engineHost) StorageConnected() any { return h.storageConnected }

// Storage implements StorageProvider.
func (h *engineHost) Storage() core.Storage { return h.storage }

// SetStorage implements StorageSetter. Called by plugins that provide
// alternative storage backends (e.g., S3) to replace the default Local driver.
func (h *engineHost) SetStorage(s core.Storage) { h.storage = s }

// RegisterEmailSender implements EmailSenderRegistrar. Called by the plugin
// that supplies a mailer, from its Start, for transactional email.
func (h *engineHost) RegisterEmailSender(s core.EmailSender) { h.emailSender = s }

// WithFlowRegistry wires the registry of flow node and trigger types the
// activator builds from started plugins. Called once at boot, after the
// activator exists and before any plugin starts, so the plugin that runs
// flows finds it in Start.
func (h *engineHost) WithFlowRegistry(r core.FlowRegistry) { h.flowRegistry = r }

// FlowRegistry implements FlowRegistryHost. Nil until WithFlowRegistry.
func (h *engineHost) FlowRegistry() core.FlowRegistry { return h.flowRegistry }

// WithTenantRegions wires the source of the tenant region role. The runtime
// passes the activator, which answers from the running plugin that holds the
// role. Called once at boot, before any plugin starts.
func (h *engineHost) WithTenantRegions(src core.TenantRegionResolverProvider) {
	h.tenantRegions = src
}

// TenantRegionResolver implements TenantRegionResolverProvider. It asks the
// source on every call and returns nil when nothing is wired or no running
// plugin holds the role.
func (h *engineHost) TenantRegionResolver() core.TenantRegionResolver {
	if h.tenantRegions == nil {
		return nil
	}
	return h.tenantRegions.TenantRegionResolver()
}

// EmailSender implements EmailSenderProvider. Returns the registered email
// sender, or nil when no plugin has registered one.
func (h *engineHost) EmailSender() core.EmailSender { return h.emailSender }

// RegisterFlowDefinitionValidator implements FlowDefinitionValidatorRegistrar.
// Called by the plugin that runs flows, in Start with its validator and in
// Stop with nil.
func (h *engineHost) RegisterFlowDefinitionValidator(v core.FlowDefinitionValidator) {
	h.flowValidator = v
}

// FlowDefinitionValidator implements FlowDefinitionValidatorProvider. Returns
// the registered validator, or nil when no plugin has registered one.
func (h *engineHost) FlowDefinitionValidator() core.FlowDefinitionValidator {
	return h.flowValidator
}

// RegisterFlowInvoker implements FlowInvokerRegistrar. Called by the plugin
// that runs flows, with its invoker when it activates and with nil when it
// deactivates.
func (h *engineHost) RegisterFlowInvoker(inv core.FlowInvoker) {
	h.flowInvoker = inv
}

// FlowInvoker implements FlowInvokerProvider. Returns the registered invoker,
// or nil when no plugin has registered one.
func (h *engineHost) FlowInvoker() core.FlowInvoker {
	return h.flowInvoker
}

// apiRoutesBox holds the registered APIRoutes behind one pointer, so an
// interface value can be stored atomically.
type apiRoutesBox struct{ r core.APIRoutes }

// RegisterAPIRoutes implements APIRoutesRegistrar. Called by the runtime
// once the API router exists.
func (h *engineHost) RegisterAPIRoutes(r core.APIRoutes) {
	if r == nil {
		h.apiRoutes.Store(nil)
		return
	}
	h.apiRoutes.Store(&apiRoutesBox{r: r})
}

// APIRoutes implements APIRoutesProvider. Nil until the runtime registers
// the router.
func (h *engineHost) APIRoutes() core.APIRoutes {
	if b := h.apiRoutes.Load(); b != nil {
		return b.r
	}
	return nil
}

// RegisterPermissionChecker implements PermissionCheckerRegistrar. Called by
// the plugin that owns the rules, with its checker in Start and with nil in
// Stop, so authorization follows that plugin's lifecycle.
func (h *engineHost) RegisterPermissionChecker(c core.PermissionChecker) {
	h.permissionChecker = c
}

// PermissionChecker implements PermissionCheckerProvider. Never nil: with no
// rule engine registered it answers core.RoleOnlyPermissionChecker, the
// kernel's own authorization over the roles on sys_users. A caller then has
// one answer to read rather than a nil that could be misread as a grant.
func (h *engineHost) PermissionChecker() core.PermissionChecker {
	if h.permissionChecker == nil {
		return core.RoleOnlyPermissionChecker{}
	}
	return h.permissionChecker
}

// RegisterContentLocalizer implements ContentLocalizerRegistrar. Called by
// the plugin that owns translations, in Start with its localizer and in Stop
// with nil.
func (h *engineHost) RegisterContentLocalizer(l core.ContentLocalizer) {
	h.contentLocalizer = l
}

// ContentLocalizer implements ContentLocalizerProvider. Returns the
// registered localizer, or nil when no plugin has registered one.
func (h *engineHost) ContentLocalizer() core.ContentLocalizer {
	return h.contentLocalizer
}

// RegisterConfigSection implements ConfigSectionRegistrar. Called by the
// plugin that owns a kind of configuration, with its section in Start and
// with nil in Stop.
func (h *engineHost) RegisterConfigSection(name string, s core.ConfigSection) {
	h.configSections.RegisterConfigSection(name, s)
}

// RegisterOwnedConfigSection implements ConfigSectionOwnerRegistrar. The
// scoped host calls it under the plugin's name, so one plugin cannot replace
// or remove another's section.
func (h *engineHost) RegisterOwnedConfigSection(owner, name string, s core.ConfigSection) error {
	return h.configSections.RegisterOwnedConfigSection(owner, name, s)
}

// ConfigSections implements ConfigSectionProvider. Empty when no plugin has
// registered a section.
func (h *engineHost) ConfigSections() []core.ConfigSection {
	return h.configSections.ConfigSections()
}

// RegisterContentEntryWriter implements ContentEntryWriterRegistrar. Called
// by the plugin that owns the editorial store, with its writer when it
// activates and with nil in Stop.
func (h *engineHost) RegisterContentEntryWriter(w core.ContentEntryWriter) {
	h.contentEntryWriter = w
}

// ContentEntryWriter implements ContentEntryWriterProvider. Returns the
// registered writer, or nil when no plugin has registered one.
func (h *engineHost) ContentEntryWriter() core.ContentEntryWriter {
	return h.contentEntryWriter
}

// RegisterMetricsGatherer implements MetricsGathererRegistrar. Called once
// by the runtime at boot with the engine's registry.
func (h *engineHost) RegisterMetricsGatherer(g core.MetricsGatherer) {
	h.metricsGatherer = g
}

// MetricsGatherer implements MetricsGathererProvider. Returns the
// registered registry, or nil before the runtime has registered one.
func (h *engineHost) MetricsGatherer() core.MetricsGatherer {
	return h.metricsGatherer
}

// SetRefreshTokenRevoker wires the refresh-token revocation function (from the
// core auth refresh store) during runtime boot.
func (h *engineHost) SetRefreshTokenRevoker(fn func(ctx context.Context, userID string) error) {
	h.revokeRefreshFn = fn
}

// RevokeAllRefreshTokens implements RefreshTokenRevoker. Delegates to the wired
// refresh store. A no-op when refresh tokens are not configured.
func (h *engineHost) RevokeAllRefreshTokens(ctx context.Context, userID string) error {
	if h.revokeRefreshFn == nil {
		return nil
	}
	return h.revokeRefreshFn(ctx, userID)
}

// SchemaSource implements core.SchemaSourceProvider. It answers the registry
// the content path reads, which is whatever engine a plugin registered, and no
// engine at all when none has.
//
// An engine that offers a reader of its own answers from it. One that does not
// is decoded from Get and List, so an engine written without the content path
// in mind still feeds it.
func (h *engineHost) SchemaSource() core.SchemaSource {
	if h.suppliedSchema != nil {
		return core.SchemaSourceOf(h.suppliedSchema)
	}
	return core.AbsentSchemaSource{}
}

// WithContentStore replaces the host's content store with the one the API
// router serves reads from. They must be the same instance: a plugin write
// through a different ContentStore cannot drop the read path's cached lists,
// so a committed row stays invisible to GET /api/v1/content for the whole
// cache TTL while the same row written through the API is visible at once.
func (h *engineHost) WithContentStore(store *db.ContentStore) {
	if store != nil {
		h.contentStore = store
	}
}

// RegisterSchemaEngine implements core.SchemaEngineRegistrar. The plugin that
// owns schema definitions calls it in Start with its engine and in Stop with
// nil. This module builds no schema engine, so the registered one is the only
// one the install has.
func (h *engineHost) RegisterSchemaEngine(e core.SchemaEngine) {
	h.suppliedSchema = e
}

// Schema implements core.Host.Schema. It is whatever a plugin registered, and
// nil when none has, because this module builds no schema engine.
func (h *engineHost) Schema() core.SchemaEngine {
	return h.suppliedSchema
}

// Secret returns the value for a concealed key by name, bypassing the
// configAdapter redaction guard. It is the engineHost-side implementation of
// the core.SecretsProvider interface. Returns the value and true when the key
// is known and non-empty, and an empty string and false when the key is
// unknown or its value is empty.
func (h *engineHost) Secret(key string) (val string, found bool) {
	if h.cfg == nil {
		return "", false
	}
	switch key {
	case "database_url":
		val = h.cfg.DatabaseURL
	case "database_replica_url":
		val = h.cfg.DatabaseReplicaURL
	case "jwt_secret":
		val = h.cfg.JWTSecret
	case "encryption_key":
		val = h.cfg.EncryptionKey
	case "storage_s3_key":
		val = h.cfg.StorageS3Key
	case "storage_s3_secret":
		val = h.cfg.StorageS3Secret
	case "license_key":
		val = h.cfg.LicenseKey
	case "audit_hmac_key":
		val = h.cfg.AuditHMACKey
	default:
		// A plugin's own credential, which core has no field for. See
		// plugin_config.go. Config redacts these by name, so this is the only
		// channel that can serve them.
		val = pluginEnv(key)
	}
	if val == "" {
		return "", false
	}
	return val, true
}

// Secrets returns a slice value for a concealed key, bypassing the
// configAdapter redaction guard. Only "jwt_secrets" is supported as a slice
// secret: the JWT rotation list.
func (h *engineHost) Secrets(key string) (vals []string, found bool) {
	if h.cfg == nil {
		return nil, false
	}
	switch key {
	case "jwt_secrets":
		vals = cloneNonEmpty(h.cfg.JWTSecrets)
	default:
		return nil, false
	}
	if len(vals) == 0 {
		return nil, false
	}
	return vals, true
}

// SignSessionToken implements SessionTokenSigner. Signs using the host's
// internal signing key (EdDSA when wired, else HMAC-SHA256 with the first
// configured secret). The caller supplies only the session identity. The
// host controls the signing material so compromised plugins cannot forge
// tokens with arbitrary secrets or roles.
func (h *engineHost) SignSessionToken(ctx context.Context, userID uuid.UUID, email string, roles []string) (string, error) {
	expiry := h.cfg.JWTExpirySecs
	if expiry <= 0 {
		expiry = 86400 // 24h default
	}

	// The tenant and the token version come from the account itself, not from
	// the caller, so every login path that reaches this method signs a session
	// carrying both. A session with no tenant claim is refused on a
	// multi-tenant install. A session with no token version would outlive the
	// account being disabled, because bumping the version is what ends its
	// sessions.
	//
	// The request context is what carries the tenant when the caller has one,
	// but a login endpoint runs before any session exists, so the row is the
	// only authority.
	//
	// The same read carries the account state, so a disabled, expired or
	// erased account gets no session here, as the password login refuses it.
	// The token version cannot stand in for that check, because a session
	// signed after the disable carries the bumped version. A row that cannot
	// be read refuses too: a token signed without it would skip the state
	// check on one database error.
	facts, err := h.sessionFactsFor(ctx, userID)
	if err != nil {
		return "", err
	}
	if facts.inactive {
		return "", core.ErrAccountInactive
	}
	return auth.Sign(h.cfg.JWTSecret, expiry, userID, email, roles, facts.tenantID, facts.tokenVersion)
}

// sessionFacts is what a session token needs from the account row.
type sessionFacts struct {
	tenantID     string
	tokenVersion int
	inactive     bool // disabled, erased, or past expires_at
}

// sessionFactsFor reads the tenant and token version stamped on a user row.
//
// It goes to the master pool directly rather than through h.pool, because
// h.pool routes through the tenant-isolated connection when the request context
// carries one. That connection has run USE tenant_x on MySQL and MSSQL, where
// sys_users does not exist: the table lives in the default schema and is not
// replicated per tenant. A public self-scoping route is exactly such a request, so
// the routed form would fail on two dialects out of three and pass on
// PostgreSQL, whose search_path still reaches public.
func (h *engineHost) sessionFactsFor(ctx context.Context, userID uuid.UUID) (sessionFacts, error) {
	master := h.rawDB
	if master == nil {
		return sessionFacts{}, nil
	}
	placeholder := "$1"
	switch h.pool.Engine() {
	case "mysql":
		placeholder = "?"
	case "mssql":
		placeholder = "@p1"
	}
	var f sessionFacts
	var disabled, anonymized bool
	var expiresAt sql.NullTime
	// lyeve:cross-tenant sys_users the account signing in names its own tenant. There is no session yet to scope this by
	row := master.QueryRowContext(ctx,
		`SELECT tenant_id, token_version, disabled, anonymized, expires_at FROM sys_users WHERE id = `+placeholder, userID)
	if err := row.Scan(&f.tenantID, &f.tokenVersion, &disabled, &anonymized, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sessionFacts{}, core.ErrAccountInactive
		}
		return sessionFacts{}, fmt.Errorf("session facts: %w", err)
	}
	f.inactive = disabled || anonymized || (expiresAt.Valid && time.Now().After(expiresAt.Time))
	return f, nil
}

// RegisterRecordRevisionStore implements RecordRevisionStoreRegistrar.
// Called by the plugin that keeps revision snapshots in Start with its store
// and in Stop with nil.
func (h *engineHost) RegisterRecordRevisionStore(s core.RecordRevisionStore) {
	h.recordRevisionStore = s
}

// RecordRevisionStore implements RecordRevisionStoreProvider. Returns the
// registered store, or nil when no plugin keeps revisions.
func (h *engineHost) RecordRevisionStore() core.RecordRevisionStore {
	return h.recordRevisionStore
}
