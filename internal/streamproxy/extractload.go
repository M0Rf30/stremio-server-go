// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

const (
	maxDefDoc           = 1 << 20
	localDefRecheck     = 5 * time.Second
	defaultDefRefresh   = 6 * time.Hour
	remoteDefCacheName  = "extractors.remote.json"
	remoteDefFetchLimit = 30 * time.Second
)

// defRegistry serves extractor definitions merged from an optional remote,
// signature-verified list and a local file; local entries win by name.
type defRegistry struct {
	localPath string
	cachePath string
	remoteURL string
	pubKey    ed25519.PublicKey
	refresh   time.Duration
	// client fetches the operator-configured remote list. It deliberately
	// does not use the SSRF-guarded proxy client: like STREMIO_TRACKERS_URL,
	// the URL comes from the environment, not from a request, and may point
	// at a self-hosted LAN/localhost copy. Authenticity comes from the
	// ed25519 signature, not from the address.
	client *http.Client

	mu        sync.Mutex
	local     *DefSet
	localMod  time.Time
	lastCheck time.Time
	remote    *DefSet

	stopOnce sync.Once
	stop     chan struct{}
}

func newDefRegistry(cfg Config) *defRegistry {
	reg := &defRegistry{
		refresh: cfg.ExtractorsRefresh,
		client:  &http.Client{Timeout: remoteDefFetchLimit},
		stop:    make(chan struct{}),
	}
	if reg.refresh <= 0 {
		reg.refresh = defaultDefRefresh
	}
	if cfg.AppPath != "" {
		reg.localPath = filepath.Join(cfg.AppPath, "extractors.json")
		reg.cachePath = filepath.Join(cfg.AppPath, remoteDefCacheName)
	}
	if cfg.ExtractorsFile != "" {
		reg.localPath = cfg.ExtractorsFile
	}
	if cfg.ExtractorsURL != "" {
		key, err := parsePubKey(cfg.ExtractorsPubKey)
		if err != nil {
			logging.For("extractor").Warn("remote extractors disabled: a valid ed25519 public key is required", "err", err)
		} else {
			reg.remoteURL, reg.pubKey = cfg.ExtractorsURL, key
			reg.loadRemoteCache()
			go reg.refreshLoop()
		}
	}
	return reg
}

func (reg *defRegistry) close() {
	reg.stopOnce.Do(func() { close(reg.stop) })
}

// parsePubKey accepts a 32-byte ed25519 key as hex or base64.
func parsePubKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("STREMIO_EXTRACTORS_PUBKEY is not set")
	}
	for _, dec := range []func(string) ([]byte, error){hex.DecodeString, base64.StdEncoding.DecodeString, base64.RawURLEncoding.DecodeString} {
		if b, err := dec(s); err == nil && len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, errors.New("public key must be 32 bytes, hex or base64")
}

// decodeSig accepts a raw 64-byte signature or its hex/base64 text form.
func decodeSig(b []byte) ([]byte, error) {
	if len(b) == ed25519.SignatureSize {
		return b, nil
	}
	s := strings.TrimSpace(string(b))
	for _, dec := range []func(string) ([]byte, error){hex.DecodeString, base64.StdEncoding.DecodeString, base64.RawURLEncoding.DecodeString} {
		if d, err := dec(s); err == nil && len(d) == ed25519.SignatureSize {
			return d, nil
		}
	}
	return nil, errors.New("bad signature encoding")
}

// lookup returns the definition for a host name, reloading the local file
// if it changed.
func (reg *defRegistry) lookup(name string) (*Extractor, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.reloadLocalLocked()
	if reg.local != nil {
		if ex, ok := reg.local.Extractors[name]; ok {
			return ex, true
		}
	}
	if reg.remote != nil {
		if ex, ok := reg.remote.Extractors[name]; ok {
			return ex, true
		}
	}
	return nil, false
}

func (reg *defRegistry) reloadLocalLocked() {
	if reg.localPath == "" || time.Since(reg.lastCheck) < localDefRecheck {
		return
	}
	reg.lastCheck = time.Now()
	fi, err := os.Stat(reg.localPath)
	if err != nil {
		if reg.local != nil && errors.Is(err, os.ErrNotExist) {
			logging.For("extractor").Info("local extractors removed", "path", reg.localPath)
		}
		reg.local, reg.localMod = nil, time.Time{}
		return
	}
	if fi.ModTime().Equal(reg.localMod) {
		return
	}
	reg.localMod = fi.ModTime()
	b, err := readCapped(reg.localPath)
	if err != nil {
		logging.For("extractor").Warn("read local extractors", "path", reg.localPath, "err", err)
		return
	}
	ds, err := ParseDefSet(b)
	if err != nil {
		// keep the previous good set
		logging.For("extractor").Warn("invalid local extractors; keeping previous", "path", reg.localPath, "err", err)
		return
	}
	reg.local = ds
	logging.For("extractor").Info("local extractors loaded", "path", reg.localPath, "count", len(ds.Extractors))
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxDefDoc+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDefDoc {
		return nil, errors.New("definitions file too large")
	}
	return b, nil
}

func (reg *defRegistry) loadRemoteCache() {
	if reg.cachePath == "" {
		return
	}
	doc, err1 := readCapped(reg.cachePath)
	sig, err2 := readCapped(reg.cachePath + ".sig")
	if err1 != nil || err2 != nil {
		return
	}
	ds, err := reg.verify(doc, sig)
	if err != nil {
		logging.For("extractor").Warn("ignoring cached remote extractors", "err", err)
		return
	}
	reg.mu.Lock()
	reg.remote = ds
	reg.mu.Unlock()
}

func (reg *defRegistry) verify(doc, sig []byte) (*DefSet, error) {
	s, err := decodeSig(sig)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(reg.pubKey, doc, s) {
		return nil, errors.New("signature verification failed")
	}
	return ParseDefSet(doc)
}

func (reg *defRegistry) refreshLoop() {
	t := time.NewTicker(reg.refresh)
	defer t.Stop()
	for {
		if err := reg.fetchRemote(); err != nil {
			logging.For("extractor").Warn("remote extractors refresh failed", "url", reg.remoteURL, "err", err)
		}
		select {
		case <-reg.stop:
			return
		case <-t.C:
		}
	}
}

func (reg *defRegistry) fetchRemote() error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteDefFetchLimit)
	defer cancel()
	go func() {
		select {
		case <-reg.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	doc, err := reg.get(ctx, reg.remoteURL)
	if err != nil {
		return err
	}
	sig, err := reg.get(ctx, reg.remoteURL+".sig")
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	ds, err := reg.verify(doc, sig)
	if err != nil {
		return err
	}
	reg.mu.Lock()
	reg.remote = ds
	reg.mu.Unlock()
	logging.For("extractor").Info("remote extractors loaded", "url", reg.remoteURL, "count", len(ds.Extractors))
	if reg.cachePath != "" {
		if err := atomicWrite(reg.cachePath, doc); err == nil {
			_ = atomicWrite(reg.cachePath+".sig", sig)
		}
	}
	return nil
}

func (reg *defRegistry) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c := reg.client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDefDoc+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDefDoc {
		return nil, errors.New("document too large")
	}
	return b, nil
}

func atomicWrite(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".extractors-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
