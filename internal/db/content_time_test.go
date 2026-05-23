package db

import (
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A datetime with no offset means one instant on every dialect: UTC. Bound
// as text, PostgreSQL would read it in the session's zone and MySQL and SQL
// Server as the wall clock, so it is bound as a UTC time.
func TestBindContentValue_DatetimeTextIsUTC(t *testing.T) {
	f := domain.SchemaField{Name: "at", FieldType: "datetime"}
	want := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	for _, in := range []string{
		"2026-10-01T09:30:00Z",
		"2026-10-01T11:30:00+02:00",
		"2026-10-01T09:30:00",
		"2026-10-01 09:30:00",
		"2026-10-01T09:30",
	} {
		got, ok := bindContentValue(f, in).(time.Time)
		if !ok || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("bind %q = %v, want %v in UTC", in, got, want)
		}
	}
	if got := bindContentValue(f, "not a time"); got != "not a time" {
		t.Errorf("unparseable text was changed to %v", got)
	}
	if got := bindContentValue(domain.SchemaField{Name: "t", FieldType: "text"}, "2026-10-01 09:30:00"); got != "2026-10-01 09:30:00" {
		t.Errorf("a text field was bound as a time: %v", got)
	}
}
