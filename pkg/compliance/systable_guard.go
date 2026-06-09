// sys_* table DDL guard (public API).
//
// Provides CheckSysTableDDL as a public sentinel so plugins outside the core
// module can reject destructive DDL targeting core system tables.

package compliance

import (
	"errors"
	"regexp"
	"strings"
)

// ErrSysTableDDL is returned when a DDL statement attempts to modify a core
// sys_* table. Plugins and tests can use errors.Is(err, ErrSysTableDDL).
var ErrSysTableDDL = errors.New("sys_* table DDL is prohibited: a system table cannot be dropped, truncated or renamed through an API endpoint")

// sysTableDDLRe matches DROP TABLE, DROP SCHEMA, TRUNCATE and ALTER TABLE
// against any sys_* table. It ignores case and accepts IF EXISTS, a public.
// qualifier and double-quote, backtick or bracket quoting.
var sysTableDDLRe = regexp.MustCompile(
	`(?i)(?:` +
		`DROP\s+(?:TABLE|SCHEMA)(?:\s+IF\s+EXISTS)?\s+(?:["\x60\[]?(?:public\.)?sys_\w+)` +
		`|TRUNCATE\s+(?:TABLE\s+)?(?:["\x60\[]?sys_\w+)` +
		`|ALTER\s+TABLE\s+(?:["\x60\[]?(?:public\.)?sys_\w+)` +
		`)`)

// CheckSysTableDDL returns ErrSysTableDDL if sql targets a sys_* table for a
// destructive DDL operation (DROP, TRUNCATE, ALTER). Returns nil if the
// statement is safe. Call this on untrusted DDL (user input, a recorded-DDL
// replay). For TRUSTED plugin migration rollback use CheckCoreSysTableDDL.
func CheckSysTableDDL(sql string) error {
	if sysTableDDLRe.MatchString(strings.TrimSpace(sql)) {
		return ErrSysTableDDL
	}
	return nil
}

// coreSysTableDDLRe matches destructive DDL against only the CORE
// engine-owned sys_ tables (sys_users, the tables this module creates): NOT
// plugin-owned tables that use the sys_ prefix by convention (sys_content_*,
// sys_log_*, sys_api_keys, sys_tenants, ...), which their owning plugin's
// rollback must be able to drop.
var coreSysTableDDLRe = regexp.MustCompile(
	`(?i)(?:` +
		`DROP\s+(?:TABLE|SCHEMA)(?:\s+IF\s+EXISTS)?\s+["\x60\[]?(?:public\.)?sys_(?:users|setup_lock|plugin_config|device_logins)\b` +
		`|TRUNCATE\s+(?:TABLE\s+)?["\x60\[]?sys_(?:users|setup_lock|plugin_config|device_logins)\b` +
		`|ALTER\s+TABLE\s+["\x60\[]?(?:public\.)?sys_(?:users|setup_lock|plugin_config|device_logins)\b` +
		`)`)

// CheckCoreSysTableDDL returns ErrSysTableDDL only if sql targets a core
// engine-owned sys_ table. Use on the TRUSTED plugin migration rollback path.
// A plugin may drop its own sys_-prefixed tables but never the core ones.
func CheckCoreSysTableDDL(sql string) error {
	if coreSysTableDDLRe.MatchString(strings.TrimSpace(sql)) {
		return ErrSysTableDDL
	}
	return nil
}
