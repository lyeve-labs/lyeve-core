package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
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
var _ compliance.SessionEnder = (*sysUserEraser)(nil)
var _ compliance.SubjectAccountResolver = (*sysUserEraser)(nil)

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

// EraseSubject anonymizes every unerased sys_users row that belongs to
// identifier, as resolveSubjectIDs finds them, then revokes their active
// sessions. One address can hold an account in each tenant, so one request
// can erase several. Replaces email with a per-row sentinel, clears
// password_hash, provider_email and provider_seen_at, empties roles, and
// bumps token_version so every previously issued JWT is rejected immediately.
// Skips rows already marked anonymized, so a second erasure is a no-op. It
// returns the number of rows erased.
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
	boolTrue, boolFalse := "1", "0"
	if dialect == "postgres" {
		boolTrue, boolFalse = "true", "false"
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

	// The subject's accounts are confirmed in Go and erased by id. SQL alone
	// cannot decide which accounts hold the address: MySQL's collations ignore
	// accents and read ß as ss, and SQL Server's default collation reads ß
	// as ss, so a request for an address that holds no account would reach the
	// account of its variant and erase another person. The ids also drive
	// refresh-token revocation, and the UPDATE rewrites email, so they are read
	// first.
	ids, err := e.resolveSubjectIDs(ctx, q, idCast, boolFalse, identifier)
	if err != nil {
		return 0, err
	}

	// token_version is bumped so the tokenVersionCheck middleware rejects every
	// access token issued before the erasure, without waiting for natural
	// expiry. anonymized is set so a post-erase export (which matches by the
	// still-present id) excludes the subject.
	//
	// An address also matches provider_email, where a tenant-local account
	// keeps the address its provider asserted, and both provider columns are
	// cleared with the rest of the row.
	//
	// A row already erased is skipped by its anonymized flag. An empty
	// password_hash does not mean erased: an account a SAML provider created
	// has no password, and the account still holds the person's address.
	update := fmt.Sprintf(`UPDATE sys_users
	 SET email = %s,
	     password_hash = '',
	     roles = %s,
	     token_version = token_version + 1,
	     updated_at = %s,
	     sessions_revoked_at = %s,
	     anonymized = %s,
	     provider_email = NULL,
	     provider_seen_at = NULL
	 WHERE anonymized = %s AND id IN `, erasedEmail, emptyRoles, utcFn, utcFn, boolTrue, boolFalse)

	n, err := execByIDs(ctx, q, update, ids)
	if err != nil {
		return n, err
	}

	// The accounts erased can be any tenant's, so the lines below are about
	// the install and go under no tenant. Stamped with the request's tenant,
	// they would tell its admins how many accounts the address holds
	// elsewhere.
	logCtx := logging.WithoutTenant(ctx)
	e.revokeRefreshTokens(ctx, ids, identifier)

	if n > 0 {
		slog.InfoContext(logCtx, "sys_users erased", "subject_ref", compliance.SubjectRef(identifier), "rows", n)
	}
	return n, nil
}

// eraseIDBatch is how many ids one UPDATE names, well under the 2100
// parameters SQL Server accepts.
const eraseIDBatch = 500

// execByIDs runs update, which ends in "id IN ", once per batch of ids and
// returns the rows it changed.
func execByIDs(ctx context.Context, q core.Querier, update string, ids []uuid.UUID) (int64, error) {
	var n int64
	for rest := ids; len(rest) > 0; {
		batch := rest[:min(len(rest), eraseIDBatch)]
		rest = rest[len(batch):]
		marks := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			marks[i] = "$" + strconv.Itoa(i+1)
			args[i] = id
		}
		tag, err := q.Exec(ctx, update+"("+strings.Join(marks, ", ")+")", args...)
		if err != nil {
			return n, fmt.Errorf("sys_users erasure: %w", err)
		}
		n += tag.RowsAffected
	}
	return n, nil
}

// revokeRefreshTokens revokes the refresh tokens of every account in ids.
// Best-effort: a revoked refresh token cannot mint a fresh access token, and
// a failure here undoes nothing, so it is logged, not returned.
func (e *sysUserEraser) revokeRefreshTokens(ctx context.Context, ids []uuid.UUID, identifier string) {
	revoker, ok := e.host.(core.RefreshTokenRevoker)
	if !ok {
		return
	}
	logCtx := logging.WithoutTenant(ctx)
	for _, id := range ids {
		if err := revoker.RevokeAllRefreshTokens(ctx, id.String()); err != nil {
			slog.WarnContext(logCtx, "sys_users erasure: refresh token revocation failed",
				"subject_ref", compliance.SubjectRef(identifier), "err", err)
		}
	}
}

// SubjectAccounts names the accounts the erasure resolves the subject's
// addresses from, the ones EraseSubject anonymizes.
func (e *sysUserEraser) SubjectAccounts(ctx context.Context, identifier string) ([]compliance.SubjectAccount, error) {
	return compliance.SubjectAccounts(ctx, e.host, identifier)
}

// AddressHolders names the accounts of tenant that hold address, so the
// erasure keeps an address two accounts hold away from both.
func (e *sysUserEraser) AddressHolders(ctx context.Context, tenant, address string) ([]compliance.SubjectAccount, error) {
	return compliance.AddressHolders(ctx, e.host, tenant, address)
}

// EndSessions signs the subject out of every unerased account that belongs
// to identifier and locks it, and erases nothing. The account is disabled,
// which every sign-in refuses, and token_version is bumped in the same
// statement so every access token issued before is rejected. The refresh
// tokens are revoked after. The erasure calls it before any eraser runs, so
// the subject cannot sign back in and write new rows behind an eraser that
// has already run. The account keeps its address until EraseSubject runs,
// which is once every other eraser has succeeded, and stays locked while an
// eraser keeps failing.
func (e *sysUserEraser) EndSessions(ctx context.Context, identifier string) error {
	if strings.TrimSpace(identifier) == "" {
		return nil
	}
	q := e.host.Querier(ctx)
	utcFn, idCast, boolTrue, boolFalse := "NOW()", "id::text", "true", "false"
	switch e.host.Dialect() {
	case "mysql":
		utcFn, idCast, boolTrue, boolFalse = "NOW(6)", "id", "1", "0"
	case "mssql":
		utcFn, idCast, boolTrue, boolFalse = "SYSUTCDATETIME()", "CAST(id AS NVARCHAR(36))", "1", "0"
	}
	ids, err := e.resolveSubjectIDs(ctx, q, idCast, boolFalse, identifier)
	if err != nil {
		return err
	}
	update := fmt.Sprintf(`UPDATE sys_users
	 SET disabled = %s,
	     token_version = token_version + 1,
	     sessions_revoked_at = %s,
	     updated_at = %s
	 WHERE anonymized = %s AND id IN `, boolTrue, utcFn, utcFn, boolFalse)
	if _, err := execByIDs(ctx, q, update, ids); err != nil {
		return fmt.Errorf("sys_users sign-out: %w", err)
	}
	e.revokeRefreshTokens(ctx, ids, identifier)
	return nil
}

// resolveSubjectIDs returns the ids of the unerased sys_users rows that belong
// to identifier: an account whose email or provider_email is the address, or
// whose id is identifier. SQL selects the candidates and
// compliance.SameAddress keeps the rows that hold the address. The id is read
// through the text cast the id half compares on, then parsed back to a
// canonical uuid so refresh-token revocation keys by the exact string the
// refresh store recorded at issue time.
//
// The id half keeps the identifier as supplied, because folding a cast id is
// not this function's business. A disabled account is still found: the
// erasure's own sign-out disables it before the account eraser runs.
func (e *sysUserEraser) resolveSubjectIDs(ctx context.Context, q core.Querier, idCast, boolFalse, identifier string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s, email, provider_email, CASE WHEN %s = $4 THEN 1 ELSE 0 END FROM sys_users
		 WHERE (%s OR %s OR %s = $4) AND anonymized = %s`,
		idCast, idCast, compliance.AddressCandidates("email", 1), compliance.AddressCandidates("provider_email", 1), idCast, boolFalse),
		append(compliance.AddressForms(identifier), identifier)...)
	if err != nil {
		return nil, fmt.Errorf("sys_users erasure: resolve: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var (
			idText, email string
			provider      sql.NullString
			byID          int
		)
		if err := rows.Scan(&idText, &email, &provider, &byID); err != nil {
			return nil, fmt.Errorf("sys_users erasure: scan id: %w", err)
		}
		if byID == 0 && !compliance.SameAddress(email, identifier) &&
			(!provider.Valid || !compliance.SameAddress(provider.String, identifier)) {
			continue
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
