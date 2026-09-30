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
| `BIND_ADDRESS` | _(unset)_ | interface the HTTP/HTTPS listeners bind to. Unset (the default) binds **every** interface — on an IPv6-enabled host that includes globally routable addresses, and this API is **unauthenticated**. Set `127.0.0.1` (or `::1`) to restrict it to loopback, which is all the official Stremio desktop/web client needs. When set to a specific non-loopback, non-wildcard address (e.g. a LAN IP), a second listener is also started on `127.0.0.1:HTTP_PORT` sharing the same handler, so `ffmpeg`/`ffprobe` self-requests (HLS transcode, `/yt`, `/proxy`) keep working; if that fallback bind itself fails, the server logs a warning and falls back to reaching itself via the configured `BIND_ADDRESS` instead. |
| `STREMIO_ALLOWED_ORIGINS` | _(unset)_ | comma-separated extra allowed browser `Origin` values, checked on state-changing routes and `/proxy`. Each entry is `scheme://host[:port]` (exact match), `host[:port]` (matches either `http://` or `https://`), or `*.domain` (any scheme, subdomains only — the bare domain itself does not match). `*` alone restores the legacy behavior (no Origin check, `Access-Control-Allow-Origin: *` on every response, no `Vary`). Unset: requests with **no** `Origin` header (native players, curl) are always allowed with `Access-Control-Allow-Origin: *`; browser requests are allowed from the official Stremio web origins (`https://web.stremio.com`, `https://web.strem.io`, `https://app.strem.io`, `https://staging.strem.io`, `https://*.stremio.rocks`), `localhost`/`127.0.0.1`/`[::1]` on any port, this server's own interface IPs on any port, plus anything listed here. A disallowed `Origin` (including the literal `null`) gets `403 {"error":"origin not allowed"}` before the request is routed — GET and the CORS `OPTIONS` preflight alike. An allowed non-empty `Origin` gets `Access-Control-Allow-Origin: <that origin>` + `Vary: Origin` instead of `*`; its preflight also gets `Access-Control-Allow-Private-Network: true` when the request sent `Access-Control-Request-Private-Network: true`. |
| `HTTP_PORT` | `11470` | enginefs HTTP API port |
| `HTTPS_PORT` | `12470` | HTTPS port (`0` disables). Serves a persisted cert if present, else self-signed; with a Stremio authKey it auto-provisions and renews a browser-trusted Let's Encrypt cert via `/get-https`. |
| `BT_LISTEN_PORT` | `0` | BitTorrent peer port (`0` = OS-assigned) |
| `APP_PATH` | `~/.stremio-server` | data/cache root |
| `STREMIO_MEMORY_CACHE_SIZE` | `0` | in-RAM piece-cache budget in bytes; `0` writes pieces to disk (default). When `>0`, stream through a bounded RAM cache and never write piece data to disk (mobile / low-disk / HuggingFace). |
| `STREMIO_TORRENT_IDLE_TIMEOUT` | `300` | seconds a torrent may sit with no open stream readers and no access before it is dropped (peers disconnected, cached pieces freed). Matches official Stremio's inactive-torrent reclaim so a stopped stream is released even when `cacheSize` is unlimited, while staying alive long enough for instant scrub/resume/next-episode. `0` disables idle removal (cache-size LRU only). |
| `STREMIO_MAX_SEED_RATIO` | `0` | stop uploading a torrent once its share ratio (bytes uploaded / bytes downloaded) reaches this value; `0` (the default) seeds without limit. Accepts fractions, e.g. `0.5`. Enforced per torrent on the same 30 s janitor tick as the cache/idle passes, and **independent of `STREMIO_TORRENT_IDLE_TIMEOUT`**: reaching the ratio only pauses uploading, it never drops the torrent or purges its cache, so capping seeding does not shorten how long a torrent stays available for instant scrub/resume. Uploading resumes automatically if the ratio falls back below the cap (more data downloaded, or the cap raised). A torrent that has downloaded nothing is never paused. |
| `STREMIO_CREATE_METADATA_TIMEOUT` | `90` | seconds (or a Go duration string, e.g. `2m`) `/create`, `/{infoHash}/create`, and `/{infoHash}/{fileIdx}` wait for a newly-added torrent's metadata before returning `504`. Slow-starting torrents or debrid links may need a longer window than the default. |
| `WEB_UI_LOCATION` | `https://web.stremio.com/` | redirect target for `GET /` |
| `LOCAL_FILES_DIR` | _(unset)_ | directory scanned by the local-files addon |
| `STREMIO_ARCHIVE_LOCAL_ROOT` | _(unset)_ | root directory under which `/{zip,rar,7zip,tar,tgz}/create` may open a **local** archive path. Unset falls back to `LOCAL_FILES_DIR`; if both are unset, local-path archive sources are disabled entirely and only `http(s)://` sources are accepted. Paths are resolved through symlinks and must stay inside the root. |
| `STREMIO_ARCHIVE_ALLOW_PRIVATE` | _(off)_ | `1`/`true` lets `/{zip,rar,7zip,tar,tgz}/create` and `/nzb/create` reach private/loopback/RFC1918 hosts — including the NNTP `servers[]` connections `/nzb/create` opens and any HTTP redirect an archive/NZB fetch follows (both are re-validated at dial time, not just the initial URL). Off by default (SSRF guard); the cloud-metadata address stays blocked either way. Enable only to pull archives/NZBs from a LAN host. |
| `STREMIO_FTP_ALLOW_PRIVATE` | _(off)_ | `1`/`true` lets `/ftp` reach private/loopback/RFC1918 hosts. Off by default (SSRF guard); the cloud-metadata address stays blocked either way. Enable to stream from a LAN NAS. |
| `STREMIO_LOCAL_IMDB` | `on` | local-files add-on resolves filenames to IMDB ids/metadata via IMDb's suggestion API (catalog posters/titles). **Enabled by default**; set `=0`/`off` to disable — local files then keep filename titles + `local:` ids and no request is sent to IMDb. |
| `STREMIO_LOCAL_FILES_PUBLIC_URL` | _(unset)_ | externally reachable base URL of this server (e.g. `http://192.168.1.50:11470`, trailing `/` trimmed). When set, local-files add-on streams are returned as `<base>/local-addon/file/<hash>` (HEAD + byte ranges) instead of same-host `file://` URLs, so remote Stremio clients can play them, and that endpoint is enabled. Only files already indexed from `LOCAL_FILES_DIR` are reachable, by opaque hash — but **any host that can reach the port can then download them**; unset (default) keeps `file://` URLs and the endpoint returns `404`. |
| `STREMIO_HWACCEL` | _(auto)_ | `0` forces software transcode; or pin `vaapi`/`nvenc`/… |
| `STREMIO_HLS_SESSION_TTL` | `60` | seconds (or a Go duration string, e.g. `2m`) an HLS transcode session may sit idle before the reaper evicts it and frees its segment cache. A paused player that comes back after this window sees `unknown session` and must re-request the master playlist. `0` disables idle eviction: sessions (and their segment caches) are kept until `DELETE /hlsv2/{id}` or shutdown (across restarts, with `STREMIO_HLS_PERSIST`), and still count toward `STREMIO_HLS_MAX_SESSIONS`. Negative or invalid values fall back to `60`; note that `0` itself also fell back to `60` before this meaning was introduced. |
| `STREMIO_HLS_REAPER_INTERVAL` | _(derived)_ | seconds (or a duration string) between HLS idle-session sweeps. Defaults to `min(30s, STREMIO_HLS_SESSION_TTL/2)` so a shorter TTL is still reaped promptly; set explicitly to override the derived value. |
| `STREMIO_HLS_NEG_PROBE_TTL` | `300` (`5m`) | seconds (or a duration string) a failed/zero-duration `ffprobe` result is cached, to avoid hammering a broken URL (e.g. a torrent with no peers yet) with repeated probes. |
| `STREMIO_HLS_POS_PROBE_TTL` | `600` (`10m`) | seconds (or a duration string) a successful `ffprobe` result is cached, so duplicate HLS sessions for the same URL skip re-probing. |
| `STREMIO_HLS_SESSION_OVERRIDES` | _(off)_ | `1`/`true` lets the request that creates an HLS session override its idle TTL and output quality via query parameters on `GET /hlsv2/{id}/master.m3u8` (see [Per-session HLS overrides](#per-session-hls-overrides)). Off by default: the parameters are then ignored entirely. **With the gate on, clients can raise as well as lower the env and `/settings` limits; enable it only when every client that can reach the server is trusted.** |
| `STREMIO_HLS_MAX_SESSIONS` | `64` | hard cap on simultaneously registered HLS transcode sessions. |
| `STREMIO_HLS_WORK_DIR` | _(OS temp dir)_ | root directory for HLS segment working directories (`stremio-hls-*` created under it). Unset uses the OS default temp dir, matching prior behaviour. Set to move segment I/O onto a specific fast or large disk. |
| `STREMIO_HLS_PERSIST` | _(off)_ | `1`/`true` keeps HLS sessions and their transcoded segments across restarts. Requires `STREMIO_HLS_WORK_DIR` and, to be useful, a much longer `STREMIO_HLS_SESSION_TTL`. See [Persistent HLS sessions](#persistent-hls-sessions). |
| `STREMIO_HLS_VAAPI_DEVICE` | `/dev/dri/renderD128` | VAAPI render-node device path used for hardware transcoding. Override on multi-GPU hosts (e.g. `/dev/dri/renderD129`). |
| `STREMIO_HLS_SEGMENT_TIMEOUT` | `120` | seconds (or a duration string) allowed for one `ffmpeg` segment transcode before it's killed. |
| `STREMIO_HLS_SUBTITLE_TIMEOUT` | `120` | seconds (or a duration string) allowed for one `ffmpeg` subtitle-track extraction before it's killed. |
| `STREMIO_HLS_SEEK_PREROLL` | `10` | seconds (or a duration string, e.g. `2500ms`) of source decoded ahead of each HLS segment's start, **for sources in an indexed container (Matroska/WebM, MP4/MOV) only**. Segments use a hybrid seek: a fast input seek to `start - pre-roll`, then a precise output seek that decodes and discards frames up to `start`, so every 4 s segment decodes at least 4 s + pre-roll of source. For indexed containers ffmpeg's input seek is already frame-accurate when re-encoding, so a smaller value (`0` = pure input seek) cuts decode work, which matters on low-power boxes decoding e.g. 4K HEVC in software. It can only shrink the margin: values above `10` are clamped, negative ones fall back to the default, and **every other source keeps the full 10 s**: MPEG-TS (`.ts`/`.m2ts`), HLS, FLV, raw streams and anything unrecognised, since their input seek can start decoding at the keyframe *after* the seek point. A segment from an indexed source that still comes out with missing or late video (e.g. a broken index in a partial download) is re-transcoded once with the full 10 s. So is a segment whose video is legitimately late (variable-frame-rate content holding one frame for seconds); such segments are never cached, so with a smaller pre-roll each request for one costs two transcodes. |
| `STREMIO_HLS_PROBE_TIMEOUT` | `30` | seconds (or a duration string) allowed for the combined `ffprobe` duration/stream probe. Slow-starting torrents and debrid links may need a longer window than the default (the timeout result is then negatively cached — see `STREMIO_HLS_NEG_PROBE_TTL`). |
| `STREMIO_TRANSCODE_VIDEO_BITRATE` | `8M` | target video bitrate (`-b:v`) for HLS transcodes, ffmpeg bitrate syntax (e.g. `8M`, `800k`). Superseded per-session by the `transcodeMaxBitRate` `/settings` value when it's set (`>0`) — see below. |
| `STREMIO_TRANSCODE_MAXRATE` | `8M` | video bitrate ceiling (`-maxrate`). `transcodeMaxBitRate` `/settings` overrides this (and `STREMIO_TRANSCODE_VIDEO_BITRATE`) with `-bufsize` set to 2x its value when set (`>0`; unit is bits/second, e.g. `8000000` for 8 Mbps). |
| `STREMIO_TRANSCODE_BUFSIZE` | `16M` | video rate-control buffer size (`-bufsize`). See `STREMIO_TRANSCODE_MAXRATE`. |
| `STREMIO_TRANSCODE_MAX_WIDTH` | `0` (no downscale) | maximum output video width; source is downscaled (never upscaled), aspect-preserved, dimensions forced even. `0` keeps full source resolution (today's behaviour). The `transcodeMaxWidth` `/settings` value (schema default `1920`) overrides this only when it's both `>0` and different from `1920` — its own fresh-install default is intentionally excluded so an untouched `/settings` file never starts capping resolution; set this env var to `1920` explicitly for that exact cap. |
| `STREMIO_TRANSCODE_MAX_HEIGHT` | `0` (no downscale) | maximum output video height; same downscale rules as `STREMIO_TRANSCODE_MAX_WIDTH`. No corresponding `/settings` key exists upstream. |
| `STREMIO_TRANSCODE_VAAPI_QP` | `23` | VAAPI encoder quantization parameter (`-qp`, lower = higher quality/bitrate). |
| `STREMIO_TRANSCODE_NVENC_PRESET` | `p4` | NVENC encoder preset (`-preset`). Overridden by the `transcodeProfile` `/settings` value — see `STREMIO_TRANSCODE_X264_PRESET`. |
| `STREMIO_TRANSCODE_QSV_PRESET` | `veryfast` | Intel Quick Sync (`h264_qsv`) encoder preset (`-preset`): `veryfast`, `faster`, `fast`, `medium`, `slow`, `slower` or `veryslow`. Overridden by the `transcodeProfile` `/settings` value — see `STREMIO_TRANSCODE_X264_PRESET`. Behaviour change: QSV previously always used `veryfast` and ignored `transcodeProfile`; a `transcodeProfile` of e.g. `slow`/`veryslow` now reaches QSV as well and may not keep up in real time on low-power iGPUs (the env default stays `veryfast` for that reason). |
| `STREMIO_TRANSCODE_X264_PRESET` | `veryfast` | libx264 encoder preset (`-preset`). The `transcodeProfile` `/settings` value overrides this, `STREMIO_TRANSCODE_NVENC_PRESET` and `STREMIO_TRANSCODE_QSV_PRESET` together when it names a recognized libx264 preset (`ultrafast` … `veryslow`), mapped to the closest NVENC preset and to the same-named QSV preset (`ultrafast`/`superfast`, which QSV lacks, map to `veryfast`). |
| `STREMIO_TRANSCODE_X264_CRF` | `23` | libx264 constant rate factor (`-crf`, lower = higher quality). |
| `STREMIO_TRANSCODE_TONEMAP` | `off` | HDR→SDR tone mapping for HLS transcodes of HDR10/HDR10+/HLG sources (detected from the probed PQ/HLG transfer, not bit depth). `off` (default) keeps today's plain pixel-format conversion, which leaves HDR sources looking washed out and grey. `on`/`true`/`1`/`yes` = `mobius` (in testing it came closest to the SDR reference brightness; `hable` was noticeably darker); or pick the tonemap algorithm explicitly: `mobius`, `hable`, `reinhard`, `clip`, `linear`, `gamma`. Invalid values log a warning and stay off. Requires an ffmpeg with the `zscale` (libzimg) and `tonemap` filters; if either is missing a warning is logged at startup and transcodes fall back to today's behaviour. Runs in software on every encoder path and is CPU-heavy — combine with `STREMIO_TRANSCODE_MAX_WIDTH`/`_MAX_HEIGHT` (the downscale happens before tone mapping). See below. |
| `STREMIO_TRANSCODE_AUDIO_CHANNELS` | `2` | output audio channel count (`-ac`). |
| `STREMIO_TRANSCODE_AUDIO_BITRATE` | `192k` | output AAC audio bitrate (`-b:a`), ffmpeg bitrate syntax. |
| `STREMIO_TRANSCODE_CONCURRENCY` | _(`runtime.NumCPU()`)_ | maximum concurrent `ffmpeg` segment transcode jobs across every HLS session. The `transcodeConcurrency` `/settings` value (schema default `1`) overrides this live — re-read on every transcode, not just new sessions — only when it's both `>0` and different from `1`, for the same untouched-default reason as `STREMIO_TRANSCODE_MAX_WIDTH`. |
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

The HLS master playlist's `BANDWIDTH` and video `CODECS` (H.264 profile-level)
are derived from the effective bitrate cap and output resolution — the
`STREMIO_TRANSCODE_MAXRATE`/`transcodeMaxBitRate` bits/second value halved for
`BANDWIDTH`, and the output width×height mapped to the matching H.264 level
(≤1280×720 → Level 3.1, ≤1920×1080 → Level 4.1, ≤2560×1440 → Level 5.0, else
Level 5.1). An unconfigured server (no bitrate/resolution knob touched) keeps
advertising the historical fixed `BANDWIDTH=4000000`/`avc1.640029` regardless
of actual source resolution, so upgrading never changes existing playback.
A per-session quality override (below) always counts as "configured" and
takes the derived path, even when its values equal the defaults.

### Per-session HLS overrides

With `STREMIO_HLS_SESSION_OVERRIDES=1`, a front end serving several
independent sessions can give each its own quality and lifetime, e.g. one
watch-party room at 5 Mbps for a friend on a slow link and another at
12 Mbps, or a title prepared hours ahead of a scheduled party with a longer
idle TTL than casual sessions. The request that creates a session may add:

| Parameter | Meaning | Accepted values |
|---|---|---|
| `ttl` | idle-eviction TTL for this session | whole seconds or a Go duration (`3600`, `90m`), from `60s` (the stock default, so an override never makes a session less durable) to `30 days` |
| `maxWidth`, `maxHeight` | downscale caps, same rules as `STREMIO_TRANSCODE_MAX_WIDTH`/`_HEIGHT` (aspect-preserving, never upscales) | `16`–`7680`, rounded down to even |
| `bitrate` | video target (`-b:v`); unless given explicitly, `maxRate` = `bitrate` and `bufSize` = 2× `bitrate` | ffmpeg syntax, decimals allowed as for the `STREMIO_TRANSCODE_*` env knobs (`6M`, `1.5M`, `800k`), `100k`–`200M` |
| `maxRate`, `bufSize` | explicit `-maxrate` / `-bufsize`; when `bitrate` and `maxRate` are both given, `maxRate` must be at least `bitrate` | as `bitrate`; `bufSize` up to `400M` |

```text
GET /hlsv2/room-a/master.m3u8?mediaURL=…&bitrate=5M&maxHeight=720&ttl=4h
```

- **Precedence:** per-session override > `/settings` (`transcodeMaxBitRate`,
  `transcodeMaxWidth`) > env. Knobs not overridden keep their `/settings`/env
  value; the master playlist `BANDWIDTH`/`CODECS` follow the effective values.
  A per-session `ttl` beats `STREMIO_HLS_SESSION_TTL=0`
  (`DisableIdleEviction`) too: a session created with an explicit `ttl` is
  still evicted by the reaper on schedule even while global idle eviction is
  off; only a session with no `ttl` of its own is kept forever in that mode.
- **Validation:** an invalid or out-of-range value fails the request with
  `400` and a message naming the parameter; no session is created. Parameter
  names are case-sensitive camelCase as above: unknown or mis-cased names
  (e.g. `maxrate`) are ignored, and a repeated parameter uses its first
  value. The `mediaURL` SSRF check and session-id guard run first and are
  unaffected.
- **Trust:** overrides are not clamped to the operator's limits. With the
  gate on, clients can raise as well as lower the env and `/settings` limits
  (e.g. `maxWidth=7680&bitrate=200M`), and a long `ttl` can hold
  `STREMIO_HLS_MAX_SESSIONS` slots and disk for days. Enable it only when
  every client that can reach the server is trusted.
- **Existing sessions:** a session's transcode settings are frozen when it is
  created, so quality parameters on later `master.m3u8` requests for the same
  id are ignored. `ttl` is the exception: it replaces the session's TTL, so a
  caller can extend (or shorten, never below `60s`) a session's lifetime.
- **TTL granularity:** idle sessions are reaped on each
  `STREMIO_HLS_REAPER_INTERVAL` tick, so a session is removed between `ttl`
  and `ttl` + one reaper interval (≤30s by default) after its last access. A
  long-TTL session keeps its slot in `STREMIO_HLS_MAX_SESSIONS` (and its
  segment cache on disk) until then.

`DELETE /hlsv2/{id}` ends an HLS transcode session explicitly, for front ends
that manage their own session lifecycle rather than relying on the idle
reaper (and the only way to end one when `STREMIO_HLS_SESSION_TTL=0`). The
session is unregistered at once, so later requests for it get `unknown
session`, and its segment cache is removed from disk as soon as no request
for it is still being served. Responses: `204` deleted, `404` unknown
session, `400` invalid id (the same ids `master.m3u8` rejects). Like every
route it is subject to the `STREMIO_ALLOWED_ORIGINS` Origin allowlist.
Re-creating the same id is refused until a deferred removal has finished.

HDR→SDR tone mapping (`STREMIO_TRANSCODE_TONEMAP`) applies only to sources
whose first video stream reports a PQ (`smpte2084`) or HLG (`arib-std-b67`)
transfer; SDR sources (including 10-bit SDR) are transcoded exactly as before.
Missing or unexpected primaries/matrix/range tags fall back to BT.2020
limited range.
The chain is `zscale` (linearise, and downscale when a max size is set) →
`tonemap` → `zscale` to BT.709, done in software before each encoder's usual
input (`format=yuv420p`, or `format=nv12,hwupload` for VAAPI), and the output
is tagged BT.709. On an 8-core Ryzen 7 7735HS with a 4K HEVC HDR10 source and
1080p libx264 output, a 4-second segment took roughly 1–1.5 s longer with
tone mapping than without; without a downscale (4K output) it runs about 2x
slower than real time here, so set a max width/height on low-power hardware.
Dolby Vision profile 5 (no HDR10/HLG base layer) is detected and left
untouched, since its colours are only correct after applying the DV
reshaping this chain does not do; profiles 7/8.1/8.4 go through the HDR10 or
HLG path via their base layer.
### Persistent HLS sessions

By default HLS sessions live in memory and in a fresh random
`stremio-hls-*` directory that is deleted on shutdown, so a restart loses
every transcoded segment. `STREMIO_HLS_PERSIST=1` (with
`STREMIO_HLS_WORK_DIR` set; otherwise a warning is logged and nothing
persists) keeps them in `<STREMIO_HLS_WORK_DIR>/stremio-hls-persist/<id>/`
with a `session.json` each, so a front end can pre-transcode a title hours
ahead and still have it after a restart, crash or update.

- **Raise `STREMIO_HLS_SESSION_TTL`.** The TTL still applies: with the
  default `60`, a session is evicted a minute after its last use and
  discarded by any restart longer than a minute, which makes persistence
  nearly useless. Set it to cover the gap between pre-transcoding and
  watching (e.g. `12h`), or set it to `0` to keep sessions until they're
  deleted. A warning is logged if it's left at the default.
- **On start**, each session is restored without re-probing and its
  existing segments are served from disk. Sessions idle past their TTL
  (a per-session `ttl` if they have one, otherwise
  `STREMIO_HLS_SESSION_TTL`, and never when that is `0`),
  unreadable or invalid ones, and ones whose segments no longer match the
  current process-wide encoder settings (`STREMIO_TRANSCODE_X264_CRF`,
  `_VAAPI_QP`, `_AUDIO_BITRATE`, `_AUDIO_CHANNELS`) are deleted, as are
  partial `*.tmp*` files; beyond `STREMIO_HLS_MAX_SESSIONS`, the least
  recently used are deleted. A session keeps its own bitrate/size/preset
  snapshot, so later `/settings` or env changes to those only affect new
  sessions, exactly as without persistence.
- **One process per directory.** The directory is locked exclusively (flock
  on POSIX, an exclusive open on Windows); a second server pointed at the
  same `STREMIO_HLS_WORK_DIR` warns and runs without persistence.
- **Security.** `session.json` holds the media URL, which can embed
  credentials (e.g. debrid links). It's written `0600`, and the persist
  directory and session directories are `0700`. On POSIX an existing
  `stremio-hls-persist` that isn't owned by the server's user, or is a
  symlink, is refused (the server falls back to non-persistent mode); one
  that is ours but more open than `0700` is tightened to `0700`. Use a
  dedicated `STREMIO_HLS_WORK_DIR` owned by the server's user, not a shared
  directory such as `/tmp`.
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

### Library mode (`libstremio-server.so`)

Android 10+ denies `exec()` of files an app downloaded into its own data
directory (SELinux W^X for targetSdk ≥ 29, i.e. Kodi 19+), but still allows
`dlopen()` of them. `cmd/libstremio` therefore builds the same server with
`-buildmode=c-shared` for `android/{arm64,armv7}` (`make lib-android`; shipped
inside the Android release archives next to the executable). Exports:

```c
int   ServerStart(char* logPath, char* envJSON); /* blocks until stopped; 0 ok, 1 init error, 2 already running */
int   ServerStop(void);                          /* graceful, safe when idle */
char* ServerVersion(void);                       /* static, do not free */
```

`envJSON` is an object of the same environment variables the executable reads.
Start may be called again after Stop. The library never installs signal
handlers or calls `os.Exit`; Go cannot unload it (golang/go#11100), so load it
once per process. `scripts/libstremio_smoke.py` exercises it through ctypes.

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
