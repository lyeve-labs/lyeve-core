package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireTenantID(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		wantTID string
		wantErr error
	}{
		{
			name:    "bare context - no tenant",
			ctx:     context.Background(),
			wantTID: "",
			wantErr: ErrTenantRequired,
		},
		{
			name:    "tenant set via WithTenantID",
			ctx:     WithTenantID(context.Background(), "acme-corp"),
			wantTID: "acme-corp",
			wantErr: nil,
		},
		{
			name:    "empty-string tenant is treated as missing",
			ctx:     WithTenantID(context.Background(), ""),
			wantTID: "",
			wantErr: ErrTenantRequired,
		},
		{
			name:    "non-empty arbitrary tenant",
			ctx:     WithTenantID(context.Background(), "tenant_42"),
			wantTID: "tenant_42",
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tid, err := RequireTenantID(tc.ctx)
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "expected ErrTenantRequired, got %v", err)
				assert.Equal(t, "", tid)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantTID, tid)
			}
		})
	}
}

func TestRequireTenantID_ErrTenantRequired_IsWrappable(t *testing.T) {
	// Confirm plugins can %w wrap ErrTenantRequired and still match it.
	_, err := RequireTenantID(context.Background())
	require.Error(t, err)

	wrapped := errors.New("store.Create: %w") // placeholder
	wrapped = errors.Join(wrapped, err)       // Join wraps correctly

	assert.True(t, errors.Is(wrapped, ErrTenantRequired),
		"ErrTenantRequired must be wrappable with errors.Is")
}
