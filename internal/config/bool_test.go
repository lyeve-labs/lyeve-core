package config

import (
	"os"
	"testing"
)

func TestEnvBool(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		fallback bool
		want     bool
	}{
		// Truthy values (strconv.ParseBool truthy)
		{name: "literal true", envVal: "true", fallback: false, want: true},
		{name: "literal 1", envVal: "1", fallback: false, want: true},
		{name: "literal TRUE", envVal: "TRUE", fallback: false, want: true},
		{name: "literal True", envVal: "True", fallback: false, want: true},
		{name: "literal t", envVal: "t", fallback: false, want: true},
		{name: "literal T", envVal: "T", fallback: false, want: true},

		// Falsy values (strconv.ParseBool falsy)
		{name: "literal false", envVal: "false", fallback: true, want: false},
		{name: "literal 0", envVal: "0", fallback: true, want: false},
		{name: "literal FALSE", envVal: "FALSE", fallback: true, want: false},
		{name: "literal False", envVal: "False", fallback: true, want: false},
		{name: "literal f", envVal: "f", fallback: true, want: false},
		{name: "literal F", envVal: "F", fallback: true, want: false},

		// Fallback when env is empty or unset
		{name: "empty string, fallback true", envVal: "", fallback: true, want: true},
		{name: "empty string, fallback false", envVal: "", fallback: false, want: false},

		// Invalid values -> fallback (silent)
		{name: "invalid value, fallback true", envVal: "garbage", fallback: true, want: true},
		{name: "invalid value, fallback false", envVal: "garbage", fallback: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "ENVBOOL_TEST"
			if tt.envVal == "" {
				os.Unsetenv(key)
			} else {
				os.Setenv(key, tt.envVal)
			}
			t.Cleanup(func() { os.Unsetenv(key) })

			got := envBool(key, tt.fallback)
			if got != tt.want {
				t.Errorf("envBool(%q, %v) with env[%q]=%q = %v, want %v",
					key, tt.fallback, key, tt.envVal, got, tt.want)
			}
		})
	}
}
