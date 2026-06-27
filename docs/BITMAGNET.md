# Bitmagnet Add-on

The `/bitmagnet` add-on turns a self-hosted [Bitmagnet](https://bitmagnet.io)
DHT index into Stremio streams. It is a build-your-own index, not a
Torrentio drop-in.

The chain from crawl to playback:

1. **Bitmagnet** — crawls the BitTorrent DHT, classifies torrents against
   TMDB/IMDB metadata, and exposes a GraphQL API.
2. **/bitmagnet add-on** — resolves a Stremio `tt` (IMDB) id to a title and
   year via Cinemeta, queries Bitmagnet by title, filters by content type and
   season/episode, and returns infoHash streams.
3. **stremio-server-go engine** — receives the infoHash from the Stremio client
   and streams it via its DHT peer engine.

---

## Run Bitmagnet locally

A compose file is provided at `deploy/bitmagnet/compose.yaml`. It runs:

- `ghcr.io/bitmagnet-io/bitmagnet:latest` — WebUI and GraphQL on port 3333, DHT
  on port 3334 (TCP + UDP)
- `postgres:16-alpine` — database backend

**Start:**

```sh
podman-compose -f deploy/bitmagnet/compose.yaml up -d
```

`podman-compose` (the Python tool) does not require the Podman API socket.
The `podman compose` plugin does — if you use that variant, enable the socket
first:

```sh
systemctl --user enable --now podman.socket
```

After the containers start, Bitmagnet runs database migrations (through v19)
and launches its workers (`http_server`, `queue_server`, `dht_crawler`). The
WebUI and GraphQL API are available at:

| Endpoint | URL |
|---|---|
| WebUI | `http://localhost:3333` |
| GraphQL / GraphiQL | `http://localhost:3333/graphql` |

`GET /status` may return `503` on a fresh instance; this is harmless. The
GraphQL API is up and the add-on uses only that endpoint.

**TMDB classification:** Bitmagnet ships with a default TMDB API key
rate-limited to 1 request/second. For real classification throughput, set your
own key by uncommenting `TMDB_API_KEY` in the compose env section. Without
classification, `tt`/IMDB matching does not work and the add-on returns no
streams.

**Stop:**

```sh
podman-compose -f deploy/bitmagnet/compose.yaml down
```

Named volumes `bitmagnet-config` and `bitmagnet-pgdata` persist across `down`
(the crawl index and database are preserved). To wipe them entirely:

```sh
# Confirm exact names on your system first:
podman volume ls

podman volume rm stremio-server-go_bitmagnet-pgdata stremio-server-go_bitmagnet-config
```

---

## Wire stremio-server-go

Set `STREMIO_BITMAGNET_URL` to the Bitmagnet GraphQL endpoint before starting
the server:

```sh
STREMIO_BITMAGNET_URL=http://localhost:3333/graphql ./stremio-server
```

If the variable is unset, the `/bitmagnet` routes serve the manifest but return
no streams (the add-on is inert).

**Install in Stremio** — open the app, go to **Addons**, and paste:

```text
http://<server-host>:11470/bitmagnet/manifest.json
```

Replace `<server-host>` with the host running `stremio-server-go` (e.g.
`127.0.0.1` for a local setup).

---

## Using it

When Stremio requests streams for a title:

1. The add-on resolves the `tt` id to a title and year via
   [Cinemeta](https://v3-cinemeta.strem.io) (6-hour cache).
2. It queries the local Bitmagnet GraphQL API by title, filtered by content
   type and — for series — season and episode number.
3. Results are sorted by seeders and returned as infoHash streams. No file
   index is set; the engine selects the best matching file.
4. The Stremio client hands the infoHash back to the same server, which streams
   via its DHT engine.

> **Metadata source / privacy.** Step 1 calls a Cinemeta-compatible metadata
> add-on, set by `STREMIO_METADATA_URL` (default `https://v3-cinemeta.strem.io`).
> Point it at a self-hosted Cinemeta mirror or a TMDB/aiometadata add-on's
> configured base, or set it to empty/`off` to disable the lookup — the add-on
> then queries Bitmagnet by the raw IMDB id (lower match quality).

A fresh Bitmagnet instance returns no results until the DHT crawler has
discovered and classified matching content. Coverage improves as the crawler
runs continuously.

---

## Caveats and limits

- **DHT is not keyword-searchable.** The BitTorrent DHT resolves infoHash to
  peers; it does not support text queries. Bitmagnet pre-crawls the DHT and
  builds a local relational index. You must let the crawler run to populate it.
- **Coverage is probabilistic and grows slowly.** The crawler discovers
  torrents as they surface on the DHT; rare or old content may never appear.
  Run the service continuously for best coverage.
- **Always-on host required.** The DHT crawler needs a stable host running
  24/7, disk space for the Postgres index, and an open UDP port (3334) for DHT
  traffic. This is not suitable for HuggingFace Spaces or other ephemeral
  hosts.
- **Classification required.** The add-on matches by TMDB/IMDB metadata.
  Torrents not yet classified by Bitmagnet are invisible to the add-on. The
  default TMDB key is rate-limited to 1 req/s; supply your own for practical
  throughput.
- **Unauthenticated routes.** `/bitmagnet/*` routes require no authentication
  (Stremio add-ons must be reachable by the client without credentials). They
  return stream metadata only and do not relay content.
- **Not a day-one Torrentio replacement.** The add-on complements existing
  add-ons and improves as the crawl matures.
