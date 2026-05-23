package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A single-tenant deployment runs as the slug "default", which names no
// database of its own. PostgreSQL never had to care, because Apply sets
// `search_path = "tenant_default", public` and a search path naming a schema
// that does not exist falls through to public. MySQL and MSSQL have no
// fallback: `USE tenant_default` is an error, and the request dies before it
// reaches a handler with
//
//	Error 1049 (42000): Unknown database 'tenant_default'
//
// The implicit tenant belongs on the engine's own database. It does not get
// there by being left alone, though: the connection carries whatever the
// previous request did to it. Apply puts it back, which is what the real-DB
// tests in tenancy_stranded_connection_test.go exercise. With no default
// database configured there is nothing to put it back to, and Apply must still
// never reach for tenant_default.
func TestDatabaseTenancy_TheImplicitTenantDoesNotSwitchDatabase(t *testing.T) {
	tenantFn := func(context.Context) string { return implicitTenant }

	for name, tenancy := range map[string]Tenancy{
		"mysql": NewMySQLDatabaseTenancy(tenantFn, ""),
		"mssql": NewMSSQLDatabaseTenancy(tenantFn, ""),
	} {
		t.Run(name, func(t *testing.T) {
			// A nil *sql.Conn is enough: Apply must return before it touches it.
			require.NotPanics(t, func() {
				assert.NoError(t, tenancy.Apply(context.Background(), nil))
			}, "the implicit tenant must not reach a USE tenant_default")
		})
	}
}

// A named tenant still switches, so isolation is unchanged for real tenants.
// With a nil conn the switch cannot succeed, which is the point: it was
// attempted.
func TestDatabaseTenancy_ANamedTenantStillSwitchesDatabase(t *testing.T) {
	tenantFn := func(context.Context) string { return "acme" }

	for name, tenancy := range map[string]Tenancy{
		"mysql": NewMySQLDatabaseTenancy(tenantFn, "lyeve"),
		"mssql": NewMSSQLDatabaseTenancy(tenantFn, "lyeve"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Panics(t, func() {
				_ = tenancy.Apply(context.Background(), nil)
			}, "a named tenant must still attempt the switch")
		})
	}
}

// An unusable default database name must not fail the request. Refusing to
// serve because a name is missing or malformed would turn a configuration gap
// into an outage, so the request reaches its handler on whatever database the
// connection already had.
func TestDatabaseTenancy_AnUnusableDefaultDatabaseIsNotAnError(t *testing.T) {
	tenantFn := func(context.Context) string { return implicitTenant }

	for _, name := range []string{"", "unsafe db", "has`backtick", "has]bracket", "a;DROP DATABASE x"} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, safeDatabaseNameRe.MatchString(name), "must be rejected by the guard")
			require.NotPanics(t, func() {
				assert.NoError(t, NewMySQLDatabaseTenancy(tenantFn, name).Apply(context.Background(), nil))
				assert.NoError(t, NewMSSQLDatabaseTenancy(tenantFn, name).Apply(context.Background(), nil))
			})
		})
	}
}

// The engine's own database is named by whoever deployed it, not by the tenant
// slug rules, so the guard on it has to accept the names people actually use.
func TestDatabaseTenancy_TheDefaultDatabaseGuardAcceptsRealDatabaseNames(t *testing.T) {
	for _, name := range []string{"lyeve", "engine_db", "LyEveProd", "lyeve-prod", "cms$1", "master"} {
		assert.True(t, safeDatabaseNameRe.MatchString(name), "%q is a legal database name", name)
	}
}
