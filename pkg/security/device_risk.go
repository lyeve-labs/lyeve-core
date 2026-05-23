package security

import (
	"context"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/google/uuid"
)

// DeviceFingerprint captures the signals collected from a login request
// for risk assessment. The auth handler computes it once and passes it
// through context to avoid duplicate computation.
type DeviceFingerprint struct {
	// UserAgent is the raw User-Agent header.
	UserAgent string `json:"user_agent"`

	// IP is the client IP address (after proxy resolution).
	IP string `json:"ip"`

	// TLSFingerprint is the JA4 fingerprint string from the reverse proxy
	// (via X-TLS-Fingerprint), or empty when unavailable.
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`

	// HeaderHash is a stable hash of relevant request headers (Accept, Accept-Language,
	// Accept-Encoding, Sec-CH-UA-*, etc.) used to detect header drift / impersonation.
	HeaderHash string `json:"header_hash,omitempty"`
}

// RiskLevel categorizes the assessed risk of a login attempt.
type RiskLevel string

const (
	// RiskLow is the lowest risk level for a login attempt.
	RiskLow RiskLevel = "low"
	// RiskMedium is a moderate risk level for a login attempt.
	RiskMedium RiskLevel = "medium"
	// RiskHigh is a high risk level for a login attempt.
	RiskHigh RiskLevel = "high"
	// RiskCritical is the highest risk level for a login attempt.
	RiskCritical RiskLevel = "critical"
)

// RiskAssessment is the result of evaluating a login attempt.
type RiskAssessment struct {
	Level  RiskLevel `json:"level"`
	Score  int       `json:"score"`  // 0-100, higher = riskier
	Reason string    `json:"reason"` // human-readable summary

	// RequireMFA forces the caller to escalate to MFA regardless of user preference.
	RequireMFA bool `json:"require_mfa"`

	// RequireAlert signals that an admin notification should be fired.
	RequireAlert bool `json:"require_alert"`

	// DeviceRecognized is true when the fingerprint matches a previously trusted device.
	DeviceRecognized bool `json:"device_recognized"`

	// DeviceID is the UUID of the matching device (empty if unrecognized).
	DeviceID string `json:"device_id,omitempty"`
}

// riskAssessmentCtxKey passes a pre-computed RiskAssessment from the auth
// handler to RecordLogin, avoiding a redundant Assess call on the hot path.
type riskAssessmentCtxKey struct{}

// RiskAssessmentCtxKey is the context key for a pre-computed *RiskAssessment.
// The auth handler stores the Assess result before calling RecordLogin,
// which checks this key first to avoid a redundant Assess call.
var RiskAssessmentCtxKey riskAssessmentCtxKey

// DeviceRiskAssessor is the minimal interface the engine needs from a
// device-fingerprinting plugin. The auth handler calls Assess before
// issuing a session token to decide whether step-up MFA or an admin
// alert is required.
type DeviceRiskAssessor interface {
	// Assess evaluates the device fingerprint for the given user and returns
	// a risk score + recommended action.
	Assess(ctx context.Context, userID uuid.UUID, fp DeviceFingerprint) (*RiskAssessment, error)

	// RecordLogin records a successful or failed login attempt for this
	// (user, device) pair so the risk engine can learn over time.
	RecordLogin(ctx context.Context, userID uuid.UUID, fp DeviceFingerprint, success bool) error

	// TrustDevice marks the fingerprint as trusted for the given user.
	// Subsequent logins from matching fingerprints will score lower risk.
	TrustDevice(ctx context.Context, userID uuid.UUID, fp DeviceFingerprint, label string) (deviceID uuid.UUID, err error)

	// UntrustDevice removes trust from a previously trusted device.
	// userID ensures ownership scoping: a user can only untrust their own devices.
	UntrustDevice(ctx context.Context, userID, deviceID uuid.UUID) error

	// ListTrustedDevices returns paginated trusted devices for the given user.
	ListTrustedDevices(ctx context.Context, userID uuid.UUID, limit, offset int) ([]TrustedDevice, int, error)
}

// TrustedDevice represents a device the user has explicitly trusted.
type TrustedDevice struct {
	ID              uuid.UUID `json:"id"`
	UserID          uuid.UUID `json:"user_id"`
	Label           string    `json:"label"`
	FingerprintHash string    `json:"-"` // never serialized to clients
	LastSeenAt      time.Time `json:"last_seen_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// DeviceRiskAssessorProvider is an optional interface plugins implement to
// expose their risk assessor. The runtime detects it after Start() and
// passes it to the admin router for AuthHandler consumption.
type DeviceRiskAssessorProvider interface {
	core.Plugin
	RiskAssessor() DeviceRiskAssessor
}
