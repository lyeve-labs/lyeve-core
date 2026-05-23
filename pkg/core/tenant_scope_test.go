package core

import (
	"context"
	"testing"
)

// TestResolveTenantScope pins which callers reach the cross-tenant scope. The
// empty tenant makes a store drop its tenant_id predicate, and sys_* tables are
// not replicated per tenant, so an unearned empty answer here is every tenant's
// rows in one response.
func TestResolveTenantScope(t *testing.T) {
	admin := &AuthClaims{Roles: []string{"admin"}}
	super := &AuthClaims{Roles: []string{"super_admin"}}

	cases := []struct {
		name   string
		ctx    context.Context
		wantID string
		wantOK bool
	}{
		{
			name:   "context tenant wins",
			ctx:    WithTenantID(context.WithValue(context.Background(), ClaimsKey, super), "acme"),
			wantID: "acme", wantOK: true,
		},
		{
			name:   "claim tenant when context is empty",
			ctx:    context.WithValue(context.Background(), ClaimsKey, &AuthClaims{Roles: []string{"admin"}, TenantID: "beta"}),
			wantID: "beta", wantOK: true,
		},
		{
			name:   "admin with no tenant anywhere is refused",
			ctx:    context.WithValue(context.Background(), ClaimsKey, admin),
			wantID: "", wantOK: false,
		},
		{
			name:   "super_admin with no tenant holds the cross-tenant scope",
			ctx:    context.WithValue(context.Background(), ClaimsKey, super),
			wantID: "", wantOK: true,
		},
		{
			name:   "no claims at all is refused",
			ctx:    context.Background(),
			wantID: "", wantOK: false,
		},
		{
			name:   "an empty context tenant does not stand in for the claim",
			ctx:    WithTenantID(context.WithValue(context.Background(), ClaimsKey, admin), ""),
			wantID: "", wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := ResolveTenantScope(tc.ctx)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
