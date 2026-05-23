package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDSAR_ControlCharIdentifierRejected pins the boundary check. A null byte in
// the identifier is not storable text on Postgres, so without this every
// registered eraser runs, fails on a driver encoding error, and logs it: one bad
// request turns into a failure per registered eraser and a response built from
// them. Reject it where it arrives instead.
func TestDSAR_ControlCharIdentifierRejected(t *testing.T) {
	h := NewDSARHandler(nil, nil, nil)

	routes := map[string]http.HandlerFunc{
		"erase":  h.Erase,
		"export": h.Export,
	}
	identifiers := map[string]string{
		"null byte": "user\x00@example.com",
		"newline":   "user\n@example.com",
		"delete":    "user\x7f@example.com",
	}

	for route, fn := range routes {
		for name, ident := range identifiers {
			t.Run(route+"/"+name, func(t *testing.T) {
				body := `{"identifier":` + quoteJSON(ident) + `}`
				req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/"+route, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()

				fn(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
				}
				if strings.Contains(rec.Body.String(), "SQLSTATE") {
					t.Errorf("response must not carry driver error text: %s", rec.Body.String())
				}
			})
		}
	}
}

// quoteJSON renders s as a JSON string literal, escaping control characters so
// the payload itself stays valid JSON.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteString("\\u")
			const hex = "0123456789abcdef"
			b.WriteByte('0')
			b.WriteByte('0')
			b.WriteByte(hex[(r>>4)&0xf])
			b.WriteByte(hex[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
