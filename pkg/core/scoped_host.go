package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// ScopedHost wraps a Host and enforces capability-based access control.
// Each method checks the plugin's granted capabilities before delegating
// to the inner host. When a required capability is missing the method
// returns a zero value or denied stub.
//
// The grant is the host capability policy's entry for the plugin narrowed by
// what the plugin declared through RegisterPluginWithCaps, or the declaration
// alone when the policy has no entry. A plugin registered through plain
// RegisterPlugin that the policy does not name is granted nothing, so every
// gated method denies it.
type ScopedHost struct {
	inner Host
	caps  Capability
	name  string

	mu     sync.Mutex
	warned map[string]bool
}

// NewScopedHost creates a capability-scoped wrapper around inner.
// name is the plugin's registered name. caps is the plugin's grant, the value
// of plugin.CapPolicy(name). It is not PluginCaps(name), which reports CapAll
// for a plugin that declared nothing.
func NewScopedHost(inner Host, name string, caps Capability) *ScopedHost {
	return &ScopedHost{inner: inner, name: name, caps: caps}
}

func (h *ScopedHost) has(need Capability) bool {
	return h.caps.Has(need)
}

// CapabilityChecker is implemented by a host that enforces plugin
// capabilities. A plugin reaches it by type assertion and can then refuse to
// start instead of running against a stub that accepts every call and does
// nothing:
//
//	if cc, ok := host.(core.CapabilityChecker); ok && !cc.HasCapability(core.CapHooks) {
//	    return errors.New("hook access required")
//	}
//
// This is a different axis from Capabilities(), which reports license
// features. A license can entitle a feature the plugin has no capability to
// reach, and the two can disagree.
type CapabilityChecker interface {
	HasCapability(c Capability) bool
}

var _ CapabilityChecker = (*ScopedHost)(nil)

// HasCapability reports whether this plugin holds c. The denied stubs are
// deliberately indistinguishable from working ones at the call site, so a
// plugin that cannot function without a grant should test for it here rather
// than infer it from behavior.
func (h *ScopedHost) HasCapability(c Capability) bool {
	return h.has(c)
}

// warnDenied logs the first denial of each distinct operation for this plugin.
//
// A denied call returns a stub, not an error, so the caller carries on as if
// it had worked: a subscription that never fires, a migration that never runs,
// a secret that reads as the empty string. Without this line the only evidence
// is the missing behavior, which surfaces far from the cause. The map bounds
// the output to one line per operation so a denial on a per-request path
// cannot flood the log.
func (h *ScopedHost) warnDenied(op, target string, need Capability) {
	if h == nil || h.inner == nil {
		return
	}
	key := op + "\x00" + target
	h.mu.Lock()
	if h.warned == nil {
		h.warned = make(map[string]bool)
	}
	seen := h.warned[key]
	h.warned[key] = true
	h.mu.Unlock()
	if seen {
		return
	}
	lg := h.inner.Logger(context.Background())
	if lg == nil {
		return
	}
	attrs := []any{"plugin", h.name, "op", op, "capability", capName(need)}
	if target != "" {
		attrs = append(attrs, "target", target)
	}
	lg.Warn("plugin capability denied", attrs...)
}

// warnUnavailable logs the first time a plugin asks the host for a service
// another plugin provides and gets nil. It is not a denial: no grant is
// missing, the providing plugin is simply not there. The same map bounds it
// to one line per operation, because the ask is on a per-request path.
func (h *ScopedHost) warnUnavailable(op string) {
	if h == nil || h.inner == nil {
		return
	}
	key := op + "\x00unavailable"
	h.mu.Lock()
	if h.warned == nil {
		h.warned = make(map[string]bool)
	}
	seen := h.warned[key]
	h.warned[key] = true
	h.mu.Unlock()
	if seen {
		return
	}
	lg := h.inner.Logger(context.Background())
	if lg == nil {
		return
	}
	lg.Warn("plugin host service unavailable", "plugin", h.name, "op", op)
}

// capName renders a Capability as the constant name a plugin author would
// recognize. A bitmask value in the log tells an operator nothing about which
// grant to add.
func capName(c Capability) string {
	switch c {
	case CapDBRead:
		return "CapDBRead"
	case CapDBWrite:
		return "CapDBWrite"
	case CapRawDB:
		return "CapRawDB"
	case CapConfigSecret:
		return "CapConfigSecret"
	case CapHooks:
		return "CapHooks"
	case CapRoutes:
		return "CapRoutes"
	case CapAdmin:
		return "CapAdmin"
	case CapSchema:
		return "CapSchema"
	case CapFlowRegistry:
		return "CapFlowRegistry"
	case CapMetrics:
		return "CapMetrics"
	case CapConfigSectionsRead:
		return "CapConfigSectionsRead"
	default:
		return fmt.Sprintf("Capability(%d)", uint32(c))
	}
}

// Querier returns a read-write querier when CapDBWrite is held, a
// read-only querier when only CapDBRead is held, or a deniedQuerier
// when neither is held.
func (h *ScopedHost) Querier(ctx context.Context) Querier {
	if !h.has(CapDBRead) {
		return deniedQuerier{host: h, need: CapDBRead}
	}
	q := h.inner.Querier(ctx)
	if !h.has(CapDBWrite) {
		return readOnlyQuerier{inner: q}
	}
	return q
}

// QuerierRO returns a read-replica querier. Denied without CapDBRead.
// Write enforcement is not applied: QuerierRO is always read-only.
func (h *ScopedHost) QuerierRO(ctx context.Context) Querier {
	if !h.has(CapDBRead) {
		return deniedQuerier{host: h, need: CapDBRead}
	}
	return h.inner.QuerierRO(ctx)
}

// RawDB returns the underlying *sql.DB. Denied without CapRawDB.
func (h *ScopedHost) RawDB() *sql.DB {
	if !h.has(CapRawDB) {
		h.warnDenied("RawDB", "", CapRawDB)
		return nil
	}
	return h.inner.RawDB()
}

// MigrationDB returns the migration database handle. Denied without CapRawDB.
func (h *ScopedHost) MigrationDB() *sql.DB {
	if !h.has(CapRawDB) {
		h.warnDenied("MigrationDB", "", CapRawDB)
		return nil
	}
	return h.inner.MigrationDB()
}

// Schema returns the schema engine. Reads pass through. Mutating ops
// (Apply, Delete, ApplyPending) return ErrCapDenied without CapSchema.
func (h *ScopedHost) Schema() SchemaEngine {
	inner := h.inner.Schema()
	if !h.has(CapSchema) {
		return &scopedSchemaEngine{inner: inner}
	}
	return inner
}

// UpsertContent forwards content writes to the inner host. Implements
// ContentWriter so a plugin that receives a ScopedHost can mirror its
// editorial writes into the per-schema tables the public read routes serve.
func (h *ScopedHost) UpsertContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error {
	cw, ok := h.inner.(ContentWriter)
	if !ok {
		return fmt.Errorf("content writer not available")
	}
	return cw.UpsertContent(ctx, schemaName, id, data)
}

// DeleteContent forwards content deletes to the inner host. Implements
// ContentWriter.
func (h *ScopedHost) DeleteContent(ctx context.Context, schemaName string, id uuid.UUID) error {
	cw, ok := h.inner.(ContentWriter)
	if !ok {
		return fmt.Errorf("content writer not available")
	}
	return cw.DeleteContent(ctx, schemaName, id)
}

// SetContentStatus forwards a status transition to the inner host.
// Implements ContentWriter.
func (h *ScopedHost) SetContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error {
	cw, ok := h.inner.(ContentWriter)
	if !ok {
		return fmt.Errorf("content writer not available")
	}
	return cw.SetContentStatus(ctx, schemaName, id, status)
}

// ProjectContentStatus forwards a status write that announces nothing to
// the inner host. Implements ContentStatusProjector.
func (h *ScopedHost) ProjectContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error {
	pj, ok := h.inner.(ContentStatusProjector)
	if !ok {
		return fmt.Errorf("content status projector not available")
	}
	return pj.ProjectContentStatus(ctx, schemaName, id, status)
}

var (
	_ ContentWriter          = (*ScopedHost)(nil)
	_ ContentStatusProjector = (*ScopedHost)(nil)
)

// Config returns the host configuration. Without CapConfigSecret,
// secret keys return empty strings. All other keys pass through.
func (h *ScopedHost) Config() Config {
	inner := h.inner.Config()
	if h.has(CapConfigSecret) {
		return inner
	}
	return &ScopedConfig{inner: inner, host: h}
}

// Hooks returns the hook bus for subscribing to events.
// Denied without CapHooks.
func (h *ScopedHost) Hooks() HookBus {
	if !h.has(CapHooks) {
		return deniedHookBus{host: h}
	}
	return h.inner.Hooks()
}

// HookPublisher returns the hook bus for publishing events.
// Denied without CapHooks.
func (h *ScopedHost) HookPublisher() HookPublisher {
	if !h.has(CapHooks) {
		return deniedHookPublisher{host: h}
	}
	return h.inner.HookPublisher()
}

// Logger returns the structured logger from the inner host.
// Unconditionally delegated.
func (h *ScopedHost) Logger(ctx context.Context) *slog.Logger {
	return h.inner.Logger(ctx)
}

// Version returns the engine version string.
// Unconditionally delegated.
func (h *ScopedHost) Version() string {
	return h.inner.Version()
}

// Dialect returns the database dialect ("postgres", "mysql", or "mssql").
// Unconditionally delegated.
func (h *ScopedHost) Dialect() string {
	return h.inner.Dialect()
}

// Tracer creates a named OpenTelemetry tracer on the inner host.
// Unconditionally delegated.
func (h *ScopedHost) Tracer(name string) trace.Tracer {
	return h.inner.Tracer(name)
}

// Optional provider forwarding
// ScopedHost implements every optional provider interface that the
// engine Host may expose, delegating through type assertions on inner.
// Sensitive providers are gated on the corresponding capability,
// while non-sensitive providers pass through unconditionally.

// SignSessionToken forwards to inner if it implements SessionTokenSigner.
func (h *ScopedHost) SignSessionToken(ctx context.Context, userID uuid.UUID, email string, roles []string) (string, error) {
	if s, ok := h.inner.(SessionTokenSigner); ok {
		return s.SignSessionToken(ctx, userID, email, roles)
	}
	return "", ErrCapDenied
}

// CachedFetch forwards to inner if it implements QueryCacheProvider.
func (h *ScopedHost) CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn CacheFetcher) (bool, error) {
	if p, ok := h.inner.(QueryCacheProvider); ok {
		return p.CachedFetch(ctx, pluginName, tenantID, dialect, sql, args, ttl, dest, fn)
	}
	if fn == nil {
		return false, nil
	}
	data, err := fn()
	if err != nil {
		return false, err
	}
	if dest != nil {
		if err := json.Unmarshal(data, dest); err != nil {
			return false, fmt.Errorf("cached_fetch unmarshal: %w", err)
		}
	}
	return false, nil
}

// ClusterBus forwards to inner, with every topic under "plugin.<name>." so a
// plugin reaches only its own copies on other replicas.
func (h *ScopedHost) ClusterBus() cluster.Bus {
	p, ok := h.inner.(ClusterBusProvider)
	if !ok {
		return nil
	}
	return cluster.Prefixed(p.ClusterBus(), "plugin."+h.name)
}

// InvalidateCache forwards to inner if it implements QueryCacheProvider.
func (h *ScopedHost) InvalidateCache(prefix string) int {
	if p, ok := h.inner.(QueryCacheProvider); ok {
		return p.InvalidateCache(prefix)
	}
	return 0
}

// StorageConnected forwards to inner if it implements StorageConnectedProvider.
func (h *ScopedHost) StorageConnected() any {
	if p, ok := h.inner.(StorageConnectedProvider); ok {
		return p.StorageConnected()
	}
	return nil
}

// Storage forwards to inner if it implements StorageProvider.
func (h *ScopedHost) Storage() Storage {
	if p, ok := h.inner.(StorageProvider); ok {
		return p.Storage()
	}
	return nil
}

// SetStorage forwards to inner if it implements StorageSetter.
func (h *ScopedHost) SetStorage(s Storage) {
	if p, ok := h.inner.(StorageSetter); ok {
		p.SetStorage(s)
	}
}

// AdminQuerier forwards to inner if CapAdmin is held and inner
// implements AdminQuerierProvider.
func (h *ScopedHost) AdminQuerier(ctx context.Context) Querier {
	if !h.has(CapAdmin) {
		return deniedQuerier{host: h, need: CapAdmin}
	}
	if p, ok := h.inner.(AdminQuerierProvider); ok {
		return p.AdminQuerier(ctx)
	}
	return deniedQuerier{host: h, need: CapAdmin}
}

// FlowRegistry forwards to inner if CapFlowRegistry is held and inner
// implements FlowRegistryHost. Denied, it returns a registry with no
// contributions and logs once: the plugin that runs flows would otherwise
// read an empty catalog as an install where nothing contributes.
func (h *ScopedHost) FlowRegistry() FlowRegistry {
	if !h.has(CapFlowRegistry) {
		h.warnDenied("FlowRegistry", "", CapFlowRegistry)
		return emptyFlowRegistry{}
	}
	if p, ok := h.inner.(FlowRegistryHost); ok {
		return p.FlowRegistry()
	}
	return nil
}

var _ FlowRegistryHost = (*ScopedHost)(nil)

// TenantRegionResolver forwards to inner if it implements
// TenantRegionResolverProvider. It needs no capability: the answer is a
// region name, and the resolver is asked about the tenant the caller names.
// Nil, with no log line, when no plugin holds the role, because a reader is
// expected to fall back then rather than treat it as a fault.
func (h *ScopedHost) TenantRegionResolver() TenantRegionResolver {
	if p, ok := h.inner.(TenantRegionResolverProvider); ok {
		return p.TenantRegionResolver()
	}
	return nil
}

var _ TenantRegionResolverProvider = (*ScopedHost)(nil)

// EmailSender forwards to inner if it implements EmailSenderProvider.
func (h *ScopedHost) EmailSender() EmailSender {
	if p, ok := h.inner.(EmailSenderProvider); ok {
		return p.EmailSender()
	}
	return nil
}

// RegisterEmailSender forwards to inner if it implements EmailSenderRegistrar.
func (h *ScopedHost) RegisterEmailSender(s EmailSender) {
	if p, ok := h.inner.(EmailSenderRegistrar); ok {
		p.RegisterEmailSender(s)
	}
}

// FlowDefinitionValidator forwards to inner if it implements
// FlowDefinitionValidatorProvider. Nil comes back when the inner host cannot
// provide one or nothing has registered, and the first nil per plugin is
// logged: a caller treats nil as the feature being absent and carries on, so
// the log line is the only evidence of what it asked for and did not get.
func (h *ScopedHost) FlowDefinitionValidator() FlowDefinitionValidator {
	if p, ok := h.inner.(FlowDefinitionValidatorProvider); ok {
		if v := p.FlowDefinitionValidator(); v != nil {
			return v
		}
	}
	h.warnUnavailable("FlowDefinitionValidator")
	return nil
}

// RegisterFlowDefinitionValidator forwards to inner if it implements
// FlowDefinitionValidatorRegistrar.
func (h *ScopedHost) RegisterFlowDefinitionValidator(v FlowDefinitionValidator) {
	if p, ok := h.inner.(FlowDefinitionValidatorRegistrar); ok {
		p.RegisterFlowDefinitionValidator(v)
	}
}

// FlowInvoker forwards to inner if it implements FlowInvokerProvider. Nil,
// and the log line, as for FlowDefinitionValidator: a transport asking on an install where
// flows are not licensed sees nil and exposes nothing.
func (h *ScopedHost) FlowInvoker() FlowInvoker {
	if p, ok := h.inner.(FlowInvokerProvider); ok {
		if inv := p.FlowInvoker(); inv != nil {
			return inv
		}
	}
	h.warnUnavailable("FlowInvoker")
	return nil
}

// RegisterFlowInvoker forwards to inner if it implements
// FlowInvokerRegistrar.
func (h *ScopedHost) RegisterFlowInvoker(inv FlowInvoker) {
	if p, ok := h.inner.(FlowInvokerRegistrar); ok {
		p.RegisterFlowInvoker(inv)
	}
}

// PermissionChecker forwards to inner if it implements
// PermissionCheckerProvider. Never nil: with no rule engine registered it
// answers RoleOnlyPermissionChecker, which is the kernel's own authorization
// and what an install with no rules enforces. There is no log line, because
// that is the ordinary shape of a build without the plugin that owns the
// rules, and the boot states it once.
func (h *ScopedHost) PermissionChecker() PermissionChecker {
	if p, ok := h.inner.(PermissionCheckerProvider); ok {
		if c := p.PermissionChecker(); c != nil {
			return c
		}
	}
	return RoleOnlyPermissionChecker{}
}

// RegisterPermissionChecker forwards to inner if it implements
// PermissionCheckerRegistrar. This is how the plugin that owns the rules
// supplies them: with its checker in Start and with nil in Stop.
func (h *ScopedHost) RegisterPermissionChecker(c PermissionChecker) {
	if p, ok := h.inner.(PermissionCheckerRegistrar); ok {
		p.RegisterPermissionChecker(c)
	}
}

// ContentLocalizer forwards to inner if it implements
// ContentLocalizerProvider. Nil, and the log line, as for FlowDefinitionValidator: a
// transport asking on an install with no localizer registered sees nil and
// serves the source fields.
func (h *ScopedHost) ContentLocalizer() ContentLocalizer {
	if p, ok := h.inner.(ContentLocalizerProvider); ok {
		if l := p.ContentLocalizer(); l != nil {
			return l
		}
	}
	h.warnUnavailable("ContentLocalizer")
	return nil
}

// EngineDBConn returns a connection bound to the engine's own database.
// Denied without CapRawDB, which is what RawDB and MigrationDB cost: this
// hands out the same reach, on a connection carrying no DML guard and no
// tenant scoping.
//
// A host that provides no such connection is a database failure, not
// permission to fall back to an ordinary one. Falling back would return a
// tenant-bound connection, which is the whole failure this exists to prevent.
func (h *ScopedHost) EngineDBConn(ctx context.Context) (*sql.Conn, error) {
	if !h.has(CapRawDB) {
		h.warnDenied("EngineDBConn", "", CapRawDB)
		return nil, ErrCapDenied
	}
	if p, ok := h.inner.(EngineDBConnProvider); ok {
		return p.EngineDBConn(ctx)
	}
	return nil, errors.New("engine database connection: host does not provide one")
}

// SchemaSource forwards to inner if it implements SchemaSourceProvider. It is
// the registry the content path reads, which is a plugin's once one has
// registered an engine. A host that provides none reads as no engine rather
// than as an install with no content types.
func (h *ScopedHost) SchemaSource() SchemaSource {
	if p, ok := h.inner.(SchemaSourceProvider); ok {
		if s := p.SchemaSource(); s != nil {
			return s
		}
	}
	return AbsentSchemaSource{}
}

// RegisterSchemaEngine forwards to inner if it implements
// SchemaEngineRegistrar.
func (h *ScopedHost) RegisterSchemaEngine(e SchemaEngine) {
	if p, ok := h.inner.(SchemaEngineRegistrar); ok {
		p.RegisterSchemaEngine(e)
	}
}

// RegisterContentLocalizer forwards to inner if it implements
// ContentLocalizerRegistrar.
func (h *ScopedHost) RegisterContentLocalizer(l ContentLocalizer) {
	if p, ok := h.inner.(ContentLocalizerRegistrar); ok {
		p.RegisterContentLocalizer(l)
	}
}

// RegisterConfigSection forwards to inner, under this plugin's name, if
// inner implements ConfigSectionOwnerRegistrar. This is how a plugin offers
// its configuration to a bundle: with its section in Start and with nil in
// Stop. A name another plugin or the engine already holds is refused and
// logged, so no plugin can replace or remove a section it does not own. A
// host that cannot record the owner is not handed the section at all.
func (h *ScopedHost) RegisterConfigSection(name string, s ConfigSection) {
	p, ok := h.inner.(ConfigSectionOwnerRegistrar)
	if !ok {
		return
	}
	if err := p.RegisterOwnedConfigSection(h.name, name, s); err != nil {
		if lg := h.inner.Logger(context.Background()); lg != nil {
			lg.Warn("config section refused", "plugin", h.name, "section", name, "error", err)
		}
	}
}

// ConfigSections forwards to inner if CapConfigSectionsRead is held and
// inner implements ConfigSectionProvider. A section exports its owner's
// secrets under the caller's sealer and applies a bundle to its owner's
// tables, so only the plugin that assembles a bundle may list them. Denied,
// or on a host that keeps no registry, it returns nil, which a bundle then
// reports rather than reading as an instance with no configuration.
func (h *ScopedHost) ConfigSections() []ConfigSection {
	if !h.has(CapConfigSectionsRead) {
		h.warnDenied("ConfigSections", "", CapConfigSectionsRead)
		return nil
	}
	if p, ok := h.inner.(ConfigSectionProvider); ok {
		return p.ConfigSections()
	}
	return nil
}

// ContentEntryWriter forwards to inner if it implements
// ContentEntryWriterProvider. Nil, and the log line, as for FlowDefinitionValidator: an
// import on an install with no entry writer registered sees nil and writes
// the generated table directly.
func (h *ScopedHost) ContentEntryWriter() ContentEntryWriter {
	if p, ok := h.inner.(ContentEntryWriterProvider); ok {
		if w := p.ContentEntryWriter(); w != nil {
			return w
		}
	}
	h.warnUnavailable("ContentEntryWriter")
	return nil
}

// RegisterContentEntryWriter forwards to inner if it implements
// ContentEntryWriterRegistrar.
func (h *ScopedHost) RegisterContentEntryWriter(w ContentEntryWriter) {
	if p, ok := h.inner.(ContentEntryWriterRegistrar); ok {
		p.RegisterContentEntryWriter(w)
	}
}

// RecordRevisionStore forwards to inner if it implements
// RecordRevisionStoreProvider. Nil, and the log line, as for
// ContentEntryWriter: a build with no plugin keeping revisions has no
// history to serve.
func (h *ScopedHost) RecordRevisionStore() RecordRevisionStore {
	if p, ok := h.inner.(RecordRevisionStoreProvider); ok {
		if s := p.RecordRevisionStore(); s != nil {
			return s
		}
	}
	h.warnUnavailable("RecordRevisionStore")
	return nil
}

// RegisterRecordRevisionStore forwards to inner if it implements
// RecordRevisionStoreRegistrar.
func (h *ScopedHost) RegisterRecordRevisionStore(s RecordRevisionStore) {
	if p, ok := h.inner.(RecordRevisionStoreRegistrar); ok {
		p.RegisterRecordRevisionStore(s)
	}
}

// MetricsGatherer forwards to inner if CapMetrics is held and inner
// implements MetricsGathererProvider. Denied, it returns nil and logs the
// grant to add: the registry carries every tenant's request series, so a
// plugin reads it only when the policy says it exports them.
func (h *ScopedHost) MetricsGatherer() MetricsGatherer {
	if !h.has(CapMetrics) {
		h.warnDenied("MetricsGatherer", "", CapMetrics)
		return nil
	}
	if p, ok := h.inner.(MetricsGathererProvider); ok {
		if g := p.MetricsGatherer(); g != nil {
			return g
		}
	}
	h.warnUnavailable("MetricsGatherer")
	return nil
}

var (
	_ FlowDefinitionValidatorProvider  = (*ScopedHost)(nil)
	_ FlowDefinitionValidatorRegistrar = (*ScopedHost)(nil)
	_ FlowInvokerProvider              = (*ScopedHost)(nil)
	_ FlowInvokerRegistrar             = (*ScopedHost)(nil)
	_ PermissionCheckerProvider        = (*ScopedHost)(nil)
	_ PermissionCheckerRegistrar       = (*ScopedHost)(nil)
	_ ContentLocalizerProvider         = (*ScopedHost)(nil)
	_ ContentLocalizerRegistrar        = (*ScopedHost)(nil)
	_ ConfigSectionProvider            = (*ScopedHost)(nil)
	_ ConfigSectionRegistrar           = (*ScopedHost)(nil)
	_ ContentEntryWriterProvider       = (*ScopedHost)(nil)
	_ ContentEntryWriterRegistrar      = (*ScopedHost)(nil)
	_ RecordRevisionStoreProvider      = (*ScopedHost)(nil)
	_ RecordRevisionStoreRegistrar     = (*ScopedHost)(nil)
	_ MetricsGathererProvider          = (*ScopedHost)(nil)
	_ GoroutineTunablesProvider        = (*ScopedHost)(nil)
	_ LogRingProvider                  = (*ScopedHost)(nil)
	_ EngineRateLimitsProvider         = (*ScopedHost)(nil)
	_ APIRoutesProvider                = (*ScopedHost)(nil)
)

// APIRoutes forwards to inner if it implements APIRoutesProvider. Nil
// before the runtime has built the API router. A plugin then refuses the
// paths it cannot check.
func (h *ScopedHost) APIRoutes() APIRoutes {
	if p, ok := h.inner.(APIRoutesProvider); ok {
		if r := p.APIRoutes(); r != nil {
			return r
		}
	}
	h.warnUnavailable("APIRoutes")
	return nil
}

// GoroutineTunables forwards to inner if it implements
// GoroutineTunablesProvider. The primitives themselves are forwarded to
// every plugin through ScalingHost, so the views and settings over them
// need no capability of their own.
func (h *ScopedHost) GoroutineTunables() GoroutineTunables {
	if p, ok := h.inner.(GoroutineTunablesProvider); ok {
		return p.GoroutineTunables()
	}
	return nil
}

// DeclaredResources forwards to inner if it implements
// DeclaredResourceProvider. The entries come from the configuration file the
// operator wrote, not from the database, so no capability gates them.
func (h *ScopedHost) DeclaredResources(section string) ([]json.RawMessage, error) {
	if p, ok := h.inner.(DeclaredResourceProvider); ok {
		return p.DeclaredResources(section)
	}
	return nil, nil
}

// LogRing forwards to inner if it implements LogRingProvider. The records
// in the ring reach every plugin's sink through the slog chain already, so
// the ring needs no capability of its own.
func (h *ScopedHost) LogRing() LogRing {
	if p, ok := h.inner.(LogRingProvider); ok {
		return p.LogRing()
	}
	return nil
}

// EngineRateLimits forwards to inner if it implements
// EngineRateLimitsProvider. The values are the engine's settings, readable
// by any plugin, so they need no capability of their own.
func (h *ScopedHost) EngineRateLimits() []EngineRateLimit {
	if p, ok := h.inner.(EngineRateLimitsProvider); ok {
		return p.EngineRateLimits()
	}
	return nil
}

// RevokeAllRefreshTokens forwards to inner if it implements RefreshTokenRevoker.
func (h *ScopedHost) RevokeAllRefreshTokens(ctx context.Context, userID string) error {
	if p, ok := h.inner.(RefreshTokenRevoker); ok {
		return p.RevokeAllRefreshTokens(ctx, userID)
	}
	return nil
}

// AcquireTenantConn forwards to inner if it implements TenancyConnProvider.
func (h *ScopedHost) AcquireTenantConn(ctx context.Context, tenantID string) (context.Context, func(), error) {
	if p, ok := h.inner.(TenancyConnProvider); ok {
		return p.AcquireTenantConn(ctx, tenantID)
	}
	return ctx, func() {}, nil
}

// Secret forwards to inner if CapConfigSecret is held and inner
// implements SecretsProvider.
func (h *ScopedHost) Secret(key string) (string, bool) {
	if !h.has(CapConfigSecret) {
		return "", false
	}
	if p, ok := h.inner.(SecretsProvider); ok {
		return p.Secret(key)
	}
	return "", false
}

// Secrets forwards to inner if CapConfigSecret is held and inner
// implements SecretsProvider.
func (h *ScopedHost) Secrets(key string) ([]string, bool) {
	if !h.has(CapConfigSecret) {
		return nil, false
	}
	if p, ok := h.inner.(SecretsProvider); ok {
		return p.Secrets(key)
	}
	return nil, false
}

// ScopedConfig

// ScopedConfig wraps a Config and filters secret keys when CapConfigSecret
// is not granted. String returns "" for secret keys. Strings returns nil.
// Bool and Duration pass through unchanged.
type ScopedConfig struct {
	inner Config
	host  *ScopedHost
}

// SecretKeys are the config keys gated by CapConfigSecret. This is the
// canonical list used by enginehost's configAdapter (redaction) and
// ScopedConfig (cap enforcement).
var SecretKeys = map[string]bool{
	"database_url":         true,
	"database_replica_url": true,
	"jwt_secret":           true,
	"jwt_secrets":          true,
	"encryption_key":       true,
	"api_key_pepper":       true,
	"redis_url":            true,
	"storage_s3_key":       true,
	"storage_s3_secret":    true,
	"search_es_api_key":    true,
	"search_es_password":   true,
	"search_meili_api_key": true,
	"smtp_pass":            true,
	"smtp_user":            true,
	"license_key":          true,
	"license_cache_dir":    true,
	"rate_limit_redis_url": true,
	// Still verifies console signatures during a rotation, so it is as secret
	// as the current key. The _key suffix rule does not reach it.
	"admin_console_key_previous": true,
}

// OperatorOnlyKeys are settings that only the operator may set, through the
// environment or the configuration file, never through the admin API. Each
// decides where tenant data is encrypted or where backups and credentials are
// sent. A super admin can already read every tenant, so this is not about
// trust in that role: it keeps a stolen admin session from repointing
// encryption or backups, which would outlast the session and reach data the
// session never touched. The resolver ignores a stored value for these keys
// and the save routes refuse them.
var OperatorOnlyKeys = map[string]bool{
	"kms_provider":                      true,
	"kms_region":                        true,
	"kms_key_id":                        true,
	"kms_endpoint":                      true,
	"replication_targets":               true,
	"storage_s3_backup_allowed_buckets": true,
	// Where uploaded files and backups are stored. The engine reads these
	// once at boot, before the admin layer loads, so a stored value would not
	// apply. Reserving them stops the admin API from accepting a value that
	// would do nothing.
	"storage_s3_endpoint": true,
	"storage_s3_bucket":   true,
	"storage_s3_region":   true,
	// Where data exports may be uploaded.
	"data_export_s3_allowed_buckets": true,
	"data_export_s3_allowed_regions": true,
	// Private networks outbound webhooks and mail may reach, which widen
	// the SSRF guard to internal addresses.
	"webhook_allowed_private_networks": true,
	"smtp_allowed_private_networks":    true,
}

// IsOperatorOnlyKey reports whether key may be set only by the operator. It
// folds case and treats '.', '-' and ' ' as '_', as the resolver names keys,
// so no spelling of a reserved key gets past a save route.
func IsOperatorOnlyKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.NewReplacer(".", "_", "-", "_", " ", "_").Replace(k)
	return OperatorOnlyKeys[k]
}

// secretSuffixes name a key as credential material regardless of whether core
// knows the key at all.
//
// SecretKeys can only list what core itself configures. Plugins bring their own
// credentials that core has never heard of, and those reach the engine through
// the environment fallback rather than a config field, so a name-based rule is
// the only thing standing between them and the unredacted Config channel.
var secretSuffixes = []string{"_secret", "_key", "_password", "_pass", "_token"}

// IsSecretKey reports whether key carries credential material and must never be
// served through Config. Reachable only through SecretsProvider.
//
// The comparison is case-folded because the unknown-key path resolves a config
// key from the environment under its upper-cased name, so a plugin can name the
// same variable either way, and neither spelling may escape the map or the
// suffix rule.
func IsSecretKey(key string) bool {
	lower := strings.ToLower(key)
	if SecretKeys[lower] {
		return true
	}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// String returns the config value for key. Secret keys always return "": a
// scoped plugin host cannot read secret configuration through Config.
func (c *ScopedConfig) String(key string) string {
	if IsSecretKey(key) {
		c.host.warnDenied("Config.String", key, CapConfigSecret)
		return ""
	}
	return c.inner.String(key)
}

// Bool returns the boolean config value for key. Pass-through to inner
// without secret scoping (Bool keys are never sensitive).
func (c *ScopedConfig) Bool(key string) bool {
	return c.inner.Bool(key)
}

// Duration returns the time.Duration config value for key. Pass-through to
// inner without secret scoping (Duration keys are never sensitive).
func (c *ScopedConfig) Duration(key string) time.Duration {
	return c.inner.Duration(key)
}

// Strings returns the config values for key. Secret keys always return nil: a
// scoped plugin host cannot read secret configuration through Config.
//
// The predicate is IsSecretKey, the same one String uses, so a key String
// refuses is refused here too, a plugin's own multi-valued credential
// included.
func (c *ScopedConfig) Strings(key string) []string {
	if IsSecretKey(key) {
		c.host.warnDenied("Config.Strings", key, CapConfigSecret)
		return nil
	}
	return c.inner.Strings(key)
}

// readOnlyQuerier
// Allows Query/QueryRow through but denies Exec/Begin with ErrCapDenied.
// Used when a plugin has CapDBRead but not CapDBWrite.

type readOnlyQuerier struct {
	inner Querier
}

func (r readOnlyQuerier) QueryRow(ctx context.Context, sQL string, args ...any) (Row, error) {
	return r.inner.QueryRow(ctx, sQL, args...)
}

func (r readOnlyQuerier) Query(ctx context.Context, sQL string, args ...any) (Rows, error) {
	return r.inner.Query(ctx, sQL, args...)
}

func (r readOnlyQuerier) Exec(ctx context.Context, sQL string, args ...any) (CommandTag, error) {
	return CommandTag{}, ErrCapDenied
}

func (r readOnlyQuerier) Begin(ctx context.Context) (Tx, error) {
	return nil, ErrCapDenied
}

// scopedSchemaEngine
// Allows reads (List, Get, PreviewDDL, ValidateContent) but denies
// mutating ops (Apply, Delete, ApplyPending) with ErrCapDenied.

type scopedSchemaEngine struct {
	inner SchemaEngine
}

func (s *scopedSchemaEngine) Apply(ctx context.Context, name string, definition json.RawMessage) error {
	return ErrCapDenied
}

func (s *scopedSchemaEngine) Delete(ctx context.Context, name string) error {
	return ErrCapDenied
}

func (s *scopedSchemaEngine) ApplyPending(ctx context.Context) (int, error) {
	return 0, ErrCapDenied
}

func (s *scopedSchemaEngine) List(ctx context.Context) ([]json.RawMessage, error) {
	return s.inner.List(ctx)
}

func (s *scopedSchemaEngine) Get(ctx context.Context, name string) (json.RawMessage, error) {
	return s.inner.Get(ctx, name)
}

func (s *scopedSchemaEngine) PreviewDDL(ctx context.Context, name string, definition json.RawMessage) ([]DDLStatement, error) {
	return s.inner.PreviewDDL(ctx, name, definition)
}

func (s *scopedSchemaEngine) ValidateContent(ctx context.Context, schemaName string, data map[string]any) ([]SchemaValidationError, error) {
	return s.inner.ValidateContent(ctx, schemaName, data)
}

// Denied stubs

type deniedQuerier struct {
	host *ScopedHost
	need Capability
}

func (d deniedQuerier) warn(op string) {
	d.host.warnDenied(op, "", d.need)
}

// QueryRow reports no error, so the denial only reaches the caller when it
// scans. The warning is logged here, where the denial actually happened.
func (d deniedQuerier) QueryRow(ctx context.Context, sQL string, args ...any) (Row, error) {
	d.warn("Querier.QueryRow")
	return deniedRow{}, nil
}
func (d deniedQuerier) Query(ctx context.Context, sQL string, args ...any) (Rows, error) {
	d.warn("Querier.Query")
	return nil, ErrCapDenied
}
func (d deniedQuerier) Exec(ctx context.Context, sQL string, args ...any) (CommandTag, error) {
	d.warn("Querier.Exec")
	return CommandTag{}, ErrCapDenied
}
func (d deniedQuerier) Begin(ctx context.Context) (Tx, error) {
	d.warn("Querier.Begin")
	return nil, ErrCapDenied
}

type deniedRow struct{}

func (deniedRow) Scan(dest ...any) error { return ErrCapDenied }

// deniedHookBus accepts every subscription and delivers nothing. A caller
// cannot tell it from the real bus by its return value, so each subscription
// is logged: the alternative is a plugin whose handlers simply never run.
type deniedHookBus struct {
	host *ScopedHost
}

func (d deniedHookBus) Subscribe(schema string, event EventType, handler EventHandler) Subscription {
	d.host.warnDenied("Hooks.Subscribe", schema+":"+string(event), CapHooks)
	return deniedSubscription{}
}

// EventLicenseChanged tells a plugin its own entitlement moved. It is the one
// system event a plugin receives without CapHooks.
//
// The capability guards the data bus: content and schema events, and the right
// to publish. A notification that this plugin's license changed is neither. It
// is the engine speaking to the plugin about itself, and the plugin can learn
// the same thing from Capabilities() on any request.
//
// A gated plugin rebuilds its handlers from this event, and the engine
// re-collects routes immediately afterwards expecting that rebuild to have
// happened. Withheld, a license change after boot would reach a plugin
// without CapHooks only at the next restart.
const EventLicenseChanged = "license.changed"

func (d deniedHookBus) On(event string, handler SystemEventHandler) Subscription {
	if event == EventLicenseChanged {
		return d.host.inner.Hooks().On(event, handler)
	}
	d.host.warnDenied("Hooks.On", event, CapHooks)
	return deniedSubscription{}
}

type deniedSubscription struct{}

func (deniedSubscription) Unsubscribe() {}

// deniedHookPublisher returns ErrCapDenied, but publish sites routinely
// discard the error because a failed hook is not meant to fail the write. The
// warning is what makes a dropped event visible.
type deniedHookPublisher struct {
	host *ScopedHost
}

func (d deniedHookPublisher) Publish(ctx context.Context, event Event) error {
	d.host.warnDenied("HookPublisher.Publish", string(event.Type), CapHooks)
	return ErrCapDenied
}

// Scaling methods are delegated directly to the inner host, with no
// capability check.
//
// The tracker is the one exception in shape, not in access: the plugin gets
// a view bound to its own name, so every goroutine it starts is attributed
// to it in the snapshot and a leak names the plugin that owns it.

func (h *ScopedHost) WorkerPool() *engine.WorkerPool { return h.inner.WorkerPool() }
func (h *ScopedHost) GoroutineTracker() *engine.GoroutineTracker {
	t := h.inner.GoroutineTracker()
	if t == nil {
		return nil
	}
	return t.ForOwner(h.name)
}
func (h *ScopedHost) ParallelEngine() *engine.ParallelEngine { return h.inner.ParallelEngine() }
func (h *ScopedHost) AsyncHookExecutor() *engine.AsyncHookExecutor {
	return h.inner.AsyncHookExecutor()
}
func (h *ScopedHost) DistLock(name string) *engine.DistLock { return h.inner.DistLock(name) }

// LicenseHost methods: delegated directly to inner host. Feature gating
// is enforced at the license level, not the plugin-capability level, so
// ScopedHost passes these through unconditionally.

func (h *ScopedHost) Capabilities() CapabilitySet {
	return h.inner.Capabilities()
}

func (h *ScopedHost) HasFeature(ctx context.Context, feature string) bool {
	return h.inner.HasFeature(ctx, feature)
}
