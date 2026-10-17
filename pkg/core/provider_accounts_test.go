package core

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTenantLocalEmail_IsTheAccountsOwnKey(t *testing.T) {
	id := uuid.MustParse("6F1C2B3A-0000-4000-8000-00000000ABCD")
	key := TenantLocalEmail(id)
	assert.Equal(t, "6f1c2b3a-0000-4000-8000-00000000abcd@external.invalid", key, "lower case, as every stored address is")
	assert.Equal(t, NormalizeEmail(key), key)
	assert.True(t, IsTenantLocalEmail(id, key))
	assert.False(t, IsTenantLocalEmail(uuid.New(), key), "another account's key")
	assert.False(t, IsTenantLocalEmail(id, "person@agency.example"))
}

func TestProviderWindowAdmits(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	day := 24 * time.Hour
	cases := []struct {
		name   string
		seenAt *time.Time
		days   int
		want   bool
	}{
		{"no provider vouches for the account", nil, 0, true},
		{"one day inside the default window", ago(29 * day), ProviderWindowDefaultDays, true},
		{"at the edge of the window", ago(30 * day), ProviderWindowDefaultDays, false},
		{"past the window", ago(31 * day), ProviderWindowDefaultDays, false},
		{"0 admits no provider account", ago(time.Minute), 0, false},
		{"the longest window", ago(364 * day), ProviderWindowMaxDays, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ProviderWindowAdmits(tc.seenAt, tc.days, now))
		})
	}
}

func TestTenantAccount_SignInEmail(t *testing.T) {
	id := uuid.New()
	assert.Equal(t, "person@agency.example", (&TenantAccount{ID: id, Email: TenantLocalEmail(id), ProviderEmail: "person@agency.example"}).SignInEmail())
	assert.Equal(t, "person@agency.example", (&TenantAccount{ID: id, Email: "person@agency.example"}).SignInEmail())
}

// A scoped host over a host that keeps no accounts reports none and applies
// the default window, so a plugin can tell the two apart and still refuses
// an account past 30 days.
func TestScopedHost_ProviderRolesFallBack(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "bare", CapAll)
	ctx := context.Background()
	assert.Nil(t, h.TenantAccounts())
	days, err := h.ProviderWindowDays(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, ProviderWindowDefaultDays, days)
	old := time.Now().Add(-31 * 24 * time.Hour)
	ok, err := h.ProviderWindowAdmitsAccount(ctx, "acme", &old)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = h.ProviderWindowAdmitsAccount(ctx, "acme", nil)
	require.NoError(t, err)
	assert.True(t, ok)
}
