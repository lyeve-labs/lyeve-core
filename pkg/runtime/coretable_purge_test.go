package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The purge list is what the boot-time validator checks tenant_id-bearing
// tables against, so a core-owned table missing from it boots with a warning
// and leaks on every tenant delete.
//
// The other half is that a table this engine does not create must not be on
// it. Membership and the admin token tables belong to the tenancy plugin, and
// the plugin purges them. Naming them here would make a build without that
// plugin delete from tables it never created, and a build with it delete the
// same rows twice.
func TestCoreOwnedTenantTables_ExcludesTablesTheKernelDoesNotCreate(t *testing.T) {
	for _, table := range []string{"sys_user_tenants", "sys_admin_tokens", "sys_admin_token_requests"} {
		require.NotContains(t, coreOwnedTenantTables, table)
	}
}

// An approved device sign-in names its tenant and goes with it.
func TestCoreOwnedTenantTables_CoversDeviceLogins(t *testing.T) {
	require.Contains(t, coreOwnedTenantTables, "sys_device_logins")
}
