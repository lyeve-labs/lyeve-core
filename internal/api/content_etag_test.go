package api

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// An entry's fields are a map, and Go walks a map in a different order on each
// pass. The tag has to describe the entry, not the walk, or a client's
// If-None-Match never matches and every conditional read sends the body again.
func TestETagFor_SameEntrySameTag(t *testing.T) {
	data := map[string]any{}
	for i := 0; i < 24; i++ {
		data[fmt.Sprintf("field_%02d", i)] = i
	}
	item := &domain.Content{ID: uuid.New(), SchemaName: "post", Data: data}

	first := etagFor(item)
	require.NotEmpty(t, first)
	for i := 0; i < 50; i++ {
		assert.Equal(t, first, etagFor(item), "read %d tagged the unchanged entry differently", i)
	}

	data["field_00"] = "changed"
	assert.NotEqual(t, first, etagFor(item), "a changed entry keeps its tag")
}
