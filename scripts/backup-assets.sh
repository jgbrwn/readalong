#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${RCLONE_REMOTE:=readalong-r2}"
: "${R2_BUCKET:=readalong-private}"
rclone sync ./data/books "${RCLONE_REMOTE}:${R2_BUCKET}/readalong/books" --checksum --fast-list
