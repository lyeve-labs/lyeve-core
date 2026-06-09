//go:build integration && !mutest

package compliance_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// auditTableDDL declares the audit plugin's table with the column the tests
// read. This binary links no plugin, so nothing else creates it, and the test
// that the engine leaves the table alone needs a table to leave alone.
func auditTableDDL(dialect string) []string {
	switch dialect {
	case "postgres":
		return []string{`CREATE TABLE IF NOT EXISTS sys_audit_log (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), resource_id TEXT NULL)`}
	case "mysql":
		return []string{"CREATE TABLE IF NOT EXISTS sys_audit_log (`id` CHAR(36) PRIMARY KEY DEFAULT (UUID()), `resource_id` TEXT NULL)"}
	case "mssql":
		return []string{`IF OBJECT_ID(N'sys_audit_log', N'U') IS NULL CREATE TABLE sys_audit_log (id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(), resource_id NVARCHAR(MAX) NULL)`}
	}
	return nil
}

func init() {
	plugintest.RegisterTestDDL("audit", auditTableDDL)
}

// captureWriter stands in for the audit plugin. The engine hands an entry to
// whatever writer is registered and never touches the table itself, so this
// is where the engine's half of the contract ends and the only place to
// observe it.
type captureWriter struct {
	mu      sync.Mutex
	entries []compliance.AuditEntry
}

func (c *captureWriter) Writer(_ context.Context, _ core.Host, entry compliance.AuditEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
}

func (c *captureWriter) WriterSync(ctx context.Context, host core.Host, entry compliance.AuditEntry) error {
	c.Writer(ctx, host, entry)
	return nil
}

func (c *captureWriter) find(resourceID string) (compliance.AuditEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.ResourceID == resourceID {
			return e, true
		}
	}
	return compliance.AuditEntry{}, false
}

// registerCapture installs the writer for one test and takes it out again.
// The registry is global, so leaving one behind changes what the next test
// sees.
func registerCapture(t *testing.T) *captureWriter {
	t.Helper()
	w := &captureWriter{}
	compliance.RegisterAuditWriter(w)
	t.Cleanup(compliance.ClearAuditWriter)
	return w
}

// The engine owes the entry, correctly filled, handed to the registered
// writer. It never writes the table itself, because the writer may chain every
// row to the one before it. That is what these assert.
func TestRecordAudit_HandsTheEntryToTheRegisteredWriter(t *testing.T) {
	host := plugintest.Postgres(t)
	w := registerCapture(t)
	ctx := context.Background()

	userID := uuid.New()
	claims := &core.AuthClaims{
		UserID:   userID.String(),
		TenantID: "test-tenant",
		Roles:    []string{"admin"},
	}
	resourceID := uuid.New()

	compliance.RecordAudit(context.WithValue(ctx, core.ClaimsKey, claims), host,
		"apikey.create", "api_key", resourceID.String(), "127.0.0.1", "test-agent")

	entry, ok := w.find(resourceID.String())
	require.True(t, ok, "expected exactly one entry for apikey.create")
	assert.Equal(t, "apikey.create", entry.Action)
	assert.Equal(t, "api_key", entry.ResourceType)
	assert.Equal(t, resourceID.String(), entry.ResourceID)
	assert.Equal(t, "127.0.0.1", entry.IP)
	assert.Equal(t, "test-tenant", entry.TenantID)

	uid, isUUID := entry.UserID.(*uuid.UUID)
	require.True(t, isUUID, "the actor is carried as a *uuid.UUID")
	assert.Equal(t, userID, *uid)
}

// With no writer registered the engine writes nothing at all. The table
// belongs to the plugin, and a row written around it breaks the chain the
// plugin verifies, which reads as tampering.
func TestRecordAudit_WritesNothingWithNoWriter(t *testing.T) {
	host := plugintest.Postgres(t)
	compliance.ClearAuditWriter()
	ctx := context.Background()

	resourceID := uuid.New()
	compliance.RecordAudit(ctx, host, "apikey.create", "api_key",
		resourceID.String(), "127.0.0.1", "test-agent")

	var count int
	row, err := host.Querier(ctx).QueryRow(ctx,
		`SELECT COUNT(*) FROM sys_audit_log WHERE resource_id = $1`, resourceID.String())
	require.NoError(t, err)
	require.NoError(t, row.Scan(&count))
	assert.Zero(t, count, "the engine must not write the plugin's table")
}
