package dialect

import "testing"

func TestPostgres_QuoteIdentifier(t *testing.T) {
	d := Postgres{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "articles", `"articles"`},
		{"underscore", "_my_table", `"_my_table"`},
		{"with digits", "col_123", `"col_123"`},
		{"embedded quote", `art"cles`, `"art""cles"`},
		{"injection attempt", `x"; DROP TABLE users --`, `"x""; DROP TABLE users --"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.QuoteIdentifier(tt.in)
			if got != tt.want {
				t.Errorf("QuoteIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMySQL_QuoteIdentifier(t *testing.T) {
	d := MySQL{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "articles", "`articles`"},
		{"underscore", "_my_table", "`_my_table`"},
		{"embedded backtick", "col`name", "`col``name`"},
		{"injection attempt", "x`; DROP TABLE users --", "`x``; DROP TABLE users --`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.QuoteIdentifier(tt.in)
			if got != tt.want {
				t.Errorf("QuoteIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMSSQL_QuoteIdentifier(t *testing.T) {
	d := MSSQL{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "articles", "[articles]"},
		{"underscore", "_my_table", "[_my_table]"},
		{"embedded bracket", "col]name", "[col]]name]"},
		{"injection attempt", "x]; DROP TABLE users --", "[x]]; DROP TABLE users --]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.QuoteIdentifier(tt.in)
			if got != tt.want {
				t.Errorf("QuoteIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
