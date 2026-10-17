<!--
SPDX-FileCopyrightText: 2026 The rivulet-engine Authors

SPDX-License-Identifier: MIT
-->

# DVR recording

An opt-in, MediaFlow/EasyProxy-compatible DVR. An addon with a DVR option and
this server as its EasyProxy URL adds a record entry per stream, a catalog of
recordings, and *Stop & Watch* / *DELETE* entries; they all call the endpoints
below, which mirror [EasyProxy](https://github.com/realbestia1/EasyProxy)
(`routes/recordings.py`, `services/recording_manager.py`).

```text
RIVULET_DVR_ENABLED=1
RIVULET_PROXY_PASSWORD=...      # strongly recommended: required as api_password when set
```

Opening `{server}/record?url=<stream>&name=<title>&duration=14400&api_password=<pw>`
starts an `ffmpeg -c copy` recorder in the background and answers `302` to the
live proxy URL (`/proxy/hls/manifest.m3u8?d=…`), so the player starts watching
while the recording runs. The recorder reads the *same* proxy URL on loopback,
so request headers (`h_*`), embed/extractor resolution (the `url` may be an embed
page matched by an `extractors.json` definition, or force one with `&extractor=`),
the SSRF guard and the upstream proxy (`RIVULET_PROXY_UPSTREAM`, `&proxy=`) apply
to the recording exactly as to playback. `url` may also be a `/proxy/…` URL of
this server. Repeating the request for a source that is already recording does not
start a second recorder.

| Endpoint | Behaviour |
|---|---|
| `GET /record?url=&name=&duration=` | start + `302` live; `400` bad url/duration, `429` over `RIVULET_DVR_MAX_ACTIVE`, `503` no `ffmpeg`, `507` store full |
| `GET /record/stop/{id}` | stop (finalises the file) and `302` to `/api/recordings/{id}/stream` |
| `GET /api/recordings[?status=]` | `{"recordings":[…],"active_count":n,"system_stats":{…}}` |
| `GET /api/recordings/active` | `{"recordings":[…active…]}` |
| `POST /api/recordings/start` | JSON `{url,name,duration,extractor,proxy}` → `201` recording (`200` if already recording) |
| `GET /api/recordings/{id}` | one recording, `404` if unknown |
| `POST /api/recordings/{id}/stop` | stop, returns the recording |
| `DELETE /api/recordings/{id}` · `GET …/{id}/delete` | delete file + metadata (the `GET` form is for players that can only open a URL); `DELETE /api/recordings[/all][?status=]` deletes many |
| `GET /api/recordings/{id}/stream` | `video/mp2t`; `Range` for finished recordings, live-follow while it records |
| `GET /api/recordings/{id}/download` | attachment, `Range` supported |

A recording is `{id, name, url, file_path, status, started_at, stopped_at,
duration_seconds, file_size_bytes, error_message, is_active, elapsed_seconds,
max_duration_seconds}`; `status` is `recording`, `completed` (source ended or the
duration cap was reached), `stopped` (user or shutdown) or `failed`.
`file_path` is the bare file name, `url` is the requested source with
`api_password`/`token` removed. Files are MPEG-TS (`<id>_<name>.ts`, growing and
playable while recording) with a `<id>.json` sidecar in `RIVULET_DVR_DIR`.

Notes:

- **Duration** is in seconds. Missing, empty, `0` or negative uses
  `RIVULET_DVR_DEFAULT_DURATION`; anything above `RIVULET_DVR_MAX_DURATION` is
  clamped; a non-number is `400`. Besides ffmpeg's `-t`, a wall-clock watchdog
  ends a recorder that stalls past the cap.
- **Names** are only a label: the file name is generated from the id plus the name
  reduced to letters, digits, `-`, `_` and spaces (≤ 50 characters), so a title can
  never address a path outside `RIVULET_DVR_DIR`.
- **Auth**: every DVR route requires `api_password` (query, or `X-Api-Password`)
  when `RIVULET_PROXY_PASSWORD` is set, and honours `RIVULET_PROXY_IP_ACL` (which
  must then include `127.0.0.1`, as the recorder reads the proxy on loopback).
  With neither set anyone who can reach the server can record and read - a warning
  is logged. `/record`, `/record/stop`, `…/delete` and all non-`GET` DVR requests
  are side-effecting routes: a browser-originated cross-site request without an
  allowed `Origin` is refused (`403`), even as `<video src>`. Playback
  (`…/stream`, `…/download`) and JSON reads are not blocked.
- **Shutdown** stops every active recorder gracefully (`q` to ffmpeg, kill after
  10 s) so partial files stay playable; a recording interrupted by a crash is
  marked `failed` on the next start (empty partial files are removed).
- Not implemented: the `/recordings` web UI, ClearKey (`key_id`/`key`) and
  `max_res` — addons using this API do not need them.
