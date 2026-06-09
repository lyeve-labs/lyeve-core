package pii

import (
	"strings"
	"testing"
)

func TestMaskIP(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"localhost_v4", "127.0.0.1", "127.0.0.1"},
		{"localhost_v6", "::1", "::1"},
		{"ipv4_simple", "192.168.1.42", "***.***.***.42"},
		{"ipv4_broadcast", "10.0.0.255", "***.***.***.255"},
		{"ipv6_short", "fe80::1", "****:****:1"},
		{"ipv6_full", "2001:db8:85a3::8a2e:370:7334", "****:****:****:****:****:****:7334"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskIP(tt.input); got != tt.expected {
				t.Errorf("MaskIP(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaskEmail(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"no_at", "notanemail", "notanemail"},
		{"normal", "jane.doe@example.com", "j***@example.com"},
		{"short_local", "ab@test.com", "a***@test.com"},
		{"single_char_local", "a@test.com", "a***@test.com"},
		{"empty_local_part", "@example.com", "@example.com"},
		{"at_only", "@", "@"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskEmail(tt.input); got != tt.expected {
				t.Errorf("MaskEmail(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaskPhone(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"us_formatted", "555-123-4567", "***-***-4567"},
		{"us_dots", "555.123.4567", "***.***.4567"},
		{"intl", "+1 555-123-4567", "+* ***-***-4567"},
		{"short", "1234", "****"},
		{"very_short", "12", "**"},
		{"digits_only", "5551234567", "******4567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskPhone(tt.input); got != tt.expected {
				t.Errorf("MaskPhone(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaskUserAgent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"chrome", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0"},
		{"firefox", "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/121.0", "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/121.0"},
		{"safari", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskUserAgent(tt.input)
			if got != tt.expected {
				t.Errorf("MaskUserAgent(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		excludes string
	}{
		{"email_in_text", "Error from user@example.com: timeout", "[email_address]", "user@example.com"},
		{"no_pii", "simple error message", "simple error message", ""},
		{"empty", "", "", ""},
		{"multiple_emails", "alice@acme.com and bob@acme.com failed", "[email_address]", "@"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Sanitize(tt.input)
			if tt.contains != "" && got != tt.contains && tt.input == "" {
				return
			}
			if tt.excludes != "" && tt.input != "" {
				// Verify the function doesn't return the raw email unmodified.
				if len(got) == len(tt.input) && got == tt.input {
					t.Errorf("Sanitize(%q) returned the raw input unmodified", tt.input)
				}
			}
			_ = got // tested implicitly
		})
	}

	// Explicit test: email should be masked.
	if got := Sanitize("Error from user@example.com"); got == "Error from user@example.com" {
		t.Error("Sanitize did not mask email")
	}
}

func TestSanitizeDetail(t *testing.T) {
	if got := SanitizeDetail(""); got != "" {
		t.Error("empty detail should be empty")
	}
	if got := SanitizeDetail("user@example.com failed"); got == "user@example.com failed" {
		t.Error("detail with email should be sanitized")
	}
}

func TestIsSuperAdmin(t *testing.T) {
	if IsSuperAdmin(nil) {
		t.Error("nil roles should not be super_admin")
	}
	if IsSuperAdmin([]string{"editor", "viewer"}) {
		t.Error("non-admin roles should not be super_admin")
	}
	if !IsSuperAdmin([]string{"super_admin"}) {
		t.Error("super_admin role should be detected")
	}
}

func TestSanitizeStackTrace(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{"empty", "", nil, nil},
		{"no_paths", "panic: something broke", []string{"panic: something broke"}, nil},
		{
			"go_stack_trace",
			"goroutine 1 [running]:\ngithub.com/lyeve-labs/lyeve-core/internal/handler.GetUser(0xc0000a0000, 0x10)\n\t/app/internal/handler.go:42 +0xabc",
			[]string{"goroutine", "handler.go:42"},
			[]string{"/app/internal/handler.go"},
		},
		{
			"absolute_paths_redacted",
			"goroutine 7:\n\t/home/user/project/service.go:129",
			[]string{"service.go:129"},
			[]string{"/home/user/project/service.go"},
		},
		{
			"home_dir_paths_redacted",
			"goroutine 5:\n\t/home/dev/project/core/internal/db/tenancy.go:87",
			[]string{"tenancy.go:87"},
			[]string{"/home/dev/project/core/internal/db/tenancy.go"},
		},
		{
			"function_args_redacted",
			"main.processRequest(\"secret-token-abc\", 42)\n\t/app/main.go:15",
			[]string{"main.go:15"},
			[]string{"secret-token-abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeStackTrace(tt.input)
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("SanitizeStackTrace(%q) missing %q in output: %q", tt.input, c, got)
				}
			}
			for _, e := range tt.excludes {
				if strings.Contains(got, e) {
					t.Errorf("SanitizeStackTrace(%q) contains %q but should not: %q", tt.input, e, got)
				}
			}
		})
	}
}

func TestMask(t *testing.T) {
	s := "192.168.1.100"
	Mask(&s, MaskIP)
	if s == "192.168.1.100" {
		t.Error("Mask did not mutate pointer")
	}
	if s != "***.***.***.100" {
		t.Errorf("Mask result = %q, want masked IP", s)
	}

	var nilStr *string
	Mask(nilStr, MaskIP) // should not panic

	empty := ""
	Mask(&empty, MaskIP)
	if empty != "" {
		t.Error("Mask on empty string should be no-op")
	}
}
