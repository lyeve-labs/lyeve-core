package db

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func schemaWithDefault() *domain.Schema {
	return &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "status", FieldType: "text", Default: "draft"},
			{Name: "views", FieldType: "number", Default: float64(0)},
			{Name: "note", FieldType: "text"},
		},
	}
}

func colIndex(cols []string, want string) int {
	for i, c := range cols {
		if c == want {
			return i
		}
	}
	return -1
}

// The DDL generator emits no DEFAULT clause, so the writer applies a declared
// default, or a field relying on one would store NULL.
func TestBuildInsert_AppliesTheDeclaredDefault(t *testing.T) {
	d := dialect.Must("postgres")
	cols, args := buildInsert(schemaWithDefault(), map[string]any{"title": "hello"}, d)

	i := colIndex(cols, `"status"`)
	require.GreaterOrEqual(t, i, 0, "a field with a default has to be written: %v", cols)
	assert.Equal(t, "draft", args[i])

	j := colIndex(cols, `"views"`)
	require.GreaterOrEqual(t, j, 0)
	assert.EqualValues(t, 0, args[j])

	assert.Equal(t, -1, colIndex(cols, `"note"`),
		"a field with neither a value nor a default stays out of the insert")
}

// A supplied value wins, including a zero one: the default fills an absent
// key, it does not override what the caller sent.
func TestBuildInsert_ASuppliedValueWinsOverTheDefault(t *testing.T) {
	d := dialect.Must("postgres")
	cols, args := buildInsert(schemaWithDefault(),
		map[string]any{"title": "hello", "status": "published", "views": float64(0)}, d)

	i := colIndex(cols, `"status"`)
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "published", args[i])

	j := colIndex(cols, `"views"`)
	require.GreaterOrEqual(t, j, 0)
	assert.EqualValues(t, 0, args[j])
}

// An empty string is a value, not an absence.
func TestBuildInsert_AnEmptyStringIsNotAnAbsentKey(t *testing.T) {
	d := dialect.Must("postgres")
	cols, args := buildInsert(schemaWithDefault(), map[string]any{"status": ""}, d)

	i := colIndex(cols, `"status"`)
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "", args[i], "the caller asked for empty, not for the default")
}
