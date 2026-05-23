package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// sysUserEraser implements compliance.SubjectEraser for the sys_users table.
// It carries the engine host so it can get a context-scoped Querier and
// dialect string at EraseSubject time.
type sysUserEraser struct {
	host core.Host
}

// newSysUserEraser constructs a sys_users eraser. Callers pass the engine
// host so the eraser can issue db queries at runtime.
func newSysUserEraser(host core.Host) *sysUserEraser {
	return &sysUserEraser{host: host}
}

var _ compliance.SubjectEraser = (*sysUserEraser)(nil)
var _ compliance.AccountEraser = (*sysUserEraser)(nil)

// ErasesAccount puts this eraser at the end of the fan-out.
//
// It replaces the subject's email with a per-row sentinel, and a plugin store
// keys its rows by user id, so a plugin handed an address resolves it through
// sys_users when it is called. Going first would take that address away
// before any of them ran, and an email-keyed erasure would report zero rows
// from every plugin that looks the subject up.
func (e *sysUserEraser) ErasesAccount() {}

// registerSysUserEraser puts the engine-owned account eraser on the global
// DSAR registry. Lives here for the same reason as registerSysUserExporter:
// runtime.go binds the name "compliance" to the internal controls package, so
// registration happens beside the eraser itself.
func registerSysUserEraser(host core.Host) {
	compliance.RegisterSubjectEraser(newSysUserEraser(host))
}

// logSubjectEraserRoster names every plugin the DSAR erasure fan-out will
// reach, once, after every route an eraser registers through has run.
//
// Registration is a call a plugin author has to remember, and nothing fails
// when they do not: the endpoint sums rows across the registry and reports one
// total, so a plugin missing from the fan-out looks exactly like a plugin that
// matched nothing. This is the line an operator can read to see who is
// actually covered.
//
// Lives here for the same reason as the registration above: runtime.go binds
// the name "compliance" to the internal controls package.
func logSubjectEraserRoster(ctx context.Context, logger *slog.Logger) {
	erasers := compliance.SubjectErasers()
	names := make([]string, 0, len(erasers))
	for _, e := range erasers {
		names = append(names, compliance.EraserName(e))
	}
	sort.Strings(names)
	logger.InfoContext(ctx, "subject erasure fan-out registered",
		"erasers", len(names),
		"plugins", strings.Join(names, ","),
	)
}

// EraseSubject anonymizes a sys_users row where identifier matches the
// email or id column, then revokes the subject's active sessions. Replaces
// email with a per-row sentinel, clears password_hash, empties roles, and
// bumps token_version so every previously issued JWT is rejected immediately.
// Skips rows with empty password_hash (idempotency guard). Rows affected:
// 1 for a match, 0 for no match.
func (e *sysUserEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	if strings.TrimSpace(identifier) == "" {
		return 0, nil
	}

	q := e.host.Querier(ctx)
	dialect := e.host.Dialect()

	// Dialect-specific SQL fragments.
	//   utcFn - wall-clock UTC function (for updated_at)
	//   emptyRoles - an empty roles literal in the dialect's native type
	//   idCast - expression to convert id to comparable text
	var utcFn, emptyRoles, idCast string
	switch dialect {
	case "postgres":
		utcFn = "NOW()"
		emptyRoles = "'{}'" // empty TEXT[] literal
		idCast = "id::text" // UUID -> TEXT
	case "mysql":
		utcFn = "NOW(6)"                  // microsecond precision
		emptyRoles = "CAST('[]' AS JSON)" // empty JSON array
		idCast = "id"                     // CHAR(36) already comparable to text
	case "mssql":
		utcFn = "SYSUTCDATETIME()"
		emptyRoles = "'[]'"                 // empty JSON array as NVARCHAR
		idCast = "CAST(id AS NVARCHAR(36))" // UNIQUEIDENTIFIER -> text
	default:
		utcFn = "NOW()"
		emptyRoles = "'{}'"
		idCast = "id::text"
	}

	// anonymized is BOOLEAN on Postgres and TINYINT/BIT on MySQL/MSSQL.
	boolTrue := "1"
	if dialect == "postgres" {
		boolTrue = "true"
	}

	// A constant sentinel collides on uq_sys_users_email the second time a
	// different subject is erased. Fold the row id in so each erased account
	// gets a unique email (36-char id plus fixed affixes, under the 320-char
	// column limit). CONCAT is available on all three dialects.
	//
	// LOWER wraps the result because sys_users.email is stored lower case and
	// this is a write path like any other. SQL Server renders a
	// UNIQUEIDENTIFIER as an upper-case string, so without the fold an erasure
	// on MSSQL would be the one row in the table that breaks the rule.
	erasedEmail := fmt.Sprintf("LOWER(CONCAT('erased-', %s, '@localhost'))", idCast)

	// Resolve the ids before anonymizing: the UPDATE rewrites email, so the
	// matching key is gone afterwards. The ids drive refresh-token revocation
	// only, so resolving is skipped when no revoker is wired.
	revoker, canRevoke := e.host.(core.RefreshTokenRevoker)
	var ids []uuid.UUID
	if canRevoke {
		var err error
		ids, err = e.resolveSubjectIDs(ctx, q, idCast, identifier)
		if err != nil {
			slog.WarnContext(ctx, "sys_users erasure: resolve subject ids", "err", err)
		}
	}

	// Single SQL with dialect-aware id cast. token_version is bumped so the
	// tokenVersionCheck middleware rejects every access token issued before
	// the erasure, without waiting for natural expiry. anonymized is set so a
	// post-erase export (which matches by the still-present id) excludes the
	// subject.
	//
	// The email and id halves take separate parameters. Stored addresses are
	// lower case, so the email half has to compare against the normalized
	// identifier or an erasure request naming Foo@x.com would report zero rows
	// erased on PostgreSQL and answer a subject request with a silent no-op.
	// The id half keeps the identifier as supplied, because folding a cast id
	// is not this function's business.
	qtext := fmt.Sprintf(`UPDATE sys_users
	 SET email = %s,
	     password_hash = '',
	     roles = %s,
	     token_version = token_version + 1,
	     updated_at = %s,
	     sessions_revoked_at = %s,
	     anonymized = %s
	 WHERE (email = $1 OR %s = $2) AND password_hash != ''`, erasedEmail, emptyRoles, utcFn, utcFn, boolTrue, idCast)

	tag, err := q.Exec(ctx, qtext, core.NormalizeEmail(identifier), identifier)
	if err != nil {
		return 0, fmt.Errorf("sys_users erasure: %w", err)
	}
	n := tag.RowsAffected

	// Best-effort: a revoked refresh token cannot mint a fresh access token.
	// Failures here do not undo the erasure, so they are logged, not returned.
	if canRevoke {
		for _, id := range ids {
			if err := revoker.RevokeAllRefreshTokens(ctx, id.String()); err != nil {
				slog.WarnContext(ctx, "sys_users erasure: refresh token revocation failed",
					"subject_ref", compliance.SubjectRef(identifier), "err", err)
			}
		}
	}

	if n > 0 {
		slog.InfoContext(ctx, "sys_users erased", "subject_ref", compliance.SubjectRef(identifier), "rows", n)
	}
	return n, nil
}

// resolveSubjectIDs returns the ids of the sys_users rows matching identifier
// before the row is anonymized. It reads the id through the same text cast the
// UPDATE matches on, then parses it back to a canonical uuid so refresh-token
// revocation keys by the exact string the refresh store recorded at issue time.
func (e *sysUserEraser) resolveSubjectIDs(ctx context.Context, q core.Querier, idCast, identifier string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM sys_users WHERE (email = $1 OR %s = $2) AND password_hash != ''`, idCast, idCast),
		core.NormalizeEmail(identifier), identifier)
	if err != nil {
		return nil, fmt.Errorf("sys_users erasure: resolve: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, fmt.Errorf("sys_users erasure: scan id: %w", err)
		}
		id, err := uuid.Parse(idText)
		if err != nil {
			return nil, fmt.Errorf("sys_users erasure: parse id %q: %w", idText, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sys_users erasure: iterate ids: %w", err)
	}
	return ids, nil
}
