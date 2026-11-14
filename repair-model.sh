#!/bin/bash
# Force a clean Parakeet re-download into the shared models volume, then restart whisper.
set -euo pipefail
cd "$(dirname "$0")"
export PARAKEET_FORCE_DOWNLOAD=1
docker compose --profile setup build whisper model-download
docker compose --profile setup run --rm -e PARAKEET_FORCE_DOWNLOAD=1 model-download
docker compose up -d --force-recreate whisper
echo "waiting for model..."
for i in $(seq 1 60); do
  if curl -sf http://127.0.0.1:${WHISPER_HOST_PORT:-8080}/ready >/dev/null 2>&1; then
    echo "OK model ready"
    curl -s http://127.0.0.1:${WHISPER_HOST_PORT:-8080}/ready
    echo
    exit 0
  fi
  sleep 5
done
echo "still not ready — check: docker compose logs whisper"
docker compose logs --tail 40 whisper
exit 1
