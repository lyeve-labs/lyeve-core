package db

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

const engineDB = "engine_db"

func TestQualifySysTables_MySQL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "table in FROM position",
			in:   "SELECT id FROM sys_users WHERE tenant_id = $1",
			want: "SELECT id FROM `engine_db`.`sys_users` WHERE tenant_id = $1",
		},
		{
			name: "insert target",
			in:   "INSERT INTO sys_orders (id, tenant_id) VALUES ($1, $2)",
			want: "INSERT INTO `engine_db`.`sys_orders` (id, tenant_id) VALUES ($1, $2)",
		},
		{
			name: "update and delete targets",
			in:   "UPDATE sys_widgets SET size = $1; DELETE FROM sys_orders",
			want: "UPDATE `engine_db`.`sys_widgets` SET size = $1; DELETE FROM `engine_db`.`sys_orders`",
		},
		{
			name: "join qualifies both tables and leaves column references alone",
			in:   "SELECT sys_users.email FROM sys_users JOIN sys_teams ON sys_teams.owner_id = sys_users.id",
			want: "SELECT sys_users.email FROM `engine_db`.`sys_users` JOIN `engine_db`.`sys_teams` ON sys_teams.owner_id = sys_users.id",
		},
		{
			name: "already qualified reference is left alone",
			in:   "SELECT 1 FROM public.sys_users",
			want: "SELECT 1 FROM public.sys_users",
		},
		{
			name: "table name as data stays a string literal",
			in:   "SELECT 1 FROM information_schema.tables WHERE table_name = 'sys_users'",
			want: "SELECT 1 FROM information_schema.tables WHERE table_name = 'sys_users'",
		},
		{
			name: "escaped quote inside a literal does not end it",
			in:   "SELECT 'it''s sys_users' FROM sys_widgets",
			want: "SELECT 'it''s sys_users' FROM `engine_db`.`sys_widgets`",
		},
		{
			name: "line comment is copied verbatim",
			in:   "-- sys_users lives in the engine database\nSELECT 1 FROM sys_widgets",
			want: "-- sys_users lives in the engine database\nSELECT 1 FROM `engine_db`.`sys_widgets`",
		},
		{
			name: "block comment is copied verbatim",
			in:   "SELECT /* not sys_users */ 1 FROM sys_widgets",
			want: "SELECT /* not sys_users */ 1 FROM `engine_db`.`sys_widgets`",
		},
		{
			name: "backtick-quoted table name is qualified once",
			in:   "SELECT 1 FROM `sys_users`",
			want: "SELECT 1 FROM `engine_db`.`sys_users`",
		},
		{
			name: "identifier that merely contains the prefix is untouched",
			in:   "SELECT 1 FROM plugin_sys_users",
			want: "SELECT 1 FROM plugin_sys_users",
		},
		{
			name: "per-tenant content table is untouched",
			in:   "SELECT id FROM _article WHERE id = $1",
			want: "SELECT id FROM _article WHERE id = $1",
		},
		{
			name: "no sys reference is a passthrough",
			in:   "SELECT id FROM _article JOIN _author ON _author.id = _article.author_id",
			want: "SELECT id FROM _article JOIN _author ON _author.id = _article.author_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualifySysTables(tc.in, "mysql", engineDB); got != tc.want {
				t.Errorf("QualifySysTables()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestQualifySysTables_MSSQL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The empty middle part resolves against the caller's default
			// schema, exactly as the unqualified name would have.
			name: "table gets the database and an empty schema part",
			in:   "SELECT id FROM sys_users WHERE tenant_id = $1",
			want: "SELECT id FROM [engine_db]..[sys_users] WHERE tenant_id = $1",
		},
		{
			name: "column reference survives on the correlation name",
			in:   "SELECT sys_users.email FROM sys_users",
			want: "SELECT sys_users.email FROM [engine_db]..[sys_users]",
		},
		{
			name: "bracket-quoted table name is qualified once",
			in:   "SELECT 1 FROM [sys_users]",
			want: "SELECT 1 FROM [engine_db]..[sys_users]",
		},
		{
			name: "sys catalog views are not sys_ tables",
			in:   "SELECT name FROM sys.tables WHERE name = 'sys_users'",
			want: "SELECT name FROM sys.tables WHERE name = 'sys_users'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualifySysTables(tc.in, "mssql", "engine_db"); got != tc.want {
				t.Errorf("QualifySysTables()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Postgres resolves sys_* through the public entry on its search_path, so the
// statement it is handed must come out byte-for-byte unchanged.
func TestQualifySysTables_PostgresIsPassthrough(t *testing.T) {
	in := "SELECT id FROM sys_users WHERE tenant_id = $1"
	if got := QualifySysTables(in, "postgres", "engine_db"); got != in {
		t.Errorf("postgres was rewritten: %s", got)
	}
}

// A database name the server would not report, or one that could not survive
// quoting, disables the rewrite rather than producing SQL out of it.
func TestQualifySysTables_UnusableDatabaseName(t *testing.T) {
	in := "SELECT id FROM sys_users"
	for _, name := range []string{"", "no`quote", "has space", "a;DROP TABLE x"} {
		if got := QualifySysTables(in, "mysql", name); got != in {
			t.Errorf("database name %q produced a rewrite: %s", name, got)
		}
	}
}

// The qualifier runs before the placeholder pass. What it inserts must not
// disturb that pass, and the two together must produce the statement the driver
// is actually sent.
func TestQualifySysTables_ComposesWithPlaceholderRewrite(t *testing.T) {
	in := "SELECT id FROM sys_users WHERE tenant_id = $2 AND email = $1"

	q, args := rewritePlaceholders(QualifySysTables(in, "mysql", engineDB), "mysql", []any{"a@b", "acme"})
	wantMySQL := "SELECT id FROM `engine_db`.`sys_users` WHERE tenant_id = ? AND email = ?"
	if q != wantMySQL {
		t.Errorf("mysql\n got: %s\nwant: %s", q, wantMySQL)
	}
	if len(args) != 2 || args[0] != "acme" || args[1] != "a@b" {
		t.Errorf("mysql args were not reordered to match the ? positions: %v", args)
	}

	q, _ = rewritePlaceholders(QualifySysTables(in, "mssql", "engine_db"), "mssql", []any{"a@b", "acme"})
	wantMSSQL := "SELECT id FROM [engine_db]..[sys_users] WHERE tenant_id = @p2 AND email = @p1"
	if q != wantMSSQL {
		t.Errorf("mssql\n got: %s\nwant: %s", q, wantMSSQL)
	}
}

// An unterminated literal or identifier is malformed SQL either way. It must
// come back byte-identical so the driver reports the real syntax error rather
// than one the rewrite invented.
func TestQualifySysTables_UnterminatedRegionsAreUntouched(t *testing.T) {
	for _, in := range []string{
		"SELECT 'sys_users",
		"SELECT 1 FROM `sys_users",
		"SELECT 1 FROM sys_users -- trailing",
		"SELECT /* sys_users",
	} {
		got := QualifySysTables(in, "mysql", engineDB)
		if in == "SELECT 1 FROM sys_users -- trailing" {
			if got != "SELECT 1 FROM `engine_db`.`sys_users` -- trailing" {
				t.Errorf("%q -> %q", in, got)
			}
			continue
		}
		if got != in {
			t.Errorf("%q was rewritten to %q", in, got)
		}
	}
}

func FuzzQualifySysTables(f *testing.F) {
	f.Add("SELECT id FROM sys_users WHERE tenant_id = $1")
	f.Add("INSERT INTO sys_widgets (id) VALUES ($1) ON DUPLICATE KEY UPDATE id = VALUES(id)")
	f.Add("SELECT 'sys_x' FROM `sys_y` /* sys_z */ -- sys_w")
	f.Fuzz(func(t *testing.T, q string) {
		for _, engine := range []string{"mysql", "mssql", "postgres"} {
			out := QualifySysTables(q, engine, engineDB)
			// The rewrite only ever inserts text. It must never drop any.
			if len(out) < len(q) {
				t.Fatalf("engine %s shortened the statement: %q -> %q", engine, q, out)
			}
			// It must be idempotent: a second pass finds every sys_* reference
			// already qualified and leaves it alone.
			if again := QualifySysTables(out, engine, engineDB); again != out {
				t.Fatalf("engine %s is not idempotent:\n1: %q\n2: %q", engine, out, again)
			}
		}
	})
}

// TestQualifySysTables_RegisteredSharedTable proves that a plugin table
// registered via sqlx.RegisterSharedTable is qualified exactly like a sys_*
// table on MySQL/MSSQL, and left alone on Postgres.
func TestQualifySysTables_RegisteredSharedTable(t *testing.T) {
	const table = "gadgets"
	sqlx.RegisterSharedTable(table)

	t.Run("mysql table in FROM position", func(t *testing.T) {
		got := QualifySysTables("SELECT id FROM gadgets WHERE tenant_id = $1", "mysql", engineDB)
		want := "SELECT id FROM `engine_db`.`gadgets` WHERE tenant_id = $1"
		if got != want {
			t.Errorf("got %s\nwant %s", got, want)
		}
	})
	t.Run("mssql insert target", func(t *testing.T) {
		got := QualifySysTables("INSERT INTO gadgets (id, tenant_id) VALUES ($1, $2)", "mssql", engineDB)
		want := "INSERT INTO [engine_db]..[gadgets] (id, tenant_id) VALUES ($1, $2)"
		if got != want {
			t.Errorf("got %s\nwant %s", got, want)
		}
	})
	t.Run("column reference is left alone", func(t *testing.T) {
		got := QualifySysTables("SELECT gadgets.id FROM gadgets WHERE tenant_id = $1", "mysql", engineDB)
		want := "SELECT gadgets.id FROM `engine_db`.`gadgets` WHERE tenant_id = $1"
		if got != want {
			t.Errorf("got %s\nwant %s", got, want)
		}
	})
	t.Run("already qualified is left alone", func(t *testing.T) {
		got := QualifySysTables("SELECT 1 FROM public.gadgets", "mysql", engineDB)
		if got != "SELECT 1 FROM public.gadgets" {
			t.Errorf("got %s", got)
		}
	})
	t.Run("postgres is a passthrough", func(t *testing.T) {
		got := QualifySysTables("SELECT id FROM gadgets WHERE tenant_id = $1", "postgres", engineDB)
		if got != "SELECT id FROM gadgets WHERE tenant_id = $1" {
			t.Errorf("got %s", got)
		}
	})
}
