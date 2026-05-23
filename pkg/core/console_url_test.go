package core

import (
	"errors"
	"testing"
	"time"
)

type consoleTestConfig struct {
	console    string
	production bool
}

func (c consoleTestConfig) String(key string) string {
	if key == ConfigKeyConsoleURL {
		return c.console
	}
	return ""
}

func (c consoleTestConfig) Bool(key string) bool        { return key == "is_production" && c.production }
func (consoleTestConfig) Duration(string) time.Duration { return 0 }
func (consoleTestConfig) Strings(string) []string       { return nil }

func TestConsoleURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		want    string
		wantErr error
	}{
		{"set", consoleTestConfig{console: "https://admin.example.com"}, "https://admin.example.com", nil},
		{"trailing slash dropped", consoleTestConfig{console: "https://admin.example.com/"}, "https://admin.example.com", nil},
		{"set in production", consoleTestConfig{console: "https://admin.example.com", production: true}, "https://admin.example.com", nil},
		{"unset outside production", consoleTestConfig{}, DevConsoleURL, nil},
		{"unset in production", consoleTestConfig{production: true}, "", ErrConsoleURLUnset},
		{"no config", nil, "", ErrConsoleURLUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConsoleURL(tc.cfg)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
