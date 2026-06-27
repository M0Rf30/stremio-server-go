# Torznab Add-on

The `/torznab` add-on queries any [Torznab](https://torznab.github.io/spec-1.3-draft/)-compatible
indexer — [Prowlarr](https://github.com/Prowlarr/Prowlarr),
[Jackett](https://github.com/Jackett/Jackett), [NZBHydra2](https://github.com/theotherp/nzbhydra2),
or [Bitmagnet's built-in `/torznab` endpoint](https://bitmagnet.io) — and returns torrent
streams. Playback runs through the same DHT engine used by all other add-ons.

It **complements** the `/bitmagnet` add-on: `/bitmagnet` speaks Bitmagnet's own GraphQL API,
while `/torznab` speaks the universal Torznab standard and can point at any compatible indexer.
Unlike `/bitmagnet`, the Torznab add-on searches by IMDB id directly (no title-resolution step),
so it usually needs no Cinemeta lookup for movies and series.

---

## Configure

| Variable | Default | Purpose |
|---|---|---|
| `STREMIO_TORZNAB_URL` | _(unset)_ | Torznab indexer API base URL. Unset = add-on is inert. |
| `STREMIO_TORZNAB_APIKEY` | _(unset)_ | API key sent with every request. Required by Prowlarr and Jackett; not needed for Bitmagnet. |

Example base URLs:

| Indexer | Base URL | Key required |
|---|---|---|
| Bitmagnet | `http://localhost:3333/torznab` | No |
| Prowlarr | `http://<host>:9696/<indexer-id>/api` | Yes |
| Jackett | `http://<host>:9117/api/v2.0/indexers/<id>/results/torznab/api` | Yes |

Start the server with the env vars set:

```sh
STREMIO_TORZNAB_URL=http://localhost:3333/torznab ./stremio-server
```

For Prowlarr with an API key:

```sh
STREMIO_TORZNAB_URL=http://192.168.1.10:9696/42/api \
STREMIO_TORZNAB_APIKEY=abc123def456 \
./stremio-server
```

If `STREMIO_TORZNAB_URL` is unset, the `/torznab` routes serve the manifest but return no streams.

---

## Install in Stremio

Open the Stremio app, go to **Addons**, and paste:

```text
http://<server-host>:11470/torznab/manifest.json
```

Replace `<server-host>` with the host running `stremio-server-go` (e.g. `127.0.0.1` for a
local setup).

---

## How it works

For each stream request Stremio sends an IMDB `tt` id and, for series, a season and episode
number. The add-on maps these directly to Torznab queries:

| Content type | Torznab query |
|---|---|
| Movie | `t=movie&imdbid=<id>&cat=2000` |
| Series episode | `t=tvsearch&imdbid=<id>&season=<S>&ep=<E>&cat=5000` |

The numeric IMDB id (digits only, `tt` prefix stripped) is sent. The configured API key is
appended as `&apikey=<key>` when set.

The response is RSS/XML with `<torznab:attr>` extensions. For each `<item>` the add-on:

1. Resolves the infohash from the `infohash` torznab attribute; if absent, parses
   `urn:btih:<HASH>` from the magnet URI in the `<enclosure>` or the `magneturl` attribute.
   Base32-encoded hashes (32 chars) are decoded to hex; 40-char hex is normalized to lowercase.
   Items with no resolvable infohash are skipped.
2. Reads seeders from the `seeders` attribute (defaults to 0 if absent) and sorts results
   highest-seeder-first.
3. Returns the results as infoHash streams. The Stremio client hands the infoHash back to
   the same `stremio-server-go` instance, which streams via its DHT engine.

If the IMDB id query returns no results, the add-on falls back to a keyword search
(`t=search&q=<title>`) using the title resolved from
[Cinemeta](https://v3-cinemeta.strem.io) (6-hour cache).

> The metadata source is the Cinemeta-compatible add-on at `STREMIO_METADATA_URL`
> (default `https://v3-cinemeta.strem.io`; set to empty/`off` to disable). Since
> Torznab searches by IMDB id first, disabling it only removes the title-search
> fallback.

---

## Caveats

- **Infohash required.** Each result must carry a magnet URI or an `infohash` attribute.
  Results that only provide a `.torrent` download URL are skipped; the engine streams
  by infoHash, not by file URL.
- **Coverage depends on your indexer.** The add-on is only as good as the configured
  indexer's index. Prowlarr is recommended because it aggregates many indexers behind a
  single Torznab URL.
- **Unauthenticated routes.** `/torznab/*` routes require no authentication (Stremio
  add-ons must be reachable by the client without credentials). They return stream
  metadata only and do not relay content.
- **Indexer reachability.** The server must be able to reach the Torznab endpoint at
  query time. Prowlarr/Jackett must themselves be able to reach their upstream indexers.
- **Not a guarantee of results.** An indexer that has not yet indexed a title, or one
  whose upstream trackers are down, returns an empty result set. This is not an error.
