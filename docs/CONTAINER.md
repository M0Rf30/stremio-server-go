# Container Deployment

The project ships a multi-stage `Dockerfile` (Podman/Docker) that compiles
the server from source in a Go 1.24 Alpine build stage, then produces a lean
Alpine 3.20 runtime image with `ffmpeg`, `ffprobe`, and `yt-dlp` bundled.  The
container runs as non-root UID 1000 and stores all persistent data (cache,
`server-settings.json`, TLS material) under `/data`.

---

## Pull the prebuilt image

CI publishes multi-arch images (`linux/amd64`, `linux/arm64`) to the GitHub
Container Registry on every push to `main` and every `v*.*.*` tag, so you can
skip the local **Build** section below:

```sh
podman pull ghcr.io/m0rf30/stremio-server-go:latest   # or a tag, e.g. :v0.1.0
```

Available tags: `latest` (default branch), semver (`vX.Y.Z` and `X.Y`), the
branch name, and `sha-<short>`. Replace `podman` with `docker` as needed.

---

## Build

### Podman

```sh
podman build -t stremio-server-go .
```

### Docker

```sh
docker build -t stremio-server-go .
```

### Optional build args

Embed version metadata at build time:

```sh
podman build -t stremio-server-go \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  .
```

Replace `podman` with `docker` as needed.

---

## Run (standalone)

### Podman

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -v stremio-data:/data \
  stremio-server-go
```

Point any Stremio client's **streaming server URL** at `http://HOST:11470`.

#### Pinning a BitTorrent peer port (inbound peering)

By default `BT_LISTEN_PORT=0` lets the OS assign a port.  To allow inbound
peers (better swarm participation, especially for seeding), pin a fixed port and
publish it on both TCP and UDP:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -p 11471:11471/tcp \
  -p 11471:11471/udp \
  -e BT_LISTEN_PORT=11471 \
  -v stremio-data:/data \
  stremio-server-go
```

#### Re-enabling self-signed HTTPS

`HTTPS_PORT` defaults to `0` (disabled) in the image.  To serve self-signed
HTTPS inside the container:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -p 12470:12470 \
  -e HTTPS_PORT=12470 \
  -v stremio-data:/data \
  stremio-server-go
```

For a **browser-trusted** cert (the Stremio UI's "HTTPS endpoint for streaming"
`*.stremio.rocks` flow) rather than self-signed, see [HTTPS.md](HTTPS.md).

### Docker equivalents

Replace `podman` with `docker` in every command above; all flags are identical.

### Health check

The image defines a `HEALTHCHECK` that polls `/heartbeat`.  Docker, BuildKit,
and HuggingFace Spaces honour it automatically.  Podman builds OCI-format
images by default, which silently drop `HEALTHCHECK` (you will see a
`HEALTHCHECK is not supported for OCI image format` warning at build time).  To
retain it under Podman, build with the Docker image format:

```sh
podman build --format docker -t stremio-server-go .
```

---

## Environment variables

| Variable | In-container default | Purpose |
|---|---|---|
| `HTTP_PORT` | `11470` | enginefs HTTP API port |
| `HTTPS_PORT` | `0` | self-signed / provisioned HTTPS port; `0` disables |
| `BT_LISTEN_PORT` | `0` | BitTorrent peer port; `0` = OS-assigned |
| `APP_PATH` | `/data` | data/cache root (settings, cert, HLS cache) |
| `WEB_UI_LOCATION` | `https://web.stremio.com/` | redirect target for `GET /` |
| `LOCAL_FILES_DIR` | _(unset)_ | directory scanned by the local-files addon; mount a volume and set this to enable |
| `STREMIO_HWACCEL` | _(auto)_ | `0` forces software libx264; or pin `vaapi` / `nvenc` / `qsv` / `videotoolbox` / `v4l2m2m` |
| `STREMIO_HTTP_LOG` | _(unset)_ | set to `1` to log each request path |
| `STREMIO_MEMORY_CACHE_SIZE` | `0` | in-RAM piece-cache budget in bytes; `0` writes pieces to `/data`. Set `>0` to never touch disk (ideal on ephemeral Spaces / low-disk hosts) |
| `STREMIO_MEM_LIMIT` | _(unset)_ | soft memory ceiling in bytes (`GOMEMLIMIT` also honoured); RSS high-water is returned to the OS every 5 min |
| `STREMIO_DISABLE_WEBTORRENT` | `1` (image) | WebTorrent/WebRTC (pion) peers are off by default in the image — cuts ~60% of goroutines & RAM; set `0` to re-enable |
| `STREMIO_DISABLE_TRACKERS` | _(unset)_ | `1` = DHT/PEX/webseeds only (no tracker announces); for private / DHT-only operation |
| `STREMIO_TRACKERS_URL` | _(curated list)_ | source URL for the public tracker list; empty / `off` skips the remote fetch (embedded + DHT/PEX only) |
| `STREMIO_BT_ENCRYPTION` | `prefer` | peer-connection encryption; `require` refuses plaintext peers (DPI evasion), `disable` turns obfuscation off |
| `STREMIO_BT_PROXY` | _(unset)_ | SOCKS5/HTTP proxy for tracker announces, webseeds, metainfo + tracker-list fetch (e.g. `socks5://127.0.0.1:9050`). Peer connections stay direct |
| `STREMIO_DHT_BOOTSTRAP` | _(defaults)_ | extra comma-separated `host:port` DHT bootstrap nodes, appended to the defaults, for filtered networks |
| `STREMIO_BT_ANONYMOUS` | _(unset)_ | `1` hides the client version/fingerprint advertised to peers |
| `STREMIO_PROXY_PASSWORD` | _(unset)_ | `api_password` required on `/proxy/*` (set as a **secret**) |
| `STREMIO_PROXY_SECRET` | _(auto)_ | signing key for signed proxy URLs; set explicitly on ephemeral hosts (an auto value changes on every restart) |
| `STREMIO_PROXY_PUBLIC_URL` | _(derive)_ | external base URL written into rewritten manifests — required behind an edge proxy (HF Spaces) |
| `STREMIO_PROXY_UPSTREAM` | _(unset)_ | outbound upstream proxy for `/proxy` fetches (`socks5`/`http`/`https`) |
| `STREMIO_BITMAGNET_URL` | _(unset)_ | Bitmagnet GraphQL endpoint → enables the `/bitmagnet` add-on |
| `STREMIO_TORZNAB_URL` | _(unset)_ | Torznab indexer base URL → enables the `/torznab` add-on |
| `STREMIO_TORZNAB_APIKEY` | _(unset)_ | API key for the Torznab indexer (required by Prowlarr / Jackett) |

---

## Example configurations

Curated `podman run` recipes (swap `podman`→`docker`; all flags are identical).
The full env-var reference lives in the project
[README](../README.md#environment).

### Ephemeral / low-disk (in-RAM piece cache)

Stream through a bounded RAM cache and never write piece data to disk — ideal on
a HuggingFace Space without persistent storage, or any low-disk host:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -e STREMIO_MEMORY_CACHE_SIZE=536870912 \
  -e STREMIO_MEM_LIMIT=1073741824 \
  stremio-server-go
```

512 MiB piece cache under a 1 GiB soft memory ceiling. Raise both for higher
bitrates; lower them on tightly constrained Spaces.

### Censorship-resistant (DPI evasion + proxy)

Force encrypted peer handshakes and route tracker/metadata traffic through a
SOCKS5 proxy (here a host-side Tor daemon) for networks that block trackers or
fingerprint BitTorrent via DPI:

```sh
podman run -d \
  --name stremio-server \
  --network host \
  -e STREMIO_BT_ENCRYPTION=require \
  -e STREMIO_BT_PROXY=socks5://127.0.0.1:9050 \
  -e STREMIO_BT_ANONYMOUS=1 \
  -v stremio-data:/data \
  stremio-server-go
```

`--network host` (or a shared pod) lets the container reach the host's Tor
SOCKS port. Supply `STREMIO_DHT_BOOTSTRAP=host:port,…` as well when the default
DHT routers are filtered. Peer connections themselves are not tunneled — see the
[censorship-resistance notes](../README.md#censorship-resistance).

### MediaFlow-compatible stream-proxy relay

Expose the authenticated `/proxy` relay (signed URLs, HLS/DASH rewriting) behind
a public endpoint — the configuration used by the HuggingFace Space. Set the
password and secret as **secrets**, not in the image:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -e STREMIO_PROXY_PUBLIC_URL=https://your-host.example \
  -e STREMIO_PROXY_PASSWORD=change-me \
  -e STREMIO_PROXY_SECRET=$(openssl rand -hex 32) \
  -e STREMIO_PROXY_IP_ACL=10.0.0.0/8 \
  -v stremio-data:/data \
  stremio-server-go
```

`STREMIO_PROXY_PUBLIC_URL` must be the externally reachable base URL, otherwise
rewritten child segment/key URLs are unreachable behind the edge proxy. Pin
`STREMIO_PROXY_SECRET` explicitly so signed URLs survive restarts.

### Private / DHT-only (no tracker announces)

Discover peers via DHT, PEX, and webseeds only — no outbound tracker announces:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -e STREMIO_DISABLE_TRACKERS=1 \
  -v stremio-data:/data \
  stremio-server-go
```

### Indexer-backed streams (Torznab / Bitmagnet)

Enable the catalog add-ons that resolve streams from a self-hosted indexer:

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -e STREMIO_TORZNAB_URL=http://prowlarr:9696/1/api \
  -e STREMIO_TORZNAB_APIKEY=your-api-key \
  -e STREMIO_BITMAGNET_URL=http://bitmagnet:3333/graphql \
  -v stremio-data:/data \
  stremio-server-go
```

Add the served `/torznab` and `/bitmagnet` manifests to Stremio. Full guides:
[docs/TORZNAB.md](TORZNAB.md), [docs/BITMAGNET.md](BITMAGNET.md).

---

## Hardware-accelerated transcoding

The server probes available encoders at startup and falls back to software
libx264 automatically.  To pass GPU devices into the container:

### VAAPI / Intel QSV (Linux DRI)

```sh
podman run -d \
  --name stremio-server \
  -p 11470:11470 \
  -v stremio-data:/data \
  --device /dev/dri \
  --group-add keep-groups \
  -e STREMIO_HWACCEL=vaapi \
  stremio-server-go
```

Use `STREMIO_HWACCEL=qsv` for QSV on the same device node.

### NVENC (NVIDIA)

Requires the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html) installed on the host.

```sh
docker run -d \
  --name stremio-server \
  -p 11470:11470 \
  -v stremio-data:/data \
  --gpus all \
  -e STREMIO_HWACCEL=nvenc \
  stremio-server-go
```

With CDI: `--device nvidia.com/gpu=all` (Podman 4.7+, CDI configured).

### CPU-only hosts

Force software transcode to avoid auto-detect overhead on systems without a GPU:

```sh
-e STREMIO_HWACCEL=0
```

---

## Deploy to HuggingFace Spaces (Docker SDK)

1. **Create a new Space** — choose **Docker** as the SDK and start from the
   blank template.

2. **`Dockerfile` is already provided** — HuggingFace auto-detects only
   `Dockerfile` at the repo root

3. **Add the Space README front matter** — the Space `README.md` must begin
   with this YAML block (HuggingFace parses it to configure the Space):

   ```yaml
   ---
   title: Stremio Server Go
   emoji: 🎬
   colorFrom: purple
   colorTo: indigo
   sdk: docker
   app_port: 11470
   pinned: false
   ---
   ```

   `app_port: 11470` tells HuggingFace to route its public HTTPS endpoint to
   the container's port 11470.  Because HF terminates TLS at the edge, the
   container itself does not need to serve HTTPS — that is why `HTTPS_PORT`
   defaults to `0` in the image.

4. **Set environment variables** — in the Space **Settings → Variables and
   secrets** panel, add any overrides, for example:

   | Key | Value |
   |---|---|
   | `STREMIO_HWACCEL` | `0` |
   | `STREMIO_HTTP_LOG` | `1` |
   | `STREMIO_MEMORY_CACHE_SIZE` | `536870912` |
   | `STREMIO_PROXY_PUBLIC_URL` | `https://<your-space>.hf.space` |
   | `STREMIO_PROXY_PASSWORD` | _(secret)_ |
   | `STREMIO_PROXY_SECRET` | _(secret, 32-byte hex)_ |

   Secrets are injected as environment variables at runtime and are not visible
   in build logs.

5. **Enable persistent storage** — in **Settings → Persistent storage**, attach
   storage to the Space.  HuggingFace mounts it at `/data`, which is already
   owned by UID 1000 in the image.  Without persistent storage, cache, settings,
   and any provisioned TLS material are lost on every restart.

---

## Caveats on HuggingFace

- **Only port 11470 is reachable** — HuggingFace routes exactly `app_port`
  through its public HTTPS proxy.  The BitTorrent peer port (`BT_LISTEN_PORT`)
  is never exposed, so inbound peering and seeding are not possible.  Outbound
  DHT, PEX, tracker announces, and webseed downloads still work normally.

- **Spaces sleep when idle** — a Space with no traffic is paused after a period
  of inactivity.  The first request after a cold start may be slow while the
  container restarts.

- **Ephemeral storage by default** — without persistent storage enabled, the
  `/data` directory is wiped on every restart.  Enable persistent storage (see
  above) to retain cache and settings across restarts.

- **CPU Spaces have no GPU** — HuggingFace CPU Spaces provide no GPU device.
  Set `STREMIO_HWACCEL=0` to force software transcoding; the auto-detect path
  will fall back to libx264 anyway, but setting it explicitly avoids startup
  probe noise.

---

## Security and legal

This server uses a **localhost-trust model with no authentication**.  Anything
that can reach the port is fully trusted.  The server acts as an open reverse
proxy (`/proxy`) and shells out to `ffmpeg` and `yt-dlp`; never expose it to
untrusted networks.

When deploying to HuggingFace Spaces (or any public host), be aware:

- A public, unauthenticated torrent/proxy endpoint may violate the
  HuggingFace [Terms of Service](https://huggingface.co/terms-of-service).
- Respect copyright law.  `yt-dlp` routes and the proxy are intended for
  personal, lawful use only.

This software is intended for personal use on private networks.
