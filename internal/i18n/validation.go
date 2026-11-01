package i18n

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// LocalizeValidation renders each content validation failure in loc. A
// failure whose code the catalog knows gets the catalog's message, with the
// field among the message parameters. Any other failure keeps the message
// the engine wrote, and one with neither a known code nor a message reads as
// a field that failed validation. Every surface that refuses a content write
// answers with these, so a refusal reads the same whichever surface made it.
// The engine's parameter map is never written to. Nil when there is nothing
// to report.
func LocalizeValidation(loc Locale, errs []core.SchemaValidationError) []core.SchemaValidationError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]core.SchemaValidationError, len(errs))
	for i, e := range errs {
		params := make(map[string]any, len(e.Params)+1)
		for k, v := range e.Params {
			params[k] = v
		}
		if _, ok := params["field"]; !ok {
			params["field"] = e.Field
		}
		out[i] = core.SchemaValidationError{
			Field:   e.Field,
			Code:    e.Code,
			Rule:    e.Rule,
			Value:   e.Value,
			Message: e.Message,
			Params:  params,
		}
		if code := Code(e.Code); Known(code) {
			out[i].Message = code.Error(loc, params)
		} else if e.Message == "" {
			out[i].Message = CodeValidationUnknownField.Error(loc, params)
		}
		if e.Code == "" {
			out[i].Code = string(CodeValidationUnknownField)
		}
	}
	return out
}
