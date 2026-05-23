package auth_test

import (
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

func TestValidatePassword_StrongPassword(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	if err := auth.ValidatePassword(p, "MySecurePass1!"); err != nil {
		t.Errorf("strong password should pass: %v", err)
	}
}

func TestValidatePassword_TooShort(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy() // min 12
	err := auth.ValidatePassword(p, "Ab1!")
	if !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("expected ErrPasswordTooShort, got %v", err)
	}
}

func TestValidatePassword_ExactlyMinLength(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{MinLength: 12, RequireUpperLower: true, RequireDigit: true}
	err := auth.ValidatePassword(p, "Abcdefghij1K") // 12 chars, upper+lower+digit
	if err != nil {
		t.Errorf("exactly min length should pass: %v", err)
	}
}

func TestValidatePassword_NoUppercase(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "alllowercase1")
	if !errors.Is(err, auth.ErrPasswordNoComplexity) {
		t.Errorf("expected ErrPasswordNoComplexity, got %v", err)
	}
}

func TestValidatePassword_NoLowercase(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "ALLUPPERCASE1")
	if !errors.Is(err, auth.ErrPasswordNoComplexity) {
		t.Errorf("expected ErrPasswordNoComplexity, got %v", err)
	}
}

func TestValidatePassword_NoDigit(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "MixedCaseOnly")
	if !errors.Is(err, auth.ErrPasswordNoComplexity) {
		t.Errorf("expected ErrPasswordNoComplexity, got %v", err)
	}
}

func TestValidatePassword_CommonPassword(t *testing.T) {
	t.Parallel()
	// Disable complexity so we can test common check independently.
	p := auth.PasswordPolicy{
		MinLength:         4,
		RequireUpperLower: false,
		RequireDigit:      false,
		CheckCommon:       true,
	}
	common := []string{
		"password",
		"password1234",
		"administrator",
		"thunderbird1",
		"qwertyuiop",
	}
	for _, pw := range common {
		err := auth.ValidatePassword(p, pw)
		if !errors.Is(err, auth.ErrPasswordTooCommon) {
			t.Errorf("password %q: expected ErrPasswordTooCommon, got %v", pw, err)
		}
	}
}

func TestValidatePassword_DisabledComplexity(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{
		MinLength:         8,
		RequireUpperLower: false,
		RequireDigit:      false,
		CheckCommon:       false,
	}
	err := auth.ValidatePassword(p, "alllowercase")
	if err != nil {
		t.Errorf("complexity disabled, should pass: %v", err)
	}
}

func TestValidatePassword_DisabledCommonCheck(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{
		MinLength:         8,
		RequireUpperLower: true,
		RequireDigit:      true,
		CheckCommon:       false,
	}
	err := auth.ValidatePassword(p, "Password1")
	if err != nil {
		t.Errorf("common check disabled, should pass: %v", err)
	}
}

func TestValidatePassword_CustomMinLength(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{
		MinLength:         4,
		RequireUpperLower: false,
		RequireDigit:      false,
		CheckCommon:       false,
	}
	err := auth.ValidatePassword(p, "Ab1!")
	if err != nil {
		t.Errorf("min length 4, should pass: %v", err)
	}
}

func TestValidatePassword_OnlyRequireDigit(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{
		MinLength:         8,
		RequireUpperLower: false,
		RequireDigit:      true,
		CheckCommon:       false,
	}
	err := auth.ValidatePassword(p, "no_upper_case_1")
	if err != nil {
		t.Errorf("only digit required, should pass: %v", err)
	}
}

func TestValidatePassword_EmptyPassword(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "")
	if !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("empty password: expected ErrPasswordTooShort, got %v", err)
	}
}

func TestDefaultPasswordPolicy(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	if p.MinLength != 12 {
		t.Errorf("MinLength = %d, want 12", p.MinLength)
	}
	if !p.RequireUpperLower {
		t.Error("RequireUpperLower should be true")
	}
	if !p.RequireDigit {
		t.Error("RequireDigit should be true")
	}
	if !p.CheckCommon {
		t.Error("CheckCommon should be true")
	}
}

func TestValidatePassword_UnicodeMixedCase(t *testing.T) {
	t.Parallel()
	p := auth.PasswordPolicy{
		MinLength:         8,
		RequireUpperLower: true,
		RequireDigit:      true,
		CheckCommon:       false,
	}
	// Unicode letters with upper/lower case
	err := auth.ValidatePassword(p, "ÄrgerMüde1")
	if err != nil {
		t.Errorf("unicode mixed case should pass: %v", err)
	}
}

func TestValidatePassword_OnlyDigits(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "123456789012")
	if !errors.Is(err, auth.ErrPasswordNoComplexity) {
		t.Errorf("only digits: expected ErrPasswordNoComplexity, got %v", err)
	}
}

func TestValidatePassword_SpecialCharsWithComplexity(t *testing.T) {
	t.Parallel()
	p := auth.DefaultPasswordPolicy()
	err := auth.ValidatePassword(p, "MyStr0ng!Pass#")
	if err != nil {
		t.Errorf("strong password with special chars should pass: %v", err)
	}
}
