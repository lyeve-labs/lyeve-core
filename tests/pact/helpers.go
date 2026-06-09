//go:build pact
// +build pact

package pact

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pact-foundation/pact-go/v2/models"
)

func findPactFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("Pact directory %q not readable: %v", dir, err)
		return nil
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

// buildStateHandlers returns provider state handlers for verification. Every
// state is accepted without setup, so verification runs against an instance
// seeded beforehand.
func buildStateHandlers() models.StateHandlers {
	return models.StateHandlers{
		"a tenant exists":                         stateOK,
		"a content item exists":                   stateOK,
		"an API key exists":                       stateOK,
		"a user with admin role is authenticated": stateOK,
		"a cron job exists":                       stateOK,
		"a webhook exists":                        stateOK,
		"a media file exists":                     stateOK,
		"a tenant with audit log entries exists":  stateOK,
		"an auth provider is configured":          stateOK,
		"an MFA secret is enrolled":               stateOK,
		"a schema definition exists":              stateOK,
		"clean test data":                         stateOK,
		"an OAuth provider is configured":         stateOK,
		"a scheduled content item exists":         stateOK,
		"a content revision history exists":       stateOK,
		"no items exist":                          stateOK,
		"an analytics event has been tracked":     stateOK,
		"a rate limit rule exists":                stateOK,
		"users exist":                             stateOK,
		"roles exist":                             stateOK,
		"a webhook delivery exists":               stateOK,
		"an MFA challenge is pending":             stateOK,
		"a WebAuthn credential is registered":     stateOK,
		"a replay run exists":                     stateOK,
		"a usage snapshot exists":                 stateOK,
	}
}

// stateOK is a no-op state handler for states that don't need fixture setup.
func stateOK(setup bool, s models.ProviderState) (models.ProviderStateResponse, error) {
	if setup {
		return models.ProviderStateResponse{
			"status": "ok",
			"state":  s.Name,
		}, nil
	}
	return nil, nil
}

const apiBase = "/api/admin"

func route(base, path string) string {
	return fmt.Sprintf("%s%s", base, path)
}
