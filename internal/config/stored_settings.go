package config

import (
	"reflect"
	"sort"
)

// Settings the engine reads at boot and their stored values.
//
// Load runs before the database is open, so it cannot see a value the admin
// layer holds. Once that layer is installed, boot reads the settings again and
// a stored value takes effect at that start. Without this second read, a value
// saved in the console for the token lifetime or the body limits would be
// accepted and never used.

// bootFields are the settings the engine acts on before the stored layer can be
// read: reaching and sizing the database, decrypting what it holds, the
// listeners, telemetry, storage, the license and the resource limits. They keep
// the values Load read, because the engine is already running on them.
var bootFields = map[string]bool{
	"Mode":                              true,
	"AdminListenAddr":                   true,
	"APIListenAddr":                     true,
	"DatabaseURL":                       true,
	"DatabaseDriver":                    true,
	"DatabaseMaxConns":                  true,
	"DatabaseMinConns":                  true,
	"DatabaseHealthCheckPeriod":         true,
	"DatabaseConnMaxLifetime":           true,
	"DatabaseConnMaxIdleTime":           true,
	"DatabaseReplicaURL":                true,
	"DatabaseReplicaMaxConns":           true,
	"PoolSizing":                        true,
	"PoolerMode":                        true,
	"DBWarmupParallelism":               true,
	"MigrationsPath":                    true,
	"EncryptionKey":                     true,
	"JWTKeyPath":                        true,
	"InstanceID":                        true,
	"Environment":                       true,
	"MultiTenant":                       true,
	"OTLPEndpoint":                      true,
	"OTLPInsecure":                      true,
	"TracingSamplingRate":               true,
	"TracingTenantSamplingRates":        true,
	"OutboundAllowCGNAT":                true,
	"StorageDriver":                     true,
	"StorageLocalPath":                  true,
	"StorageBaseURL":                    true,
	"LicenseKey":                        true,
	"LicenseCacheDir":                   true,
	"LicenseServerURL":                  true,
	"BackpressureEnabled":               true,
	"BackpressureMaxInflight":           true,
	"BackpressureTenantQuotaPct":        true,
	"BackpressurePoolPressureThreshold": true,
	"MemoryLimitBytes":                  true,
	"CPUQuota":                          true,
}

// ApplyStoredSettings reads the engine's settings again now that the stored
// layer is installed, and writes into c each one a stored value changed. The
// environment and the configuration file keep the precedence the resolver
// gives them, and the fields in bootFields keep their boot values. It returns
// the names of the fields it changed.
//
// Nothing is read again when no setting read at boot has a stored value. A
// stored value that fails validation leaves c as it was and returns the error,
// so a bad save cannot stop the engine from starting.
//
// Call it once during boot, before anything reads c concurrently.
func ApplyStoredSettings(c *Config) ([]string, error) {
	r := ActiveResolver()
	if !r.storedAtStart() {
		return nil, nil
	}
	// These reads are still part of starting, so the keys keep reporting that
	// a later save waits for the next start.
	r.rereading.Store(true)
	next, err := resolveConfig()
	r.rereading.Store(false)
	if err != nil {
		return nil, err
	}

	cur, upd := reflect.ValueOf(c).Elem(), reflect.ValueOf(next).Elem()
	var changed []string
	for i := 0; i < cur.NumField(); i++ {
		name := cur.Type().Field(i).Name
		if bootFields[name] || !cur.Field(i).CanSet() {
			continue
		}
		if reflect.DeepEqual(cur.Field(i).Interface(), upd.Field(i).Interface()) {
			continue
		}
		cur.Field(i).Set(upd.Field(i))
		changed = append(changed, name)
	}
	sort.Strings(changed)
	return changed, nil
}

// storedAtStart reports whether any key read only at start now resolves from
// the admin layer.
func (r *Resolver) storedAtStart() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	keys := make([]string, 0, len(r.atStart))
	for k := range r.atStart {
		keys = append(keys, k)
	}
	r.mu.RUnlock()
	for _, k := range keys {
		if r.Resolve(k).From == SourceAdmin {
			return true
		}
	}
	return false
}
