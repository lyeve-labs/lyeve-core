package enginehost

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errTenantRolesUnwired is the answer before the admin router has handed the
// host its reader. It is an outage the caller may retry, never a refusal.
var errTenantRolesUnwired = fmt.Errorf("tenant roles are not available yet: %w", core.ErrServiceUnavailable)

type tenantRolesBox struct{ reader core.TenantRolesReader }

// SetTenantRolesReader wires the engine's own resolution of an account's
// roles in a tenant, the one its sign-in uses. Nil clears it.
func (h *engineHost) SetTenantRolesReader(r core.TenantRolesReader) {
	h.tenantRoles.Store(&tenantRolesBox{reader: r})
}

// TenantRoles implements core.TenantRolesReader.
func (h *engineHost) TenantRoles(ctx context.Context, userID uuid.UUID, tenantID string) ([]string, error) {
	if b := h.tenantRoles.Load(); b != nil && b.reader != nil {
		return b.reader.TenantRoles(ctx, userID, tenantID)
	}
	return nil, errTenantRolesUnwired
}
