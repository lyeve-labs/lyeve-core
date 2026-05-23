//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// A caller with its own source of truth projects rows into the per-schema
// table, which is what the public routes and GraphQL read, so a projected row
// must be readable there on every dialect.
func TestContentStore_UpsertContentProjectsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			pool, sc := mirrorFixture(t, name)
			ctx := context.Background()
			store := db.NewContentStore(pool, newTestRegistry(t, pool).Source())

			id := uuid.New()
			row := map[string]any{"title": "projected"}
			if err := store.UpsertContent(ctx, sc.Name, id, row); err != nil {
				t.Fatalf("UpsertContent: %v", err)
			}

			got, err := store.GetByID(ctx, sc.Name, id)
			if err != nil {
				t.Fatalf("the projected row is not readable from the schema table: %v", err)
			}
			if got.Data["title"] != "projected" {
				t.Errorf("projected title = %v, want %q", got.Data["title"], "projected")
			}
		})
	}
}

// A caller can name a schema its tenant never defined. There is no generated
// table to project into, and the caller has to be able to tell that apart from
// a write that failed.
func TestContentStore_UpsertContentReportsUndefinedSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	pool, _ := mirrorFixture(t, "postgres")
	store := db.NewContentStore(pool, newTestRegistry(t, pool).Source())

	err := store.UpsertContent(context.Background(), "default", uuid.New(), map[string]any{"title": "x"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpsertContent on an undefined schema = %v, want domain.ErrNotFound", err)
	}
}

func mirrorFixture(t *testing.T, dialectName string) (db.DB, *domain.Schema) {
	t.Helper()
	var pool db.DB
	switch dialectName {
	case "mysql":
		pool = testdb.MySQL(t)
	case "mssql":
		pool = testdb.MSSQL(t)
	default:
		pool = testdb.Postgres(t)
	}
	ctx := context.Background()
	reg := newTestRegistry(t, pool)
	sc := &domain.Schema{
		Name:   "mirror_entries",
		Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}},
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("upsert schema: %v", err)
	}
	return pool, sc
}
