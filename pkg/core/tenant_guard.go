package core

import (
	"context"
	"errors"
	"regexp"
	"sync"
)

// DefaultTenantSlug is the tenant a single-tenant deployment runs as.
//
// Nothing registers it. It is the name the engine gives traffic that never
// named a tenant, so tenant-scoped rows written by an unscoped caller carry it
// and every per-tenant reader groups by it. To the queries in this process it
// is an ordinary tenant. To the roster in sys_tenants it does not exist.
//
// A plugin that treats sys_tenants as the list of every tenant has to account
// for it: any database that has served one request already holds its rows.
const DefaultTenantSlug = "default"

// ErrTenantRequired is returned by RequireTenantID when the context carries
// no tenant identity. Callers should treat this as a hard-fail: proceeding
// without a tenant on shared sys_* tables is a cross-tenant IDOR.
var ErrTenantRequired = errors.New("tenant_id is required but was empty in context")

// RequireTenantID wraps TenantIDFromCtx with fail-closed semantics.
// It returns the tenant ID when present, or ErrTenantRequired when the
// context carries no tenant identity.
//
// Plugin store functions should call this as their first line:
//
//	tenantID, err := core.RequireTenantID(ctx)
//	if err != nil {
//	    return fmt.Errorf("StoreName.FuncName: %w", err)
//	}
//
// Use it instead of TenantIDFromCtx wherever an empty tenant must fail.
func RequireTenantID(ctx context.Context) (string, error) {
	tid := TenantIDFromCtx(ctx)
	if tid == "" {
		return "", ErrTenantRequired
	}
	return tid, nil
}

// TenantSlot carries the tenant a request resolved to back out to middleware
// that wrapped the whole chain.
//
// Tenancy is resolved partway down: TenantHeader puts it on a new context, and
// only the handlers below it ever see that. A middleware on the outside holds
// the request it was given, so anything it logs or records afterwards has no
// tenant unless it reads the slot, and a tenant-scoped search of those
// records finds none of them.
//
// The outer middleware installs a slot before the chain runs, whoever resolves
// the tenant fills it in, and the outer middleware reads it afterwards.
type TenantSlot struct {
	mu sync.Mutex
	id string
}

// Set records the resolved tenant. Safe for concurrent use.
func (s *TenantSlot) Set(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
}

// Get returns the resolved tenant, or "" when nothing resolved one.
func (s *TenantSlot) Get() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

type tenantSlotKey struct{}

// WithTenantSlot returns a context carrying a fresh slot, and the slot.
func WithTenantSlot(ctx context.Context) (context.Context, *TenantSlot) {
	s := &TenantSlot{}
	return context.WithValue(ctx, tenantSlotKey{}, s), s
}

// TenantSlotFrom returns the slot on the context, or nil when there is none.
func TenantSlotFrom(ctx context.Context) *TenantSlot {
	s, _ := ctx.Value(tenantSlotKey{}).(*TenantSlot)
	return s
}

// tenantSlugRe is the one spelling of a tenant a request may name.
//
// A slug becomes a schema or database name, so it is bounded to what is safe
// as an identifier on every dialect: lower case, starting with a letter, and
// short enough for the tightest of the three limits.
var tenantSlugRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// IsValidTenantSlug reports whether s is a tenant slug in canonical form.
//
// The canonical form has to be checked in Go rather than left to the roster
// query. MySQL and MSSQL collate their default string columns case
// insensitively, so `SELECT ... WHERE slug = 'Acme'` answers with the row for
// `acme` and the request then runs under the spelling the caller sent: the
// isolation predicate, the audit record and the cache key all carry a name the
// engine never issued, and the same request behaves differently on PostgreSQL,
// where the comparison is case sensitive. Refusing a non-canonical spelling
// before the lookup makes the three dialects agree and costs a regex.
func IsValidTenantSlug(s string) bool { return tenantSlugRe.MatchString(s) }
