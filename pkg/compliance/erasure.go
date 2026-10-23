package compliance

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
// The identifier is an email address, an account id (a UUID string) or an IP
// address, the three keys the engine's erase route accepts. Each eraser
// matches the ones its stores keep and returns (0, nil) for the others.
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
// others resolve the subject through, and makes the fan-out run it last, and
// only once every other eraser has succeeded.
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

// SessionEnder is an account eraser that can sign the subject out without
// erasing the account. The fan-out calls it before any eraser runs, and runs
// the account eraser itself only once every other eraser has succeeded.
//
// An account erased while another owner's eraser failed would leave that
// owner nothing to resolve the subject by on the next run, because the
// account no longer holds the address. Erasing by a kept copy of the address
// instead reaches whoever holds the address next. So the account keeps it
// until everything else is erased, and the subject is signed out at once.
type SessionEnder interface {
	EndSessions(ctx context.Context, identifier string) error
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
// fan-out must call them: everything else first, then the erasures declared
// Final, then the account erasers.
//
// See AccountEraser. An eraser that rewrites the address the others resolve
// through has to go last or it takes the key away from them. Registration
// order would put it first, because the engine registers before the plugin
// activator runs. A Final erasure goes just before it, because the account
// eraser resolves nothing once it has run.
//
// The rest keep the order they registered in, so this changes nothing for a
// registry with no account eraser and no Final erasure in it.
func (r *SubjectEraserRegistry) Erasers() []SubjectEraser {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]SubjectEraser, 0, len(r.erasers))
	var final, last []SubjectEraser
	for _, e := range r.erasers {
		if _, isAccount := e.(AccountEraser); isAccount {
			last = append(last, e)
			continue
		}
		if d, ok := e.(*DeclaredEraser); ok && d.final {
			final = append(final, e)
			continue
		}
		out = append(out, e)
	}
	return append(append(out, final...), last...)
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
// Failed names, by EraserName and in fan-out order, every eraser that returned
// an error and every one that never ran because the context ended. The error
// a run returns is only the first of them, and a caller that records or
// retries an incomplete erasure needs all of them.
//
// Shared counts the subject's addresses no eraser reached because another
// account in the same tenant holds them too (see ResolveSubjectAddresses).
// Rows under them stay, because they cannot be told apart from the other
// account's data, so a run with any is unfinished although every eraser
// succeeded.
type ErasureResult struct {
	Rows   int64
	Holds  []HoldRef
	Failed []string
	Shared int
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
// then fans the identifier out to all registered erasers. The subject is
// signed out first, and the subject's addresses are then resolved for every
// eraser to read (see SubjectAddresses). Each eraser is called independently,
// and a failure in one stops none of the others except the account eraser
// and the erasures declared Final, which run only once every other eraser has
// succeeded (see SessionEnder). Errors are
// logged and the first non-nil error is returned alongside the total rows
// affected, and the result names every eraser that failed or never ran.
//
// A subject under an active hold is not erased and not an error: the run
// returns zero rows and names the holds, so the requester is told their
// request is deferred rather than quietly given a success that erased nothing.
func RunSubjectErasureWithHolds(ctx context.Context, identifier string) (ErasureResult, error) {
	identifier = canonicalSubject(identifier)
	holds, err := CheckSubjectHolds(ctx, identifier)
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
	return runSubjectErasureOn(ctx, globalEraserRegistry, identifier)
}

// CheckSubjectHolds asks the registered checker which holds cover the
// subject, and applies LYEVE_GDPR_HOLD_CHECK when the checker cannot answer:
// ErrHoldCheckUnavailable by default, no holds when the install set proceed.
// Every erasure path asks through it, so a plugin's own erasure route answers
// a failed check as the engine's does.
//
// No registered checker means no hold can exist: the erasure proceeds. That
// is the only case where an absent answer is a safe answer, and it is why
// this does not collapse into one fail-open or fail-closed policy.
func CheckSubjectHolds(ctx context.Context, identifier string) ([]HoldRef, error) {
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
// the request that produced it. The reference is an HMAC of the identifier
// under the install's subject reference key, so it correlates every line about
// one subject, across replicas and restarts, and a list of candidate addresses
// matches none of them without that key. Whoever holds the key can still
// compute the reference for an address they already know.
func subjectRef(identifier string) string {
	mac := hmac.New(sha256.New, *subjectRefKey.Load())
	mac.Write([]byte(identifier))
	return hex.EncodeToString(mac.Sum(nil)[:6])
}

// SubjectRef is the reference an eraser outside this package logs in place of
// the identifier, for the reason subjectRef gives. It matches the reference
// the fan-out logs, so the lines of one erasure run still correlate.
func SubjectRef(identifier string) string { return subjectRef(identifier) }

// subjectRefInfo names the subject reference key in its derivation, so the
// install secret it comes from is never used raw and no other key derived
// from that secret equals it.
const subjectRefInfo = "lyeve subject reference key v1"

// subjectRefKey keys subjectRef. It starts random for the process, so a
// reference logged before the runtime installs the install's key, or by a
// test or a tool that never does, matches nothing outside the process.
var subjectRefKey atomic.Pointer[[]byte]

func init() {
	key := make([]byte, sha256.Size)
	_, _ = rand.Read(key)
	subjectRefKey.Store(&key)
}

// SetSubjectRefSecret keys every later subject reference to secret, through a
// key derived with HKDF-SHA-256. The runtime calls it once at boot with a
// secret every replica shares and the install keeps across restarts, so the
// references agree wherever and whenever a line is written. An empty secret is
// refused, because a key derived from it is the same on every install.
func SetSubjectRefSecret(secret []byte) error {
	if len(secret) == 0 {
		return fmt.Errorf("subject reference secret is empty")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, subjectRefInfo, sha256.Size)
	if err != nil {
		return fmt.Errorf("derive subject reference key: %w", err)
	}
	subjectRefKey.Store(&key)
	return nil
}

// SubjectDigest is how an audit entry names the subject of an export or an
// erasure: "sha256:" and the hex SHA-256 of the identifier as the request
// gave it. The audit log outlives the erasure, so it must not keep the
// address the erasure removed, but it has to prove the request happened.
// Anyone holding the identifier can recompute the digest and find the entry,
// and the log alone does not say who it was about. The digest is whole and
// unkeyed, unlike subjectRef, because the entry is evidence that has to be
// found from the identifier rather than a correlation key.
func SubjectDigest(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return "sha256:" + hex.EncodeToString(sum[:])
}

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

func runSubjectErasureOn(ctx context.Context, reg *SubjectEraserRegistry, identifier string) (ErasureResult, error) {
	erasers := reg.Erasers()
	var res ErasureResult
	fail := func(e SubjectEraser) {
		if name := EraserName(e); !slices.Contains(res.Failed, name) {
			res.Failed = append(res.Failed, name)
		}
	}
	if err := ctx.Err(); err != nil {
		for _, e := range erasers {
			fail(e)
		}
		return res, err
	}
	ref := subjectRef(identifier)

	// The subject is signed out and locked before anything is erased, so a
	// run that stops partway leaves an account nobody can sign in to. When
	// the sign-out fails nothing runs: an erasure that went ahead would leave
	// the subject signed in, writing rows behind the erasers that had run.
	for _, eraser := range erasers {
		ender, ok := eraser.(SessionEnder)
		if !ok {
			continue
		}
		if err := ender.EndSessions(ctx, identifier); err != nil {
			slog.Error("subject_sign_out_failed",
				"eraser", EraserName(eraser),
				"subject_ref", ref,
				"err", err,
			)
			for _, e := range erasers {
				fail(e)
			}
			return res, fmt.Errorf("%w: %w", ErrSubjectNotSignedOut, err)
		}
	}

	// The subject's addresses are read once, now, from the rows as they
	// stand, and every eraser matches against the same set. A set that
	// cannot be read is not guessed at: every eraser would miss what it left
	// out, and the account eraser would then take the key to it away.
	ctx, err := ResolveSubjectAddresses(ctx, identifier, erasers)
	if err != nil {
		slog.Error("subject_addresses_unresolved",
			"subject_ref", ref,
			"err", err,
		)
		for _, e := range erasers {
			fail(e)
		}
		return res, fmt.Errorf("%w: %w", ErrSubjectAddressesUnresolved, err)
	}
	res.Shared = sharedSubjectAddresses(ctx)

	var total int64
	var firstErr error
	othersFailed := false
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
			for _, e := range erasers[i:] {
				fail(e)
			}
			res.Rows = total
			return res, err
		}
		_, isAccount := eraser.(AccountEraser)
		declared, _ := eraser.(*DeclaredEraser)
		if (isAccount || declared != nil && declared.final) && othersFailed {
			// The account keeps the address the failed owners resolve the
			// subject by, and the next run erases it. A Final erasure keeps
			// the rows the next run reads the subject's addresses from.
			slog.Warn("subject_erasure_account_deferred",
				"eraser", EraserName(eraser),
				"subject_ref", ref,
			)
			fail(eraser)
			continue
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
			fail(eraser)
			if !isAccount {
				othersFailed = true
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
	res.Rows = total
	return res, firstErr
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
