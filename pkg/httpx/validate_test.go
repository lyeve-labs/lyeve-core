package httpx_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testReq struct {
	Name   string `json:"name"   validate:"required,max=64"`
	Email  string `json:"email"  validate:"required,email,max=255"`
	Status string `json:"status" validate:"required,oneof=active inactive"`
}

func TestValidate_OK(t *testing.T) {
	err := httpx.Validate(testReq{Name: "alice", Email: "a@b.com", Status: "active"})
	assert.NoError(t, err)
}

func TestValidate_RequiredField(t *testing.T) {
	err := httpx.Validate(testReq{Email: "a@b.com", Status: "active"})
	require.Error(t, err)

	var verr *httpx.ValidationError
	require.True(t, errors.As(err, &verr))
	require.Len(t, verr.Fields, 1)
	assert.Equal(t, "name", verr.Fields[0].Field)
	assert.Equal(t, "required", verr.Fields[0].Tag)
}

func TestValidate_MaxLength(t *testing.T) {
	longName := make([]byte, 65)
	for i := range longName {
		longName[i] = 'x'
	}
	err := httpx.Validate(testReq{Name: string(longName), Email: "a@b.com", Status: "active"})
	require.Error(t, err)

	var verr *httpx.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.Equal(t, "name", verr.Fields[0].Field)
	assert.Equal(t, "max", verr.Fields[0].Tag)
}

func TestValidate_InvalidEmail(t *testing.T) {
	err := httpx.Validate(testReq{Name: "alice", Email: "not-an-email", Status: "active"})
	require.Error(t, err)

	var verr *httpx.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.Equal(t, "email", verr.Fields[0].Field)
	assert.Equal(t, "email", verr.Fields[0].Tag)
}

func TestValidate_InvalidEnum(t *testing.T) {
	err := httpx.Validate(testReq{Name: "alice", Email: "a@b.com", Status: "bogus"})
	require.Error(t, err)

	var verr *httpx.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.Equal(t, "status", verr.Fields[0].Field)
	assert.Equal(t, "oneof", verr.Fields[0].Tag)
}

func TestValidate_MultipleErrors(t *testing.T) {
	err := httpx.Validate(testReq{})
	require.Error(t, err)

	var verr *httpx.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.GreaterOrEqual(t, len(verr.Fields), 2) // name + email required
}

func TestValidate_NilInput(t *testing.T) {
	// Non-struct should return a non-validation error.
	err := httpx.Validate("not a struct")
	assert.Error(t, err)
}

// A size constraint means three different things to the validator depending on
// the field's kind, and one of them is a crash. These pin the behavior the
// lint (tools/lint-validate-tags.sh) exists to keep out of the tree.
func TestValidate_SizeConstraintByFieldKind(t *testing.T) {
	t.Run("on a bool it panics as soon as the field is present", func(t *testing.T) {
		type body struct {
			Enabled *bool `json:"enabled,omitempty" validate:"omitempty,max=255"`
		}

		// omitempty does not save this. For a pointer it means "not nil", so
		// the rule runs on any request that mentions the field at all: true
		// or false. Only leaving the key out entirely avoids the crash, which
		// is why a field written this way passes every test that ignores it.
		if err := httpx.Validate(&body{}); err != nil {
			t.Fatalf("expected an absent field to be skipped, got %v", err)
		}

		for _, set := range []bool{false, true} {
			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatalf("expected max on a bool to panic with the field set to %v", set)
					}
					if !strings.Contains(fmt.Sprint(r), "bool") {
						t.Errorf("expected the panic to name the field type, got %v", r)
					}
				}()
				_ = httpx.Validate(&body{Enabled: &set})
			}()
		}
	})

	t.Run("on a number it caps the value, not a length", func(t *testing.T) {
		type body struct {
			StatusCode *int `json:"status_code,omitempty" validate:"omitempty,max=255"`
		}

		ok, notFound := 200, 404

		if err := httpx.Validate(&body{StatusCode: &ok}); err != nil {
			t.Fatalf("expected 200 to pass a max of 255, got %v", err)
		}
		if err := httpx.Validate(&body{StatusCode: &notFound}); err == nil {
			t.Fatal("expected 404 to fail a max of 255, which is the whole problem")
		}
	})

	t.Run("a non-nil pointer to zero is not skipped by omitempty", func(t *testing.T) {
		// This is why a status code that never arrived cannot carry min=100:
		// the pointer is set, so the rule runs on the zero value.
		type body struct {
			StatusCode *int `json:"status_code,omitempty" validate:"omitempty,min=100,max=599"`
		}

		none := 0
		if err := httpx.Validate(&body{StatusCode: &none}); err == nil {
			t.Fatal("expected a set-but-zero pointer to reach the rule")
		}
	})
}
