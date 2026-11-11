package enginehost

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin reaches the mark only through its scoped host, which forwards it by
// type assertion. A request marked through that chain must carry the same mark
// the listeners recognize, or every loopback call would share the engine's own
// rate limit bucket.
func TestScopedHost_MarkLoopbackOverEngineHostSetsTheEngineMark(t *testing.T) {
	host := NewHost(db.NoDatabase(), nil, nil, hooks.NewRegistry(), "test")
	scoped := core.NewScopedHost(host, "mcp", core.CapDBWrite|core.CapRawDB|core.CapRoutes)

	want := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	apimw.MarkLoopback(want)
	require.NotEmpty(t, want.Header.Get(apimw.LoopbackHeader))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	scoped.MarkLoopback(req)
	assert.Equal(t, want.Header.Get(apimw.LoopbackHeader), req.Header.Get(apimw.LoopbackHeader))
}
