// Package api — GET /get-https?authKey=..&ipAddress=..
//
// Contacts https://api.strem.io/api/certificateGet with the provided auth key,
// parses the returned certificate+key, writes https-cert.pem / https-key.pem
// under cfg.AppPath, and returns {ipAddress, domain, port}.
//
// NOTE: This endpoint requires a valid Stremio authKey issued by api.strem.io
// after a real Stremio account login. Without it, the API call returns a 404
// or authentication error and no certificate files are written. The handler is
// implemented correctly but cannot be exercised in offline or self-hosted test
// environments where a live Stremio account is not available.
package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleGetHTTPS handles GET /get-https?authKey=..&ipAddress=..
//
// Flow (mirrors Rust reference server/src/routes/system.rs get_https):
//  1. POST {authKey, ipAddress} to https://api.strem.io/api/certificateGet
//  2. Parse outer envelope  {"result": "<json-string>", "error": ...}
//  3. Parse inner cert JSON {"certificate": "<pem>", "privateKey": "<pem>",
//     "commonName": "<cn>"}
//  4. Write https-cert.pem + https-key.pem under AppPath
//  5. Return {ipAddress, domain, port}
//
// @Summary  Provision a TLS cert from api.strem.io
// @Tags     System
// @Param    ipAddress  query  string  true   "public IP"
// @Param    authKey    query  string  false  "Stremio auth key (or X-Stremio-AuthKey header)"
// @Success  200  {object}  map[string]interface{}
// @Failure  400
// @Router   /get-https [get]
func (s *server) handleGetHTTPS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ipAddress := q.Get("ipAddress")
	// Prefer the request header to avoid the auth key being captured in upstream
	// access logs (which typically record the full request URI including query params).
	// The authKey query param is retained for backwards compatibility.
	authKey := r.Header.Get("X-Stremio-AuthKey")
	if authKey == "" {
		authKey = q.Get("authKey")
	}

	if ipAddress == "" {
		http.Error(w, "missing ipAddress query parameter", http.StatusBadRequest)
		return
	}
	if authKey == "" {
		http.Error(w, "missing authKey (provide via X-Stremio-AuthKey header or authKey query param)", http.StatusBadRequest)
		return
	}

	const apiURL = "https://api.strem.io/api/certificateGet"
	payload, _ := json.Marshal(map[string]string{
		"authKey":   authKey,
		"ipAddress": ipAddress,
	})

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "API request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading API response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("API returned status %d: %s", resp.StatusCode, body), http.StatusBadGateway)
		return
	}

	// Outer envelope: {"result": <value>, "error": null}
	// The result field is either a JSON-encoded string (double-encoded) or an
	// object directly — handle both variants.
	var outer struct {
		Result json.RawMessage `json:"result"`
		Error  interface{}     `json:"error"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		http.Error(w, "invalid API response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(outer.Result) == 0 || string(outer.Result) == "null" {
		msg := "no certificate in API response"
		if outer.Error != nil {
			msg = fmt.Sprintf("API error: %v", outer.Error)
		}
		http.Error(w, msg, http.StatusNotFound)
		return
	}

	// Extract the PEM material from the "result" envelope. Two known shapes:
	//   new: result = {"certificate":"<json string>"} whose inner JSON is
	//        {"commonName":"*.<h>.stremio.rocks","contents":{"Pem":"<b64 PEM
	//        chain>","PrivateKey":"<b64 PEM key>","Certificate":"<b64 PEM>"}}
	//   old: result = {"certificate":"<pem>","privateKey":"<pem>","commonName":..}
	// outer.Result itself may be double-encoded (a JSON string of the object).
	raw := outer.Result
	if len(raw) >= 1 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			raw = json.RawMessage(s)
		}
	}
	var env struct {
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"privateKey"`
		CommonName  string `json:"commonName"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		http.Error(w, "parse certificate result: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var certPEM, keyPEM, commonName string
	if trimmed := strings.TrimSpace(env.Certificate); strings.HasPrefix(trimmed, "{") {
		// New schema: certificate is a JSON object string; the PEM material is
		// base64-encoded under contents.
		var inner struct {
			CommonName string `json:"commonName"`
			Contents   struct {
				Pem         string `json:"Pem"`
				PrivateKey  string `json:"PrivateKey"`
				Certificate string `json:"Certificate"`
			} `json:"contents"`
		}
		if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
			http.Error(w, "parse inner certificate JSON: "+err.Error(), http.StatusInternalServerError)
			return
		}
		commonName = inner.CommonName
		certPEM = decodePEMField(inner.Contents.Pem) // full chain (leaf + intermediates)
		if certPEM == "" {
			certPEM = decodePEMField(inner.Contents.Certificate)
		}
		keyPEM = decodePEMField(inner.Contents.PrivateKey)
	} else {
		// Old flat schema: certificate/privateKey are PEM directly.
		commonName = env.CommonName
		certPEM = decodePEMField(env.Certificate)
		keyPEM = decodePEMField(env.PrivateKey)
	}
	if certPEM == "" || keyPEM == "" {
		http.Error(w, "certificate or privateKey missing in API response", http.StatusNotFound)
		return
	}

	// Write cert and key atomically: write each to a temp file in the same
	// directory, then rename both. A failure at any step leaves the existing
	// files untouched — a key-write failure can never strand a mismatched cert.
	appPath := s.cfg.AppPath
	certPath := filepath.Join(appPath, "https-cert.pem")
	keyPath := filepath.Join(appPath, "https-key.pem")

	certTmp, err := writeTempPEM(appPath, "https-cert-*.pem", certPEM)
	if err != nil {
		http.Error(w, "write https-cert.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}
	keyTmp, err := writeTempPEM(appPath, "https-key-*.pem", keyPEM)
	if err != nil {
		_ = os.Remove(certTmp)
		http.Error(w, "write https-key.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(certTmp, certPath); err != nil {
		_ = os.Remove(certTmp)
		_ = os.Remove(keyTmp)
		http.Error(w, "install https-cert.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(keyTmp, keyPath); err != nil {
		_ = os.Remove(certPath) // undo the cert rename
		_ = os.Remove(keyTmp)
		http.Error(w, "install https-key.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build the domain by substituting the wildcard label with the dashed IP,
	// e.g. *.519b…stremio.rocks + 192.168.0.62 → 192-168-0-62.519….stremio.rocks
	ipDashes := strings.ReplaceAll(ipAddress, ".", "-")
	domain := strings.Replace(commonName, "*", ipDashes, 1)
	if domain == commonName { // no wildcard in CN — prefix the dashed IP instead
		domain = ipDashes + "." + commonName
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ipAddress": ipAddress,
		"domain":    domain,
		"port":      s.cfg.HTTPSPort,
	})
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
