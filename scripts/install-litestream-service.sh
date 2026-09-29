#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "Missing .env; configure the app first." >&2
  exit 1
fi
if ! command -v litestream >/dev/null 2>&1; then
  echo "Litestream is not installed; install a current version before enabling R2 replication." >&2
  exit 1
fi
if ! grep -Eq '^R2_ENABLED=(true|1)$' .env ||
   ! grep -Eq '^R2_ACCOUNT_ID=.+$' .env ||
   ! grep -Eq '^R2_ACCESS_KEY_ID=.+$' .env ||
   ! grep -Eq '^R2_SECRET_ACCESS_KEY=.+$' .env; then
  echo "Enable R2 and set bucket-scoped R2 S3 credentials in .env first." >&2
  exit 1
fi

chmod 600 .env
mkdir -p data/books
chmod 750 data data/books
make build
unit=$(mktemp)
trap 'rm -f "$unit"' EXIT
python3 - "$PWD" "$(id -un)" "$(command -v litestream)" deploy/readalong-with-litestream.service "$unit" <<'PY'
from pathlib import Path
import sys

app_dir, user, litestream, source, destination = sys.argv[1:]
text = Path(source).read_text()
text = text.replace("@APP_DIR@", app_dir).replace("@SERVICE_USER@", user).replace("@LITESTREAM_BIN@", litestream)
if "@APP_DIR@" in text or "@SERVICE_USER@" in text or "@LITESTREAM_BIN@" in text:
    raise SystemExit("unexpanded systemd template values")
Path(destination).write_text(text)
PY
sudo systemctl disable --now readalong.service 2>/dev/null || true
sudo install -o root -g root -m 0644 "$unit" /etc/systemd/system/readalong-with-litestream.service
sudo systemctl daemon-reload
sudo systemctl enable --now readalong-with-litestream.service
sudo systemctl --no-pager --full status readalong-with-litestream.service
