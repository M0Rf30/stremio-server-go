// Command stremio-server is a lightweight, IPv6-capable drop-in replacement for
// Stremio's closed-source streaming server (server.js), built on
// anacrolix/torrent. It serves the enginefs HTTP API that stremio-web expects.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: pprof is served only on the loopback STREMIO_PPROF listener, never the main handler
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/api"
	"github.com/M0Rf30/stremio-server-go/internal/engine"
	"github.com/M0Rf30/stremio-server-go/internal/logging"
	"github.com/M0Rf30/stremio-server-go/internal/media"
	"github.com/M0Rf30/stremio-server-go/internal/settings"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// version is overridable at build time with -ldflags "-X main.version=...".
// It is reported as settings.serverVersion; keep it aligned with a real Stremio
// server version so stremio-web does not gate features.
var version = "4.21.0"

// Build metadata, injected at release time via
// -ldflags "-X main.buildVersion=... -X main.buildCommit=... -X main.buildDate=...".
// These are distinct from `version` (the Stremio-compatible serverVersion).
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildDate    = ""
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		logging.For("config").Warn("invalid integer env", "key", key, "value", v, "default", def)
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		logging.For("config").Warn("invalid integer env", "key", key, "value", v, "default", def)
	}
	return def
}

// envBool parses a boolean env var. Unset → def. "0", "false", "no", "off"
// (case-insensitive) → false; any other non-empty value → true.
func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// metadataURL resolves the Cinemeta-compatible metadata addon base URL used by
// the /bitmagnet and /torznab add-ons to turn an IMDB id into a title. Unset →
// the official Cinemeta. An explicit empty value or off/0/false/no/disabled
// turns resolution off (the add-ons then query by the raw IMDB id).
func metadataURL() string {
	v, ok := os.LookupEnv("STREMIO_METADATA_URL")
	if !ok {
		return "https://v3-cinemeta.strem.io"
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false", "no", "disable", "disabled":
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// defaultTrackersURL is the curated public tracker list fetched and ranked at
// startup. Override via STREMIO_TRACKERS_URL; an empty/off value disables the
// remote fetch entirely (the embedded/cached list plus DHT/PEX still apply).
const defaultTrackersURL = "https://raw.githubusercontent.com/XIU2/TrackersListCollection/master/best.txt"

func trackersURL() string {
	v, ok := os.LookupEnv("STREMIO_TRACKERS_URL")
	if !ok {
		return defaultTrackersURL
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false", "no", "disable", "disabled":
		return ""
	}
	return strings.TrimSpace(v)
}

// @title        stremio-server-go enginefs API
// @version      4.21.0
// @description  HTTP API served by stremio-server-go, a pure-Go drop-in for Stremio's streaming server (server.js). serverVersion is reported as 4.21.0 for client feature-gating; it is independent of the binary build version.
// @license.name MIT
// @license.url  https://github.com/M0Rf30/stremio-server-go/blob/main/LICENSE
// @host         127.0.0.1:11470
// @BasePath     /
// @schemes      http https
func main() {
	logging.Setup()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version":
			fmt.Printf("stremio-server %s (server-api %s)\n", buildVersion, version)
			if buildCommit != "" {
				fmt.Printf("commit %s, built %s\n", buildCommit, buildDate)
			}
			return
		}
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		logging.For("config").Warn("cannot determine home directory; using /tmp", "err", homeErr)
		home = "/tmp"
	}
	appPath := os.Getenv("APP_PATH")
	if appPath == "" {
		appPath = filepath.Join(home, ".stremio-server")
	}
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		logging.Fatal("cannot create app path", "err", err, "path", appPath)
	}

	cfg := types.Config{
		HTTPPort:          envInt("HTTP_PORT", 11470),
		HTTPSPort:         envInt("HTTPS_PORT", 12470), // self-signed HTTPS for https web UIs (WebKitGTK)
		AppPath:           appPath,
		CacheRoot:         appPath,
		MemoryCacheSize:   envInt64("STREMIO_MEMORY_CACHE_SIZE", 0), // bytes; 0 = disabled (write pieces to disk)
		ListenPort:        envInt("BT_LISTEN_PORT", 0),
		WebUI:             getenv("WEB_UI_LOCATION", "https://web.stremio.com/"),
		Version:           version,
		TrackersMax:       envInt("STREMIO_TRACKERS_MAX", 5),
		ProxyPassword:     getenv("STREMIO_PROXY_PASSWORD", ""),
		ProxySecret:       proxySecret(appPath),
		ProxyIPACL:        getenv("STREMIO_PROXY_IP_ACL", ""),
		ProxyPrebuffer:    envInt("STREMIO_PROXY_PREBUFFER", 3),
		ProxySegCacheTTL:  envInt("STREMIO_PROXY_SEG_CACHE_TTL", 300),
		ProxyPublicURL:    getenv("STREMIO_PROXY_PUBLIC_URL", ""),
		ProxyUpstream:     getenv("STREMIO_PROXY_UPSTREAM", ""),
		BitmagnetURL:      getenv("STREMIO_BITMAGNET_URL", ""),
		TorznabURL:        getenv("STREMIO_TORZNAB_URL", ""),
		TorznabAPIKey:     getenv("STREMIO_TORZNAB_APIKEY", ""),
		MetadataURL:       metadataURL(),                               // Cinemeta-compatible meta addon base; "" disables (STREMIO_METADATA_URL)
		DisableTrackers:   envBool("STREMIO_DISABLE_TRACKERS", false),  // disable all tracker announces (DHT/PEX/webseeds still used); accepts 1/true/0/false
		DisableWebtorrent: envBool("STREMIO_DISABLE_WEBTORRENT", true), // default disabled; set =0/false to enable WebRTC/WebTorrent (pion) peers
		EnableDLNA:        envBool("STREMIO_ENABLE_DLNA", false),       // default disabled; set =1/true to enable /casting DLNA discovery + control
		PeersPerTorrent:   envInt("STREMIO_PEERS_PER_TORRENT", 0),      // 0 = default 50/25/500; lower (e.g. 30) trims peer goroutines & RAM
		TrackersURL:       trackersURL(),                               // remote tracker list; "" disables remote fetch (STREMIO_TRACKERS_URL)
		LocalIMDB:         envBool("STREMIO_LOCAL_IMDB", true),         // local-files addon IMDB resolution; default on
		BTEncryption:      getenv("STREMIO_BT_ENCRYPTION", "prefer"),
		BTProxy:           getenv("STREMIO_BT_PROXY", ""),
		DHTBootstrap:      getenv("STREMIO_DHT_BOOTSTRAP", ""),
		BTAnonymous:       envBool("STREMIO_BT_ANONYMOUS", false),
		IdleTimeout:       time.Duration(envInt("STREMIO_TORRENT_IDLE_TIMEOUT", 300)) * time.Second, // 0 = disabled
	}
	if cfg.DisableWebtorrent {
		logging.For("engine").Info("webtorrent/webrtc peers disabled")
	}
	if !cfg.EnableDLNA {
		logging.For("casting").Info("dlna disabled")
	}
	if cfg.MetadataURL == "" {
		logging.For("metadata").Info("metadata (cinemeta) resolution disabled")
	}
	if cfg.TrackersURL == "" {
		logging.For("engine").Info("remote tracker list disabled (DHT/PEX/embedded only)")
	}
	if !cfg.LocalIMDB {
		logging.For("localaddon").Info("IMDB resolution disabled")
	}
	if cfg.BTEncryption == "require" {
		logging.For("engine").Info("bittorrent encryption required (plaintext peers refused)")
	}
	if cfg.BTEncryption == "disable" {
		logging.For("engine").Info("bittorrent encryption disabled")
	}
	if cfg.BTProxy != "" {
		logging.For("engine").Info("bittorrent proxy configured (trackers/webseeds/metainfo only; peers direct)", "proxy", cfg.BTProxy)
	}
	if cfg.DHTBootstrap != "" {
		logging.For("engine").Info("extra dht bootstrap nodes configured", "nodes", cfg.DHTBootstrap)
	}
	if cfg.BTAnonymous {
		logging.For("engine").Info("anonymous mode enabled (client fingerprint hidden)")
	}
	if cfg.IdleTimeout <= 0 {
		logging.For("engine").Info("idle torrent removal disabled")
	} else {
		logging.For("engine").Info("idle torrent removal enabled", "timeout", cfg.IdleTimeout.String())
	}

	// Optional soft memory ceiling for RAM-constrained hosts (the runtime also
	// honors the native GOMEMLIMIT env). 0 = unset.
	if lim := envInt64("STREMIO_MEM_LIMIT", 0); lim > 0 {
		debug.SetMemoryLimit(lim)
		logging.For("runtime").Info("soft memory limit set", "bytes", lim)
	}
	// Periodically return reclaimable idle heap (e.g. freed after a large
	// torrent or cache drained) to the OS. Guarded by a threshold so an idle or
	// steady-state server skips the forced GC entirely instead of paying a
	// stop-the-world collection on every tick.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		var ms runtime.MemStats
		for range t.C {
			runtime.ReadMemStats(&ms)
			if ms.HeapIdle-ms.HeapReleased > 64<<20 {
				debug.FreeOSMemory()
			}
		}
	}()

	ss, err := settings.New(cfg)
	if err != nil {
		logging.Fatal("settings init failed", "err", err)
	}
	em, err := engine.New(cfg)
	if err != nil {
		logging.Fatal("engine init failed", "err", err)
	}
	defer func() { _ = em.Close() }()

	// Wire the cache-eviction janitor without adding to the types interface.
	// StartJanitor is detected via structural type assertion on the concrete *manager.
	if j, ok := em.(interface{ StartJanitor(func() int64) }); ok {
		j.StartJanitor(func() int64 {
			switch n := ss.Get("cacheSize").(type) {
			case float64:
				return int64(n)
			case int:
				return int64(n)
			case int64:
				return n
			default:
				return 0 // nil/unknown => unlimited
			}
		})
	}

	// Wire live bandwidth limits from settings — no changes to the types interface.
	// SetLimitFn is detected via structural assertion on the concrete *manager, matching
	// the pattern used for StartJanitor above.
	//   • btDownloadSpeedHardLimit: 0 = unlimited; positive = bytes/sec download cap.
	//   • seedingEnabled: false → upload effectively disabled (1 byte/sec); true → unlimited.
	if l, ok := em.(interface{ SetLimitFn(func() (int64, int64)) }); ok {
		l.SetLimitFn(func() (int64, int64) {
			// --- download cap ---
			var down int64
			switch n := ss.Get("btDownloadSpeedHardLimit").(type) {
			case float64:
				if n > 0 {
					down = int64(n)
				}
			case int:
				if n > 0 {
					down = int64(n)
				}
			case int64:
				if n > 0 {
					down = n
				}
			}

			// --- upload cap ---
			// seedingEnabled=false → 1 byte/sec (effectively no upload) so peers
			// get valid rate.Limiter reservations but seeding is negligible.
			// seedingEnabled=true → 0 (unlimited).
			var up int64
			if seeding, _ := ss.Get("seedingEnabled").(bool); !seeding {
				up = 1
			}

			return down, up
		})
	}

	baseLocal := fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort)
	prober := media.New(baseLocal)

	handler := api.New(em, ss, prober, cfg)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logging.For("http").Info("listening", "version", version, "addr", baseLocal, "app_path", appPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Fatal("http server error", "err", err)
		}
	}()

	// Optional pprof endpoint for diagnostics; disabled unless STREMIO_PPROF is
	// set (e.g. STREMIO_PPROF=127.0.0.1:6060). Handlers come from net/http/pprof.
	// ppSrv is declared here so it can join the graceful-shutdown WaitGroup below.
	var ppSrv *http.Server
	if addr := os.Getenv("STREMIO_PPROF"); addr != "" {
		ppSrv = &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logging.For("pprof").Info("listening", "addr", addr)
			if err := ppSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logging.For("pprof").Error("server error", "err", err)
			}
		}()
	}

	var (
		tlsSrv   *http.Server
		provStop = make(chan struct{})
	)
	if cfg.HTTPSPort > 0 {
		// Prefer an API-issued cert (written by GET /get-https) over a self-signed one.
		// Both use the same HTTPS listener; cert preference is transparent to clients.
		certFile := filepath.Join(appPath, "https-cert.pem")
		keyFile := filepath.Join(appPath, "https-key.pem")
		cert, certErr := tls.LoadX509KeyPair(certFile, keyFile)
		if certErr != nil {
			logging.For("https").Warn("no persisted cert; falling back to self-signed", "err", certErr)
			cert, certErr = selfSignedCert()
		} else {
			logging.For("https").Info("using persisted cert", "path", certFile)
		}
		if certErr != nil {
			logging.For("https").Warn("cert init failed; https disabled", "err", certErr)
		} else {
			holder := &certHolder{}
			holder.set(cert)
			if h, ok := handler.(interface{ SetCertReloadHook(func()) }); ok {
				h.SetCertReloadHook(func() {
					c, err := tls.LoadX509KeyPair(certFile, keyFile)
					if err != nil {
						logging.For("https").Error("live cert reload failed", "err", err)
						return
					}
					holder.set(c)
					logging.For("https").Info("live cert reloaded", "path", certFile)
				})
			}
			tlsSrv = &http.Server{
				Addr:              fmt.Sprintf(":%d", cfg.HTTPSPort),
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				TLSConfig:         &tls.Config{GetCertificate: holder.get, MinVersion: tls.VersionTLS12},
			}
			go func() {
				logging.For("https").Info("listening", "version", version, "addr", fmt.Sprintf("https://127.0.0.1:%d", cfg.HTTPSPort), "app_path", appPath)
				if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logging.For("https").Error("server error", "err", err)
				}
			}()
			// Auto-provision/renew a browser-trusted cert from api.strem.io when an
			// authKey is available (env STREMIO_CERT_AUTHKEY or cached by a prior
			// /get-https call). Hot-swaps the live cert; no restart needed.
			go renewCertLoop(appPath, holder, provStop)
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logging.For("http").Info("shutting down")
	close(provStop) // stop the cert renewer

	var shutWg sync.WaitGroup
	shutOne := func(s *http.Server, name string) {
		defer shutWg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			logging.For(name).Error("shutdown error", "err", err)
		}
	}
	shutWg.Add(1)
	go shutOne(srv, "http")
	if tlsSrv != nil {
		shutWg.Add(1)
		go shutOne(tlsSrv, "https")
	}
	if ppSrv != nil {
		shutWg.Add(1)
		go shutOne(ppSrv, "pprof")
	}
	shutWg.Wait()
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// proxySecret returns the proxy signing secret.
// Priority: STREMIO_PROXY_SECRET env var > <appPath>/proxy-secret file > auto-generated.
// A generated secret is persisted to <appPath>/proxy-secret (mode 0o600).
func proxySecret(appPath string) string {
	if s := os.Getenv("STREMIO_PROXY_SECRET"); s != "" {
		return s
	}
	secretFile := filepath.Join(appPath, "proxy-secret")
	if data, err := os.ReadFile(secretFile); err == nil {
		return strings.TrimSpace(string(data))
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		logging.Fatal("failed to generate proxy secret", "err", err)
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		logging.For("proxy").Warn("failed to persist secret", "path", secretFile, "err", err)
	}
	return secret
}
