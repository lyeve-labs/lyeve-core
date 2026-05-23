//go:build !mutest

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestUserStore_CreateWithTenant validates that Create persists tenant_id and
// the returned User includes it.
func TestUserStore_CreateWithTenant(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	store := db.NewUserStore(pool)

	ctx := context.Background()

	// Create with explicit tenant.
	u, err := store.Create(ctx, "tenant-test@ex.com", "hash123", []string{"editor"}, "acme-corp")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.TenantID != "acme-corp" {
		t.Errorf("TenantID = %q, want %q", u.TenantID, "acme-corp")
	}

	// Create with empty tenant (default).
	u2, err := store.Create(ctx, "no-tenant@ex.com", "hash456", []string{"editor"}, "")
	if err != nil {
		t.Fatalf("Create (empty tenant): %v", err)
	}
	if u2.TenantID != "" {
		t.Errorf("TenantID = %q, want empty", u2.TenantID)
	}

	// Clean up.
	_ = store.Delete(ctx, u.ID)
	_ = store.Delete(ctx, u2.ID)
}

func TestUserStore_GetByEmailReturnsTenantID(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	store := db.NewUserStore(pool)
	ctx := context.Background()

	u, err := store.Create(ctx, "getbyemail-tenant@ex.com", "hash", []string{"admin"}, "acme")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = store.Delete(ctx, u.ID) }()

	fetched, err := store.GetByEmail(ctx, "getbyemail-tenant@ex.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if fetched.TenantID != "acme" {
		t.Errorf("TenantID = %q, want %q", fetched.TenantID, "acme")
	}
}

// Every access token embeds the user's token_version, and logout revokes
// outstanding tokens by bumping that column. If the reads that feed token
// signing drop the column, the signed tokens carry version zero, the
// revocation middleware waves them through, and logout invalidates nothing.
func TestUserStore_ReadsCarryTokenVersionAfterBump(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		suffix := uuid.NewString()[:8]
		email := fmt.Sprintf("tv-%s@example.com", suffix)
		tenant := "tv_" + suffix

		created, err := store.Create(ctx, email, "!hash!", []string{"editor"}, tenant)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, created.ID) })

		seeded, err := store.GetTokenVersion(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetTokenVersion: %v", err)
		}
		if seeded < 1 {
			t.Fatalf("seeded token_version = %d, want at least 1", seeded)
		}
		if created.TokenVersion != seeded {
			t.Errorf("Create returned token_version %d, want %d", created.TokenVersion, seeded)
		}

		if err := store.BumpTokenVersion(ctx, created.ID); err != nil {
			t.Fatalf("BumpTokenVersion: %v", err)
		}
		want := seeded + 1

		byEmail, err := store.GetByEmail(ctx, email)
		if err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if byEmail.TokenVersion != want {
			t.Errorf("GetByEmail token_version = %d, want %d", byEmail.TokenVersion, want)
		}

		byID, err := store.GetByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if byID.TokenVersion != want {
			t.Errorf("GetByID token_version = %d, want %d", byID.TokenVersion, want)
		}

		// List has its own paged query per dialect, so reading the single-row
		// getters alone would leave the mssql paging branch unread.
		page, err := store.List(ctx, 100, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, u := range page {
			if u.ID != created.ID {
				continue
			}
			found = true
			if u.TokenVersion != want {
				t.Errorf("List token_version = %d, want %d", u.TokenVersion, want)
			}
		}
		if !found {
			t.Fatalf("List did not return the created user")
		}
	})
}

// TestUserStore_BumpTokenVersion_LeavesOtherAccountsUntouched pins the blast
// radius of a token-version bump to the one account it names.
//
// A bump is how logout, deletion and erasure revoke access tokens. If it ever
// reached further than its own row, one account logging out would sign every
// other caller out of the installation, and the failure would read as a
// flaky session rather than as revocation gone wide.
func TestUserStore_BumpTokenVersion_LeavesOtherAccountsUntouched(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		suffix := uuid.NewString()[:8]

		subject, err := store.Create(ctx, fmt.Sprintf("bump-subject-%s@example.com", suffix),
			"!hash!", []string{"editor"}, "bump_"+suffix)
		if err != nil {
			t.Fatalf("Create subject: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, subject.ID) })

		bystanders := make([]*domain.User, 0, 3)
		for i := 0; i < 3; i++ {
			u, err := store.Create(ctx, fmt.Sprintf("bump-bystander%d-%s@example.com", i, suffix),
				"!hash!", []string{"editor"}, "bump_"+suffix)
			if err != nil {
				t.Fatalf("Create bystander %d: %v", i, err)
			}
			t.Cleanup(func() { _ = store.Delete(ctx, u.ID) })
			bystanders = append(bystanders, u)
		}

		before := make(map[uuid.UUID]int, len(bystanders))
		for _, u := range bystanders {
			v, err := store.GetTokenVersion(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetTokenVersion bystander: %v", err)
			}
			before[u.ID] = v
		}

		if err := store.BumpTokenVersion(ctx, subject.ID); err != nil {
			t.Fatalf("BumpTokenVersion: %v", err)
		}

		got, err := store.GetTokenVersion(ctx, subject.ID)
		if err != nil {
			t.Fatalf("GetTokenVersion subject: %v", err)
		}
		if got != subject.TokenVersion+1 {
			t.Errorf("subject token_version = %d, want %d", got, subject.TokenVersion+1)
		}

		for _, u := range bystanders {
			v, err := store.GetTokenVersion(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetTokenVersion bystander: %v", err)
			}
			if v != before[u.ID] {
				t.Errorf("bystander %s token_version = %d, want %d unchanged", u.ID, v, before[u.ID])
			}
		}
	})
}

// TestUserStore_SetAccountState_DisableRevokesLiveTokens pins the reason the
// disable path bumps token_version.
//
// The auth path rejects a disabled user on login, refresh and session
// validation, but an access token already issued stays valid until it expires.
// Without the bump, an account lockout would not take effect for up to the
// token lifetime, which is the window an operator disables an account to close.
func TestUserStore_SetAccountState_DisableRevokesLiveTokens(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("skipping integration test: CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	store := db.NewUserStore(pool)
	ctx := context.Background()

	u, err := store.Create(ctx, "lockout@ex.com", "hash", []string{"editor"}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = store.Delete(ctx, u.ID) }()

	before, err := store.GetTokenVersion(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}

	disabled := true
	got, err := store.SetAccountState(ctx, u.ID, &disabled, nil)
	if err != nil {
		t.Fatalf("SetAccountState: %v", err)
	}
	if !got.Disabled {
		t.Error("Disabled = false, want true")
	}

	after, err := store.GetTokenVersion(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}
	if after != before+1 {
		t.Errorf("token_version = %d, want %d: tokens issued before the lockout stay valid", after, before+1)
	}
}

// TestUserStore_SetAccountState_OmittedFieldSurvives guards the shape of the
// update. A caller sending only "disabled" must not clear an expiry it never
// mentioned, which is what a struct of plain values would do.
func TestUserStore_SetAccountState_OmittedFieldSurvives(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("skipping integration test: CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	store := db.NewUserStore(pool)
	ctx := context.Background()

	u, err := store.Create(ctx, "expiry-keep@ex.com", "hash", []string{"editor"}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = store.Delete(ctx, u.ID) }()

	want := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	exp := &want
	if _, err := store.SetAccountState(ctx, u.ID, nil, &exp); err != nil {
		t.Fatalf("SetAccountState (set expiry): %v", err)
	}

	disabled := true
	got, err := store.SetAccountState(ctx, u.ID, &disabled, nil)
	if err != nil {
		t.Fatalf("SetAccountState (disable): %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("ExpiresAt was cleared by an update that did not mention it")
	}
	if !got.ExpiresAt.Truncate(time.Second).Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}

	// A pointer to nil is the explicit clear, and must be honored.
	var none *time.Time
	cleared, err := store.SetAccountState(ctx, u.ID, nil, &none)
	if err != nil {
		t.Fatalf("SetAccountState (clear expiry): %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil after an explicit clear", cleared.ExpiresAt)
	}
}
