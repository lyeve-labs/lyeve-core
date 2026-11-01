package compliance

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// stalledAuditWriter stands in for a writer whose queue is full and no longer
// drains: each call waits until its context ends. syncCtx keeps the context
// WriterSync was handed.
type stalledAuditWriter struct {
	syncCtx context.Context
}

func (w *stalledAuditWriter) Writer(ctx context.Context, _ core.Host, _ AuditEntry) {
	<-ctx.Done()
}

func (w *stalledAuditWriter) WriterSync(ctx context.Context, _ core.Host, _ AuditEntry) error {
	w.syncCtx = ctx
	return nil
}

type callerKey struct{}

// An admin write that records an entry while the writer is stalled answers
// within a second, and the dropped entry is logged by action and tenant with
// nothing about its subject.
func TestRecordAudit_AsyncWriteToAStalledWriterReturnsWithinASecond(t *testing.T) {
	RegisterAuditWriter(&stalledAuditWriter{})
	t.Cleanup(ClearAuditWriter)
	var log bytes.Buffer
	host := unlicensedLoggingHost{logger: slog.New(slog.NewTextHandler(&log, nil))}
	ctx := core.WithTenantID(context.Background(), "tenant-a")

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		RecordAudit(ctx, host, "tenant.domain.create", "domain", "shop.example.com", "1.2.3.4", "curl/8")
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		assert.GreaterOrEqual(t, took, asyncAuditWait)
	case <-time.After(time.Second):
		t.Fatal("RecordAudit is still waiting on a stalled writer after a second")
	}
	assert.Contains(t, log.String(), "level=ERROR")
	assert.Contains(t, log.String(), "action=tenant.domain.create")
	assert.Contains(t, log.String(), "tenant_id=tenant-a")
	assert.NotContains(t, log.String(), "shop.example.com")
	assert.NotContains(t, log.String(), "1.2.3.4")
}

// A caller that set a shorter bound of its own keeps it.
func TestRecordAudit_AsyncWriteKeepsAShorterCallerDeadline(t *testing.T) {
	RegisterAuditWriter(&stalledAuditWriter{})
	t.Cleanup(ClearAuditWriter)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	RecordAudit(ctx, nil, "auth.login", "user", "u1", "1.2.3.4", "curl/8")

	assert.Less(t, time.Since(start), asyncAuditWait)
}

// A synchronous write waits as long as its caller allows, so it gets the
// caller's context with no deadline added.
func TestRecordAudit_SyncWriteKeepsTheCallerContext(t *testing.T) {
	w := &stalledAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)
	ctx := context.WithValue(context.Background(), callerKey{}, "caller")

	RecordAudit(ctx, nil, "auth.mfa.disable", "user", "u1", "1.2.3.4", "curl/8", WithSync())

	require.NotNil(t, w.syncCtx)
	assert.Equal(t, "caller", w.syncCtx.Value(callerKey{}))
	_, hasDeadline := w.syncCtx.Deadline()
	assert.False(t, hasDeadline)
}
