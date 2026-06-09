// Package i18n provides localized error codes and messages. Handlers emit
// stable machine-readable codes that resolve to per-locale human-readable
// strings for API responses.
package i18n

import (
	"fmt"
	"sort"
)

// Code is a stable machine-readable identifier for a translatable error.
// Codes follow the pattern COMPONENT_REASON (e.g. AUTH_INVALID_CREDENTIALS).
// The code itself never changes. Only the per-locale message templates change.
type Code string

// Error codes: the canonical registry of every translatable user-facing error.
// Add new codes here and provide translation entries in translations.go.
const (
	// Generic
	CodeInternalError    Code = "INTERNAL_ERROR"
	CodeInvalidBody      Code = "INVALID_BODY"
	CodeNotFound         Code = "NOT_FOUND"
	CodeForbidden        Code = "FORBIDDEN"
	CodeUnauthorized     Code = "UNAUTHORIZED"
	CodeRateLimited      Code = "RATE_LIMITED"
	CodeValidationFailed Code = "VALIDATION_FAILED"

	// Auth
	CodeInvalidCredentials      Code = "AUTH_INVALID_CREDENTIALS"
	CodeSetupAlreadyComplete    Code = "AUTH_SETUP_ALREADY_COMPLETE"
	CodeSetupTokenInvalid       Code = "AUTH_SETUP_TOKEN_INVALID"
	CodeSetupModeRequired       Code = "SETUP_REQUIRED"
	CodePasswordTooShort        Code = "AUTH_PASSWORD_TOO_SHORT"
	CodePasswordHashFailed      Code = "AUTH_PASSWORD_HASH_FAILED"
	CodePasswordNeedsComplexity Code = "AUTH_PASSWORD_NEEDS_COMPLEXITY"
	CodePasswordTooCommon       Code = "AUTH_PASSWORD_TOO_COMMON"
	CodeUserCreateFailed        Code = "AUTH_USER_CREATE_FAILED"
	CodeTokenSignFailed         Code = "AUTH_TOKEN_SIGN_FAILED"
	CodeChallengeFailed         Code = "AUTH_CHALLENGE_FAILED"
	CodeChallengeExpired        Code = "AUTH_CHALLENGE_EXPIRED"
	CodeMFANotConfigured        Code = "AUTH_MFA_NOT_CONFIGURED"
	CodeMFAInvalidCode          Code = "AUTH_MFA_INVALID_CODE"
	CodeTokenReuse              Code = "AUTH_TOKEN_REUSE"
	CodeTokenExpired            Code = "AUTH_TOKEN_EXPIRED"
	CodeSessionRevoked          Code = "AUTH_SESSION_REVOKED"
	CodeRefreshNotEnabled       Code = "AUTH_REFRESH_NOT_ENABLED"
	CodeMFANotAvailable         Code = "AUTH_MFA_NOT_AVAILABLE"
	CodeAccountLocked           Code = "AUTH_ACCOUNT_LOCKED"
	CodeAccountDisabled         Code = "AUTH_ACCOUNT_DISABLED"
	CodeAccountExpired          Code = "AUTH_ACCOUNT_EXPIRED"
	CodeMFAStoreError           Code = "AUTH_MFA_STORE_ERROR"
	CodeMFALockedOut            Code = "AUTH_MFA_LOCKED_OUT"

	// Users
	CodeUserNotFound  Code = "USER_NOT_FOUND"
	CodeInvalidUserID Code = "USER_INVALID_ID"
	CodeRolesRequired Code = "USER_ROLES_REQUIRED"

	// Tenant features
	CodeTenantFeatureWithheld Code = "TENANT_FEATURE_WITHHELD"
	CodeTenantFeatureUnknown  Code = "TENANT_FEATURE_UNKNOWN"
	CodeTenantSlugInvalid     Code = "TENANT_SLUG_INVALID"

	// Schema
	CodeSchemaNotFound  Code = "SCHEMA_NOT_FOUND"
	CodeMigrationFailed Code = "SCHEMA_MIGRATION_FAILED"

	// Content
	CodeContentNotFound Code = "CONTENT_NOT_FOUND"

	// DB
	CodeDatabaseError Code = "DATABASE_ERROR"

	// Availability
	// CodeSessionStoreUnavailable tells a client the session store could not
	// be reached, so its token was neither accepted nor rejected. Distinct
	// from the unauthorized codes on purpose: a client must retry rather than
	// discard the session.
	CodeSessionStoreUnavailable Code = "SESSION_STORE_UNAVAILABLE"

	// Validation
	CodeValidationRequired       Code = "VALIDATION_REQUIRED"
	CodeValidationMinLength      Code = "VALIDATION_MIN_LENGTH"
	CodeValidationMaxLength      Code = "VALIDATION_MAX_LENGTH"
	CodeValidationMinValue       Code = "VALIDATION_MIN_VALUE"
	CodeValidationMaxValue       Code = "VALIDATION_MAX_VALUE"
	CodeValidationEmail          Code = "VALIDATION_EMAIL"
	CodeValidationUUID           Code = "VALIDATION_UUID"
	CodeValidationURL            Code = "VALIDATION_URL"
	CodeValidationTenantSlug     Code = "VALIDATION_TENANT_SLUG"
	CodeValidationStrongPassword Code = "VALIDATION_STRONG_PASSWORD"
	CodeValidationUnknownField   Code = "VALIDATION_UNKNOWN_FIELD"
	CodeValidationFieldType      Code = "VALIDATION_FIELD_TYPE"

	// Schema Validation
	CodeSchemaValidationEmail        Code = "SCHEMA_VALIDATION_EMAIL"
	CodeSchemaValidationURL          Code = "SCHEMA_VALIDATION_URL"
	CodeSchemaValidationRegex        Code = "SCHEMA_VALIDATION_REGEX"
	CodeSchemaValidationEnum         Code = "SCHEMA_VALIDATION_ENUM"
	CodeSchemaValidationMin          Code = "SCHEMA_VALIDATION_MIN"
	CodeSchemaValidationMax          Code = "SCHEMA_VALIDATION_MAX"
	CodeSchemaValidationRequiredWith Code = "SCHEMA_VALIDATION_REQUIRED_WITH"
	CodeSchemaValidationLtField      Code = "SCHEMA_VALIDATION_LT_FIELD"
	CodeSchemaValidationGtField      Code = "SCHEMA_VALIDATION_GT_FIELD"
	CodeSchemaValidationLteField     Code = "SCHEMA_VALIDATION_LTE_FIELD"
	CodeSchemaValidationGteField     Code = "SCHEMA_VALIDATION_GTE_FIELD"
	CodeSchemaValidationRange        Code = "SCHEMA_VALIDATION_RANGE"
	CodeSchemaValidationCustom       Code = "SCHEMA_VALIDATION_CUSTOM"
	CodeSchemaValidationCrossField   Code = "SCHEMA_VALIDATION_CROSS_FIELD"
)

// defaultParams returns default template parameters for a code.
// Returns nil for codes that need no parameters.
func defaultParams(c Code) map[string]any {
	switch c {
	case CodePasswordTooShort:
		return map[string]any{"min": 8}
	default:
		return nil
	}
}

// Error returns the translated message for this code in the given locale,
// with template parameters interpolated. Falls back through the locale chain.
func (c Code) Error(loc Locale, params map[string]any) string {
	t := Load(c, loc)
	// Merge default params under caller-supplied params.
	merged := defaultParams(c)
	if merged == nil {
		merged = params
	} else {
		for k, v := range params {
			merged[k] = v
		}
	}
	if merged == nil {
		return t
	}
	return interpolate(t, merged)
}

func interpolate(tmpl string, params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := tmpl
	for _, k := range keys {
		placeholder := "{" + k + "}"
		replacement := fmt.Sprint(params[k])
		result = replaceAll(result, placeholder, replacement)
	}
	return result
}

func replaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	for {
		idx := indexOf(s, old)
		if idx < 0 {
			out = append(out, s...)
			break
		}
		out = append(out, s[:idx]...)
		out = append(out, new...)
		s = s[idx+len(old):]
	}
	return string(out)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
