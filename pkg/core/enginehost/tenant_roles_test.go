package enginehost

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type rolesFunc func(context.Context, uuid.UUID, string) ([]string, error)

func (f rolesFunc) TenantRoles(ctx context.Context, userID uuid.UUID, tenantID string) ([]string, error) {
	return f(ctx, userID, tenantID)
}

// Before the admin router wires the reader, and once it is cleared, a read
// is an outage the caller may retry, never an answer that the account holds
// nothing.
func TestEngineHost_TenantRolesAnswersOnlyOnceWired(t *testing.T) {
	h := &engineHost{}
	ctx := context.Background()
	if roles, err := h.TenantRoles(ctx, uuid.New(), "acme"); !errors.Is(err, core.ErrServiceUnavailable) || errors.Is(err, core.ErrNotFound) || roles != nil {
		t.Fatalf("unwired: roles %v, err %v, want an outage", roles, err)
	}

	id := uuid.New()
	h.SetTenantRolesReader(rolesFunc(func(_ context.Context, userID uuid.UUID, tenantID string) ([]string, error) {
		if userID != id || tenantID != "acme" {
			return nil, core.ErrNotFound
		}
		return []string{"editor"}, nil
	}))
	if roles, err := h.TenantRoles(ctx, id, "acme"); err != nil || !slices.Equal(roles, []string{"editor"}) {
		t.Fatalf("wired: roles %v, err %v, want the reader's answer", roles, err)
	}
	if _, err := h.TenantRoles(ctx, id, "globex"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("wired, another tenant: err %v, want the reader's ErrNotFound", err)
	}

	h.SetTenantRolesReader(nil)
	if _, err := h.TenantRoles(ctx, id, "acme"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("cleared: err %v, want an outage", err)
	}
}
