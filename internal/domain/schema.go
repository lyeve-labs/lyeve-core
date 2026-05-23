package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Content is a single record belonging to a schema.
type Content struct {
	ID         uuid.UUID      `json:"id"`
	SchemaName string         `json:"schema_name"`
	Data       map[string]any `json:"data"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`

	// ResolvedLocale is the locale Data was served in when a read asked for
	// one and a localizer answered. Empty, and absent from the JSON, on every
	// other read.
	ResolvedLocale string `json:"resolved_locale,omitempty"`
}

// The schema definition types live in pkg/core, because a definition is a data
// shape that anything outside this module may need to read. The aliases below
// let code in this module name them through domain.

// Schema defines the structure of a content type.
type Schema = core.Schema

// SchemaField is one column or property within a Schema.
type SchemaField = core.SchemaField

// PopulateConfig defines how to populate relation fields on content responses.
type PopulateConfig = core.PopulateConfig

// RelationType describes the cardinality of a relation field.
type RelationType = core.RelationType

const (
	// RelBelongsTo marks a field that holds the foreign key column.
	RelBelongsTo = core.RelBelongsTo
	// RelHasOne marks a field that is the inverse of belongs_to.
	RelHasOne = core.RelHasOne
	// RelHasMany marks a one-to-many relation field.
	RelHasMany = core.RelHasMany
	// RelManyToMany marks a many-to-many relation via a pivot table.
	RelManyToMany = core.RelManyToMany
)

// PivotTableName returns the canonical pivot table name for a many-to-many
// relation between two schemas.
func PivotTableName(a, b string) string { return core.PivotTableName(a, b) }

// FKColumn returns the FK column name for a belongs_to or has_one field.
func FKColumn(fieldName, override string) string { return core.FKColumn(fieldName, override) }

// TableName returns the physical table name for a schema.
func TableName(schemaName string) string { return core.TableName(schemaName) }

// RelationFields returns the names of every relation-type field in a schema.
func RelationFields(fields []SchemaField) []string { return core.RelationFields(fields) }
