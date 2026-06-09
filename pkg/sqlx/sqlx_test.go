package sqlx

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Identifier quoting

func TestQuotePG(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"simple", "hello", `"hello"`},
		{"with double quote", `say "hi"`, `"say ""hi"""`},
		{"leading quote", `"start`, `"""start"`},
		{"trailing quote", `end"`, `"end"""`},
		{"all quotes", `"""`, `""""""""`},
		{"empty", "", `""`},
		{"schema qualified", `public.users`, `"public.users"`},
		{"mixed case", `MyTable`, `"MyTable"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuotePG(tt.input))
		})
	}
}

func TestQuoteMySQL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"simple", "hello", "`hello`"},
		{"with backtick", "say `hi`", "`say ``hi```"},
		{"leading backtick", "`start", "```start`"},
		{"empty", "", "``"},
		{"reserved word", "select", "`select`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuoteMySQL(tt.input))
		})
	}
}

func TestQuoteMSSQL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"simple", "hello", "[hello]"},
		{"with bracket", "say [hi]", "[say [hi]]]"},
		{"leading bracket", "[start", "[[start]"},
		{"empty", "", "[]"},
		{"spaces", "my table", "[my table]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuoteMSSQL(tt.input))
		})
	}
}

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		name, input, dialect, want string
	}{
		{"pg", "foo", "postgres", `"foo"`},
		{"mysql", "foo", "mysql", "`foo`"},
		{"mssql", "foo", "mssql", "[foo]"},
		{"sqlserver alias", "foo", "sqlserver", "[foo]"},
		{"unknown falls to pg", "foo", "oracle", `"foo"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuoteIdent(tt.input, tt.dialect))
		})
	}
}

// PostgreSQL array parsing

func TestParsePGArray(t *testing.T) {
	tests := []struct {
		name, input string
		want        []string
	}{
		{"empty", "{}", nil},
		{"whitespace only", "{   }", nil},
		{"single", "{a}", []string{"a"}},
		{"multiple", "{a,b,c}", []string{"a", "b", "c"}},
		{"quoted value", `{a,"b c",d}`, []string{"a", "b c", "d"}},
		{"escaped quote", `{"a\"b"}`, []string{`a"b`}},
		{"empty element", "{a,,b}", []string{"a", "", "b"}},
		{"unicode", "{año,bär,café}", []string{"año", "bär", "café"}},
		{"trailing comma value", `{hello,}`, []string{"hello", ""}},
		{"comma in quoted", `{"x,y"}`, []string{"x,y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePGArray(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Tag scanning

func TestScanTags(t *testing.T) {
	tests := []struct {
		name, input string
		want        []string
	}{
		// Empty / null
		{"empty string", "", []string{}},
		{"pg empty", "{}", []string{}},
		{"json empty", "[]", []string{}},
		{"null literal", "null", []string{}},

		// PG array format
		{"pg simple", "{a,b,c}", []string{"a", "b", "c"}},
		{"pg quoted", `{"hello world",b}`, []string{"hello world", "b"}},

		// JSON format
		{`json simple`, `["a","b","c"]`, []string{"a", "b", "c"}},
		{`json quoted`, `["hello world","b"]`, []string{"hello world", "b"}},

		// Fallback
		{"single value fallback", "plain", []string{"plain"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScanTags(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestScanTagsNull(t *testing.T) {
	tests := []struct {
		name string
		ns   sql.NullString
		want []string
	}{
		{"valid", sql.NullString{String: "{a,b}", Valid: true}, []string{"a", "b"}},
		{"null", sql.NullString{Valid: false}, nil},
		{"empty string valid", sql.NullString{String: "", Valid: true}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ScanTagsNull(tt.ns))
		})
	}
}

// Safe JSON marshaling

func TestMustMarshalBytes(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		b := MustMarshalBytes(map[string]string{"key": "val"})
		var m map[string]string
		assert.NoError(t, json.Unmarshal(b, &m))
		assert.Equal(t, "val", m["key"])
	})

	t.Run("unmarshalable", func(t *testing.T) {
		// channels can't be marshaled
		b := MustMarshalBytes(make(chan int))
		assert.Equal(t, []byte("{}"), b)
	})
}

func TestMustMarshalString(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		s := MustMarshalString(map[string]string{"key": "val"})
		assert.Contains(t, s, `"key"`)
		assert.Contains(t, s, `"val"`)
	})

	t.Run("unmarshalable", func(t *testing.T) {
		s := MustMarshalString(make(chan int))
		assert.Equal(t, "", s)
	})
}

// Round-trip: ScanTags ↔ ParsePGArray for PG format

func TestScanTags_PGRoundtrip(t *testing.T) {
	input := `{"hello world",simple,"escaped\"quote"}`
	tags := ScanTags(input)
	assert.Equal(t, []string{"hello world", "simple", `escaped"quote`}, tags)
}

func TestScanTagsAny(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want []string
	}{
		{"string", "{a,b}", []string{"a", "b"}},
		{"bytes", []byte("{a,b}"), []string{"a", "b"}},
		{"nil", nil, nil},
		{"int fallback", 42, []string{"42"}},
		{"json string", `["a","b"]`, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ScanTagsAny(tt.raw))
		})
	}
}
