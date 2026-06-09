package uuidx

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse(t *testing.T) {
	id, err := Parse("550e8400-e29b-41d4-a716-446655440000")
	assert.NoError(t, err)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", id.String())

	_, err = Parse("not-a-uuid")
	assert.Error(t, err)
}

func TestParam(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/items/550e8400-e29b-41d4-a716-446655440000", nil)
	req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")

	id, err := Param(req, "id")
	assert.NoError(t, err)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", id.String())

	// Missing param
	req2, _ := http.NewRequest(http.MethodGet, "/items/", nil)
	_, err = Param(req2, "id")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing")

	// Bad UUID
	req3, _ := http.NewRequest(http.MethodGet, "/items/bad", nil)
	req3.SetPathValue("id", "bad")
	_, err = Param(req3, "bad")
	assert.Error(t, err)
}
