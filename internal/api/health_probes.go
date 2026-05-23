package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"golang.org/x/sys/unix"
)

// Types

// ProbeResult is the outcome of a single health probe check.
type ProbeResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
	Took   string `json:"took"`
}

// ProbeReport is the full health check response body.
type ProbeReport struct {
	Status  string        `json:"status"` // "ok" | "degraded"
	Checked string        `json:"checked_at"`
	Results []ProbeResult `json:"checks"`
}

// ProbeCheck is a named health probe function.
type ProbeCheck struct {
	Name     string
	Check    func(ctx context.Context) error
	Required bool // false -> failure is advisory, not fatal
	// Liveness marks a probe as an answer to "is this process broken beyond
	// recovery". Only a failure a restart can actually fix belongs here.
	// Saturation and dependency probes must not: /healthz is wired to
	// kubelet's livenessProbe, which kills the container, and killing the pod
	// that is busy serving does not reduce the load that saturated it.
	Liveness bool

	// PublicError says this probe's error text is written to be read by
	// anyone. /healthz, /readyz and /startup take no credential, so whatever
	// a probe returns is served to whoever asks.
	//
	// The default is false and the caller gets "unavailable". A probe that
	// passes a driver error straight through answers an anonymous request
	// with the host, the port and sometimes the credential state of a backend:
	// go-redis says "dial tcp <host>:<port>" or "NOAUTH", an S3 client names
	// the endpoint and the bucket, and a plugin returns whatever it wrote.
	//
	// Set it only on a probe whose failure text is a fixed string the probe
	// itself chose, never on one that wraps an error from somewhere else.
	PublicError bool
}

// probeUnavailable is what an anonymous caller is told about a probe that
// did not declare its error text public.
const probeUnavailable = "unavailable"

// publicProbeError returns the failure text to serve, and logs the real one.
//
// The probe endpoints are the only routes that answer with no credential and
// report on the engine's dependencies, so this is the one place where an
// error reaching a response is not a bug to be fixed at the source but a
// decision the probe has to have made on purpose.
func publicProbeError(c ProbeCheck, err error) string {
	if c.PublicError {
		return err.Error()
	}
	slog.Warn("health probe failed",
		"probe", c.Name,
		"required", c.Required,
		"err", err,
	)
	return probeUnavailable
}

// ProbeRegistry manages health probe registration and execution.
//
// Startup probe caches the first successful full-check timestamp.
// Once startup passes, /startup always returns 200 without re-running checks.
//
// During graceful shutdown, SetDraining() flips the readiness probe to
// return 503 so load balancers deregister the pod before the server stops
// accepting new connections.
type ProbeRegistry struct {
	mu              sync.RWMutex
	checks          []ProbeCheck
	startupPassedAt time.Time
	draining        atomic.Bool
}

// NewProbeRegistry returns an empty ProbeRegistry.
func NewProbeRegistry() *ProbeRegistry {
	return &ProbeRegistry{}
}

// Add registers a health probe. Add before the server starts: it is not
// goroutine-safe with concurrent Run calls.
func (r *ProbeRegistry) Add(check ProbeCheck) {
	r.checks = append(r.checks, check)
}

// Liveness runs the probes marked Liveness, which in practice is the DB ping.
// Returns an error if the DB is unreachable.
//
// It deliberately ignores the other Required checks. Those answer "should this
// pod take traffic", which is [Readiness]. Running them here would let a
// saturated connection pool fail the kubelet's livenessProbe and restart the
// container under the load that saturated it.
func (r *ProbeRegistry) Liveness(ctx context.Context) (ProbeReport, error) {
	// If no checks are registered, return ok (nothing to check).
	r.mu.RLock()
	checks := r.checks
	r.mu.RUnlock()

	report := ProbeReport{
		Status:  "ok",
		Checked: time.Now().UTC().Format(time.RFC3339),
	}

	for _, c := range checks {
		if !c.Liveness {
			continue
		}
		start := time.Now()
		err := c.Check(ctx)
		elapsed := time.Since(start)
		pr := ProbeResult{
			Name:   c.Name,
			Passed: err == nil,
			Took:   elapsed.String(),
		}
		if err != nil {
			pr.Error = publicProbeError(c, err)
		}
		report.Results = append(report.Results, pr)
	}

	return report, nil
}

// Readiness runs all registered checks. Returns the report and a boolean
// indicating whether all Required checks passed.
func (r *ProbeRegistry) Readiness(ctx context.Context) (ProbeReport, bool) {
	r.mu.RLock()
	checks := r.checks
	r.mu.RUnlock()

	report := ProbeReport{
		Status:  "ok",
		Checked: time.Now().UTC().Format(time.RFC3339),
	}
	allOK := true

	for _, c := range checks {
		start := time.Now()
		err := c.Check(ctx)
		elapsed := time.Since(start)
		pr := ProbeResult{
			Name:   c.Name,
			Passed: err == nil,
			Took:   elapsed.String(),
		}
		if err != nil {
			pr.Error = publicProbeError(c, err)
			if c.Required {
				allOK = false
			}
		}
		report.Results = append(report.Results, pr)
	}

	if !allOK {
		report.Status = "degraded"
	}

	return report, allOK
}

// Startup runs all checks like Readiness, but caches success. Once the
// first full pass occurs, subsequent calls return 200 immediately without
// re-running checks. This matches Kubernetes startup probe semantics.
func (r *ProbeRegistry) Startup(ctx context.Context) (ProbeReport, bool) {
	r.mu.RLock()
	if !r.startupPassedAt.IsZero() {
		r.mu.RUnlock()
		return ProbeReport{
			Status:  "ok",
			Checked: r.startupPassedAt.UTC().Format(time.RFC3339),
			Results: []ProbeResult{
				{Name: "startup", Passed: true, Took: "cached"},
			},
		}, true
	}
	r.mu.RUnlock()

	report, allOK := r.Readiness(ctx)
	if allOK {
		r.mu.Lock()
		r.startupPassedAt = time.Now()
		r.mu.Unlock()
	}
	return report, allOK
}

// AwaitStartup runs the startup probe every interval until it passes or ctx
// ends, and reports whether it passed. The readiness gate opens on the first
// pass, so without this the gate waits for something outside the process to
// call /startup or /readyz: a Kubernetes probe does, but a plain container,
// a systemd unit or a compose stack with no healthcheck never does, and each
// would serve 503 until the gate's timeout.
func (r *ProbeRegistry) AwaitStartup(ctx context.Context, interval time.Duration) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		_, ok := r.Startup(probeCtx)
		cancel()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// StartTime returns the time the startup probe first passed, or the zero
// time if it has never passed.
func (r *ProbeRegistry) StartTime() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.startupPassedAt
}

// SetDraining marks the registry as draining. Once set, /readyz returns
// 503 regardless of probe results so load balancers deregister the pod.
// /healthz is unaffected: the pod should still be considered alive during
// the pre-stop drain window.
//
// SetDraining is idempotent and goroutine-safe.
func (r *ProbeRegistry) SetDraining() {
	r.draining.Store(true)
}

// IsDraining reports whether the registry is in the draining state.
func (r *ProbeRegistry) IsDraining() bool {
	return r.draining.Load()
}

// Built-in probe constructor helpers

// NewDBProbe returns a ProbeCheck that pings the database. It is the only
// probe that answers liveness: an engine that cannot reach its database has
// nothing to serve, and it is the one check whose failure a restart can clear
// (a wedged pool, an exhausted file descriptor table).
func NewDBProbe(ping func(context.Context) error) ProbeCheck {
	return ProbeCheck{
		Name:     "database",
		Check:    ping,
		Required: true,
		Liveness: true,
		// The caller returns a fixed string rather than the driver's text,
		// for the same reason every handler does.
		PublicError: true,
	}
}

// NewRedisProbe returns a ProbeCheck that pings the distributed cache backing
// refresh tokens. ping should call redis.Client.Ping(ctx).Err().
//
// required decides whether an outage takes the pod out of service. Every
// replica shares this dependency, so a required probe unreadies all of them at
// once and turns a degraded service into no service. Degraded is survivable:
// access tokens keep working for their lifetime and a refresh answers 503 with
// SESSION_STORE_UNAVAILABLE, which asks the client to retry rather than telling
// it the session is gone. Operators who would rather fail closed set
// HEALTH_REDIS_PROBE_REQUIRED=true.
func NewRedisProbe(ping func(context.Context) error, required bool) ProbeCheck {
	return ProbeCheck{
		Name:     "redis",
		Check:    ping,
		Required: required,
	}
}

// NewDiskProbe returns a ProbeCheck that checks available disk space on path.
// minFreeBytes is the threshold in bytes. The check fails when available
// space drops below it.
func NewDiskProbe(path string, minFreeBytes uint64) ProbeCheck {
	return ProbeCheck{
		Name: "disk",
		// Every failure this probe returns is a fixed string it chose.
		PublicError: true,
		Check: func(ctx context.Context) error {
			var stat unix.Statfs_t
			if err := unix.Statfs(path, &stat); err != nil {
				// Fallback: use syscall for broader compat.
				var s syscall.Statfs_t
				if err2 := syscall.Statfs(path, &s); err2 != nil {
					// The syscall error names the path it could not stat, and
					// this text is served without a credential, so it is a
					// fixed string like every other return from this probe.
					slog.Warn("disk probe: statfs failed", "path", path, "err", err2)
					return errors.New("disk unavailable")
				}
				avail := s.Bavail * uint64(s.Bsize)
				if avail < minFreeBytes {
					return &diskSpaceError{path: path, available: avail, required: minFreeBytes}
				}
				return nil
			}
			avail := stat.Bavail * uint64(stat.Bsize)
			if avail < minFreeBytes {
				return &diskSpaceError{path: path, available: avail, required: minFreeBytes}
			}
			return nil
		},
		Required: true,
	}
}

type diskSpaceError struct {
	path      string
	available uint64
	required  uint64
}

// Error returns a human-readable disk-space error message.
func (e *diskSpaceError) Error() string {
	return "low disk space"
}

// NewPluginReadinessProbe returns a ProbeCheck that verifies all running
// plugins report ready via the ReadinessReporter interface. Non-Required
// so a single slow plugin does not take the pod out of service, but still
// surfaces in the probe report for operators.
func NewPluginReadinessProbe(ready func(context.Context) error) ProbeCheck {
	return ProbeCheck{
		Name:     "plugins",
		Check:    ready,
		Required: false, // advisory: operators see status, k8s doesn't unroute
	}
}

// NewPingProbe returns a ProbeCheck for a provider-level ping check, such as
// the storage provider's. It is not Required, so a provider outage does not
// take the pod out of service.
func NewPingProbe(name string, ping func(context.Context) error) ProbeCheck {
	return ProbeCheck{
		Name:     name,
		Check:    ping,
		Required: false, // advisory: a provider outage shouldn't trigger pod restart
	}
}

// NewGoroutineProbe returns a ProbeCheck that checks the number of goroutines
// against maxGoroutines. When the count exceeds the threshold, the check
// returns an advisory (non-Required) failure: high goroutine count is a
// warning, not a restart trigger.
func NewGoroutineProbe(maxGoroutines int) ProbeCheck {
	return ProbeCheck{
		Name: "goroutines",
		Check: func(ctx context.Context) error {
			n := runtime.NumGoroutine()
			if n > maxGoroutines {
				return &goroutineCountError{current: n, max: maxGoroutines}
			}
			return nil
		},
		Required: false, // advisory: high goroutine count is a warning
	}
}

type goroutineCountError struct {
	current int
	max     int
}

// Error returns a human-readable goroutine-count error message.
func (e *goroutineCountError) Error() string {
	return "goroutine count exceeds threshold"
}

// NewPoolUtilizationProbe returns a ProbeCheck that verifies the database
// connection pool is not near exhaustion. statsFn returns the current pool
// stats on each check. maxUtilization is the ceiling (0-1): when
// InUse/MaxOpen exceeds it, the check fails.
//
// Saturation is advisory. Readiness answers "can this pod serve", not "is it
// busy": taking a saturated pod out of service moves its load to its peers,
// which are under the same load and cross the same threshold, so the endpoint
// set drains to nothing exactly at peak traffic. deploy/kubernetes.yaml runs a
// single replica, where the first pod to cross the line is the whole service.
// A pod at 90% utilization still answers requests, slowly. A pod with no
// endpoints answers none. Operators still see this in the readiness report,
// and it is the right signal to scale on.
func NewPoolUtilizationProbe(statsFn func() (inUse, maxOpen int), maxUtilization float64) ProbeCheck {
	return ProbeCheck{
		Name: "pool_utilization",
		// poolUtilizationError writes a fixed string on purpose: the probe is
		// served without a credential, so the pool's own numbers stay out.
		PublicError: true,
		Check: func(ctx context.Context) error {
			inUse, maxOpen := statsFn()
			if maxOpen <= 0 {
				return nil // pool not configured: skip check
			}
			util := float64(inUse) / float64(maxOpen)
			if util > maxUtilization {
				return &poolUtilizationError{inUse: inUse, maxOpen: maxOpen, utilization: util, threshold: maxUtilization}
			}
			return nil
		},
		Required: false,
	}
}

type poolUtilizationError struct {
	inUse       int
	maxOpen     int
	utilization float64
	threshold   float64
}

// Error returns a human-readable pool-utilization error message.
func (e *poolUtilizationError) Error() string {
	return "pool utilization exceeds threshold"
}

// HTTP handlers

const healthProbeTimeout = 5 * time.Second

// healthzHandler returns the liveness probe handler.
// GET /healthz
// Auth: public (no auth, Kubernetes probe).
func healthzHandler(reg *ProbeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
		defer cancel()

		report, _ := reg.Liveness(ctx)
		allOK := true
		for _, pr := range report.Results {
			if !pr.Passed {
				allOK = false
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if !allOK {
			report.Status = "degraded"
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = jsonpool.WriteJSON(w, report) // err suppressed: response write to client
	}
}

// readyzHandler returns the readiness probe handler.
// GET /readyz
// Auth: public (no auth, Kubernetes probe).
// When draining, returns 503 immediately so load balancers deregister the pod.
func readyzHandler(reg *ProbeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Draining: return 503 immediately. Load balancers should
		// deregister the pod on the next health check cycle.
		if reg.IsDraining() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = jsonpool.WriteJSON(w, ProbeReport{
				Status:  "draining",
				Checked: time.Now().UTC().Format(time.RFC3339),
				Results: []ProbeResult{
					{Name: "draining", Passed: false, Error: "pod is draining", Took: "0s"},
				},
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
		defer cancel()

		// ReadinessGate refuses every non-probe request until the startup probe
		// has passed once, so a readyz that answers 200 before then tells a load
		// balancer to send traffic the node can only answer with 503 "service
		// initializing". The readiness checks do not cover it: they test the
		// dependencies, not whether the engine has finished coming up.
		//
		// Run the probe rather than reading its flag. Nothing runs it at boot,
		// only the /startup handler does, so a readyz that merely waited for
		// the flag would stay 503 until something polled /startup, which many
		// deployments never do. Startup caches its pass, so this costs one run.
		if reg.StartTime().IsZero() {
			if _, ok := reg.Startup(ctx); !ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = jsonpool.WriteJSON(w, ProbeReport{
					Status:  "starting",
					Checked: time.Now().UTC().Format(time.RFC3339),
					Results: []ProbeResult{
						{Name: "startup", Passed: false, Error: "startup probe has not passed yet", Took: "0s"},
					},
				})
				return
			}
		}

		report, allOK := reg.Readiness(ctx)

		w.Header().Set("Content-Type", "application/json")
		if !allOK {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = jsonpool.WriteJSON(w, report) // err suppressed: response write to client
	}
}

// startupHandler returns the startup probe handler.
// GET /startup
// Auth: public (no auth, Kubernetes probe).
// Caches success. Subsequent calls return 200 without re-running checks.
func startupHandler(reg *ProbeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
		defer cancel()

		report, allOK := reg.Startup(ctx)

		w.Header().Set("Content-Type", "application/json")
		if !allOK {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = jsonpool.WriteJSON(w, report) // err suppressed: response write to client
	}
}

// WithHealthProbes injects a ProbeRegistry so /healthz, /readyz, and /startup
// endpoints are served. When reg is nil, the endpoints return 200 with an
// empty probe report (no checks registered).
func WithHealthProbes(reg *ProbeRegistry) RouterOption {
	return func(o *routerOptions) {
		o.healthProbes = reg
	}
}
