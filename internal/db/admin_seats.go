package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// AdminSeatHolders returns every account whose own roles grant an admin seat,
// across every tenant, because seats are counted per install.
//
// The WHERE clause only narrows the scan. On MySQL and MSSQL it is a LIKE over
// the JSON text, where the underscore in super_admin matches any character,
// so each row's decoded roles decide.
func (s *UserStore) AdminSeatHolders(ctx context.Context) ([]uuid.UUID, error) {
	engine := s.pool.Engine()
	query := `SELECT id, roles FROM sys_users WHERE roles LIKE '%"admin"%' OR roles LIKE '%"super_admin"%'`
	if engine == "postgres" || engine == "postgresql" || engine == "" {
		query = `SELECT id, roles FROM sys_users WHERE 'admin' = ANY(roles) OR 'super_admin' = ANY(roles)`
	}
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list admin seat holders: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var roles []string
		if err := rows.Scan(scanUUID(engine, &id), newRolesScanner(&roles)); err != nil {
			return nil, fmt.Errorf("scan admin seat holder: %w", err)
		}
		if core.GrantsAdminSeat(roles) {
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list admin seat holders: %w", err)
	}
	return out, nil
}

// ownRoles is the roles column of one account, or nil when it does not exist.
func (s *UserStore) ownRoles(ctx context.Context, id uuid.UUID) ([]string, error) {
	row, err := s.pool.QueryRow(ctx, `SELECT roles FROM sys_users WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("read account roles: %w", err)
	}
	var roles []string
	if err := row.Scan(newRolesScanner(&roles)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read account roles: %w", err)
	}
	return roles, nil
}

// AdminSeatGuard is the kernel's core.AdminSeatGuard. It enforces an optional
// ceiling on admin accounts that a licensing implementation may state. It
// counts the accounts whose own roles grant a seat and, when the running
// membership reader can say so, the accounts that hold one only through a
// membership.
type AdminSeatGuard struct {
	users   *UserStore
	limit   func() int
	members func() core.MembershipReader
}

var (
	_ core.AdminSeatGuard  = (*AdminSeatGuard)(nil)
	_ core.AdminSeatWriter = (*AdminSeatGuard)(nil)
)

// adminSeatLock is the write lock every write that may add a seat holds. One
// name for the install, because seats are counted per install.
const adminSeatLock = "core.admin_seats"

// NewAdminSeatGuard builds the guard. limit answers the ceiling at the time of
// the write, where 0 or less is unlimited, so a changed ceiling applies to the
// next write without a restart. members may be nil, as may what it returns.
func NewAdminSeatGuard(users *UserStore, limit func() int, members func() core.MembershipReader) *AdminSeatGuard {
	return &AdminSeatGuard{users: users, limit: limit, members: members}
}

// CheckAdminSeat implements core.AdminSeatGuard.
//
// Only the next seat is refused. An install that holds more than the ceiling,
// because the ceiling came after its accounts or went down, keeps every
// account it has, and an account that already holds a seat may have its roles
// rewritten freely.
func (g *AdminSeatGuard) CheckAdminSeat(ctx context.Context, account uuid.UUID, roles []string) error {
	limit, count, err := g.needsCount(ctx, account, roles)
	if err != nil || !count {
		return err
	}
	return g.withinLimit(ctx, account, limit)
}

// WithAdminSeat implements core.AdminSeatWriter. Every write runs in one
// transaction, so a write of several statements lands whole or not at all
// whichever case it is in. A write that cannot add a seat takes no lock, so
// an issuer's admin seen on every request never queues behind a seat count.
// One that can runs under the install's seat lock, where the count is read
// again and the write commits before the next write counts.
func (g *AdminSeatGuard) WithAdminSeat(ctx context.Context, account uuid.UUID, roles []string, write func(context.Context) error) error {
	_, count, err := g.needsCount(ctx, account, roles)
	if err != nil {
		return err
	}
	if !count {
		return WriteInTransaction(ctx, g.users.pool, write)
	}
	return SerializeWrite(ctx, g.users.pool, adminSeatLock, func(ctx context.Context) error {
		// Read again under the lock: the license, the account and the
		// count may all have moved while this write waited.
		if err := g.CheckAdminSeat(ctx, account, roles); err != nil {
			return err
		}
		return write(ctx)
	})
}

// needsCount reports whether a write giving account roles has to count the
// install, and the ceiling to count against. It does not when roles grant no
// seat, when no ceiling applies, or when the account already holds a seat of
// its own.
func (g *AdminSeatGuard) needsCount(ctx context.Context, account uuid.UUID, roles []string) (limit int, count bool, err error) {
	if !core.GrantsAdminSeat(roles) {
		return 0, false, nil
	}
	limit = g.limit()
	if limit <= 0 {
		return 0, false, nil
	}
	if account != uuid.Nil {
		// Most writes for an existing account are for one that already
		// holds its seat, such as an issuer's admin on every request, and
		// one row answers that without counting the install.
		own, err := g.users.ownRoles(ctx, account)
		if err != nil {
			return 0, false, err
		}
		if core.GrantsAdminSeat(own) {
			return 0, false, nil
		}
	}
	return limit, true, nil
}

// withinLimit counts the install and answers whether account may take a seat
// under limit.
func (g *AdminSeatGuard) withinLimit(ctx context.Context, account uuid.UUID, limit int) error {
	held, err := g.holders(ctx)
	if err != nil {
		return err
	}
	if account != uuid.Nil && held[account] {
		return nil
	}
	if len(held) < limit {
		return nil
	}
	return &core.AdminSeatCapError{Limit: limit, Current: len(held)}
}

// holders is the set of accounts holding a seat. A set rather than a sum,
// because an admin at home who is also an admin through a membership is one
// seat.
func (g *AdminSeatGuard) holders(ctx context.Context) (map[uuid.UUID]bool, error) {
	own, err := g.users.AdminSeatHolders(ctx)
	if err != nil {
		return nil, err
	}
	held := make(map[uuid.UUID]bool, len(own))
	for _, id := range own {
		held[id] = true
	}
	if g.members == nil {
		return held, nil
	}
	src, ok := g.members().(core.AdminSeatHolders)
	if !ok {
		return held, nil
	}
	viaMembership, err := src.AdminSeatHolders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list membership admin seat holders: %w", err)
	}
	for _, id := range viaMembership {
		held[id] = true
	}
	return held, nil
}
