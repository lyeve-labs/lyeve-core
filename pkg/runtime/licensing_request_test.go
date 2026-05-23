package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// gatedFeature is a name the license below grants and an install with no
// license does not hold.
const gatedFeature = "feature-a"

// grantingManager is a licensing manager whose license grants a fixed set of
// names and withholds nothing from any tenant.
type grantingManager struct {
	licensing.Manager
	features []string
}

func (m grantingManager) Snapshot() licensing.Snapshot {
	return licensing.Snapshot{Plan: "plan-a", State: "active", Features: append([]string{}, m.features...), Caps: map[string]int{}}
}

func (grantingManager) Withholds(string, string) bool { return false }

// licensedHost is an engine host whose license grants features, wired the way
// the runtime wires the manager it loaded. A nil features is an install whose
// license grants nothing.
func licensedHost(t *testing.T, features []string) core.Host {
	t.Helper()
	h := enginehost.NewHost(db.NoDatabase(), nil, &config.Config{}, hooks.NewRegistry(), "test")
	setter, ok := h.(interface{ WithLicensing(licensing.Manager) })
	require.True(t, ok, "the engine host takes a licensing manager")
	setter.WithLicensing(grantingManager{features: features})
	return h
}

// A plugin middleware decides a gated capability by asking the host on the
// request. The probe stands in for one and records what it was told. The
// host answers from the license in force, so no middleware has to carry the
// license to it first, and the probe's place in the chain changes nothing.
func TestHasFeature_APluginMiddlewareSeesTheLicense(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	pool := testdb.Postgres(t)
	cfg := &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecret:        "test-secret-for-the-license-order",
		JWTSecrets:       []string{"test-secret-for-the-license-order"},
		JWTExpirySecs:    3600,
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
	}

	// told builds an admin router with the probe first or last in the extra
	// chain, sends one request, and reports what the host told the probe.
	told := func(host core.Host, first bool) bool {
		var granted bool
		probe := api.WithMiddleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				granted = host.HasFeature(r.Context(), gatedFeature)
				next.ServeHTTP(w, r)
			})
		})
		other := api.WithMiddleware(func(next http.Handler) http.Handler { return next })
		chain := []api.RouterOption{probe, other}
		if !first {
			chain = []api.RouterOption{other, probe}
		}
		router, err := api.NewAdminRouter(pool, cfg, chain...)
		require.NoError(t, err)
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/admin/rate-limits", nil))
		return granted
	}

	licensed := licensedHost(t, []string{gatedFeature})
	assert.True(t, told(licensed, true), "a plugin middleware in the extra chain must see the license")
	assert.True(t, told(licensed, false), "a plugin middleware last in the chain sees it as well")

	// The control: an install with no license is told no, so the answers above
	// came from the license and not from a probe that is always granted.
	assert.False(t, told(licensedHost(t, nil), true), "an install with no license is granted nothing gated")
}
