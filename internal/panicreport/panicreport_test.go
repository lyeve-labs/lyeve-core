package panicreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReport_HandsThePanicToTheReporter(t *testing.T) {
	var got []Panic
	Set(func(p Panic) { got = append(got, p) })
	t.Cleanup(func() { Set(nil) })

	want := Panic{TenantID: "acme", Method: "GET", Route: "/api/v1/boom", Value: "boom", Stack: []byte("stack")}
	Report(want)
	assert.Equal(t, []Panic{want}, got)
}

func TestReport_NoReporterIsANoOp(t *testing.T) {
	Set(nil)
	assert.NotPanics(t, func() { Report(Panic{Goroutine: "worker", Value: "boom"}) })
}

// Report runs inside a recover, so a reporter that panics must not raise a
// second panic there.
func TestReport_APanickingReporterIsContained(t *testing.T) {
	Set(func(Panic) { panic("reporter broke") })
	t.Cleanup(func() { Set(nil) })

	assert.NotPanics(t, func() { Report(Panic{Goroutine: "worker", Value: "boom"}) })
}
