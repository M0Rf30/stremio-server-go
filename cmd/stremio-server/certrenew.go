package main

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/api"
)

// renewBefore triggers renewal when the live cert expires within this window.
const renewBefore = 30 * 24 * time.Hour

// renewCheckInterval is how often the renewer re-evaluates the live cert.
const renewCheckInterval = 12 * time.Hour

// certHolder stores the active TLS certificate behind tls.Config.GetCertificate
// so the renewer can hot-swap it without restarting the HTTPS listener.
type certHolder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

func (h *certHolder) set(c tls.Certificate) {
	h.mu.Lock()
	h.cert = &c
	h.mu.Unlock()
}

func (h *certHolder) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert, nil
}

func (h *certHolder) leaf() *x509.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cert == nil || len(h.cert.Certificate) == 0 {
		return nil
	}
	c, err := x509.ParseCertificate(h.cert.Certificate[0])
	if err != nil {
		return nil
	}
	return c
}

// certNeedsRenewal reports whether the live cert should be (re)provisioned: when
// it is missing, self-signed (Issuer == Subject), or within renewBefore of expiry.
func certNeedsRenewal(h *certHolder) (bool, string) {
	leaf := h.leaf()
	if leaf == nil {
		return true, "no usable cert"
	}
	if leaf.Issuer.String() == leaf.Subject.String() {
		return true, "self-signed cert"
	}
	if time.Until(leaf.NotAfter) < renewBefore {
		return true, "cert expiring " + leaf.NotAfter.Format(time.RFC3339)
	}
	return false, ""
}

// renewCertLoop provisions a browser-trusted cert from api.strem.io and hot-swaps
// it into holder when needed, then re-checks every renewCheckInterval. It is a
// no-op while no authKey is available (env STREMIO_CERT_AUTHKEY, or one cached by
// a prior /get-https call), so it never disturbs an existing self-signed setup
// unless the user has opted into HTTPS provisioning.
func renewCertLoop(appPath string, holder *certHolder, stop <-chan struct{}) {
	check := func() {
		authKey := os.Getenv("STREMIO_CERT_AUTHKEY")
		if authKey == "" {
			authKey = api.CachedAuthKey(appPath)
		}
		if authKey == "" {
			return // nothing to provision with; leave the existing cert alone
		}
		ip := getenv("STREMIO_CERT_IP", api.PrimaryIPv4())
		if ip == "" {
			return
		}
		need, reason := certNeedsRenewal(holder)
		if !need {
			return
		}
		res, err := api.ProvisionCert(appPath, authKey, ip)
		if err != nil {
			log.Printf("https: auto-provision failed (%s): %v", reason, err)
			return
		}
		cert, err := tls.LoadX509KeyPair(
			filepath.Join(appPath, "https-cert.pem"),
			filepath.Join(appPath, "https-key.pem"),
		)
		if err != nil {
			log.Printf("https: reload after provision failed: %v", err)
			return
		}
		holder.set(cert)
		log.Printf("https: provisioned cert for %s (%s; valid until %s)",
			res.Domain, reason, res.NotAfter.Format(time.RFC3339))
	}

	check() // attempt once at startup
	t := time.NewTicker(renewCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			check()
		}
	}
}
