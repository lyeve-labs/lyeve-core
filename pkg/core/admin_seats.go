package core

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// CapAdminSeats names the ceiling on admin seats. The licensing implementation
// states the number under this name, and the engine holds none of its own: a
// build whose licensing states no ceiling has unlimited seats.
const CapAdminSeats = "admin.seats"

// An admin seat is an account that holds admin or super_admin, in its own
// roles or in any tenant membership. Those two roles are what the admin route
// groups admit, and the roles an identity provider asserts have them stripped
// before they reach an account. Editors and viewers hold no seat.
//
// An account counts once per install, however many tenant memberships it
// holds, because sys_users holds every tenant's accounts in one table.
//
// A disabled account still holds its seat. Re-enabling it then needs no
// check, and disabling someone is never a way to make room for a new admin
// that the next re-enable would push past the ceiling.

// IsAdminSeatRole reports whether holding role makes an account an admin seat.
func IsAdminSeatRole(role string) bool {
	return role == "admin" || role == "super_admin"
}

// GrantsAdminSeat reports whether any of roles makes an account an admin seat.
func GrantsAdminSeat(roles []string) bool {
	for _, r := range roles {
		if IsAdminSeatRole(r) {
			return true
		}
	}
	return false
}

// AdminSeatCapError is the refusal of a write that would add an admin seat
// past the ceiling. A handler answers it with httpx.CapExceeded under
// CapAdminSeats.
type AdminSeatCapError struct {
	Limit   int
	Current int
}

func (e *AdminSeatCapError) Error() string {
	return fmt.Sprintf("admin seats: %d of %d in use", e.Current, e.Limit)
}

// AdminSeatGuard is asked before any write that would give an account an
// admin seat: creating one, changing its roles, or granting it a membership.
type AdminSeatGuard interface {
	// CheckAdminSeat reports whether account may come to hold roles. account
	// is uuid.Nil for an account not yet created. It returns nil when roles
	// grant no seat, when account already holds one, when no ceiling applies
	// or when the install is under it, and an *AdminSeatCapError otherwise.
	// Any other error means the count could not be read.
	//
	// The answer holds only until another write takes a seat, so a caller
	// that goes on to write roles asks through WriteWithAdminSeat, which
	// holds the count still until the write commits.
	CheckAdminSeat(ctx context.Context, account uuid.UUID, roles []string) error
}

// AdminSeatWriter is implemented by a guard that can hold the seat count
// still while a write runs. WithAdminSeat answers as CheckAdminSeat does and,
// when the account may hold roles, runs write before any other write can take
// a seat, so two writes at the ceiling cannot both pass.
//
// write always runs in one transaction, whether or not it could add a seat:
// every query it makes through the host's querier with the context it is
// handed is part of that transaction, an error from write rolls back all of
// it, and write opens no transaction of its own. Only a write that could add
// a seat also waits for the install's seat lock.
type AdminSeatWriter interface {
	WithAdminSeat(ctx context.Context, account uuid.UUID, roles []string, write func(ctx context.Context) error) error
}

// WriteWithAdminSeat runs write once guard admits account holding roles, and
// tells the guard's answer apart from the write's own failure: seatErr is an
// *AdminSeatCapError or a count that could not be read, and writeErr is what
// write returned. A guard that is an AdminSeatWriter runs write in one
// transaction and holds the count still until it commits. One that is not is
// asked first and write runs after, outside any transaction, and a nil guard
// runs write unchecked. The engine's guard and the test host's are both
// AdminSeatWriters.
func WriteWithAdminSeat(ctx context.Context, guard AdminSeatGuard, account uuid.UUID, roles []string, write func(ctx context.Context) error) (seatErr, writeErr error) {
	if guard == nil {
		return nil, write(ctx)
	}
	sw, ok := guard.(AdminSeatWriter)
	if !ok {
		if err := guard.CheckAdminSeat(ctx, account, roles); err != nil {
			return err, nil
		}
		return nil, write(ctx)
	}
	ran := false
	err := sw.WithAdminSeat(ctx, account, roles, func(ctx context.Context) error {
		ran = true
		writeErr = write(ctx)
		return writeErr
	})
	if writeErr != nil {
		return nil, writeErr
	}
	if err != nil && !ran {
		return err, nil
	}
	// The write succeeded and the commit after it did not, which is the
	// write's failure to persist rather than a refusal.
	return nil, err
}

// AdminSeatGuardProvider is implemented by the engine host. A plugin that
// writes roles to an account reads the guard from it. Nil means no guard is
// wired, which only a host built outside the runtime answers.
type AdminSeatGuardProvider interface {
	AdminSeatGuard() AdminSeatGuard
}

// AdminSeatHolders is implemented by a MembershipReader whose memberships
// carry roles. It lists every account that holds an admin seat through a
// membership, so the guard counts them beside the accounts whose own roles
// grant one.
type AdminSeatHolders interface {
	AdminSeatHolders(ctx context.Context) ([]uuid.UUID, error)
}

// AdminSeatGuard forwards to inner if it implements AdminSeatGuardProvider,
// and answers nil otherwise.
func (h *ScopedHost) AdminSeatGuard() AdminSeatGuard {
	if p, ok := h.inner.(AdminSeatGuardProvider); ok {
		return p.AdminSeatGuard()
	}
	return nil
}
