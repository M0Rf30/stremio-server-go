<!--
SPDX-FileCopyrightText: 2026 The rivulet-engine Authors

SPDX-License-Identifier: MIT
-->

# HLS transcoding

On-demand `ffmpeg` transcoding behind `/hlsv2`. Settings live in [Configuration](CONFIGURATION.md); this page covers behaviour, per-session overrides and persistence.

The HLS master playlist's `BANDWIDTH` and video `CODECS` (H.264 profile-level)
are derived from the effective bitrate cap and output resolution — the variant's
peak for `BANDWIDTH` (the `RIVULET_TRANSCODE_MAXRATE`/`transcodeMaxBitRate`
bits/second value plus `RIVULET_TRANSCODE_AUDIO_BITRATE`), and the output
width×height mapped to the matching H.264 level (≤1280×720 → Level 3.1,
≤1920×1080 → Level 4.1, ≤2560×1440 → Level 5.0, else Level 5.1). An unconfigured
server (no bitrate/resolution knob touched) keeps advertising the historical
fixed `BANDWIDTH=4000000`/`avc1.640029` regardless of actual source resolution,
so upgrading never changes existing playback.
A per-session quality override (below) always counts as "configured" and
takes the derived path, even when its values equal the defaults.

## Per-session HLS overrides

With `RIVULET_HLS_SESSION_OVERRIDES=1`, a front end serving several
independent sessions can give each its own quality and lifetime, e.g. one
watch-party room at 5 Mbps for a friend on a slow link and another at
12 Mbps, or a title prepared hours ahead of a scheduled party with a longer
idle TTL than casual sessions. The request that creates a session may add:

| Parameter | Meaning | Accepted values |
|---|---|---|
| `ttl` | idle-eviction TTL for this session | whole seconds or a Go duration (`3600`, `90m`), from `60s` (the stock default, so an override never makes a session less durable) to `30 days` |
| `maxWidth`, `maxHeight` | downscale caps, same rules as `RIVULET_TRANSCODE_MAX_WIDTH`/`_HEIGHT` (aspect-preserving, never upscales) | `16`–`7680`, rounded down to even |
| `bitrate` | video target (`-b:v`); unless given explicitly, `maxRate` = `bitrate` and `bufSize` = 2× `bitrate` | ffmpeg syntax, decimals allowed as for the `RIVULET_TRANSCODE_*` env knobs (`6M`, `1.5M`, `800k`), `100k`–`200M` |
| `maxRate`, `bufSize` | explicit `-maxrate` / `-bufsize`; when `bitrate` and `maxRate` are both given, `maxRate` must be at least `bitrate` | as `bitrate`; `bufSize` up to `400M` |

```text
GET /hlsv2/room-a/master.m3u8?mediaURL=…&bitrate=5M&maxHeight=720&ttl=4h
```

- **Precedence:** per-session override > `/settings` (`transcodeMaxBitRate`,
  `transcodeMaxWidth`) > env. Knobs not overridden keep their `/settings`/env
  value; the master playlist `BANDWIDTH`/`CODECS` follow the effective values.
  A per-session `ttl` beats `RIVULET_HLS_SESSION_TTL=0`
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
  `RIVULET_HLS_MAX_SESSIONS` slots and disk for days. Enable it only when
  every client that can reach the server is trusted.
- **Existing sessions:** a session's transcode settings are frozen when it is
  created, so quality parameters on later `master.m3u8` requests for the same
  id are ignored. `ttl` is the exception: it replaces the session's TTL, so a
  caller can extend (or shorten, never below `60s`) a session's lifetime.
- **TTL granularity:** idle sessions are reaped on each
  `RIVULET_HLS_REAPER_INTERVAL` tick, so a session is removed between `ttl`
  and `ttl` + one reaper interval (≤30s by default) after its last access. A
  long-TTL session keeps its slot in `RIVULET_HLS_MAX_SESSIONS` (and its
  segment cache on disk) until then.

`DELETE /hlsv2/{id}` ends an HLS transcode session explicitly, for front ends
that manage their own session lifecycle rather than relying on the idle
reaper (and the only way to end one when `RIVULET_HLS_SESSION_TTL=0`). The
session is unregistered at once, so later requests for it get `unknown
session`, and its segment cache is removed from disk as soon as no request
for it is still being served. Responses: `204` deleted, `404` unknown
session, `400` invalid id (the same ids `master.m3u8` rejects). Like every
route it is subject to the `RIVULET_ALLOWED_ORIGINS` Origin allowlist.
Re-creating the same id is refused until a deferred removal has finished.

`GET /hlsv2/{id}/{file}` answers `404` for an unknown session, a segment that
is missing or past the end, and any other transcode failure. When the source
fails while a segment is transcoded, the segment is answered `502` (for
example, the origin answered `429`), or `504` when the origin timed out or the
segment ran past its time limit (`RIVULET_HLS_SEGMENT_TIMEOUT`, doubled when a
hardware attempt precedes the software fallback). The reason is in the body.
Before answering, the server makes its own transcode attempts: a software
fallback after a failed hardware attempt, and at most one retry with a longer
input seek. A client should back off before it requests the segment again.

HDR→SDR tone mapping (`RIVULET_TRANSCODE_TONEMAP`) applies only to sources
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

## Persistent HLS sessions

By default HLS sessions live in memory and in a fresh random
`rivulet-engine-hls-*` directory that is deleted on shutdown, so a restart loses
every transcoded segment. `RIVULET_HLS_PERSIST=1` (with
`RIVULET_HLS_WORK_DIR` set; otherwise a warning is logged and nothing
persists) keeps them in `<RIVULET_HLS_WORK_DIR>/rivulet-engine-hls-persist/<id>/`
with a `session.json` each, so a front end can pre-transcode a title hours
ahead and still have it after a restart, crash or update.

- **Raise `RIVULET_HLS_SESSION_TTL`.** The TTL still applies: with the
  default `60`, a session is evicted a minute after its last use and
  discarded by any restart longer than a minute, which makes persistence
  nearly useless. Set it to cover the gap between pre-transcoding and
  watching (e.g. `12h`), or set it to `0` to keep sessions until they're
  deleted. A warning is logged if it's left at the default.
- **On start**, each session is restored without re-probing and its
  existing segments are served from disk. Sessions idle past their TTL
  (a per-session `ttl` if they have one, otherwise
  `RIVULET_HLS_SESSION_TTL`, and never when that is `0`),
  unreadable or invalid ones, and ones whose segments no longer match the
  current process-wide encoder settings (`RIVULET_TRANSCODE_X264_CRF`,
  `_VAAPI_QP`, `_AUDIO_BITRATE`, `_AUDIO_CHANNELS`) are deleted, as are
  partial `*.tmp*` files; beyond `RIVULET_HLS_MAX_SESSIONS`, the least
  recently used are deleted. A session keeps its own bitrate/size/preset
  snapshot, so later `/settings` or env changes to those only affect new
  sessions, exactly as without persistence.
- **One process per directory.** The directory is locked exclusively (flock
  on POSIX, an exclusive open on Windows); a second server pointed at the
  same `RIVULET_HLS_WORK_DIR` warns and runs without persistence.
- **Security.** `session.json` holds the media URL, which can embed
  credentials (e.g. debrid links). It's written `0600`, and the persist
  directory and session directories are `0700`. On POSIX an existing
  `rivulet-engine-hls-persist` that isn't owned by the server's user, or is a
  symlink, is refused (the server falls back to non-persistent mode); one
  that is ours but more open than `0700` is tightened to `0700`. Use a
  dedicated `RIVULET_HLS_WORK_DIR` owned by the server's user, not a shared
  directory such as `/tmp`.
