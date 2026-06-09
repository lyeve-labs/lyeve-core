package compliance

import (
	"errors"
	"testing"
)

func TestCheckSysTableDDL(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		refused bool
	}{
		{name: "DROP TABLE sys_users", sql: "DROP TABLE sys_users", refused: true},
		{name: "DROP TABLE IF EXISTS sys_users", sql: "DROP TABLE IF EXISTS sys_users", refused: true},
		{name: "DROP TABLE public.sys_users", sql: "DROP TABLE public.sys_users", refused: true},
		{name: "DROP TABLE IF EXISTS public.sys_users", sql: "DROP TABLE IF EXISTS public.sys_users", refused: true},
		{name: "DROP TABLE \"sys_users\"", sql: "DROP TABLE \"sys_users\"", refused: true},
		{name: "DROP TABLE `sys_users`", sql: "DROP TABLE `sys_users`", refused: true},
		{name: "DROP TABLE [sys_users]", sql: "DROP TABLE [sys_users]", refused: true},
		{name: "drop table sys_users (lowercase)", sql: "drop table sys_users", refused: true},

		{name: "DROP TABLE _articles", sql: "DROP TABLE _articles", refused: false},
		{name: "DROP TABLE IF EXISTS _blog_posts", sql: "DROP TABLE IF EXISTS _blog_posts", refused: false},
		{name: "DROP TABLE tenant_foo._articles", sql: "DROP TABLE tenant_foo._articles", refused: false},

		{name: "DROP SCHEMA sys_foo", sql: "DROP SCHEMA sys_foo CASCADE", refused: true},
		{name: "DROP SCHEMA IF EXISTS sys_foo", sql: "DROP SCHEMA IF EXISTS sys_foo CASCADE", refused: true},

		{name: "DROP SCHEMA tenant_test", sql: "DROP SCHEMA tenant_test CASCADE", refused: false},
		{name: "DROP SCHEMA IF EXISTS tenant_foo", sql: "DROP SCHEMA IF EXISTS tenant_foo CASCADE", refused: false},

		{name: "TRUNCATE sys_users", sql: "TRUNCATE sys_users", refused: true},
		{name: "TRUNCATE TABLE sys_users", sql: "TRUNCATE TABLE sys_users", refused: true},

		{name: "TRUNCATE _articles", sql: "TRUNCATE _articles", refused: false},

		// Any ALTER on a sys_* table is refused, not only a rename.
		{name: "ALTER TABLE sys_users RENAME TO", sql: "ALTER TABLE sys_users RENAME TO foo", refused: true},
		{name: "ALTER TABLE public.sys_users RENAME TO", sql: "ALTER TABLE public.sys_users RENAME TO foo", refused: true},
		{name: "ALTER TABLE sys_users ADD COLUMN", sql: "ALTER TABLE sys_users ADD COLUMN foo TEXT", refused: true},

		// DML is not DDL, so the guard lets it through.
		{name: "SELECT from sys_users", sql: "SELECT * FROM sys_users", refused: false},
		{name: "INSERT into sys_users", sql: "INSERT INTO sys_users (email) VALUES ('a@b.com')", refused: false},
		{name: "UPDATE sys_users", sql: "UPDATE sys_users SET email='a' WHERE id='x'", refused: false},
		{name: "DELETE from sys_users", sql: "DELETE FROM sys_users WHERE id='x'", refused: false},

		{name: "whitespace before DROP", sql: "  \n  DROP TABLE sys_users", refused: true},
		// The pattern is not anchored, so a leading comment line does not hide the DROP.
		{name: "sql with comment before DROP", sql: "-- cleanup\nDROP TABLE sys_users", refused: true},
		{name: "create sys_users (safe)", sql: "CREATE TABLE IF NOT EXISTS sys_users (id UUID)", refused: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSysTableDDL(tt.sql)
			if tt.refused && !errors.Is(err, ErrSysTableDDL) {
				t.Errorf("CheckSysTableDDL(%q) = %v, want ErrSysTableDDL", tt.sql, err)
			}
			if !tt.refused && err != nil {
				t.Errorf("CheckSysTableDDL(%q) = %v, want nil", tt.sql, err)
			}
		})
	}
}

// TestCheckCoreSysTableDDL covers the rollback-path guard. It refuses DDL on
// the four kernel tables and lets a plugin drop its own sys_-prefixed tables.
func TestCheckCoreSysTableDDL(t *testing.T) {
	blocked := []string{
		"DROP TABLE sys_users",
		"DROP TABLE IF EXISTS sys_setup_lock",
		"DROP TABLE IF EXISTS public.sys_plugin_config",
		"TRUNCATE TABLE sys_device_logins",
		"ALTER TABLE sys_users DROP COLUMN email",
		"DROP TABLE [sys_users]",
	}
	for _, sql := range blocked {
		if err := CheckCoreSysTableDDL(sql); !errors.Is(err, ErrSysTableDDL) {
			t.Errorf("expected ErrSysTableDDL (core table) for %q, got %v", sql, err)
		}
	}
	// Plugin-owned sys_-prefixed tables must be droppable on rollback.
	allowed := []string{
		"DROP TABLE IF EXISTS sys_widget_entries",
		"DROP TABLE IF EXISTS sys_widget_config",
		"DROP TABLE sys_example_entries",
		"DROP TABLE IF EXISTS sys_widgets",
		"DROP TABLE IF EXISTS sys_user_sessions", // shares a prefix with sys_users
		"DROP SCHEMA sys_foo CASCADE",
	}
	for _, sql := range allowed {
		if err := CheckCoreSysTableDDL(sql); err != nil {
			t.Errorf("expected nil (plugin-owned table) for %q, got %v", sql, err)
		}
	}
}
