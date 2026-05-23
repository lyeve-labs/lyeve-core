package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// Headers the admin console sets on every request it makes on a browser's
// behalf. The console is a server, so the engine otherwise sees the console's
// address on every login it forwards, and every per-address control (the
// login limiter, lockout scoring, device risk) counts all of its users as one.
const (
	ConsoleClientHeader    = "X-Lyeve-Console-Client"
	ConsoleTimeHeader      = "X-Lyeve-Console-Time"
	ConsoleSignatureHeader = "X-Lyeve-Console-Signature"

	// ConsoleHostHeader carries the host the browser addressed, and
	// ConsoleHostSignatureHeader the console's signature over it. The console
	// reaches the engine at an internal address, so without them every public
	// request it forwards resolves its tenant from that address, and an
	// install with several tenants resolves none before sign-in.
	ConsoleHostHeader          = "X-Lyeve-Console-Host"
	ConsoleHostSignatureHeader = "X-Lyeve-Console-Host-Signature"
)

// maxConsoleHostLen bounds a vouched host to what DNS allows plus a port.
const maxConsoleHostLen = 261

// ConsoleSignatureSkew bounds how far a signed request's timestamp may sit
// from the engine's clock, on either side. Twice it is the replay window for a
// captured request.
const ConsoleSignatureSkew = 30 * time.Second

// ConsoleSignature returns the hex HMAC-SHA256 the console sends for one
// request. The signature covers the method, the decoded path, the raw query,
// the client address it vouches for and a digest of the Authorization header,
// so a captured signature cannot vouch for another address, another route or
// another session.
//
// The path is signed decoded because a proxy between the console and the
// engine may re-escape it: Go's reverse proxy writes | and ^ as %7C and %5E,
// which the console sent raw. Decoding gives both sides one spelling. The
// query is passed through unchanged by the proxies in front of the engine and
// is signed as sent.
func ConsoleSignature(key []byte, unixTime, method, path, rawQuery, clientIP, authorization string) string {
	authDigest := sha256.Sum256([]byte(authorization))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("lyeve-console-v1\n" + unixTime + "\n" + method + "\n" + path + "\n" + rawQuery + "\n" +
		clientIP + "\n" + hex.EncodeToString(authDigest[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// ConsoleHostSignature returns the hex HMAC-SHA256 the console sends over the
// host it vouches for. It is bound to the request's own signature, which
// already covers the time, the method, the path, the query, the client and
// the session, so a captured host signature vouches for nothing else.
//
// It is a second header rather than a field of the request signature so that
// an engine that does not read it still verifies the request: it ignores a
// header it does not know, and a console that signs the host keeps working
// against it.
func ConsoleHostSignature(key []byte, unixTime, requestSignature, host string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("lyeve-console-host-v1\n" + unixTime + "\n" + requestSignature + "\n" + host))
	return hex.EncodeToString(mac.Sum(nil))
}

// ClientAddress sets r.RemoteAddr to the address of the caller the engine is
// serving. It must run after StripUntrustedProxyHeaders and before anything
// that keys on an address.
//
// Two sources can name a caller other than the socket peer:
//
//   - A request signed with consoleKey names its client in
//     ConsoleClientHeader. A signature that is present but wrong, stale or
//     unverifiable is refused with 401 rather than ignored, so a console with
//     the wrong key fails loudly instead of collapsing every user onto one
//     address.
//   - A trusted proxy's X-Forwarded-For, walked right to left, because the
//     leftmost entry is whatever the client wrote before the proxy appended
//     the real address.
//
// A request the console signed may also name the host the browser addressed,
// under its own signature. It is recorded with core.WithRequestHost, which is
// what host-based tenant resolution reads. A host signature that does not
// verify is refused with 401 like a bad request signature. A signed host that
// is not a hostname this engine can map is ignored, and the request resolves
// by r.Host like an unsigned one: the browser may open the
// console at any name its URL parser allows, and a name no domain row can hold
// is not a reason to refuse every request. A host header on a request nobody
// signed is dropped.
//
// Only tenant resolution reads the vouched host. AllowedHosts, CORS and the
// HTTPS redirect keep reading r.Host, which is the name the engine was reached
// at: checking the vouched host against ALLOWED_HOSTS would refuse every
// console request on an install that lists only its internal name, and a
// redirect must not take its target from a header.
//
// The forwarding headers are removed once resolved, so a later reader cannot
// resolve the chain a second time from an address that is itself trusted.
// The console headers are always removed, whatever the outcome.
func ClientAddress(trustedCIDRs []*net.IPNet, consoleKeys ...[]byte) func(http.Handler) http.Handler {
	return clientAddress(trustedCIDRs, consoleKeys, time.Now)
}

func clientAddress(trustedCIDRs []*net.IPNet, consoleKeys [][]byte, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			client := r.Header.Get(ConsoleClientHeader)
			stamp := r.Header.Get(ConsoleTimeHeader)
			sig := r.Header.Get(ConsoleSignatureHeader)
			host := r.Header.Get(ConsoleHostHeader)
			hostSig := r.Header.Get(ConsoleHostSignatureHeader)
			r.Header.Del(ConsoleClientHeader)
			r.Header.Del(ConsoleTimeHeader)
			r.Header.Del(ConsoleSignatureHeader)
			r.Header.Del(ConsoleHostHeader)
			r.Header.Del(ConsoleHostSignatureHeader)

			resolved := ""
			if len(consoleKeys) > 0 && sig != "" {
				ip, key, reason := verifyConsoleSignature(consoleKeys, now(), r, client, stamp, sig)
				if reason == "" && (host != "" || hostSig != "") {
					reason = verifyConsoleHost(key, stamp, sig, host, hostSig)
				}
				if reason != "" {
					slog.WarnContext(r.Context(), "console signature refused", "reason", reason,
						"peer", peerIP(r.RemoteAddr), "method", r.Method, "path", r.URL.Path)
					httpx.ErrorReq(w, r, http.StatusUnauthorized, "invalid console signature")
					return
				}
				resolved = ip
				if host != "" && validConsoleHost(host) {
					r = r.WithContext(core.WithRequestHost(r.Context(), strings.ToLower(host)))
				}
			} else if len(trustedCIDRs) > 0 {
				resolved = reqparse.ClientIPTrusted(r, trustedCIDRs)
			}

			if resolved != "" && resolved != peerIP(r.RemoteAddr) {
				r.RemoteAddr = net.JoinHostPort(resolved, "0")
			}
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-IP")
			r.Header.Del("True-Client-IP")
			next.ServeHTTP(w, r)
		})
	}
}

// verifyConsoleSignature returns the client address in its canonical form, so
// ::ffff:a.b.c.d and a.b.c.d share one limiter bucket, and the key that
// signed it, or the reason the signature was refused.
func verifyConsoleSignature(keys [][]byte, now time.Time, r *http.Request, client, stamp, sig string) (string, []byte, string) {
	ip := net.ParseIP(client)
	if ip == nil {
		return "", nil, "client address does not parse"
	}
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", nil, "timestamp does not parse"
	}
	drift := now.Sub(time.Unix(secs, 0))
	if drift > ConsoleSignatureSkew || drift < -ConsoleSignatureSkew {
		return "", nil, "timestamp outside the allowed skew"
	}
	for _, key := range keys {
		want := ConsoleSignature(key, stamp, r.Method, r.URL.Path, r.URL.RawQuery, client, r.Header.Get("Authorization"))
		if hmac.Equal([]byte(want), []byte(sig)) {
			return ip.String(), key, ""
		}
	}
	return "", nil, "signature does not match"
}

// verifyConsoleHost returns the reason a vouched host is refused, or "". The
// key is the one that signed the request, so a rotation that is half done
// still verifies a host signed under the old key. The host's shape is not
// checked here: a host the console signed is the console's to name, and the
// caller decides whether to use it.
func verifyConsoleHost(key []byte, stamp, requestSig, host, hostSig string) string {
	if host == "" || hostSig == "" {
		return "host or its signature missing"
	}
	want := ConsoleHostSignature(key, stamp, requestSig, host)
	if !hmac.Equal([]byte(want), []byte(hostSig)) {
		return "host signature does not match"
	}
	return ""
}

// validConsoleHost reports whether host is a hostname or address with an
// optional port, the shape a Host header carries. An underscore is allowed,
// because a container name holds one and a browser's URL parser keeps it.
func validConsoleHost(host string) bool {
	if len(host) > maxConsoleHostLen {
		return false
	}
	name := host
	if h, port, err := net.SplitHostPort(host); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		name = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		name = host[1 : len(host)-1]
		return net.ParseIP(name) != nil && strings.Contains(name, ":")
	} else if strings.ContainsAny(host, "[]") {
		return false
	}
	if net.ParseIP(name) != nil {
		return true
	}
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "-") {
		return false
	}
	for _, c := range name {
		if !(c == '.' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
