#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "Missing .env; run ./scripts/bootstrap-exe.sh and configure it first." >&2
  exit 1
fi
chmod 600 .env
mkdir -p data/books
chmod 750 data data/books
make build
unit=$(mktemp)
trap 'rm -f "$unit"' EXIT
python3 - "$PWD" "$(id -un)" deploy/readalong.service "$unit" <<'PY'
from pathlib import Path
import sys

app_dir, user, source, destination = sys.argv[1:]
text = Path(source).read_text()
text = text.replace("@APP_DIR@", app_dir).replace("@SERVICE_USER@", user)
if "@APP_DIR@" in text or "@SERVICE_USER@" in text:
    raise SystemExit("unexpanded systemd template values")
Path(destination).write_text(text)
PY
sudo install -o root -g root -m 0644 "$unit" /etc/systemd/system/readalong.service
sudo systemctl daemon-reload
sudo systemctl disable --now readalong-with-litestream.service 2>/dev/null || true
sudo systemctl enable readalong.service
if sudo systemctl is-active --quiet readalong.service; then
  sudo systemctl restart readalong.service
else
  sudo systemctl start readalong.service
fi
sudo systemctl --no-pager --full status readalong.service
