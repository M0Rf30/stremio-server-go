// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var errUnauthorized = errors.New("unauthorized")
var errForbidden = errors.New("forbidden")

// tokenDestPrefixParam is a reserved token param key: instead of an exact
// query-parameter match it requires the decoded destination (d) to start with
// the sealed value. Used for DASH SegmentTemplate sub-tokens.
const tokenDestPrefixParam = "d_prefix"

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
		// Bind token to its sealed query parameters. An empty Params map
		// imposes no constraint (legacy tokens), mirroring the Endpoint check.
		// A sealed key must appear exactly once with exactly the sealed value
		// (a duplicate "d" could otherwise be interpreted differently by
		// different consumers). The reserved key tokenDestPrefixParam binds the
		// decoded destination to a prefix instead (DASH template URLs whose d
		// still holds $...$ placeholders at signing time).
		if len(t.Params) > 0 {
			q := r.URL.Query()
			for k, v := range t.Params {
				if k == tokenDestPrefixParam {
					if !strings.HasPrefix(decodeDest(q.Get("d")), v) {
						return errUnauthorized
					}
					continue
				}
				vals := q[k]
				if len(vals) == 0 {
					if v != "" {
						return errUnauthorized
					}
					continue
				}
				if len(vals) != 1 || vals[0] != v {
					return errUnauthorized
				}
			}
		}
		return nil
	}

	// Password check.
	if h.cfg.Password != "" {
		provided := r.URL.Query().Get("api_password")
		if subtle.ConstantTimeCompare([]byte(provided), h.passwordBytes) != 1 {
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
	// Reuse the pre-built GCM; cipher.AEAD is goroutine-safe (F7).
	gcm := h.signingGCM
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

// subToken mints a token for a rewritten sub-URL of a token-authorised
// manifest request. It reuses the parent token's expiry and IP binding (so the
// sub-URL never outlives the grant) and seals endpoint plus params so the
// token cannot be replayed against another path or destination. Returns ""
// when opts did not originate from a token-authorised request or signing fails.
// params may be nil for URLs whose destination is not known up front (e.g.
// DASH SegmentTemplate placeholders).
func (h *Handler) subToken(opts *Options, endpoint string, params map[string]string) string {
	if opts == nil || opts.subTokenExp == 0 || len(h.cfg.Secret) == 0 {
		return ""
	}
	tok, err := h.signToken(token{Endpoint: endpoint, Params: params, Exp: opts.subTokenExp, IP: opts.subTokenIP})
	if err != nil {
		return ""
	}
	return tok
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
	// Reuse the pre-built GCM; cipher.AEAD is goroutine-safe (F7).
	gcm := h.signingGCM
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
