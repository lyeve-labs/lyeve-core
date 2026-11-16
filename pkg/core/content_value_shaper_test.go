package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shapingEngine is a schema engine that also supplies a shaper.
type shapingEngine struct {
	SchemaEngine
	calls int
}

func (s *shapingEngine) ShapeValues(_ context.Context, _ *Schema, data map[string]any) (map[string]any, []SchemaValidationError, error) {
	s.calls++
	return map[string]any{"body": "shaped"}, nil, nil
}

var richTextType = &Schema{Fields: []SchemaField{
	{Name: "title", FieldType: "text"},
	{Name: "body", FieldType: "rich_text"},
	{Name: "doc", FieldType: "rich_text", Format: RichTextFormatDocument},
}}

// absent asserts sh is the absent shaper: a rich text value is refused, an
// html one as well as a document, and a write naming no rich text field
// passes unchanged.
func absent(t *testing.T, sh ContentValueShaper) {
	t.Helper()
	data, errs, err := sh.ShapeValues(context.Background(), richTextType, map[string]any{"title": "a", "doc": map[string]any{"document": map[string]any{"type": "doc"}}})
	require.ErrorIs(t, err, ErrNoContentValueShaper)
	assert.ErrorIs(t, err, ErrNoSchemaEngine, "an absent shaper reads as an absent engine")
	assert.Nil(t, data)
	assert.Nil(t, errs)

	_, _, err = sh.ShapeValues(context.Background(), richTextType, map[string]any{"doc": nil})
	require.ErrorIs(t, err, ErrNoContentValueShaper, "a null names the field too")

	for _, sent := range []map[string]any{{"title": "a", "body": "<div>b</div>"}, {"body": nil}} {
		_, _, err = sh.ShapeValues(context.Background(), richTextType, sent)
		require.ErrorIs(t, err, ErrNoContentValueShaper, "an html value is refused too")
	}

	plain := map[string]any{"title": "a"}
	data, errs, err = sh.ShapeValues(context.Background(), richTextType, plain)
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Equal(t, plain, data)
}

func TestContentValueShaperOf_AnswersTheEnginesShaperOrTheAbsentOne(t *testing.T) {
	e := &shapingEngine{}
	assert.Same(t, e, ContentValueShaperOf(e))

	absent(t, ContentValueShaperOf(nil))
	absent(t, ContentValueShaperOf(plainEngine{}))
}

// A shaper built before the engine registers follows the registration, and
// the removal after it.
func TestLazyContentValueShaper_FollowsTheRegistration(t *testing.T) {
	var current ContentValueShaper
	lazy := LazyContentValueShaper(func() ContentValueShaper { return current })
	absent(t, lazy)

	e := &shapingEngine{}
	current = e
	data, _, err := lazy.ShapeValues(context.Background(), richTextType, map[string]any{"body": "x"})
	require.NoError(t, err)
	assert.Equal(t, "shaped", data["body"])
	assert.Equal(t, 1, e.calls)

	current = nil
	absent(t, lazy)
	absent(t, LazyContentValueShaper(nil))
}

type shaperHost struct {
	Host
	shaper ContentValueShaper
}

func (h shaperHost) ContentValueShaper() ContentValueShaper { return h.shaper }

func TestScopedHost_ContentValueShaperForwardsOrIsAbsent(t *testing.T) {
	e := &shapingEngine{}
	assert.Same(t, e, NewScopedHost(shaperHost{shaper: e}, "reader", 0).ContentValueShaper())

	absent(t, NewScopedHost(shaperHost{}, "reader", 0).ContentValueShaper())
	absent(t, NewScopedHost(struct{ Host }{}, "reader", 0).ContentValueShaper())
}

func TestContentValueError_IsAValidationFailureNamingEachField(t *testing.T) {
	err := error(&ContentValueError{Failures: []SchemaValidationError{
		{Field: "body", Code: "rich_text_too_large"},
		{Field: "intro", Code: "rich_text_invalid"},
	}})
	assert.ErrorIs(t, err, ErrValidation)
	assert.Equal(t, "content values refused: body: rich_text_too_large, intro: rich_text_invalid", err.Error())
	var refused *ContentValueError
	require.True(t, errors.As(errors.Join(errors.New("write"), err), &refused))
	assert.Len(t, refused.Failures, 2)
}
