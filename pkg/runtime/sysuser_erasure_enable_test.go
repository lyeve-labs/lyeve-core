package runtime

import (
	"context"
	"database/sql"
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

// An erasure locks the account when it signs the subject out and anonymizes
// it only once every other eraser has succeeded. An administrator who enabled
// the account in between let the person sign back in and write rows behind
// the erasers that had already run. The enable is refused until the erasure
// completes, and an administrator's own disable and enable work as before.

// accountFixture is one account with a known password, and the two requests
// these tests make against it.
type accountFixture struct {
	pool  db.DB
	id    uuid.UUID
	email string
}

const fixturePassword = "correct-horse-battery"

func newAccountFixture(t *testing.T, pool db.DB, prefix string) accountFixture {
	t.Helper()
	hash, err := auth.HashPassword("bcrypt", fixturePassword)
	require.NoError(t, err)
	a := accountFixture{pool: pool, id: uuid.New(), email: prefix + "-" + uuid.NewString()[:8] + "@example.com"}
	_, err = pool.Exec(context.Background(),
		`INSERT INTO sys_users (id, email, password_hash) VALUES ($1, $2, $3)`, a.id, a.email, hash)
	require.NoError(t, err)
	return a
}

func (a accountFixture) signIn() int {
	h := api.NewAuthHandler(db.NewUserStore(a.pool), nil, a.pool, "test-secret-at-least-32-bytes-long!!", 3600, false)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
		strings.NewReader(`{"email":"`+a.email+`","password":"`+fixturePassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)
	return rr.Code
}

// setState calls PUT /api/admin/users/{id}/state with body.
func (a accountFixture) setState(body string) *httptest.ResponseRecorder {
	h := api.NewUsersHandler(db.NewUserStore(a.pool))
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+a.id.String()+"/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.id.String())
	rr := httptest.NewRecorder()
	h.UpdateState(rr, req)
	return rr
}

// remove calls DELETE /api/admin/users/{id}.
func (a accountFixture) remove() *httptest.ResponseRecorder {
	h := api.NewUsersHandler(db.NewUserStore(a.pool))
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+a.id.String(), nil)
	req.SetPathValue("id", a.id.String())
	rr := httptest.NewRecorder()
	h.Delete(rr, req)
	return rr
}

// marked reports whether the account still exists and carries the erasure
// marker.
func (a accountFixture) marked(t *testing.T) (exists, pending bool) {
	t.Helper()
	row, err := a.pool.QueryRow(context.Background(),
		`SELECT CASE WHEN erasure_pending_at IS NULL THEN 0 ELSE 1 END FROM sys_users WHERE id = $1`, a.id)
	require.NoError(t, err)
	var n int
	err = row.Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return true, n == 1
}

func (a accountFixture) disabled(t *testing.T) bool {
	t.Helper()
	row, err := a.pool.QueryRow(context.Background(), `SELECT disabled FROM sys_users WHERE id = $1`, a.id)
	require.NoError(t, err)
	var disabled bool
	require.NoError(t, row.Scan(&disabled))
	return disabled
}

func TestErasure_AdminEnableLeavesThePendingLock_Postgres(t *testing.T) {
	testErasureAdminEnableLeavesThePendingLock(t, testdb.Postgres(t))
}

func TestErasure_AdminEnableLeavesThePendingLock_MySQL(t *testing.T) {
	testErasureAdminEnableLeavesThePendingLock(t, testdb.MySQL(t))
}

func TestErasure_AdminEnableLeavesThePendingLock_MSSQL(t *testing.T) {
	testErasureAdminEnableLeavesThePendingLock(t, testdb.MSSQL(t))
}

func testErasureAdminEnableLeavesThePendingLock(t *testing.T, pool db.DB) {
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

	a := newAccountFixture(t, pool, "pending")
	require.Equal(t, http.StatusOK, a.signIn(), "the account signs in before the erasure")

	_, err := compliance.RunSubjectErasureWithHolds(ctx, a.id.String())
	require.Error(t, err, "one eraser fails, so the erasure is incomplete")

	rr := a.setState(`{"disabled":false}`)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.True(t, a.disabled(t), "the enable leaves the account locked")
	assert.Equal(t, http.StatusUnauthorized, a.signIn(), "the subject still cannot sign in")

	broken.Store(false)
	_, err = compliance.RunSubjectErasureWithHolds(ctx, a.id.String())
	require.NoError(t, err)
	row, err := pool.QueryRow(ctx, `SELECT anonymized, erasure_pending_at FROM sys_users WHERE id = $1`, a.id)
	require.NoError(t, err)
	var anonymized bool
	var pending sql.NullTime
	require.NoError(t, row.Scan(&anonymized, &pending))
	assert.True(t, anonymized)
	assert.False(t, pending.Valid, "anonymization clears the marker")
}

// Deleting the account removed the marker with it. A new account for the
// same person then signed in unmarked, and the erasure's retry, which runs by
// the old id, found no address to erase the rest by.
func TestErasure_AdminDeleteLeavesThePendingAccount_Postgres(t *testing.T) {
	testErasureAdminDeleteLeavesThePendingAccount(t, testdb.Postgres(t))
}

func TestErasure_AdminDeleteLeavesThePendingAccount_MySQL(t *testing.T) {
	testErasureAdminDeleteLeavesThePendingAccount(t, testdb.MySQL(t))
}

func TestErasure_AdminDeleteLeavesThePendingAccount_MSSQL(t *testing.T) {
	testErasureAdminDeleteLeavesThePendingAccount(t, testdb.MSSQL(t))
}

func testErasureAdminDeleteLeavesThePendingAccount(t *testing.T, pool db.DB) {
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

	a := newAccountFixture(t, pool, "pending-delete")
	_, err := compliance.RunSubjectErasureWithHolds(ctx, a.id.String())
	require.Error(t, err, "one eraser fails, so the erasure is incomplete")

	rr := a.remove()
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	exists, pending := a.marked(t)
	assert.True(t, exists, "the delete leaves the account in place")
	assert.True(t, pending, "the account keeps the erasure's marker")
	if exists {
		assert.True(t, a.disabled(t))
	}
}

func TestUpdateState_DisableAndEnableWithNoErasurePending_Postgres(t *testing.T) {
	testUpdateStateDisableAndEnableWithNoErasurePending(t, testdb.Postgres(t))
}

func TestUpdateState_DisableAndEnableWithNoErasurePending_MySQL(t *testing.T) {
	testUpdateStateDisableAndEnableWithNoErasurePending(t, testdb.MySQL(t))
}

func TestUpdateState_DisableAndEnableWithNoErasurePending_MSSQL(t *testing.T) {
	testUpdateStateDisableAndEnableWithNoErasurePending(t, testdb.MSSQL(t))
}

func testUpdateStateDisableAndEnableWithNoErasurePending(t *testing.T, pool db.DB) {
	t.Helper()
	a := newAccountFixture(t, pool, "toggle")

	rr := a.setState(`{"disabled":true}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, a.disabled(t))
	assert.Equal(t, http.StatusUnauthorized, a.signIn(), "a disabled account cannot sign in")

	rr = a.setState(`{"disabled":false}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.False(t, a.disabled(t))
	assert.Equal(t, http.StatusOK, a.signIn(), "an enabled account signs in again")

	rr = a.remove()
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	exists, _ := a.marked(t)
	assert.False(t, exists, "an account with no erasure pending is deleted")
}
