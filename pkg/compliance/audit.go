package compliance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/google/uuid"
)

// AuditLogWriter registry

// AuditEntry carries all fields the kernel assembles for a single audit
// record. Passed to AuditLogWriter.Writer and WriterSync instead of a long
// positional parameter list.
type AuditEntry struct {
	TenantID     string
	UserID       any // *uuid.UUID or nil
	Action       string
	ResourceType string
	ResourceID   string
	IP           string
	UserAgent    string
	BeforeJSON   string // marshaled before-state, or ""
	AfterJSON    string // marshaled after-state, or ""

	// AdminTokenID names the admin token that made the change, when one did.
	// UserID is then the token's owner. The same id also leads UserAgent as
	// "admin-token:<id>", so a log with no column of its own for it still
	// says which token acted.
	AdminTokenID string
}

// adminTokenActor is the user agent prefix that names an admin token.
const adminTokenActor = "admin-token:"

// ActorUserAgent returns the user agent an audit row records for a caller:
// the request's own, led by the admin token's id when a token is the caller.
//
// A caller that is not a token cannot claim to be one: a user agent it sends
// opening with the prefix loses that word, so only the engine writes it.
func ActorUserAgent(adminTokenID, userAgent string) string {
	for strings.HasPrefix(userAgent, adminTokenActor) {
		_, rest, _ := strings.Cut(userAgent, " ")
		userAgent = strings.TrimLeft(rest, " ")
	}
	if adminTokenID == "" {
		return userAgent
	}
	if userAgent == "" {
		return adminTokenActor + adminTokenID
	}
	return adminTokenActor + adminTokenID + " " + userAgent
}

// AuditLogWriter is the interface an audit plugin implements to store audit
// entries. The plugin registers it with RegisterAuditWriter when it starts.
// With no writer registered, entries are dropped.
//
// Writer must not block. Failures are logged but not returned, because audit
// writes are best-effort for normal operations.
//
// WriterSync blocks until the entry is durably written (or fails). Use it
// for security-critical events that must be confirmed before proceeding.
// Most callers should use Writer for better throughput.
//
// Implementations must be safe for concurrent calls.
type AuditLogWriter interface {
	Writer(ctx context.Context, host core.Host, entry AuditEntry)
	WriterSync(ctx context.Context, host core.Host, entry AuditEntry) error
}

var (
	auditWriterMu sync.Mutex
	auditWriter   AuditLogWriter
)

// RegisterAuditWriter sets the global audit log writer. Called by an audit
// plugin during Start(). Idempotent: subsequent calls silently replace the
// previous writer (supports test suites that start/stop the plugin multiple
// times). Callers that need to unset (tests) call ClearAuditWriter.
func RegisterAuditWriter(w AuditLogWriter) {
	auditWriterMu.Lock()
	defer auditWriterMu.Unlock()
	auditWriter = w
}

// ClearAuditWriter removes the registered audit log writer. Exposed for
// tests that need a clean registry. Do not call in production.
func ClearAuditWriter() {
	auditWriterMu.Lock()
	auditWriter = nil
	auditWriterMu.Unlock()
}

// Record helpers

// AuditOption configures the behavior of audit record operations.
type AuditOption func(*auditOptions)

type auditOptions struct {
	sync bool
}

// WithSync requests synchronous audit write confirmation. The call blocks
// until the entry is durably written or the context is canceled. Use for
// security-critical events where the caller must confirm persistence before
// acknowledging success. Most audit calls should remain async for throughput.
func WithSync() AuditOption {
	return func(o *auditOptions) { o.sync = true }
}

// RecordAudit hands an audit entry to the registered AuditLogWriter. With none
// registered the entry is dropped. It is a fire-and-forget best-effort
// operation: audit write failures are logged but do not fail the originating
// request.
func RecordAudit(ctx context.Context, host core.Host, action, resourceType, resourceID, ip, userAgent string, opts ...AuditOption) {
	RecordAuditWithState(ctx, host, action, resourceType, resourceID, ip, userAgent, nil, nil, opts...)
}

// RecordAuditWithState is like RecordAudit but captures before/after state
// snapshots for compliance auditing. before and after are marshaled to JSON.
// Pass nil to omit them.
//
// The entry goes to the registered AuditLogWriter. With none registered the
// entry is dropped.
func RecordAuditWithState(ctx context.Context, host core.Host, action, resourceType, resourceID, ip, userAgent string, before, after any, opts ...AuditOption) {
	claims := core.GetClaims(ctx)
	var userID interface{}
	if claims != nil && claims.UserID != "" {
		uid, err := uuid.Parse(claims.UserID)
		if err == nil {
			userID = &uid
		}
	}

	// The row belongs to the tenant that was acted on, which the tenancy
	// middleware resolved onto the context. It differs from the caller's home
	// tenant whenever a super_admin acts on another tenant via X-Tenant-ID.
	// Filing under the JWT claim instead would drop the action out of the
	// affected tenant's log and out of reach of that tenant's retention policy.
	// The claim only serves contexts the middleware never touched, such as
	// background workers that carry claims alone.
	tenantID := core.TenantIDFromCtx(ctx)
	if tenantID == "" && claims != nil {
		tenantID = claims.TenantID
	}

	// An admin token acts for its owner, so the owner stays the user and the
	// token is named beside them.
	var adminTokenID string
	if claims != nil {
		adminTokenID = claims.AdminTokenID
	}
	userAgent = ActorUserAgent(adminTokenID, userAgent)

	var beforeJSON, afterJSON string
	if before != nil {
		b, err := json.Marshal(before)
		if err == nil {
			beforeJSON = string(b)
		}
	}
	if after != nil {
		b, err := json.Marshal(after)
		if err == nil {
			afterJSON = string(b)
		}
	}

	// Check for a registered writer.
	auditWriterMu.Lock()
	w := auditWriter
	auditWriterMu.Unlock()

	if w != nil {
		// Resolve options.
		var ao auditOptions
		for _, opt := range opts {
			opt(&ao)
		}
		entry := AuditEntry{
			TenantID: tenantID, UserID: userID, Action: action,
			ResourceType: resourceType, ResourceID: resourceID,
			IP: ip, UserAgent: userAgent,
			BeforeJSON: beforeJSON, AfterJSON: afterJSON,
			AdminTokenID: adminTokenID,
		}
		if ao.sync {
			if err := w.WriterSync(ctx, host, entry); err != nil && host != nil {
				if slog := host.Logger(ctx); slog != nil {
					slog.Error("audit sync write failed", "action", action, "resource_type", resourceType, "err", err)
				}
			}
		} else {
			w.Writer(ctx, host, entry)
		}
		return
	}

	// No writer, no entry. The table belongs to the writer, which may chain
	// every row to the one before it, so a row written here would carry no
	// chain hash and break the chain the writer verifies. A gap reads as
	// tampering, which is worse than the entry never existing.
	if host == nil {
		return
	}
	if slog := host.Logger(ctx); slog != nil {
		slog.Debug("audit entry dropped: no writer registered",
			"action", action, "resource_type", resourceType)
	}
}

// RecordAuditFromRequest is a convenience wrapper that reads IP and User-Agent
// from the HTTP request.
func RecordAuditFromRequest(r *http.Request, host core.Host, action, resourceType, resourceID string) {
	RecordAudit(r.Context(), host, action, resourceType, resourceID,
		r.RemoteAddr, r.UserAgent())
}

// RecordAuditFromRequestWithState is a convenience wrapper that reads IP and
// User-Agent from the HTTP request and captures before/after state snapshots.
func RecordAuditFromRequestWithState(r *http.Request, host core.Host, action, resourceType, resourceID string, before, after any) {
	RecordAuditWithState(r.Context(), host, action, resourceType, resourceID,
		r.RemoteAddr, r.UserAgent(), before, after)
}
