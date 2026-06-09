package compliance

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// unlicensedLoggingHost logs where the test can read it and refuses every
// license feature.
type unlicensedLoggingHost struct {
	core.Host
	logger *slog.Logger
}

func (h unlicensedLoggingHost) Logger(context.Context) *slog.Logger { return h.logger }

func (h unlicensedLoggingHost) HasFeature(context.Context, string) bool { return false }

// An entry with no writer to take it is dropped, and the operator can find
// that in the debug log whatever the license grants.
func TestRecordAudit_LogsADroppedEntryWhateverTheLicense(t *testing.T) {
	ClearAuditWriter()
	var log bytes.Buffer
	host := unlicensedLoggingHost{logger: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	RecordAudit(context.Background(), host, "apikey.create", "api_key", "k1", "1.2.3.4", "curl/8")

	assert.Contains(t, log.String(), "audit entry dropped: no writer registered")
	assert.Contains(t, log.String(), "action=apikey.create")
}
