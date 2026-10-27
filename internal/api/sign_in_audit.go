package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Sign-in audit actions. Every sign-in method files under these three names
// and states its method in the entry, so one filter reads all sign-ins.
const (
	auditActionSignIn            = "auth.login"
	auditActionSignInMFARequired = "auth.login.mfa_required"
	auditActionSignInRefused     = "auth.login.refused"
)

// signInAttempt watches one sign-in request for its audit entry. It keeps
// the status the handler answered with, and the handler fills in what it
// learned about the account on the way. Watching the status rather than
// recording at every return keeps a refusal from going unrecorded when a
// new return is added.
type signInAttempt struct {
	http.ResponseWriter
	status int
	step   string
	// email is the address as typed. It names the attempt by its digest when
	// no account matched.
	email   string
	account uuid.UUID
	// tenant is the account's tenant, and the session's once one is issued.
	tenant string
	issued bool
}

func (a *signInAttempt) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *signInAttempt) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

func (a *signInAttempt) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// watchSignIn wraps w for a sign-in at the given step, "password" or
// "second_factor". The caller defers auditSignIn with the result.
func watchSignIn(w http.ResponseWriter, step string) *signInAttempt {
	return &signInAttempt{ResponseWriter: w, step: step}
}

// auditSignIn records the attempt's outcome. A request that named neither an
// account nor an address, such as a body that did not parse, is not a
// sign-in attempt and records nothing.
func (h *AuthHandler) auditSignIn(r *http.Request, a *signInAttempt) {
	if h.host == nil || (a.account == uuid.Nil && a.email == "") {
		return
	}
	action := auditActionSignInRefused
	switch {
	case a.issued:
		action = auditActionSignIn
	case a.status == http.StatusOK:
		action = auditActionSignInMFARequired
	}
	after := map[string]any{"method": "password", "step": a.step, "status": a.status}
	resourceID := ""
	if a.account != uuid.Nil {
		resourceID = a.account.String()
	} else {
		after["subject_ref"] = compliance.SubjectRef(core.NormalizeEmail(a.email))
	}

	ctx := r.Context()
	if a.tenant != "" {
		ctx = core.WithTenantID(ctx, a.tenant)
	}
	// The password proved who is acting, so the account is the actor. A
	// refused attempt proved nothing and has none.
	if action != auditActionSignInRefused {
		ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: resourceID, TenantID: a.tenant})
	}
	compliance.RecordAuditWithState(ctx, h.host, action, "user", resourceID, clientIP(r), r.UserAgent(), nil, after)
}
