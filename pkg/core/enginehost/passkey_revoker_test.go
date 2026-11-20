package enginehost

import (
	"context"
	"errors"
	stdlog "log"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// countingRevoker removes count passkeys for every account it is asked about.
type countingRevoker struct {
	count int
	asked []uuid.UUID
}

func (r *countingRevoker) RevokePasskeys(_ context.Context, userID uuid.UUID) (int, error) {
	r.asked = append(r.asked, userID)
	return r.count, nil
}

// revokerSource stands in for the activator: what it answers changes as the
// plugin holding the role starts and stops.
type revokerSource struct{ current core.PasskeyRevoker }

func (s *revokerSource) PasskeyRevoker() core.PasskeyRevoker { return s.current }

// The host asks the source on every call, so a holder that starts after the
// caller, or stops, is seen by the next reset.
func TestEngineHost_RevokePasskeysAsksTheRunningHolder(t *testing.T) {
	src := &revokerSource{}
	h := &engineHost{}
	h.WithPasskeyRevokers(src)
	var host core.PasskeyRevoker = h
	user := uuid.New()

	holder := &countingRevoker{count: 3}
	src.current = holder
	n, err := host.RevokePasskeys(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []uuid.UUID{user}, holder.asked)

	src.current = failingRevoker{}
	_, err = host.RevokePasskeys(context.Background(), user)
	assert.Error(t, err, "the holder's failure reaches the caller")
}

type failingRevoker struct{}

func (failingRevoker) RevokePasskeys(context.Context, uuid.UUID) (int, error) {
	return 0, errors.New("passkey store unreachable")
}

// warningLog keeps each warning's message and the tenant its context files it
// under.
type warningLog struct {
	mu    sync.Mutex
	lines []warningLine
}

type warningLine struct{ msg, tenant string }

func (l *warningLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *warningLog) Handle(ctx context.Context, rec slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, warningLine{rec.Message, core.TenantIDFromCtx(ctx)})
	return nil
}

func (l *warningLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *warningLog) WithGroup(string) slog.Handler      { return l }

// With no running plugin that supplies passkey removal, a reset removes
// nothing and goes ahead, before the runtime wires a source and after. An
// older mfa may still run and store passkeys then, so the warning names the
// missing removal, not missing passkeys. The line is about the account, so it
// is filed under the account's tenant when the request acts there and under
// no tenant when it acts in another.
func TestEngineHost_RevokePasskeysWithNoSupplierRemovesNothingAndWarns(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.Postgres(t)
	account, err := db.NewUserStore(pool).Create(context.Background(), "revoker-"+uuid.NewString()[:8]+"@example.com", "x", []string{"editor"}, "globex")
	require.NoError(t, err)

	log := &warningLog{}
	prev, logOut, logFlags := slog.Default(), stdlog.Writer(), stdlog.Flags()
	slog.SetDefault(slog.New(log))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		stdlog.SetOutput(logOut)
		stdlog.SetFlags(logFlags)
	})

	unwired := &engineHost{pool: pool}
	wired := &engineHost{pool: pool}
	wired.WithPasskeyRevokers(&revokerSource{})
	for acting, h := range map[string]*engineHost{"globex": unwired, "acme": wired} {
		n, err := h.RevokePasskeys(core.WithTenantID(context.Background(), acting), account.ID)
		require.NoError(t, err)
		assert.Zero(t, n)
	}

	require.Len(t, log.lines, 2)
	tenants := map[string]bool{}
	for _, l := range log.lines {
		assert.Equal(t, "passkeys not removed: no running plugin supplies passkey removal", l.msg)
		tenants[l.tenant] = true
	}
	assert.Equal(t, map[string]bool{"globex": true, "": true}, tenants,
		"a line about the acting tenant's own account keeps it, one about another tenant's names none")
}
