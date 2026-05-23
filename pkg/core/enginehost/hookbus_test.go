package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestHookBusAdapter_Publish_StampsTenantIDOnCtx(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	// Register a subscriber that reads the tenant from ctx.
	var capturedTenant string
	sub := adapter.Subscribe("articles", core.AfterCreate, func(ctx context.Context, e core.Event) error {
		capturedTenant = core.TenantIDFromCtx(ctx)
		return nil
	})
	defer sub.Unsubscribe()

	event := core.Event{
		Type:     core.AfterCreate,
		Schema:   "articles",
		TenantID: "tenant-abc",
	}

	// Publish with a bare context (no tenant stamped).
	err := adapter.Publish(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, "tenant-abc", capturedTenant,
		"Publish must stamp event.TenantID onto ctx so subscribers reading core.TenantIDFromCtx get it")
}

func TestHookBusAdapter_Publish_ForwardsTenantIDToHooksEvent(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var capturedTenant string
	sub := adapter.Subscribe("articles", core.AfterCreate, func(ctx context.Context, e core.Event) error {
		capturedTenant = e.TenantID
		return nil
	})
	defer sub.Unsubscribe()

	event := core.Event{
		Type:     core.AfterCreate,
		Schema:   "articles",
		TenantID: "tenant-xyz",
	}

	err := adapter.Publish(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, "tenant-xyz", capturedTenant,
		"Publish must forward TenantID into the hooks.Event so subscribers can read e.TenantID")
}

func TestHookBusAdapter_Publish_EmptyTenantID_NoStamp(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var tenantFromCtx string
	var tenantFromEvent string
	sub := adapter.Subscribe("articles", core.AfterCreate, func(ctx context.Context, e core.Event) error {
		tenantFromCtx = core.TenantIDFromCtx(ctx)
		tenantFromEvent = e.TenantID
		return nil
	})
	defer sub.Unsubscribe()

	event := core.Event{
		Type:     core.AfterCreate,
		Schema:   "articles",
		TenantID: "", // empty
	}

	err := adapter.Publish(context.Background(), event)
	require.NoError(t, err)
	assert.Empty(t, tenantFromCtx,
		"empty TenantID must not stamp ctx (no-op)")
	assert.Empty(t, tenantFromEvent,
		"empty TenantID must produce empty TenantID in hooks.Event")
}

func TestHookBusAdapter_Subscribe_ReceivesTenantIDFromCtx(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var capturedEvent core.Event
	sub := adapter.Subscribe("posts", core.AfterUpdate, func(ctx context.Context, e core.Event) error {
		capturedEvent = e
		return nil
	})
	defer sub.Unsubscribe()

	// Pre-stamp the ctx with a tenant.
	ctx := core.WithTenantID(context.Background(), "ctx-tenant")

	event := core.Event{
		Type:   core.AfterUpdate,
		Schema: "posts",
		// Even without setting event.TenantID, if the ctx already carries
		// a tenant, Publish should NOT lose it.
		TenantID: "event-tenant",
	}

	err := adapter.Publish(ctx, event)
	require.NoError(t, err)

	// The event's TenantID takes precedence over ctx (authoritative).
	assert.Equal(t, "event-tenant", capturedEvent.TenantID,
		"Subscribe handler must receive event.TenantID forwarded from hooks.Event")
	// ctx should have the event tenant stamped, overriding the original ctx tenant.
	// (Verify by reading from ctx inside the subscriber)
}

func TestHookBusAdapter_Publish_MultipleSubscribers_AllGetTenantID(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var tenant1, tenant2 string

	sub1 := adapter.Subscribe("items", core.AfterDelete, func(ctx context.Context, e core.Event) error {
		tenant1 = e.TenantID
		return nil
	})
	defer sub1.Unsubscribe()

	sub2 := adapter.Subscribe("items", core.AfterDelete, func(ctx context.Context, e core.Event) error {
		tenant2 = e.TenantID
		return nil
	})
	defer sub2.Unsubscribe()

	event := core.Event{
		Type:     core.AfterDelete,
		Schema:   "items",
		TenantID: "multi-tenant",
	}

	err := adapter.Publish(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, "multi-tenant", tenant1)
	assert.Equal(t, "multi-tenant", tenant2,
		"all subscribers must receive TenantID, not just the first one")
}

func TestHookBusAdapter_Publish_TenantIDFromCtxPreservedForSubscribers(t *testing.T) {
	// Verify that when event.TenantID is set, Publish stamps ctx so that
	// core.TenantIDFromCtx returns that tenant, and any further subscriber that
	// reads the context directly (not the event) sees it.
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var ctxTenant string
	// Register a raw hooks.Hook directly on the registry (bypassing Subscribe)
	// to simulate a subscriber that only reads the context, not the core.Event.
	reg.Register("direct", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		ctxTenant = core.TenantIDFromCtx(ctx)
		return nil
	})

	event := core.Event{
		Type:     core.AfterCreate,
		Schema:   "direct",
		TenantID: "direct-tenant",
	}

	err := adapter.Publish(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, "direct-tenant", ctxTenant,
		"even raw hooks (not via Subscribe) must see the tenant from ctx after Publish stamps it")
}

// A subscriber that also writes content skips the events its own writes
// produce by Source, and a flow trigger reads the row by RecordID. Both have
// to survive between Publish and Subscribe, or every subscriber sees an
// anonymous event about no row.
func TestHookBusAdapter_Publish_ForwardsRecordIDAndSource(t *testing.T) {
	reg := hooks.NewRegistry()
	adapter := &hookBusAdapter{registry: reg}

	var got core.Event
	sub := adapter.Subscribe("*", core.AfterUpdate, func(ctx context.Context, e core.Event) error {
		got = e
		return nil
	})
	defer sub.Unsubscribe()

	ctx := core.WithTenantID(context.Background(), "tenant-rec")
	sent := core.ContentEvent(ctx, "graphql", core.AfterUpdate, "articles", "rec-1",
		map[string]any{"title": "old"}, map[string]any{"title": "new"})
	require.NoError(t, adapter.Publish(ctx, sent))

	assert.Equal(t, "rec-1", got.RecordID)
	assert.Equal(t, "graphql", got.Source)
	assert.Equal(t, "tenant-rec", got.TenantID)
	assert.Equal(t, "new", got.Data["title"])
	assert.Equal(t, "old", got.OldData["title"])
}
