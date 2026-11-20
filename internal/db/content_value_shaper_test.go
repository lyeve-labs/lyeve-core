//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// markingShaper is a hand-written shaper whose output a test can tell from
// its input. An html field's string is wrapped in a paragraph, so a value
// shaped twice shows two. A document field's object keeps its document and
// gains the HTML drawn from it, and any HTML the caller sent is dropped. A
// value of "refuse" in any rich text field is a failure naming the field.
type markingShaper struct{}

func (markingShaper) ShapeValues(_ context.Context, sc *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	var failures []core.SchemaValidationError
	for _, f := range sc.Fields {
		v, ok := data[f.Name]
		if !ok || f.FieldType != "rich_text" {
			continue
		}
		if v == "refuse" {
			failures = append(failures, core.SchemaValidationError{Field: f.Name, Code: "rich_text_refused", Rule: "rich_text"})
			continue
		}
		if f.Format == core.RichTextFormatDocument {
			obj, _ := v.(map[string]any)
			out[f.Name] = map[string]any{"document": obj["document"], "html": "<p>drawn</p>"}
			continue
		}
		if s, isStr := v.(string); isStr {
			out[f.Name] = "<p>" + s + "</p>"
		}
	}
	return out, failures, nil
}

// richTextSchema applies a type holding a plain field, an html rich text
// field and a document rich text field, and drops its table afterward.
func richTextSchema(t *testing.T, pool db.DB, name string) (*testRegistry, *domain.Schema) {
	t.Helper()
	reg := newTestRegistry(t, pool)
	sc := &domain.Schema{
		Name: name,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "body", FieldType: "rich_text"},
			{Name: "doc", FieldType: "rich_text", Format: core.RichTextFormatDocument},
		},
	}
	require.NoError(t, reg.Upsert(context.Background(), sc))
	t.Cleanup(func() { _ = reg.Delete(context.Background(), name) })
	return reg, sc
}

var sentDocument = map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph"}}}

// Every entry point of the store passes the values through the shaper once,
// before it writes, and stores what the shaper answered. The read gives a
// document field back as its object and an html field as the text stored.
func TestContentStore_EveryWriteStoresTheShapedValue(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg, sc := richTextSchema(t, pool, "shaped_entries")
		store := db.NewContentStore(pool, reg.Source()).WithValueShaper(markingShaper{})
		sent := func(title string) map[string]any {
			return map[string]any{
				"title": title,
				"body":  title,
				"doc":   map[string]any{"document": sentDocument, "html": "<script>sent</script>"},
			}
		}
		assertShaped := func(t *testing.T, id uuid.UUID, title string) {
			t.Helper()
			got, err := db.NewContentStore(pool, reg.Source()).GetByID(ctx, sc.Name, id)
			require.NoError(t, err)
			assert.Equal(t, title, got.Data["title"])
			assert.Equal(t, "<p>"+title+"</p>", got.Data["body"], "an html field reads as the text stored")
			doc, ok := got.Data["doc"].(map[string]any)
			require.True(t, ok, "a document field reads as an object, got %T", got.Data["doc"])
			assert.Equal(t, "<p>drawn</p>", doc["html"])
			assert.Equal(t, "doc", doc["document"].(map[string]any)["type"])
		}

		t.Run("Insert", func(t *testing.T) {
			c, err := store.Insert(ctx, sc.Name, sent("insert"))
			require.NoError(t, err)
			assert.Equal(t, "<p>insert</p>", c.Data["body"], "the written row comes back shaped")
			assertShaped(t, c.ID, "insert")
		})
		t.Run("InsertWithID", func(t *testing.T) {
			id := uuid.New()
			_, err := store.InsertWithID(ctx, sc.Name, id, sent("with id"))
			require.NoError(t, err)
			assertShaped(t, id, "with id")
		})
		t.Run("UpsertContentChecked inserts", func(t *testing.T) {
			id := uuid.New()
			require.NoError(t, store.UpsertContentChecked(ctx, sc.Name, id, sent("upsert new"), nil, nil))
			assertShaped(t, id, "upsert new")
		})
		t.Run("UpsertContentChecked updates", func(t *testing.T) {
			id := uuid.New()
			require.NoError(t, store.UpsertContent(ctx, sc.Name, id, sent("first")))
			require.NoError(t, store.UpsertContentChecked(ctx, sc.Name, id, sent("upsert old"),
				func(context.Context, map[string]any) error { return nil }, nil))
			assertShaped(t, id, "upsert old")
		})
		t.Run("Update", func(t *testing.T) {
			c, err := store.Insert(ctx, sc.Name, sent("before"))
			require.NoError(t, err)
			_, err = store.Update(ctx, sc.Name, c.ID, sent("update"), c.UpdatedAt)
			require.NoError(t, err)
			assertShaped(t, c.ID, "update")
		})
		t.Run("BulkInsert", func(t *testing.T) {
			got, err := store.BulkInsert(ctx, sc.Name, []map[string]any{sent("bulk one"), sent("bulk two")})
			require.NoError(t, err)
			require.Len(t, got, 2)
			assertShaped(t, got[0].ID, "bulk one")
			assertShaped(t, got[1].ID, "bulk two")
		})
	})
}

// A failure from the shaper refuses the write at every entry point as a
// validation error naming the field, and no row is written or changed.
func TestContentStore_ShaperRefusalStoresNothing(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg, sc := richTextSchema(t, pool, "refused_entries")
		store := db.NewContentStore(pool, reg.Source()).WithValueShaper(markingShaper{})
		kept, err := store.Insert(ctx, sc.Name, map[string]any{"title": "kept", "body": "kept"})
		require.NoError(t, err)
		refused := map[string]any{"title": "refused", "body": "refuse"}

		assertRefused := func(t *testing.T, err error) {
			t.Helper()
			require.ErrorIs(t, err, domain.ErrValidation)
			var shaped *core.ContentValueError
			require.True(t, errors.As(err, &shaped), "%v", err)
			require.Len(t, shaped.Failures, 1)
			assert.Equal(t, "body", shaped.Failures[0].Field)
			assert.Equal(t, "rich_text_refused", shaped.Failures[0].Code)
		}
		_, err = store.Insert(ctx, sc.Name, refused)
		assertRefused(t, err)
		_, err = store.InsertWithID(ctx, sc.Name, uuid.New(), refused)
		assertRefused(t, err)
		assertRefused(t, store.UpsertContent(ctx, sc.Name, uuid.New(), refused))
		assertRefused(t, store.UpsertContent(ctx, sc.Name, kept.ID, refused))
		_, err = store.Update(ctx, sc.Name, kept.ID, refused, kept.UpdatedAt)
		assertRefused(t, err)
		_, err = store.BulkInsert(ctx, sc.Name, []map[string]any{{"title": "fine", "body": "fine"}, refused})
		assertRefused(t, err)

		rows, err := store.List(ctx, sc.Name, 100, 0, nil)
		require.NoError(t, err)
		require.Len(t, rows, 1, "no refused write stored a row")
		assert.Equal(t, "<p>kept</p>", rows[0].Data["body"])
		assert.Equal(t, "kept", rows[0].Data["title"])
	})
}

// A store with no shaper refuses, at every entry point, a write that names a
// document field, as a write with no schema engine is refused, and stores
// nothing. A write that names no document field is stored as it was sent, an
// html value included.
func TestContentStore_DocumentWithNoShaperIsRefused(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg, sc := richTextSchema(t, pool, "unshaped_entries")
		store := db.NewContentStore(pool, reg.Source())
		plain, err := store.Insert(ctx, sc.Name, map[string]any{"title": "plain", "body": "<div>as sent</div>"})
		require.NoError(t, err, "an html value needs no shaper")
		_, err = store.Update(ctx, sc.Name, plain.ID, map[string]any{"title": "plain again"}, plain.UpdatedAt)
		require.NoError(t, err)

		rich := map[string]any{"title": "rich", "doc": map[string]any{"document": sentDocument}}
		assertUnavailable := func(t *testing.T, err error) {
			t.Helper()
			require.ErrorIs(t, err, core.ErrNoContentValueShaper)
			assert.ErrorIs(t, err, core.ErrNoSchemaEngine, "the absence reads as an absent engine")
			assert.NotErrorIs(t, err, domain.ErrValidation)
		}
		_, err = store.Insert(ctx, sc.Name, rich)
		assertUnavailable(t, err)
		_, err = store.InsertWithID(ctx, sc.Name, uuid.New(), rich)
		assertUnavailable(t, err)
		assertUnavailable(t, store.UpsertContent(ctx, sc.Name, uuid.New(), rich))
		_, err = store.Update(ctx, sc.Name, plain.ID, map[string]any{"doc": nil}, time.Time{})
		assertUnavailable(t, err)
		_, err = store.BulkInsert(ctx, sc.Name, []map[string]any{rich})
		assertUnavailable(t, err)

		rows, err := store.List(ctx, sc.Name, 100, 0, nil)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "plain again", rows[0].Data["title"])
		assert.Equal(t, "<div>as sent</div>", rows[0].Data["body"])
		assert.Nil(t, rows[0].Data["doc"])
	})
}

// A document field written before the store shaped anything, or by a writer
// with its own SQL, holds text that is not an object. It reads as that text,
// and an html field holding text that would decode as JSON reads as the text.
func TestContentStore_RichTextReadKeepsTextThatIsNoDocument(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg, sc := richTextSchema(t, pool, "legacy_entries")
		store := db.NewContentStore(pool, reg.Source()).WithValueShaper(passShaper{})
		c, err := store.Insert(ctx, sc.Name, map[string]any{
			"body": `{"document":{"type":"doc"},"html":"<p>a</p>"}`,
			"doc":  "<p>old html</p>",
		})
		require.NoError(t, err)
		got, err := db.NewContentStore(pool, reg.Source()).GetByID(ctx, sc.Name, c.ID)
		require.NoError(t, err)
		assert.Equal(t, `{"document":{"type":"doc"},"html":"<p>a</p>"}`, got.Data["body"])
		assert.Equal(t, "<p>old html</p>", got.Data["doc"])
	})
}

// passShaper stores what it is sent.
type passShaper struct{}

func (passShaper) ShapeValues(_ context.Context, _ *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	return data, nil, nil
}
