package db

import "testing"

// The expected values come from running each statement against the engines the
// project supports: postgres:16-alpine reports 100, mysql:8.0 reports 151, and
// azure-sql-edge reports 32767.
func TestServerMaxConnQuery_PerEngine(t *testing.T) {
	tests := []struct {
		name   string
		engine string
		want   string
	}{
		{"postgres", "postgres", "SELECT setting FROM pg_settings WHERE name = 'max_connections'"},
		{"postgres alias", "postgresql", "SELECT setting FROM pg_settings WHERE name = 'max_connections'"},
		{"mysql", "mysql", "SELECT @@max_connections"},
		{"mssql", "mssql", "SELECT @@MAX_CONNECTIONS"},
		{"unknown engine has no query", "cockroach", ""},
		{"empty engine has no query", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverMaxConnQuery(tt.engine); got != tt.want {
				t.Errorf("serverMaxConnQuery(%q) = %q, want %q", tt.engine, got, tt.want)
			}
		})
	}
}

func TestPoolCrowdsServer(t *testing.T) {
	tests := []struct {
		name      string
		maxConns  int32
		serverMax int
		want      bool
	}{
		{"a pool of 100 against stock postgres", 100, 100, true},
		{"a pool of 100 against stock mysql", 100, 151, true},
		{"the default of 25 against stock postgres", 25, 100, false},
		{"the default leaves room for four instances", 25, 100, false},
		{"just over the threshold", 35, 100, true},
		{"exactly at the threshold is not crowded", 34, 100, false},
		{"tuned server absorbs a wide pool", 100, 500, false},
		{"sql server default is never crowded", 100, 32767, false},
		{"unlimited pool is not measured", 0, 100, false},
		{"negative pool is not measured", -1, 100, false},
		{"unreadable server limit is not crowded", 100, 0, false},
		{"negative server limit is not crowded", 100, -5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := poolCrowdsServer(tt.maxConns, tt.serverMax); got != tt.want {
				t.Errorf("poolCrowdsServer(%d, %d) = %v, want %v",
					tt.maxConns, tt.serverMax, got, tt.want)
			}
		})
	}
}
