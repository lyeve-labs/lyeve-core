package db_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// seatMembers is a membership reader that also lists the accounts holding a
// seat through a membership, standing in for the tenancy plugin's store.
type seatMembers struct {
	holders []uuid.UUID
	err     error
}

func (m seatMembers) Get(context.Context, uuid.UUID, string) (*core.Membership, error) {
	return nil, core.ErrNotFound
}

func (m seatMembers) ListForUser(context.Context, uuid.UUID) ([]*core.Membership, error) {
	return nil, nil
}

func (m seatMembers) AdminSeatHolders(context.Context) ([]uuid.UUID, error) {
	return m.holders, m.err
}

func createUsers(t *testing.T, store *db.UserStore, prefix string, n int, roles ...string) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := range n {
		u, err := store.Create(context.Background(), fmt.Sprintf("%s%d@seats.test", prefix, i), "x", roles, fmt.Sprintf("tenant_%d", i%2))
		require.NoError(t, err)
		ids = append(ids, u.ID)
	}
	return ids
}

// Only admin and super_admin hold a seat. A role whose JSON text differs from
// super_admin by the one character a LIKE underscore matches must not count
// on the dialects that narrow the scan with LIKE.
func TestUserStore_AdminSeatHolders_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		store := db.NewUserStore(pool)
		admins := createUsers(t, store, "admin", 2, "admin")
		supers := createUsers(t, store, "super", 1, "editor", "super_admin")
		createUsers(t, store, "editor", 2, "editor")
		createUsers(t, store, "viewer", 1, "viewer")
		createUsers(t, store, "lookalike", 1, "super-admin")
		createUsers(t, store, "none", 1)

		got, err := store.AdminSeatHolders(context.Background())
		require.NoError(t, err)
		assert.ElementsMatch(t, append(admins, supers...), got)
	})
}

func TestAdminSeatGuard_CheckAdminSeat_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		admins := createUsers(t, store, "admin", 4, "admin")
		editors := createUsers(t, store, "editor", 1, "editor")
		guard := func(limit int, members core.MembershipReader) *db.AdminSeatGuard {
			return db.NewAdminSeatGuard(store, func() int { return limit }, func() core.MembershipReader { return members })
		}
		refusal := func(t *testing.T, err error) *core.AdminSeatCapError {
			t.Helper()
			var capErr *core.AdminSeatCapError
			require.ErrorAs(t, err, &capErr)
			return capErr
		}

		t.Run("the next seat past the ceiling is refused", func(t *testing.T) {
			capErr := refusal(t, guard(4, nil).CheckAdminSeat(ctx, uuid.Nil, []string{"admin"}))
			assert.Equal(t, 4, capErr.Limit)
			assert.Equal(t, 4, capErr.Current)
			refusal(t, guard(4, nil).CheckAdminSeat(ctx, editors[0], []string{"super_admin"}))
		})

		t.Run("roles that grant no seat are never refused", func(t *testing.T) {
			require.NoError(t, guard(1, nil).CheckAdminSeat(ctx, uuid.Nil, []string{"editor", "viewer"}))
			require.NoError(t, guard(1, nil).CheckAdminSeat(ctx, admins[0], []string{"viewer"}))
		})

		t.Run("an account holding a seat may keep or change it", func(t *testing.T) {
			require.NoError(t, guard(1, nil).CheckAdminSeat(ctx, admins[2], []string{"super_admin"}))
		})

		t.Run("no ceiling refuses nothing", func(t *testing.T) {
			require.NoError(t, guard(0, nil).CheckAdminSeat(ctx, uuid.Nil, []string{"admin"}))
		})

		t.Run("under the ceiling admits the next seat", func(t *testing.T) {
			require.NoError(t, guard(5, nil).CheckAdminSeat(ctx, uuid.Nil, []string{"admin"}))
		})

		t.Run("a membership seat counts once beside the account's own", func(t *testing.T) {
			members := seatMembers{holders: []uuid.UUID{admins[0], editors[0]}}
			capErr := refusal(t, guard(5, members).CheckAdminSeat(ctx, uuid.Nil, []string{"admin"}))
			assert.Equal(t, 5, capErr.Current, "four own seats and one more through a membership")
			require.NoError(t, guard(5, members).CheckAdminSeat(ctx, editors[0], []string{"admin"}),
				"an editor at home who is an admin through a membership already holds the seat")
		})

		t.Run("a count that cannot be read is an error, not a refusal", func(t *testing.T) {
			err := guard(9, seatMembers{err: errors.New("down")}).CheckAdminSeat(ctx, uuid.Nil, []string{"admin"})
			require.Error(t, err)
			var capErr *core.AdminSeatCapError
			assert.False(t, errors.As(err, &capErr))
		})
	})
}

// Seven super admins create an admin each at once, on a pool of four, with one
// admin already seated and a ceiling of four. Exactly three creates pass,
// because each one in counts only after the one before it committed.
func TestAdminSeatGuard_WithAdminSeat_ConcurrentCreatesStopAtCeiling_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		store := db.NewUserStore(pool)
		createUsers(t, store, "seated", 1, "admin")
		const limit, writers = 4, 7
		guard := db.NewAdminSeatGuard(store, func() int { return limit }, nil)

		var (
			start          = make(chan struct{})
			wg             sync.WaitGroup
			admitted, full atomic.Int32
			unexpected     = make(chan error, writers)
		)
		for i := range writers {
			wg.Go(func() {
				<-start
				err := guard.WithAdminSeat(context.Background(), uuid.Nil, []string{"admin"}, func(ctx context.Context) error {
					// Widens the window between the count and the insert so
					// an unserialized guard lets every writer through.
					time.Sleep(25 * time.Millisecond)
					_, err := store.Create(ctx, fmt.Sprintf("racer%d@seats.test", i), "x", []string{"admin"}, "tenant_0")
					return err
				})
				var capErr *core.AdminSeatCapError
				switch {
				case err == nil:
					admitted.Add(1)
				case errors.As(err, &capErr):
					full.Add(1)
				default:
					unexpected <- err
				}
			})
		}
		close(start)
		wg.Wait()
		close(unexpected)
		for err := range unexpected {
			t.Errorf("unexpected error: %v", err)
		}
		assert.EqualValues(t, limit-1, admitted.Load())
		assert.EqualValues(t, writers-(limit-1), full.Load())
		held, err := store.AdminSeatHolders(context.Background())
		require.NoError(t, err)
		assert.Len(t, held, limit)
	})
}

// A write that cannot add a seat never waits on the seat lock: an account
// that already holds one, or roles that grant none, finish while another
// writer holds it.
func TestAdminSeatGuard_WithAdminSeat_SkipsLockWhenNoSeatIsAdded_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		admins := createUsers(t, store, "admin", 2, "admin")
		guard := db.NewAdminSeatGuard(store, func() int { return 2 }, nil)

		held, release := make(chan struct{}), make(chan struct{})
		holder := make(chan error, 1)
		go func() {
			holder <- db.SerializeWrite(ctx, pool, db.AdminSeatLock, func(context.Context) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held

		// Shorter than the lock's own wait, so a write that queued behind
		// the holder fails here rather than being granted the lock late.
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		noop := func(context.Context) error { return nil }
		require.NoError(t, guard.WithAdminSeat(wctx, admins[0], []string{"super_admin"}, noop))
		require.NoError(t, guard.WithAdminSeat(wctx, uuid.Nil, []string{"editor"}, noop))

		close(release)
		require.NoError(t, <-holder)

		var capErr *core.AdminSeatCapError
		require.ErrorAs(t, guard.WithAdminSeat(ctx, uuid.Nil, []string{"admin"}, noop), &capErr)
		assert.Equal(t, 2, capErr.Current)
	})
}

// A write that adds no seat still runs in one transaction. Its first insert
// lands, its second fails on the unique email, and the first is gone after,
// as a caller writing an account and then a link to it relies on.
func TestAdminSeatGuard_WithAdminSeat_WriteWithoutASeatRollsBackWhole_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		admins := createUsers(t, store, "admin", 1, "admin")
		guard := db.NewAdminSeatGuard(store, func() int { return 1 }, nil)

		cases := []struct {
			name    string
			account uuid.UUID
			roles   []string
		}{
			{"roles that grant no seat", uuid.Nil, []string{"editor"}},
			{"an account that already holds its seat", admins[0], []string{"admin"}},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				email := fmt.Sprintf("halfway%d@seats.test", i)
				err := guard.WithAdminSeat(ctx, tc.account, tc.roles, func(ctx context.Context) error {
					if _, err := pool.Begin(ctx); !errors.Is(err, db.ErrNestedWriteLock) {
						return errors.New("the write is outside a transaction")
					}
					if _, err := store.Create(ctx, email, "x", []string{"editor"}, "tenant_0"); err != nil {
						return fmt.Errorf("first insert: %w", err)
					}
					_, err := store.Create(ctx, "admin0@seats.test", "x", []string{"editor"}, "tenant_0")
					return err
				})
				require.Error(t, err, "the second insert reuses a taken email")
				assert.NotContains(t, err.Error(), "first insert")
				assert.NotContains(t, err.Error(), "outside a transaction")

				_, err = store.GetByEmail(ctx, email)
				require.ErrorIs(t, err, domain.ErrNotFound, "the first insert must roll back with the failed write")
			})
		}
	})
}
