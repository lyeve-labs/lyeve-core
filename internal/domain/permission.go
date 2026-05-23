package domain

import (
	"time"

	"github.com/google/uuid"
)

// Permission defines what a role may do on a resource.
// SchemaName is the resource: a schema name, '*' for every schema, or a
// resource of a kind a plugin registers with core.RegisterPermissionKind. The
// column and the field are named for schemas, the most common resource.
// Actions is a subset of {create, read, update, delete}, plus the actions the
// resource's kind adds. The rule is core.PermissionActionsFor.
// FieldMask lists field names stripped from read responses.
type Permission struct {
	ID         uuid.UUID `json:"id"`
	Role       string    `json:"role"`
	SchemaName string    `json:"schema_name"`
	Actions    []string  `json:"actions"`
	FieldMask  []string  `json:"field_mask"`
	CreatedAt  time.Time `json:"created_at"`
	// TenantID is the tenant a rule on a plugin resource was written in.
	// Empty means the rule applies to every tenant, which every schema rule
	// does.
	TenantID string `json:"tenant_id"`
}
