package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
)

// LoopbackHeader carries the mark of a request the engine's own process sent
// to its own listener on a person's behalf, with that person's credential.
const LoopbackHeader = "X-Lyeve-Loopback"

// loopbackMark is drawn once per process and is never written anywhere a
// caller outside the process can read it: the middleware removes it from every
// request before any handler, capture or log sees the headers.
var loopbackMark = rand.Text()

// loopbackCredentialHeaders are the headers that say who a loopback call acts
// for. Their digest separates one person's calls from another's.
var loopbackCredentialHeaders = []string{"Authorization", "X-API-Key", "Cookie"}

type loopbackBucketKey struct{}

// MarkLoopback sets the process's loopback mark on req, a request this
// process is about to send to its own listener.
func MarkLoopback(req *http.Request) {
	req.Header.Set(LoopbackHeader, loopbackMark)
}

// LoopbackCaller recognizes a request this process sent to itself and records
// a rate limit bucket for it named by the tenant and the credential it carries.
// Every in-process call arrives from the engine's own address, so keyed by
// address they would all share one bucket, and one tenant's calls would use up
// every other tenant's.
//
// Arriving from the engine's own address proves nothing, since a reverse proxy
// on the same host does too, and neither does a header any client can send. A
// request counts as the engine's own only when it carries the mark only this
// process holds and comes from the address it reached the engine at. Every
// other request, a wrong mark included, keeps its address bucket. The header
// is removed from every request, whatever the outcome.
//
// It must run after ClientAddress, so a request a proxy forwarded is compared
// by its client's address rather than the proxy's.
func LoopbackCaller() func(http.Handler) http.Handler {
	mark := []byte(loopbackMark)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get(LoopbackHeader)
			r.Header.Del(LoopbackHeader)
			if got != "" && subtle.ConstantTimeCompare([]byte(got), mark) == 1 && fromOwnAddress(r) {
				r = r.WithContext(context.WithValue(r.Context(), loopbackBucketKey{}, loopbackBucket(r)))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// fromOwnAddress reports whether the caller's address is the address the
// connection reached the engine at, which holds only for a caller on the
// engine's own host.
func fromOwnAddress(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	localHost, _, err := net.SplitHostPort(local.String())
	if err != nil {
		return false
	}
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	localIP, peerIP := net.ParseIP(localHost), net.ParseIP(peerHost)
	return localIP != nil && localIP.Equal(peerIP)
}

// loopbackBucket names a loopback call's bucket by the tenant the caller set
// and a digest of the credential headers, so the credential itself is never
// held as a key.
func loopbackBucket(r *http.Request) string {
	sum := sha256.New()
	for _, h := range loopbackCredentialHeaders {
		for _, v := range r.Header.Values(h) {
			sum.Write([]byte(h + "\x00" + v + "\x00"))
		}
	}
	return "loopback:" + r.Header.Get("X-Tenant-ID") + ":" + hex.EncodeToString(sum.Sum(nil))
}

// loopbackBucketFromCtx returns the bucket LoopbackCaller recorded, or "" for
// a request that is not the engine's own.
func loopbackBucketFromCtx(r *http.Request) string {
	k, _ := r.Context().Value(loopbackBucketKey{}).(string)
	return k
}
