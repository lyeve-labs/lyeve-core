package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

const (
	awsAlgorithm  = "AWS4-HMAC-SHA256"
	awsService    = "s3"
	awsRequest    = "aws4_request"
	awsTimeFormat = "20060102T150405Z"
	awsDateFormat = "20060102"
)

// s3Provider implements StorageProvider for AWS S3.
type s3Provider struct {
	name string // "s3" or "minio"
}

// S3Provider returns a StorageProvider for AWS S3.
func S3Provider() StorageProvider {
	return &s3Provider{name: "s3"}
}

// MinIOProvider returns a StorageProvider for MinIO (S3-compatible).
func MinIOProvider() StorageProvider {
	return &s3Provider{name: "minio"}
}

func (p *s3Provider) Name() string       { return p.name }
func (p *s3Provider) Category() Category { return CategoryStorage }

func (p *s3Provider) Capabilities() Capabilities {
	return NewCapabilities(
		StorageCapSignedURL,
		StorageCapMultipart,
		StorageCapLifecycle,
		StorageCapVersioning,
		StorageCapEncryptionAtRest,
		StorageCapCDN,
		StorageCapEventNotification,
		StorageCapCrossRegion,
	)
}

func (p *s3Provider) HealthCheck(ctx context.Context) error { return nil }

func (p *s3Provider) AutoDetect(dsn string) bool {
	detected := detectStorageFromDSN(dsn)
	if detected == "" && strings.Contains(strings.ToLower(dsn), "s3") {
		return p.name == "s3"
	}
	return detected == p.name
}

// Connect creates an S3-compatible client with AWS Signature V4 signing.
// SigV4 (AWS4-HMAC-SHA256) is computed per-request using the provided
// access key, secret key, and region.
func (p *s3Provider) Connect(ctx context.Context, config StorageConfig) (StorageClient, error) {
	if config.Bucket == "" {
		return nil, fmt.Errorf("s3(%s): bucket is required", p.name)
	}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.AccessKey == "" || config.SecretKey == "" {
		return nil, fmt.Errorf("s3(%s): access_key and secret_key are required", p.name)
	}
	endpoint := config.Endpoint
	if endpoint == "" && p.name == "minio" {
		return nil, fmt.Errorf("minio: endpoint is required (e.g. http://localhost:9000)")
	}

	scheme := "https"
	if !config.UseSSL {
		scheme = "http"
	}

	httpTimeout := 30 * time.Second
	if config.HTTPTimeout > 0 {
		httpTimeout = config.HTTPTimeout
	}
	client := &s3Client{
		bucket:      config.Bucket,
		region:      config.Region,
		accessKey:   config.AccessKey,
		secretKey:   config.SecretKey,
		endpoint:    endpoint,
		cdnBaseURL:  config.CDNBaseURL,
		multipartMB: config.MultipartThresholdMB,
		forcePath:   config.ForcePathStyle,
		useSSL:      config.UseSSL,
		scheme:      scheme,
		http:        &http.Client{Timeout: httpTimeout},
	}
	return client, nil
}

// s3Client implements StorageClient backed by HTTP requests to an S3 API.
// All requests are signed with AWS Signature V4 (AWS4-HMAC-SHA256).
// Supports AWS S3 and S3-compatible backends (MinIO, etc.).
type s3Client struct {
	bucket      string
	region      string
	accessKey   string
	secretKey   string
	endpoint    string
	cdnBaseURL  string
	multipartMB int64
	forcePath   bool
	useSSL      bool
	scheme      string
	http        *http.Client
}

func (c *s3Client) baseURL() string {
	if c.endpoint != "" {
		if c.forcePath {
			return c.scheme + "://" + c.endpoint + "/" + c.bucket
		}
		return c.scheme + "://" + c.bucket + "." + c.endpoint
	}
	if c.forcePath {
		return c.scheme + "://s3." + c.region + ".amazonaws.com/" + c.bucket
	}
	return c.scheme + "://" + c.bucket + ".s3." + c.region + ".amazonaws.com"
}

// sign applies AWS Signature V4 (AWS4-HMAC-SHA256) to req.
// Sets X-Amz-Date, X-Amz-Content-SHA256, and Authorization headers.
func (c *s3Client) sign(req *http.Request, bodyHash string) {
	now := time.Now().UTC()
	amzDate := now.Format(awsTimeFormat)
	dateStamp := now.Format(awsDateFormat)

	payloadHash := bodyHash
	if payloadHash == "" {
		h := sha256.Sum256(nil) // empty body
		payloadHash = hex.EncodeToString(h[:])
	}
	req.Header.Set("X-Amz-Content-SHA256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)

	// Canonical headers: sorted, lowercase keys, trimmed values.
	var headerKeys []string
	canonicalHeaders := make(map[string]string)
	for k, vv := range req.Header {
		key := strings.ToLower(k)
		// User-Agent can cause issues with some backends. Exclude from signing.
		if key == "user-agent" {
			continue
		}
		if _, exists := canonicalHeaders[key]; !exists {
			canonicalHeaders[key] = strings.TrimSpace(vv[0])
			headerKeys = append(headerKeys, key)
		}
	}
	sort.Strings(headerKeys)

	var canonHeaderStr, signedHeaderStr strings.Builder
	for _, k := range headerKeys {
		canonHeaderStr.WriteString(k)
		canonHeaderStr.WriteByte(':')
		canonHeaderStr.WriteString(canonicalHeaders[k])
		canonHeaderStr.WriteByte('\n')
		if signedHeaderStr.Len() > 0 {
			signedHeaderStr.WriteByte(';')
		}
		signedHeaderStr.WriteString(k)
	}
	signedHeaders := signedHeaderStr.String()

	canonicalURI := req.URL.Path
	canonicalQuery := req.URL.RawQuery

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonHeaderStr.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	canonicalRequestHash := sha256Hex([]byte(canonicalRequest))

	credentialScope := strings.Join([]string{dateStamp, c.region, awsService, awsRequest}, "/")

	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		canonicalRequestHash,
	}, "\n")

	signingKey := c.signingKey(dateStamp)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("%s Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		awsAlgorithm, c.accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

// signingKey derives the AWS SigV4 signing key for a given date.
func (c *s3Client) signingKey(dateStamp string) []byte {
	kSecret := []byte("AWS4" + c.secretKey)
	kDate := hmacSHA256(kSecret, []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(c.region))
	kService := hmacSHA256(kRegion, []byte(awsService))
	return hmacSHA256(kService, []byte(awsRequest))
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func (c *s3Client) Put(ctx context.Context, key, contentType string, r io.Reader) (*core.StorageObject, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("s3 put read: %w", err)
	}
	url := c.baseURL() + "/" + key

	bodyReader := bytes.NewReader(data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("s3 put request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))

	bodyHash := sha256Hex(data)
	c.sign(req, bodyHash)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 put: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("s3 put: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return &core.StorageObject{
		Key:          key,
		ContentType:  contentType,
		Size:         int64(len(data)),
		LastModified: time.Now(),
	}, nil
}

// maxErrorBodyBytes bounds the S3 error document quoted back in an error.
// It exists to be quoted, so a truncated one still names the failure, and a
// storage endpoint can be pointed anywhere by configuration.
const maxErrorBodyBytes = 32 << 10

func (c *s3Client) Get(ctx context.Context, key string) (io.ReadCloser, *core.StorageObject, error) {
	url := c.baseURL() + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("s3 get request: %w", err)
	}
	c.sign(req, "")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("s3 get: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("s3 get: key not found: %s", key)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		resp.Body.Close()
		return nil, nil, fmt.Errorf("s3 get: HTTP %d: %s", resp.StatusCode, string(body))
	}

	obj := &core.StorageObject{
		Key:         key,
		ContentType: resp.Header.Get("Content-Type"),
		Size:        resp.ContentLength,
		ETag:        resp.Header.Get("ETag"),
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			obj.LastModified = t
		}
	}
	return resp.Body, obj, nil
}

func (c *s3Client) Delete(ctx context.Context, key string) error {
	url := c.baseURL() + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("s3 delete request: %w", err)
	}
	c.sign(req, "")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // deleting a missing key is not an error per S3 semantics
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("s3 delete: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *s3Client) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if c.cdnBaseURL != "" {
		return strings.TrimRight(c.cdnBaseURL, "/") + "/" + key, nil
	}

	// Pre-signed URL via SigV4 query parameter authentication.
	url := c.baseURL() + "/" + key
	now := time.Now().UTC()
	amzDate := now.Format(awsTimeFormat)
	dateStamp := now.Format(awsDateFormat)
	expires := int64(ttl.Seconds())
	if expires < 1 {
		expires = 3600 // default 1 hour
	}

	credentialScope := strings.Join([]string{dateStamp, c.region, awsService, awsRequest}, "/")

	// Pre-signed URLs use UNSIGNED-PAYLOAD. Query params carry the auth.
	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalURI := "/" + c.bucket + "/" + key
	canonicalQuery := fmt.Sprintf(
		"X-Amz-Algorithm=%s&X-Amz-Credential=%s%%2F%s%%2F%s%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=%d&X-Amz-SignedHeaders=host",
		awsAlgorithm,
		c.accessKey,
		dateStamp,
		c.region,
		amzDate,
		expires,
	)

	host := reqHost(c.bucket, c.region, c.endpoint, c.forcePath)
	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	canonicalRequestHash := sha256Hex([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		canonicalRequestHash,
	}, "\n")

	signingKey := c.signingKey(dateStamp)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	return url + "?" + canonicalQuery + "&X-Amz-Signature=" + signature, nil
}

func (c *s3Client) List(ctx context.Context, prefix string, limit int) ([]*core.StorageObject, error) {
	return nil, fmt.Errorf("s3 list: not yet implemented - requires S3 ListObjectsV2 API")
}

func (c *s3Client) Ping(ctx context.Context) error {
	url := c.baseURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("s3 ping request: %w", err)
	}
	c.sign(req, "")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3 ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("s3 ping: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *s3Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// reqHost returns the Host header value for a given S3 endpoint configuration.
func reqHost(bucket, region, endpoint string, forcePath bool) string {
	if endpoint != "" {
		if forcePath {
			return endpoint
		}
		return bucket + "." + endpoint
	}
	if forcePath {
		return "s3." + region + ".amazonaws.com"
	}
	return bucket + ".s3." + region + ".amazonaws.com"
}

// init registers all storage providers.
func init() {
	Registry.Register(S3Provider())
	Registry.Register(MinIOProvider())
}
