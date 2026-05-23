// Placeholder handling inside a transaction.
//
// Plugins write $1/$2 everywhere, because that is what the pool rewrites for
// them. A transaction adapter must rewrite them too, or the same statement
// works outside a transaction and fails inside one on every engine but
// Postgres.
package enginehost

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

func txRoundTrip(t *testing.T, pool db.DB) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE TABLE tx_placeholder_probe (id INT, label VARCHAR(64))`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}

	q := &poolQuerier{pool: pool}
	tx, err := q.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The house style, exactly as a plugin would write it.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tx_placeholder_probe (id, label) VALUES ($1, $2)`, 7, "seven",
	); err != nil {
		t.Fatalf("insert inside a transaction: %v", err)
	}

	var label string
	row, err := tx.QueryRow(ctx, `SELECT label FROM tx_placeholder_probe WHERE id = $1`, 7)
	if err != nil {
		t.Fatalf("queryrow inside a transaction: %v", err)
	}
	if err := row.Scan(&label); err != nil {
		t.Fatalf("scan inside a transaction: %v", err)
	}
	if label != "seven" {
		t.Fatalf("label = %q, want %q", label, "seven")
	}

	// Savepoints inherit the engine, so a nested transaction rewrites too.
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("nested begin: %v", err)
	}
	if _, err := nested.Exec(ctx,
		`INSERT INTO tx_placeholder_probe (id, label) VALUES ($1, $2)`, 8, "eight",
	); err != nil {
		t.Fatalf("insert inside a savepoint: %v", err)
	}
	if err := nested.Commit(ctx); err != nil {
		t.Fatalf("nested commit: %v", err)
	}
}

func TestTransaction_RewritesPlaceholders_Postgres(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("skipping postgres (CI_DIALECT restriction)")
	}
	txRoundTrip(t, testdb.Postgres(t))
}

func TestTransaction_RewritesPlaceholders_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("skipping mysql (CI_DIALECT restriction)")
	}
	txRoundTrip(t, testdb.MySQL(t))
}
