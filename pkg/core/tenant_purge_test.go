package core_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TenantPurgeHandler registry tests

func TestRegisterAndCountPurgeHandlers(t *testing.T) {
	// Reset global state from other tests.
	core.ResetTenantPurgeHandlers()

	noop := func(ctx context.Context, q core.Querier, slug string) error { return nil }

	assert.Equal(t, 0, core.TenantPurgeHandlerCount(), "zero after reset")

	core.RegisterTenantPurgeHandler(noop)
	assert.Equal(t, 1, core.TenantPurgeHandlerCount(), "one after register")

	core.RegisterTenantPurgeHandler(noop)
	assert.Equal(t, 2, core.TenantPurgeHandlerCount(), "two after second register")

	handlers := core.TenantPurgeHandlers()
	assert.Len(t, handlers, 2, "handlers slice length matches count")

	core.ResetTenantPurgeHandlers()
	assert.Equal(t, 0, core.TenantPurgeHandlerCount(), "zero after reset")
}

// Covered table registry tests

func TestCoveredTableRegistry(t *testing.T) {
	core.ResetCoveredTables()

	set := core.CoveredTableSet()
	assert.Empty(t, set, "empty after reset")

	core.RegisterCoveredTable("sys_media")
	core.RegisterCoveredTable("sys_widgets")
	core.RegisterCoveredTable("sys_media") // duplicate: idempotent

	set = core.CoveredTableSet()
	assert.True(t, set["sys_media"], "sys_media covered")
	assert.True(t, set["sys_widgets"], "sys_widgets covered")
	assert.False(t, set["sys_unknown"], "sys_unknown not covered")
	assert.Len(t, set, 2, "only two unique tables")

	core.ResetCoveredTables()
	assert.Empty(t, core.CoveredTableSet(), "empty after reset")
}

// PurgeRegistrationValidator tests

func TestValidator_AllCovered(t *testing.T) {
	core.ResetCoveredTables()

	core.RegisterCoveredTable("sys_media")
	core.RegisterCoveredTable("sys_widgets")
	core.RegisterCoveredTable("sys_content_entries")

	v := core.NewPurgeRegistrationValidator()
	scoped := []string{"sys_media", "sys_widgets", "sys_content_entries"}
	missing := v.MissingTables(scoped)
	assert.Empty(t, missing, "no missing tables when all covered")
}

func TestValidator_SomeMissing(t *testing.T) {
	core.ResetCoveredTables()

	core.RegisterCoveredTable("sys_media")

	v := core.NewPurgeRegistrationValidator()
	scoped := []string{"sys_media", "sys_widgets", "sys_content_entries"}
	missing := v.MissingTables(scoped)
	assert.Len(t, missing, 2, "two missing tables")
	assert.Contains(t, missing, "sys_widgets")
	assert.Contains(t, missing, "sys_content_entries")
}

func TestValidator_NoneCovered(t *testing.T) {
	core.ResetCoveredTables()

	v := core.NewPurgeRegistrationValidator()
	scoped := []string{"sys_media", "sys_widgets"}
	missing := v.MissingTables(scoped)
	assert.Len(t, missing, 2, "all tables missing when nothing covered")
}

func TestValidator_EmptyScopedList(t *testing.T) {
	core.ResetCoveredTables()

	v := core.NewPurgeRegistrationValidator()
	missing := v.MissingTables(nil)
	assert.Empty(t, missing, "empty when scoped list is nil")

	missing = v.MissingTables([]string{})
	assert.Empty(t, missing, "empty when scoped list is empty")
}

func TestValidator_MixedCoverage(t *testing.T) {
	core.ResetCoveredTables()

	core.RegisterCoveredTable("sys_media")
	core.RegisterCoveredTable("sys_widgets")

	v := core.NewPurgeRegistrationValidator()
	scoped := []string{
		"sys_media",           // covered
		"sys_widgets",         // covered
		"sys_content_entries", // NOT covered
		"sys_webhooks",        // NOT covered
		"sys_api_keys",        // NOT covered
	}
	missing := v.MissingTables(scoped)
	require.Len(t, missing, 3)
	assert.Contains(t, missing, "sys_content_entries")
	assert.Contains(t, missing, "sys_webhooks")
	assert.Contains(t, missing, "sys_api_keys")
	assert.NotContains(t, missing, "sys_media")
	assert.NotContains(t, missing, "sys_widgets")
}

// End-to-end: purge handler + covered table wiring

func TestHandlerAndCoveredTableTogether(t *testing.T) {
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()

	noop := func(ctx context.Context, q core.Querier, slug string) error { return nil }

	core.RegisterTenantPurgeHandler(noop)
	core.RegisterCoveredTable("sys_media")

	assert.Equal(t, 1, core.TenantPurgeHandlerCount())
	assert.True(t, core.CoveredTableSet()["sys_media"])

	v := core.NewPurgeRegistrationValidator()
	missing := v.MissingTables([]string{"sys_media"})
	assert.Empty(t, missing, "table is covered by registered handler")
}

func TestHandlerWithoutCoveredTable_FoundMissing(t *testing.T) {
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()

	noop := func(ctx context.Context, q core.Querier, slug string) error { return nil }

	// Handler registered but forgot to call RegisterCoveredTable.
	core.RegisterTenantPurgeHandler(noop)

	v := core.NewPurgeRegistrationValidator()
	missing := v.MissingTables([]string{"sys_media"})
	assert.Len(t, missing, 1, "table is missing despite handler existing - forgot RegisterCoveredTable")
	assert.Equal(t, "sys_media", missing[0])
}

// Concurrent safety (smoke test)

func TestConcurrentRegistration(t *testing.T) {
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()

	noop := func(ctx context.Context, q core.Querier, slug string) error { return nil }
	done := make(chan struct{})

	for i := 0; i < 100; i++ {
		go func() {
			core.RegisterTenantPurgeHandler(noop)
			core.RegisterCoveredTable("sys_test")
			done <- struct{}{}
		}()
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	assert.Equal(t, 100, core.TenantPurgeHandlerCount())
	assert.True(t, core.CoveredTableSet()["sys_test"])
}

// Full integration: validator with large scoped set

func TestValidator_LargeScopedSet(t *testing.T) {
	core.ResetCoveredTables()

	// Twenty-seven plugins with three tenant-scoped tables each, plus the
	// engine's own sys_users.
	covered := []string{"sys_users"}
	for p := 0; p < 27; p++ {
		for n := 0; n < 3; n++ {
			covered = append(covered, fmt.Sprintf("plugin_%02d_table_%d", p, n))
		}
	}

	for _, tName := range covered {
		core.RegisterCoveredTable(tName)
	}

	v := core.NewPurgeRegistrationValidator()

	// All covered tables should report zero missing.
	missing := v.MissingTables(covered)
	assert.Empty(t, missing, "all registered tables should be covered")

	// Add an uncovered table.
	scoped := append(covered, "sys_new_uncovered_table")
	missing = v.MissingTables(scoped)
	require.Len(t, missing, 1)
	assert.Equal(t, "sys_new_uncovered_table", missing[0])
}
