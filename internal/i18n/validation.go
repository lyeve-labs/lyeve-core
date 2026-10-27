package i18n

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// LocalizeValidation renders each content validation failure in loc: the
// catalog's message for its code, with the field among the message
// parameters. A failure with no code reads as an unknown field. Every surface
// that refuses a content write answers with these, so a refusal reads the
// same whichever surface made it. Nil when there is nothing to report.
func LocalizeValidation(loc Locale, errs []core.SchemaValidationError) []core.SchemaValidationError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]core.SchemaValidationError, len(errs))
	for i, e := range errs {
		code := Code(e.Code)
		if code == "" {
			code = CodeValidationUnknownField
		}
		params := e.Params
		if params == nil {
			params = map[string]any{"field": e.Field}
		} else if _, ok := params["field"]; !ok {
			params["field"] = e.Field
		}
		out[i] = core.SchemaValidationError{
			Field:   e.Field,
			Code:    string(code),
			Rule:    e.Rule,
			Value:   e.Value,
			Message: code.Error(loc, params),
			Params:  params,
		}
	}
	return out
}
