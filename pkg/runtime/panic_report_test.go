package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type recordingReporter struct{ got []core.RecoveredPanic }

func (r *recordingReporter) ReportPanic(p core.RecoveredPanic) { r.got = append(r.got, p) }

func TestReportPanicsTo_HandsEveryFieldToTheRole(t *testing.T) {
	r := &recordingReporter{}
	reportPanicsTo(r)(panicreport.Panic{
		TenantID: "acme", Method: "GET", Route: "/api/v1/items/{id}",
		Goroutine: "", Value: "boom", Stack: []byte("goroutine 1 [running]:"),
	})

	assert.Equal(t, []core.RecoveredPanic{{
		TenantID: "acme", Method: "GET", Route: "/api/v1/items/{id}",
		Value: "boom", Stack: []byte("goroutine 1 [running]:"),
	}}, r.got)
}
