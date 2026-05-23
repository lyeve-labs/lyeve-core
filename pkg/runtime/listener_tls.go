package runtime

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// certReloadInterval bounds how often a handshake re-reads the certificate
// files' modification times.
const certReloadInterval = 30 * time.Second

// certReloader serves a PEM certificate pair and re-reads it when either file
// changes, so a certificate rotated on disk (cert-manager, a renewal job) is
// served without restarting the engine. A pair that fails to load after a
// change keeps the previous one in service and is logged, at most once per
// check interval, until a pair that loads replaces it.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modCert time.Time
	modKey  time.Time
	checked time.Time
	now     func() time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, now: time.Now}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load() error {
	ci, err := os.Stat(r.certFile)
	if err != nil {
		return fmt.Errorf("TLS_CERT_FILE: %w", err)
	}
	ki, err := os.Stat(r.keyFile)
	if err != nil {
		return fmt.Errorf("TLS_KEY_FILE: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate pair: %w", err)
	}
	r.cert, r.modCert, r.modKey = &pair, ci.ModTime(), ki.ModTime()
	return nil
}

// getCertificate is the tls.Config hook. It checks the files at most once per
// certReloadInterval.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= certReloadInterval {
		r.checked = now
		ci, cerr := os.Stat(r.certFile)
		ki, kerr := os.Stat(r.keyFile)
		if cerr == nil && kerr == nil && (!ci.ModTime().Equal(r.modCert) || !ki.ModTime().Equal(r.modKey)) {
			// load replaces the pair only when the new one parses, so a
			// failure leaves the previous one serving.
			if err := r.load(); err != nil {
				slog.Warn("TLS certificate changed on disk but does not load; serving the previous one", "err", err)
			}
		}
	}
	return r.cert, nil
}

// listenerTLSConfig returns the TLS configuration both listeners share, or nil
// when no certificate is configured.
func listenerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" {
		return nil, nil
	}
	r, err := newCertReloader(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.getCertificate,
	}, nil
}

// serve runs srv over TLS when it carries a TLS configuration, which then
// supplies the certificate, and over plain HTTP otherwise.
func serve(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}
