package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/httpclient"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// safeHTTPOnce and safeHTTP provide the lazy-initialized SSRF-safe HTTP client
// for OAuth outbound calls. Uses ssrf.NewSafeHTTPClient which blocks
// private/internal/loopback IPs at the transport level, validates redirect
// targets, and pins resolved IPs to prevent DNS rebinding attacks.
var (
	safeHTTPOnce sync.Once
	safeHTTP     *http.Client
)

// maxTokenResponseBytes bounds an OAuth token response. The specification
// puts a small JSON object here. The ceiling is generous enough for a provider
// that adds claims of its own and small enough that a hostile one cannot make
// the process grow.
const maxTokenResponseBytes = 1 << 20

func safeHTTPClient() *http.Client {
	safeHTTPOnce.Do(func() {
		safeHTTP = ssrf.NewSafeHTTPClient(30*time.Second, 5)
	})
	return safeHTTP
}

// OIDC discovery

// OIDCConfig holds the endpoint information discovered from an OIDC provider.
type OIDCConfig struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSUri               string `json:"jwks_uri"`
}

var (
	oidcDiscoveryMu    sync.RWMutex
	oidcDiscoveryCache = map[string]*oidcDiscoveryEntry{}
)

type oidcDiscoveryEntry struct {
	cfg  *OIDCConfig
	time time.Time
}

const oidcDiscoveryTTL = 10 * time.Minute

// DiscoverOIDC fetches (or returns cached) OIDC well-known configuration from
// the given issuer URL. Results are cached in-process for 10 minutes to avoid
// hammering the identity provider on every login.
func DiscoverOIDC(ctx context.Context, issuerURL string) (*OIDCConfig, error) {
	oidcDiscoveryMu.RLock()
	entry := oidcDiscoveryCache[issuerURL]
	oidcDiscoveryMu.RUnlock()
	if entry != nil && time.Since(entry.time) < oidcDiscoveryTTL {
		return entry.cfg, nil
	}

	u := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc discover: %w", err)
	}
	resp, err := safeHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discover: status %d", resp.StatusCode)
	}

	var cfg OIDCConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("oidc discover: decode: %w", err)
	}

	oidcDiscoveryMu.Lock()
	oidcDiscoveryCache[issuerURL] = &oidcDiscoveryEntry{cfg: &cfg, time: time.Now()}
	oidcDiscoveryMu.Unlock()
	return &cfg, nil
}

// PKCE helpers

// GeneratePKCE produces a code_verifier and its S256 code_challenge per
// RFC 7636. The verifier is 32 random bytes base64url-encoded. The challenge
// is the base64url-encoded SHA-256 hash of the verifier.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, buf); err != nil {
		return "", "", fmt.Errorf("pkce: generate verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// GenerateState returns a random CSRF state token (16 bytes, base64url).
func GenerateState() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// BuildAuthURL constructs the OAuth2 authorization redirect URL. It appends
// query parameters for response_type=code, client_id, redirect_uri, state,
// and PKCE code_challenge to the given authorization endpoint.
func BuildAuthURL(authEndpoint, clientID, redirectURI, state, codeChallenge string, scopes []string) string {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"scope":                 {strings.Join(scopes, " ")},
	}
	return authEndpoint + "?" + v.Encode()
}

// Token exchange + userinfo

// OAuthToken holds the response from the token endpoint.
type OAuthToken struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// ExchangeCode exchanges an authorization code + PKCE verifier for tokens
// at the provider's token endpoint. Uses HTTP Basic auth via client_secret
// when provided, otherwise relies on PKCE alone (public client).
func ExchangeCode(ctx context.Context, tokenEndpoint, clientID, clientSecret, code, codeVerifier, redirectURI string) (*OAuthToken, error) {
	params := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
	}
	if clientSecret != "" {
		params.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := safeHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	// The provider is named by tenant configuration, so the size of its
	// reply is an input rather than a constant. A token response is a few
	// hundred bytes. Anything approaching the ceiling is not one.
	body, err := httpclient.ReadBody(resp, maxTokenResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: status %d: %s", resp.StatusCode, string(body))
	}

	var tok OAuthToken
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("token exchange: decode: %w", err)
	}
	return &tok, nil
}

// FetchUserinfo calls the userinfo endpoint with the given access token and
// returns the claims map. The caller is responsible for extracting email,
// roles, and any other relevant fields.
func FetchUserinfo(ctx context.Context, userinfoEndpoint, accessToken string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := safeHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: status %d", resp.StatusCode)
	}

	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, fmt.Errorf("userinfo: decode: %w", err)
	}
	return claims, nil
}

// ExtractEmail extracts the email field from userinfo claims. Returns an
// empty string when the field is absent or not a string.
func ExtractEmail(claims map[string]any) string {
	if v, ok := claims["email"].(string); ok {
		return v
	}
	return ""
}

// ExtractRoles maps a configurable OIDC claim to a slice of CMS role strings.
// Falls back to defaultRoles when the claim is absent, empty, or not a
// recognized type (string or []string).
func ExtractRoles(claims map[string]any, rolesClaim string, defaultRoles []string) []string {
	if rolesClaim == "" {
		return defaultRoles
	}
	v, ok := claims[rolesClaim]
	if !ok {
		return defaultRoles
	}
	switch rv := v.(type) {
	case []any:
		roles := make([]string, 0, len(rv))
		for _, r := range rv {
			if s, ok := r.(string); ok {
				roles = append(roles, s)
			}
		}
		if len(roles) == 0 {
			return defaultRoles
		}
		return roles
	case string:
		if rv == "" {
			return defaultRoles
		}
		return []string{rv}
	}
	return defaultRoles
}

// FilterIDPRoles strips privileged CMS roles (admin, super_admin) from
// IdP-supplied role assertions. IdP-provided roles must never grant elevated
// CMS access: those are assigned exclusively through the admin panel.
// Roles NOT in the block list pass through unchanged.
func FilterIDPRoles(roles []string) []string {
	if len(roles) == 0 {
		return roles
	}
	blocked := map[string]bool{
		"admin":       true,
		"super_admin": true,
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if !blocked[r] {
			out = append(out, r)
		}
	}
	return out
}
