//go:build pact
// +build pact

// Package pact provides Pact provider verification for LyEve CMS.
// It verifies that the running CMS server honors contracts defined by
// consumers (admin dashboard, public API clients, etc.).
//
// The consumers generate the pact files and live in their own repositories.
// Collect those files, point PACT_FILES_DIR at them, start the server, then
// verify:
//
//	make run-go-all
//	PACT_FILES_DIR=<dir> make test-pact
//
// PROVIDER_HOST, PROVIDER_PORT and PROVIDER_PROTO override where the running
// server is; SKIP_PACT_VERIFY=1 skips the check entirely.
package pact

import (
	"os"
	"testing"

	"github.com/pact-foundation/pact-go/v2/provider"
)

const (
	providerName  = "lyeve-cms"
	providerPort  = "3002"
	providerHost  = "localhost"
	providerProto = "http"
)

// TestProviderVerification runs full provider-side Pact verification against
// a running CMS instance. Requires consumer pacts to exist on disk (generated
// by the consumer pact tests). Skip with SKIP_PACT_VERIFY=1.
func TestProviderVerification(t *testing.T) {
	if os.Getenv("SKIP_PACT_VERIFY") == "1" {
		t.Skip("SKIP_PACT_VERIFY=1")
	}

	baseURL := providerURL()
	pactDir := pactFilesDirectory()

	pactURLs := findPactFiles(t, pactDir)
	if len(pactURLs) == 0 {

	}

	t.Logf("Provider: %s | Base URL: %s | Pact files: %d", providerName, baseURL, len(pactURLs))

	v := provider.NewVerifier()
	req := provider.VerifyRequest{
		ProviderBaseURL:    baseURL,
		PactFiles:          pactURLs,
		Provider:           providerName,
		StateHandlers:      buildStateHandlers(),
		FailIfNoPactsFound: true,
	}

	if err := v.VerifyProvider(t, req); err != nil {
		t.Fatalf("Pact verification failed: %v", err)
	}
}

func providerURL() string {
	port := os.Getenv("PROVIDER_PORT")
	if port == "" {
		port = providerPort
	}
	host := os.Getenv("PROVIDER_HOST")
	if host == "" {
		host = providerHost
	}
	proto := os.Getenv("PROVIDER_PROTO")
	if proto == "" {
		proto = providerProto
	}
	return proto + "://" + host + ":" + port
}

func pactFilesDirectory() string {
	if d := os.Getenv("PACT_FILES_DIR"); d != "" {
		return d
	}
	return ""
}
