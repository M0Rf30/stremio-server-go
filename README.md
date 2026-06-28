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
- **Disk-bounded cache** - LRU eviction honouring the `cacheSize` setting, plus idle-torrent removal after inactivity (`STREMIO_TORRENT_IDLE_TIMEOUT`).
- Self-signed HTTPS on `:12470` for HTTPS web UIs (e.g. WebKitGTK shells).
- **Metrics** - `GET /metrics` exposes Prometheus-format gauges (goroutines, heap, active torrents, HLS sessions, proxy cache).

## Install

Prebuilt binaries for **Linux, macOS, Windows, and Android (arm64)** are attached
to each [release](https://github.com/M0Rf30/stremio-server-go/releases).

From source (Go 1.26+; CGO is not required):

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
| `HTTP_PORT` | `11470` | enginefs HTTP API port |
| `HTTPS_PORT` | `12470` | HTTPS port (`0` disables). Serves a persisted cert if present, else self-signed; with a Stremio authKey it auto-provisions and renews a browser-trusted Let's Encrypt cert via `/get-https`. |
| `BT_LISTEN_PORT` | `0` | BitTorrent peer port (`0` = OS-assigned) |
| `APP_PATH` | `~/.stremio-server` | data/cache root |
| `STREMIO_MEMORY_CACHE_SIZE` | `0` | in-RAM piece-cache budget in bytes; `0` writes pieces to disk (default). When `>0`, stream through a bounded RAM cache and never write piece data to disk (mobile / low-disk / HuggingFace). |
| `STREMIO_TORRENT_IDLE_TIMEOUT` | `300` | seconds a torrent may sit with no open stream readers and no access before it is dropped (peers disconnected, cached pieces freed). Matches official Stremio's inactive-torrent reclaim so a stopped stream is released even when `cacheSize` is unlimited, while staying alive long enough for instant scrub/resume/next-episode. `0` disables idle removal (cache-size LRU only). |
| `WEB_UI_LOCATION` | `https://web.stremio.com/` | redirect target for `GET /` |
| `LOCAL_FILES_DIR` | _(unset)_ | directory scanned by the local-files addon |
| `STREMIO_LOCAL_IMDB` | `on` | local-files add-on resolves filenames to IMDB ids/metadata via IMDb's suggestion API (catalog posters/titles). **Enabled by default**; set `=0`/`off` to disable — local files then keep filename titles + `local:` ids and no request is sent to IMDb. |
| `STREMIO_HWACCEL` | _(auto)_ | `0` forces software transcode; or pin `vaapi`/`nvenc`/… |
| `STREMIO_HTTP_LOG` | _(off)_ | `1` emits a structured access log line per request (`method`, `uri`, `status`, `duration_ms`, `bytes`, `remote`) |
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
| `STREMIO_PEERS_PER_TORRENT` | `50` | established peer connections per torrent (half-open=n/2, high-water=n*10); lower (e.g. 30) trims peer goroutines/RAM |
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

## Platforms

`CGO_ENABLED=0` everywhere, so all targets are pure-Go cross-compiles:
`linux/{amd64,arm64,arm}`, `darwin/{amd64,arm64}`, `windows/{amd64,arm64}`,
`android/arm64`. Android additionally builds with `-ldflags=-checklinkname=0`
(for `github.com/wlynxg/anet` on Go 1.23+); it also runs as a plain
`linux/arm64` binary under Termux.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/stremio-server` | entrypoint, wiring, TLS, version |
| `internal/types` | shared contract (structs + interfaces) |
| `internal/engine` | anacrolix client, readers, stats, trackers, cache eviction |
| `internal/api` | enginefs routes, streaming, proxy, casting, local-addon, youtube, archive/nzb/ftp |
| `internal/settings` | settings store (`server-settings.json`) |
| `internal/media` | ffprobe, HLS transcode, subtitles, opensub hash |
| `internal/archive` | uniform reader over zip / rar / 7zip / tar / tgz entries |
| `internal/nzb` | NZB parser, yEnc decoder, NNTP client, segment assembler |
| `internal/ftpstream` | FTP/FTPS + HTTP(S) byte-range stream opener |
| `internal/logging` | structured slog logger (leveled, component-tagged, text/json) |
| `docs/swagger.yaml` | OpenAPI/Swagger spec, generated from code (`make swagger`) |
| `scripts/smoke.sh` | end-to-end API smoke test |

## Security

This is a **localhost service with no authentication** - bind it to loopback and
do not expose `:11470` to untrusted networks. By design it shells out to
`ffmpeg`/`yt-dlp` and acts as an open reverse proxy (`/proxy`) for the local web
UI; treat anything that can reach the port as fully trusted.

## License

[MIT](LICENSE) - Copyright (c) 2026 Gianluca Boiano.
