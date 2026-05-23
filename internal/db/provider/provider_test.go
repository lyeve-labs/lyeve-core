package provider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_Registration(t *testing.T) {
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		p := Registry.Get(name)
		require.NotNil(t, p, "%s should be registered", name)
		assert.Equal(t, name, p.Name())
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	assert.Nil(t, Registry.Get("oracle"))
}

func TestRegistry_Names(t *testing.T) {
	names := Registry.Names()
	assert.GreaterOrEqual(t, len(names), 3)
	assert.Contains(t, names, "postgres")
}

func TestRegistry_Detect_Postgres(t *testing.T) {
	tests := []string{
		"postgresql://user:***@localhost:5432/mydb",
		"postgres://user:***@localhost:5432/mydb",
		"host=localhost dbname=mydb user=postgres",
	}
	for _, dsn := range tests {
		p := Registry.Detect(dsn)
		assert.Equal(t, "postgres", p.Name(), "DSN: %s", dsn)
	}
}

func TestRegistry_Detect_MySQL(t *testing.T) {
	tests := []string{
		"mysql://user:***@localhost:3306/mydb",
		"user:pass@tcp(localhost:3306)/mydb",
	}
	for _, dsn := range tests {
		p := Registry.Detect(dsn)
		assert.Equal(t, "mysql", p.Name(), "DSN: %s", dsn)
	}
}

func TestRegistry_Detect_MSSQL(t *testing.T) {
	p := Registry.Detect("sqlserver://user:pass@localhost:1433?database=mydb")
	assert.Equal(t, "mssql", p.Name())
}

func TestRegistry_MustGet_PanicsOnMissing(t *testing.T) {
	assert.Panics(t, func() { Registry.MustGet("oracle") })
}

// Capabilities tests

func TestCapabilities_Has(t *testing.T) {
	caps := NewCapabilities(CapCTE, CapWindowFunc, CapUpsertOnConflict)
	assert.True(t, caps.Has(CapCTE))
	assert.True(t, caps.Has(CapWindowFunc))
	assert.False(t, caps.Has(CapCTERecursive))
	assert.False(t, caps.Has(CapAdvisoryLock))
	assert.True(t, caps.Has(CapCTE|CapWindowFunc))
	assert.False(t, caps.Has(CapCTE|CapAdvisoryLock))
}

func TestCapabilities_String(t *testing.T) {
	caps := NewCapabilities(CapCTE, CapWindowFunc)
	s := caps.String()
	assert.Contains(t, s, "CTE")
	assert.Contains(t, s, "WindowFunc")
}

func TestCapabilities_EachProvider(t *testing.T) {
	type check struct {
		name    string
		caps    Capabilities
		expect  []Capability
		missing []Capability
	}
	checks := []check{
		{name: "postgres", caps: Registry.MustGet("postgres").Capabilities(),
			expect:  []Capability{CapCTE, CapWindowFunc, CapAdvisoryLock, CapJSONNative, CapUUIDNative, CapListenNotify},
			missing: []Capability{CapUpsertOnDuplicate, CapUpsertMerge},
		},
		{name: "mysql", caps: Registry.MustGet("mysql").Capabilities(),
			expect:  []Capability{CapCTE, CapWindowFunc, CapUpsertOnDuplicate, CapFullText},
			missing: []Capability{CapUUIDNative, CapTimestampTZ, CapListenNotify},
		},
		{name: "mssql", caps: Registry.MustGet("mssql").Capabilities(),
			expect:  []Capability{CapCTE, CapCTERecursive, CapWindowFunc, CapUpsertMerge},
			missing: []Capability{CapCreateTableIfNotExists, CapUUIDNative, CapJSONNative},
		},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			for _, e := range c.expect {
				assert.True(t, c.caps.Has(e), "%s should have %s", c.name, e.Name())
			}
			for _, m := range c.missing {
				assert.False(t, c.caps.Has(m), "%s should NOT have %s", c.name, m.Name())
			}
		})
	}
}

// DSN Parser tests

func TestDSNParser_Postgres(t *testing.T) {
	p := Registry.MustGet("postgres")
	info := p.DSNParser().Parse("postgresql://nopw@localhost:5432/mydb?sslmode=disable")
	assert.Equal(t, "postgres", info.Engine)
	assert.Equal(t, "localhost", info.Host)
	assert.Equal(t, 5432, info.Port)
	assert.Equal(t, "mydb", info.DBName)
	assert.Equal(t, "nopw", info.User)
	assert.Equal(t, "", info.Password)
	assert.Equal(t, "disable", info.Params["sslmode"])
}

func TestDSNParser_MySQL(t *testing.T) {
	p := Registry.MustGet("mysql")
	info := p.DSNParser().Parse("root@tcp(127.0.0.1:3306)/mydb?parseTime=true")
	assert.Equal(t, "mysql", info.Engine)
	assert.Equal(t, "127.0.0.1", info.Host)
	assert.Equal(t, 3306, info.Port)
	assert.Equal(t, "mydb", info.DBName)
	assert.Equal(t, "root", info.User)
	assert.Equal(t, "true", info.Params["parseTime"])
}

func TestDSNParser_MSSQL(t *testing.T) {
	p := Registry.MustGet("mssql")
	info := p.DSNParser().Parse("sqlserver://user@db.example.com:1433?database=myapp&encrypt=true")
	assert.Equal(t, "mssql", info.Engine)
	assert.Equal(t, "db.example.com", info.Host)
	assert.Equal(t, 1433, info.Port)
	assert.Equal(t, "myapp", info.DBName)
}

func TestDSNParser_AllProviders(t *testing.T) {
	for _, name := range Registry.Names() {
		p := Registry.MustGet(name)
		t.Run(name, func(t *testing.T) {
			dsn := sampleDSN(name)
			info := p.DSNParser().Parse(dsn)
			assert.Equal(t, name, info.Engine)
			assert.NotEmpty(t, info.Raw)
		})
	}
}

func sampleDSN(engine string) string {
	switch engine {
	case "postgres":
		return "postgresql://u:***@localhost:5432/db"
	case "mysql":
		return "u:p@tcp(localhost:3306)/db"
	case "mssql":
		return "sqlserver://u:p@localhost:1433?database=db"
	default:
		return "postgresql://u:***@localhost:5432/db"
	}
}

// Pool tests

func TestPoolDefaults_Postgres(t *testing.T) {
	pd := Registry.MustGet("postgres").PoolDefaults()
	assert.Equal(t, int32(25), pd.RecommendedMaxConns)
	assert.Equal(t, int32(500), pd.MaxConnLimit)
}

func TestApplyPoolOptions_FillsZeros(t *testing.T) {
	defaults := PoolDefaults{
		RecommendedMaxConns: 25, RecommendedMinConns: 2,
		RecommendedConnMaxLifetime: 1 * time.Hour, RecommendedConnMaxIdleTime: 5 * time.Minute,
		MaxConnLimit: 500,
	}
	opts := ApplyPoolOptions(PoolOptions{}, defaults)
	assert.Equal(t, int32(25), opts.MaxConns)
	assert.Equal(t, int32(2), opts.MinConns)
	assert.Equal(t, 30*time.Second, opts.HealthCheckPeriod)
}

func TestApplyPoolOptions_ClampsMax(t *testing.T) {
	defaults := PoolDefaults{MaxConnLimit: 100}
	opts := ApplyPoolOptions(PoolOptions{MaxConns: 500}, defaults)
	assert.Equal(t, int32(100), opts.MaxConns)
}

func TestApplyPoolOptions_PreservesExplicit(t *testing.T) {
	opts := ApplyPoolOptions(PoolOptions{MaxConns: 10, MinConns: 3}, DefaultPoolDefaults())
	assert.Equal(t, int32(10), opts.MaxConns)
	assert.Equal(t, int32(3), opts.MinConns)
}

// Provider interface completeness

func TestAllProviders_SatisfyInterface(t *testing.T) {
	for _, name := range Registry.Names() {
		p := Registry.MustGet(name)
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, p.Name())
			assert.NotNil(t, p.Dialect())
			assert.NotZero(t, p.Capabilities().Mask(), "%s should declare capabilities", name)
			assert.NotEmpty(t, p.DriverName())
			assert.NotNil(t, p.DSNParser())
			pd := p.PoolDefaults()
			assert.GreaterOrEqual(t, pd.MaxConnLimit, pd.RecommendedMaxConns)
			assert.GreaterOrEqual(t, pd.RecommendedMaxConns, int32(1))
		})
	}
}
