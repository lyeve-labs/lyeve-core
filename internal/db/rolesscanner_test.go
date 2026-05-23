package db

import (
	"testing"
)

func TestRolesScanner_Scan_PostgresArray(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		want    []string
		wantErr bool
	}{
		{
			name:  "simple values",
			input: "{editor,admin}",
			want:  []string{"editor", "admin"},
		},
		{
			name:  "single value",
			input: "{editor}",
			want:  []string{"editor"},
		},
		{
			name:  "empty array",
			input: "{}",
			want:  []string{},
		},
		{
			name:  "quoted values",
			input: `{"quoted val",plain}`,
			want:  []string{"quoted val", "plain"},
		},
		{
			name:  "escaped quotes inside value",
			input: `{"with\"quote",normal}`,
			want:  []string{`with"quote`, "normal"},
		},
		{
			name:  "default editor role",
			input: `{"editor"}`,
			want:  []string{"editor"},
		},
		{
			name:  "three roles",
			input: "{editor,admin,superadmin}",
			want:  []string{"editor", "admin", "superadmin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			s := newRolesScanner(&got)
			if err := s.Scan(tt.input); (err != nil) != tt.wantErr {
				t.Errorf("Scan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !rolesEqual(got, tt.want) {
				t.Errorf("Scan() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRolesScanner_Scan_JSON(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		want    []string
		wantErr bool
	}{
		{
			name:  "json string array - MySQL style",
			input: `["editor","admin"]`,
			want:  []string{"editor", "admin"},
		},
		{
			name:  "json string single - MySQL style",
			input: `["editor"]`,
			want:  []string{"editor"},
		},
		{
			name:  "json empty array",
			input: `[]`,
			want:  []string{},
		},
		{
			name:  "json with spaces",
			input: `["editor", "admin"]`,
			want:  []string{"editor", "admin"},
		},
		{
			name:  "json as []byte - go-sql-driver/mysql returns []byte",
			input: []byte(`["editor","admin"]`),
			want:  []string{"editor", "admin"},
		},
		{
			name:    "invalid json",
			input:   `[not valid]`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			s := newRolesScanner(&got)
			if err := s.Scan(tt.input); (err != nil) != tt.wantErr {
				t.Errorf("Scan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !rolesEqual(got, tt.want) {
				t.Errorf("Scan() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRolesScanner_Scan_NilAndEmpty(t *testing.T) {
	// nil
	var roles []string
	s := newRolesScanner(&roles)
	if err := s.Scan(nil); err != nil {
		t.Errorf("Scan(nil) unexpected error: %v", err)
	}
	if roles != nil {
		t.Errorf("Scan(nil) = %v, want nil", roles)
	}

	// empty string
	roles = nil
	if err := s.Scan(""); err != nil {
		t.Errorf("Scan(\"\") unexpected error: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("Scan(\"\") = %v, want empty slice", roles)
	}
}

func TestRolesScanner_Scan_UnsupportedType(t *testing.T) {
	var roles []string
	s := newRolesScanner(&roles)
	if err := s.Scan(42); err == nil {
		t.Error("Scan(42) expected error, got nil")
	}
}

func TestSplitPGArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "simple",
			input: "editor,admin",
			want:  []string{"editor", "admin"},
		},
		{
			name:  "single",
			input: "editor",
			want:  []string{"editor"},
		},
		{
			name:  "empty string",
			input: "",
			want:  []string{""}, // single empty part
		},
		{
			name:  "quoted with comma inside",
			input: `"hello, world",plain`,
			want:  []string{"hello, world", "plain"},
		},
		{
			name:  "escaped quote",
			input: `"he said \"hello\" ",normal`,
			want:  []string{`he said "hello" `, "normal"},
		},
		{
			name:  "trailing whitespace ignored",
			input: `editor , admin`,
			want:  []string{"editor", "admin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitPGArray(tt.input)
			if !rolesEqual(got, tt.want) {
				t.Errorf("splitPGArray(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// rolesEqual compares two string slices for equality.
func rolesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
