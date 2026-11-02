package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// numberSchema answers the schema a content write's body decodes against, or
// nil when there is none to read. A nil schema decodes the body as before, and
// the write then fails where the content rules load the schema, with the
// answer it always had.
func (h *ContentHandler) numberSchema(r *http.Request, schemaName string) *domain.Schema {
	if h.schemas == nil {
		return nil
	}
	sc, err := h.schemas.GetByName(r.Context(), schemaName)
	if err != nil {
		return nil
	}
	return sc
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
// convert is left as it came, for the content rules to refuse by the field's
// name.
func coerceNumbers(sc *domain.Schema, data map[string]any) {
	if !hasExactNumbers(sc) {
		return
	}
	for k, v := range data {
		switch exactFieldType(sc, k) {
		case "integer":
			if n, isNum := v.(json.Number); isNum {
				if i, err := core.IntegerValue(n); err == nil {
					data[k] = i
				}
			}
		case "decimal":
			if n, isNum := v.(json.Number); isNum {
				data[k] = string(n)
			}
		default:
			data[k] = numbersAsFloats(v)
		}
	}
}

// numbersAsFloats replaces every json.Number in v with its float64.
func numbersAsFloats(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return f
		}
	case map[string]any:
		for k, e := range t {
			t[k] = numbersAsFloats(e)
		}
	case []any:
		for i, e := range t {
			t[i] = numbersAsFloats(e)
		}
	}
	return v
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
