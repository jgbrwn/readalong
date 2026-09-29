#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== Readalong exe.dev bootstrap =="
if command -v apt-get >/dev/null 2>&1; then
  sudo apt-get update
  sudo apt-get install -y ffmpeg python3 python3-venv curl jq unzip ca-certificates
fi

if [[ ! -x .tools/bin/yt-dlp ]]; then
  python3 -m venv .tools
fi
.tools/bin/python -m pip install -U pip 'yt-dlp[default]'

if [[ ! -x .tools/bin/deno ]]; then
  curl -fsSL https://deno.land/install.sh | DENO_INSTALL="$PWD/.tools" sh
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Go is not installed. Install a current Go release (1.23+), then rerun doctor."
fi

mkdir -p data/books
chmod 750 data

if [[ ! -f .env ]]; then cp .env.example .env; chmod 600 .env; fi
chmod 600 .env

echo "Bootstrap complete. Configure GROQ_API_KEY and ADMIN_BOOTSTRAP_EMAILS in .env, then run ./scripts/doctor.sh"
