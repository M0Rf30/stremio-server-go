// Command stremio-server is a lightweight, IPv6-capable drop-in replacement for
// Stremio's closed-source streaming server (server.js), built on
// anacrolix/torrent. It serves the enginefs HTTP API that stremio-web expects.
//
// All bootstrap logic (env parsing, subsystem wiring, HTTP/HTTPS listeners,
// graceful shutdown) lives in internal/app so it can be shared with
// cmd/libstremio, the c-shared library used on SELinux-enforcing Android
// where exec() of a bundled binary is blocked. main here keeps only what is
// specific to running as a standalone OS process: the version subcommand,
// OS signal handling, and process exit codes.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/M0Rf30/stremio-server-go/internal/app"
)

// version is overridable at build time with -ldflags "-X main.version=...".
// It is reported as settings.serverVersion; keep it aligned with a real Stremio
// server version so stremio-web does not gate features.
var version = app.DefaultVersion

// Build metadata, injected at release time via
// -ldflags "-X main.buildVersion=... -X main.buildCommit=... -X main.buildDate=...".
// These are distinct from `version` (the Stremio-compatible serverVersion).
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildDate    = ""
)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := app.Run(ctx, app.Config{Lookup: app.OSLookup, Version: version}, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stremio-server:", err)
		os.Exit(1)
	}
}
