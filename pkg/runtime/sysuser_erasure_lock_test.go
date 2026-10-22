package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// The erasure signs the subject out first and erases the account only once
// every other owner has erased. Signing out alone let the subject sign
// straight back in and write rows behind an eraser that had already run, and
// an owner whose eraser kept failing left the account usable for good. The
// sign-out now locks the account until the erasure completes.

// switchableEraser fails while broken is set.
type switchableEraser struct{ broken *atomic.Bool }

func (e switchableEraser) EraseSubject(context.Context, string) (int64, error) {
	if e.broken.Load() {
		return 0, errors.New("store is down")
	}
	return 0, nil
}

func (switchableEraser) EraserName() string { return "forms" }

func TestErasure_LocksTheAccountUntilItCompletes_Postgres(t *testing.T) {
	testErasureLocksTheAccountUntilItCompletes(t, testdb.Postgres(t))
}

func TestErasure_LocksTheAccountUntilItCompletes_MySQL(t *testing.T) {
	testErasureLocksTheAccountUntilItCompletes(t, testdb.MySQL(t))
}

func TestErasure_LocksTheAccountUntilItCompletes_MSSQL(t *testing.T) {
	testErasureLocksTheAccountUntilItCompletes(t, testdb.MSSQL(t))
}

func testErasureLocksTheAccountUntilItCompletes(t *testing.T, pool db.DB) {
	t.Helper()
	ctx := context.Background()
	host := &revokingHost{Host: testhost.New(pool)}
	compliance.ResetSubjectErasers()
	t.Cleanup(compliance.ResetSubjectErasers)
	compliance.ResetHoldChecker()
	t.Cleanup(compliance.ResetHoldChecker)
	var broken atomic.Bool
	broken.Store(true)
	compliance.RegisterSubjectEraser(newSysUserEraser(host))
	compliance.RegisterSubjectEraser(switchableEraser{broken: &broken})

	const password = "correct-horse-battery"
	hash, err := auth.HashPassword("bcrypt", password)
	require.NoError(t, err)
	id := uuid.New()
	email := "locked-" + uuid.NewString()[:8] + "@example.com"
	_, err = pool.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash) VALUES ($1, $2, $3)`, id, email, hash)
	require.NoError(t, err)

	signIn := func() int {
		h := api.NewAuthHandler(db.NewUserStore(pool), nil, pool, "test-secret-at-least-32-bytes-long!!", 3600, false)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
			strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)
		return rr.Code
	}
	require.Equal(t, http.StatusOK, signIn(), "the account signs in before the erasure")

	res, err := compliance.RunSubjectErasureWithHolds(ctx, id.String())
	require.Error(t, err)
	assert.Contains(t, res.Failed, "forms")
	assert.Equal(t, http.StatusUnauthorized, signIn(), "the subject cannot sign back in while the erasure is incomplete")

	broken.Store(false)
	res, err = compliance.RunSubjectErasureWithHolds(ctx, id.String())
	require.NoError(t, err)
	assert.Empty(t, res.Failed)
	row, err := pool.QueryRow(ctx, `SELECT anonymized, email FROM sys_users WHERE id = $1`, id)
	require.NoError(t, err)
	var anonymized bool
	var gotEmail string
	require.NoError(t, row.Scan(&anonymized, &gotEmail))
	assert.True(t, anonymized, "the retry finds the locked account and anonymizes it")
	assert.NotEqual(t, email, gotEmail)
}
