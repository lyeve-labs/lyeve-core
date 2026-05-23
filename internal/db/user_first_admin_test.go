//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// firstAdminDialects opens each dialect with a pool wide enough that every
// concurrent caller holds its own session. A four-connection pool would queue
// the callers and hide the race.
var firstAdminDialects = []struct {
	name string
	open func(t *testing.T, maxConns int) db.DB
}{
	{"postgres", testdb.PostgresWithMaxConns},
	{"mysql", testdb.MySQLWithMaxConns},
	{"mssql", testdb.MSSQLWithMaxConns},
}

func TestCreateFirstAdmin_ConcurrentCallersYieldOneAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs database containers")
	}
	const callers = 12
	for _, d := range firstAdminDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT excludes %s", d.name)
			}
			pool := d.open(t, callers+2)
			store := db.NewUserStore(pool)
			ctx := context.Background()

			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, callers)
			for i := range callers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, errs[i] = store.CreateFirstAdmin(ctx, fmt.Sprintf("admin%d@example.test", i), "hash", "")
				}(i)
			}
			close(start)
			wg.Wait()

			won, refused := 0, 0
			for i, err := range errs {
				switch {
				case err == nil:
					won++
				case errors.Is(err, db.ErrSetupComplete):
					refused++
				default:
					t.Errorf("caller %d: unexpected error: %v", i, err)
				}
			}
			n, err := store.Count(ctx)
			if err != nil {
				t.Fatalf("count users: %v", err)
			}
			if won != 1 || n != 1 {
				t.Fatalf("created %d accounts (%d callers reported success), want exactly 1", n, won)
			}
			if refused != callers-1 {
				t.Errorf("refused %d callers with ErrSetupComplete, want %d", refused, callers-1)
			}
		})
	}
}

// A claim that has locked the row and inserted its account but not committed
// must make a second caller wait, then refuse it. Without the lock the second
// caller counts zero, because the first account is not yet visible, and
// creates a second super admin.
func TestCreateFirstAdmin_WaitsForAClaimInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("needs database containers")
	}
	for _, d := range firstAdminDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT excludes %s", d.name)
			}
			pool := d.open(t, testdb.DefaultMaxConns)
			store := db.NewUserStore(pool)
			ctx := context.Background()
			engine := pool.Engine()

			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback() //nolint:errcheck // committed below. Rollback only on a failed test
			txExec := func(q string, args ...any) {
				t.Helper()
				q, args = db.RewritePlaceholders(q, engine, args)
				if _, err := tx.ExecContext(ctx, q, args...); err != nil {
					t.Fatalf("in-flight claim: %v", err)
				}
			}
			txExec(`UPDATE sys_setup_lock SET claimed_at = $1 WHERE id = 1`, time.Now().UTC())
			roles := "{super_admin}"
			if engine != "postgres" {
				roles = `["super_admin"]`
			}
			txExec(`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id) VALUES ($1, $2, $3, $4, $5)`,
				"6f1c3e2a-0b7d-4c55-9a8e-2d4f6b1a9c30", "first@example.test", "hash", roles, "")

			done := make(chan error, 1)
			go func() {
				_, err := store.CreateFirstAdmin(ctx, "second@example.test", "hash", "")
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("second claim returned (%v) while the first held the lock", err)
			case <-time.After(500 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit first claim: %v", err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, db.ErrSetupComplete) {
					t.Fatalf("second claim after the first committed: err = %v, want ErrSetupComplete", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("second claim never returned after the first committed")
			}
			n, err := store.Count(ctx)
			if err != nil {
				t.Fatalf("count users: %v", err)
			}
			if n != 1 {
				t.Fatalf("accounts = %d, want 1", n)
			}
		})
	}
}

func TestCreateFirstAdmin_RefusesOnceAnAccountExists(t *testing.T) {
	if testing.Short() {
		t.Skip("needs database containers")
	}
	for _, d := range firstAdminDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT excludes %s", d.name)
			}
			pool := d.open(t, testdb.DefaultMaxConns)
			store := db.NewUserStore(pool)
			ctx := context.Background()

			if _, err := store.Create(ctx, "editor@example.test", "hash", []string{"editor"}, ""); err != nil {
				t.Fatalf("seed account: %v", err)
			}
			_, err := store.CreateFirstAdmin(ctx, "late@example.test", "hash", "")
			if !errors.Is(err, db.ErrSetupComplete) {
				t.Fatalf("CreateFirstAdmin with an existing account: err = %v, want ErrSetupComplete", err)
			}

			u, err := store.GetByEmail(ctx, "editor@example.test")
			if err != nil {
				t.Fatalf("get seeded account: %v", err)
			}
			if len(u.Roles) != 1 || u.Roles[0] != "editor" {
				t.Errorf("seeded roles = %v, want [editor]", u.Roles)
			}
		})
	}
}

func TestCreateFirstAdmin_GrantsSuperAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs database containers")
	}
	for _, d := range firstAdminDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT excludes %s", d.name)
			}
			pool := d.open(t, testdb.DefaultMaxConns)
			store := db.NewUserStore(pool)

			u, err := store.CreateFirstAdmin(context.Background(), "Owner@Example.test", "hash", "default")
			if err != nil {
				t.Fatalf("CreateFirstAdmin: %v", err)
			}
			if u.Email != "owner@example.test" {
				t.Errorf("email = %q, want the normalized address", u.Email)
			}
			if len(u.Roles) != 1 || u.Roles[0] != "super_admin" {
				t.Errorf("roles = %v, want [super_admin]", u.Roles)
			}
			if u.TenantID != "default" {
				t.Errorf("tenant_id = %q, want default", u.TenantID)
			}
		})
	}
}
