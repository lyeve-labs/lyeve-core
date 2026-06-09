package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Subject erasure (GDPR Art.17 right to be forgotten)
//
// ## Scoping
//
// The /api/admin/gdpr/* endpoints require super_admin, a platform role. An
// identifier is unique for accounts and not for anything else, and the
// difference decides whether a bundle is complete or a breach.
//
// An ACCOUNT is globally unique. `sys_users.email` carries a single-column
// UNIQUE constraint on all three dialects and user ids are UUIDs, so an
// identifier names at most one account across every tenant.
//
// PLUGIN data is not. An email recipient, a form submission and a comment
// author are arbitrary strings stored per tenant: the same address may
// reference different people in different tenants. So a sweep keyed on an
// address can collect rows belonging to two people, and merging them hands one
// controller the other's data.
//
// Implementors receive the full request context including tenant claims via
// core.TenantIDFromCtx(ctx). Tenant-scoped stores are expected to use it. The
// contract does not require it, because engine-owned tables like sys_users are
// correctly answered across every tenant.
//
// Export keys its results by tenant and never merges them.
// RunSubjectExportAcrossTenants is the sweep, and the caller has to ask for it.

// SubjectEraser is an interface that plugins implement to support
// GDPR Art.17 right to erasure. A plugin declares its erasers through
// SubjectEraserDeclarer, which the engine reads whether or not the plugin
// starts. The coordinator fans out erasure requests to every registered
// eraser independently.
//
// The identifier is plugin-defined: an email address, user ID (UUID string),
// IP address, or any other key that identifies the data subject's records
// within the plugin's stores.
type SubjectEraser interface {
	// EraseSubject deletes or fully anonymizes all PII for the given
	// identifier in this plugin's stores. Returns the number of rows
	// affected (deleted or updated).
	//
	// Implementations must handle the case where no matching records
	// exist: return (0, nil), not an error.
	EraseSubject(ctx context.Context, identifier string) (int64, error)
}

// AccountEraser marks the one eraser that rewrites the account record the
// others resolve the subject through, and makes the fan-out run it last.
//
// A DSAR names a person by address far more often than by id, and a plugin
// store keys its rows by user id. So a plugin resolves the one to the other,
// through sys_users, at the moment it is called. The account eraser replaces
// that address with a per-row sentinel, so every resolution after it returns
// nothing. It runs last, because the other erasers resolve the subject through
// the account record it rewrites. Run first, it would leave every plugin that
// looks the subject up answering zero, which reads exactly like a person with
// no data anywhere else.
type AccountEraser interface {
	SubjectEraser

	// ErasesAccount marks this eraser as the one that rewrites the account
	// record. It is never called.
	ErasesAccount()
}

// SubjectEraserRegistry holds registered erasers. Safe for concurrent use.
type SubjectEraserRegistry struct {
	mu      sync.Mutex
	erasers []SubjectEraser
}

// Register adds an eraser. Nil erasers are silently ignored, and registering
// the same eraser twice is a no-op, so a plugin may register from Start and
// again when reconfigured.
//
// The dedupe is load-bearing rather than tidiness. Without it the registry
// would grow on every reconfiguration and one erasure would run the same
// UPDATE once per registration. Consuming SubjectEraserProvider gives the same
// eraser a second route in, which would double it on the first boot.
//
// An eraser whose dynamic type cannot be compared is appended without the
// check rather than panicking on it. Every registered eraser is a pointer,
// so this guards against a future one rather than a live case.
func (r *SubjectEraserRegistry) Register(e SubjectEraser) {
	if e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reflect.TypeOf(e).Comparable() {
		for _, existing := range r.erasers {
			if existing == e {
				return
			}
		}
	}
	r.erasers = append(r.erasers, e)
}

// Erasers returns a snapshot copy of registered erasers, in the order the
// fan-out must call them: everything else first, then the account erasers.
//
// See AccountEraser. An eraser that rewrites the address the others resolve
// through has to go last or it takes the key away from them. Registration
// order would put it first, because the engine registers before the plugin
// activator runs.
//
// The rest keep the order they registered in, so this changes nothing for a
// registry with no account eraser in it.
func (r *SubjectEraserRegistry) Erasers() []SubjectEraser {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]SubjectEraser, 0, len(r.erasers))
	var last []SubjectEraser
	for _, e := range r.erasers {
		if _, isAccount := e.(AccountEraser); isAccount {
			last = append(last, e)
			continue
		}
		out = append(out, e)
	}
	return append(out, last...)
}

// Len returns the number of registered erasers.
func (r *SubjectEraserRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.erasers)
}

var globalEraserRegistry = &SubjectEraserRegistry{}

// RegisterSubjectEraser registers an eraser on the global registry. The
// engine calls it for its own tables and for every declared erasure. A
// plugin that calls it from Start reaches erasure only while it runs, so its
// tables belong in SubjectEraserDeclarer instead.
func RegisterSubjectEraser(e SubjectEraser) {
	globalEraserRegistry.Register(e)
}

// SubjectErasers returns a snapshot of the globally registered erasers, in the
// order the fan-out calls them. Exposed so the boot path and its tests can
// report what erasure will actually touch without running one.
func SubjectErasers() []SubjectEraser {
	return globalEraserRegistry.Erasers()
}

// ErasureResult is what an erasure run did. Holds is non-empty when an active
// legal hold stopped it, in which case Rows is zero and nothing was erased.
type ErasureResult struct {
	Rows  int64
	Holds []HoldRef
}

// RunSubjectErasure fans out the given identifier to all registered erasers
// and returns the rows affected. Legal holds are honored. Use
// RunSubjectErasureWithHolds when the caller needs to report which hold
// stopped the run.
func RunSubjectErasure(ctx context.Context, identifier string) (int64, error) {
	res, err := RunSubjectErasureWithHolds(ctx, identifier)
	return res.Rows, err
}

// RunSubjectErasureWithHolds consults the registered legal-hold checker and
// then fans the identifier out to all registered erasers. Each eraser is
// called independently. A failure in one does not stop the others. Errors are
// logged and the first non-nil error is returned alongside the total rows
// affected.
//
// A subject under an active hold is not erased and not an error: the run
// returns zero rows and names the holds, so the requester is told their
// request is deferred rather than quietly given a success that erased nothing.
func RunSubjectErasureWithHolds(ctx context.Context, identifier string) (ErasureResult, error) {
	holds, err := subjectHolds(ctx, identifier)
	if err != nil {
		return ErasureResult{}, err
	}
	if len(holds) > 0 {
		slog.InfoContext(ctx, "subject_erasure_held",
			"subject_ref", subjectRef(identifier),
			"holds", len(holds),
		)
		return ErasureResult{Holds: holds}, nil
	}
	rows, err := runSubjectErasureOn(ctx, globalEraserRegistry, identifier)
	return ErasureResult{Rows: rows}, err
}

// subjectHolds asks the registered checker which holds cover the subject.
//
// No registered checker means no hold can exist: the erasure proceeds. That
// is the only case where an absent answer is a safe answer, and it is why
// this does not collapse into one fail-open or fail-closed policy.
func subjectHolds(ctx context.Context, identifier string) ([]HoldRef, error) {
	checker := RegisteredHoldChecker()
	if checker == nil {
		return nil, nil
	}
	holds, err := checker.SubjectHolds(ctx, identifier)
	if err == nil {
		return holds, nil
	}
	if proceedOnHoldCheckFailure() {
		slog.WarnContext(ctx, "legal hold check failed; erasing anyway because LYEVE_GDPR_HOLD_CHECK=proceed",
			"subject_ref", subjectRef(identifier),
			"err", err,
		)
		return nil, nil
	}
	slog.ErrorContext(ctx, "legal hold check failed; refusing erasure rather than risk destroying held evidence",
		"subject_ref", subjectRef(identifier),
		"err", err,
	)
	return nil, ErrHoldCheckUnavailable
}

// subjectRef derives a stable reference to a data subject for logging.
//
// Erasure exists to remove the subject's data, so naming the subject in the
// log undoes it where nobody thinks to look, and log retention usually outlives
// the request that produced it. A truncated digest still correlates every line
// belonging to one erasure run without recording who it was about.
func subjectRef(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(sum[:6])
}

// SubjectRef is the reference an eraser outside this package logs in place of
// the identifier, for the reason subjectRef gives. It matches the reference
// the fan-out logs, so the lines of one erasure run still correlate.
func SubjectRef(identifier string) string { return subjectRef(identifier) }

// slowEraser is the point past which an eraser is reported even though it
// matched nothing. A no-match that takes this long is a scan, and a scan is
// what makes the fan-out miss the write timeout.
const slowEraser = 250 * time.Millisecond

// EraserName returns the owning package of an eraser, which is the plugin it
// belongs to. The registry holds bare interface values, so without a name the
// log could only give an index, and reading one would mean reconstructing boot
// order by hand before telling which plugin was slow or failing.
func EraserName(e SubjectEraser) string {
	if e == nil {
		return "unknown"
	}
	if named, ok := e.(interface{ EraserName() string }); ok {
		return named.EraserName()
	}
	t := reflect.TypeOf(e)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	pkg := t.PkgPath()
	if pkg == "" {
		return t.Name()
	}
	// A plugin's package path ends in <repo>/plugin, so the segment before the
	// leaf is the one that identifies it.
	parts := strings.Split(pkg, "/")
	last := parts[len(parts)-1]
	if last == "plugin" && len(parts) > 1 {
		last = parts[len(parts)-2]
	}
	return strings.TrimPrefix(last, "lyeve-plugin-")
}

func runSubjectErasureOn(ctx context.Context, reg *SubjectEraserRegistry, identifier string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	erasers := reg.Erasers()
	ref := subjectRef(identifier)
	var total int64
	var firstErr error
	for i, eraser := range erasers {
		if err := ctx.Err(); err != nil {
			// Erasure is a committed side effect, so a fan-out abandoned partway
			// leaves the subject partly erased. The run returns the rows already
			// gone and logs that it is incomplete, rather than reporting an
			// erasure that did nothing.
			slog.Error("subject_erasure_abandoned",
				"subject_ref", ref,
				"erasers_run", i,
				"erasers_pending", len(erasers)-i,
				"rows", total,
				"err", err,
			)
			return total, err
		}
		started := time.Now()
		n, err := eraser.EraseSubject(ctx, identifier)
		elapsed := time.Since(started)
		total += n
		if err != nil {
			slog.Error("subject_erasure_failed",
				"eraser_index", i,
				"eraser", EraserName(eraser),
				"subject_ref", ref,
				"duration_ms", elapsed.Milliseconds(),
				"err", err,
			)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A slow eraser is otherwise invisible: the fan-out is sequential, so
		// one full scan sets the latency of the whole request and nothing in
		// the log says which one it was.
		if n > 0 || elapsed >= slowEraser {
			slog.Info("subject_erasure",
				"eraser_index", i,
				"eraser", EraserName(eraser),
				"subject_ref", ref,
				"duration_ms", elapsed.Milliseconds(),
				"rows", n,
			)
		}
	}
	return total, firstErr
}

// SubjectEraserProvider is an optional interface a plugin can implement to
// expose its eraser rather than registering it from Start.
//
// The runtime type-asserts every active plugin to this interface after
// activation and registers what it hands back, so implementing it is enough on
// its own. Calling RegisterSubjectEraser as well is safe: Register ignores an
// eraser it already holds.
type SubjectEraserProvider interface {
	core.Plugin
	SubjectEraser() SubjectEraser
}

// Retention purge

// Purger is an optional interface that plugins implement to support
// periodic retention-based data purging (GDPR Art.5 storage limitation).
// Plugins with time-limited PII implement this to delete expired records
// on a configurable schedule.
type Purger interface {
	// PurgeExpired deletes records that have exceeded their retention
	// period. Returns the number of rows deleted.
	PurgeExpired(ctx context.Context) (int64, error)

	// PurgeInterval returns the suggested interval between purge runs.
	// Nothing in the engine schedules purges, so the plugin runs its own
	// loop. A zero or negative duration means purging is disabled.
	PurgeInterval() time.Duration
}

// PurgerRegistry holds registered purgers.
type PurgerRegistry struct {
	mu      sync.Mutex
	purgers []Purger
}

var globalPurgerRegistry = &PurgerRegistry{}

// RegisterPurger registers a plugin's purger on the global registry. Nothing
// in the engine reads the registry, so registering schedules no purge.
func RegisterPurger(p Purger) {
	if p == nil {
		return
	}
	globalPurgerRegistry.mu.Lock()
	defer globalPurgerRegistry.mu.Unlock()
	globalPurgerRegistry.purgers = append(globalPurgerRegistry.purgers, p)
}

// Purgers returns a snapshot copy of registered purgers.
func (r *PurgerRegistry) Purgers() []Purger {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Purger, len(r.purgers))
	copy(out, r.purgers)
	return out
}

// ResetSubjectErasers replaces the global eraser registry with a fresh empty
// one. Use in tests to start with a clean slate before registering erasers.
func ResetSubjectErasers() {
	globalEraserRegistry = &SubjectEraserRegistry{}
}

// ResetPurgers replaces the global purger registry with a fresh empty one.
// Use in tests to start with a clean slate before registering purgers.
func ResetPurgers() {
	globalPurgerRegistry = &PurgerRegistry{}
}
