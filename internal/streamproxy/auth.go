package streamproxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

var errUnauthorized = errors.New("unauthorized")
var errForbidden = errors.New("forbidden")

// token is the payload sealed inside a signed URL token.
type token struct {
	Endpoint string            `json:"endpoint"`
	Params   map[string]string `json:"params"`
	Exp      int64             `json:"exp"`
	IP       string            `json:"ip"`
}

// authorize checks the request against the configured IP ACL, signed token, and password.
// Returns nil on success, errUnauthorized or errForbidden on failure.
func (h *Handler) authorize(r *http.Request) error {
	// IP ACL check: if a list is set the client must match at least one entry.
	if len(h.cfg.IPACL) > 0 {
		ip := clientIP(r)
		allowed := false
		for _, cidr := range h.cfg.IPACL {
			if cidr.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			return errForbidden
		}
	}

	// Signed token: if present and Secret is configured, verify and short-circuit password.
	if tok := r.URL.Query().Get("token"); tok != "" && len(h.cfg.Secret) > 0 {
		t, err := h.verifyToken(tok, clientIP(r))
		if err != nil {
			return errUnauthorized
		}
		// Bind token to its intended endpoint: reject if the token was issued for a
		// different path. An empty Endpoint field skips the check (legacy tokens).
		if t.Endpoint != "" && t.Endpoint != r.URL.Path {
			return errUnauthorized
		}
		return nil
	}

	// Password check.
	if h.cfg.Password != "" {
		provided := r.URL.Query().Get("api_password")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.Password)) != 1 {
			return errUnauthorized
		}
	}

	return nil
}

// signToken seals t with AES-GCM using cfg.Secret.
// The output is nonce||ciphertext encoded as base64url.
func (h *Handler) signToken(t token) (string, error) {
	if len(h.cfg.Secret) == 0 {
		return "", errors.New("no signing secret configured")
	}
	plain, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(h.cfg.Secret)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

// verifyToken decodes and verifies a signed token string.
// It checks expiry and, when t.IP is set, that client matches.
func (h *Handler) verifyToken(s string, client net.IP) (token, error) {
	if len(h.cfg.Secret) == 0 {
		return token{}, errors.New("no signing secret configured")
	}
	ct, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return token{}, errors.New("invalid token encoding")
	}
	block, err := aes.NewCipher(h.cfg.Secret)
	if err != nil {
		return token{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return token{}, err
	}
	ns := gcm.NonceSize()
	if len(ct) < ns {
		return token{}, errors.New("token too short")
	}
	plain, err := gcm.Open(nil, ct[:ns], ct[ns:], nil)
	if err != nil {
		return token{}, errors.New("token decryption failed")
	}
	var t token
	if err := json.Unmarshal(plain, &t); err != nil {
		return token{}, errors.New("invalid token payload")
	}
	if t.Exp < time.Now().Unix() {
		return token{}, errors.New("token expired")
	}
	if t.IP != "" {
		cs := ""
		if client != nil {
			cs = client.String()
		}
		if cs != t.IP {
			return token{}, errors.New("token IP mismatch")
		}
	}
	return t, nil
}
