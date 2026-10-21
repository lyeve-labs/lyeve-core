package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A handler registered for every delete is still one of the handlers, in its
// place, and is the only one the every-delete listing returns.
func TestTenantPurgeHandlersOnEveryDelete_ListsOnlyThoseRegisteredForIt(t *testing.T) {
	ResetTenantPurgeHandlers()
	t.Cleanup(ResetTenantPurgeHandlers)

	var ran []string
	handler := func(name string) TenantPurgeHandler {
		return func(context.Context, Querier, string) error {
			ran = append(ran, name)
			return nil
		}
	}
	RegisterTenantPurgeHandler(handler("rows"))
	RegisterTenantPurgeHandlerOnEveryDelete(handler("buffer"))
	RegisterTenantPurgeHandler(handler("more rows"))

	for _, h := range TenantPurgeHandlers() {
		assert.NoError(t, h(context.Background(), nil, "acme"))
	}
	assert.Equal(t, []string{"rows", "buffer", "more rows"}, ran, "every handler runs on a full purge, in order")

	ran = nil
	for _, h := range TenantPurgeHandlersOnEveryDelete() {
		assert.NoError(t, h(context.Background(), nil, "acme"))
	}
	assert.Equal(t, []string{"buffer"}, ran)

	ResetTenantPurgeHandlers()
	assert.Empty(t, TenantPurgeHandlersOnEveryDelete())
	assert.Zero(t, TenantPurgeHandlerCount())
}
