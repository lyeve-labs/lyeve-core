package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// stampedRecord is one log line as a tenant's log stream sees it: the tenant
// the engine stamped on it and the account it names.
type stampedRecord struct {
	message string
	tenant  string
	userID  string
}

// stampRecorder sits under the engine's enrich handler, so it sees the
// tenant_id the stream filters on, taken from the logging context.
type stampRecorder struct {
	mu      *sync.Mutex
	records *[]stampedRecord
}

func (s stampRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (s stampRecorder) WithAttrs([]slog.Attr) slog.Handler       { return s }
func (s stampRecorder) WithGroup(string) slog.Handler            { return s }

func (s stampRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := stampedRecord{message: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "tenant_id":
			rec.tenant = a.Value.String()
		case "user_id":
			rec.userID = a.Value.String()
		}
		return true
	})
	s.mu.Lock()
	*s.records = append(*s.records, rec)
	s.mu.Unlock()
	return nil
}

// captureStampedLogs routes the default logger through the enrich handler
// into a recorder and returns a reader for the records naming userID.
func captureStampedLogs(t *testing.T) func(userID uuid.UUID) []stampedRecord {
	t.Helper()
	var mu sync.Mutex
	var records []stampedRecord
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewEnrichHandler(stampRecorder{mu: &mu, records: &records}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(userID uuid.UUID) []stampedRecord {
		mu.Lock()
		defer mu.Unlock()
		var out []stampedRecord
		for _, r := range records {
			if r.userID == userID.String() {
				out = append(out, r)
			}
		}
		return out
	}
}

// deviceAttempt is one failed attempt the device history was handed, with
// the tenant its row would be stamped with.
type deviceAttempt struct {
	tenant string
	userID uuid.UUID
}

// recordingRiskAssessor keeps the tenant of every login attempt it records.
// The device history stores each attempt under the tenant on its context. A
// failed sign-in calls nothing else on it.
type recordingRiskAssessor struct {
	security.DeviceRiskAssessor
	mu       sync.Mutex
	attempts []deviceAttempt
}

func (a *recordingRiskAssessor) RecordLogin(ctx context.Context, userID uuid.UUID, _ security.DeviceFingerprint, _ bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = append(a.attempts, deviceAttempt{tenant: core.TenantIDFromCtx(ctx), userID: userID})
	return nil
}

func (a *recordingRiskAssessor) tenantsFor(userID uuid.UUID) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, at := range a.attempts {
		if at.userID == userID {
			out = append(out, at.tenant)
		}
	}
	return out
}

// A wrong password on one tenant's domain, for an address whose account lives
// in another tenant, leaves nothing that tenant can read: every log line and
// device history row it writes is stamped with the account's own tenant,
// through the lockout that follows. The domain's own accounts still record
// under the domain's tenant, and the instance host is unchanged.
func TestPasswordLogin_FailureOnAnotherTenantsDomainIsRecordedUnderTheAccountsTenant(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		hash, err := auth.HashPassword("bcrypt", providerTestPassword)
		require.NoError(t, err)
		h := NewAuthHandler(users, nil, pool, "test-secret-at-least-32-bytes-long!!", 3600, false)
		devices := &recordingRiskAssessor{}
		h.WithDeviceRiskAssessor(devices)
		logsFor := captureStampedLogs(t)

		account := func(tenant string) (uuid.UUID, string) {
			u, err := users.Create(ctx, "signin-"+uuid.NewString()[:8]+"@"+tenant+".example", hash, []string{"editor"}, tenant)
			require.NoError(t, err)
			return u.ID, u.Email
		}

		doors := map[string]func(http.ResponseWriter, *http.Request){"login": h.Login, "token": h.Token}
		for doorName, door := range doors {
			// One attempt past the limit, so the lockout line is written too.
			failAll := func(address, hostTenant string) {
				for i := 0; i <= h.MaxFailedAttempts; i++ {
					body := fmt.Sprintf(`{"email":%q,"password":"wrong-password"}`, address)
					req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					if hostTenant != "" {
						req = req.WithContext(core.WithTenantID(req.Context(), hostTenant))
					}
					rec := httptest.NewRecorder()
					door(rec, req)
					require.Equal(t, http.StatusUnauthorized, rec.Code, "%s attempt %d", doorName, i)
				}
			}
			assertStamped := func(id uuid.UUID, want, scenario string) {
				records := logsFor(id)
				var failed, locked int
				for _, r := range records {
					assert.Equal(t, want, r.tenant, "%s, %s: %q", doorName, scenario, r.message)
					if strings.Contains(r.message, "password did not verify") {
						failed++
					}
					if strings.Contains(r.message, "locked out") {
						locked++
					}
				}
				assert.Equal(t, h.MaxFailedAttempts, failed, "%s, %s: failed attempts logged", doorName, scenario)
				assert.Equal(t, 1, locked, "%s, %s: lockout lines logged", doorName, scenario)
				tenants := devices.tenantsFor(id)
				assert.Len(t, tenants, h.MaxFailedAttempts, "%s, %s: device history rows", doorName, scenario)
				for _, got := range tenants {
					assert.Equal(t, want, got, "%s, %s: device history tenant", doorName, scenario)
				}
			}

			foreign, foreignAddress := account("acme")
			failAll(foreignAddress, "globex")
			assertStamped(foreign, "acme", "an acme account on globex's domain")

			own, ownAddress := account("globex")
			failAll(ownAddress, "globex")
			assertStamped(own, "globex", "a globex account on globex's domain")

			instance, instanceAddress := account("acme")
			failAll(instanceAddress, "")
			assertStamped(instance, "", "an acme account on the instance host")
		}
	})
}
