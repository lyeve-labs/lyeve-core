package i18n

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodeError(t *testing.T) {
	// English
	en := LocaleEN
	fr := LocaleFR
	ja := Locale("ja")

	t.Run("known code en", func(t *testing.T) {
		msg := CodeInvalidCredentials.Error(en, nil)
		assert.Equal(t, "Invalid email or password.", msg)
	})

	t.Run("known code fr fallback", func(t *testing.T) {
		msg := CodeInvalidCredentials.Error(fr, nil)
		assert.Equal(t, "Email ou mot de passe invalide.", msg)
	})

	t.Run("fr-CA falls back through fr to en", func(t *testing.T) {
		msg := CodeInvalidCredentials.Error(Locale("fr-CA"), nil)
		assert.Equal(t, "Email ou mot de passe invalide.", msg)
	})

	t.Run("ja unsupported falls back to en", func(t *testing.T) {
		msg := CodeInvalidCredentials.Error(ja, nil)
		assert.Equal(t, "Invalid email or password.", msg)
	})

	t.Run("unknown code returns code string", func(t *testing.T) {
		msg := Code("UNKNOWN_CODE").Error(en, nil)
		assert.Equal(t, "UNKNOWN_CODE", msg)
	})

	t.Run("password too short with params interpolation", func(t *testing.T) {
		msg := CodePasswordTooShort.Error(en, nil)
		assert.Equal(t, "Password must be at least 8 characters.", msg)
	})

	t.Run("password too short fr", func(t *testing.T) {
		msg := CodePasswordTooShort.Error(fr, nil)
		assert.Equal(t, "Le mot de passe doit comporter au moins 8 caractères.", msg)
	})

	t.Run("password too short with custom params", func(t *testing.T) {
		msg := CodePasswordTooShort.Error(en, map[string]any{"min": 12})
		assert.Equal(t, "Password must be at least 12 characters.", msg)
	})
}

func TestLoad(t *testing.T) {
	t.Run("all codes have en translation", func(t *testing.T) {
		for code := range translations {
			msg := Load(code, LocaleEN)
			assert.NotEmpty(t, msg, "code %q has no en translation", code)
		}
	})

	t.Run("all codes have fr translation", func(t *testing.T) {
		for code := range translations {
			msg := Load(code, LocaleFR)
			assert.NotEmpty(t, msg, "code %q has no fr translation", code)
		}
	})
}

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name   string
		tmpl   string
		params map[string]any
		want   string
	}{
		{
			name:   "no params",
			tmpl:   "Hello world",
			params: nil,
			want:   "Hello world",
		},
		{
			name:   "single param",
			tmpl:   "Must be at least {min}",
			params: map[string]any{"min": 8},
			want:   "Must be at least 8",
		},
		{
			name:   "multiple params",
			tmpl:   "Field {field} must be between {min} and {max}",
			params: map[string]any{"field": "age", "min": 0, "max": 120},
			want:   "Field age must be between 0 and 120",
		},
		{
			name:   "missing param stays as placeholder",
			tmpl:   "Hello {name}",
			params: nil,
			want:   "Hello {name}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interpolate(tt.tmpl, tt.params)
			assert.Equal(t, tt.want, got)
		})
	}
}
