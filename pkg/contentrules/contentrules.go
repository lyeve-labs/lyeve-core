// Package contentrules applies the rules the Content API applies to a write,
// for a writer outside the engine that stores content rows itself. A writer
// that calls these accepts and refuses exactly what the Content API does, and
// says why in the same words.
//
// The rules come from the schema engine registered on the host, as they do for
// the Content API. A host with no engine, or with one that supplies no rules,
// answers core.ErrNoContentRules, and the writer refuses the write.
package contentrules

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// PrepareCreate fills every field data leaves out from its declared default
// and validates the result, in that order, as a create through the Content
// API does: a required field with a default is satisfied by leaving it out. A
// field data carries is kept as sent, an explicit null included, so a null on
// a required field is refused rather than defaulted.
//
// It returns the data to write, and the failures with each message in the
// locale the request on ctx resolved. No failures and no error means the
// write may go ahead. An error means the rules could not be applied, and the
// write must not go ahead.
func PrepareCreate(ctx context.Context, host core.Host, sc *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	data, errs, err := rulesOf(host).PrepareCreate(ctx, sc, data)
	if err != nil {
		return nil, nil, err
	}
	return data, i18n.LocalizeValidation(i18n.LocaleFromCtx(ctx), errs), nil
}

// ValidatePatch validates a partial update the way an update through the
// Content API does. A field patch leaves out is not checked, so a stored value
// the update does not touch never refuses it. A field patch carries gets every
// rule a create applies, so an explicit null on a required field is refused. A
// cross-field rule runs against current with patch laid over it, and only when
// patch names one of its targets. No defaults are filled: a field an update
// leaves out keeps what is stored. An error means the rules could not be
// applied, and the write must not go ahead.
func ValidatePatch(ctx context.Context, host core.Host, sc *core.Schema, patch, current map[string]any) ([]core.SchemaValidationError, error) {
	errs, err := rulesOf(host).ValidatePatch(ctx, sc, patch, current)
	if err != nil {
		return nil, err
	}
	return i18n.LocalizeValidation(i18n.LocaleFromCtx(ctx), errs), nil
}

// rulesOf asks the host for the registered engine's rules on every call, so a
// writer built before the engine registered still reads them.
func rulesOf(host core.Host) core.ContentRules {
	if p, ok := host.(core.ContentRulesProvider); ok {
		if r := p.ContentRules(); r != nil {
			return r
		}
	}
	return core.AbsentContentRules{}
}
