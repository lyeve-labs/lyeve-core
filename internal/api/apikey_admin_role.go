package api

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// adminKeyDeprecationLink is the page that replaces an API key holding an
// admin role on the admin API.
const adminKeyDeprecationLink = `<https://docs.lyeve.com/how-to/admin-tokens/>; rel="deprecation"`

// adminKeyLogInterval bounds the warning to one line per key per interval,
// so a pipeline calling every second does not fill the log.
const adminKeyLogInterval = time.Hour

// adminKeyLogCap bounds the remembered keys. Past it the memory is reset,
// which can only repeat a warning early.
const adminKeyLogCap = 10000

// adminRoleKeyDeprecation refuses an API key that holds admin or super_admin
// on the admin API. Admin tokens are the credential there.
// The refusal is 401 with a Link to the admin tokens guide, and the engine
// logs the key id, its tenant and the route at most once an hour per key so
// an operator can find the automation still using one.
type adminRoleKeyDeprecation struct {
	pattern func(*http.Request) string
	now     func() time.Time
	logger  func() *slog.Logger

	mu     sync.Mutex
	logged map[string]time.Time
}

func newAdminRoleKeyDeprecation(pattern func(*http.Request) string) *adminRoleKeyDeprecation {
	return &adminRoleKeyDeprecation{
		pattern: pattern,
		now:     time.Now,
		logger:  slog.Default,
		logged:  make(map[string]time.Time),
	}
}

// isAdminRoleKey reports whether the caller is an API key holding an admin
// role. Sessions and admin tokens never are.
func isAdminRoleKey(c *core.AuthClaims) bool {
	if c == nil || c.AdminTokenID != "" || (!c.IsAPIKey && c.APIKeyID == "") {
		return false
	}
	return c.HasRole("admin") || c.HasRole("super_admin")
}

func (d *adminRoleKeyDeprecation) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := core.GetClaims(r.Context())
		if isAdminRoleKey(c) {
			w.Header().Add("Link", adminKeyDeprecationLink)
			if d.shouldLog(c.APIKeyID) {
				d.logger().Warn("refused an API key with an admin role on the admin API; it needs an admin token",
					"api_key_id", c.APIKeyID,
					"tenant_id", c.TenantID,
					"method", r.Method,
					"route", d.pattern(r))
			}
			httpx.Error(w, http.StatusUnauthorized, "API keys with an admin role are not accepted on the admin API. Use an admin token.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d *adminRoleKeyDeprecation) shouldLog(keyID string) bool {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.logged[keyID]; ok && now.Sub(last) < adminKeyLogInterval {
		return false
	}
	if len(d.logged) >= adminKeyLogCap {
		d.logged = make(map[string]time.Time)
	}
	d.logged[keyID] = now
	return true
}
