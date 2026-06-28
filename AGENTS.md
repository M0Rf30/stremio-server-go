# Repository Guidelines

## Project Overview

`stremio-server-go` is a pure-Go, IPv6-capable drop-in replacement for Stremio's
closed-source streaming server (`server.js`), built on
[`anacrolix/torrent`](https://github.com/anacrolix/torrent). It serves the exact
**enginefs HTTP API** that `stremio-web`/`stremio-core` expect on `:11470`
(HTTPS `:12470`), so it can be pointed at instead of the official binary. It is a
localhost-trust service with **no authentication** by design.

Response shapes (e.g. `stats.json`, `/settings`, `/create`) MUST stay
byte-compatible with the official `server.js` and the strict `stremio-core`
deserializers — treat `internal/types` as a wire contract, not just structs.

## Architecture & Data Flow

`cmd/stremio-server/main.go` reads env config, then wires the four subsystems and
injects them into the HTTP layer:

```text
main → engine.New(cfg) ─┐
       settings.New(cfg)─┼→ api.New(em, ss, prober, cfg) → http.Handler
       media.New(cfg) ───┘
```

Request path: `server.ServeHTTP` (applies CORS) → `route()` (manual dispatch on
the first path segment) → `func (s *server) handleX(w, r, seg)`.

Streaming (`GET /{infoHash}/{fileIdx}`): `EnsureEngine(ih)` adds the torrent →
`Ready(ctx)` blocks for metadata → `NewReader(idx)` returns a piece-prioritized
`io.ReadSeekCloser` → the handler honors `Range`, copies via a pooled 64 KiB
buffer, and sets DLNA headers. The engine prioritizes the streamed file
(demoting others), primes moov/header pieces, scales readahead by bandwidth, and
runs an LRU janitor against `settings.cacheSize`.

Archive/NZB/FTP follow the same shape (see Patterns): a package-level
`map[key]*session` guarded by a mutex, with a lazily-started TTL janitor.
Settings persist to `<APP_PATH>/server-settings.json` via atomic temp+rename.

## Key Directories

| Path | Purpose |
|---|---|
| `cmd/stremio-server` | entrypoint: env parsing, wiring, TLS, graceful shutdown, pprof |
| `internal/types` | shared contract: `Config` + `Engine`/`EngineManager`/`SettingsStore`/`MediaProber` interfaces + `Stats`/`Options` wire structs |
| `internal/api` | enginefs routes, streaming, proxy, casting, addons (`archive.go`, `nzb.go`, `ftp.go`, `bitmagnet.go`, `torznab.go`, `localaddon.go`, `youtube.go`, `gethttps.go`) |
| `internal/engine` | anacrolix client wrapper, readers, trackers, LRU cache eviction, in-RAM piece storage |
| `internal/media` | `ffprobe`/`ffmpeg` shell-outs: probe, HLS transcode, subtitles, opensub hash |
| `internal/streamproxy` | `/proxy` HLS/DASH rewrite, DRM decrypt, signed URLs, segment cache |
| `internal/netguard` | SSRF guard: private/loopback/cloud-metadata IP checks + a dialer `Control` hook (DNS-rebinding-safe), shared by the proxy, `/create`, and ftpstream |
| `internal/settings` | `server-settings.json` store (atomic persist) |
| `internal/logging` | `slog`-based structured logger (text/json, leveled, component-tagged) |
| `internal/archive` `internal/nzb` `internal/ftpstream` | format/protocol backends behind the matching api handlers |

## Development Commands

```sh
make build        # host binary (CGO_ENABLED=0) → ./stremio-server
make run          # build + run
make test         # go test -p 1 -race ./...   (serial, race detector)
make vet          # go vet ./...
make fmt          # gofmt -s -w .
make fmt-check    # fail if not gofmt -s clean
make lint         # golangci-lint run ./... (if installed)
make swagger      # regenerate docs/swagger.{yaml,json} from // @… annotations (needs swaggo/swag)
make build-all    # cross-compile all 8 release targets → dist/
make smoke        # ./scripts/smoke.sh end-to-end API test (needs a running build)
```

Cross-compile targets (all `CGO_ENABLED=0`): `linux/{amd64,arm64,armv7}`,
`darwin/{amd64,arm64}`, `windows/{amd64,arm64}`, `android/arm64` (needs
`-ldflags=-checklinkname=0`). Benchmarks: `go test -bench . -benchmem ./internal/api/`.

## Code Conventions & Common Patterns

- **Dependency injection** via `api.New(em, ss, prober, cfg)` returning interface
  types from `internal/types`; never reach into concrete impls from `api`.
- **Handlers** are methods: `func (s *server) handleX(w http.ResponseWriter, r *http.Request, seg []string)`, dispatched from `route()`. JSON via `writeJSON(w, code, map[string]any{...})`.
- **Session caches** are package-level `map[key]*state` + `sync.Mutex` + a
  `time.Time` lastAccess + a janitor started once via `sync.Once`
  (`archiveStartJanitor`, `nzbStartJanitor`, casting `deviceCache`,
  `availableInterfaces` TTL cache). Reuse this pattern; don't invent new ones.
- **Logging**: `logging.For("component").Info("lowercase event", "snake_case_key", val)`.
  Errors as `..., "err", err`. NEVER assign `logging.For(...)` to a package-level
  var (it captures the pre-`Setup` default) — call it inline or store it in a
  constructor that runs after `main` calls `logging.Setup()`.
- **Errors**: every return checked or explicitly `_ =`-discarded; use
  `errors.Is(err, target)` for sentinels (never `==`) and `%w` to wrap.
- **Env config** lives only in `main.go` via `getenv`/`envInt`/`envBool`, or a
  `LookupEnv`-based helper when empty must mean "disabled" (see `metadataURL()`,
  `trackersURL()` — `off`/`0`/`false`/empty → disabled).
- **Imports**: the project module goes in its **own** goimports group
  (`goimports local-prefixes: github.com/M0Rf30/stremio-server-go`); keep
  `gofmt -s` clean.
- **Pure Go only**: `CGO_ENABLED=0`. Avoid new dependencies and never add cgo or
  shell-outs to new binaries (existing exec is limited to `ffmpeg`/`yt-dlp`).

## Important Files

- `cmd/stremio-server/main.go` — wiring, env, TLS hot-swap, shutdown.
- `internal/api/api.go` — `route()` dispatcher, streaming, `parseRange`, CORS, shared helpers.
- `internal/types/types.go` — the interface + wire-shape contract (`Stats`, `Options`).
- `internal/engine/engine.go` + `trackers.go` — torrent client, readers, ranked tracker list.
- `internal/media/hls.go` — HLS session manager (path-traversal-guarded session ids).
- `.golangci.yml`, `Makefile`, `Dockerfile`, `.goreleaser.yml`, `docs/swagger.yaml`.

## Runtime / Tooling Preferences

- **Go 1.26** (the `go.mod` directive; CI uses `go-version-file: go.mod`, Docker uses `golang:1.26-alpine`). Keep the directive build-compatible across the cross-compile matrix.
- **`CGO_ENABLED=0`** everywhere — no native deps.
- **golangci-lint v2** (`errcheck, errorlint, gosec, govet, ineffassign, misspell, revive, staticcheck, unconvert, unused, whitespace`). `gosec` excludes by-design rules (variable URLs/subprocess/file paths — this is a localhost media server). `_test.go` files skip `errcheck`/`gosec`/`bodyclose`/`unparam` but still face `gofmt`/`staticcheck`/`revive`.
- **Runtime deps** for full functionality: `ffmpeg`/`ffprobe` (HLS/probe/subtitles), `yt-dlp` (`/yt`). Bundled in the container image.
- Config knobs are env vars (`STREMIO_*`, `HTTP_PORT`, `APP_PATH`, …) — see the table in `README.md`; add new ones in `main.go` and document them there.

## Testing & QA

- **Framework**: stdlib `testing` + `net/http/httptest` only — no external test
  libraries. Tests are table-driven and live in-package as `*_test.go`.
- **HTTP handler tests** use in-package fakes in `internal/api/api_test.go`
  (`fakeEngine`/`fakeEM`/`fakeSS`/`fakeProber`, `newHandler(t, …)`,
  `testEngine()`) to drive `api.New` through `httptest`. Reuse these.
- **Offline & deterministic** is mandatory: no real network, torrents, ffmpeg, or
  NNTP. Use `httptest.Server` for HTTP, `t.TempDir()` for files, in-test fixtures
  for zip/NZB/yEnc, and `net.Pipe` for NNTP.
- **Run**: `make test` (= `go test -p 1 -race ./...`); per-package
  `go test ./internal/api/`; coverage `go test -cover ./...`.
- **Expectations for new code**: add tests in the owning package; assert response
  shapes against the `server.js`/`stremio-core` contract (see
  `internal/types/types_test.go` `TestOptionsJSONShape`); keep benchmarks
  (`bench_test.go`) green as perf-regression guards. `cmd/` has no tests (thin wiring).
