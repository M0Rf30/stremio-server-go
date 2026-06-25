#!/usr/bin/env bash
# Smoke test the running stremio-server-go on :11470.
# Uses a well-seeded, legal public-domain torrent (Sintel, Blender open movie).
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:11470}"
IH="08ada5a7a6183aae1e09d831df6748d566095a10"
TR1="udp://tracker.opentrackr.org:1337/announce"
TR2="udp://open.tracker.cs.buenos-aires.gob.ar:1337/announce"

jqget() { curl -fsS "$1" | (jq "${2:-.}" 2>/dev/null || cat); }

echo "== heartbeat =="
jqget "$BASE/heartbeat"

echo "== network-info (expect IPv6 addresses present) =="
jqget "$BASE/network-info"

echo "== settings.values.serverVersion =="
jqget "$BASE/settings" ".values.serverVersion"

echo "== create engine for Sintel =="
curl -fsS "$BASE/$IH/create?tr=$TR1&tr=$TR2" -o /dev/null -w "create http=%{http_code}\n" || true

echo "== wait for metadata, then stats =="
for i in $(seq 1 30); do
  NAME=$(curl -fsS "$BASE/$IH/stats.json" | (jq -r '.name // empty' 2>/dev/null || true))
  if [ -n "${NAME:-}" ]; then echo "torrent name: $NAME"; break; fi
  sleep 1
done

echo "== file list =="
jqget "$BASE/$IH/stats.json" ".files[].name"

echo "== HEAD the guessed stream (range support) =="
curl -fsS -I "$BASE/$IH/-1" -w "\nstream http (HEAD): %{http_code}\n" | grep -iE "accept-ranges|content-type|content-length" || true

echo "== per-file stats (stream progress) =="
jqget "$BASE/$IH/0/stats.json" "{name, streamName, streamProgress, peers, downloadSpeed}"

echo "== cleanup =="
jqget "$BASE/$IH/remove"
echo "OK"
