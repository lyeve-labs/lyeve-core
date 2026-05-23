package runtime

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// Setup mode carries the setup token, so a deployment that set a listener
// pair must not serve it in clear.
func TestSetupMode_ServesTheListenerPair(t *testing.T) {
	certFile, keyFile := writePair(t, t.TempDir(), "setup", time.Now())
	cfg, err := listenerTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilDone(ctx, "setup mode", []*http.Server{newBasicServer(addr, ok, cfg)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() { cancel(); <-done })

	tlsClient := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // the test checks the scheme, not the chain
	}}
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = tlsClient.Get("https://" + addr + "/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("TLS request to setup mode: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS status = %d", resp.StatusCode)
	}

	plain, err := http.Get("http://" + addr + "/") //nolint:noctx // a one-shot probe in a test
	if err == nil {
		_ = plain.Body.Close()
		if plain.StatusCode == http.StatusOK {
			t.Fatal("setup mode answered plain HTTP with a pair configured")
		}
	}
}
