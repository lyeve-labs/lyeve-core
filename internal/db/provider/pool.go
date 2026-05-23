package provider

import "time"

// PoolOptions carries provider-agnostic pool-tuning parameters.
type PoolOptions struct {
	MaxConns          int32
	MinConns          int32
	ConnMaxLifetime   time.Duration
	ConnMaxIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// DefaultPoolOptions returns sensible defaults for any engine.
func DefaultPoolOptions() PoolOptions {
	return PoolOptions{
		MaxConns:          25,
		MinConns:          2,
		ConnMaxLifetime:   1 * time.Hour,
		ConnMaxIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	}
}

// PoolDefaults returns engine-specific recommended pool sizing. Providers
// override these for engines with different concurrency models.
type PoolDefaults struct {
	RecommendedMaxConns        int32
	RecommendedMinConns        int32
	RecommendedConnMaxLifetime time.Duration
	RecommendedConnMaxIdleTime time.Duration

	// MaxConnLimit is the hard cap beyond which the engine degrades.
	// MySQL = ~1000, Postgres = ~500.
	MaxConnLimit int32
}

// DefaultPoolDefaults returns sensible cross-engine defaults.
func DefaultPoolDefaults() PoolDefaults {
	return PoolDefaults{
		RecommendedMaxConns:        25,
		RecommendedMinConns:        2,
		RecommendedConnMaxLifetime: 1 * time.Hour,
		RecommendedConnMaxIdleTime: 5 * time.Minute,
		MaxConnLimit:               100,
	}
}

// ApplyPoolOptions applies opts over defaults, filling zero values with defaults.
func ApplyPoolOptions(opts PoolOptions, defaults PoolDefaults) PoolOptions {
	if opts.MaxConns <= 0 {
		opts.MaxConns = defaults.RecommendedMaxConns
	}
	if opts.MinConns <= 0 {
		opts.MinConns = defaults.RecommendedMinConns
	}
	if opts.ConnMaxLifetime <= 0 {
		opts.ConnMaxLifetime = defaults.RecommendedConnMaxLifetime
	}
	if opts.ConnMaxIdleTime <= 0 {
		opts.ConnMaxIdleTime = defaults.RecommendedConnMaxIdleTime
	}
	if opts.HealthCheckPeriod <= 0 {
		opts.HealthCheckPeriod = 30 * time.Second
	}
	// Clamp to engine max.
	if opts.MaxConns > defaults.MaxConnLimit {
		opts.MaxConns = defaults.MaxConnLimit
	}
	return opts
}
