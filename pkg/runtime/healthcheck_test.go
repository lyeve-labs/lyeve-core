package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A TLS listener answers a plain request with 400, so the probe has to speak
// TLS when the listener does, or the container never turns healthy.
func TestHealthcheckTarget_FollowsTheListener(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewTLSServer(ok)
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	url, client := healthcheckTarget(port, true)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("TLS probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS probe status = %d", resp.StatusCode)
	}

	url, client = healthcheckTarget(port, false)
	if resp, err := client.Get(url); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a plain probe against a TLS listener must not read as healthy")
		}
	}
}
