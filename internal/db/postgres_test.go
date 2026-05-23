//go:build !mutest

package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestPostgresIntegration starts a real PostgreSQL container via testcontainers,
// connects through the existing db.Connect seam, runs core migrations, and
// performs a smoke test (write + read) to validate the full stack.
func TestPostgresIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Start PostgreSQL container

	pg, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_test"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("cms"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := pg.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	// Connect through the existing seam

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := Connect(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Run core migrations

	n, err := Migrate(dsn, "../../migrations", nil)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Logf("applied %d core migrations", n)

	// Smoke test: write + read via sys_users

	testEmail := fmt.Sprintf("smoke-%d@test.local", time.Now().UnixNano())

	_, err = pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles)
		 VALUES ($1, $2, $3, $4)`,
		"00000000-0000-0000-0000-000000000001",
		testEmail,
		"$2a$10$placeholder-hash-for-smoke-test",
		`{"super_admin"}`,
	)
	if err != nil {
		t.Fatalf("insert smoke row: %v", err)
	}

	row, qrErr := pool.QueryRow(ctx,
		`SELECT email, roles FROM sys_users WHERE id = $1`,
		"00000000-0000-0000-0000-000000000001",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var gotEmail, gotRoles string
	if err := row.Scan(&gotEmail, &gotRoles); err != nil {
		t.Fatalf("scan smoke row: %v", err)
	}
	if gotEmail != testEmail {
		t.Errorf("email = %q, want %q", gotEmail, testEmail)
	}
	if gotRoles != "{super_admin}" {
		t.Errorf("roles = %q, want {super_admin}", gotRoles)
	}

	// Clean up test row
	pool.Exec(ctx, `DELETE FROM sys_users WHERE id = $1`,
		"00000000-0000-0000-0000-000000000001") //nolint:errcheck

	t.Log("postgres integration: PASS")
}
