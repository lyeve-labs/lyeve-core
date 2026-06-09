package schema

import "github.com/lyeve-labs/lyeve-core/internal/domain"

// StripSystemFlags drops every client-supplied System flag: only the server
// may mark a field system. It runs before validation, because a system
// field is exempt from the identifier checks, so a client setting the flag
// on a relation to "drop table" would otherwise pass them.
func StripSystemFlags(sc *domain.Schema) {
	for i := range sc.Fields {
		sc.Fields[i].System = false
	}
}

// NormalizeSystemFields rewrites a validated definition the way the engine
// stores one: the id field, the foreign key column of every belongs_to
// relation and the opted-in timestamps are put back as locked system
// fields. Every path that persists a definition runs StripSystemFlags,
// validates, then this, the engine's own schema route and the host's
// SchemaEngine alike, so a schema created by a plugin reads back exactly
// like one created by hand.
func NormalizeSystemFields(sc *domain.Schema) {
	sc.Fields = InjectIDField(sc.Fields)
	sc.Fields = InjectFKFields(sc.Fields)
	InjectTimestampFields(sc)
}

// InjectTimestampFields appends created_at and/or updated_at as locked system
// fields at the end of the field list based on the schema's WithCreatedAt /
// WithUpdatedAt flags. Any user-supplied fields with those names are removed
// first to prevent duplicates.
func InjectTimestampFields(sc *domain.Schema) {
	filtered := make([]domain.SchemaField, 0, len(sc.Fields))
	for _, f := range sc.Fields {
		if f.Name != "created_at" && f.Name != "updated_at" {
			filtered = append(filtered, f)
		}
	}
	if sc.WithCreatedAt {
		filtered = append(filtered, domain.SchemaField{
			Name: "created_at", FieldType: "datetime", Required: true, System: true,
		})
	}
	if sc.WithUpdatedAt {
		filtered = append(filtered, domain.SchemaField{
			Name: "updated_at", FieldType: "datetime", Required: true, System: true,
		})
	}
	sc.Fields = filtered
}

// InjectIDField ensures the first field is always a required, indexed uid `id`.
func InjectIDField(fields []domain.SchemaField) []domain.SchemaField {
	const idName = "id"
	idField := domain.SchemaField{
		Name:      idName,
		FieldType: "uid",
		Required:  true,
		Unique:    true,
		Indexed:   true,
		System:    true,
	}
	filtered := make([]domain.SchemaField, 0, len(fields)+1)
	for _, f := range fields {
		if f.Name != idName {
			filtered = append(filtered, f)
		}
	}
	return append([]domain.SchemaField{idField}, filtered...)
}

// InjectFKFields adds a System:true field documenting the physical FK column for
// every belongs_to relation field (e.g., field "author" -> field "author_id").
// The function is idempotent: it strips stale FK system fields before re-injecting.
func InjectFKFields(fields []domain.SchemaField) []domain.SchemaField {
	type entry struct {
		afterField string
		fk         domain.SchemaField
	}

	var toInject []entry
	fkSet := make(map[string]struct{})

	for _, f := range fields {
		isBelongsTo := f.FieldType == "relation" &&
			(f.RelationType == domain.RelBelongsTo || f.RelationType == "")
		if !isBelongsTo {
			continue
		}
		fkName := domain.FKColumn(f.Name, f.RelationFKName)
		fkSet[fkName] = struct{}{}
		toInject = append(toInject, entry{
			afterField: f.Name,
			fk: domain.SchemaField{
				Name:      fkName,
				FieldType: "uid",
				System:    true,
				Required:  f.Required,
				Indexed:   true,
			},
		})
	}

	if len(toInject) == 0 {
		return fields
	}

	// Remove stale FK system fields so re-injection is idempotent.
	filtered := make([]domain.SchemaField, 0, len(fields))
	for _, f := range fields {
		if _, isFK := fkSet[f.Name]; isFK && f.System {
			continue
		}
		filtered = append(filtered, f)
	}

	// Re-insert each FK field immediately after its parent relation field.
	result := make([]domain.SchemaField, 0, len(filtered)+len(toInject))
	for _, f := range filtered {
		result = append(result, f)
		for _, e := range toInject {
			if e.afterField == f.Name {
				result = append(result, e.fk)
			}
		}
	}
	return result
}
