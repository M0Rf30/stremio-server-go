// Package api — shared TLS-certificate provisioning used by both the
// GET /get-https handler and the background auto-renewer (cmd/stremio-server).
//
// ProvisionCert contacts https://api.strem.io/api/certificateGet, parses the
// issued Let's Encrypt wildcard cert (current nested schema or the legacy flat
// one), installs https-cert.pem / https-key.pem under appPath, and returns the
// issued domain plus expiry. The authKey can be cached so renewals need no
// further user interaction.
package api

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certAuthKeyFile caches the last Stremio authKey used for provisioning so the
// background renewer can refresh the cert without another /get-https call.
const certAuthKeyFile = "cert-authkey"

// ProvisionResult describes a certificate issued and installed by ProvisionCert.
type ProvisionResult struct {
	Domain     string
	CommonName string
	NotAfter   time.Time
}

// provisionError carries an HTTP status so handleGetHTTPS preserves its original
// response codes while sharing the provisioning logic with the renewer.
type provisionError struct {
	status int
	msg    string
}

func (e *provisionError) Error() string { return e.msg }

func perr(status int, format string, a ...any) *provisionError {
	return &provisionError{status: status, msg: fmt.Sprintf(format, a...)}
}

// ProvisionCert requests a TLS certificate for ipAddress from api.strem.io using
// authKey, atomically installs https-cert.pem/https-key.pem under appPath, and
// returns the issued domain and expiry.
func ProvisionCert(appPath, authKey, ipAddress string) (*ProvisionResult, error) {
	if ipAddress == "" {
		return nil, perr(http.StatusBadRequest, "missing ipAddress")
	}
	if authKey == "" {
		return nil, perr(http.StatusBadRequest, "missing authKey")
	}

	const apiURL = "https://api.strem.io/api/certificateGet"
	payload, _ := json.Marshal(map[string]string{"authKey": authKey, "ipAddress": ipAddress})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, perr(http.StatusBadGateway, "API request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, perr(http.StatusBadGateway, "reading API response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, perr(http.StatusBadGateway, "API returned status %d: %s", resp.StatusCode, body)
	}

	// Outer envelope: {"result": <value>, "error": null}.
	var outer struct {
		Result json.RawMessage `json:"result"`
		Error  interface{}     `json:"error"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, perr(http.StatusBadGateway, "invalid API response: %v", err)
	}
	if len(outer.Result) == 0 || string(outer.Result) == "null" {
		if outer.Error != nil {
			return nil, perr(http.StatusNotFound, "API error: %v", outer.Error)
		}
		return nil, perr(http.StatusNotFound, "no certificate in API response")
	}

	certPEM, keyPEM, commonName, err := parseCertResult(outer.Result)
	if err != nil {
		return nil, err
	}
	if certPEM == "" || keyPEM == "" {
		return nil, perr(http.StatusNotFound, "certificate or privateKey missing in API response")
	}

	if err := installCertFiles(appPath, certPEM, keyPEM); err != nil {
		return nil, err
	}

	res := &ProvisionResult{CommonName: commonName, Domain: buildCertDomain(commonName, ipAddress)}
	if leaf := firstLeaf(certPEM); leaf != nil {
		res.NotAfter = leaf.NotAfter
	}
	return res, nil
}

// parseCertResult extracts cert PEM, key PEM, and common name from the
// certificateGet "result" value. Two known shapes:
//
//	new: result = {"certificate":"<json string>"} whose inner JSON is
//	     {"commonName":"*.<h>.stremio.rocks","contents":{"Pem":"<b64 PEM chain>",
//	      "PrivateKey":"<b64 PEM key>","Certificate":"<b64 PEM>"}}
//	old: result = {"certificate":"<pem>","privateKey":"<pem>","commonName":..}
//
// result itself may also be double-encoded (a JSON string of the object).
func parseCertResult(result json.RawMessage) (certPEM, keyPEM, commonName string, err error) {
	raw := result
	if len(raw) >= 1 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			raw = json.RawMessage(s)
		}
	}
	var env struct {
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"privateKey"`
		CommonName  string `json:"commonName"`
	}
	if e := json.Unmarshal(raw, &env); e != nil {
		return "", "", "", perr(http.StatusInternalServerError, "parse certificate result: %v", e)
	}

	if trimmed := strings.TrimSpace(env.Certificate); strings.HasPrefix(trimmed, "{") {
		var inner struct {
			CommonName string `json:"commonName"`
			Contents   struct {
				Pem         string `json:"Pem"`
				PrivateKey  string `json:"PrivateKey"`
				Certificate string `json:"Certificate"`
			} `json:"contents"`
		}
		if e := json.Unmarshal([]byte(trimmed), &inner); e != nil {
			return "", "", "", perr(http.StatusInternalServerError, "parse inner certificate JSON: %v", e)
		}
		commonName = inner.CommonName
		certPEM = decodePEMField(inner.Contents.Pem) // full chain (leaf + intermediates)
		if certPEM == "" {
			certPEM = decodePEMField(inner.Contents.Certificate)
		}
		keyPEM = decodePEMField(inner.Contents.PrivateKey)
	} else {
		commonName = env.CommonName
		certPEM = decodePEMField(env.Certificate)
		keyPEM = decodePEMField(env.PrivateKey)
	}
	return certPEM, keyPEM, commonName, nil
}

// installCertFiles writes cert and key atomically with safe rollback. Any
// existing cert/key files are renamed to .bak siblings before the new files
// are written; on any failure the backups are restored so the
// previously-installed cert/key remain intact. On success the backups are
// removed. Both files are written with mode 0600.
func installCertFiles(appPath, certPEM, keyPEM string) error {
	certPath := filepath.Join(appPath, "https-cert.pem")
	keyPath := filepath.Join(appPath, "https-key.pem")
	certBak := certPath + ".bak"
	keyBak := keyPath + ".bak"

	// Track which backups exist so restore knows what to put back.
	certBacked := false
	keyBacked := false

	// restore moves backup files back to their original paths; used on any
	// failure after backups have been created.
	restore := func() {
		if certBacked {
			_ = os.Rename(certBak, certPath)
		}
		if keyBacked {
			_ = os.Rename(keyBak, keyPath)
		}
	}

	// Back up existing cert (if any) before touching certPath.
	if _, err := os.Lstat(certPath); err == nil {
		if err := os.Rename(certPath, certBak); err != nil {
			return perr(http.StatusInternalServerError, "backup https-cert.pem: %v", err)
		}
		certBacked = true
	}

	// Back up existing key (if any) before touching keyPath.
	if _, err := os.Lstat(keyPath); err == nil {
		if err := os.Rename(keyPath, keyBak); err != nil {
			restore()
			return perr(http.StatusInternalServerError, "backup https-key.pem: %v", err)
		}
		keyBacked = true
	}

	// Write new cert to a temp file (mode 0600 via writeTempPEM).
	certTmp, err := writeTempPEM(appPath, "https-cert-*.pem", certPEM)
	if err != nil {
		restore()
		return perr(http.StatusInternalServerError, "write https-cert.pem: %v", err)
	}

	// Write new key to a temp file (mode 0600 via writeTempPEM).
	keyTmp, err := writeTempPEM(appPath, "https-key-*.pem", keyPEM)
	if err != nil {
		_ = os.Remove(certTmp)
		restore()
		return perr(http.StatusInternalServerError, "write https-key.pem: %v", err)
	}

	// Rename cert temp into place.
	if err := os.Rename(certTmp, certPath); err != nil {
		_ = os.Remove(certTmp)
		_ = os.Remove(keyTmp)
		restore()
		return perr(http.StatusInternalServerError, "install https-cert.pem: %v", err)
	}

	// Rename key temp into place. On failure, remove the newly-installed cert
	// so restore() can put the original cert back in its place cleanly.
	if err := os.Rename(keyTmp, keyPath); err != nil {
		_ = os.Remove(certPath)
		_ = os.Remove(keyTmp)
		restore()
		return perr(http.StatusInternalServerError, "install https-key.pem: %v", err)
	}

	// Success: remove backup siblings.
	_ = os.Remove(certBak)
	_ = os.Remove(keyBak)
	return nil
}

// buildCertDomain substitutes the wildcard label with the dashed IP, e.g.
// *.519b…stremio.rocks + 192.168.0.62 → 192-168-0-62.519….stremio.rocks.
func buildCertDomain(commonName, ipAddress string) string {
	ipDashes := strings.ReplaceAll(ipAddress, ".", "-")
	domain := strings.Replace(commonName, "*", ipDashes, 1)
	if domain == commonName { // no wildcard in CN — prefix the dashed IP instead
		domain = ipDashes + "." + commonName
	}
	return domain
}

// firstLeaf returns the first CERTIFICATE block parsed from a PEM bundle, or nil.
func firstLeaf(certPEM string) *x509.Certificate {
	rest := []byte(certPEM)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil
		}
		if blk.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
				return c
			}
		}
	}
}

// CacheAuthKey persists authKey (mode 0600) under appPath for later renewals.
func CacheAuthKey(appPath, authKey string) {
	if authKey == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(appPath, certAuthKeyFile), []byte(authKey), 0o600)
}

// CachedAuthKey returns the last cached authKey, or "" if none.
func CachedAuthKey(appPath string) string {
	b, err := os.ReadFile(filepath.Join(appPath, certAuthKeyFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// PrimaryIPv4 returns the first non-loopback IPv4 address, matching the address
// reported by /settings baseUrl. Returns "" if none is found.
func PrimaryIPv4() string {
	for _, a := range availableInterfaces() {
		if net.ParseIP(a) != nil && strings.Count(a, ":") == 0 {
			return a
		}
	}
	return ""
}

// writeTempPEM writes content to a new temp file (mode 0600) inside dir and
// returns the temp file's path. The caller must rename or remove the file.
// os.CreateTemp already creates files with mode 0600 on Unix.
func writeTempPEM(dir, pattern, content string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return "", writeErr
		}
		return "", closeErr
	}
	return name, nil
}

// decodePEMField returns PEM text from an API field that is either raw PEM
// (already starts with "-----BEGIN") or base64-encoded PEM. Empty input → "".
func decodePEMField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-----BEGIN") {
		return s
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}
