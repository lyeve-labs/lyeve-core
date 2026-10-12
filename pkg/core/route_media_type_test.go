package core

import "testing"

// A response media type names a body when it is a type and a subtype. The
// only ranges are type/* and */*, parameters are allowed, and the bare type
// comes back without them, lowercased as a Content-Type is compared.
func TestParseResponseMediaType_TypeAndSubtype(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"text/event-stream", "text/event-stream", true},
		{"text/csv; charset=utf-8", "text/csv", true},
		{"Application/X-NDJSON", "application/x-ndjson", true},
		{"application/samlmetadata+xml", "application/samlmetadata+xml", true},
		{"image/*", "image/*", true},
		{"*/*", "*/*", true},
		{"", "", false},
		{"csv", "", false},
		{"text/", "", false},
		{"*/json", "", false},
		{"text/csv; charset", "", false},
	} {
		got, ok := ParseResponseMediaType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseResponseMediaType(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
