//go:build integration && !mutest

// Integration tests for the GDPR subject erasure (right-to-be-forgotten)
// workflow. These tests exercise the full pipeline: real database tables,
// real compliance.SubjectEraser implementations that issue SQL, and the
// RunSubjectErasure coordinator that fans out across registrants.
//
// Each test runs against PostgreSQL, MySQL, and MSSQL via testcontainers.
// Complements the unit tests in erasure_test.go which use mock erasers.
package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test table setup: dialect-portable DDL

// ensureTestTables creates the test schema used by all erasure tests. Each
// table carries a column that the eraser will anonymize (email, name, ip).
//
// Dialect branches handle type/constraint differences. The $N placeholders
// in Exec are DDL-only (no args), so the rewrite layer is a no-op.
func ensureTestTables(t *testing.T, ctx context.Context, host core.Host) {
	t.Helper()
	q := host.Querier(ctx)

	stmts := testTableDDL(host.Dialect())
	for _, stmt := range stmts {
		_, err := q.Exec(ctx, stmt)
		if err != nil {
			t.Fatalf("create test table [%s]: %v", host.Dialect(), err)
		}
	}
}

func testTableDDL(dialect string) []string {
	switch dialect {
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS test_contacts (
				id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				email       TEXT NOT NULL,
				full_name   TEXT NOT NULL DEFAULT '',
				tenant_id   TEXT NOT NULL DEFAULT '',
				created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`,
			`CREATE TABLE IF NOT EXISTS test_sessions (
				id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				ip_address  INET NOT NULL,
				user_agent  TEXT NOT NULL DEFAULT '',
				email       TEXT NOT NULL DEFAULT '',
				tenant_id   TEXT NOT NULL DEFAULT '',
				created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`,
		}
	case "mysql":
		return []string{
			"CREATE TABLE IF NOT EXISTS test_contacts (" +
				"id          CHAR(36) NOT NULL DEFAULT (UUID()) PRIMARY KEY," +
				"email       TEXT NOT NULL," +
				"full_name   TEXT NOT NULL," +
				"tenant_id   VARCHAR(255) NOT NULL DEFAULT ''," +
				"created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP" +
				")",
			"CREATE TABLE IF NOT EXISTS test_sessions (" +
				"id          CHAR(36) NOT NULL DEFAULT (UUID()) PRIMARY KEY," +
				"ip_address  VARCHAR(45) NOT NULL," +
				"user_agent  TEXT NOT NULL," +
				"email       TEXT NOT NULL," +
				"tenant_id   VARCHAR(255) NOT NULL DEFAULT ''," +
				"created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP" +
				")",
		}
	case "mssql":
		return []string{
			"IF OBJECT_ID('test_contacts') IS NULL CREATE TABLE test_contacts (" +
				"id          UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID() PRIMARY KEY," +
				"email       NVARCHAR(320) NOT NULL," +
				"full_name   NVARCHAR(255) NOT NULL DEFAULT N''," +
				"tenant_id   NVARCHAR(255) NOT NULL DEFAULT N''," +
				"created_at  DATETIME2 NOT NULL DEFAULT SYSUTCDATETIME()" +
				")",
			"IF OBJECT_ID('test_sessions') IS NULL CREATE TABLE test_sessions (" +
				"id          UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID() PRIMARY KEY," +
				"ip_address  NVARCHAR(45) NOT NULL," +
				"user_agent  NVARCHAR(MAX) NOT NULL DEFAULT N''," +
				"email       NVARCHAR(320) NOT NULL DEFAULT N''," +
				"tenant_id   NVARCHAR(255) NOT NULL DEFAULT N''," +
				"created_at  DATETIME2 NOT NULL DEFAULT SYSUTCDATETIME()" +
				")",
		}
	default:
		return nil
	}
}

// Test data seeding

// seedTestData populates test_contacts and test_sessions with known rows so
// erasure assertions have deterministic baselines.
func seedTestData(t *testing.T, ctx context.Context, host core.Host, tenant string) {
	t.Helper()
	q := host.Querier(ctx)

	for _, email := range []string{"user@example.com", "user@example.com"} {
		_, err := q.Exec(ctx,
			`INSERT INTO test_contacts (email, full_name, tenant_id) VALUES ($1, $2, $3)`,
			email, "Alice Smith", tenant,
		)
		require.NoError(t, err, "seed test_contacts")
	}

	_, err := q.Exec(ctx,
		`INSERT INTO test_sessions (ip_address, user_agent, email, tenant_id) VALUES ($1, $2, $3, $4)`,
		"192.168.1.1", "Mozilla/5.0", "user@example.com", tenant,
	)
	require.NoError(t, err, "seed test_sessions")

	// 1 contact for a different user (should NOT be erased by our identifier).
	_, err = q.Exec(ctx,
		`INSERT INTO test_contacts (email, full_name, tenant_id) VALUES ($1, $2, $3)`,
		"other@example.com", "Bob Jones", tenant,
	)
	require.NoError(t, err, "seed other user contact")
}

// countActive returns the number of rows in test_contacts where email
// matches the given identifier and has NOT been anonymized.
func countActive(t *testing.T, ctx context.Context, host core.Host, table, identifier string) int {
	t.Helper()
	q := host.Querier(ctx)
	var n int
	row, err := q.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE email = $1`, table),
		identifier,
	)
	require.NoError(t, err)
	err = row.Scan(&n)
	require.NoError(t, err, "countActive %s", table)
	return n
}

// countRows returns the total number of rows in a table.
func countRows(t *testing.T, ctx context.Context, host core.Host, table string) int {
	t.Helper()
	q := host.Querier(ctx)
	var n int
	row, err := q.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table),
	)
	require.NoError(t, err)
	err = row.Scan(&n)
	require.NoError(t, err, "countRows %s", table)
	return n
}

// countAnonymized returns the number of rows in test_contacts that have
// the erased sentinel values: 'erased@localhost' for email, empty for full_name.
// No identifier filter needed: the sentinels are set by the eraser.
func countAnonymized(t *testing.T, ctx context.Context, host core.Host) int {
	t.Helper()
	q := host.Querier(ctx)
	var n int
	row, err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM test_contacts WHERE email = 'erased@localhost' AND full_name = ''`,
	)
	require.NoError(t, err)
	err = row.Scan(&n)
	require.NoError(t, err, "countAnonymized")
	return n
}

// Test eraser implementations (real DB-backed)

// contactEraser anonymizes the email and full_name columns in test_contacts.
type contactEraser struct {
	host core.Host
}

var _ compliance.SubjectEraser = (*contactEraser)(nil)

func (e *contactEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	if identifier == "" {
		return 0, fmt.Errorf("contact eraser: empty identifier")
	}
	q := e.host.Querier(ctx)
	tag, err := q.Exec(ctx,
		`UPDATE test_contacts SET email = 'erased@localhost', full_name = '' WHERE email = $1`,
		identifier,
	)
	if err != nil {
		return 0, fmt.Errorf("contact eraser: %w", err)
	}
	return tag.RowsAffected, nil
}

// sessionEraser anonymizes the ip_address and user_agent columns in test_sessions.
type sessionEraser struct {
	host core.Host
}

var _ compliance.SubjectEraser = (*sessionEraser)(nil)

func (e *sessionEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	if identifier == "" {
		return 0, fmt.Errorf("session eraser: empty identifier")
	}
	q := e.host.Querier(ctx)
	tag, err := q.Exec(ctx,
		`UPDATE test_sessions SET ip_address = '0.0.0.0', user_agent = '', email = 'erased@localhost' WHERE email = $1`,
		identifier,
	)
	if err != nil {
		return 0, fmt.Errorf("session eraser: %w", err)
	}
	return tag.RowsAffected, nil
}

// failingEraser always fails with the configured error.
type failingEraser struct {
	err error
}

var _ compliance.SubjectEraser = (*failingEraser)(nil)

func (e *failingEraser) EraseSubject(_ context.Context, _ string) (int64, error) {
	return 0, e.err
}

// Tests

// TestErasure_FullSuccess verifies the happy path: 2 erasers each anonymize
// their respective rows, the total rows count is correct, and the data in the
// DB is actually anonymized.
func TestErasure_FullSuccess(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		ensureTestTables(t, ctx, host)
		seedTestData(t, ctx, host, "test-tenant")
		t.Logf("seeded 2 contacts + 1 session for user@example.com, 1 contact for other@example.com")

		compliance.RegisterSubjectEraser(&contactEraser{host: host})
		compliance.RegisterSubjectEraser(&sessionEraser{host: host})

		const identifier = "user@example.com"
		total, err := compliance.RunSubjectErasure(ctx, identifier)

		assert.NoError(t, err, "RunSubjectErasure should succeed")
		// 2 contacts + 1 session = 3 rows total.
		assert.Equal(t, int64(3), total, "total rows erased should include contacts (2) + sessions (1)")

		anonymized := countAnonymized(t, ctx, host)
		assert.Equal(t, 2, anonymized, "both contacts for user@example.com should be anonymized")

		otherActive := countActive(t, ctx, host, "test_contacts", "other@example.com")
		assert.Equal(t, 1, otherActive, "other@example.com contact should not be affected")

		var erasedSessions int
		row, err := host.Querier(ctx).QueryRow(ctx,
			`SELECT COUNT(*) FROM test_sessions WHERE email = 'erased@localhost' AND ip_address = '0.0.0.0'`,
		)
		require.NoError(t, err)
		err = row.Scan(&erasedSessions)
		require.NoError(t, err)
		assert.Equal(t, 1, erasedSessions, "the session should be anonymized")
	})
}

// TestErasure_PartialFailure verifies that the coordinator continues
// processing erasers after one fails and returns both the partial total
// and the first error.
func TestErasure_PartialFailure(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		ensureTestTables(t, ctx, host)
		seedTestData(t, ctx, host, "test-tenant")

		// Register: 1st succeeds, 2nd fails, 3rd succeeds.
		compliance.RegisterSubjectEraser(&contactEraser{host: host})
		compliance.RegisterSubjectEraser(&failingEraser{err: errors.New("simulated db failure")})
		compliance.RegisterSubjectEraser(&sessionEraser{host: host})

		const identifier = "user@example.com"
		total, err := compliance.RunSubjectErasure(ctx, identifier)

		require.Error(t, err, "a partial failure should return an error")
		assert.ErrorContains(t, err, "simulated db failure")

		assert.Equal(t, int64(3), total,
			"rows from successful erasers should be counted despite the failing one")

		anonymized := countAnonymized(t, ctx, host)
		assert.Equal(t, 2, anonymized,
			"contacts should be anonymized even though the middle eraser failed")
	})
}

// TestErasure_AllFail verifies that when every eraser fails, the total is 0
// and the first error is returned.
func TestErasure_AllFail(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()

		compliance.RegisterSubjectEraser(&failingEraser{err: errors.New("timeout")})
		compliance.RegisterSubjectEraser(&failingEraser{err: errors.New("permission denied")})

		total, err := compliance.RunSubjectErasure(ctx, "user@example.com")

		assert.Equal(t, int64(0), total, "no rows should be affected when all erasers fail")
		require.Error(t, err)
		assert.ErrorContains(t, err, "timeout", "first error should be returned")
	})
}

// TestErasure_NoErasers verifies that RunSubjectErasure handles an empty
// registry gracefully: returns 0 rows and no error.
func TestErasure_NoErasers(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		total, err := compliance.RunSubjectErasure(ctx, "user@example.com")

		assert.NoError(t, err, "empty registry should not return an error")
		assert.Equal(t, int64(0), total, "empty registry should return 0 rows")
	})
}

// TestErasure_NoMatch verifies that an identifier matching no rows returns
// 0 but no error.
func TestErasure_NoMatch(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		ensureTestTables(t, ctx, host)
		seedTestData(t, ctx, host, "test-tenant")

		compliance.RegisterSubjectEraser(&contactEraser{host: host})
		compliance.RegisterSubjectEraser(&sessionEraser{host: host})

		total, err := compliance.RunSubjectErasure(ctx, "nonexistent@example.com")

		assert.NoError(t, err, "no match should not be an error")
		assert.Equal(t, int64(0), total, "no rows should match nonexistent identifier")

		allContacts := countRows(t, ctx, host, "test_contacts")
		assert.Equal(t, 3, allContacts, "no contacts should be deleted")
	})
}

// TestErasure_ContextCanceled verifies that erasure respects context
// cancellation and returns an appropriate error.
func TestErasure_ContextCanceled(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already canceled

		compliance.RegisterSubjectEraser(&contactEraser{host: host})

		total, err := compliance.RunSubjectErasure(ctx, "user@example.com")

		assert.Equal(t, int64(0), total, "no rows should be affected when context is canceled")
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled, "should return context.Canceled")
	})
}

// TestErasure_NilEraserIgnored verifies that registering a nil eraser is
// silently ignored (tested at the dispatcher level).
func TestErasure_NilEraserIgnored(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		ensureTestTables(t, ctx, host)
		seedTestData(t, ctx, host, "test-tenant")

		// Register: nil, real eraser, nil.
		compliance.RegisterSubjectEraser(nil)
		compliance.RegisterSubjectEraser(&contactEraser{host: host})
		compliance.RegisterSubjectEraser(nil)

		const identifier = "user@example.com"
		total, err := compliance.RunSubjectErasure(ctx, identifier)

		assert.NoError(t, err, "nil erasers should not cause an error")
		assert.Equal(t, int64(2), total, "only the real eraser should contribute rows")

		anonymized := countAnonymized(t, ctx, host)
		assert.Equal(t, 2, anonymized, "contacts should be anonymized despite nil registrations")
	})
}

// TestErasure_MultipleIdentifiers verifies that the erasure is scoped by
// identifier: erasing one user does not affect another user's data.
func TestErasure_MultipleIdentifiers(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectErasers()
		defer compliance.ResetSubjectErasers()

		ctx := context.Background()
		ensureTestTables(t, ctx, host)
		seedTestData(t, ctx, host, "test-tenant")

		q := host.Querier(ctx)
		_, err := q.Exec(ctx,
			`INSERT INTO test_contacts (email, full_name, tenant_id) VALUES ($1, $2, $3)`,
			"alice@example.com", "Alice Wonderland", "test-tenant",
		)
		require.NoError(t, err)

		compliance.RegisterSubjectEraser(&contactEraser{host: host})

		total1, err1 := compliance.RunSubjectErasure(ctx, "user@example.com")
		require.NoError(t, err1)
		assert.Equal(t, int64(2), total1, "should erase 2 contacts for user@example.com")

		aliceActive := countActive(t, ctx, host, "test_contacts", "alice@example.com")
		assert.Equal(t, 1, aliceActive, "alice@example.com should not be affected by erasing user@example.com")

		total2, err2 := compliance.RunSubjectErasure(ctx, "alice@example.com")
		require.NoError(t, err2)
		assert.Equal(t, int64(1), total2, "should erase 1 contact for alice@example.com")
	})
}
