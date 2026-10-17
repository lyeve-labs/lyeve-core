package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestTenantPurgeOutcome_RunsEachRegisteredOutcomeOnceInOrder(t *testing.T) {
	ctx, end := core.WithTenantPurgeOutcomes(context.Background())
	var got []string
	assert.True(t, core.OnTenantPurgeOutcome(ctx, func(_ context.Context, committed bool) {
		got = append(got, "first", map[bool]string{true: "commit", false: "rollback"}[committed])
	}))
	assert.True(t, core.OnTenantPurgeOutcome(ctx, func(_ context.Context, committed bool) {
		got = append(got, "second", map[bool]string{true: "commit", false: "rollback"}[committed])
	}))

	end(ctx, false)
	end(ctx, true)

	assert.Equal(t, []string{"first", "rollback", "second", "rollback"}, got)
}

func TestTenantPurgeOutcome_RefusedOutsideADeleteAndAfterItEnds(t *testing.T) {
	ran := false
	fn := func(context.Context, bool) { ran = true }

	assert.False(t, core.OnTenantPurgeOutcome(context.Background(), fn))

	ctx, end := core.WithTenantPurgeOutcomes(context.Background())
	end(ctx, true)
	assert.False(t, core.OnTenantPurgeOutcome(ctx, fn))
	end(ctx, true)
	assert.False(t, ran)
}
