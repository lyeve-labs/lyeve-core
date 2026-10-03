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

// A projection is the only way an admin write reaches the table the public
// routes read, so the status it carries has to land in the _status column.
// The column defaults to published, and a status that never arrives makes
// every draft public.
func TestContentStore_UpsertContentCarriesStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			store, sc := statusFixture(t, name, true)
			ctx := context.Background()
			id := uuid.New()

			listed := func() bool {
				t.Helper()
				items, err := store.List(ctx, sc.Name, 50, 0, nil)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				for _, it := range items {
					if it.ID == id {
						return true
					}
				}
				return false
			}
			status := func() any {
				t.Helper()
				got, err := store.GetByID(ctx, sc.Name, id)
				if err != nil {
					t.Fatalf("GetByID: %v", err)
				}
				return got.Data["_status"]
			}

			if err := store.UpsertContent(ctx, sc.Name, id, map[string]any{"title": "t", "_status": "draft"}); err != nil {
				t.Fatalf("project draft: %v", err)
			}
			if got := status(); got != "draft" {
				t.Fatalf("_status after a draft projection = %v, want draft", got)
			}
			if listed() {
				t.Fatal("a draft is listed on the published read")
			}

			if err := store.UpsertContent(ctx, sc.Name, id, map[string]any{"title": "t", "_status": "published"}); err != nil {
				t.Fatalf("project publish: %v", err)
			}
			if !listed() {
				t.Fatal("a published entry is missing from the published read")
			}

			if err := store.UpsertContent(ctx, sc.Name, id, map[string]any{"title": "t", "_status": "draft"}); err != nil {
				t.Fatalf("project unpublish: %v", err)
			}
			if listed() {
				t.Fatal("an unpublished entry is still listed on the published read")
			}
		})
	}
}

// A create on the public route carries _status the same way, and a value the
// read filters never match is refused rather than stored.
func TestContentStore_InsertStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			store, sc := statusFixture(t, name, true)
			ctx := context.Background()

			item, err := store.Insert(ctx, sc.Name, map[string]any{"title": "t", "_status": "draft"})
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if item.Data["_status"] != "draft" {
				t.Errorf("_status = %v, want draft", item.Data["_status"])
			}

			item, err = store.Insert(ctx, sc.Name, map[string]any{"title": "t"})
			if err != nil {
				t.Fatalf("Insert without status: %v", err)
			}
			if item.Data["_status"] != "published" {
				t.Errorf("_status without a value = %v, want the column default published", item.Data["_status"])
			}

			for _, bad := range []any{"live", 1, true} {
				_, err = store.Insert(ctx, sc.Name, map[string]any{"title": "t", "_status": bad})
				if !errors.Is(err, domain.ErrValidation) {
					t.Errorf("Insert with _status %v = %v, want domain.ErrValidation", bad, err)
				}
				_, err = store.Update(ctx, sc.Name, item.ID, map[string]any{"_status": bad}, item.UpdatedAt)
				if !errors.Is(err, domain.ErrValidation) {
					t.Errorf("Update with _status %v = %v, want domain.ErrValidation", bad, err)
				}
			}
		})
	}
}

// A schema without draft and publish has no _status column. A projection
// sends the status whatever the schema, so the key is ignored there.
func TestContentStore_StatusIgnoredWithoutDraftPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			store, sc := statusFixture(t, name, false)
			ctx := context.Background()
			id := uuid.New()
			if err := store.UpsertContent(ctx, sc.Name, id, map[string]any{"title": "t", "_status": "draft"}); err != nil {
				t.Fatalf("UpsertContent: %v", err)
			}
			if err := store.UpsertContent(ctx, sc.Name, id, map[string]any{"title": "u", "_status": "draft"}); err != nil {
				t.Fatalf("UpsertContent update: %v", err)
			}
		})
	}
}

func statusFixture(t *testing.T, dialectName string, draftPublish bool) (*db.ContentStore, *domain.Schema) {
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
		Name:             "status_entries",
		Fields:           []domain.SchemaField{{Name: "title", FieldType: "text"}},
		WithDraftPublish: draftPublish,
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("upsert schema: %v", err)
	}
	t.Cleanup(func() { _ = reg.Delete(ctx, sc.Name) })
	return db.NewContentStore(pool, reg.Source()), sc
}
