package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchJWKS: oversized body rejection (io.LimitReader)

func TestFetchJWKS_OversizedBody(t *testing.T) {
	clearJWKSCache()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ~2 MiB JSON to trip the 1 MiB limit.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"padding": strings.Repeat("x", 2<<20),
		})
	}))
	defer srv.Close()

	_, err := fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: decode")
}

// fetchJWKS: negative cache (down/erroring provider not re-hit within TTL)

func TestFetchJWKS_NegativeCache_FetchError(t *testing.T) {
	clearJWKSCache()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: status 500")
	assert.Equal(t, int32(1), reqCount.Load())

	_, err = fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: status 500")
	assert.Equal(t, int32(1), reqCount.Load(), "second call must use negative cache, not hit server")
}

func TestFetchJWKS_NegativeCache_Non200Status(t *testing.T) {
	clearJWKSCache()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: status 503")
	assert.Equal(t, int32(1), reqCount.Load())

	_, err = fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: status 503")
	assert.Equal(t, int32(1), reqCount.Load())
}

func TestFetchJWKS_NegativeCache_DecodeError(t *testing.T) {
	clearJWKSCache()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not valid json {{{"))
	}))
	defer srv.Close()

	_, err := fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: decode")
	assert.Equal(t, int32(1), reqCount.Load())

	_, err = fetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwks: decode")
	assert.Equal(t, int32(1), reqCount.Load())
}

// fetchJWKS: positive cache unchanged (successful fetch)

func TestFetchJWKS_PositiveCacheUnchanged(t *testing.T) {
	clearJWKSCache()

	var reqCount atomic.Int32
	key, _ := genRSA2048(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		jwksServerHandler(t, []rsaJWK{{kid: "k1", pub: key}})(w, r)
	}))
	defer srv.Close()

	keys, err := fetchJWKS(context.Background(), srv.URL)
	require.NoError(t, err)
	require.NotNil(t, keys["k1"])
	assert.Equal(t, int32(1), reqCount.Load())

	keys, err = fetchJWKS(context.Background(), srv.URL)
	require.NoError(t, err)
	require.NotNil(t, keys["k1"])
	assert.Equal(t, int32(1), reqCount.Load(), "second call must use positive cache")
}

// helpers

func clearJWKSCache() {
	jwksMu.Lock()
	jwksCache = map[string]*jwksEntry{}
	jwksMu.Unlock()
}

// Same logic as jwksServer but returns a handler for composition with request counters.
func jwksServerHandler(t *testing.T, keys []rsaJWK) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		type jwkResp struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		}
		var set struct {
			Keys []jwkResp `json:"keys"`
		}
		for _, k := range keys {
			eInt := k.pub.E
			set.Keys = append(set.Keys, jwkResp{
				Kty: "RSA",
				Kid: k.kid,
				Alg: "RS256",
				Use: "sig",
				N:   base64.RawURLEncoding.EncodeToString(k.pub.N.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(eInt)).Bytes()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}
}
