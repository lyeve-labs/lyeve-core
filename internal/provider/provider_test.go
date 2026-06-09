package provider

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderInterface(t *testing.T) {
	t.Parallel()

	t.Run("s3 provider satisfies Provider and StorageProvider", func(t *testing.T) {
		p := S3Provider()
		assert.Equal(t, "s3", p.Name())
		assert.Equal(t, CategoryStorage, p.Category())
		assert.NotNil(t, p.Capabilities())
	})

	t.Run("all providers have unique names", func(t *testing.T) {
		providers := []Provider{
			S3Provider(), MinIOProvider(),
		}
		names := make(map[string]bool)
		for _, p := range providers {
			name := p.Name()
			assert.False(t, names[name], "duplicate provider name: %s", name)
			names[name] = true
		}
	})
}

func TestRegistryRegister(t *testing.T) {
	t.Run("init-registered providers are retrievable", func(t *testing.T) {
		got := Registry.Get("s3")
		require.NotNil(t, got)
		assert.Equal(t, "s3", got.Name())
	})

	t.Run("mustGet panics on missing", func(t *testing.T) {
		assert.Panics(t, func() {
			Registry.MustGet("nonexistent-provider-12345")
		})
	})

	t.Run("duplicate registration panics", func(t *testing.T) {
		assert.Panics(t, func() {
			Registry.Register(S3Provider())
		})
	})
}

func TestRegistryByCategory(t *testing.T) {
	t.Run("storage providers", func(t *testing.T) {
		storage := Registry.ByCategory(CategoryStorage)
		assert.GreaterOrEqual(t, len(storage), 2)
		for _, p := range storage {
			assert.Equal(t, CategoryStorage, p.Category())
		}
	})
}

func TestDetectEngineFromDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		dsn      string
		expected string
	}{
		{"postgres standard", "postgres://user:pass@localhost:5432/db", "postgres"},
		{"postgresql alias", "postgresql://user:pass@localhost:5432/db", "postgres"},
		{"cockroachdb cluster param", "postgres://root@localhost:26257/db?options=--cluster=mycluster", "cockroachdb"},
		{"cockroachdb keyword", "postgres://root@localhost:26257/db?cockroach=true", "cockroachdb"},
		{"mysql dsn", "mysql://user:pass@localhost:3306/db", "mysql"},
		{"tidb scheme", "tidb://user:pass@localhost:4000/db", "mysql"},
		{"mssql dsn", "mssql://user:pass@localhost:1433/db", "mssql"},
		{"sqlserver alias", "sqlserver://user:pass@localhost:1433/db", "mssql"},
		{"sqlite dsn", "sqlite:///tmp/db.sqlite", "sqlite"},
		{"sqlite file prefix", "file:/tmp/db.sqlite", "sqlite"},
		{"mysql tcp format", "user:pass@tcp(localhost:3306)/db", "mysql"},
		{"empty string", "", "postgres"},
		{"unknown scheme", "foo://user:pass@host/db", "postgres"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectEngineFromDSN(tt.dsn)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestDetectStorageFromDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dsn      string
		expected string
	}{
		{"s3://mybucket", "s3"},
		{"s3://mybucket?minio=1", "minio"},
		{"gs://mybucket", ""},
		{"azblob://mycontainer", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			got := detectStorageFromDSN(tt.dsn)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("empty capabilities", func(t *testing.T) {
		c := NewCapabilities()
		assert.Equal(t, uint64(0), c.Mask())
		assert.False(t, c.Has(StorageCapSignedURL))
	})

	t.Run("single flag", func(t *testing.T) {
		c := NewCapabilities(StorageCapSignedURL)
		assert.True(t, c.Has(StorageCapSignedURL))
		assert.False(t, c.Has(StorageCapMultipart))
	})

	t.Run("multiple flags", func(t *testing.T) {
		c := NewCapabilities(StorageCapSignedURL, StorageCapMultipart)
		assert.True(t, c.Has(StorageCapSignedURL))
		assert.True(t, c.Has(StorageCapMultipart))
	})

	t.Run("no in-category flag collisions", func(t *testing.T) {
		// Flags within the SAME category should not collide
		assert.NotEqual(t, StorageCapSignedURL, StorageCapMultipart)
	})
}

func TestCostTracker(t *testing.T) {
	t.Parallel()

	t.Run("record and snapshot", func(t *testing.T) {
		ct := NewCostTracker()

		ct.Record(OperationCost{
			Provider:  "redis",
			Operation: "get",
			BytesIn:   1024,
			Latency:   5 * time.Millisecond,
			Timestamp: time.Now(),
		})
		ct.Record(OperationCost{
			Provider:  "redis",
			Operation: "set",
			BytesOut:  512,
			Latency:   3 * time.Millisecond,
			Timestamp: time.Now(),
		})

		costs, since, until := ct.Snapshot()
		assert.Len(t, costs, 2)
		assert.False(t, since.IsZero())
		assert.False(t, until.IsZero())
		assert.True(t, !until.Before(since))

		// Second snapshot: counters reset
		costs2, _, _ := ct.Snapshot()
		assert.Len(t, costs2, 0)
	})

	t.Run("record with error", func(t *testing.T) {
		ct := NewCostTracker()
		ct.Record(OperationCost{
			Provider:  "s3",
			Operation: "put",
			BytesOut:  500,
			Latency:   200 * time.Millisecond,
			Error:     "rate limited",
			Timestamp: time.Now(),
		})

		costs, _, _ := ct.Snapshot()
		require.Len(t, costs, 1)
		assert.Equal(t, "s3", costs[0].Provider)
		assert.Equal(t, int64(500), costs[0].BytesOut)
		assert.Equal(t, "rate limited", costs[0].Error)
	})

	t.Run("total accumulates", func(t *testing.T) {
		ct := NewCostTracker()
		ct.Record(OperationCost{Provider: "s3", Operation: "put", BytesOut: 100, Timestamp: time.Now()})
		ct.Record(OperationCost{Provider: "s3", Operation: "put", BytesOut: 200, Timestamp: time.Now()})
		ct.Record(OperationCost{Provider: "s3", Operation: "get", BytesIn: 50, Timestamp: time.Now()})

		totals := ct.Total()
		assert.Len(t, totals, 2)
	})

	t.Run("concurrent recording", func(t *testing.T) {
		ct := NewCostTracker()
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				ct.Record(OperationCost{
					Provider:  "test",
					Operation: "op",
					Latency:   time.Duration(n) * time.Microsecond,
					Timestamp: time.Now(),
				})
			}(i)
		}
		wg.Wait()

		totals := ct.Total()
		require.Len(t, totals, 1)
		assert.Equal(t, "test", totals[0].Provider)
	})
}

func TestHealthStatus(t *testing.T) {
	t.Parallel()

	t.Run("healthy status", func(t *testing.T) {
		hs := HealthStatus{
			Provider:  "postgres",
			Category:  CategoryDB,
			Healthy:   true,
			Latency:   1 * time.Millisecond,
			CheckedAt: time.Now(),
		}
		assert.True(t, hs.Healthy)
		assert.Empty(t, hs.Error)
	})

	t.Run("unhealthy status", func(t *testing.T) {
		hs := HealthStatus{
			Provider:  "s3",
			Category:  CategoryStorage,
			Healthy:   false,
			Error:     "connection refused",
			CheckedAt: time.Now(),
		}
		assert.False(t, hs.Healthy)
		assert.Equal(t, "connection refused", hs.Error)
	})
}

func TestSplitKey(t *testing.T) {
	tests := []struct {
		key          string
		expectedProv string
		expectedOp   string
	}{
		{"postgres:query", "postgres", "query"},
		{"s3:put", "s3", "put"},
		{"provider:op:extra", "provider:op", "extra"},
		{"nocolon", "nocolon", ""},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			prov, op := splitKey(tt.key)
			assert.Equal(t, tt.expectedProv, prov)
			assert.Equal(t, tt.expectedOp, op)
		})
	}
}

func TestAutoDetectCoverage(t *testing.T) {
	t.Run("s3 auto-detect", func(t *testing.T) {
		assert.True(t, S3Provider().AutoDetect("s3://mybucket"))
		assert.False(t, S3Provider().AutoDetect("gs://mybucket"))
	})
}

func TestCategoryConstants(t *testing.T) {
	assert.Equal(t, Category("db"), CategoryDB)
	assert.Equal(t, Category("storage"), CategoryStorage)
}
