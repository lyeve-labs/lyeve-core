package provider

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// mockDB implements db.DB, with a knob per failure a caller wants to see.
type mockDB struct {
	name          string
	pingErr       error
	pingDelay     time.Duration
	connErr       error
	closeErr      error
	execErr       error
	beginErr      error
	engine        string
	pingCount     int
	mu            sync.Mutex
	pingCallbacks []func()
}

func newMockDB(name, engine string) *mockDB {
	return &mockDB{name: name, engine: engine}
}

func (m *mockDB) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	return nil, nil // not used in failure path tests
}
func (m *mockDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return nil, nil // not used in failure path tests
}
func (m *mockDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if m.execErr != nil {
		return nil, m.execErr
	}
	return nil, nil
}
func (m *mockDB) Begin(ctx context.Context) (*sql.Tx, error) {
	if m.beginErr != nil {
		return nil, m.beginErr
	}
	return nil, nil
}
func (m *mockDB) Conn(ctx context.Context) (*sql.Conn, error) {
	if m.connErr != nil {
		return nil, m.connErr
	}
	return nil, nil
}
func (m *mockDB) Ping(ctx context.Context) error {
	m.mu.Lock()
	m.pingCount++
	callbacks := m.pingCallbacks
	pingErr := m.pingErr // snapshot under lock: test may write concurrently
	pingDelay := m.pingDelay
	m.mu.Unlock()
	// Invoke callbacks (for triggering circuit breaker in tests)
	for _, cb := range callbacks {
		cb()
	}
	if pingDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pingDelay):
		}
	}
	return pingErr
}
func (m *mockDB) Close() error       { return m.closeErr }
func (m *mockDB) Stats() sql.DBStats { return sql.DBStats{} }
func (m *mockDB) Engine() string     { return m.engine }
func (m *mockDB) SQLDB() *sql.DB     { return nil }
func (m *mockDB) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) {
	return nil, nil
}

func (m *mockDB) PingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingCount
}

func (m *mockDB) SetPingCallback(cb func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingCallbacks = append(m.pingCallbacks, cb)
}

func (m *mockDB) SetPingErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingErr = err
}
