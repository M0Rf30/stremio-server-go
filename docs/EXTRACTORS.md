<!--
SPDX-FileCopyrightText: 2026 The rivulet-engine Authors

SPDX-License-Identifier: MIT
-->

# Extractors

`/extractor/video` resolves a page into a stream using definitions that live
outside the binary, and `/proxy/stream`, `/proxy/hls/manifest.m3u8` and
`/proxy/mpd/manifest.m3u8` can use them to resolve embed pages (see `match`
below). The server ships with none; unknown `host=` values return `400`.

**Sources** (local entries override remote ones with the same name):

- `$APP_PATH/extractors.json` (or `RIVULET_EXTRACTORS_FILE`): checked for
  changes every few seconds; an invalid edit is logged and the previous good set
  stays active.
- `RIVULET_EXTRACTORS_URL`: fetched at startup and every 6h together with a
  detached ed25519 signature at `<URL>.sig` (raw 64 bytes, hex or base64). It is
  ignored unless `RIVULET_EXTRACTORS_PUBKEY` is set and the signature verifies.
  The last verified copy is cached under `APP_PATH`. Like `RIVULET_TRACKERS_URL`,
  this URL is operator-configured, so it may point at a LAN/localhost copy; the
  signature, not the address, is what makes it trusted.

**Signed remote list:** host `extractors.json` plus a hex `extractors.json.sig`
somewhere reachable and opt in by setting

```sh
RIVULET_EXTRACTORS_URL=https://definitions.example/extractors.json
RIVULET_EXTRACTORS_PUBKEY=<ed25519 public key, hex or base64>
```

Nothing is fetched unless both are set; a list whose signature does not verify
is ignored.

**Format** (`version` must be `1`; unknown fields are rejected):

```json
{
  "version": 1,
  "extractors": {
    "example": {
      "steps": [
        {"if_path": "^/(movie|tv)/", "fetch": "{origin}/api{path}", "json": "src", "set": "embed", "optional": true},
        {"fetch": "{embed}", "headers": {"Referer": "{input}"}},
        {"if_url": "/iframe", "regex": "<iframe[^>]+src=\"([^\"]+)", "transform": ["html_unescape"], "follow": true},
        {"regex": "'token'\\s*:\\s*'(\\w+)'", "set": "token"},
        {"regex": "url\\s*:\\s*'(https?://[^']+)'", "set": "playlist"},
        {"regex": "canPlayFHD\\s*=\\s*true", "set": "fhd", "optional": true}
      ],
      "result": {
        "url": "{playlist}",
        "query": {"token": "{token}", "h": "{fhd?1}"},
        "headers": {"Referer": "{page_origin}/"},
        "endpoint": "hls"
      }
    }
  }
}
```

| Step field | Meaning |
|---|---|
| `fetch` + `headers` | request a URL (resolved against the current page); GET unless `method` says otherwise. Alone, the response becomes the current page; with `json`/`regex`, only the captured value is kept |
| `method` | `GET` (default) or `POST`; needs `fetch` |
| `body` | any JSON value, sent by a `POST` step as `application/json` (unless `headers` sets `Content-Type`). Every string leaf is a template (`{var}`, `{var?text}`) expanded before serialization, so substituted values are JSON-escaped automatically; object keys stay literal. A `body` without `"method": "POST"` is rejected; at most 64 KB serialized, 16 levels deep |
| `json` | dot path (`a.0.b`) into the fetched body or `from` variable |
| `regex` | RE2 pattern on the current page or `from` variable; group 1 (or the whole match) is captured |
| `value` | literal template, stored in `set` |
| `transform` | `base64`, `html_unescape`, `url_unescape`, `unpack_js` (p,a,c,k,e,d), `reverse`, `trim`, `scheme_https` |
| `set` | variable that receives the value |
| `follow` | fetch the captured URL and make it the current page |
| `if_path` / `if_url` | run the step only if the input path / current page URL matches |
| `optional` | a failure skips the step instead of failing the request |

Templates use `{var}` or `{var?text}` (`text` if `var` is non-empty). Built-in
variables: `input`, `origin`, `host`, `path`, `query` (of the input URL), `url`
and `page_origin` (current page), `body`. `result.endpoint` is `stream`, `hls`
or `mpd`; empty query/header values are dropped.

**Embed pages on `/proxy/stream`, `/proxy/hls` and `/proxy/mpd`:** a definition
may also set `match`, an RE2 regex on the destination (`d`) of those endpoints.
A matching destination is treated as an embed page (not media) and resolved
with that definition first, as EasyProxy does, then served on the result's
`endpoint` with the definition's headers (caller `h_` headers take precedence):
when that is the endpoint that received the request (for example an
`/proxy/hls/manifest.m3u8` request for a definition whose result endpoint is
`hls`) the playlist is served directly; otherwise the request is redirected
(302) to the proxy URL of the result endpoint, with the same auth/signing
parameters. Resolutions are cached for 10 minutes so a player's Range or
playlist-refresh requests don't refetch the page; a page that can't be
resolved returns `502` instead of its HTML. This is how a MediaFlow-style link
such as `/proxy/hls/manifest.m3u8?d=<hoster play URL>` plays when a definition
matches it.

```json
"embedhost": {
  "match": "^https://embed\\.example/e/",
  "steps": [{"fetch": "{input}"}, {"regex": "src:\\s*\"([^\"]+)\"", "set": "src"}],
  "result": {"url": "{src}", "headers": {"Referer": "{input}"}, "endpoint": "stream"}
}
```

A multi-step API hop with a POST (the response is JSON, captured by `json`):

```json
"apihost": {
  "match": "^https://api\\.example/play/",
  "steps": [
    {"fetch": "https://api.example/resolve", "method": "POST",
     "headers": {"X-Client": "web"},
     "body": {"url": "{input}", "lang": "it"},
     "json": "0.url", "set": "src"}
  ],
  "result": {"url": "{src}", "endpoint": "hls"}
}
```

Limits: 32 steps, 8 fetches and 20 s per request, 4 MB per page, 1 MB per
definitions file. Every fetch and the resolved URL go through the same
private-address checks as `/proxy`. There is no scripting, so pages that need a
JavaScript runtime or anti-bot challenges cannot be described.
