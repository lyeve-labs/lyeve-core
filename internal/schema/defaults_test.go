package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestApplyDefaults_FillsOmittedFieldsOnly(t *testing.T) {
	sc := &domain.Schema{Fields: []domain.SchemaField{
		{Name: "id", FieldType: "uid", System: true, Default: "never"},
		{Name: "status", FieldType: "text", Required: true, Default: "pending"},
		{Name: "score", FieldType: "number", Default: float64(0)},
		{Name: "note", FieldType: "text"},
		{Name: "owner", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "users", Default: "u1"},
	}}

	t.Run("an omitted field takes its default", func(t *testing.T) {
		got := ApplyDefaults(sc, map[string]any{"note": "x"})
		assert.Equal(t, "pending", got["status"])
		assert.Equal(t, float64(0), got["score"])
		assert.Equal(t, "u1", got["owner"])
		_, hasID := got["id"]
		assert.False(t, hasID, "a system field never takes a default")
	})

	t.Run("a value the caller sent is kept, null included", func(t *testing.T) {
		got := ApplyDefaults(sc, map[string]any{"status": "approved", "score": nil})
		assert.Equal(t, "approved", got["status"])
		v, ok := got["score"]
		assert.True(t, ok)
		assert.Nil(t, v)
	})

	t.Run("a relation sent by its column counts as sent", func(t *testing.T) {
		got := ApplyDefaults(sc, map[string]any{"owner_id": "u2"})
		_, byName := got["owner"]
		assert.False(t, byName)
		assert.Equal(t, "u2", got["owner_id"])
	})

	t.Run("a nil map becomes one that carries the defaults", func(t *testing.T) {
		got := ApplyDefaults(sc, nil)
		assert.Equal(t, "pending", got["status"])
	})

	t.Run("the defaulted value passes the required check", func(t *testing.T) {
		got := ApplyDefaults(sc, map[string]any{})
		assert.Empty(t, ValidateContent(sc, got))
		assert.NotEmpty(t, ValidateContent(sc, map[string]any{}), "without defaults the required field is refused")
	})
}

// A json field's default is a map or a list held by the schema registry, which
// hands one definition to every request. The entry a create fills is the
// request's to edit, and a before-create hook may edit it in place, so the
// default it carries has to be its own.
func TestApplyDefaults_CopiesADefaultTheEntryCanEdit(t *testing.T) {
	sc := &domain.Schema{Fields: []domain.SchemaField{
		{Name: "prefs", FieldType: "json", Default: map[string]any{
			"theme": "light",
			"tags":  []any{"new", map[string]any{"pinned": false}},
		}},
	}}
	want := map[string]any{
		"theme": "light",
		"tags":  []any{"new", map[string]any{"pinned": false}},
	}

	first := ApplyDefaults(sc, map[string]any{})
	prefs, ok := first["prefs"].(map[string]any)
	require.True(t, ok, "the default did not land as a map: %T", first["prefs"])
	prefs["theme"] = "dark"
	tags, ok := prefs["tags"].([]any)
	require.True(t, ok, "the default's list did not land as a list: %T", prefs["tags"])
	tags[0] = "edited"
	tags[1].(map[string]any)["pinned"] = true

	assert.Equal(t, want, sc.Fields[0].Default, "editing the entry rewrote the schema's default")
	assert.Equal(t, want, ApplyDefaults(sc, map[string]any{})["prefs"], "the next create took the edited default")
}
