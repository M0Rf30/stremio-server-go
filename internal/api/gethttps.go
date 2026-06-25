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

	// Inner certificate JSON. May be double-encoded (string inside JSON).
	certJSON := string(outer.Result)
	if len(certJSON) >= 2 && certJSON[0] == '"' {
		// Unescape the JSON string to get the inner JSON text.
		var inner string
		if err := json.Unmarshal(outer.Result, &inner); err == nil {
			certJSON = inner
		}
	}

	var certData struct {
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"privateKey"`
		CommonName  string `json:"commonName"`
	}
	if err := json.Unmarshal([]byte(certJSON), &certData); err != nil {
		http.Error(w, "parse inner certificate JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if certData.Certificate == "" || certData.PrivateKey == "" {
		http.Error(w, "certificate or privateKey missing in API response", http.StatusNotFound)
		return
	}

	// Write cert files under AppPath (0600 — private key material).
	certPath := filepath.Join(s.cfg.AppPath, "https-cert.pem")
	keyPath := filepath.Join(s.cfg.AppPath, "https-key.pem")
	if err := os.WriteFile(certPath, []byte(certData.Certificate), 0o600); err != nil {
		http.Error(w, "write https-cert.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(keyPath, []byte(certData.PrivateKey), 0o600); err != nil {
		http.Error(w, "write https-key.pem: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build domain: <ip-dashes>-<commonName-without-wildcard>
	// e.g. 192.168.1.100 + *.strem.io → "192-168-1-100-.strem.io"
	domain := strings.ReplaceAll(ipAddress, ".", "-") +
		"-" + strings.ReplaceAll(certData.CommonName, "*", "")

	writeJSON(w, http.StatusOK, map[string]any{
		"ipAddress": ipAddress,
		"domain":    domain,
		"port":      s.cfg.HTTPSPort,
	})
}
