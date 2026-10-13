// Package contentrules applies the rules the Content API applies to a write,
// for a writer outside the engine that stores content rows itself. A writer
// that calls these accepts and refuses exactly what the Content API does, and
// says why in the same words.
package contentrules

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	internalschema "github.com/lyeve-labs/lyeve-core/internal/schema"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// PrepareCreate fills every field data leaves out from its declared default
// and validates the result, in that order, as a create through the Content
// API does: a required field with a default is satisfied by leaving it out. A
// field data carries is kept as sent, an explicit null included, so a null on
// a required field is refused rather than defaulted.
//
// It returns the data to write, and the failures with each message in the
// locale the request on ctx resolved. No failures means the write may go
// ahead.
func PrepareCreate(ctx context.Context, sc *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError) {
	data = internalschema.ApplyDefaults(sc, data)
	errs := internalschema.ValidationErrors(internalschema.ValidateContent(sc, data))
	return data, errs.Localized(i18n.LocaleFromCtx(ctx))
}

// ValidatePatch validates a partial update the way an update through the
// Content API does. A field patch leaves out is not checked, so a stored value
// the update does not touch never refuses it. A field patch carries gets every
// rule a create applies, so an explicit null on a required field is refused. A
// cross-field rule runs against current with patch laid over it, and only when
// patch names one of its targets. No defaults are filled: a field an update
// leaves out keeps what is stored.
func ValidatePatch(ctx context.Context, sc *core.Schema, patch, current map[string]any) []core.SchemaValidationError {
	errs := internalschema.ValidationErrors(internalschema.ValidatePatch(sc, patch, current))
	return errs.Localized(i18n.LocaleFromCtx(ctx))
}
