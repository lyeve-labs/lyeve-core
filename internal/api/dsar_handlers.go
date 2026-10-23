package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// DSARHandler serves GDPR Data Subject Access Request endpoints.
// Mounted on the admin router under /api/admin/gdpr/.
type DSARHandler struct {
	// auditLog is an optional audit-log writer. When non-nil, DSAR
	// operations are recorded in the audit trail. May be nil when no audit
	// writer is wired.
	auditLog compliance.DSARAuditWriter

	// tenants enumerates the install's tenants, and tenantScope binds a
	// context to one of them. Both nil on a single-tenant install, where a
	// cross-tenant export is refused rather than quietly answered from one
	// tenant.
	tenants     compliance.TenantLister
	tenantScope compliance.TenantScopeFunc
}

// NewDSARHandler constructs a DSARHandler. Every argument may be nil.
func NewDSARHandler(
	auditLog compliance.DSARAuditWriter,
	tenants compliance.TenantLister,
	tenantScope compliance.TenantScopeFunc,
) *DSARHandler {
	return &DSARHandler{auditLog: auditLog, tenants: tenants, tenantScope: tenantScope}
}

// The 320-character ceiling on both identifiers is the longest address RFC 5321
// allows, 64 for the local part plus 1 for the separator plus 255 for the
// domain. A data subject is named by an address or an id, so nothing longer can
// identify one.
//
// The bound is not cosmetic. An unbounded identifier is compared against columns
// narrower than itself, and SQL Server refuses the comparison outright rather
// than reporting no match, so an unbounded identifier would fail every eraser
// with a 5xx for input that could never have matched a row.

const errNotASubject = "identifier must be an email address, an account id or an IP address"

// namesASubject reports whether the identifier is an email address, an
// account id or an IP address, the keys the erasers look a subject up by.
// The free-text erasers redact every standalone match of the identifier, some
// of them in every tenant, so a bare word such as a first name would be
// redacted wherever it appears, in the records of people who never asked.
func namesASubject(identifier string) bool {
	if _, err := uuid.Parse(identifier); err == nil {
		return true
	}
	if net.ParseIP(identifier) != nil {
		return true
	}
	at := strings.LastIndexByte(identifier, '@')
	return at > 0 && at < len(identifier)-1
}

// ExportRequest is the JSON body for POST /api/admin/gdpr/export.
type ExportRequest struct {
	Identifier string `json:"identifier" validate:"required,min=1,max=320"`

	// AllTenants sweeps every tenant instead of the one the request resolves
	// to. It has to be asked for. A super_admin carries no tenant claim, so
	// without this flag an unscoped request would sweep every tenant by default
	// and a forgotten header would read as a request for everyone's data.
	//
	// The bundle comes back keyed by tenant and is never merged flat: the same
	// identifier can name different people in two tenants, so a merged bundle
	// hands one controller another person's records.
	AllTenants bool `json:"all_tenants"`
}

// erasureRunLimit bounds one admin erasure. A scan of five million rows
// takes about a minute on the slowest dialect, and a run makes several, so
// fifteen minutes leaves room for the largest tenant while still ending a
// run that is stuck rather than slow.
const erasureRunLimit = 15 * time.Minute

// EraseRequest is the JSON body for POST /api/admin/gdpr/erase.
type EraseRequest struct {
	Identifier string `json:"identifier" validate:"required,min=1,max=320"`
}

// Export produces a portable PII bundle for the given data subject.
// POST /api/admin/gdpr/export
// Body: {"identifier": "..."}
// Auth: super_admin.
func (h *DSARHandler) Export(w http.ResponseWriter, r *http.Request) {
	var body ExportRequest
	if _, err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Identifier = strings.TrimSpace(body.Identifier)
	if !httpx.ValidateOrRespond(w, &body) {
		return
	}
	if middleware.ContainsNullOrControl(body.Identifier) {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "identifier contains invalid characters")
		return
	}
	if !namesASubject(body.Identifier) {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, errNotASubject)
		return
	}

	var result *compliance.SubjectExportResult
	var err error

	if body.AllTenants {
		if h.tenants == nil || h.tenantScope == nil {
			// Refuse rather than answer from one tenant: a caller who asked for
			// every tenant must never silently receive one.
			httpx.ErrorReq(w, r, http.StatusBadRequest,
				"cross-tenant export is not available on this install")
			return
		}
		var tenants []string
		tenants, err = h.tenants(r.Context())
		if err != nil {
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "could not enumerate tenants")
			return
		}
		result, err = compliance.RunSubjectExportAcrossTenants(
			r.Context(), body.Identifier, tenants, h.tenantScope)
	} else {
		result, err = compliance.RunSubjectExport(r.Context(), body.Identifier)
	}
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "export failed")
		return
	}

	// A cross-tenant export reads every tenant's data for one identifier, so
	// the audit entry has to say that it happened and what it reached.
	action := "gdpr.export"
	if body.AllTenants {
		action = "gdpr.export.all_tenants"
		// The line names other tenants, so it is logged with no tenant and
		// only an operator reading the whole log sees it.
		slog.WarnContext(logging.WithoutTenant(r.Context()), "cross-tenant DSAR export",
			"tenants_requested", result.Scope.Requested,
			"tenants_covered", result.Scope.Covered,
			"matched_tenants", result.MatchedTenants,
		)
	}
	h.writeAudit(r, action, "gdpr_dsar", compliance.SubjectDigest(body.Identifier))

	httpx.JSON(w, http.StatusOK, result)
}

// EraseResponse is the JSON response for POST /api/admin/gdpr/erase.
//
// Complete is true only when every eraser ran and succeeded. Otherwise Failed
// names each eraser that failed or never ran, and the account is among them:
// the erasure signs the subject out and locks the account first, and
// anonymizes it only once every other eraser has succeeded. Repeating the
// request runs every eraser again and finishes the erasure.
type EraseResponse struct {
	Identifier string   `json:"identifier"`
	TotalRows  int64    `json:"total_rows"`
	Complete   bool     `json:"complete"`
	Failed     []string `json:"failed,omitempty"`
	Errors     []string `json:"errors"`
	// Holds names the active legal holds that stopped the erasure. Non-empty
	// means nothing was erased: a zero row count on its own reads as "there
	// was nothing to erase", which is the opposite of what happened.
	Holds []compliance.HoldRef `json:"holds,omitempty"`
}

// Erase removes or anonymizes PII for the given data subject.
// POST /api/admin/gdpr/erase
// Body: {"identifier": "..."}
// Auth: super_admin.
func (h *DSARHandler) Erase(w http.ResponseWriter, r *http.Request) {
	var body EraseRequest
	if _, err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Identifier = strings.TrimSpace(body.Identifier)
	if !httpx.ValidateOrRespond(w, &body) {
		return
	}

	if middleware.ContainsNullOrControl(body.Identifier) {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "identifier contains invalid characters")
		return
	}
	if !namesASubject(body.Identifier) {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, errNotASubject)
		return
	}

	// A large tenant can take longer to erase than the listener's write
	// timeout, and past it the answer is lost although the run finishes, so
	// this response alone has no write deadline. A failure to clear it is
	// logged rather than refused, because the erasure itself is unaffected.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.WarnContext(r.Context(), "erasure answer keeps the listener write timeout", "err", err)
	}
	// Erasure commits as it goes, so a caller or proxy that gives up must not
	// stop a run partway. The run keeps the request's values and ignores its
	// cancellation. The bound exists because nothing else ends the run now: a
	// call stuck on a lock or a dead peer would hold its goroutine and
	// connection for good.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), erasureRunLimit)
	defer cancel()
	result, err := compliance.RunSubjectErasureWithHolds(ctx, body.Identifier)
	complete := err == nil && len(result.Holds) == 0
	status := http.StatusOK
	var refusal string
	switch {
	case errors.Is(err, compliance.ErrHoldCheckUnavailable):
		// A hold check that cannot answer is not a failed erasure, it is an
		// unanswerable one. Refusing keeps held evidence intact and tells the
		// requester to come back, which is recoverable. Erasing on a guess is
		// not.
		status = http.StatusServiceUnavailable
		refusal = "legal hold status is unavailable; erasure was not attempted"
	case errors.Is(err, compliance.ErrSubjectNotSignedOut):
		// A sign-out that failed is a database failure with nothing erased,
		// so it is refused the same way, and repeating the request is the
		// remedy.
		status = http.StatusServiceUnavailable
		refusal = "the subject could not be signed out; erasure was not attempted"
	case err != nil:
		status = http.StatusMultiStatus
	}
	// The caller may be gone by now, so the outcome is logged whether or not
	// anyone reads the answer. The subject is named by its reference only.
	slog.InfoContext(ctx, "subject_erasure_outcome",
		"subject_ref", compliance.SubjectRef(body.Identifier),
		"status", status,
		"complete", complete,
		"holds", len(result.Holds),
		"rows", result.Rows,
		"failed", result.Failed,
	)
	if refusal != "" {
		httpx.ErrorReq(w, r, status, refusal)
		return
	}

	resp := EraseResponse{
		Identifier: body.Identifier,
		TotalRows:  result.Rows,
		Complete:   complete,
		Holds:      result.Holds,
	}
	if err != nil {
		// A partial erasure must not read as a finished one, to a caller
		// reading the body or only the status. 207 keeps it a success for a
		// client that shows the errors, and keeps it off 200. The failed
		// erasers are named by their owner, and the driver text stays in the
		// log.
		resp.Failed = result.Failed
		resp.Errors = []string{
			"not finished: " + strings.Join(result.Failed, ", "),
			"the account was not erased: it is signed out and locked until every other part has erased",
			"repeat the request to finish the erasure",
		}
	}

	// Audit log.
	h.writeAudit(r, "gdpr.erase", "gdpr_dsar", compliance.SubjectDigest(body.Identifier))

	httpx.JSON(w, status, resp)
}

// writeAudit records the DSAR operation in the audit trail.
//
// The writer is supplied by a plugin and may be nil, and then a subject export
// completes with no audit entry behind it. That is the one operation whose
// whole point is being able to show afterwards who exported whose data, so
// the gap is reported rather than passed over: without this line the only
// trace of an unaudited export is its absence. The subject identifier is left
// out: it is the personal data the request was about.
func (h *DSARHandler) writeAudit(r *http.Request, action, resourceType, resourceID string) {
	if h.auditLog == nil {
		var actor string
		if claims := core.GetClaims(r.Context()); claims != nil {
			actor = claims.UserID
		}
		slog.WarnContext(r.Context(), "DSAR operation completed with no audit trail: no audit writer is wired",
			"action", action, "user_id", actor)
		return
	}
	ip := r.RemoteAddr
	ua := r.UserAgent()
	var userID *string
	if claims := core.GetClaims(r.Context()); claims != nil && claims.UserID != "" {
		userID = &claims.UserID
	}
	h.auditLog.LogEntry(action, resourceType, resourceID, ip, ua, userID)
}
