package compliance

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// An export names its subject by the identifier the operator typed, and a log
// line is stored and kept longer than the bundle. A failed exporter has to be
// findable without that line carrying the address, so it carries the digest
// the erasure lines carry and the two runs still correlate.
func TestSubjectExport_LogsADigestNotTheIdentifier(t *testing.T) {
	const identifier = "jane.doe@example.com"

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"across tenants", func() error {
			_, err := RunSubjectExportAcrossTenants(context.Background(),
				identifier, []string{"acme", "globex"}, passthroughScope)
			return err
		}},
		{"one tenant", func() error {
			_, err := RunSubjectExport(core.WithTenantID(context.Background(), "acme"), identifier)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRegistry(t)
			t.Cleanup(func() { resetRegistry(t) })
			RegisterSubjectExporter(&tenantScopedExporter{
				section: "email",
				failFor: map[string]bool{"acme": true},
			})

			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(restore) })

			require.NoError(t, tc.run())

			out := buf.String()
			if !strings.Contains(out, "subject_export_failed") {
				t.Fatalf("the failed exporter was not logged: %s", out)
			}
			if strings.Contains(out, identifier) {
				t.Errorf("the export logged the identifier it was asked about: %s", out)
			}
			if !strings.Contains(out, SubjectRef(identifier)) {
				t.Errorf("the export line carries no subject reference to correlate it: %s", out)
			}
		})
	}
}
