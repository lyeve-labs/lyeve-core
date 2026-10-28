package api

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// fakeMemberships stands in for the reader a tenancy plugin supplies. The
// engine stores no memberships, so this is a non-database boundary and gets a
// hand-written fake rather than a table.
//
// Lookups fold case on the tenant, which is what MySQL and MSSQL do with the
// stored value. A fake that matched exactly would pass the spelling test on
// its own strictness rather than on the engine's guard.
type fakeMemberships struct {
	rows map[uuid.UUID]map[string]*core.Membership
}

func newFakeMemberships() *fakeMemberships {
	return &fakeMemberships{rows: map[uuid.UUID]map[string]*core.Membership{}}
}

func (f *fakeMemberships) Grant(_ context.Context, userID uuid.UUID, tenantID string, roles []string) (*core.Membership, error) {
	byTenant, ok := f.rows[userID]
	if !ok {
		byTenant = map[string]*core.Membership{}
		f.rows[userID] = byTenant
	}
	now := time.Now().UTC()
	m := &core.Membership{UserID: userID, TenantID: tenantID, Roles: roles, CreatedAt: now, UpdatedAt: now}
	byTenant[strings.ToLower(tenantID)] = m
	return m, nil
}

func (f *fakeMemberships) Revoke(_ context.Context, userID uuid.UUID, tenantID string) error {
	byTenant, ok := f.rows[userID]
	if !ok {
		return core.ErrNotFound
	}
	key := strings.ToLower(tenantID)
	if _, ok := byTenant[key]; !ok {
		return core.ErrNotFound
	}
	delete(byTenant, key)
	return nil
}

func (f *fakeMemberships) Get(_ context.Context, userID uuid.UUID, tenantID string) (*core.Membership, error) {
	m, ok := f.rows[userID][strings.ToLower(tenantID)]
	if !ok {
		return nil, core.ErrNotFound
	}
	return m, nil
}

func (f *fakeMemberships) ListForUser(_ context.Context, userID uuid.UUID) ([]*core.Membership, error) {
	byTenant := f.rows[userID]
	keys := make([]string, 0, len(byTenant))
	for k := range byTenant {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*core.Membership, 0, len(keys))
	for _, k := range keys {
		out = append(out, byTenant[k])
	}
	return out, nil
}
