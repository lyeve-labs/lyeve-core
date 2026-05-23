package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testCreateUserReq struct {
	Email    string `json:"email"    validate:"required,email,max=255"`
	Password string `json:"password" validate:"required,min=8,max=128,strongpassword"`
	Slug     string `json:"slug"     validate:"required,tenant_slug"`
	Age      int    `json:"age"      validate:"min=0,max=150"`
}

type testOptionalReq struct {
	Name  string `json:"name"  validate:"omitempty,min=3,max=100"`
	Email string `json:"email" validate:"omitempty,email"`
}

type testMinimalReq struct {
	ID string `json:"id" validate:"required,uuid"`
}

func TestBody_ValidRequest(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := GetBody[testCreateUserReq](r)
		assert.Equal(t, "user@example.com", req.Email)
		assert.Equal(t, "P@ssw0rd123!", req.Password)
		assert.Equal(t, "my-tenant", req.Slug)
		assert.Equal(t, 42, req.Age)
		w.WriteHeader(http.StatusCreated)
	}))

	body := `{"email":"user@example.com","password":"P@ssw0rd123!","slug":"my-tenant","age":42}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestBody_InvalidJSON(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad json`))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, string(i18n.CodeInvalidBody), resp["code"])
}

func TestBody_MissingRequired(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"","password":"","slug":""}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, string(i18n.CodeValidationFailed), resp["code"])
	assert.Contains(t, resp["error"], "failed validation")

	fields, ok := resp["fields"].([]any)
	require.True(t, ok, "fields should be an array")
	assert.NotEmpty(t, fields)
}

func TestBody_EmailValidation(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"not-an-email","password":"P@ssw0rd123!","slug":"my-tenant","age":25}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	fields, _ := resp["fields"].([]any)
	foundEmail := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["field"] == "email" {
			foundEmail = true
			assert.Equal(t, string(i18n.CodeValidationEmail), fm["code"])
			assert.Contains(t, fm["message"], "email")
		}
	}
	assert.True(t, foundEmail, "should have email field error")
}

func TestBody_MinMaxLength(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"user@example.com","password":"short","slug":"my-tenant","age":25}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	fields, _ := resp["fields"].([]any)
	foundPass := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["field"] == "password" {
			foundPass = true
			assert.Equal(t, string(i18n.CodeValidationMinLength), fm["code"])
		}
	}
	assert.True(t, foundPass, "should have password field error")
}

func TestBody_NumericMinMax(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"user@example.com","password":"P@ssw0rd123!","slug":"my-tenant","age":-5}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	fields, _ := resp["fields"].([]any)
	foundAge := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["field"] == "age" {
			foundAge = true
			code, _ := fm["code"].(string)
			assert.Contains(t, code, "VALIDATION_MIN")
		}
	}
	assert.True(t, foundAge, "should have age field error")
}

func TestBody_TenantSlugValidation(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"user@example.com","password":"P@ssw0rd123!","slug":"INVALID_SLUG","age":25}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	fields, _ := resp["fields"].([]any)
	foundSlug := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["field"] == "slug" {
			foundSlug = true
			assert.Equal(t, string(i18n.CodeValidationTenantSlug), fm["code"])
		}
	}
	assert.True(t, foundSlug, "should have slug field error")
}

func TestBody_MultipleFieldErrors(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	// Bad email + password too short + invalid slug + negative age
	body := `{"email":"bad","password":"x","slug":"","age":-1}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	fields, ok := resp["fields"].([]any)
	require.True(t, ok)
	// Should have multiple field errors
	assert.GreaterOrEqual(t, len(fields), 2, "should have at least 2 field errors")
}

func TestBody_I18NFrench(t *testing.T) {
	// Locale middleware has already set the locale.
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	body := `{"email":"","password":"","slug":""}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	// Inject French locale into context (simulates Locale middleware).
	ctx := i18n.WithLocale(req.Context(), i18n.LocaleFR)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	errMsg, _ := resp["error"].(string)
	assert.Contains(t, errMsg, "données") // "Les données fournies n'ont pas passé la validation."

	fields, _ := resp["fields"].([]any)
	found := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["code"] == string(i18n.CodeValidationRequired) {
			found = true
			msg, _ := fm["message"].(string)
			assert.Contains(t, msg, "obligatoire") // French for "required"
		}
	}
	assert.True(t, found, "should have French required error")
}

func TestBody_UUIDValidation(t *testing.T) {
	handler := Body[testMinimalReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	tests := []struct {
		name  string
		body  string
		valid bool
	}{
		{"valid UUID", `{"id":"550e8400-e29b-41d4-a716-446655440000"}`, true},
		{"invalid UUID", `{"id":"not-a-uuid"}`, false},
		{"empty UUID", `{"id":""}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req.Header.Set("Accept-Language", "en")
			rec := httptest.NewRecorder()

			if tt.valid {
				realHandler := Body[testMinimalReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				realHandler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusOK, rec.Code)
			} else {
				handler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusBadRequest, rec.Code)

				var resp map[string]any
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
				assert.Equal(t, string(i18n.CodeValidationFailed), resp["code"])
			}
		})
	}
}

func TestBody_Omitempty(t *testing.T) {
	handler := Body[testOptionalReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := GetBody[testOptionalReq](r)
		assert.Equal(t, "", req.Name)
		assert.Equal(t, "", req.Email)
		w.WriteHeader(http.StatusOK)
	}))

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetBody_NoMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	result := GetBody[testCreateUserReq](req)
	assert.Equal(t, testCreateUserReq{}, result)
}

func TestTenantSlugValidator(t *testing.T) {
	v := getValidator()
	require.NotNil(t, v)

	type slugReq struct {
		Slug string `validate:"tenant_slug"`
	}

	tests := []struct {
		name  string
		slug  string
		valid bool
	}{
		{"valid simple", "my-tenant", true},
		{"valid short", "a-b", true},
		{"valid numeric", "tenant-42", true},
		{"valid single segment", "mytenant", true},
		{"valid max length", strings.Repeat("a", 63), true},
		{"empty passes (required handles it)", "", true},
		{"too short 2 chars", "ab", false},
		{"too short 1 char", "a", false},
		{"too long 64 chars", strings.Repeat("a", 64), false},
		{"uppercase", "My-Tenant", false},
		{"leading hyphen", "-my-tenant", false},
		{"trailing hyphen", "my-tenant-", false},
		{"consecutive hyphens", "my--tenant", false},
		{"special chars", "my_tenant", false},
		{"starts with digit", "42-tenant", true},
		{"hyphen then digit", "my-42", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Struct(slugReq{Slug: tt.slug})
			if tt.valid {
				assert.NoError(t, err, "expected valid: %q", tt.slug)
			} else {
				assert.Error(t, err, "expected invalid: %q", tt.slug)
			}
		})
	}
}

func TestStrongPasswordValidator(t *testing.T) {
	v := getValidator()
	require.NotNil(t, v)

	type pwReq struct {
		Password string `validate:"strongpassword"`
	}

	tests := []struct {
		name  string
		pw    string
		valid bool
	}{
		{"all classes", "P@ssw0rd!", true},
		{"long complex", "MyStr0ng!Pass#2024", true},
		{"empty passes (required handles it)", "", true},
		{"no uppercase", "p@ssw0rd!", false},
		{"no lowercase", "P@SSW0RD!", false},
		{"no digit", "P@ssword!", false},
		{"no special char", "Passw0rd1", false},
		{"only lowercase letters", "password", false},
		{"only digits", "12345678", false},
		{"uppercase + digit, no special", "Passw0rd", false},
		{"all lowercase + special", "p@ss!word", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Struct(pwReq{Password: tt.pw})
			if tt.valid {
				assert.NoError(t, err, "expected valid: %q", tt.pw)
			} else {
				assert.Error(t, err, "expected invalid: %q", tt.pw)
			}
		})
	}
}

func TestBody_StrongPasswordIntegration(t *testing.T) {
	handler := Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	tests := []struct {
		name     string
		password string
		wantCode i18n.Code
	}{
		{"weak all lowercase", "aaaaaaaa", i18n.CodeValidationStrongPassword},
		{"missing uppercase", "p@ssw0rd!", i18n.CodeValidationStrongPassword},
		{"missing special", "Passw0rd1", i18n.CodeValidationStrongPassword},
		{"missing digit", "P@ssword!", i18n.CodeValidationStrongPassword},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"email":"user@example.com","password":"` + tt.password + `","slug":"my-tenant","age":25}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Accept-Language", "en")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)

			var resp map[string]any
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

			fields, _ := resp["fields"].([]any)
			foundPW := false
			for _, f := range fields {
				fm := f.(map[string]any)
				if fm["field"] == "password" {
					code, _ := fm["code"].(string)
					if code == string(tt.wantCode) {
						foundPW = true
					}
				}
			}
			assert.True(t, foundPW, "should have strongpassword error for password %q", tt.password)
		})
	}
}

func TestTagNameFunc_UsesJSONTag(t *testing.T) {
	type jsonTagReq struct {
		FirstName string `json:"first_name" validate:"required"`
	}

	v := getValidator()
	err := v.Struct(jsonTagReq{})
	require.Error(t, err)

	var verrs validator.ValidationErrors
	ok := errors.As(err, &verrs)
	require.True(t, ok)
	assert.Equal(t, "first_name", verrs[0].Field())
}

func TestTagNameFunc_NoJSONTag(t *testing.T) {
	type noJSONTagReq struct {
		FirstName string `validate:"required"`
	}

	v := getValidator()
	err := v.Struct(noJSONTagReq{})
	require.Error(t, err)

	var verrs validator.ValidationErrors
	ok := errors.As(err, &verrs)
	require.True(t, ok)
	assert.Equal(t, "FirstName", verrs[0].Field())
}

func TestTagNameFunc_JSONDash(t *testing.T) {
	type jsonDashReq struct {
		Password string `json:"-" validate:"required"`
	}

	v := getValidator()
	err := v.Struct(jsonDashReq{})
	require.Error(t, err)

	var verrs validator.ValidationErrors
	ok := errors.As(err, &verrs)
	require.True(t, ok)
	// json:"-" means hidden, so the validator falls back to struct field name.
	assert.Equal(t, "Password", verrs[0].Field())
}

func TestTagToCodeMapping(t *testing.T) {
	assert.Equal(t, i18n.CodeValidationRequired, tagToCode["required"])
	assert.Equal(t, i18n.CodeValidationMinLength, tagToCode["min"])
	assert.Equal(t, i18n.CodeValidationMaxLength, tagToCode["max"])
	assert.Equal(t, i18n.CodeValidationEmail, tagToCode["email"])
	assert.Equal(t, i18n.CodeValidationUUID, tagToCode["uuid"])
	assert.Equal(t, i18n.CodeValidationURL, tagToCode["url"])
	assert.Equal(t, i18n.CodeValidationTenantSlug, tagToCode["tenant_slug"])
	assert.Equal(t, i18n.CodeValidationStrongPassword, tagToCode["strongpassword"])

	_, ok := tagToCode["nonexistent_tag"]
	assert.False(t, ok)
}

func TestValidatorSingleton(t *testing.T) {
	v1 := getValidator()
	v2 := getValidator()
	assert.Same(t, v1, v2, "getValidator should return the same instance")
}

func TestLocaleMiddlewareWithValidator(t *testing.T) {
	// Locale middleware sets French locale. Validation uses it.
	handler := Locale(Body[testCreateUserReq]()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})))

	body := `{"email":"","password":"","slug":""}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Accept-Language", "fr")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	errMsg, _ := resp["error"].(string)
	assert.Contains(t, errMsg, "données")

	fields, _ := resp["fields"].([]any)
	foundFrench := false
	for _, f := range fields {
		fm := f.(map[string]any)
		msg, _ := fm["message"].(string)
		if strings.Contains(msg, "obligatoire") {
			foundFrench = true
		}
	}
	assert.True(t, foundFrench, "field error should be in French")
}
