package contentrules

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// cannedShaper answers every write with the same values and failures.
type cannedShaper struct {
	values map[string]any
	errs   []core.SchemaValidationError
	err    error
}

func (c cannedShaper) ShapeValues(context.Context, *core.Schema, map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	return c.values, c.errs, c.err
}

// shaperHost is a host whose registered engine supplies a shaper.
type shaperHost struct {
	core.Host
	shaper core.ContentValueShaper
}

func (h shaperHost) ContentValueShaper() core.ContentValueShaper { return h.shaper }

var richTextType = &core.Schema{Fields: []core.SchemaField{
	{Name: "title", FieldType: "text"},
	{Name: "body", FieldType: "rich_text"},
	{Name: "doc", FieldType: "rich_text", Format: core.RichTextFormatDocument},
}}

func TestShapeValues_WritesWhatTheShaperReturns(t *testing.T) {
	host := shaperHost{shaper: cannedShaper{values: map[string]any{"body": "<p>clean</p>"}}}
	data, errs, err := ShapeValues(context.Background(), host, richTextType, map[string]any{"body": "<div>dirty</div>"})
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Equal(t, map[string]any{"body": "<p>clean</p>"}, data)
}

func TestShapeValues_FailureReadsInTheRequestLocale(t *testing.T) {
	host := shaperHost{shaper: cannedShaper{errs: []core.SchemaValidationError{{
		Field: "body", Code: "VALIDATION_FIELD_TYPE", Rule: "rich_text",
	}}}}
	_, en, err := ShapeValues(context.Background(), host, richTextType, map[string]any{"body": 1})
	require.NoError(t, err)
	require.Len(t, en, 1)
	assert.Equal(t, "body", en[0].Field)
	assert.NotEmpty(t, en[0].Message)

	_, fr, err := ShapeValues(i18n.WithLocale(context.Background(), i18n.LocaleFR), host, richTextType, map[string]any{"body": 1})
	require.NoError(t, err)
	require.Len(t, fr, 1)
	assert.NotEqual(t, en[0].Message, fr[0].Message)
}

// With no shaper a rich text value is refused as unavailable, an html one as
// well as a document, and a write naming no rich text field passes unchanged.
func TestShapeValues_NoShaperRefusesRichText(t *testing.T) {
	hosts := map[string]core.Host{
		"host without the role": shaperHost{}.Host,
		"role answers nil":      shaperHost{},
		"absent shaper":         shaperHost{shaper: core.AbsentContentValueShaper{}},
	}
	for name, host := range hosts {
		t.Run(name, func(t *testing.T) {
			data, errs, err := ShapeValues(context.Background(), host, richTextType, map[string]any{"doc": map[string]any{"document": map[string]any{"type": "doc"}}})
			assert.ErrorIs(t, err, core.ErrNoContentValueShaper)
			assert.ErrorIs(t, err, core.ErrNoSchemaEngine)
			assert.Nil(t, data)
			assert.Nil(t, errs)

			_, _, err = ShapeValues(context.Background(), host, richTextType, map[string]any{"body": "<div>x</div>"})
			assert.ErrorIs(t, err, core.ErrNoContentValueShaper, "an html value is refused too")

			plain := map[string]any{"title": "x"}
			data, errs, err = ShapeValues(context.Background(), host, richTextType, plain)
			require.NoError(t, err)
			assert.Empty(t, errs)
			assert.Equal(t, plain, data)
		})
	}
}

func TestShapeValues_ShaperErrorRefusesTheWrite(t *testing.T) {
	boom := errors.New("shaper unavailable")
	data, errs, err := ShapeValues(context.Background(), shaperHost{shaper: cannedShaper{err: boom}}, richTextType, map[string]any{})
	assert.ErrorIs(t, err, boom)
	assert.Nil(t, data)
	assert.Nil(t, errs)
}
