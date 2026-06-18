package sqldialect_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// Upsert (update on conflict)

func TestUpsert_UpdateOnConflict(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		cfg     sqldialect.UpsertConfig
		want    string
	}{
		{
			name:    "postgres single conflict col",
			dialect: sqldialect.DialectPostgres,
			cfg: sqldialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want: "INSERT INTO item_codes (id, code, title, severity, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (code) DO UPDATE SET id = EXCLUDED.id, title = EXCLUDED.title, severity = EXCLUDED.severity, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at",
		},
		{
			name:    "mysql single conflict col",
			dialect: sqldialect.DialectMySQL,
			cfg: sqldialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want: "INSERT INTO item_codes (id, code, title, severity, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) ON DUPLICATE KEY UPDATE id = $1, title = $3, severity = $4, created_at = $5, updated_at = $6",
		},
		{
			name:    "mssql single conflict col",
			dialect: sqldialect.DialectMSSQL,
			cfg: sqldialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want: "MERGE item_codes WITH (HOLDLOCK) AS t USING (VALUES ($1, $2, $3, $4, $5, $6)) AS s(id, code, title, severity, created_at, updated_at) ON s.code = t.code WHEN MATCHED THEN UPDATE SET id = s.id, title = s.title, severity = s.severity, created_at = s.created_at, updated_at = s.updated_at WHEN NOT MATCHED THEN INSERT (id, code, title, severity, created_at, updated_at) VALUES (s.id, s.code, s.title, s.severity, s.created_at, s.updated_at);",
		},
		{
			name:    "postgres composite conflict cols",
			dialect: sqldialect.DialectPostgres,
			cfg: sqldialect.UpsertConfig{
				Table:      "item_votes",
				InsertCols: []string{"item_id", "user_id", "vote", "created_at"},
				ConflictOn: []string{"item_id", "user_id"},
			},
			want: "INSERT INTO item_votes (item_id, user_id, vote, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT (item_id, user_id) DO UPDATE SET vote = EXCLUDED.vote, created_at = EXCLUDED.created_at",
		},
		{
			name:    "postgres explicit update cols subset",
			dialect: sqldialect.DialectPostgres,
			cfg: sqldialect.UpsertConfig{
				Table:      "user_settings",
				InsertCols: []string{"user_id", "setting", "enabled", "created_at", "updated_at"},
				ConflictOn: []string{"user_id"},
				UpdateCols: []string{"setting", "updated_at"},
			},
			want: "INSERT INTO user_settings (user_id, setting, enabled, created_at, updated_at) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (user_id) DO UPDATE SET setting = EXCLUDED.setting, updated_at = EXCLUDED.updated_at",
		},
		{
			name:    "mysql explicit update cols subset",
			dialect: sqldialect.DialectMySQL,
			cfg: sqldialect.UpsertConfig{
				Table:      "user_settings",
				InsertCols: []string{"user_id", "setting", "enabled", "created_at", "updated_at"},
				ConflictOn: []string{"user_id"},
				UpdateCols: []string{"setting", "updated_at"},
			},
			// colPlaceholder maps each update col to its 1-based insert index:
			// setting -> $2, updated_at -> $5.
			want: "INSERT INTO user_settings (user_id, setting, enabled, created_at, updated_at) VALUES ($1, $2, $3, $4, $5) ON DUPLICATE KEY UPDATE setting = $2, updated_at = $5",
		},
		{
			name:    "mysql update col not in insert cols falls back to placeholder",
			dialect: sqldialect.DialectMySQL,
			cfg: sqldialect.UpsertConfig{
				Table:      "t",
				InsertCols: []string{"a", "b"},
				ConflictOn: []string{"a"},
				UpdateCols: []string{"b", "missing"},
			},
			// "missing" is absent from InsertCols, so colPlaceholder returns "?".
			want: "INSERT INTO t (a, b) VALUES ($1, $2) ON DUPLICATE KEY UPDATE b = $2, missing = ?",
		},
		{
			name:    "mssql composite conflict",
			dialect: sqldialect.DialectMSSQL,
			cfg: sqldialect.UpsertConfig{
				Table:      "item_votes",
				InsertCols: []string{"item_id", "user_id", "vote", "created_at"},
				ConflictOn: []string{"item_id", "user_id"},
			},
			want: "MERGE item_votes WITH (HOLDLOCK) AS t USING (VALUES ($1, $2, $3, $4)) AS s(item_id, user_id, vote, created_at) ON s.item_id = t.item_id AND s.user_id = t.user_id WHEN MATCHED THEN UPDATE SET vote = s.vote, created_at = s.created_at WHEN NOT MATCHED THEN INSERT (item_id, user_id, vote, created_at) VALUES (s.item_id, s.user_id, s.vote, s.created_at);",
		},
		{
			name:    "unknown dialect falls back to postgres",
			dialect: "cockroachdb",
			cfg: sqldialect.UpsertConfig{
				Table:      "t",
				InsertCols: []string{"a", "b"},
				ConflictOn: []string{"a"},
			},
			want: "INSERT INTO t (a, b) VALUES ($1, $2) ON CONFLICT (a) DO UPDATE SET b = EXCLUDED.b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.Upsert(tt.dialect, tt.cfg)
			if got != tt.want {
				t.Errorf("Upsert() =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}

// Upsert (do nothing)

func TestUpsert_DoNothing(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		want    string
	}{
		{
			name:    "postgres do nothing",
			dialect: sqldialect.DialectPostgres,
			want:    "INSERT INTO events (event_id, handler) VALUES ($1, $2) ON CONFLICT (event_id, handler) DO NOTHING",
		},
		{
			name:    "mysql do nothing",
			dialect: sqldialect.DialectMySQL,
			want:    "INSERT INTO events (event_id, handler) SELECT $1, $2 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM events AS existing WHERE existing.event_id = $1 AND existing.handler = $2)",
		},
		{
			name:    "mssql do nothing",
			dialect: sqldialect.DialectMSSQL,
			want:    "MERGE events WITH (HOLDLOCK) AS t USING (VALUES ($1, $2)) AS s(event_id, handler) ON s.event_id = t.event_id AND s.handler = t.handler WHEN NOT MATCHED THEN INSERT (event_id, handler) VALUES (s.event_id, s.handler);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.Upsert(tt.dialect, sqldialect.UpsertConfig{
				Table:      "events",
				InsertCols: []string{"event_id", "handler"},
				ConflictOn: []string{"event_id", "handler"},
				DoNothing:  true,
			})
			if got != tt.want {
				t.Errorf("Upsert(DoNothing) =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}

// InsertDoNothing

func TestInsertDoNothing(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		want    string
	}{
		{
			name:    "postgres",
			dialect: sqldialect.DialectPostgres,
			want:    "INSERT INTO request_nonces (nonce) VALUES ($1) ON CONFLICT (nonce) DO NOTHING",
		},
		{
			name:    "mysql",
			dialect: sqldialect.DialectMySQL,
			want:    "INSERT INTO request_nonces (nonce) SELECT $1 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM request_nonces AS existing WHERE existing.nonce = $1)",
		},
		{
			name:    "mssql",
			dialect: sqldialect.DialectMSSQL,
			want:    "MERGE request_nonces WITH (HOLDLOCK) AS t USING (VALUES ($1)) AS s(nonce) ON s.nonce = t.nonce WHEN NOT MATCHED THEN INSERT (nonce) VALUES (s.nonce);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.InsertDoNothing(tt.dialect, "request_nonces", []string{"nonce"}, []string{"nonce"})
			if got != tt.want {
				t.Errorf("InsertDoNothing() =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}

// The MySQL statement guards its insert by comparing the conflict key against
// the values the same statement binds. A conflict column the insert does not
// bind has no value to compare, so the builder falls back to swallowing the
// duplicate key rather than emitting a statement short of an argument.
func TestInsertDoNothing_MySQLUnboundConflictColumn(t *testing.T) {
	got := sqldialect.InsertDoNothing(sqldialect.DialectMySQL, "t", []string{"a"}, []string{"b"})
	want := "INSERT INTO t (a) VALUES ($1) ON DUPLICATE KEY UPDATE b = b"
	if got != want {
		t.Errorf("InsertDoNothing() =\n  %q\nwant:\n  %q", got, want)
	}
}

// Every conflict column has to appear in the guard, or a statement with a
// composite key probes for the wrong row and inserts a duplicate.
func TestInsertDoNothing_MySQLCompositeKeyGuard(t *testing.T) {
	got := sqldialect.InsertDoNothing(sqldialect.DialectMySQL, "t",
		[]string{"id", "tenant_id", "name"}, []string{"tenant_id", "name"})
	want := "INSERT INTO t (id, tenant_id, name) SELECT $1, $2, $3 FROM DUAL " +
		"WHERE NOT EXISTS (SELECT 1 FROM t AS existing WHERE existing.tenant_id = $2 AND existing.name = $3)"
	if got != want {
		t.Errorf("InsertDoNothing() =\n  %q\nwant:\n  %q", got, want)
	}
}

// ILike

func TestILike(t *testing.T) {
	tests := []struct {
		name        string
		dialect     string
		column      string
		placeholder string
		want        string
	}{
		{
			name:        "postgres",
			dialect:     sqldialect.DialectPostgres,
			column:      "operation_name",
			placeholder: "$1",
			want:        "operation_name ILIKE '%' || $1 || '%'",
		},
		{
			name:        "mysql",
			dialect:     sqldialect.DialectMySQL,
			column:      "operation_name",
			placeholder: "$1",
			want:        "operation_name LIKE CONCAT('%', $1, '%')",
		},
		{
			name:        "mssql",
			dialect:     sqldialect.DialectMSSQL,
			column:      "operation_name",
			placeholder: "$1",
			want:        "CHARINDEX($1, operation_name) > 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.ILike(tt.dialect, tt.column, tt.placeholder)
			if got != tt.want {
				t.Errorf("ILike() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Cast

func TestCast(t *testing.T) {
	tests := []struct {
		name        string
		dialect     string
		expr        string
		targetType  string
		pgShorthand []bool
		want        string
	}{
		{
			name:       "postgres default shorthand",
			dialect:    sqldialect.DialectPostgres,
			expr:       "u.updated_at",
			targetType: "text",
			want:       "u.updated_at::text",
		},
		{
			name:        "postgres explicit no shorthand",
			dialect:     sqldialect.DialectPostgres,
			expr:        "u.updated_at",
			targetType:  "CHAR",
			pgShorthand: []bool{false},
			want:        "CAST(u.updated_at AS CHAR)",
		},
		{
			name:        "postgres explicit shorthand true",
			dialect:     sqldialect.DialectPostgres,
			expr:        "u.updated_at",
			targetType:  "text",
			pgShorthand: []bool{true},
			want:        "u.updated_at::text",
		},
		{
			name:       "mysql",
			dialect:    sqldialect.DialectMySQL,
			expr:       "u.updated_at",
			targetType: "CHAR",
			want:       "CAST(u.updated_at AS CHAR)",
		},
		{
			name:       "mssql",
			dialect:    sqldialect.DialectMSSQL,
			expr:       "data",
			targetType: "NVARCHAR(MAX)",
			want:       "CAST(data AS NVARCHAR(MAX))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.Cast(tt.dialect, tt.expr, tt.targetType, tt.pgShorthand...)
			if got != tt.want {
				t.Errorf("Cast() = %q, want %q", got, tt.want)
			}
		})
	}
}

// LimitOffset

func TestLimitOffset(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		limit   int
		offset  int
		want    string
	}{
		{name: "postgres", dialect: sqldialect.DialectPostgres, limit: 20, offset: 40, want: "LIMIT 20 OFFSET 40"},
		{name: "mysql", dialect: sqldialect.DialectMySQL, limit: 20, offset: 40, want: "LIMIT 20 OFFSET 40"},
		{name: "mssql", dialect: sqldialect.DialectMSSQL, limit: 20, offset: 40, want: "OFFSET 40 ROWS FETCH NEXT 20 ROWS ONLY"},
		{name: "mssql single row uses ROW", dialect: sqldialect.DialectMSSQL, limit: 1, offset: 0, want: "OFFSET 0 ROWS FETCH NEXT 1 ROW ONLY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.LimitOffset(tt.dialect, tt.limit, tt.offset)
			if got != tt.want {
				t.Errorf("LimitOffset() = %q, want %q", got, tt.want)
			}
		})
	}
}

// LimitOffsetPlaceholders

func TestLimitOffsetPlaceholders(t *testing.T) {
	tests := []struct {
		name      string
		dialect   string
		limitIdx  int
		offsetIdx int
		want      string
	}{
		{name: "postgres", dialect: sqldialect.DialectPostgres, limitIdx: 3, offsetIdx: 4, want: "LIMIT $3 OFFSET $4"},
		{name: "mysql", dialect: sqldialect.DialectMySQL, limitIdx: 3, offsetIdx: 4, want: "LIMIT $3 OFFSET $4"},
		{name: "mssql", dialect: sqldialect.DialectMSSQL, limitIdx: 3, offsetIdx: 4, want: "OFFSET CAST(CASE WHEN $3 <= 0 THEN 9223372036854775807 ELSE $4 END AS BIGINT) ROWS FETCH NEXT CAST(CASE WHEN $3 <= 0 THEN 1 ELSE $3 END AS BIGINT) ROWS ONLY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.LimitOffsetPlaceholders(tt.dialect, tt.limitIdx, tt.offsetIdx)
			if got != tt.want {
				t.Errorf("LimitOffsetPlaceholders() = %q, want %q", got, tt.want)
			}
		})
	}
}

// QuoteIdentifier

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		ident   string
		want    string
	}{
		{name: "postgres simple", dialect: sqldialect.DialectPostgres, ident: "users", want: `"users"`},
		{name: "mysql simple", dialect: sqldialect.DialectMySQL, ident: "users", want: "`users`"},
		{name: "mssql simple", dialect: sqldialect.DialectMSSQL, ident: "users", want: "[users]"},
		{name: "unknown dialect falls back to postgres", dialect: "oracle", ident: "users", want: `"users"`},
		// Embedded close-quote characters are doubled per dialect convention.
		{name: "postgres doubles embedded double-quote", dialect: sqldialect.DialectPostgres, ident: `a"b`, want: `"a""b"`},
		{name: "mysql doubles embedded backtick", dialect: sqldialect.DialectMySQL, ident: "a`b", want: "`a``b`"},
		{name: "mssql doubles embedded close-bracket", dialect: sqldialect.DialectMSSQL, ident: "a]b", want: "[a]]b]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqldialect.QuoteIdentifier(tt.dialect, tt.ident)
			if got != tt.want {
				t.Errorf("QuoteIdentifier() = %q, want %q", got, tt.want)
			}
		})
	}
}
