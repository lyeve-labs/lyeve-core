package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errCredentialEnded is a stream's credential no longer admitting its caller:
// the token expired, the account's sessions ended, or the key was revoked,
// disabled or expired.
var errCredentialEnded = errors.New("credential expired or revoked")

// streamCredential returns the check a content stream repeats before each
// event, so the stream lasts no longer than the credential it opened with.
//
// A session token is held to its expiry and to the account's token version,
// the check every request passes. A sign-out, a password a super admin sets,
// a disabled account and an erasure each raise the version, and a deleted
// account has none. A key goes back through the key middleware that
// admitted it, so a key revoked, disabled or expired since the open is
// refused as a new request with it would be. A key that comes back on other
// terms, with its roles, scopes or schema list edited, ends the stream as
// well, because the stream judges every event by the terms it opened with
// and a reconnect takes the new ones. A key store that cannot answer reads as
// a refusal there, so it ends the stream too, and the client reconnects.
//
// tokenVersion nil skips the version, as the router's own check does, and
// keyAuth nil means no key could have opened the stream on this router.
func streamCredential(tokenVersion func(context.Context, string) (int, error), keyAuth func(http.Handler) http.Handler) func(*http.Request) error {
	return func(r *http.Request) error {
		if c := claimsFromCtx(r); c != nil {
			if c.ExpiresAt != nil && !time.Now().Before(c.ExpiresAt.Time) {
				return errCredentialEnded
			}
			if tokenVersion == nil {
				return nil
			}
			current, err := tokenVersion(r.Context(), c.UserID)
			switch {
			case errors.Is(err, domain.ErrNotFound), errors.Is(err, errTokenSubject):
				return errCredentialEnded
			case err != nil:
				return fmt.Errorf("stream token version: %w", err)
			case c.TokenVersion < current:
				return errCredentialEnded
			}
			return nil
		}
		ac := core.GetClaims(r.Context())
		if ac == nil {
			return errNoCaller
		}
		if !ac.IsAPIKey || keyAuth == nil {
			return nil
		}
		if !keyStillAdmits(r, keyAuth, ac) {
			return errCredentialEnded
		}
		return nil
	}
}

// keyStillAdmits runs the request's key through keyAuth again, with the
// claims it was admitted under hidden so the middleware looks the key up
// rather than skipping a caller it already knows, and reports whether the
// same key comes back on the same terms.
func keyStillAdmits(r *http.Request, keyAuth func(http.Handler) http.Handler, opened *core.AuthClaims) bool {
	var now *core.AuthClaims
	probe := r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, (*core.AuthClaims)(nil)))
	keyAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		now = core.GetClaims(r.Context())
	})).ServeHTTP(discardResponse{header: http.Header{}}, probe)
	return now != nil && now.APIKeyID == opened.APIKeyID && now.TenantID == opened.TenantID &&
		slices.Equal(now.Roles, opened.Roles) && slices.Equal(now.Scopes, opened.Scopes) &&
		slices.Equal(now.Schemas, opened.Schemas)
}

// discardResponse is the writer a credential replay runs against. The key
// middleware writes nothing of its own on the way through, and nothing it
// does may reach the stream's client.
type discardResponse struct{ header http.Header }

func (d discardResponse) Header() http.Header { return d.header }

func (discardResponse) Write(b []byte) (int, error) { return len(b), nil }

func (discardResponse) WriteHeader(int) {}
