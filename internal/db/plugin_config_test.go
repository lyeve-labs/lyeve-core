//go:build !short && !mutest

// External test package: internal/testdb imports internal/db, so an in-package
// test cannot reach the harness.
package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

func TestPluginConfig_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	store := db.NewPluginConfigStore(testdb.Postgres(t))
	ctx := context.Background()

	// The editor reads a missing row as "using defaults", so this must be
	// ErrNotFound and not an empty object.
	if _, err := store.Get(ctx, "cron"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get before save = %v, want ErrNotFound", err)
	}

	want := map[string]any{"poll_interval": "30s", "max_workers": float64(4), "enabled": true}
	if err := store.Set(ctx, "cron", want, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := store.Get(ctx, "cron")
	if err != nil {
		t.Fatalf("Get after save: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}

	// Saving again replaces rather than merges: the editor sends the whole
	// document, so a key the operator deleted must not survive.
	if err := store.Set(ctx, "cron", map[string]any{"enabled": false}, nil); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	got, err = store.Get(ctx, "cron")
	if err != nil {
		t.Fatalf("Get after replace: %v", err)
	}
	if _, stale := got["poll_interval"]; stale {
		t.Error("replaced config still carries a removed key")
	}

	if err := store.Delete(ctx, "cron"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "cron"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	// Deleting an absent row is how Reset behaves for a plugin on defaults.
	if err := store.Delete(ctx, "cron"); err != nil {
		t.Errorf("Delete of absent row: %v", err)
	}
}

func TestPluginConfig_TenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	store := db.NewPluginConfigStore(testdb.Postgres(t))
	ctxA := tenant.WithID(context.Background(), "tenant_a")
	ctxB := tenant.WithID(context.Background(), "tenant_b")

	if err := store.Set(ctxA, "cron", map[string]any{"secret_hint": "a-only"}, nil); err != nil {
		t.Fatalf("Set as tenant A: %v", err)
	}

	if _, err := store.Get(ctxB, "cron"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("tenant B read tenant A's plugin config (err = %v)", err)
	}

	// A delete run by another tenant must not clear the owner's row.
	if err := store.Delete(ctxB, "cron"); err != nil {
		t.Fatalf("Delete as tenant B: %v", err)
	}
	if _, err := store.Get(ctxA, "cron"); err != nil {
		t.Fatalf("tenant B's delete removed tenant A's config: %v", err)
	}
}
