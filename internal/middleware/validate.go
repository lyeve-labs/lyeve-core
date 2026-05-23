package middleware

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
)

type validatedBodyKey[T any] struct{}

// Body decodes the JSON request body into T, validates it with
// go-playground/validator struct tags, and either returns 400 with
// i18n-translated field errors or injects the validated T into context.
// T must carry validate:"..." and json:"..." tags. Field names in errors
// come from the json tag, falling back to the struct field name.
//
// Usage:
//
//	type CreateUserReq struct {
//	    Email    string `json:"email"    validate:"required,email,max=255"`
//	    Password string `json:"password" validate:"required,min=8,max=128,strongpassword"`
//	    Slug     string `json:"slug"     validate:"required,tenant_slug"`
//	}
//
//	r.With(middleware.Body[CreateUserReq]()).Post("/users", func(w, r) {
//	    req := middleware.GetBody[CreateUserReq](r)
//	})
func Body[T any]() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v T
			if err := jsonpool.DecodeJSON(r.Body, &v); err != nil {
				writeValidationError(w, r, i18n.CodeInvalidBody, nil)
				return
			}

			if err := getValidator().Struct(v); err != nil {
				var verrs validator.ValidationErrors
				if errors.As(err, &verrs) {
					fields := translateFieldErrors(r, verrs)
					writeValidationFields(w, r, fields)
					return
				}
				// Non-validation error (e.g. invalid type): treat as bad body.
				writeValidationError(w, r, i18n.CodeInvalidBody, nil)
				return
			}

			ctx := context.WithValue(r.Context(), validatedBodyKey[T]{}, v)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetBody retrieves the validated request body of type T from context.
// Returns the zero value when Body[T] was not applied.
func GetBody[T any](r *http.Request) T {
	if v, ok := r.Context().Value(validatedBodyKey[T]{}).(T); ok {
		return v
	}
	var zero T
	return zero
}

var (
	defaultValidator *validator.Validate
	validatorOnce    sync.Once
)

func getValidator() *validator.Validate {
	validatorOnce.Do(func() {
		v := validator.New()

		v.RegisterTagNameFunc(func(fld reflect.StructField) string {
			tag := fld.Tag.Get("json")
			if tag == "" || tag == "-" {
				return ""
			}
			if idx := strings.IndexByte(tag, ','); idx != -1 {
				return tag[:idx]
			}
			return tag
		})

		_ = v.RegisterValidation("tenant_slug", validateTenantSlug)        // err suppressed: init-time, name is unique
		_ = v.RegisterValidation("strongpassword", validateStrongPassword) // err suppressed: init-time, name is unique

		defaultValidator = v
	})
	return defaultValidator
}

// tenantSlugRE matches a valid tenant slug: lowercase alphanumeric+hyphens,
// no leading/trailing/consecutive hyphens, 3-63 characters.
var tenantSlugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])?$`)

func validateTenantSlug(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	if s == "" {
		// empty is caught by "required", so it is not reported twice
		return true
	}
	if !tenantSlugRE.MatchString(s) || strings.Contains(s, "--") {
		return false
	}
	return len(s) >= 3 && len(s) <= 63
}

// passwordUpper and related vars are character classes required for a strong password.
var (
	passwordUpper   = regexp.MustCompile(`[A-Z]`)
	passwordLower   = regexp.MustCompile(`[a-z]`)
	passwordDigit   = regexp.MustCompile(`[0-9]`)
	passwordSpecial = regexp.MustCompile(`[^A-Za-z0-9]`)
)

func validateStrongPassword(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	if s == "" {
		// empty is caught by "required", so it is not reported twice
		return true
	}
	return passwordUpper.MatchString(s) &&
		passwordLower.MatchString(s) &&
		passwordDigit.MatchString(s) &&
		passwordSpecial.MatchString(s)
}

var tagToCode = map[string]i18n.Code{
	"required":       i18n.CodeValidationRequired,
	"min":            i18n.CodeValidationMinLength,
	"max":            i18n.CodeValidationMaxLength,
	"email":          i18n.CodeValidationEmail,
	"uuid":           i18n.CodeValidationUUID,
	"url":            i18n.CodeValidationURL,
	"tenant_slug":    i18n.CodeValidationTenantSlug,
	"strongpassword": i18n.CodeValidationStrongPassword,
}

// ValidationField is a single field-level validation error with a translated
// message and stable error code.
type ValidationField struct {
	Field   string    `json:"field"`
	Message string    `json:"message"`
	Code    i18n.Code `json:"code"`
}

func translateFieldErrors(r *http.Request, verrs validator.ValidationErrors) []ValidationField {
	loc := i18n.LocaleFromCtx(r.Context())
	fields := make([]ValidationField, 0, len(verrs))

	for _, fe := range verrs {
		code := tagToCode[fe.Tag()]
		if code == "" {
			code = i18n.CodeValidationUnknownField
		}

		params := make(map[string]any)
		params["field"] = fe.Field()

		if fe.Param() != "" {
			params[fe.Tag()] = fe.Param()
			if fe.Tag() == "min" || fe.Tag() == "max" {
				params = map[string]any{"field": fe.Field(), fe.Tag(): fe.Param()}
			}
		}

		// For numeric fields, min/max refer to value not length. The
		// translation uses {min}/{max} generically for both.
		fields = append(fields, ValidationField{
			Field:   fe.Field(),
			Message: code.Error(loc, params),
			Code:    code,
		})
	}

	return fields
}

func writeValidationFields(w http.ResponseWriter, r *http.Request, fields []ValidationField) {
	loc := i18n.LocaleFromCtx(r.Context())
	summary := i18n.CodeValidationFailed.Error(loc, nil)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = jsonpool.WriteJSON(w, map[string]any{ // err suppressed: response write to client
		"error":  summary,
		"code":   i18n.CodeValidationFailed,
		"fields": fields,
	})
}

func writeValidationError(w http.ResponseWriter, r *http.Request, code i18n.Code, params map[string]any) {
	loc := i18n.LocaleFromCtx(r.Context())
	msg := code.Error(loc, params)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = jsonpool.WriteJSON(w, map[string]any{ // err suppressed: response write to client
		"error": msg,
		"code":  code,
	})
}
