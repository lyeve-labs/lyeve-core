package runtime

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Healthcheck probes the local /healthz on PORT, 3002 when unset, and exits
// the process: 0 when it answers 200, 1 otherwise. It is the container
// HEALTHCHECK command, which runs the binary a second time, so a main calls it
// before the engine boots.
func Healthcheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3002"
	}
	url, client := healthcheckTarget(port, os.Getenv("TLS_CERT_FILE") != "")
	resp, err := client.Get(url)
	if err != nil {
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

// healthcheckTarget is the URL and client the healthcheck probes with. With
// TLS on the listener answers a plain request with 400, which would keep the
// container unhealthy forever. The probe goes to loopback, where the
// certificate's names do not apply, so it checks liveness, not the chain.
func healthcheckTarget(port string, useTLS bool) (string, *http.Client) {
	if !useTLS {
		return fmt.Sprintf("http://127.0.0.1:%s/healthz", port), http.DefaultClient
	}
	return fmt.Sprintf("https://127.0.0.1:%s/healthz", port), &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // loopback liveness probe. The listener's names do not cover 127.0.0.1
		},
	}
}
