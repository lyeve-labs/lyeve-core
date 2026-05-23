package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

func TestSysUserEraser_EmptyIdentifier(t *testing.T) {
	mock := mockhost.New(t)
	eraser := newSysUserEraser(mock.Host())

	n, err := eraser.EraseSubject(context.Background(), "")
	if err != nil {
		t.Fatalf("expected no error for empty identifier, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows for empty identifier, got %d", n)
	}

	// No Exec should have been called.
	if len(mock.Spy().Calls) > 0 {
		t.Errorf("expected no DB calls for empty identifier, got %d", len(mock.Spy().Calls))
	}
}

func TestSysUserEraser_BlankIdentifier(t *testing.T) {
	mock := mockhost.New(t)
	eraser := newSysUserEraser(mock.Host())

	n, err := eraser.EraseSubject(context.Background(), "   ")
	if err != nil {
		t.Fatalf("expected no error for blank identifier, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows for blank identifier, got %d", n)
	}
}

func TestSysUserEraser_ExecCalled(t *testing.T) {
	mock := mockhost.New(t)
	eraser := newSysUserEraser(mock.Host())
	ctx := context.Background()
	identifier := "test@example.com"

	// Construct the expected SQL the eraser would generate for postgres.
	expectedSQL := fmt.Sprintf(`UPDATE sys_users
	 SET email = %s,
	     password_hash = '',
	     roles = %s,
	     token_version = token_version + 1,
	     updated_at = %s,
	     sessions_revoked_at = %s,
	     anonymized = %s
	 WHERE (email = $1 OR %s = $2) AND password_hash != ''`,
		"LOWER(CONCAT('erased-', id::text, '@localhost'))", "'{}'", "NOW()", "NOW()", "true", "id::text")

	// Set up the expectation: return 1 row affected.
	mock.Spy().OnExec(expectedSQL).ReturnTag(core.CommandTag{RowsAffected: 1})

	n, err := eraser.EraseSubject(ctx, identifier)
	if err != nil {
		t.Fatalf("EraseSubject failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row affected, got %d", n)
	}
}

func TestSysUserEraser_TwoSubjectsNoUniqueViolation(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		eraser := newSysUserEraser(host)
		q := host.Querier(ctx)

		// Two subjects with distinct emails. Erasing both must succeed. A
		// constant sentinel would collide on uq_sys_users_email on the second.
		emails := []string{"erase-a@example.com", "erase-b@example.com"}
		for _, email := range emails {
			if _, err := q.Exec(ctx,
				`INSERT INTO sys_users (email, password_hash) VALUES ($1, $2)`,
				email, "hashed"); err != nil {
				t.Fatalf("seed %s: %v", email, err)
			}
		}

		for _, email := range emails {
			n, err := eraser.EraseSubject(ctx, email)
			if err != nil {
				t.Fatalf("erase %s: %v", email, err)
			}
			if n != 1 {
				t.Fatalf("erase %s affected %d rows, want 1", email, n)
			}
		}
	})
}

func TestSysUserEraser_RedactsEmailByEmail(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		eraser := newSysUserEraser(host)
		q := host.Querier(ctx)

		const email = "redact@example.com"
		if _, err := q.Exec(ctx,
			`INSERT INTO sys_users (email, password_hash) VALUES ($1, $2)`,
			email, "hashed"); err != nil {
			t.Fatalf("seed: %v", err)
		}

		n, err := eraser.EraseSubject(ctx, email)
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		if n != 1 {
			t.Fatalf("affected %d rows, want 1", n)
		}

		// The original email must be gone.
		var still int
		row, err := q.QueryRow(ctx, `SELECT COUNT(*) FROM sys_users WHERE email = $1`, email)
		if err != nil {
			t.Fatalf("count original email: %v", err)
		}
		if err := row.Scan(&still); err != nil {
			t.Fatalf("count original email scan: %v", err)
		}
		if still != 0 {
			t.Fatalf("original email %q still present after erasure", email)
		}

		// The surviving row carries a per-row sentinel, not the original.
		var redacted string
		row, err = q.QueryRow(ctx, `SELECT email FROM sys_users WHERE email LIKE $1`, "erased-%@localhost")
		if err != nil {
			t.Fatalf("read redacted email: %v", err)
		}
		if err := row.Scan(&redacted); err != nil {
			t.Fatalf("read redacted email scan: %v", err)
		}
		if !strings.HasPrefix(redacted, "erased-") || !strings.HasSuffix(redacted, "@localhost") {
			t.Fatalf("redacted email = %q, want erased-<id>@localhost", redacted)
		}
	})
}

// TestSysUserEraser_BumpsTokenVersion proves erasure invalidates outstanding
// access tokens by bumping token_version, not just by emptying the account.
func TestSysUserEraser_BumpsTokenVersion(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		eraser := newSysUserEraser(host)
		q := host.Querier(ctx)

		id := uuid.New()
		email := fmt.Sprintf("revoke-%s@example.com", uuid.NewString()[:8])
		if _, err := q.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, token_version) VALUES ($1, $2, $3, 1)`,
			id, email, "hashed"); err != nil {
			t.Fatalf("seed: %v", err)
		}

		n, err := eraser.EraseSubject(ctx, email)
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		if n != 1 {
			t.Fatalf("affected %d rows, want 1", n)
		}

		var tv int
		row, err := q.QueryRow(ctx, `SELECT token_version FROM sys_users WHERE id = $1`, id)
		if err != nil {
			t.Fatalf("read token_version: %v", err)
		}
		if err := row.Scan(&tv); err != nil {
			t.Fatalf("scan token_version: %v", err)
		}
		if tv != 2 {
			t.Fatalf("token_version = %d, want 2 (erasure must bump it)", tv)
		}
	})
}

// revokingHost wraps a mockhost core.Host and records refresh-token
// revocations so the eraser's session-kill path can be asserted without a real
// refresh store.
type revokingHost struct {
	core.Host
	revoked []string
}

func (h *revokingHost) RevokeAllRefreshTokens(_ context.Context, userID string) error {
	h.revoked = append(h.revoked, userID)
	return nil
}

func TestSysUserEraser_RevokesRefreshTokens(t *testing.T) {
	mock := mockhost.New(t)
	wrapped := &revokingHost{Host: mock.Host()}
	eraser := newSysUserEraser(wrapped)

	ctx := context.Background()
	subjectID := uuid.New()

	// The eraser resolves the id through the postgres text cast before the
	// UPDATE, then revokes that id.
	resolveSQL := `SELECT id::text FROM sys_users WHERE (email = $1 OR id::text = $2) AND password_hash != ''`
	mock.Spy().OnQuery(resolveSQL).ReturnRows(mockhost.Rows("id").Add(subjectID.String()))

	updateSQL := fmt.Sprintf(`UPDATE sys_users
	 SET email = %s,
	     password_hash = '',
	     roles = %s,
	     token_version = token_version + 1,
	     updated_at = %s,
	     sessions_revoked_at = %s,
	     anonymized = %s
	 WHERE (email = $1 OR %s = $2) AND password_hash != ''`,
		"LOWER(CONCAT('erased-', id::text, '@localhost'))", "'{}'", "NOW()", "NOW()", "true", "id::text")
	mock.Spy().OnExec(updateSQL).ReturnTag(core.CommandTag{RowsAffected: 1})

	n, err := eraser.EraseSubject(ctx, subjectID.String())
	if err != nil {
		t.Fatalf("EraseSubject: %v", err)
	}
	if n != 1 {
		t.Fatalf("affected %d rows, want 1", n)
	}
	if len(wrapped.revoked) != 1 || wrapped.revoked[0] != subjectID.String() {
		t.Fatalf("revoked = %v, want [%s]", wrapped.revoked, subjectID.String())
	}
}

// Erasure exists to remove the subject's data, and a log line is stored too:
// the logging plugin keeps them in the database. Naming the subject in the
// line the erasure writes would put the identifier back the moment it was
// taken out, so the line carries the same digest the fan-out logs.
func TestSysUserEraser_LogsADigestNotTheIdentifier(t *testing.T) {
	host := plugintest.Postgres(t)
	ctx := context.Background()
	const email = "logged@example.com"
	if _, err := host.Querier(ctx).Exec(ctx,
		`INSERT INTO sys_users (email, password_hash) VALUES ($1, $2)`, email, "hashed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := newSysUserEraser(host).EraseSubject(ctx, email); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if strings.Contains(buf.String(), email) {
		t.Fatalf("the erasure logged the identifier it erased: %s", buf.String())
	}
	if !strings.Contains(buf.String(), compliance.SubjectRef(email)) {
		t.Fatalf("the erasure line carries no subject reference to correlate it: %s", buf.String())
	}
}

// failingRevokerHost refuses every refresh-token revocation, which is the one
// branch of the eraser that logs per account.
type failingRevokerHost struct {
	core.Host
}

func (failingRevokerHost) RevokeAllRefreshTokens(context.Context, string) error {
	return fmt.Errorf("refresh store unavailable")
}

// A failed revocation is logged after the account is erased, so the line must
// not name the account either. The id is an identifier an erasure can be asked
// for, so it is the subject's data as much as the address is.
func TestSysUserEraser_RevocationFailureLogsADigestNotTheAccount(t *testing.T) {
	host := plugintest.Postgres(t)
	ctx := context.Background()
	const email = "revoked@example.com"
	row, err := host.Querier(ctx).QueryRow(ctx,
		`INSERT INTO sys_users (email, password_hash) VALUES ($1, $2) RETURNING id::text`,
		email, "hashed")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var id string
	if err := row.Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := newSysUserEraser(failingRevokerHost{Host: host}).EraseSubject(ctx, email); err != nil {
		t.Fatalf("erase: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "refresh token revocation failed") {
		t.Fatalf("the failed revocation was not logged: %s", out)
	}
	if strings.Contains(out, email) || strings.Contains(out, id) {
		t.Fatalf("the revocation failure named the erased account: %s", out)
	}
	if !strings.Contains(out, compliance.SubjectRef(email)) {
		t.Fatalf("the revocation failure carries no subject reference: %s", out)
	}
}
