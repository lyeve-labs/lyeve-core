package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// UserStore handles CRUD for sys_users.
type UserStore struct{ pool DB }

// NewUserStore constructs a UserStore backed by the given DB pool.
func NewUserStore(pool DB) *UserStore { return &UserStore{pool: pool} }

// Count returns the total number of users in the database.
func (s *UserStore) Count(ctx context.Context) (int64, error) {
	var n int64
	row, err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sys_users`)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Create inserts a new user and returns it.
// Uses Exec + GetByID instead of RETURNING for cross-dialect portability.
//
// The address is normalized here rather than in the handlers so that the two
// callers that reach this store, admin user creation and first-boot setup,
// cannot disagree about it, and so a third caller added later inherits the
// rule instead of having to remember it.
func (s *UserStore) Create(ctx context.Context, email, passwordHash string, roles []string, tenantID string) (*domain.User, error) {
	email = core.NormalizeEmail(email)
	id := uuid.New()
	rolesArg, err := encodeStringArray(s.pool.Engine(), roles)
	if err != nil {
		return nil, fmt.Errorf("create user: encode roles: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sys_users (id, email, password_hash, roles, tenant_id)
		VALUES ($1, $2, $3, $4, $5)`,
		id, email, passwordHash, rolesArg, tenantID,
	); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return s.GetByID(ctx, id)
}

// ErrSetupComplete is returned by CreateFirstAdmin when an account already
// exists. It wraps domain.ErrConflict so status mapping reads it as a 409.
var ErrSetupComplete = fmt.Errorf("first admin already exists: %w", domain.ErrConflict)

// CreateFirstAdmin creates the bootstrap super_admin, and only while
// sys_users is empty.
//
// A count followed by an insert lets two callers that count together both
// win. So both statements run in one transaction that first updates the
// single row of sys_setup_lock: the row lock is held to commit, a concurrent
// caller waits on it, and the count it then runs sees the committed account
// and refuses with ErrSetupComplete. A locked known row serializes the same
// way on all three dialects and at every isolation level the engine runs.
func (s *UserStore) CreateFirstAdmin(ctx context.Context, email, passwordHash, tenantID string) (*domain.User, error) {
	email = core.NormalizeEmail(email)
	engine := s.pool.Engine()
	rolesArg, err := encodeStringArray(engine, []string{"super_admin"})
	if err != nil {
		return nil, fmt.Errorf("create first admin: encode roles: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create first admin: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // deferred rollback. The commit result takes precedence

	exec := func(q string, args ...any) (sql.Result, error) {
		q, args = rewritePlaceholders(q, engine, args)
		return tx.ExecContext(ctx, q, args...)
	}

	res, err := exec(`UPDATE sys_setup_lock SET claimed_at = $1 WHERE id = 1`, time.Now().UTC()) //nolint:raw-tx-placeholder // exec rewrites the placeholders for the pool's dialect
	if err != nil {
		return nil, fmt.Errorf("create first admin: lock: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The migration seeds the row. One missing means it was deleted by
		// hand. Recreating it inside this transaction still serializes: a
		// concurrent caller's insert collides on the primary key.
		if _, err := exec(`INSERT INTO sys_setup_lock (id, claimed_at) VALUES (1, $1)`, time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("create first admin: lock: %w", err)
		}
	}

	var count int64
	q, args := rewritePlaceholders(`SELECT COUNT(*) FROM sys_users`, engine, nil)
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&count); err != nil {
		return nil, fmt.Errorf("create first admin: count: %w", err)
	}
	if count > 0 {
		return nil, ErrSetupComplete
	}

	id := uuid.New()
	if _, err := exec(`
		INSERT INTO sys_users (id, email, password_hash, roles, tenant_id)
		VALUES ($1, $2, $3, $4, $5)`,
		id, email, passwordHash, rolesArg, tenantID,
	); err != nil {
		return nil, fmt.Errorf("create first admin: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create first admin: commit: %w", err)
	}
	return s.GetByID(ctx, id)
}

// GetByEmail fetches a user by email. Returns domain.ErrNotFound if missing.
//
// The argument is normalized with the same function Create uses, so the login
// path resolves exactly the row the create path wrote. The predicate stays a
// plain equality: wrapping the column in LOWER() would make the unique index
// on email unusable and turn every login into a table scan.
func (s *UserStore) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	email = core.NormalizeEmail(email)
	var u domain.User
	row, err := s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, roles, tenant_id, token_version, disabled, expires_at, created_at, updated_at
		FROM sys_users WHERE email = $1`, email)
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	if err := row.Scan(scanUUID(s.pool.Engine(), &u.ID), &u.Email, &u.PasswordHash, newRolesScanner(&u.Roles),
		&u.TenantID, &u.TokenVersion, &u.Disabled, &u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return &u, nil
}

// GetByID fetches a user by UUID. Returns domain.ErrNotFound if missing.
func (s *UserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	var u domain.User
	row, err := s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, roles, tenant_id, token_version, disabled, expires_at, created_at, updated_at
		FROM sys_users WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	if err := row.Scan(scanUUID(s.pool.Engine(), &u.ID), &u.Email, &u.PasswordHash, newRolesScanner(&u.Roles),
		&u.TenantID, &u.TokenVersion, &u.Disabled, &u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}

// scanUserList scans sql.Rows into a User slice.
func scanUserList(engine string, rows *sql.Rows) ([]*domain.User, error) {
	defer rows.Close()

	var users []*domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(scanUUID(engine, &u.ID), &u.Email, &u.PasswordHash, newRolesScanner(&u.Roles),
			&u.TenantID, &u.TokenVersion, &u.Disabled, &u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, &u)
	}
	return users, rows.Err()
}

// List returns a page of users ordered by created_at ascending.
// limit caps the result set. Offset controls the starting position.
// Both are mandatory: callers must supply explicit bounds.
func (s *UserStore) List(ctx context.Context, limit, offset int) ([]*domain.User, error) {
	var query string
	if s.pool.Engine() == "mssql" {
		query = `SELECT id, email, password_hash, roles, tenant_id, token_version, disabled, expires_at, created_at, updated_at
		FROM sys_users ORDER BY created_at ASC OFFSET $2 ROWS FETCH NEXT $1 ROWS ONLY`
	} else {
		query = `SELECT id, email, password_hash, roles, tenant_id, token_version, disabled, expires_at, created_at, updated_at
		FROM sys_users ORDER BY created_at ASC LIMIT $1 OFFSET $2`
	}
	rows, err := s.pool.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return scanUserList(s.pool.Engine(), rows)
}

// UpdateRoles changes the roles of a user.
// Uses Exec + GetByID instead of RETURNING for cross-dialect portability.
func (s *UserStore) UpdateRoles(ctx context.Context, id uuid.UUID, roles []string) (*domain.User, error) {
	rolesArg, err := encodeStringArray(s.pool.Engine(), roles)
	if err != nil {
		return nil, fmt.Errorf("update roles: encode roles: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sys_users SET roles = $2, updated_at = $3
		WHERE id = $1`,
		id, rolesArg, time.Now().UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("update roles: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, domain.ErrNotFound
	}
	return s.GetByID(ctx, id)
}

// SetAccountState updates the account lockout fields on a user.
//
// A nil argument leaves that column untouched, so a caller that sends only
// "disabled" cannot silently clear an expiry it never mentioned. expiresAt
// carries a second level of indirection for the same reason: a nil pointer
// means "not supplied", while a pointer to nil means "clear it".
//
// Disabling bumps token_version in the same statement. The auth path rejects
// a disabled user on login, refresh and session validation, but an access
// token already in a client's hands stays valid until it expires, so without
// the bump a lockout would not take effect for up to the token lifetime.
func (s *UserStore) SetAccountState(ctx context.Context, id uuid.UUID, disabled *bool, expiresAt **time.Time) (*domain.User, error) {
	sets := []string{"updated_at = $2"}
	args := []any{id, time.Now().UTC()}

	if disabled != nil {
		args = append(args, *disabled)
		sets = append(sets, fmt.Sprintf("disabled = $%d", len(args)))
		if *disabled {
			sets = append(sets, "token_version = token_version + 1", "sessions_revoked_at = $2")
		}
	}
	if expiresAt != nil {
		args = append(args, *expiresAt)
		sets = append(sets, fmt.Sprintf("expires_at = $%d", len(args)))
	}
	if len(sets) == 1 {
		return s.GetByID(ctx, id)
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE sys_users SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("set account state: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, domain.ErrNotFound
	}
	return s.GetByID(ctx, id)
}

// Delete removes a user by ID.
func (s *UserStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sys_users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// GetTokenVersion returns the current token_version for a user.
// Returns 0 (the default) if the user is not found: callers should
// treat that as an invalid user.
func (s *UserStore) GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error) {
	var v int
	row, err := s.pool.QueryRow(ctx, `SELECT token_version FROM sys_users WHERE id = $1`, id)
	if err != nil {
		return 0, fmt.Errorf("get token version: %w", err)
	}
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return 0, domain.ErrNotFound
		}
		return 0, fmt.Errorf("get token version: %w", err)
	}
	return v, nil
}

// SetPassword replaces a user's password hash and bumps token_version in the
// same statement, so the sessions the old password opened end with it. It is
// the only way an existing user's password changes. There is no self-service
// path.
//
// A trusted issuer's account is refused with ErrConflict: its user signs in
// through the issuer only, and a password set here would give them a login
// that outlives the issuer's say over who they are and what they hold.
func (s *UserStore) SetPassword(ctx context.Context, id uuid.UUID, passwordHash string) (*domain.User, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sys_users SET password_hash = $2, token_version = token_version + 1, updated_at = $3, sessions_revoked_at = $3 WHERE id = $1 AND password_hash <> $4`,
		id, passwordHash, time.Now().UTC(), auth.ExternalIssuerMarker)
	if err != nil {
		return nil, fmt.Errorf("set password: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		u, getErr := s.GetByID(ctx, id)
		if getErr == nil && u.PasswordHash == auth.ExternalIssuerMarker {
			return nil, fmt.Errorf("set password: account belongs to a trusted issuer: %w", domain.ErrConflict)
		}
		return nil, domain.ErrNotFound
	}
	return s.GetByID(ctx, id)
}

// BumpTokenVersion increments token_version for a user, invalidating all
// previously-issued JWT access tokens. Called during logout.
func (s *UserStore) BumpTokenVersion(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sys_users SET token_version = token_version + 1, updated_at = $2, sessions_revoked_at = $2 WHERE id = $1`, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("bump token version: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// EnsureExternalUser returns the local account for a trusted issuer's user,
// creating it on first sight with the given id, and keeps its roles equal to
// what the issuer's policy grants now. The account can never sign in with a
// password: its hash is passwordHash, the caller's no-password marker.
//
// An id that already exists in another tenant is a conflict, as is an email
// another account holds: an issuer's user never takes over a local account.
func (s *UserStore) EnsureExternalUser(ctx context.Context, id uuid.UUID, email, passwordHash string, roles []string, tenantID string) (*domain.User, error) {
	u, err := s.GetByID(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		email = core.NormalizeEmail(email)
		rolesArg, encErr := encodeStringArray(s.pool.Engine(), roles)
		if encErr != nil {
			return nil, fmt.Errorf("ensure external user: encode roles: %w", encErr)
		}
		// The savepoint keeps a serialized write's transaction usable when
		// the insert fails, so the read-back below can still run on Postgres.
		if err := savepoint(ctx, s.pool.Engine(), "ensure_external_user", func() error {
			_, err := s.pool.Exec(ctx, `
				INSERT INTO sys_users (id, email, password_hash, roles, tenant_id)
				VALUES ($1, $2, $3, $4, $5)`,
				id, email, passwordHash, rolesArg, tenantID,
			)
			return err
		}); err != nil {
			// A concurrent first request may have created the same id. Any
			// other failure, a taken email among them, stays an error.
			if again, getErr := s.GetByID(ctx, id); getErr == nil {
				u = again
			} else {
				return nil, fmt.Errorf("ensure external user: %w: %w", domain.ErrConflict, err)
			}
		} else if u, err = s.GetByID(ctx, id); err != nil {
			return nil, fmt.Errorf("ensure external user: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("ensure external user: %w", err)
	}
	if u.TenantID != tenantID {
		return nil, fmt.Errorf("ensure external user: account is in another tenant: %w", domain.ErrConflict)
	}
	// An account that no longer carries the marker is no longer the issuer's:
	// an erasure clears the hash, and the next token must not revive the
	// anonymized row with the issuer's roles.
	if u.PasswordHash != passwordHash {
		return nil, fmt.Errorf("ensure external user: account is no longer the issuer's: %w", domain.ErrConflict)
	}
	if !sameRoles(u.Roles, roles) {
		if u, err = s.UpdateRoles(ctx, id, roles); err != nil {
			return nil, fmt.Errorf("ensure external user: %w", err)
		}
	}
	return u, nil
}

// SessionsRevokedAt returns when the account's sessions last ended: the last
// time token_version moved. Nil when it never has.
func (s *UserStore) SessionsRevokedAt(ctx context.Context, id uuid.UUID) (*time.Time, error) {
	row, err := s.pool.QueryRow(ctx, `SELECT sessions_revoked_at FROM sys_users WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("sessions revoked at: %w", err)
	}
	var at sql.NullTime
	if err := row.Scan(&at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("sessions revoked at: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time.UTC()
	return &t, nil
}

func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, r := range a {
		seen[r]++
	}
	for _, r := range b {
		if seen[r] == 0 {
			return false
		}
		seen[r]--
	}
	return true
}
