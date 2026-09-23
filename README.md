# stremio-server-go

A lightweight, **IPv6-capable**, open-source drop-in replacement for Stremio's
closed-source streaming server (`server.js`), built on
[`anacrolix/torrent`](https://github.com/anacrolix/torrent). It serves the exact
`enginefs` HTTP API that `stremio-web` expects, so it drops straight in.

Not affiliated with or endorsed by Stremio.

## Features

- **Dual-stack torrent engine** - IPv4 + IPv6, TCP + uTP, BEP32 DHT, PEX,
  WebTorrent, webseeds; curated public trackers with RTT ranking + 24h refresh.
- **Full enginefs API** - `create`/`stats.json`/`remove`, Range/206 streaming
  with DLNA headers, `network-info`, `device-info`, `settings`, `opensubHash`,
  `subtitlesTracks`, `/probe`, `/tracks`, `/list`, `/:ih/peers`.
- **HLSv2 transcoding** - on-demand ffmpeg with multi-platform hardware accel
  (VAAPI / NVENC / QSV / VideoToolbox / V4L2M2M, verified at startup, libx264
  fallback), multi-audio + embedded-subtitle renditions, and seek-aware segments.
- **Subtitles** - SRT / WebVTT / ASS-SSA to WebVTT, OpenSubtitles hashing.
- **Reverse proxy** (`/proxy`), **DLNA casting** (`/casting`, SSDP discovery +
  UPnP AVTransport control), **local-files addon** (`/local-addon`, IMDB-resolved),
  **YouTube** (`/yt`, via `yt-dlp`), and **`/get-https`** (Stremio cert provisioning).
- **Archive streaming** - direct playback of media inside ZIP / RAR / 7z / TAR /
  TGZ containers (`/zip`, `/rar`, `/7zip`, `/tar`, `/tgz`), plus **Usenet/NZB**
  (`/nzb`, NNTP + yEnc) and **FTP/FTPS** (`/ftp`) streaming - all pure-Go.
- **Disk-bounded cache** - LRU eviction honouring the `cacheSize` setting: `0` is a true "no caching" mode (every torrent with zero open readers is purged once its idle grace window elapses; a negative/unlimited value never evicts on size), plus idle-torrent removal after inactivity (`STREMIO_TORRENT_IDLE_TIMEOUT`).
- Self-signed HTTPS on `:12470` for HTTPS web UIs (e.g. WebKitGTK shells).
- **Metrics** - `GET /metrics` exposes Prometheus-format gauges (goroutines, heap, active torrents, HLS sessions, proxy cache) with permissive CORS headers (`Access-Control-Allow-Origin: *`); **do not expose to untrusted networks** — bind to loopback only.

## Install

Prebuilt binaries for **Linux, macOS, Windows, and Android (arm64/armv7)** are
attached to each [release](https://github.com/M0Rf30/stremio-server-go/releases).

From source (Go 1.27.1+; CGO is not required):

```sh
go install github.com/M0Rf30/stremio-server-go/cmd/stremio-server@latest
# or, in a checkout:
make build          # -> ./stremio-server
make build-all      # cross-compile every target into dist/
```

### Container / HuggingFace

A multi-stage `Dockerfile` (ffmpeg + yt-dlp bundled, non-root, `/data` volume)
builds an image runnable under Podman/Docker or as a HuggingFace Space.
See [docs/CONTAINER.md](docs/CONTAINER.md).

### Browser-trusted HTTPS for the Stremio UI

The Stremio web app can't talk to a plaintext local server. The
**"HTTPS endpoint for streaming"** setting provisions a browser-trusted
`*.stremio.rocks` cert for your LAN IP via `/get-https`. Full setup +
troubleshooting: [docs/HTTPS.md](docs/HTTPS.md).

### Decentralized torrents (Bitmagnet)

The `/bitmagnet` add-on streams from a self-hosted
[Bitmagnet](https://bitmagnet.io) DHT index. A compose file and full setup
guide are in [docs/BITMAGNET.md](docs/BITMAGNET.md).

### Universal indexers (Torznab)

The `/torznab` add-on queries any Torznab-compatible indexer (Prowlarr, Jackett,
NZBHydra2, or Bitmagnet's built-in `/torznab` endpoint). Setup guide:
[docs/TORZNAB.md](docs/TORZNAB.md).

## Run

```sh
./stremio-server            # http://127.0.0.1:11470  (https on :12470)
./stremio-server version    # print build version
```

Then point any Stremio client's **streaming server URL** at
`http://127.0.0.1:11470`.

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `BIND_ADDRESS` | _(unset)_ | interface the HTTP/HTTPS listeners bind to. Unset (the default) binds **every** interface — on an IPv6-enabled host that includes globally routable addresses, and this API is **unauthenticated**. Set `127.0.0.1` (or `::1`) to restrict it to loopback, which is all the official Stremio desktop/web client needs. |
| `STREMIO_ALLOWED_ORIGINS` | _(unset)_ | comma-separated extra allowed browser `Origin` values, checked on state-changing routes and `/proxy`. Each entry is `scheme://host[:port]` (exact match), `host[:port]` (matches either `http://` or `https://`), or `*.domain` (any scheme, subdomains only — the bare domain itself does not match). `*` alone restores the legacy behavior (no Origin check, `Access-Control-Allow-Origin: *` on every response, no `Vary`). Unset: requests with **no** `Origin` header (native players, curl) are always allowed with `Access-Control-Allow-Origin: *`; browser requests are allowed from the official Stremio web origins (`https://web.stremio.com`, `https://web.strem.io`, `https://app.strem.io`, `https://staging.strem.io`, `https://*.stremio.rocks`), `localhost`/`127.0.0.1`/`[::1]` on any port, this server's own interface IPs on any port, plus anything listed here. A disallowed `Origin` (including the literal `null`) gets `403 {"error":"origin not allowed"}` before the request is routed — GET and the CORS `OPTIONS` preflight alike. An allowed non-empty `Origin` gets `Access-Control-Allow-Origin: <that origin>` + `Vary: Origin` instead of `*`; its preflight also gets `Access-Control-Allow-Private-Network: true` when the request sent `Access-Control-Request-Private-Network: true`. |
| `HTTP_PORT` | `11470` | enginefs HTTP API port |
| `HTTPS_PORT` | `12470` | HTTPS port (`0` disables). Serves a persisted cert if present, else self-signed; with a Stremio authKey it auto-provisions and renews a browser-trusted Let's Encrypt cert via `/get-https`. |
| `BT_LISTEN_PORT` | `0` | BitTorrent peer port (`0` = OS-assigned) |
| `APP_PATH` | `~/.stremio-server` | data/cache root |
| `STREMIO_MEMORY_CACHE_SIZE` | `0` | in-RAM piece-cache budget in bytes; `0` writes pieces to disk (default). When `>0`, stream through a bounded RAM cache and never write piece data to disk (mobile / low-disk / HuggingFace). |
| `STREMIO_TORRENT_IDLE_TIMEOUT` | `300` | seconds a torrent may sit with no open stream readers and no access before it is dropped (peers disconnected, cached pieces freed). Matches official Stremio's inactive-torrent reclaim so a stopped stream is released even when `cacheSize` is unlimited, while staying alive long enough for instant scrub/resume/next-episode. `0` disables idle removal (cache-size LRU only). |
| `STREMIO_MAX_SEED_RATIO` | `0` | stop uploading a torrent once its share ratio (bytes uploaded / bytes downloaded) reaches this value; `0` (the default) seeds without limit. Accepts fractions, e.g. `0.5`. Enforced per torrent on the same 30 s janitor tick as the cache/idle passes, and **independent of `STREMIO_TORRENT_IDLE_TIMEOUT`**: reaching the ratio only pauses uploading, it never drops the torrent or purges its cache, so capping seeding does not shorten how long a torrent stays available for instant scrub/resume. Uploading resumes automatically if the ratio falls back below the cap (more data downloaded, or the cap raised). A torrent that has downloaded nothing is never paused. |
| `WEB_UI_LOCATION` | `https://web.stremio.com/` | redirect target for `GET /` |
| `LOCAL_FILES_DIR` | _(unset)_ | directory scanned by the local-files addon |
| `STREMIO_ARCHIVE_LOCAL_ROOT` | _(unset)_ | root directory under which `/{zip,rar,7zip,tar,tgz}/create` may open a **local** archive path. Unset falls back to `LOCAL_FILES_DIR`; if both are unset, local-path archive sources are disabled entirely and only `http(s)://` sources are accepted. Paths are resolved through symlinks and must stay inside the root. |
| `STREMIO_ARCHIVE_ALLOW_PRIVATE` | _(off)_ | `1`/`true` lets `/{zip,rar,7zip,tar,tgz}/create` and `/nzb/create` reach private/loopback/RFC1918 hosts — including the NNTP `servers[]` connections `/nzb/create` opens and any HTTP redirect an archive/NZB fetch follows (both are re-validated at dial time, not just the initial URL). Off by default (SSRF guard); the cloud-metadata address stays blocked either way. Enable only to pull archives/NZBs from a LAN host. |
| `STREMIO_FTP_ALLOW_PRIVATE` | _(off)_ | `1`/`true` lets `/ftp` reach private/loopback/RFC1918 hosts. Off by default (SSRF guard); the cloud-metadata address stays blocked either way. Enable to stream from a LAN NAS. |
| `STREMIO_LOCAL_IMDB` | `on` | local-files add-on resolves filenames to IMDB ids/metadata via IMDb's suggestion API (catalog posters/titles). **Enabled by default**; set `=0`/`off` to disable — local files then keep filename titles + `local:` ids and no request is sent to IMDb. |
| `STREMIO_HWACCEL` | _(auto)_ | `0` forces software transcode; or pin `vaapi`/`nvenc`/… |
| `STREMIO_HTTP_LOG` | _(off)_ | `1` emits a structured access log line per request (`method`, `uri`, `status`, `duration_ms`, `bytes`, `remote`). Standard boolean parsing: `0`/`false`/`no`/`off` (case-insensitive) disable it, any other non-empty value enables it — unlike most other on/off knobs here this used to be "any non-empty value enables", so `STREMIO_HTTP_LOG=0` now disables logging instead of enabling it. |
| `STREMIO_LOG_LEVEL` | `info` | log verbosity: `debug` / `info` / `warn` / `error` |
| `STREMIO_LOG_FORMAT` | `text` | log output format: `text` (compact `time LEVEL component: msg key=value`) or `json` |
| `STREMIO_PROXY_PASSWORD` | _(unset)_ | `api_password` required on `/proxy/*` requests |
| `STREMIO_PROXY_SECRET` | _(auto)_ | signing key for signed proxy URLs (auto-generated under `APP_PATH`) |
| `STREMIO_PROXY_IP_ACL` | _(unset)_ | comma-separated CIDR allowlist for proxy clients |
| `STREMIO_PROXY_PREBUFFER` | `3` | upcoming segments to prefetch (`0` = off) |
| `STREMIO_PROXY_SEG_CACHE_TTL` | `300` | proxy segment cache TTL, seconds (`0` = off) |
| `STREMIO_PROXY_PUBLIC_URL` | _(derive)_ | external base URL written into rewritten manifests |
| `STREMIO_PROXY_UPSTREAM` | _(unset)_ | outbound upstream proxy for stream proxy (socks5/http/https); overridden per-request by `&proxy=` |
| `STREMIO_BITMAGNET_URL` | _(unset)_ | GraphQL endpoint of a self-hosted Bitmagnet instance; enables the `/bitmagnet` add-on. Unset = add-on serves the manifest but returns no streams. |
| `STREMIO_TORZNAB_URL` | _(unset)_ | Torznab indexer API base URL; enables the `/torznab` add-on. Unset = add-on serves the manifest but returns no streams. |
| `STREMIO_TORZNAB_APIKEY` | _(unset)_ | API key for the Torznab indexer. Required by Prowlarr and Jackett; not needed for Bitmagnet. |
| `STREMIO_METADATA_URL` | `https://v3-cinemeta.strem.io` | Cinemeta-compatible metadata add-on base URL used by `/bitmagnet` and `/torznab` to resolve an IMDB id to a title. **Enabled by default** (official Cinemeta). Point it at a self-hosted mirror or a TMDB/aiometadata add-on's configured base (anything serving `/meta/{type}/{id}.json`); set to empty / `off` to disable the lookup (add-ons then query by raw IMDB id). |
| `STREMIO_DISABLE_TRACKERS` | _(off)_ | disable all tracker announces (DHT/PEX/webseeds only) — for private/DHT-only operation |
| `STREMIO_TRACKERS_URL` | _(curated list)_ | source URL for the public tracker list fetched + ranked at startup (newline-delimited). Defaults to a curated list; set to empty / `off` to skip the remote fetch entirely (embedded/cached trackers + DHT/PEX only). Drives the `trackersSourceUrl` setting. |
| `STREMIO_TRACKERS_MAX` | `5` | max number of fastest-probed public trackers retained from the ranked list |
| `STREMIO_DISABLE_WEBTORRENT` | `on` | WebTorrent/WebRTC (pion) peers are **disabled by default** — cuts ~60% of goroutines & RAM; TCP/uTP/DHT peers unaffected. Set `=0`/`false` to re-enable. |
| `STREMIO_ENABLE_DLNA` | _(off)_ | enable DLNA/UPnP casting on `/casting` (SSDP discovery + UPnP AVTransport control). **Disabled by default**; set `=1`/`true` to enable. |
| `STREMIO_CERT_AUTHKEY` | _(unset)_ | Stremio authKey used to auto-provision/renew a trusted HTTPS cert from `api.strem.io`. If unset, a key cached from a prior `/get-https` call is reused. |
| `STREMIO_CERT_IP` | _(primary IPv4)_ | IP encoded into the provisioned cert's domain; defaults to the first non-loopback IPv4. |
| `STREMIO_PEERS_PER_TORRENT` | `0` | established peer connections per torrent. `0` (the default) is a sentinel meaning "use the built-in defaults" (50 established, 25 half-open, 500 high-water); set explicitly (e.g. `30`) to override (half-open=n/2, high-water=n*10). Lower values trim peer goroutines/RAM. |
| `STREMIO_MEM_LIMIT` | _(unset)_ | soft memory ceiling in bytes (runtime/debug.SetMemoryLimit; GOMEMLIMIT env also works). RSS high-water is returned to the OS every 5 min regardless |
| `STREMIO_BT_ENCRYPTION` | `prefer` | BitTorrent peer-connection encryption (MSE/PE header obfuscation). `prefer` encrypts when the peer supports it and falls back to plaintext (default, DPI-detectable). `require` refuses plaintext entirely (RC4 only) for DPI evasion in censored networks. `disable` turns obfuscation off. |
| `STREMIO_BT_PROXY` | _(unset)_ | Upstream proxy for BitTorrent **tracker announces, HTTP webseeds, metainfo fetch, and the tracker-list download** (`socks5://host:port` or `http(s)://host:port`). Lets you reach trackers blocked by your ISP. **Peer connections are not proxied** — use `STREMIO_BT_ENCRYPTION=require` for peer-traffic DPI evasion. |
| `STREMIO_DHT_BOOTSTRAP` | _(defaults)_ | Comma-separated `host:port` DHT bootstrap nodes appended to the built-in defaults. Use your own reachable nodes when the default bootstrap routers are blocked. |
| `STREMIO_BT_ANONYMOUS` | _(off)_ | `1`/`true` hides the client version/fingerprint advertised to peers (anacrolix AnonymousMode). |
| `STREMIO_PPROF` | _(unset)_ | when set to a `host:port` (e.g. `127.0.0.1:6060`), serves `net/http/pprof` on that address for profiling |

The stream proxy (HLS/DASH manifest rewriting, on-the-fly decryption, signed
URLs) is documented in [docs/PROXY.md](docs/PROXY.md).

### Censorship resistance

The server uses DHT (BEP32), PEX (BEP11), HTTP webseeds, and a ranked public
tracker list, so peer discovery continues even when individual trackers are
blocked. Four knobs let you harden further in censored or monitored networks:

- **`STREMIO_BT_ENCRYPTION=require`** — forces RC4 header obfuscation on every
  peer handshake, defeating DPI signatures that fingerprint plain BitTorrent
  traffic. Peers that refuse encryption are dropped.
- **`STREMIO_BT_PROXY`** — routes tracker announces, metainfo fetches, HTTP
  webseeds, and the tracker-list download through a SOCKS5 or HTTP proxy
  (e.g. `socks5://127.0.0.1:9050` for Tor). Use this when your ISP blocks
  trackers or the tracker-list host (`raw.githubusercontent.com`).
- **`STREMIO_DHT_BOOTSTRAP`** — supplies alternate DHT bootstrap nodes when the
  default routers (`router.bittorrent.com`, `dht.transmissionbt.com`, etc.) are
  filtered; accepts a comma-separated `host:port` list appended to the defaults.
- **`STREMIO_BT_ANONYMOUS=1`** — suppresses the client version string advertised
  to peers, reducing passive fingerprintability.

Example — running behind Tor:

```sh
STREMIO_BT_ENCRYPTION=require \
STREMIO_BT_PROXY=socks5://127.0.0.1:9050 \
STREMIO_BT_ANONYMOUS=1 \
stremio-server
```

**Limitation:** the proxy covers tracker/webseed/metainfo traffic only. Peer
TCP/uTP connections are established through the listen socket directly and are
not tunneled. For full peer anonymity a network-level VPN or transparent Tor
proxy is still required; the proxy + encryption knobs primarily defeat
tracker-level blocking and DPI-based fingerprinting of peer handshakes.

### Playback start priming and BitTorrent speed limits

When a stream starts, the engine raises piece priority on two small byte
windows of the file — the first 4 MiB (reaches the MP4 `ftyp`/`moov` atom for
faststart files, or a codec-init box) and the last 8 MiB (covers a
moov-at-the-end MP4 or an MKV's `Cues`/`Tags`) — to `PiecePriorityNow`, so the
player can parse the container and begin playback before the file finishes
downloading. This is intentionally a **byte** window (≈1 piece each at a
typical 16 MiB piece length), not a fixed piece count: anacrolix already
raises the reader's own current piece to `Now` and its live readahead window
to `Readahead` automatically, and orders same-priority pieces only by
partial → rarest → index. Pinning more head pieces at `Now`
than the true header needs puts them in the same priority tier as the
reader's actual current piece — on a thin swarm the tie-break can starve the
read and stall playback (players with a short low-speed timeout, e.g. Kodi's
Rivulet addon, then abort the stream). Everything past the header window is
left to the reader's own growing readahead buffer instead of being
explicitly pinned.

Three related settings (`GET`/`POST /settings`, not env vars) shape BitTorrent
downloading:

- **`btDownloadSpeedHardLimit`** (bytes/sec, `0` = unlimited) — a hard
  throughput cap applied globally via the rate limiter (shared across all
  torrents). The default, 3670016 (3.5 MiB/s, matching the official
  server's "Default" profile), is below the bitrate of most 4K remuxes; raise
  it (the official "Fast" profile uses 39321600) or set `0` for 4K.
- **`btDownloadSpeedSoftLimit`** (bytes/sec, `0` = disabled) together with
  **`btMinPeersForStable`** (peer count) implement the official Stremio
  contract: once a torrent is *already* downloading at or above the soft
  limit **and** has at least `btMinPeersForStable` peers connected, the
  server stops searching for additional peers for that torrent (the existing
  peers have proven they can sustain that speed) and resumes discovery the
  moment speed drops back below the limit. Unlike the hard limit, the soft
  limit never throttles bytes/sec — it only pauses/resumes peer acquisition,
  implemented via `Torrent.SetMaxEstablishedConns` (clamped to the current
  connection count while paused, restored to the configured
  `STREMIO_PEERS_PER_TORRENT` budget once resumed). Both settings are
  evaluated per torrent on the same 5 s tick that applies the bandwidth
  limiters.

## Platforms

`linux/{amd64,arm64,arm}`, `darwin/{amd64,arm64}`, `windows/{amd64,arm64}` all
build `CGO_ENABLED=0` as pure-Go cross-compiles. `android/{arm64,armv7}` are
the exception: Android ships no `/etc/resolv.conf`, so a pure-Go binary's
resolver falls back to `127.0.0.1:53` (nothing listens there) and every DNS
lookup fails, so both build `CGO_ENABLED=1` against an NDK clang, linking
against bionic's real resolver instead (`-ldflags=-checklinkname=0` is also
needed for `github.com/wlynxg/anet` on Go 1.23+). `android/arm64` also runs
as a plain `linux/arm64` binary under Termux.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/stremio-server` | entrypoint, wiring, TLS, version |
| `internal/types` | shared contract (structs + interfaces) |
| `internal/engine` | anacrolix client, readers, stats, trackers, cache eviction |
| `internal/api` | enginefs routes, streaming, proxy, casting, local-addon, youtube, archive/nzb/ftp |
| `internal/streamproxy` | `/proxy` HLS/DASH rewrite, DRM decrypt, signed URLs, segment cache |
| `internal/netguard` | SSRF guard: private/loopback/cloud-metadata checks + dial-time `Control` hook, shared by the proxy, archive/nzb/ftp fetches, and the media relay |
| `internal/settings` | settings store (`server-settings.json`) |
| `internal/media` | ffprobe, HLS transcode, subtitles, opensub hash |
| `internal/archive` | uniform reader over zip / rar / 7zip / tar / tgz entries |
| `internal/nzb` | NZB parser, yEnc decoder, NNTP client, segment assembler |
| `internal/ftpstream` | FTP/FTPS + HTTP(S) byte-range stream opener |
| `internal/logging` | structured slog logger (leveled, component-tagged, text/json) |
| `docs/swagger.yaml` | OpenAPI/Swagger spec, generated from code (`make swagger`) |
| `scripts/smoke.sh` | end-to-end API smoke test |

## Security

This is a **localhost service with no authentication** - treat anything that
can reach the port as fully trusted. **Recommended:** set
`BIND_ADDRESS=127.0.0.1` — the default binds **every** interface, including
any globally routable IPv6 address, to an unauthenticated API. Do not expose
`:11470`/`:12470` to untrusted networks.

Any website open in the user's browser can reach `http://127.0.0.1:11470`
directly (ordinary same-origin-policy behavior — the browser allows the
request, it just can't read a cross-origin response without CORS). To stop an
arbitrary page from silently driving the API just because the port is
reachable, state-changing routes and `/proxy` check the request's `Origin`
header against an allowlist (see `STREMIO_ALLOWED_ORIGINS` above); requests
with no `Origin` header (native players, curl, most non-browser clients) are
unaffected.

By design the server shells out to `ffmpeg`/`yt-dlp` and acts as an open
reverse proxy (`/proxy`) for the local web UI. Remote URLs given to
`ffmpeg`/`ffprobe` (`/probe`, `/tracks`, `/hlsv2` `mediaURL=`) are never
dialed by the ffmpeg/yt-dlp subprocess directly — they're fetched through a
guarded loopback relay, so the subprocess only ever talks to this server and
the SSRF guard (private/loopback/link-local/cloud-metadata blocking,
re-checked at dial time to defeat DNS rebinding) governs the actual fetch.
The same dial-time guard, including redirects, covers archive/NZB downloads
(`STREMIO_ARCHIVE_ALLOW_PRIVATE`) and FTP (`STREMIO_FTP_ALLOW_PRIVATE`).

## License

[MIT](LICENSE) - Copyright (c) 2026 Gianluca Boiano.
