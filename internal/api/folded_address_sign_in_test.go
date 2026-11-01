//go:build !mutest

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// Wrong passwords for an address that a database collation compares as equal
// to an account's address count nothing against that account, for an
// install-wide account on the instance host and a tenant-local account on its
// tenant's domain. The holder still signs in with the password, and the other
// address does not sign in with it.
func TestPasswordSignIn_DistinctAddressCountsNothingAgainstTheHolder(t *testing.T) {
	pairs := []struct{ name, stored, asked string }{
		{"accent", "josé", "jose"},
		{"sharp s", "straße", "strasse"},
		{"ligature", "ﬁ", "fi"},
	}
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		s := newHostSignIn(t, pool)
		kinds := []struct {
			name   string
			on     host
			create func(address string) uuid.UUID
		}{
			{"install-wide", instanceHost, func(address string) uuid.UUID {
				u, err := s.users.Create(context.Background(), address, s.hash, []string{"editor"}, "acme")
				require.NoError(t, err)
				return u.ID
			}},
			{"tenant-local", globexDomain, func(address string) uuid.UUID {
				u, created, err := s.users.CreateTenantLocal(context.Background(), "globex", address, s.hash, []string{"editor"}, true)
				require.NoError(t, err)
				require.True(t, created)
				return u.ID
			}},
		}
		for _, kind := range kinds {
			for _, pair := range pairs {
				for _, door := range signInDoorsNames {
					at := kind.name + ", " + pair.name + ", " + door
					suffix := "-" + uuid.NewString()[:8] + "@example.com"
					stored, asked := pair.stored+suffix, pair.asked+suffix
					id := kind.create(stored)

					for i := 0; i <= s.handler.MaxFailedAttempts; i++ {
						got := s.send(t, door, kind.on, asked, "not-the-password", "")
						require.Equal(t, http.StatusUnauthorized, got.code, "%s: attempt %d", at, i)
					}
					count, err := lockoutCount(s.handler.lockoutChecker, id)
					require.NoError(t, err)
					assert.Zero(t, count, "%s: failed attempts counted against the holder", at)

					other := s.send(t, door, kind.on, asked, providerTestPassword, "")
					assert.Equal(t, http.StatusUnauthorized, other.code, "%s: the other address signed in", at)
					holder := s.send(t, door, kind.on, stored, providerTestPassword, "")
					assert.Equal(t, http.StatusOK, holder.code, "%s: the holder was refused", at)
				}
			}
		}
	})
}
