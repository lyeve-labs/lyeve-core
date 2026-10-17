package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// seedBatch is one INSERT of the rows numbered first to last, inclusive.
type seedBatch struct{ first, last int }

// seedBatches splits the rows numbered 1 to rows into batches of at most size
// rows. The last batch is short when size does not divide rows, so a count
// under one batch is one short batch rather than none.
//
// The large-migration tests that use it carry the integration tag, which CI
// never sets. This file has no tag, so the batching is checked on every run.
func seedBatches(rows, size int) []seedBatch {
	var batches []seedBatch
	for first := 1; first <= rows; first += size {
		batches = append(batches, seedBatch{first: first, last: min(first+size-1, rows)})
	}
	return batches
}

func TestSeedBatches_CoverEveryRowAskedFor(t *testing.T) {
	tests := []struct {
		name string
		rows int
		size int
		want []seedBatch
	}{
		{name: "fewer rows than one batch", rows: 2_000, size: 10_000, want: []seedBatch{{1, 2_000}}},
		{name: "exactly one batch", rows: 10_000, size: 10_000, want: []seedBatch{{1, 10_000}}},
		{name: "a short last batch", rows: 12_345, size: 10_000, want: []seedBatch{{1, 10_000}, {10_001, 12_345}}},
		{name: "sql server batch size", rows: 2_345, size: 1_000, want: []seedBatch{{1, 1_000}, {1_001, 2_000}, {2_001, 2_345}}},
		{name: "no rows", rows: 0, size: 1_000, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, seedBatches(tt.rows, tt.size))
		})
	}
}
