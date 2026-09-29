#!/usr/bin/env bash
set -u
cd "$(dirname "$0")/.."
fail=0
check(){ if command -v "$1" >/dev/null 2>&1; then echo "[OK] $1: $(command -v "$1")"; else echo "[FAIL] missing $1"; fail=1; fi; }
check ffmpeg; check ffprobe
if [[ -x .tools/bin/yt-dlp ]]; then echo "[OK] yt-dlp: $PWD/.tools/bin/yt-dlp"; elif command -v yt-dlp >/dev/null 2>&1; then echo "[OK] yt-dlp: $(command -v yt-dlp)"; else echo "[FAIL] missing yt-dlp"; fail=1; fi
if [[ -x .tools/bin/deno ]]; then echo "[OK] deno: $PWD/.tools/bin/deno"; elif command -v deno >/dev/null 2>&1; then echo "[OK] deno: $(command -v deno)"; else echo "[FAIL] missing deno JavaScript runtime"; fail=1; fi
check go; check python3
[[ -w data ]] && echo "[OK] data writable" || { echo "[FAIL] data not writable"; fail=1; }
if [[ -f .env ]] && grep -Eq '^GROQ_API_KEY=.+$' .env; then echo "[OK] GROQ_API_KEY appears configured"; else echo "[WARN] GROQ_API_KEY not configured in .env"; fi
if [[ -f .env ]]; then
  mode=$(stat -c '%a' .env)
  if (( (8#$mode & 077) == 0 )); then echo "[OK] .env permissions: $mode"; else echo "[FAIL] .env is accessible by group/others ($mode); run chmod 600 .env"; fail=1; fi
else
  echo "[FAIL] .env missing; run ./scripts/bootstrap-exe.sh"; fail=1
fi
if [[ -f .env ]] && grep -Eq '^R2_ENABLED=(true|1)$' .env; then
  if grep -Eq '^R2_ACCOUNT_ID=.+$' .env && grep -Eq '^R2_ACCESS_KEY_ID=.+$' .env && grep -Eq '^R2_SECRET_ACCESS_KEY=.+$' .env; then
    echo "[OK] R2 runtime credentials appear configured"
  else
    echo "[FAIL] R2_ENABLED is true but bucket-scoped R2 S3 credentials are missing"
    fail=1
  fi
else
  echo "[INFO] R2 mirroring is disabled"
fi
exit "$fail"
