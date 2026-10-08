package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// certCheckInterval bounds how often the cert files are stat'ed. Renewals
// happen every ~60 days, so this only needs to be frequent enough that a
// renewed cert is picked up well before the old one expires.
const certCheckInterval = time.Minute

// reloadingCert serves a TLS certificate from disk and picks up renewals
// (e.g. from an ACME client's cron/timer) without restarting the relay —
// a restart would drop every active tunnel.
type reloadingCert struct {
	certFile, keyFile string

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

func newReloadingCert(certFile, keyFile string) (*reloadingCert, error) {
	rc := &reloadingCert{certFile: certFile, keyFile: keyFile}
	if err := rc.reload(); err != nil {
		return nil, err
	}
	return rc, nil
}

func (rc *reloadingCert) reload() error {
	cst, err := os.Stat(rc.certFile)
	if err != nil {
		return err
	}
	kst, err := os.Stat(rc.keyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(rc.certFile, rc.keyFile)
	if err != nil {
		return fmt.Errorf("load %s / %s: %w", rc.certFile, rc.keyFile, err)
	}
	rc.cert, rc.certMod, rc.keyMod = &cert, cst.ModTime(), kst.ModTime()
	return nil
}

// GetCertificate is used as tls.Config.GetCertificate. If a reload fails
// (e.g. the ACME client is mid-write), the previous certificate keeps being
// served and the reload is retried on the next check.
func (rc *reloadingCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if time.Since(rc.lastCheck) >= certCheckInterval {
		rc.lastCheck = time.Now()
		cst, cerr := os.Stat(rc.certFile)
		kst, kerr := os.Stat(rc.keyFile)
		if cerr == nil && kerr == nil && (!cst.ModTime().Equal(rc.certMod) || !kst.ModTime().Equal(rc.keyMod)) {
			if err := rc.reload(); err != nil {
				log.Printf("direct listener: cert reload failed, keeping previous cert: %v", err)
			} else {
				log.Printf("direct listener: reloaded TLS certificate")
			}
		}
	}
	return rc.cert, nil
}
