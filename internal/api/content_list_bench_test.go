//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// BenchmarkContentStore_List50 reads a page of 50 rows, uncached, from a
// schema of float numbers and from one whose numbers are an integer and two
// decimals, which the store projects as text to keep them exact. The two
// report what exactness costs per page.
//
// Run with: go test ./internal/api/ -run='^$' -bench=BenchmarkContentStore_List50 -benchmem
func BenchmarkContentStore_List50(b *testing.B) {
	pool := tbDB(b)
	ctx := context.Background()
	eng := schemaengine.New(testhost.New(pool))

	exact := map[string]any{"name": "line_items_exact", "fields": []map[string]any{
		{"name": "title", "field_type": "text"},
		{"name": "quantity", "field_type": "integer"},
		{"name": "price", "field_type": "decimal", "precision": 10, "scale": 2},
		{"name": "weight", "field_type": "decimal", "precision": 12, "scale": 3},
	}}
	float := map[string]any{"name": "line_items_float", "fields": []map[string]any{
		{"name": "title", "field_type": "text"},
		{"name": "quantity", "field_type": "number"},
		{"name": "price", "field_type": "number"},
		{"name": "weight", "field_type": "number"},
	}}
	for _, sc := range []map[string]any{float, exact} {
		def, err := json.Marshal(sc)
		if err != nil {
			b.Fatal(err)
		}
		name := sc["name"].(string)
		if err := eng.Apply(ctx, name, def); err != nil {
			b.Fatalf("apply %s: %v", name, err)
		}
	}
	store := db.NewContentStore(pool, eng.SchemaSource())

	for _, name := range []string{"line_items_float", "line_items_exact"} {
		for i := range 50 {
			if _, err := store.Insert(ctx, name, map[string]any{
				"title":    fmt.Sprintf("item %d", i),
				"quantity": int64(i * 1000),
				"price":    "19.90",
				"weight":   "1.250",
			}); err != nil {
				b.Fatalf("insert into %s: %v", name, err)
			}
		}
	}

	for _, name := range []string{"line_items_float", "line_items_exact"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := store.List(ctx, name, 50, 0, nil)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) != 50 {
					b.Fatalf("listed %d rows, want 50", len(rows))
				}
			}
		})
	}
}
