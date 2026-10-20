package core

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
)

// CacheFetcher is the callback that produces the data to cache on a miss.
type CacheFetcher func() ([]byte, error)

// EmailSender is a minimal email sender interface for plugins.
type EmailSender interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// ErrEmailNotConfigured is what an EmailSender returns when the tenant and
// the instance have no mail transport at all. Nothing was attempted, so a
// caller may treat it as "no mail here" and take its own fallback, where any
// other error from Send is a delivery that failed.
var ErrEmailNotConfigured = errors.New("email: no mail transport configured")

// EmailSenderProvider is implemented by the engine Host.
type EmailSenderProvider interface {
	EmailSender() EmailSender
}

// EmailSenderRegistrar is implemented by the engine Host. Through ScopedHost a
// registration needs CapEmailSender, and only the plugin that registered the
// sender may replace or clear it. A refusal comes back as the error.
type EmailSenderRegistrar interface {
	RegisterEmailSender(s EmailSender) error
}

// RefreshTokenRevoker is an optional interface the engine Host implements.
type RefreshTokenRevoker interface {
	RevokeAllRefreshTokens(ctx context.Context, userID string) error
}

// ClusterBusProvider reaches the other replicas of this engine. A plugin that
// caches something another replica can change broadcasts after its write and
// subscribes to drop its own copy. See pkg/cluster. The bus a plugin receives
// prefixes every topic with the plugin's name. It is nil on a host that was
// given none, such as a test harness, and it carries nothing on an install
// that runs no replication: the broadcast is dropped and this process stays
// the only replica.
type ClusterBusProvider interface {
	ClusterBus() cluster.Bus
}

// QueryCacheProvider is an optional interface for query result caching.
type QueryCacheProvider interface {
	CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn CacheFetcher) (bool, error)
	InvalidateCache(prefix string) int
}

// SessionTokenSigner is an optional interface for session JWT signing.
// SignSessionToken refuses an account that is disabled or past its expiry
// with ErrAccountInactive, the same accounts the password login refuses. A
// login route answers that as it answers a failed credential.
type SessionTokenSigner interface {
	SignSessionToken(ctx context.Context, userID uuid.UUID, email string, roles []string) (string, error)
}

// ErrAccountInactive is SignSessionToken's answer for a disabled or expired
// account.
var ErrAccountInactive = errors.New("account is disabled or expired")

// MFAChallengeCompleter finishes a password sign-in's second-factor challenge
// with a factor a plugin verified itself, such as a passkey. It applies the
// rules the engine's own code completion applies: the challenge is single use
// across every completion, it completes only on a host where its sign-in
// could have started, and the session acts in the challenge's tenant with
// the roles the account holds there.
type MFAChallengeCompleter interface {
	// MFAChallengeAccount names the account a challenge was issued for, so the
	// caller can verify that account's factor. It spends nothing. A challenge
	// that is malformed, expired, already spent, or for another tenant than
	// the request's mapped domain is refused with ErrMFAChallengeInvalid.
	MFAChallengeAccount(ctx context.Context, challengeToken string) (MFAChallengeAccount, error)

	// CompleteMFAChallenge spends the challenge and signs the session. Call it
	// only after the account's factor was verified. Of two calls for one
	// challenge only one signs, and the other gets ErrMFAChallengeInvalid. A
	// disabled or expired account gets ErrAccountInactive. Any other error is
	// state the engine could not read or write, and nothing was signed.
	CompleteMFAChallenge(ctx context.Context, challengeToken string) (string, error)
}

// MFAChallengeAccount is the account an MFA challenge was issued for.
type MFAChallengeAccount struct {
	UserID   uuid.UUID
	Email    string
	TenantID string
}

// ErrMFAChallengeInvalid is the MFAChallengeCompleter answer for a challenge
// that cannot be completed. A sign-in route answers it as a failed
// credential.
var ErrMFAChallengeInvalid = errors.New("mfa challenge is invalid, expired or spent")
