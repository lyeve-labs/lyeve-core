package core

import (
	"context"
	"fmt"
	"strings"
)

// ContentValueShaper turns the field values a content write carries into the
// values the store keeps. The kernel's content store calls it on every write,
// after the before-write hooks and before the row is written, so no writer
// that goes through the store keeps a value the shaper did not produce, and a
// value a hook changed is shaped too. The schema engine supplies it beside
// its ContentRules, and an engine that supplies none leaves the role absent.
//
// It is a role of its own rather than part of ContentRules because the rules
// answer only whether a write passes, and a shaper changes what is stored: a
// rich text field's HTML is cleaned, or its document drawn into HTML.
type ContentValueShaper interface {
	// ShapeValues returns the data to store, the failures that refuse the
	// write, or an error when the values could not be shaped. data is a
	// whole create or a partial update, so a field data leaves out is left
	// out of the answer too, and a field it carries comes back in the form
	// to store. The store writes the map returned, never data, so a shaper
	// may answer a new map or data itself. An error refuses the write.
	ShapeValues(ctx context.Context, sc *Schema, data map[string]any) (map[string]any, []SchemaValidationError, error)
}

// ContentValueShaperProvider is implemented by the engine host and forwarded
// by ScopedHost. It answers the shaper of whatever schema engine is
// registered, and AbsentContentValueShaper when none is or the registered one
// supplies none. A caller reads it per write and never at Start.
type ContentValueShaperProvider interface {
	ContentValueShaper() ContentValueShaper
}

// ErrNoContentValueShaper is what a write answers when it carries a value
// only a shaper may store and none is registered. It wraps ErrNoSchemaEngine,
// so a writer refuses it as unavailable, as it refuses a write with no rules.
var ErrNoContentValueShaper = fmt.Errorf("%w, or it supplies no content value shaper", ErrNoSchemaEngine)

// AbsentContentValueShaper is what an install has with no schema engine, or
// with one that supplies no shaper. It refuses a write that names a rich_text
// field stored as a document with ErrNoContentValueShaper, because only a
// shaper checks the document and draws the HTML a read serves from it. Every
// other write, an html field's included, is answered unchanged.
type AbsentContentValueShaper struct{}

func (AbsentContentValueShaper) ShapeValues(_ context.Context, sc *Schema, data map[string]any) (map[string]any, []SchemaValidationError, error) {
	if sc != nil {
		for _, f := range sc.Fields {
			if f.FieldType != "rich_text" || f.Format != RichTextFormatDocument {
				continue
			}
			if _, ok := data[f.Name]; ok {
				return nil, nil, ErrNoContentValueShaper
			}
		}
	}
	return data, nil, nil
}

// ContentValueShaperOf answers the shaper a SchemaEngine supplies, and
// AbsentContentValueShaper for a nil engine or one that does not implement
// ContentValueShaper.
func ContentValueShaperOf(e SchemaEngine) ContentValueShaper {
	if s, ok := e.(ContentValueShaper); ok {
		return s
	}
	return AbsentContentValueShaper{}
}

// LazyContentValueShaper defers resolution to each call, for the reason
// LazyContentRules does: the content store is built before any plugin starts
// and registers its engine. A resolve that answers nil is the absent shaper.
func LazyContentValueShaper(resolve func() ContentValueShaper) ContentValueShaper {
	return lazyContentValueShaper{resolve: resolve}
}

type lazyContentValueShaper struct {
	resolve func() ContentValueShaper
}

func (l lazyContentValueShaper) ShapeValues(ctx context.Context, sc *Schema, data map[string]any) (map[string]any, []SchemaValidationError, error) {
	var s ContentValueShaper = AbsentContentValueShaper{}
	if l.resolve != nil {
		if r := l.resolve(); r != nil {
			s = r
		}
	}
	return s.ShapeValues(ctx, sc, data)
}

// ContentValueError refuses a write whose values the shaper failed. Failures
// are what the shaper answered, each naming its field. It is a validation
// failure: errors.Is(err, ErrValidation) holds, and the Content API answers it
// as 422 with the failures under "errors".
type ContentValueError struct {
	Failures []SchemaValidationError
}

func (e *ContentValueError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = f.Field + ": " + f.Code
	}
	return "content values refused: " + strings.Join(parts, ", ")
}

// Is reports the refusal as ErrValidation.
func (e *ContentValueError) Is(target error) bool { return target == ErrValidation }
