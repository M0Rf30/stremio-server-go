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
