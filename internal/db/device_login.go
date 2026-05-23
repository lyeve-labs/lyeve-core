package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// DeviceLoginStore reads and writes sys_device_logins.
//
// No read or write carries a tenant predicate, because a request has no
// tenant until it is approved: the device that starts it cannot know one, and
// the admin who approves it names the tenant by approving. Each row is found
// by a secret its caller holds, the device code's hash or the user code, or by
// the id a read of one of those returned. The tenant a row is bound to is what
// the tenant purge deletes by.
type DeviceLoginStore struct{ pool DB }

// NewDeviceLoginStore constructs a DeviceLoginStore backed by the given pool.
func NewDeviceLoginStore(pool DB) *DeviceLoginStore { return &DeviceLoginStore{pool: pool} }

const deviceLoginCols = `SELECT id, device_code_hash, user_code, client_name, requester_ip, requester_net, status,
	user_id, tenant_id, token_version, poll_interval, created_at, expires_at, approved_at, last_polled_at
	FROM sys_device_logins`

func (s *DeviceLoginStore) scan(row interface{ Scan(dest ...any) error }) (*domain.DeviceLogin, error) {
	engine := s.pool.Engine()
	var (
		d      domain.DeviceLogin
		user   *uuid.UUID
		tenant sql.NullString
		tv     sql.NullInt64
	)
	if err := row.Scan(scanUUID(engine, &d.ID), &d.DeviceCodeHash, &d.UserCode, &d.ClientName,
		&d.RequesterIP, &d.RequesterNet, &d.Status, scanNullUUID(engine, &user), &tenant, &tv, &d.PollInterval,
		&d.CreatedAt, &d.ExpiresAt, &d.ApprovedAt, &d.LastPolledAt); err != nil {
		return nil, err
	}
	d.UserID = user
	if tenant.Valid {
		t := tenant.String
		d.TenantID = &t
	}
	if tv.Valid {
		v := int(tv.Int64)
		d.TokenVersion = &v
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.ExpiresAt = d.ExpiresAt.UTC()
	if d.ApprovedAt != nil {
		v := d.ApprovedAt.UTC()
		d.ApprovedAt = &v
	}
	if d.LastPolledAt != nil {
		v := d.LastPolledAt.UTC()
		d.LastPolledAt = &v
	}
	return &d, nil
}

func (s *DeviceLoginStore) one(ctx context.Context, what, where string, args ...any) (*domain.DeviceLogin, error) {
	row, err := s.pool.QueryRow(ctx, deviceLoginCols+where, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	d, err := s.scan(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return d, nil
}

// Create inserts a pending request. A taken device code hash or user code
// comes back as the driver's unique violation, which the caller retries with
// fresh codes.
func (s *DeviceLoginStore) Create(ctx context.Context, d *domain.DeviceLogin) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sys_device_logins (id, device_code_hash, user_code, client_name, requester_ip,
			requester_net, status, poll_interval, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		d.ID, d.DeviceCodeHash, d.UserCode, d.ClientName, d.RequesterIP,
		d.RequesterNet, domain.DeviceLoginPending, d.PollInterval, d.CreatedAt.UTC(), d.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("create device login: %w", err)
	}
	return nil
}

// CountPendingFrom returns how many unexpired pending requests the network
// key (DeviceLogin.RequesterNet) has open.
func (s *DeviceLoginStore) CountPendingFrom(ctx context.Context, net string, now time.Time) (int, error) {
	return s.count(ctx, "count pending device logins",
		`SELECT COUNT(*) FROM sys_device_logins WHERE requester_net = $1 AND status = $2 AND expires_at > $3`,
		net, domain.DeviceLoginPending, now.UTC())
}

// CountPending returns how many unexpired pending requests the install holds
// in all.
func (s *DeviceLoginStore) CountPending(ctx context.Context, now time.Time) (int, error) {
	return s.count(ctx, "count pending device logins",
		`SELECT COUNT(*) FROM sys_device_logins WHERE status = $1 AND expires_at > $2`,
		domain.DeviceLoginPending, now.UTC())
}

func (s *DeviceLoginStore) count(ctx context.Context, what, q string, args ...any) (int, error) {
	row, err := s.pool.QueryRow(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return n, nil
}

// GetByUserCode returns the request whose user code is userCode, in whatever
// state, or domain.ErrNotFound. The caller normalizes the code first.
func (s *DeviceLoginStore) GetByUserCode(ctx context.Context, userCode string) (*domain.DeviceLogin, error) {
	return s.one(ctx, "get device login by user code", ` WHERE user_code = $1`, userCode)
}

// GetByDeviceCodeHash returns the request whose device code hashes to hash,
// in whatever state, or domain.ErrNotFound.
func (s *DeviceLoginStore) GetByDeviceCodeHash(ctx context.Context, hash string) (*domain.DeviceLogin, error) {
	return s.one(ctx, "get device login by device code", ` WHERE device_code_hash = $1`, hash)
}

// Approve binds the request to the user, the tenant their session acts in and
// the token version that session carries. A nil tenant is a user with no
// tenant at all. domain.ErrConflict when the request was decided or expired
// since it was read.
func (s *DeviceLoginStore) Approve(ctx context.Context, id, userID uuid.UUID, tenantID *string, tokenVersion int, at time.Time) error {
	var tenant any
	if tenantID != nil {
		tenant = *tenantID
	}
	res, err := s.pool.Exec(ctx,
		`UPDATE sys_device_logins SET status = $1, user_id = $2, tenant_id = $3, token_version = $4, approved_at = $5
		 WHERE id = $6 AND status = $7 AND expires_at > $5`,
		domain.DeviceLoginApproved, userID, tenant, tokenVersion, at.UTC(), id, domain.DeviceLoginPending)
	if err != nil {
		return fmt.Errorf("approve device login: %w", err)
	}
	return affectedOne(res, "approve device login")
}

// Deny refuses the request. domain.ErrConflict when it was decided or expired
// since it was read.
func (s *DeviceLoginStore) Deny(ctx context.Context, id uuid.UUID, at time.Time) error {
	res, err := s.pool.Exec(ctx,
		`UPDATE sys_device_logins SET status = $1 WHERE id = $2 AND status = $3 AND expires_at > $4`,
		domain.DeviceLoginDenied, id, domain.DeviceLoginPending, at.UTC())
	if err != nil {
		return fmt.Errorf("deny device login: %w", err)
	}
	return affectedOne(res, "deny device login")
}

// MarkPolled records a poll at at when the previous one came no later than
// notAfter, and reports whether it did. False means the device polled too
// soon, and nothing was written.
func (s *DeviceLoginStore) MarkPolled(ctx context.Context, id uuid.UUID, at, notAfter time.Time) (bool, error) {
	res, err := s.pool.Exec(ctx,
		`UPDATE sys_device_logins SET last_polled_at = $1
		 WHERE id = $2 AND (last_polled_at IS NULL OR last_polled_at <= $3)`,
		at.UTC(), id, notAfter.UTC())
	if err != nil {
		return false, fmt.Errorf("mark device login polled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark device login polled: %w", err)
	}
	return n == 1, nil
}

// SlowDown records a poll that came too soon: the poll time moves to at and
// the interval grows by step seconds, to at most limit.
func (s *DeviceLoginStore) SlowDown(ctx context.Context, id uuid.UUID, at time.Time, step, limit int) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sys_device_logins SET last_polled_at = $1,
			poll_interval = CASE WHEN poll_interval + $2 > $3 THEN $3 ELSE poll_interval + $2 END
		 WHERE id = $4`,
		at.UTC(), step, limit, id); err != nil {
		return fmt.Errorf("slow down device login: %w", err)
	}
	return nil
}

// MarkUsed moves an approved request to used, which only one caller can do.
// domain.ErrConflict when it was not approved, including when another
// exchange used it first.
func (s *DeviceLoginStore) MarkUsed(ctx context.Context, id uuid.UUID) error {
	res, err := s.pool.Exec(ctx,
		`UPDATE sys_device_logins SET status = $1 WHERE id = $2 AND status = $3`,
		domain.DeviceLoginUsed, id, domain.DeviceLoginApproved)
	if err != nil {
		return fmt.Errorf("use device login: %w", err)
	}
	return affectedOne(res, "use device login")
}

// Prune deletes at most limit requests that expired before before, and
// reports how many went.
func (s *DeviceLoginStore) Prune(ctx context.Context, before time.Time, limit int) (int64, error) {
	var q string
	switch s.pool.Engine() {
	case "mysql":
		q = `DELETE FROM sys_device_logins WHERE expires_at < $1 ORDER BY expires_at LIMIT $2`
	case "mssql":
		q = `DELETE TOP ($2) FROM sys_device_logins WHERE expires_at < $1`
	default:
		q = `DELETE FROM sys_device_logins WHERE id IN (
			SELECT id FROM sys_device_logins WHERE expires_at < $1 ORDER BY expires_at LIMIT $2)`
	}
	res, err := s.pool.Exec(ctx, q, before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune device logins: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune device logins: %w", err)
	}
	return n, nil
}

// affectedOne turns an update that matched no row into domain.ErrConflict.
func affectedOne(res interface{ RowsAffected() (int64, error) }, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, domain.ErrConflict)
	}
	return nil
}
