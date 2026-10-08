package main

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

func writeSelfSigned(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
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
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func leafCN(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestReloadingCertPicksUpRenewal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeSelfSigned(t, certPath, keyPath, "first.example")

	rc, err := newReloadingCert(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := rc.GetCertificate(nil)
	if got := leafCN(t, c); got != "first.example" {
		t.Fatalf("initial cert CN = %q", got)
	}

	// Simulate a renewal: new files with a later mtime, and the check interval elapsed.
	writeSelfSigned(t, certPath, keyPath, "second.example")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(certPath, later, later)
	os.Chtimes(keyPath, later, later)
	rc.mu.Lock()
	rc.lastCheck = time.Time{}
	rc.mu.Unlock()

	c, _ = rc.GetCertificate(nil)
	if got := leafCN(t, c); got != "second.example" {
		t.Fatalf("after renewal CN = %q, want second.example", got)
	}
}

func TestReloadingCertKeepsOldCertOnBadRenewal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeSelfSigned(t, certPath, keyPath, "good.example")
	rc, err := newReloadingCert(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// A half-written renewal must not take the listener down.
	os.WriteFile(certPath, []byte("garbage"), 0o600)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(certPath, later, later)
	rc.mu.Lock()
	rc.lastCheck = time.Time{}
	rc.mu.Unlock()

	c, err := rc.GetCertificate(nil)
	if err != nil || c == nil {
		t.Fatalf("expected previous cert to keep serving, got %v / %v", c, err)
	}
	if got := leafCN(t, c); got != "good.example" {
		t.Fatalf("CN = %q, want good.example", got)
	}
}

func TestValidateDirectListener(t *testing.T) {
	cases := []struct {
		name            string
		addr, cert, key string
		wantErr         bool
	}{
		{"disabled", "", "", "", false},
		{"enabled", ":26499", "c.pem", "k.pem", false},
		{"missing cert", ":26499", "", "k.pem", true},
		{"cert without listener", "", "c.pem", "k.pem", true},
		{"overlaps tunnel range", ":26100", "c.pem", "k.pem", true},
		{"bad address", "26499", "c.pem", "k.pem", true},
		{"port out of range", ":70000", "c.pem", "k.pem", true},
	}
	for _, tc := range cases {
		err := validateDirectListener(tc.addr, tc.cert, tc.key, 26000, 26498)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}
