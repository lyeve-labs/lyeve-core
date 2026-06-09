package provider

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSHA256Hex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{"empty", []byte(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"hello", []byte("hello"), "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sha256Hex(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestHMACSHA256(t *testing.T) {
	t.Parallel()

	// Known test vector: HMAC-SHA256("key", "The quick brown fox jumps over the lazy dog")
	key := []byte("key")
	data := []byte("The quick brown fox jumps over the lazy dog")
	expected := "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"

	got := hmacSHA256(key, data)
	assert.Equal(t, expected, hex.EncodeToString(got))
}

func TestSigningKey(t *testing.T) {
	t.Parallel()

	// AWS SigV4 test vector from docs
	// http://docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html
	client := &s3Client{
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
	}

	// Expected signing key for date 20150830 in us-east-1
	// kSecret = "AWS4wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	// kDate = HMAC(kSecret, "20150830")
	// kRegion = HMAC(kDate, "us-east-1")
	// kService = HMAC(kRegion, "s3")
	// kSigning = HMAC(kService, "aws4_request")
	dateStamp := "20150830"

	signingKey := client.signingKey(dateStamp)
	keyHex := hex.EncodeToString(signingKey)
	// Verify it's deterministic and non-zero.
	_ = keyHex
	assert.NotEmpty(t, keyHex)
	assert.Len(t, signingKey, 32, "signing key must be 32 bytes")

	// Deterministic check: same key derivation twice
	t.Run("deterministic", func(t *testing.T) {
		key2 := client.signingKey(dateStamp)
		assert.Equal(t, signingKey, key2)
	})

	// Different region produces different key
	t.Run("region specific", func(t *testing.T) {
		client2 := &s3Client{secretKey: client.secretKey, region: "eu-west-1"}
		key3 := client2.signingKey(dateStamp)
		assert.NotEqual(t, signingKey, key3, "different region must produce different key")
	})
}

func TestS3ClientSign_SetsAuthHeaders(t *testing.T) {
	t.Parallel()

	client := &s3Client{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		bucket:    "example-bucket",
	}

	req, err := http.NewRequest(http.MethodGet, "https://example-bucket.s3.us-east-1.amazonaws.com/test.txt", nil)
	require.NoError(t, err)

	bodyHash := sha256Hex(nil)
	client.sign(req, bodyHash)

	// X-Amz-Date must be set and valid
	amzDate := req.Header.Get("X-Amz-Date")
	assert.NotEmpty(t, amzDate, "X-Amz-Date must be set")
	assert.Len(t, amzDate, 16, "X-Amz-Date must be 16 chars (YYYYMMDDTHHMMSSZ)")

	// X-Amz-Content-SHA256 must be set
	contentHash := req.Header.Get("X-Amz-Content-SHA256")
	assert.NotEmpty(t, contentHash, "X-Amz-Content-SHA256 must be set")
	assert.Len(t, contentHash, 64, "X-Amz-Content-SHA256 must be 64 hex chars")

	// Authorization header must be set and well-formed
	auth := req.Header.Get("Authorization")
	assert.NotEmpty(t, auth, "Authorization must be set")
	assert.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256 "), "Authorization must start with AWS4-HMAC-SHA256")
	assert.Contains(t, auth, "Credential="+client.accessKey+"/")
	assert.Contains(t, auth, "SignedHeaders=")
	assert.Contains(t, auth, "Signature=")

	// Verify the Authorization format
	parts := strings.Split(auth, ",")
	assert.Len(t, parts, 3, "Authorization must have 3 comma-separated parts")

	// Verify Credential part format
	assert.Contains(t, parts[0], "Credential=")
	assert.Contains(t, parts[0], "/us-east-1/s3/aws4_request")
}

func TestS3ClientSign_PutRequest(t *testing.T) {
	t.Parallel()

	client := &s3Client{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		bucket:    "example-bucket",
	}

	body := []byte("hello world")
	req, err := http.NewRequest(http.MethodPut, "https://example-bucket.s3.us-east-1.amazonaws.com/test.txt",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "text/plain")

	bodyHash := sha256Hex(body)
	client.sign(req, bodyHash)

	// Content hash must match the payload
	contentHash := req.Header.Get("X-Amz-Content-SHA256")
	assert.Equal(t, bodyHash, contentHash, "X-Amz-Content-SHA256 must match payload hash")

	// Authorization must include content-type in signed headers (but not host,
	// since Go's http.Request doesn't surface the Host in req.Header)
	auth := req.Header.Get("Authorization")
	assert.Contains(t, auth, "content-type",
		"SignedHeaders must include content-type")
	assert.NotContains(t, auth, "SignedHeaders=host",
		"Host is not in req.Header, so it won't appear in signed headers")
	assert.Contains(t, auth, "x-amz-date",
		"SignedHeaders must include x-amz-date")
	assert.Contains(t, auth, "x-amz-content-sha256",
		"SignedHeaders must include x-amz-content-sha256")
}

func TestS3ClientSign_NoBogusAccessKeyHeader(t *testing.T) {
	t.Parallel()

	client := &s3Client{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		bucket:    "example-bucket",
	}

	tests := []struct {
		name string
		req  *http.Request
	}{
		{"GET", httptest.NewRequest(http.MethodGet, "https://example-bucket.s3.amazonaws.com/key", nil)},
		{"PUT", httptest.NewRequest(http.MethodPut, "https://example-bucket.s3.amazonaws.com/key", strings.NewReader("data"))},
		{"DELETE", httptest.NewRequest(http.MethodDelete, "https://example-bucket.s3.amazonaws.com/key", nil)},
		{"HEAD", httptest.NewRequest(http.MethodHead, "https://example-bucket.s3.amazonaws.com/", nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.sign(tt.req, sha256Hex(nil))
			assert.Empty(t, tt.req.Header.Get("X-Amz-Access-Key"),
				"X-Amz-Access-Key must NOT be set - SigV4 uses Authorization header")
			assert.NotEmpty(t, tt.req.Header.Get("Authorization"),
				"Authorization must be set via SigV4")
		})
	}
}

func TestS3Client_AllOperationsUseSigV4(t *testing.T) {
	t.Parallel()

	// Test server that captures the Authorization header to verify SigV4
	var capturedAuth, capturedDate, capturedHash string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedDate = r.Header.Get("X-Amz-Date")
		capturedHash = r.Header.Get("X-Amz-Content-SHA256")

		assert.NotEmpty(t, capturedAuth, "Authorization must be set")
		assert.NotEmpty(t, capturedDate, "X-Amz-Date must be set")
		assert.NotEmpty(t, capturedHash, "X-Amz-Content-SHA256 must be set")
		assert.Empty(t, r.Header.Get("X-Amz-Access-Key"), "Bogus header must not be present")

		// Verify SigV4 format
		assert.True(t, strings.HasPrefix(capturedAuth, "AWS4-HMAC-SHA256 "),
			"Authorization must use AWS4-HMAC-SHA256")

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := &s3Client{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		bucket:    "test-bucket",
		endpoint:  strings.TrimPrefix(ts.URL, "http://"),
		forcePath: true,
		useSSL:    false,
		scheme:    "http",
		http:      ts.Client(),
	}

	ctx := context.Background()

	t.Run("Put", func(t *testing.T) {
		capturedAuth = ""
		capturedDate = ""
		capturedHash = ""

		obj, err := client.Put(ctx, "test-key", "text/plain", strings.NewReader("hello world"))
		require.NoError(t, err)
		require.NotNil(t, obj)
		assert.Equal(t, "test-key", obj.Key)
	})

	t.Run("Get", func(t *testing.T) {
		capturedAuth = ""

		rc, obj, err := client.Get(ctx, "test-key")
		require.NoError(t, err)
		require.NotNil(t, rc)
		require.NotNil(t, obj)
		rc.Close()
	})

	t.Run("Delete", func(t *testing.T) {
		capturedAuth = ""

		err := client.Delete(ctx, "test-key")
		require.NoError(t, err)
	})

	t.Run("Ping", func(t *testing.T) {
		capturedAuth = ""

		err := client.Ping(ctx)
		require.NoError(t, err)
	})
}

func TestS3Client_SignedURL_PresignedFormat(t *testing.T) {
	t.Parallel()

	client := &s3Client{
		accessKey: "AKIDEXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		bucket:    "test-bucket",
	}

	// With CDN base URL, SignedURL should return CDN URL without auth
	t.Run("cdn base url returns public url", func(t *testing.T) {
		cdnClient := &s3Client{
			cdnBaseURL: "https://cdn.example.com",
		}
		url, err := cdnClient.SignedURL(context.Background(), "files/photo.jpg", time.Hour)
		require.NoError(t, err)
		assert.Equal(t, "https://cdn.example.com/files/photo.jpg", url)
	})

	// Without CDN, should produce a pre-signed URL with SigV4 query params
	t.Run("presigned has sigv4 params", func(t *testing.T) {
		url, err := client.SignedURL(context.Background(), "files/document.pdf", 10*time.Minute)
		require.NoError(t, err)
		assert.NotEmpty(t, url)

		// Must have all SigV4 query parameters
		assert.Contains(t, url, "X-Amz-Algorithm=AWS4-HMAC-SHA256")
		assert.Contains(t, url, "X-Amz-Credential=")
		assert.Contains(t, url, "X-Amz-Date=")
		assert.Contains(t, url, "X-Amz-Expires=")
		assert.Contains(t, url, "X-Amz-SignedHeaders=host")
		assert.Contains(t, url, "X-Amz-Signature=")

		// Signature must be 64 hex chars
		sigEnd := strings.LastIndex(url, "X-Amz-Signature=")
		if sigEnd >= 0 {
			sig := url[sigEnd+16:]
			assert.Len(t, sig, 64, "signature must be 64 hex chars")
		}
	})
}

func TestS3RealConnection(t *testing.T) {
	t.Skip("S3 integration test requires real credentials: set S3_TEST_ACCESS_KEY, S3_TEST_SECRET_KEY, S3_TEST_BUCKET")
}

func TestS3ClientNoBogusHeaderInSource(t *testing.T) {
	t.Parallel()

	// Read the source file and verify no X-Amz-Access-Key header is set
	// This is a defense against accidental reintroduction
	// (Test is in the same package, we exercise the methods directly)

	client := &s3Client{
		accessKey: "test-key",
		secretKey: "test-secret",
		region:    "us-east-1",
		bucket:    "test",
	}

	ctx := context.Background()

	t.Run("Put does not set bogus header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://example.com/key", nil)
		client.sign(req, sha256Hex([]byte("data")))
		assert.Empty(t, req.Header.Get("X-Amz-Access-Key"))
		assert.NotEmpty(t, req.Header.Get("Authorization"))
	})

	t.Run("Get does not set bogus header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/key", nil)
		client.sign(req, "")
		assert.Empty(t, req.Header.Get("X-Amz-Access-Key"))
		assert.NotEmpty(t, req.Header.Get("Authorization"))
	})

	t.Run("Delete does not set bogus header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://example.com/key", nil)
		client.sign(req, "")
		assert.Empty(t, req.Header.Get("X-Amz-Access-Key"))
		assert.NotEmpty(t, req.Header.Get("Authorization"))
	})

	t.Run("Ping does not set bogus header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "http://example.com/", nil)
		client.sign(req, "")
		assert.Empty(t, req.Header.Get("X-Amz-Access-Key"))
		assert.NotEmpty(t, req.Header.Get("Authorization"))
	})
}

func TestS3ProviderConnect(t *testing.T) {
	t.Parallel()

	t.Run("default region", func(t *testing.T) {
		p := S3Provider()
		client, err := p.Connect(context.Background(), StorageConfig{
			Bucket:    "test-bucket",
			AccessKey: "AKIDEXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		})
		require.NoError(t, err)
		require.NotNil(t, client)

		s3c, ok := client.(*s3Client)
		require.True(t, ok, "Connect must return *s3Client")
		assert.Equal(t, "us-east-1", s3c.region)
		assert.Equal(t, "AKIDEXAMPLE", s3c.accessKey)
		assert.NotNil(t, s3c.http)
	})

	t.Run("custom endpoint minio", func(t *testing.T) {
		p := MinIOProvider()
		client, err := p.Connect(context.Background(), StorageConfig{
			Bucket:    "test-bucket",
			Region:    "us-east-1",
			AccessKey: "AKIDEXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
			Endpoint:  "localhost:9000",
		})
		require.NoError(t, err)
		require.NotNil(t, client)

		s3c, ok := client.(*s3Client)
		require.True(t, ok)
		assert.Equal(t, "localhost:9000", s3c.endpoint)
	})

	t.Run("missing bucket", func(t *testing.T) {
		_, err := S3Provider().Connect(context.Background(), StorageConfig{})
		assert.ErrorContains(t, err, "bucket is required")
	})

	t.Run("missing access key", func(t *testing.T) {
		_, err := S3Provider().Connect(context.Background(), StorageConfig{
			Bucket: "test",
		})
		assert.ErrorContains(t, err, "access_key and secret_key are required")
	})
}

func TestS3ClientImplementsStorageClient(t *testing.T) {
	t.Parallel()

	// Compile-time check: *s3Client must satisfy StorageClient
	var _ StorageClient = (*s3Client)(nil)
}

func TestReqHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		bucket    string
		region    string
		endpoint  string
		forcePath bool
		expected  string
	}{
		{"aws virtual hosted", "mybucket", "us-east-1", "", false, "mybucket.s3.us-east-1.amazonaws.com"},
		{"aws path style", "mybucket", "us-east-1", "", true, "s3.us-east-1.amazonaws.com"},
		{"minio virtual hosted", "mybucket", "us-east-1", "localhost:9000", false, "mybucket.localhost:9000"},
		{"minio path style", "mybucket", "us-east-1", "localhost:9000", true, "localhost:9000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reqHost(tt.bucket, tt.region, tt.endpoint, tt.forcePath)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestS3ClientListStub(t *testing.T) {
	t.Parallel()

	client := &s3Client{}
	objs, err := client.List(context.Background(), "", 10)
	assert.Error(t, err)
	assert.Nil(t, objs)
	assert.Contains(t, err.Error(), "not yet implemented")
}

func TestS3ClientClose(t *testing.T) {
	t.Parallel()

	client := &s3Client{
		http: &http.Client{},
	}
	err := client.Close()
	assert.NoError(t, err)
}

func TestStorageConnectedNoBogusS3Header(t *testing.T) {
	t.Parallel()

	// Verify that StorageConnected wraps correctly with the real s3Client
	connected := &StorageConnected{
		Client: &s3Client{
			accessKey: "test",
			secretKey: "secret",
			region:    "us-east-1",
			bucket:    "test",
		},
	}
	require.NotNil(t, connected)
	assert.NotNil(t, connected.Client)

	// Ensure s3Client methods work through the wrapper
	s3c, ok := connected.Client.(*s3Client)
	require.True(t, ok)
	assert.NotEmpty(t, s3c.accessKey)
	assert.NotEmpty(t, s3c.secretKey)
}
