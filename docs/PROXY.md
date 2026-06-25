# Stream Proxy

`stremio-server-go` ships a pure-Go HTTP stream proxy that augments the legacy
`/proxy` byte-forwarder (kept for stremio-web) with:

- **HLS (M3U8) manifest rewriting** — variant/media playlists, segments,
  `EXT-X-KEY`, `EXT-X-MAP`, and `EXT-X-MEDIA`/`EXT-X-I-FRAME-STREAM-INF` URIs are
  rewritten back through the proxy, with request/response header propagation.
- **MPEG-DASH (MPD) manifest rewriting** — `BaseURL`, `SegmentTemplate`
  (`$Number$`/`$RepresentationID$`/`$Time$` placeholders preserved), `SegmentURL`,
  and `SegmentBase` initialization URLs are routed through the proxy.
- **On-the-fly decryption** — HLS `AES-128` (full-segment CBC) and CENC
  (`SAMPLE-AES`/`cenc`/`cbcs`, per-sample AES-CTR/CBC over fMP4) when key
  material is supplied.
- **Signed, expiring URLs**, an optional **API password**, and an **IP
  allowlist** so the proxy can be exposed publicly.
- **Segment caching + prefetch** for smoother playback.

CORS is already permissive server-wide (`Access-Control-Allow-Origin: *`), so
proxied streams play in browser clients without extra configuration.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/proxy/stream` | Generic stream proxy with `Range`/seeking; decrypts when key params are present. The segment workhorse. |
| `GET` | `/proxy/hls/manifest.m3u8` | Fetch + rewrite an HLS playlist. |
| `GET` | `/proxy/mpd/manifest.m3u8` | Fetch + rewrite a DASH MPD. |
| `POST` | `/generate_url` | Mint a signed, expiring proxy URL. |
| `GET` | `/base64/encode` / `/base64/check` | Encode/inspect a destination URL. |
| `GET` | `/proxy/<opts>/<path>` | Legacy stremio-web forwarder (unchanged). |

## Query parameters

| Param | Meaning |
|---|---|
| `d` | Destination URL. Plain or base64url/base64-std (auto-detected); `+` decodes to space. |
| `h_<Name>` | Header sent to the upstream (e.g. `h_User-Agent=VLC`, `h_Referer=...`). |
| `r_<Name>` | Header forced on the response (`Access-Control-*` is ignored). |
| `api_password` | Required only when `STREMIO_PROXY_PASSWORD` is set. |
| `token` | A signed token from `/generate_url` (alternative to `api_password`). |
| `method`, `key`, `key_id`, `iv` | Decryption parameters (hex or base64). `method` is `AES-128`, `SAMPLE-AES`, `cenc`, or `cbcs`. |

Manifest endpoints propagate `d`/`h_`/`r_`/`api_password` onto every rewritten
child URL, so nested playlists, segments, and keys stay authorized and
header-injected automatically.

## Decryption

- **HLS AES-128**: pass `method=AES-128&key=<hex|b64>&iv=<hex|b64>` on the
  `/proxy/stream` segment URL. The whole segment is AES-128-CBC decrypted and
  PKCS7-unpadded.
- **CENC (DASH / SAMPLE-AES)**: pass `method=cenc&key=<16-byte key>` (optionally
  `key_id`). Encrypted sample ranges inside the fMP4 (`moof`/`traf`/`senc`/
  `trun` + `mdat`) are decrypted with AES-CTR (or AES-CBC for `cbcs`), honoring
  clear/encrypted subsample spans.

Decryption is server-side and fully buffered per segment, so decrypted segments
are returned as a single `200` body (no `Range`/`206`).

## Authentication & access control

All three modes are optional and composable:

- **API password** (`STREMIO_PROXY_PASSWORD`) — clients must send a matching
  `api_password` (constant-time compared).
- **Signed URLs** — `POST /generate_url` with
  `{"endpoint":"/proxy/stream","params":{"d":"<url>"},"expiry_seconds":3600,"ip":"<optional client ip>"}`
  returns `{"url":"...?token=<tok>","expires_at":<unix>}`. The token is an
  AES-GCM sealed blob (signed with `STREMIO_PROXY_SECRET`) carrying the params,
  expiry, and optional pinned client IP. A valid `token` bypasses the password.
- **IP allowlist** (`STREMIO_PROXY_IP_ACL`) — comma-separated CIDRs; non-matching
  clients get `403`. The client IP honors `X-Forwarded-For` (first hop).

When nothing is configured, the proxy is open (consistent with the server's
localhost-trust model).

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `STREMIO_PROXY_PASSWORD` | _(unset)_ | `api_password` required on proxy requests. Unset = no password. |
| `STREMIO_PROXY_SECRET` | _(auto)_ | Key for signed URLs (hex/base64). Auto-generated and persisted at `<APP_PATH>/proxy-secret` if unset. |
| `STREMIO_PROXY_IP_ACL` | _(unset)_ | Comma-separated CIDR allowlist. Unset = allow all. |
| `STREMIO_PROXY_PREBUFFER` | `3` | Number of upcoming segments to prefetch. `0` disables. |
| `STREMIO_PROXY_SEG_CACHE_TTL` | `300` | Segment cache TTL in seconds. `0` disables caching. |
| `STREMIO_PROXY_PUBLIC_URL` | _(derive)_ | External base URL written into rewritten manifests. Unset = derived from the request (`X-Forwarded-Proto`/`Host`). |

`STREMIO_PROXY_PUBLIC_URL` matters behind a reverse proxy or on a hosted
platform (e.g. a HuggingFace Space): set it to the externally reachable origin
so rewritten manifest URLs point back at a host the player can actually reach.

## Examples

```sh
# Proxy a stream with a custom User-Agent
curl "http://127.0.0.1:11470/proxy/stream?d=https://cdn.example/video.mp4&h_User-Agent=VLC"

# Rewrite an HLS master playlist (every child URL comes back proxied)
curl "http://127.0.0.1:11470/proxy/hls/manifest.m3u8?d=https://cdn.example/live/master.m3u8"

# Base64-encode a destination for safe embedding
curl "http://127.0.0.1:11470/base64/encode?d=https://cdn.example/path?a=1&b=2"

# Mint a signed, 1-hour URL
curl -X POST http://127.0.0.1:11470/generate_url \
  -H 'Content-Type: application/json' \
  -d '{"endpoint":"/proxy/stream","params":{"d":"https://cdn.example/video.mp4"},"expiry_seconds":3600}'
```

## Security

As with the rest of the server, this is a **localhost-trust** service by
default. If you expose the proxy to a network, set `STREMIO_PROXY_PASSWORD`
(and/or `STREMIO_PROXY_IP_ACL`) and prefer signed URLs — otherwise it is an
open relay that will fetch and decrypt arbitrary URLs on behalf of any caller.
Respect copyright law and the terms of service of any host you deploy on.
