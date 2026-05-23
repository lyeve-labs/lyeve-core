package runtime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, name string, mod time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{certFile, keyFile} {
		if err := os.Chtimes(f, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return certFile, keyFile
}

func servedName(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestCertReloader_ServesARotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Add(-time.Hour)
	certFile, keyFile := writePair(t, dir, "first", start)
	r, err := newCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	clock := start
	r.now = func() time.Time { return clock }
	if got := servedName(t, r); got != "first" {
		t.Fatalf("served %q, want first", got)
	}

	writePair(t, dir, "second", start.Add(time.Minute))
	clock = clock.Add(certReloadInterval / 2)
	if got := servedName(t, r); got != "first" {
		t.Fatalf("re-read before the interval: served %q", got)
	}
	clock = clock.Add(certReloadInterval)
	if got := servedName(t, r); got != "second" {
		t.Fatalf("after rotation served %q, want second", got)
	}

	// A pair that stops loading keeps the last good one in service.
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := start.Add(2 * time.Minute)
	if err := os.Chtimes(keyFile, later, later); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(certReloadInterval)
	if got := servedName(t, r); got != "second" {
		t.Fatalf("after a broken rotation served %q, want second", got)
	}
}

func TestListenerTLSConfig(t *testing.T) {
	cfg, err := listenerTLSConfig("", "")
	if err != nil || cfg != nil {
		t.Fatalf("no files: cfg=%v err=%v, want nil nil", cfg, err)
	}
	if _, err := listenerTLSConfig(filepath.Join(t.TempDir(), "missing.crt"), "missing.key"); err == nil {
		t.Fatal("a missing certificate must refuse the boot")
	}

	certFile, keyFile := writePair(t, t.TempDir(), "engine", time.Now())
	cfg, err = listenerTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	for _, v := range []struct {
		max  uint16
		want bool
	}{{tls.VersionTLS11, false}, {tls.VersionTLS13, true}} {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: v.max}) //nolint:gosec // the test inspects the server's version floor, not the chain. A client floor of 1.0 leaves the refusal to the server
		if (err == nil) != v.want {
			t.Fatalf("max version %x: handshake err=%v, want success=%v", v.max, err, v.want)
		}
		if conn != nil {
			_ = conn.Close()
		}
		if v.want {
			break
		}
		go func() {
			c, err := ln.Accept()
			if err == nil {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}
		}()
	}
}
