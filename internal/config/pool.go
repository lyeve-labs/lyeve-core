// Config from environment variables. Every name below is read by this file.
//
// Read only when CONNECTION_POOLER names a pooler:
//
//	CONNECTION_POOLER               "pgbouncer", "proxysql", or "" / "none"
//	POOL_MAX_CLIENT_CONN            max client connections (100)
//	POOL_DEFAULT_POOL_SIZE          default pool size (20)
//	POOL_RESERVE_POOL_SIZE          reserve pool size (5)
//	POOL_TENANT_SIZES               JSON array of per-tenant overrides
//
// Read on every install, pooler or not:
//
//	POOL_HEALTH_MAX_LATENCY         max ping latency (1s)
//	POOL_HEALTH_MIN_IDLE            min idle connections (1)
//	POOL_HEALTH_MAX_UTIL            max utilization ratio (0.9)

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// PoolTenantSizeOverride is the deserialized form of a single POOL_TENANT_SIZES
// entry. Merged into the target struct at runtime.
type PoolTenantSizeOverride struct {
	Slug           string `json:"slug"`
	PoolSize       int32  `json:"pool_size"`
	MinPoolSize    int32  `json:"min_pool_size,omitempty"`
	MaxConnections int32  `json:"max_connections,omitempty"`
	DBName         string `json:"db_name,omitempty"`
}

// LoadPoolConfig reads pooler configuration from environment variables and
// returns a populated db.PoolConfig. Returns nil + empty string when no
// external pooler is configured (CONNECTION_POOLER is empty or "none").
func LoadPoolConfig() (*db.PoolConfig, string) {
	poolerMode := envOr("CONNECTION_POOLER", "none")
	if poolerMode == "none" || poolerMode == "" {
		return nil, ""
	}

	cfg := db.DefaultPoolConfig()

	if v := os.Getenv("POOL_MAX_CLIENT_CONN"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			cfg.MaxClientConn = int32(n)
		}
	}
	if v := os.Getenv("POOL_DEFAULT_POOL_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			cfg.DefaultPoolSize = int32(n)
		}
	}
	if v := os.Getenv("POOL_RESERVE_POOL_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
			cfg.ReservePoolSize = int32(n)
		}
	}

	// A parse failure keeps the defaults rather than failing the boot, because
	// a typo here should not take an install down. It is reported, though, so
	// an override that never applied can be told apart from one that did.
	if v := os.Getenv("POOL_TENANT_SIZES"); v != "" {
		var overrides []PoolTenantSizeOverride
		if err := json.Unmarshal([]byte(v), &overrides); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: POOL_TENANT_SIZES is not valid JSON (%v) - per-tenant pool sizes are unset and the defaults apply\n", err)
		} else {
			for _, o := range overrides {
				if o.Slug == "" {
					fmt.Fprintf(os.Stderr, "WARNING: POOL_TENANT_SIZES has an entry with no slug - it is ignored\n")
					continue
				}
				cfg.PerTenantPools[o.Slug] = db.TenantPoolSize{
					PoolSize:       o.PoolSize,
					MinPoolSize:    o.MinPoolSize,
					MaxConnections: o.MaxConnections,
					DBName:         o.DBName,
				}
			}
		}
	}

	return &cfg, poolerMode
}

// LoadPoolHealthOptions reads pool health-check thresholds from POOL_HEALTH_*
// environment variables. Returns defaults (1s, 1, 0.9) when unset.
func LoadPoolHealthOptions() (maxLatency time.Duration, minIdle int, maxUtil float64) {
	maxLatency = 1 * time.Second
	if v := os.Getenv("POOL_HEALTH_MAX_LATENCY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			maxLatency = d
		}
	}

	minIdle = 1
	if v := os.Getenv("POOL_HEALTH_MIN_IDLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			minIdle = n
		}
	}

	maxUtil = 0.9
	if v := os.Getenv("POOL_HEALTH_MAX_UTIL"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1.0 {
			maxUtil = f
		}
	}

	return
}
