package enginehost

import (
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

var _ core.AdminSeatGuardProvider = (*engineHost)(nil)

// membershipBox holds the membership reader behind an atomic pointer.
type membershipBox struct{ reader core.MembershipReader }

// AdminSeatGuard implements core.AdminSeatGuardProvider. It is the guard the
// kernel's own user writes ask, over the same accounts and memberships, so a
// plugin that writes roles counts seats exactly as the users API does. The
// ceiling is read from the license on every write. A host with no database
// has no accounts to count and answers nil.
func (h *engineHost) AdminSeatGuard() core.AdminSeatGuard {
	if h.pool == nil {
		return nil
	}
	return db.NewAdminSeatGuard(db.NewUserStore(h.pool),
		func() int { return h.Capabilities().Limit(core.CapAdminSeats) },
		func() core.MembershipReader {
			if b := h.seatMembers.Load(); b != nil {
				return b.reader
			}
			return nil
		})
}

// SetAdminSeatMembers wires the running membership reader into the admin
// seat guard, so an account holding a seat only through a membership counts.
// Nil clears it.
func (h *engineHost) SetAdminSeatMembers(m core.MembershipReader) {
	h.seatMembers.Store(&membershipBox{reader: m})
}
