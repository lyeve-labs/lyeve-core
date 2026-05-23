package runtime

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/provider"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// piiExemptAdminPathPrefixes lists admin paths the PII response middleware
// must not rewrite.
//
// Masking is a defense-in-depth net over subject data, but on these paths the
// email or phone number IS the record being administered, so masking them
// removes the console's ability to do its job rather than protecting anything.
// A caller reaching these routes has already passed admin authentication and
// can read the same addresses through the DSAR export, which is exempt for the
// same reason.
//
// The API router keeps unconditional masking: /api/v1 is the surface where
// subject data is served to clients and where the net belongs.
var piiExemptAdminPathPrefixes = []string{
	"/api/admin/gdpr",        // DSAR export must return the subject's real data.
	"/api/admin/auth",        // The operator's own identity.
	"/api/admin/users",       // The directory the console administers.
	"/api/admin/invitations", // Pending invites are addressed by email.
	// Credential management: the owner is an admin account, and a request
	// log's address is the one fact an operator reads it for, since the
	// callers are the operator's own machines. Masked, the list would name
	// no owner and every request would come from "[redacted-ip]".
	"/api/admin/admin-tokens",
	"/api/admin/api-keys",
}

// piiExemptAdminPath reports whether path is exempt from response masking.
// A prefix matches only at a segment boundary, so adding "/api/admin/users"
// cannot silently exempt an unrelated "/api/admin/users-report" route later.
func piiExemptAdminPath(path string) bool {
	for _, p := range piiExemptAdminPathPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// idempotencyOption fills the idempotency slot on a router with what the
// running core.IdempotencyMiddlewareProvider contributes, or nothing when no
// provider is running. The provider backs both routers with one store, so a
// key spent on one router is known to the other.
func idempotencyOption(activator *plugin.Activator) api.RouterOption {
	return api.WithIdempotencyMiddleware(activator.IdempotencyMiddleware())
}

// pluginChain is the phased middleware the running plugins contribute to one
// router. The builders take it in phase order, and mount the engine's own
// slots between the phases where those slots belong.
type pluginChain struct {
	entries []core.ChainEntry
}

func newPluginChain(entries []core.ChainEntry) *pluginChain {
	return &pluginChain{entries: entries}
}

// mountBelow appends every entry left whose phase is below next.
func (c *pluginChain) mountBelow(next core.ChainPhase, mw []api.RouterOption) []api.RouterOption {
	for len(c.entries) > 0 && c.entries[0].Phase < next {
		mw = append(mw, api.WithMiddleware(c.entries[0].Middleware))
		c.entries = c.entries[1:]
	}
	return mw
}

// mountRest appends every entry left.
func (c *pluginChain) mountRest(mw []api.RouterOption) []api.RouterOption {
	for _, e := range c.entries {
		mw = append(mw, api.WithMiddleware(e.Middleware))
	}
	c.entries = nil
	return mw
}

// holds reports whether an entry left is at phase.
func (c *pluginChain) holds(phase core.ChainPhase) bool {
	for _, e := range c.entries {
		if e.Phase == phase {
			return true
		}
	}
	return false
}

func buildAdminExtraMW(ctx context.Context, cfg *config.Config, activator *plugin.Activator,
	drainer *InflightDrainer, backpressureResult apimw.BackpressureResult,
	latencyTracker *apimw.LatencyTracker) []api.RouterOption {
	var mw []api.RouterOption

	mw = append(mw, api.WithMiddleware(drainer.Middleware))
	if cfg.BackpressureEnabled {
		mw = append(mw, api.WithMiddleware(backpressureResult.Middleware))
	}
	mw = append(mw, api.WithMiddleware(apimw.StripForwardedHeaders()))
	if cfg.InstanceRegion != "" {
		geoMW := apimw.GeoRoute(cfg.InstanceRegion)
		mw = append(mw, api.WithMiddleware(geoMW))
	}
	if bf := activator.BruteForceMiddleware(); bf != nil {
		mw = append(mw, api.WithMiddleware(bf))
	}
	if captcha := activator.CaptchaMiddleware(ctx); captcha != nil {
		mw = append(mw, api.WithMiddleware(captcha))
		slog.Info("captcha middleware wired on admin router /api/admin/auth/login")
	}
	mw = append(mw, idempotencyOption(activator))
	chain := newPluginChain(activator.ChainMiddleware(core.ChainAdmin))
	mw = chain.mountBelow(core.ChainPhaseAnalytics, mw)
	// The request sampler slot (core.RequestSamplerProvider). Its position is
	// the capability.
	if sampler := activator.RequestSampler(); sampler != nil {
		mw = append(mw, api.WithMiddleware(sampler))
	}
	mw = chain.mountBelow(core.ChainPhaseQuota, mw)
	// PII response-body masking. Identity and DSAR paths are exempt from the
	// rewrite but still record access, so the access log is populated for every
	// route the console administers PII on.
	if piiMW := activator.PIIMiddleware(); piiMW != nil {
		exemptPII := func(next http.Handler) http.Handler {
			masked := piiMW(next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if piiExemptAdminPath(r.URL.Path) {
					r = r.WithContext(core.WithSkipPIIMask(r.Context()))
				}
				masked.ServeHTTP(w, r)
			})
		}
		mw = append(mw, api.WithMiddleware(exemptPII))
		slog.Info("PII response masking wired on admin router (identity and DSAR paths exempt)")
	}
	// The rest of the chain, from the quota phase on. A per-key meter at that
	// phase has nothing to count here, because only the API router populates
	// API key claims.
	if chain.holds(core.ChainPhaseQuota) {
		slog.Info("quota enforcement middleware wired on admin router")
	}
	mw = chain.mountRest(mw)
	if cfg.RateLimitRPS > 0 {
		globalRL := apimw.RateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		if cfg.RateLimitPerTenant {
			globalRL = apimw.PerTenantRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		}
		mw = append(mw, api.WithMiddleware(globalRL))
	}
	mw = append(mw, api.WithMiddleware(latencyTracker.Middleware()))
	return mw
}

// buildAPIExtraMW constructs the ordered middleware chain for the API
// router (:3002). Extracted from RunWithOptions to reduce duplication.
func buildAPIExtraMW(ctx context.Context, cfg *config.Config, activator *plugin.Activator,
	drainer *InflightDrainer, backpressureResult apimw.BackpressureResult,
	latencyTracker *apimw.LatencyTracker) []api.RouterOption {
	var mw []api.RouterOption

	mw = append(mw, api.WithMiddleware(drainer.Middleware))
	if cfg.BackpressureEnabled {
		mw = append(mw, api.WithMiddleware(backpressureResult.Middleware))
	}
	mw = append(mw, api.WithMiddleware(apimw.StripForwardedHeaders()))
	if cfg.InstanceRegion != "" {
		geoMW := apimw.GeoRoute(cfg.InstanceRegion)
		mw = append(mw, api.WithMiddleware(geoMW))
	}
	mw = append(mw, idempotencyOption(activator))
	if bf := activator.BruteForceMiddleware(); bf != nil {
		mw = append(mw, api.WithMiddleware(bf))
	}
	if captcha := activator.CaptchaMiddleware(ctx); captcha != nil {
		mw = append(mw, api.WithMiddleware(captcha))
		slog.Info("captcha middleware wired on api router")
	}
	chain := newPluginChain(activator.ChainMiddleware(core.ChainAPI))
	mw = chain.mountBelow(core.ChainPhaseAnalytics, mw)
	// The request sampler slot (core.RequestSamplerProvider). Its position is
	// the capability.
	if sampler := activator.RequestSampler(); sampler != nil {
		mw = append(mw, api.WithMiddleware(sampler))
	}
	mw = chain.mountBelow(core.ChainPhaseQuota, mw)
	if piiMW := activator.PIIMiddleware(); piiMW != nil {
		mw = append(mw, api.WithMiddleware(piiMW))
	}
	// The quota phase: the tenant quota gate and per-API-key metering. The
	// meter only counts here, because only the API router carries API key
	// auth.
	if chain.holds(core.ChainPhaseQuota) {
		slog.Info("quota gate and api key usage metering middleware wired on API router")
	}
	// The rest of the chain, from the quota phase on: the tenant quota gate,
	// then any write guard at the residency phase.
	mw = chain.mountRest(mw)
	if cfg.RateLimitRPS > 0 {
		globalRL := apimw.RateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		if cfg.RateLimitPerTenant {
			globalRL = apimw.PerTenantRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		}
		mw = append(mw, api.WithMiddleware(globalRL))
	}
	mw = append(mw, api.WithMiddleware(latencyTracker.Middleware()))
	return mw
}

// resolveAndWireStorageProvider connects to the configured storage provider and
// wires it into the host. Errors are logged but non-fatal. Returns the connected
// client or nil.
func resolveAndWireStorageProvider(hostBase core.Host, cfg *config.Config) any {
	logger := slog.With("component", "storage-provider")
	storeProvider := provider.Registry.Get(cfg.StorageDriver)
	if storeProvider == nil {
		// Try detection from the S3 endpoint/bucket.
		for _, sp := range provider.Registry.ByCategory(provider.CategoryStorage) {
			if cfg.StorageS3Endpoint != "" && sp.AutoDetect(cfg.StorageS3Endpoint) {
				storeProvider = sp
				break
			}
		}
	}
	if storeProvider == nil {
		logger.Warn("no storage provider registered for driver, skipping", "driver", cfg.StorageDriver)
		return nil
	}
	sp, ok := storeProvider.(provider.StorageProvider)
	if !ok {
		logger.Warn("detected provider is not a StorageProvider", "name", storeProvider.Name())
		return nil
	}
	storeConfig := provider.StorageConfig{
		Bucket:               cfg.StorageS3Bucket,
		Region:               cfg.StorageS3Region,
		AccessKey:            cfg.StorageS3Key,
		SecretKey:            cfg.StorageS3Secret,
		Endpoint:             cfg.StorageS3Endpoint,
		CDNBaseURL:           cfg.StorageS3CDNBaseURL,
		UseSSL:               cfg.StorageS3UseSSL,
		ForcePathStyle:       cfg.StorageS3ForcePathStyle,
		MultipartThresholdMB: cfg.StorageS3MultipartMB,
	}
	client, err := sp.Connect(context.Background(), storeConfig)
	if err != nil {
		logger.Warn("failed to connect storage provider", "name", sp.Name(), "err", err)
		return nil
	}
	costs := provider.NewCostTracker()
	connected := &provider.StorageConnected{Client: client, Provider: sp, Costs: costs}
	logger.Info("storage provider connected", "name", sp.Name(), "bucket", cfg.StorageS3Bucket)
	if setter, ok := hostBase.(interface{ WithStorageConnected(any) }); ok {
		setter.WithStorageConnected(connected)
	}
	return connected
}

// addProviderHealthProbes adds a health check probe for each connected external
// provider. Each probe is non-Required: a provider outage
// is advisory, not a pod-restart trigger. When a provider is not configured or
// failed to connect, the probe is silently skipped.
func addProviderHealthProbes(healthReg *api.ProbeRegistry, hostBase core.Host) {
	// healthPinger is any connected provider that can be pinged.
	type healthPinger interface {
		HealthCheck(context.Context) error
	}
	// Storage provider health check.
	if sp, ok := hostBase.(core.StorageConnectedProvider); ok {
		if c := sp.StorageConnected(); c != nil {
			if p, ok := c.(healthPinger); ok {
				healthReg.Add(api.NewPingProbe("storage-provider", p.HealthCheck))
			}
		}
	}
}
