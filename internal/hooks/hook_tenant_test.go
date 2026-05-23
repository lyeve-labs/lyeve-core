package hooks

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The content handlers build a hook event from what they have in hand, which
// does not include the tenant: that lives on the request context. A subscriber
// that fans out per tenant (webhook dispatch, the message broker's topic) has
// nothing else to go on, so an empty one is not a missing detail, it is every
// tenant's events arriving indistinguishable.
func TestRegistry_Run_FillsTenantFromContext(t *testing.T) {
	t.Parallel()

	t.Run("an event with no tenant takes the context's", func(t *testing.T) {
		reg := NewRegistry()
		var seen string
		reg.Register("*", AfterCreate, func(_ context.Context, e Event) error {
			seen = e.TenantID
			return nil
		})

		ctx := core.WithTenantID(context.Background(), "acme")
		if err := reg.Run(ctx, Event{Type: AfterCreate, Schema: "posts"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if seen != "acme" {
			t.Errorf("TenantID = %q, want %q", seen, "acme")
		}
	})

	t.Run("an event that names its own tenant keeps it", func(t *testing.T) {
		reg := NewRegistry()
		var seen string
		reg.Register("*", AfterUpdate, func(_ context.Context, e Event) error {
			seen = e.TenantID
			return nil
		})

		ctx := core.WithTenantID(context.Background(), "acme")
		err := reg.Run(ctx, Event{Type: AfterUpdate, Schema: "posts", TenantID: "explicit"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if seen != "explicit" {
			t.Errorf("TenantID = %q, want the event's own", seen)
		}
	})

	t.Run("no tenant on the context leaves it empty", func(t *testing.T) {
		reg := NewRegistry()
		var seen = "unset"
		reg.Register("*", AfterDelete, func(_ context.Context, e Event) error {
			seen = e.TenantID
			return nil
		})

		if err := reg.Run(context.Background(), Event{Type: AfterDelete, Schema: "posts"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if seen != "" {
			t.Errorf("TenantID = %q, want empty", seen)
		}
	})
}
