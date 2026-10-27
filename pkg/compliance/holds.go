package compliance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A legal hold is the control that stops evidence being destroyed while a
// claim or an investigation is live. Erasure consults it first, because a held
// subject's deleted rows cannot be recovered.
//
// It is not a conflict between two obligations. GDPR Art.17(3)(e) already
// carves out processing needed for the establishment, exercise or defense of
// legal claims, so skipping the held rows and saying so is the lawful answer.
//
// The check is a seam rather than a dependency: the eraser registry is
// plugin-agnostic, and holds are supplied by a plugin through HoldChecker. The
// engine asks whoever registered a checker, and behaves coherently when nobody
// has.

// HoldRef names an active legal hold covering a data subject. It carries no
// subject data: the caller already knows the identifier it asked about.
type HoldRef struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// HoldChecker answers which active legal holds cover a data subject. An empty
// slice with a nil error means the subject is free to erase.
type HoldChecker interface {
	SubjectHolds(ctx context.Context, identifier string) ([]HoldRef, error)
}

// TenantHoldChecker is the optional interface a HoldChecker implements to
// answer which active legal holds in the context tenant cover the data of a
// resource type beyond one named subject: a hold on the whole tenant, on the
// resource type, on an action or on a time window. SubjectHolds answers only
// for the identifier it is given, so a caller that deletes a type's data in
// bulk asks this as well. It is optional, so a checker written before it
// keeps working, and CheckTenantHolds reads such a checker as holding
// nothing.
type TenantHoldChecker interface {
	TenantHolds(ctx context.Context, resourceType string) ([]HoldRef, error)
}

// HoldCheckerProvider is the optional interface a plugin implements to supply
// the checker. The runtime type-asserts every active plugin to it after
// activation, the same way it reaches SubjectEraserProvider.
type HoldCheckerProvider interface {
	core.Plugin
	HoldChecker() HoldChecker
}

// ErrHoldCheckUnavailable is returned when a checker is registered but cannot
// answer. It is deliberately distinct from "no holds": the caller must be able
// to tell "this subject is free to erase" from "nobody knows".
var ErrHoldCheckUnavailable = errors.New("legal hold status could not be determined")

// ErrSubjectNotSignedOut is returned when the subject could not be signed
// out. The erasure then runs no eraser, so a caller must not report the
// account as locked.
var ErrSubjectNotSignedOut = errors.New("the subject could not be signed out")

var (
	holdMu      sync.RWMutex
	holdChecker HoldChecker
)

// RegisterHoldChecker installs the checker erasure consults. The last
// registration wins. Plugins call this from Start.
func RegisterHoldChecker(c HoldChecker) {
	holdMu.Lock()
	defer holdMu.Unlock()
	holdChecker = c
}

// RegisteredHoldChecker returns the installed checker, or nil when no plugin
// has registered one.
func RegisteredHoldChecker() HoldChecker {
	holdMu.RLock()
	defer holdMu.RUnlock()
	return holdChecker
}

// ResetHoldChecker clears the checker. For tests.
func ResetHoldChecker() {
	holdMu.Lock()
	defer holdMu.Unlock()
	holdChecker = nil
}

// CheckTenantHolds asks the registered checker which active holds in the
// context tenant cover resourceType beyond one named subject. With no checker
// registered, or one that does not implement TenantHoldChecker, nothing is
// held. A checker that cannot answer returns ErrHoldCheckUnavailable whatever
// LYEVE_GDPR_HOLD_CHECK says, because that setting keeps erasure requests
// flowing and a bulk delete is not one.
func CheckTenantHolds(ctx context.Context, resourceType string) ([]HoldRef, error) {
	checker, ok := RegisteredHoldChecker().(TenantHoldChecker)
	if !ok {
		return nil, nil
	}
	holds, err := checker.TenantHolds(ctx, resourceType)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHoldCheckUnavailable, err)
	}
	return holds, nil
}

// proceedOnHoldCheckFailure reports whether erasure should run when a
// registered checker cannot answer.
//
// The default is to refuse, and the two cases it distinguishes are the reason
// this is not one blanket policy:
//
//   - No checker registered means no hold can exist to honor. Erasing is
//     correct, and refusing would break lawful erasure on every install
//     without a checker. That case never reaches this function.
//   - A checker that fails means holds may exist and nobody can say. Erasure
//     is deferrable and destroyed evidence is not, so it refuses.
//
// An install that would rather keep Art.17 requests flowing through a
// retention outage sets LYEVE_GDPR_HOLD_CHECK=proceed and accepts the trade.
func proceedOnHoldCheckFailure() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("LYEVE_GDPR_HOLD_CHECK")), "proceed")
}
