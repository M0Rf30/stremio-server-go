<!--
SPDX-FileCopyrightText: 2026 The rivulet-engine Authors

SPDX-License-Identifier: MIT
-->

# BitTorrent tuning

## Censorship resistance

The server uses DHT (BEP32), PEX (BEP11), HTTP webseeds, and a ranked public
tracker list, so peer discovery continues even when individual trackers are
blocked. Four knobs let you harden further in censored or monitored networks:

- **`RIVULET_BT_ENCRYPTION=require`** — forces RC4 header obfuscation on every
  peer handshake, defeating DPI signatures that fingerprint plain BitTorrent
  traffic. Peers that refuse encryption are dropped.
- **`RIVULET_BT_PROXY`** — routes tracker announces, metainfo fetches, HTTP
  webseeds, and the tracker-list download through a SOCKS5 or HTTP proxy
  (e.g. `socks5://127.0.0.1:9050` for Tor). Use this when your ISP blocks
  trackers or the tracker-list host (`raw.githubusercontent.com`).
- **`RIVULET_DHT_BOOTSTRAP`** — supplies alternate DHT bootstrap nodes when the
  default routers (`router.bittorrent.com`, `dht.transmissionbt.com`, etc.) are
  filtered; accepts a comma-separated `host:port` list appended to the defaults.
- **`RIVULET_BT_ANONYMOUS=1`** — suppresses the client version string advertised
  to peers, reducing passive fingerprintability.

Example — running behind Tor:

```sh
RIVULET_BT_ENCRYPTION=require \
RIVULET_BT_PROXY=socks5://127.0.0.1:9050 \
RIVULET_BT_ANONYMOUS=1 \
rivulet-engine
```

**Limitation:** the proxy covers tracker/webseed/metainfo traffic only. Peer
TCP/uTP connections are established through the listen socket directly and are
not tunneled. For full peer anonymity a network-level VPN or transparent Tor
proxy is still required; the proxy + encryption knobs primarily defeat
tracker-level blocking and DPI-based fingerprinting of peer handshakes.

## Playback start priming and BitTorrent speed limits

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
  `RIVULET_PEERS_PER_TORRENT` budget once resumed). Both settings are
  evaluated per torrent on the same 5 s tick that applies the bandwidth
  limiters.
