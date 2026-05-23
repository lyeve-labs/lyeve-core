// helpers_test.go: Table-driven tests for dialect-aware SQL helpers.
//
// Tests all three dialects (Postgres, MySQL, MSSQL) for each helper function
// to ensure cross-dialect parity.

package dialect_test

import (
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// Upsert

func TestUpsert_UpdateOnConflict(t *testing.T) {
	tests := []struct {
		name    string
		d       dialect.Dialect
		cfg     dialect.UpsertConfig
		want    string
		wantNot string // must NOT appear in output
	}{
		{
			name: "postgres single conflict col",
			d:    dialect.Postgres{},
			cfg: dialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want:    "INSERT INTO item_codes (id, code, title, severity, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (code) DO UPDATE SET id = EXCLUDED.id, title = EXCLUDED.title, severity = EXCLUDED.severity, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at",
			wantNot: "MERGE",
		},
		{
			name: "mysql single conflict col",
			d:    dialect.MySQL{},
			cfg: dialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want:    "INSERT INTO item_codes (id, code, title, severity, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) ON DUPLICATE KEY UPDATE id = $1, title = $3, severity = $4, created_at = $5, updated_at = $6",
			wantNot: "MERGE",
		},
		{
			name: "mssql single conflict col",
			d:    dialect.MSSQL{},
			cfg: dialect.UpsertConfig{
				Table:      "item_codes",
				InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
				ConflictOn: []string{"code"},
			},
			want:    "MERGE item_codes WITH (HOLDLOCK) AS t USING (VALUES ($1, $2, $3, $4, $5, $6)) AS s(id, code, title, severity, created_at, updated_at) ON s.code = t.code WHEN MATCHED THEN UPDATE SET id = s.id, title = s.title, severity = s.severity, created_at = s.created_at, updated_at = s.updated_at WHEN NOT MATCHED THEN INSERT (id, code, title, severity, created_at, updated_at) VALUES (s.id, s.code, s.title, s.severity, s.created_at, s.updated_at);",
			wantNot: "ON CONFLICT",
		},
		{
			name: "postgres composite conflict cols",
			d:    dialect.Postgres{},
			cfg: dialect.UpsertConfig{
				Table:      "item_votes",
				InsertCols: []string{"item_id", "user_id", "vote", "created_at"},
				ConflictOn: []string{"item_id", "user_id"},
			},
			want: "ON CONFLICT (item_id, user_id) DO UPDATE SET vote = EXCLUDED.vote, created_at = EXCLUDED.created_at",
		},
		{
			name: "postgres explicit update cols subset",
			d:    dialect.Postgres{},
			cfg: dialect.UpsertConfig{
				Table:      "user_settings",
				InsertCols: []string{"user_id", "setting", "enabled", "created_at", "updated_at"},
				ConflictOn: []string{"user_id"},
				UpdateCols: []string{"setting", "updated_at"},
			},
			want:    "ON CONFLICT (user_id) DO UPDATE SET setting = EXCLUDED.setting, updated_at = EXCLUDED.updated_at",
			wantNot: "enabled = EXCLUDED.enabled",
		},
		{
			name: "mysql explicit update cols",
			d:    dialect.MySQL{},
			cfg: dialect.UpsertConfig{
				Table:      "user_settings",
				InsertCols: []string{"user_id", "setting", "enabled", "created_at", "updated_at"},
				ConflictOn: []string{"user_id"},
				UpdateCols: []string{"setting", "updated_at"},
			},
			want:    "ON DUPLICATE KEY UPDATE setting = $2, updated_at = $5",
			wantNot: "enabled =",
		},
		{
			name: "mssql composite conflict",
			d:    dialect.MSSQL{},
			cfg: dialect.UpsertConfig{
				Table:      "item_votes",
				InsertCols: []string{"item_id", "user_id", "vote", "created_at"},
				ConflictOn: []string{"item_id", "user_id"},
			},
			want: "MERGE item_votes WITH (HOLDLOCK) AS t USING (VALUES ($1, $2, $3, $4)) AS s(item_id, user_id, vote, created_at) ON s.item_id = t.item_id AND s.user_id = t.user_id WHEN MATCHED THEN UPDATE SET vote = s.vote, created_at = s.created_at WHEN NOT MATCHED THEN INSERT (item_id, user_id, vote, created_at) VALUES (s.item_id, s.user_id, s.vote, s.created_at);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.Upsert(tt.d, tt.cfg)
			if !strings.Contains(got, tt.want) {
				t.Errorf("Upsert() =\n  %q\nwant to contain:\n  %q", got, tt.want)
			}
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Errorf("Upsert() should NOT contain %q, got:\n  %s", tt.wantNot, got)
			}
		})
	}
}

func TestUpsert_DoNothing(t *testing.T) {
	tests := []struct {
		name string
		d    dialect.Dialect
		want string
	}{
		{
			name: "postgres do nothing",
			d:    dialect.Postgres{},
			want: "INSERT INTO events (event_id, handler) VALUES ($1, $2) ON CONFLICT (event_id, handler) DO NOTHING",
		},
		{
			name: "mysql do nothing",
			d:    dialect.MySQL{},
			want: "INSERT INTO events (event_id, handler) SELECT $1, $2 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM events AS existing WHERE existing.event_id = $1 AND existing.handler = $2)",
		},
		{
			name: "mssql do nothing",
			d:    dialect.MSSQL{},
			want: "MERGE events WITH (HOLDLOCK) AS t USING (VALUES ($1, $2)) AS s(event_id, handler) ON s.event_id = t.event_id AND s.handler = t.handler WHEN NOT MATCHED THEN INSERT (event_id, handler) VALUES (s.event_id, s.handler);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.Upsert(tt.d, dialect.UpsertConfig{
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
		name string
		d    dialect.Dialect
		want string
	}{
		{
			name: "postgres",
			d:    dialect.Postgres{},
			want: "INSERT INTO request_nonces (nonce) VALUES ($1) ON CONFLICT (nonce) DO NOTHING",
		},
		{
			name: "mysql",
			d:    dialect.MySQL{},
			want: "INSERT INTO request_nonces (nonce) SELECT $1 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM request_nonces AS existing WHERE existing.nonce = $1)",
		},
		{
			name: "mssql",
			d:    dialect.MSSQL{},
			want: "MERGE request_nonces WITH (HOLDLOCK) AS t USING (VALUES ($1)) AS s(nonce) ON s.nonce = t.nonce WHEN NOT MATCHED THEN INSERT (nonce) VALUES (s.nonce);",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.InsertDoNothing(tt.d, "request_nonces", []string{"nonce"}, []string{"nonce"})
			if got != tt.want {
				t.Errorf("InsertDoNothing() =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}

// ILike

func TestILike(t *testing.T) {
	tests := []struct {
		name        string
		d           dialect.Dialect
		column      string
		placeholder string
		want        string
	}{
		{
			name:        "postgres",
			d:           dialect.Postgres{},
			column:      "operation_name",
			placeholder: "$1",
			want:        "operation_name ILIKE '%' || $1 || '%'",
		},
		{
			name:        "mysql",
			d:           dialect.MySQL{},
			column:      "operation_name",
			placeholder: "$1",
			want:        "operation_name LIKE CONCAT('%', $1, '%')",
		},
		{
			// CHARINDEX rather than LIKE: SQL Server caps a LIKE pattern at
			// 8000 bytes, and wrapping a 4000-character NVARCHAR value in %
			// takes it two over the limit.
			name:        "mssql",
			d:           dialect.MSSQL{},
			column:      "operation_name",
			placeholder: "$1",
			want:        "CHARINDEX($1, operation_name) > 0",
		},
		{
			name:        "postgres with table alias",
			d:           dialect.Postgres{},
			column:      "data::text",
			placeholder: "$2",
			want:        "data::text ILIKE '%' || $2 || '%'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.ILike(tt.d, tt.column, tt.placeholder)
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
		d           dialect.Dialect
		expr        string
		targetType  string
		pgShorthand []bool
		want        string
	}{
		{
			name:       "postgres default shorthand",
			d:          dialect.Postgres{},
			expr:       "u.updated_at",
			targetType: "text",
			want:       "u.updated_at::text",
		},
		{
			name:        "postgres explicit no shorthand",
			d:           dialect.Postgres{},
			expr:        "u.updated_at",
			targetType:  "CHAR",
			pgShorthand: []bool{false},
			want:        "CAST(u.updated_at AS CHAR)",
		},
		{
			name:       "mysql",
			d:          dialect.MySQL{},
			expr:       "u.updated_at",
			targetType: "CHAR",
			want:       "CAST(u.updated_at AS CHAR)",
		},
		{
			name:       "mssql",
			d:          dialect.MSSQL{},
			expr:       "data",
			targetType: "NVARCHAR(MAX)",
			want:       "CAST(data AS NVARCHAR(MAX))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.Cast(tt.d, tt.expr, tt.targetType, tt.pgShorthand...)
			if got != tt.want {
				t.Errorf("Cast() = %q, want %q", got, tt.want)
			}
		})
	}
}

// LimitOffset

func TestLimitOffset(t *testing.T) {
	tests := []struct {
		name   string
		d      dialect.Dialect
		limit  int
		offset int
		want   string
	}{
		{
			name: "postgres", d: dialect.Postgres{}, limit: 20, offset: 40,
			want: "LIMIT 20 OFFSET 40",
		},
		{
			name: "mysql", d: dialect.MySQL{}, limit: 20, offset: 40,
			want: "LIMIT 20 OFFSET 40",
		},
		{
			name: "mssql", d: dialect.MSSQL{}, limit: 20, offset: 40,
			want: "OFFSET 40 ROWS FETCH NEXT 20 ROWS ONLY",
		},
		{
			name: "mssql zero offset", d: dialect.MSSQL{}, limit: 1, offset: 0,
			want: "OFFSET 0 ROWS FETCH NEXT 1 ROW ONLY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.LimitOffset(tt.d, tt.limit, tt.offset)
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
		d         dialect.Dialect
		limitIdx  int
		offsetIdx int
		want      string
	}{
		{
			name: "postgres", d: dialect.Postgres{}, limitIdx: 3, offsetIdx: 4,
			want: "LIMIT $3 OFFSET $4",
		},
		{
			name: "mysql", d: dialect.MySQL{}, limitIdx: 3, offsetIdx: 4,
			want: "LIMIT $3 OFFSET $4",
		},
		{
			name: "mssql", d: dialect.MSSQL{}, limitIdx: 3, offsetIdx: 4,
			want: "OFFSET CAST(CASE WHEN $3 <= 0 THEN 9223372036854775807 ELSE $4 END AS BIGINT) ROWS FETCH NEXT CAST(CASE WHEN $3 <= 0 THEN 1 ELSE $3 END AS BIGINT) ROWS ONLY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialect.LimitOffsetPlaceholders(tt.d, tt.limitIdx, tt.offsetIdx)
			if got != tt.want {
				t.Errorf("LimitOffsetPlaceholders() = %q, want %q", got, tt.want)
			}
		})
	}
}
