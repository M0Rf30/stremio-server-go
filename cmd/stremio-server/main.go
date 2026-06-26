// Command stremio-server is a lightweight, IPv6-capable drop-in replacement for
// Stremio's closed-source streaming server (server.js), built on
// anacrolix/torrent. It serves the enginefs HTTP API that stremio-web expects.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: pprof is served only on the loopback STREMIO_PPROF listener, never the main handler
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/api"
	"github.com/M0Rf30/stremio-server-go/internal/engine"
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
		log.Printf("env %s: invalid integer %q, using default %d", key, v, def)
	}
	return def
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
		log.Printf("cannot determine home directory: %v; defaulting to /tmp", homeErr)
		home = "/tmp"
	}
	appPath := os.Getenv("APP_PATH")
	if appPath == "" {
		appPath = filepath.Join(home, ".stremio-server")
	}
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		log.Fatalf("cannot create app path %s: %v", appPath, err)
	}

	cfg := types.Config{
		HTTPPort:         envInt("HTTP_PORT", 11470),
		HTTPSPort:        envInt("HTTPS_PORT", 12470), // self-signed HTTPS for https web UIs (WebKitGTK)
		AppPath:          appPath,
		CacheRoot:        appPath,
		ListenPort:       envInt("BT_LISTEN_PORT", 0),
		WebUI:            getenv("WEB_UI_LOCATION", "https://web.stremio.com/"),
		Version:          version,
		TrackersMax:      envInt("STREMIO_TRACKERS_MAX", 5),
		ProxyPassword:    getenv("STREMIO_PROXY_PASSWORD", ""),
		ProxySecret:      proxySecret(appPath),
		ProxyIPACL:       getenv("STREMIO_PROXY_IP_ACL", ""),
		ProxyPrebuffer:   envInt("STREMIO_PROXY_PREBUFFER", 3),
		ProxySegCacheTTL: envInt("STREMIO_PROXY_SEG_CACHE_TTL", 300),
		ProxyPublicURL:   getenv("STREMIO_PROXY_PUBLIC_URL", ""),
		ProxyUpstream:    getenv("STREMIO_PROXY_UPSTREAM", ""),
	}

	ss, err := settings.New(cfg)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	em, err := engine.New(cfg)
	if err != nil {
		log.Fatalf("engine: %v", err)
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
		log.Printf("stremio-server %s listening at %s (app path %s)", version, baseLocal, appPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// Optional pprof endpoint for diagnostics; disabled unless STREMIO_PPROF is
	// set (e.g. STREMIO_PPROF=127.0.0.1:6060). Handlers come from net/http/pprof.
	if addr := os.Getenv("STREMIO_PPROF"); addr != "" {
		pp := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Printf("pprof listening at http://%s/debug/pprof/", addr)
			if err := pp.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("pprof server: %v", err)
			}
		}()
	}

	var tlsSrv *http.Server
	if cfg.HTTPSPort > 0 {
		// Prefer an API-issued cert (written by GET /get-https) over a self-signed one.
		// Both use the same HTTPS listener; cert preference is transparent to clients.
		certFile := filepath.Join(appPath, "https-cert.pem")
		keyFile := filepath.Join(appPath, "https-key.pem")
		cert, certErr := tls.LoadX509KeyPair(certFile, keyFile)
		if certErr != nil {
			log.Printf("https: no persisted cert (%v); falling back to self-signed", certErr)
			cert, certErr = selfSignedCert()
		} else {
			log.Printf("https: using persisted cert from %s", certFile)
		}
		if certErr != nil {
			log.Printf("https: cert init failed: %v (https disabled)", certErr)
		} else {
			tlsSrv = &http.Server{
				Addr:              fmt.Sprintf(":%d", cfg.HTTPSPort),
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
			}
			go func() {
				log.Printf("stremio-server %s HTTPS at https://127.0.0.1:%d", version, cfg.HTTPSPort)
				if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
					log.Printf("https server: %v", err)
				}
			}()
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	var shutWg sync.WaitGroup
	shutOne := func(s *http.Server, name string) {
		defer shutWg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			log.Printf("%s shutdown: %v", name, err)
		}
	}
	shutWg.Add(1)
	go shutOne(srv, "http")
	if tlsSrv != nil {
		shutWg.Add(1)
		go shutOne(tlsSrv, "https")
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
		log.Fatalf("proxy: failed to generate random secret: %v", err)
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		log.Printf("proxy: failed to persist secret to %s: %v", secretFile, err)
	}
	return secret
}
