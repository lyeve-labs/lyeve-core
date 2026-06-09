package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	dbprovider "github.com/lyeve-labs/lyeve-core/internal/db/provider"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a minimal Provider (no DetailedHealth) for registry tests.
type fakeProvider struct {
	name      string
	category  Category
	healthErr error
	autoMatch bool
}

func (f fakeProvider) Name() string                      { return f.name }
func (f fakeProvider) Category() Category                { return f.category }
func (f fakeProvider) Capabilities() Capabilities        { return NewCapabilities() }
func (f fakeProvider) HealthCheck(context.Context) error { return f.healthErr }
func (f fakeProvider) AutoDetect(string) bool            { return f.autoMatch }

// fakeHealthCheckerProvider additionally implements HealthChecker.
type fakeHealthCheckerProvider struct{ fakeProvider }

func (f fakeHealthCheckerProvider) DetailedHealth(context.Context) HealthStatus {
	return HealthStatus{Provider: f.name, Category: f.category, Healthy: true, Error: "detail"}
}

func TestRegistry_Detect(t *testing.T) {
	tests := []struct {
		name     string
		category Category
		dsn      string
		want     string // "" means expect nil
	}{
		{"storage s3", CategoryStorage, "s3://mybucket", "s3"},
		{"a gcs url matches no provider", CategoryStorage, "gs://mybucket", ""},
		{"an azure url matches no provider", CategoryStorage, "azblob://mycontainer", ""},
		{"no storage match", CategoryStorage, "no-such-scheme://x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Registry.Detect(tt.category, tt.dsn)
			if tt.want == "" {
				assert.Nil(t, p)
				return
			}
			require.NotNil(t, p)
			assert.Equal(t, tt.want, p.Name())
			assert.Equal(t, tt.category, p.Category())
		})
	}
}

func TestRegistry_Names(t *testing.T) {
	names := Registry.Names()
	assert.GreaterOrEqual(t, len(names), 2)
	for _, want := range []string{"s3", "minio"} {
		assert.Contains(t, names, want)
	}
}

func TestRegistry_MustGet_Success(t *testing.T) {
	p := Registry.MustGet("s3")
	require.NotNil(t, p)
	assert.Equal(t, "s3", p.Name())
}

func TestRunHealthCheck(t *testing.T) {
	t.Run("healthy provider", func(t *testing.T) {
		hs := runHealthCheck(context.Background(), fakeProvider{name: "h", category: CategoryStorage})
		assert.True(t, hs.Healthy)
		assert.Equal(t, "h", hs.Provider)
		assert.Equal(t, CategoryStorage, hs.Category)
		assert.Empty(t, hs.Error)
	})

	t.Run("unhealthy provider", func(t *testing.T) {
		hs := runHealthCheck(context.Background(), fakeProvider{name: "h", healthErr: errors.New("down")})
		assert.False(t, hs.Healthy)
		assert.Equal(t, "down", hs.Error)
	})

	t.Run("HealthChecker provider returns detailed report", func(t *testing.T) {
		hs := runHealthCheck(context.Background(),
			fakeHealthCheckerProvider{fakeProvider{name: "detailed", category: CategoryDB}})
		assert.True(t, hs.Healthy)
		assert.Equal(t, "detailed", hs.Provider)
		assert.Equal(t, "detail", hs.Error) // proves DetailedHealth() output was used
		assert.False(t, hs.CheckedAt.IsZero())
	})
}

func TestHealthCheckAll(t *testing.T) {
	r := &registry{providers: map[string]Provider{
		"ok":  fakeProvider{name: "ok", category: CategoryStorage},
		"bad": fakeProvider{name: "bad", category: CategoryStorage, healthErr: errors.New("boom")},
	}}
	results := r.HealthCheckAll(context.Background())
	require.Len(t, results, 2)

	byName := map[string]HealthStatus{}
	for _, hs := range results {
		byName[hs.Provider] = hs
	}
	assert.True(t, byName["ok"].Healthy)
	assert.False(t, byName["bad"].Healthy)
	assert.Equal(t, "boom", byName["bad"].Error)
}

func TestCostTracker_Total_RecordsError(t *testing.T) {
	ct := NewCostTracker()
	ct.Record(OperationCost{Provider: "s3", Operation: "put", BytesOut: 10, Error: "rate limited", Timestamp: time.Now()})

	totals := ct.Total()
	require.Len(t, totals, 1)
	assert.Equal(t, "rate limited", totals[0].Error)
	assert.Equal(t, int64(10), totals[0].BytesOut)
}

func TestCostTracker_Total_SkipsZeroOpsBuckets(t *testing.T) {
	ct := NewCostTracker()
	ct.Record(OperationCost{Provider: "s3", Operation: "put", BytesOut: 100, Timestamp: time.Now()})
	// Snapshot swaps counters to zero but leaves the bucket registered.
	ct.Snapshot()

	// Total now sees a zero-ops bucket and skips it.
	assert.Empty(t, ct.Total())
}

func TestDefaultPoolOptions_Unified(t *testing.T) {
	opts := DefaultPoolOptions()
	assert.Equal(t, int32(25), opts.MaxConns)
	assert.Equal(t, int32(2), opts.MinConns)
	assert.Equal(t, 1*time.Hour, opts.ConnMaxLifetime)
	assert.Equal(t, 5*time.Minute, opts.ConnMaxIdleTime)
	assert.Equal(t, 30*time.Second, opts.HealthCheckPeriod)
}

func TestDBProviderAdapter(t *testing.T) {
	inner := dbprovider.Registry.MustGet("postgres")
	a := newDBProviderAdapter(inner)

	assert.Equal(t, "postgres", a.Name())
	assert.Equal(t, CategoryDB, a.Category())
	assert.Equal(t, inner.DriverName(), a.DriverName())
	assert.Equal(t, uint64(inner.Capabilities().Mask()), a.Capabilities().Mask())
	assert.NotZero(t, a.Capabilities().Mask())

	// Provider-level health check is a no-op (real connections are polled by FailoverDB).
	assert.NoError(t, a.HealthCheck(context.Background()))

	dh := a.DetailedHealth(context.Background())
	assert.True(t, dh.Healthy)
	assert.Equal(t, "postgres", dh.Provider)
	assert.Equal(t, CategoryDB, dh.Category)

	assert.True(t, a.AutoDetect("postgres://u:p@localhost:5432/db"))
	assert.False(t, a.AutoDetect("mysql://u:p@localhost:3306/db"))
}

func TestConnectedDB(t *testing.T) {
	adapter := newDBProviderAdapter(dbprovider.Registry.MustGet("postgres"))

	t.Run("health check delegates to pool ping", func(t *testing.T) {
		m := newMockDB("p", "postgres")
		c := &ConnectedDB{DB: m, Provider: adapter, Costs: NewCostTracker()}
		assert.NoError(t, c.HealthCheck(context.Background()))

		m.pingErr = errors.New("ping failed")
		assert.EqualError(t, c.HealthCheck(context.Background()), "ping failed")
	})

	t.Run("detailed health reflects ping result", func(t *testing.T) {
		m := newMockDB("p", "postgres")
		c := &ConnectedDB{DB: m, Provider: adapter, Costs: NewCostTracker()}
		ok := c.DetailedHealth(context.Background())
		assert.True(t, ok.Healthy)
		assert.Equal(t, "postgres", ok.Provider)

		m.pingErr = errors.New("dead")
		bad := c.DetailedHealth(context.Background())
		assert.False(t, bad.Healthy)
		assert.Equal(t, "dead", bad.Error)
	})

	t.Run("record op tracks cost", func(t *testing.T) {
		ct := NewCostTracker()
		c := &ConnectedDB{DB: newMockDB("p", "postgres"), Provider: adapter, Costs: ct}
		c.RecordOp("query", 3*time.Millisecond, nil)

		totals := ct.Total()
		require.Len(t, totals, 1)
		assert.Equal(t, "postgres", totals[0].Provider)
		assert.Equal(t, "query", totals[0].Operation)

		ct2 := NewCostTracker()
		c2 := &ConnectedDB{DB: newMockDB("p", "postgres"), Provider: adapter, Costs: ct2}
		c2.RecordOp("query", time.Millisecond, errors.New("boom"))
		assert.Equal(t, "boom", ct2.Total()[0].Error)
	})
}

type fakeStorageClient struct {
	putObj  *core.StorageObject
	putErr  error
	getObj  *core.StorageObject
	getErr  error
	delErr  error
	pingErr error
}

func (c *fakeStorageClient) Put(context.Context, string, string, io.Reader) (*core.StorageObject, error) {
	return c.putObj, c.putErr
}
func (c *fakeStorageClient) Get(context.Context, string) (io.ReadCloser, *core.StorageObject, error) {
	return io.NopCloser(strings.NewReader("x")), c.getObj, c.getErr
}
func (c *fakeStorageClient) Delete(context.Context, string) error { return c.delErr }
func (c *fakeStorageClient) SignedURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (c *fakeStorageClient) List(context.Context, string, int) ([]*core.StorageObject, error) {
	return nil, nil
}
func (c *fakeStorageClient) Ping(context.Context) error { return c.pingErr }
func (c *fakeStorageClient) Close() error               { return nil }

func TestStorageConnected(t *testing.T) {
	prov := S3Provider() // real provider. Only Name() ("s3") is used
	ctx := context.Background()

	t.Run("put records bytes out from object size", func(t *testing.T) {
		ct := NewCostTracker()
		c := &StorageConnected{Client: &fakeStorageClient{putObj: &core.StorageObject{Size: 100}}, Provider: prov, Costs: ct}
		obj, err := c.Put(ctx, "k", "text/plain", strings.NewReader("data"))
		require.NoError(t, err)
		assert.Equal(t, int64(100), obj.Size)
		require.Len(t, ct.Total(), 1)
		assert.Equal(t, "put", ct.Total()[0].Operation)
		assert.Equal(t, "s3", ct.Total()[0].Provider)
		assert.Equal(t, int64(100), ct.Total()[0].BytesOut)
	})

	t.Run("get records bytes in from object size", func(t *testing.T) {
		ct := NewCostTracker()
		c := &StorageConnected{Client: &fakeStorageClient{getObj: &core.StorageObject{Size: 50}}, Provider: prov, Costs: ct}
		rc, obj, err := c.Get(ctx, "k")
		require.NoError(t, err)
		require.NotNil(t, rc)
		_ = rc.Close()
		assert.Equal(t, int64(50), obj.Size)
		assert.Equal(t, "get", ct.Total()[0].Operation)
		assert.Equal(t, int64(50), ct.Total()[0].BytesIn)
	})

	t.Run("delete records op", func(t *testing.T) {
		ct := NewCostTracker()
		c := &StorageConnected{Client: &fakeStorageClient{}, Provider: prov, Costs: ct}
		require.NoError(t, c.Delete(ctx, "k"))
		assert.Equal(t, "delete", ct.Total()[0].Operation)
	})

	t.Run("health check delegates to ping", func(t *testing.T) {
		c := &StorageConnected{Client: &fakeStorageClient{pingErr: errors.New("unreachable")}, Provider: prov, Costs: NewCostTracker()}
		assert.EqualError(t, c.HealthCheck(ctx), "unreachable")
	})

	t.Run("error is recorded", func(t *testing.T) {
		ct := NewCostTracker()
		c := &StorageConnected{Client: &fakeStorageClient{putErr: errors.New("denied")}, Provider: prov, Costs: ct}
		_, err := c.Put(ctx, "k", "text/plain", strings.NewReader("d"))
		assert.Error(t, err)
		require.Len(t, ct.Total(), 1)
		assert.Equal(t, "denied", ct.Total()[0].Error)
	})
}

func TestS3AndMinIOProviders(t *testing.T) {
	s3 := S3Provider()
	assert.NoError(t, s3.HealthCheck(context.Background()))
	assert.True(t, s3.Capabilities().Has(StorageCapMultipart))

	// The "contains s3 but no recognized scheme" fallback branch of AutoDetect.
	assert.True(t, s3.AutoDetect("s3"))
	assert.False(t, MinIOProvider().AutoDetect("s3"))

	minio := MinIOProvider()
	assert.Equal(t, "minio", minio.Name())
	assert.NoError(t, minio.HealthCheck(context.Background()))

	// MinIO requires an explicit endpoint even when keys/bucket are present.
	_, err := minio.Connect(context.Background(), StorageConfig{
		Bucket:    "b",
		Region:    "us-east-1",
		AccessKey: "AK",
		SecretKey: "SK",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint is required")
}
