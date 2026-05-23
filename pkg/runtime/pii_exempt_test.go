package runtime

import "testing"

// The PII response middleware rewrites any email it finds in a JSON body.
// On the admin router that is wrong for the paths where the address is the
// record under administration, so the exemption set is part of the contract
// rather than an optimization: losing an entry turns the operator's own account,
// the user directory and the delete confirmation into "[redacted-email]".
func TestPIIExemptAdminPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"own identity", "/api/admin/auth/me", true},
		{"login response", "/api/admin/auth/login", true},
		{"user directory", "/api/admin/users", true},
		{"single user", "/api/admin/users/04b9137e-011d-4405-97a7-3d30a8cf5c19", true},
		{"invitations", "/api/admin/invitations", true},
		{"dsar export", "/api/admin/gdpr/export/abc", true},
		{"admin tokens with their owners", "/api/admin/admin-tokens", true},
		{"admin token request log", "/api/admin/admin-tokens/0b7c/requests", true},
		{"api keys", "/api/admin/api-keys", true},
		{"api key request log", "/api/admin/api-keys/0b7c/audit", true},

		{"content is masked", "/api/admin/content/articles", false},
		{"media is masked", "/api/admin/media", false},
		{"audit log is masked", "/api/admin/audit-log", false},
		{"widget submissions are masked", "/api/admin/widgets/submissions", false},
		{"the access log itself is masked", "/api/admin/pii/access-log", false},
		{"public api is masked", "/api/v1/content/articles", false},

		// A prefix must match at a segment boundary, so a route added later that
		// merely starts with an exempt path does not inherit the exemption.
		{"unrelated route sharing a prefix", "/api/admin/user-groups", false},
		{"route extending an exempt prefix", "/api/admin/users-report", false},
		{"route extending gdpr", "/api/admin/gdpr-settings", false},
		{"route extending api-keys", "/api/admin/api-keys-export", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := piiExemptAdminPath(tt.path); got != tt.want {
				t.Errorf("piiExemptAdminPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
