package core

// IndexRefusal returns why f may not be indexed or unique, or the empty string
// when it may or when it is neither.
//
// A json field cannot be an index key on two of the three databases. MySQL
// refuses an index on a JSON column with error 3152, and SQL Server stores the
// value as NVARCHAR(MAX), which no index key can be. PostgreSQL accepts a
// B-tree on JSONB, which compares whole documents and serves no lookup anyone
// writes. A schema that asks for one is refused when it is saved, with the
// field named, rather than failing in the DDL with the database's own words
// on two dialects and working on the third. The reason carries no sentinel,
// so each validator wraps it in its own.
func IndexRefusal(f SchemaField) string {
	if f.FieldType != "json" {
		return ""
	}
	switch {
	case f.Unique:
		return "a json field cannot be unique"
	case f.Indexed:
		return "a json field cannot be indexed"
	}
	return ""
}
