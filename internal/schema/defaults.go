package schema

import "github.com/lyeve-labs/lyeve-core/internal/domain"

// ApplyDefaults fills every field the caller omitted from its declared
// default and returns the map. It runs before validation on a create, so a
// required field with a default is satisfied by leaving it out.
//
// Only a create applies defaults. An update that omits a field means "leave
// it", and filling the default in would overwrite the stored value.
//
// A field the caller sent is left as sent, null included: an explicit null
// on a required field is the caller's mistake and validation reports it. The
// default lands under the field's own name, the key validation reads. The
// store accepts either that or the physical column, so a belongs_to sent by
// its column counts as sent.
func ApplyDefaults(sc *domain.Schema, data map[string]any) map[string]any {
	if sc == nil {
		return data
	}
	if data == nil {
		data = make(map[string]any)
	}
	for _, f := range sc.Fields {
		if f.System || f.Default == nil {
			continue
		}
		if _, ok := data[f.Name]; ok {
			continue
		}
		if f.FieldType == "relation" {
			if _, ok := data[domain.FKColumn(f.Name, f.RelationFKName)]; ok {
				continue
			}
		}
		data[f.Name] = f.Default
	}
	return data
}
