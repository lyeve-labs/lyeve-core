package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

func TestQueryOffset_RejectsNegativeAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"250", 250, false},
		{"-1", 0, true},
		{"-1000", 0, true},
		{"abc", 0, true},
	} {
		req := httptest.NewRequest(http.MethodGet, "/x?offset="+tc.raw, nil)
		got, err := reqparse.QueryOffset(req)
		if tc.wantErr {
			if err == nil {
				t.Errorf("offset=%q: want an error, got %d", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("offset=%q: unexpected error %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("offset=%q = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
