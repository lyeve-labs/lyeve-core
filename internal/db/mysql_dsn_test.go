package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithParseTime_AlwaysEndsTrue(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"no query", "u:p@tcp(h:3306)/lyeve", "u:p@tcp(h:3306)/lyeve?parseTime=true"},
		{"other parameters", "u:p@tcp(h:3306)/lyeve?tls=true", "u:p@tcp(h:3306)/lyeve?tls=true&parseTime=true"},
		{"already true", "u:p@tcp(h:3306)/lyeve?parseTime=true&tls=true", "u:p@tcp(h:3306)/lyeve?parseTime=true&tls=true"},
		{"explicit false", "u:p@tcp(h:3306)/lyeve?tls=true&parseTime=false", "u:p@tcp(h:3306)/lyeve?tls=true&parseTime=true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, withParseTime(tt.dsn))
		})
	}
}

func TestDriverDSN_MySQLURLAsDocumentedGetsEveryParameter(t *testing.T) {
	got := driverDSN("mysql", "mysql://u:p@tcp(h:3306)/lyeve")
	for _, p := range []string{"parseTime=true", "multiStatements=true", "clientFoundRows=true"} {
		assert.Contains(t, got, p)
	}
	assert.NotContains(t, got, "mysql://")
}
