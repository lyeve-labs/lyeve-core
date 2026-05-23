package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A trusted issuer's user gets one local account, created on first sight and
// kept to the roles the issuer's policy grants now, and never takes over an
// account that exists already.
func TestEnsureExternalUser(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		id := uuid.New()
		email := "ext-" + id.String()[:8] + "@agency.example"

		u, err := users.EnsureExternalUser(ctx, id, email, "!EXTERNAL_ISSUER!", []string{"editor"}, "agency")
		require.NoError(t, err)
		assert.Equal(t, id, u.ID)
		assert.Equal(t, []string{"editor"}, u.Roles)
		assert.Equal(t, "agency", u.TenantID)

		again, err := users.EnsureExternalUser(ctx, id, email, "!EXTERNAL_ISSUER!", []string{"admin", "viewer"}, "agency")
		require.NoError(t, err)
		assert.Equal(t, id, again.ID, "the same account on every request")
		assert.ElementsMatch(t, []string{"admin", "viewer"}, again.Roles, "roles follow the policy")

		_, err = users.EnsureExternalUser(ctx, id, email, "!EXTERNAL_ISSUER!", []string{"editor"}, "another")
		assert.ErrorIs(t, err, domain.ErrConflict, "the account belongs to one tenant")

		before, err := users.SessionsRevokedAt(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, before, "no session has ended yet")
		require.NoError(t, users.BumpTokenVersion(ctx, id))
		at, err := users.SessionsRevokedAt(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, at, "a version bump records when the sessions ended")
		assert.WithinDuration(t, time.Now(), *at, time.Minute, "stored and read back in UTC on every dialect")

		_, err = users.SetPassword(ctx, id, "$2a$10$anotherplaceholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")
		assert.ErrorIs(t, err, domain.ErrConflict, "an issuer's account never gets a password")
		still, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "!EXTERNAL_ISSUER!", still.PasswordHash)

		_, err = pool.Exec(ctx, `UPDATE sys_users SET password_hash = '' WHERE id = $1`, id)
		require.NoError(t, err)
		_, err = users.EnsureExternalUser(ctx, id, email, "!EXTERNAL_ISSUER!", []string{"editor"}, "agency")
		assert.ErrorIs(t, err, domain.ErrConflict, "an erased account is not revived by the issuer's next token")

		local, err := users.Create(ctx, "local-"+id.String()[:8]+"@agency.example", "$2a$10$placeholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"super_admin"}, "agency")
		require.NoError(t, err)
		_, err = users.EnsureExternalUser(ctx, uuid.New(), local.Email, "!EXTERNAL_ISSUER!", []string{"editor"}, "agency")
		assert.ErrorIs(t, err, domain.ErrConflict, "an issuer's user never takes over a local account by email")
		kept, err := users.GetByID(ctx, local.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{"super_admin"}, kept.Roles)
	})
}

// A first request that loses the insert to a concurrent one still answers
// with the account, under the write lock too. The winner's row is uncommitted
// when the loser looks, so the loser inserts, waits on the key, and fails once
// the winner commits. On Postgres that failure aborts the lock's transaction,
// and only a savepoint lets the read-back run.
func TestEnsureExternalUser_LosingTheInsertRaceUnderTheLockFindsTheRow(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		users := db.NewUserStore(pool)
		id := uuid.New()
		email := "race-" + id.String()[:8] + "@agency.example"
		const marker = "!EXTERNAL_ISSUER!"
		roles := []string{"editor"}

		inserted := make(chan struct{})
		release := make(chan struct{})
		winner := make(chan error, 1)
		go func() {
			winner <- db.SerializeWrite(context.Background(), pool, "test.ensure_external.winner", func(ctx context.Context) error {
				if _, err := users.EnsureExternalUser(ctx, id, email, marker, roles, "agency"); err != nil {
					return err
				}
				close(inserted)
				<-release
				return nil
			})
		}()
		select {
		case <-inserted:
		case err := <-winner:
			t.Fatalf("winner failed before inserting: %v", err)
		}

		loser := make(chan error, 1)
		var got *domain.User
		go func() {
			loser <- db.SerializeWrite(context.Background(), pool, "test.ensure_external.loser", func(ctx context.Context) error {
				u, err := users.EnsureExternalUser(ctx, id, email, marker, roles, "agency")
				got = u
				return err
			})
		}()
		// Long enough for the loser to block on the winner's key. A loser
		// that has not reached the insert yet finds the committed row, which
		// passes too.
		time.Sleep(300 * time.Millisecond)
		close(release)

		require.NoError(t, <-winner)
		require.NoError(t, <-loser, "the loser reads the winner's account back")
		require.NotNil(t, got)
		assert.Equal(t, id, got.ID)
		assert.Equal(t, "agency", got.TenantID)
	})
}
