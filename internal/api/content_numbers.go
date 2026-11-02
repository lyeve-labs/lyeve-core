package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// writeSchema looks up, once per write, the schema a content write decodes
// its body against and validates it by. The caller answers the lookup's error
// after the decode, so a malformed body is a 400 whatever the schema.
func (h *ContentHandler) writeSchema(r *http.Request, schemaName string) (*domain.Schema, error) {
	if h.schemas == nil {
		return nil, core.ErrNoSchemaEngine
	}
	return h.schemas.GetByName(r.Context(), schemaName)
}

// hasExactNumbers reports whether sc declares an integer or a decimal field,
// whose values a float64 decode would round.
func hasExactNumbers(sc *domain.Schema) bool {
	if sc == nil {
		return false
	}
	for _, f := range sc.Fields {
		if f.FieldType == "integer" || f.FieldType == "decimal" {
			return true
		}
	}
	return false
}

// decodeContentBody decodes a content write's body into v. For a schema with
// an integer or a decimal field every number is kept as its literal text, so
// 9007199254740993 does not become 9007199254740992 before anything reads it,
// and coerceNumbers then gives each number its field's type. Any other schema
// decodes as it always has.
func decodeContentBody(r io.Reader, sc *domain.Schema, v any) error {
	if hasExactNumbers(sc) {
		return jsonpool.DecodeJSONNumbers(r, v)
	}
	return jsonpool.DecodeJSON(r, v)
}

// coerceNumbers gives each json.Number decodeContentBody left in data the
// type its field reads: an int64 for an integer field, the literal text for a
// decimal field, and a float64 everywhere else, inside json fields and
// undeclared keys too, which is what they held before. A number it cannot
// convert to its field's type is left as it came, for the content rules to
// refuse by the field's name. A number no float64 holds, such as 1e400, is
// errNumberRange, which the float decode refused as invalid JSON.
func coerceNumbers(sc *domain.Schema, data map[string]any) error {
	if !hasExactNumbers(sc) {
		return nil
	}
	for k, v := range data {
		switch exactFieldType(sc, k) {
		case "integer":
			if n, isNum := v.(json.Number); isNum {
				if !floatRange(n) {
					return errNumberRange
				}
				if i, err := core.IntegerValue(n); err == nil {
					data[k] = i
				}
			}
		case "decimal":
			if n, isNum := v.(json.Number); isNum {
				if !floatRange(n) {
					return errNumberRange
				}
				data[k] = string(n)
			}
		default:
			f, err := numbersAsFloats(v)
			if err != nil {
				return err
			}
			data[k] = f
		}
	}
	return nil
}

// errNumberRange is a JSON number outside the float64 range.
var errNumberRange = errors.New("number out of range")

// floatRange reports whether n is within the float64 range.
func floatRange(n json.Number) bool {
	_, err := strconv.ParseFloat(string(n), 64)
	return err == nil
}

// numbersAsFloats replaces every json.Number in v with its float64.
func numbersAsFloats(v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return nil, errNumberRange
		}
		return f, nil
	case map[string]any:
		for k, e := range t {
			f, err := numbersAsFloats(e)
			if err != nil {
				return nil, err
			}
			t[k] = f
		}
	case []any:
		for i, e := range t {
			f, err := numbersAsFloats(e)
			if err != nil {
				return nil, err
			}
			t[i] = f
		}
	}
	return v, nil
}

// exactFieldType answers "integer" or "decimal" for a key that names such a
// field of sc, and "" for any other key.
func exactFieldType(sc *domain.Schema, key string) string {
	for _, f := range sc.Fields {
		if f.Name == key && (f.FieldType == "integer" || f.FieldType == "decimal") {
			return f.FieldType
		}
	}
	return ""
}

// exactValues rewrites each integer and decimal value in data, once the
// content rules have passed it, as what the store will bind: an int64, and the
// decimal's canonical text with exactly its scale's digits. Hooks and events
// then see one form whatever the client sent. A value neither function takes
// is left for the store, which refuses it with the field's name.
func exactValues(sc *domain.Schema, data map[string]any) {
	for _, f := range sc.Fields {
		v, ok := data[f.Name]
		if !ok || v == nil {
			continue
		}
		switch f.FieldType {
		case "integer":
			if i, err := core.IntegerValue(v); err == nil {
				data[f.Name] = i
			}
		case "decimal":
			p, s := core.DecimalSpec(f)
			if text, err := core.DecimalValue(v, p, s); err == nil {
				data[f.Name] = text
			}
		}
	}
}
