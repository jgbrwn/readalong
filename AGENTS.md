# Readalong project guidance

Read these project documents before making significant changes:

1. `docs/MASTER_IMPLEMENTATION_BRIEF.md`
2. `docs/ARCHITECTURE.md`
3. `docs/SYNC_AND_ALIGNMENT.md`
4. `docs/API_AND_SCHEMA.md`
5. `docs/DEPLOYMENT.md`
6. `docs/TEST_PLAN.md`
7. `docs/IMPLEMENTATION_STATUS.md`

## Product direction

Build a private, mobile-first PWA for synchronized audiobook reading:

- accept uploaded audio, direct public audio URLs, and YouTube URLs;
- transcribe with Groq and preserve word timestamps;
- render a calm, lyric-style reader with seeking, playback speed, and saved progress;
- provide both transcript-only and audio + EPUB aligned reading modes;
- search the private bookshelf and discover/import source-linked public pairs;
- use exe.dev proxy identity for private per-user bookshelves.

## Keep the stack simple

- Go, `net/http`, SQLite, and embedded HTML/CSS/ES modules;
- local persistent files for active media;
- `yt-dlp`, `ffmpeg`, and `ffprobe` for media processing;
- Groq for transcription;
- optional R2/Litestream/rclone for off-VM durability.

Do not add a frontend framework, queue service, microservice, or cloud service
without a concrete product need.

## Identity and security

- Use `X-ExeDev-UserID` as the stable user key; email is mutable metadata.
- Do not hard-code real user emails. Configure admin IDs or an explicit,
  one-time bootstrap email through the private `.env`.
- Production identity headers are trusted only behind the private exe.dev proxy.
- Keep the app bound to `127.0.0.1`; `APP_PORT` is configurable.
- Enforce owner checks for every book, job, progress, and media endpoint.
- Treat pasted URLs as untrusted and block private-network/localhost SSRF.
- Never return filesystem paths or log API keys, signed URLs, or storage secrets.
- Keep each deployment's exe.dev share private unless its owner deliberately
  changes access.

## Sync and alignment

- Derive highlighting from the media element's absolute `currentTime`; never
  advance words with accumulating timers.
- Keep timing in integer milliseconds where practical.
- Preserve completed chunks and resumable job state across restarts and Groq
  rate limits.
- Do not present low-confidence EPUB alignment as precise.

Run `gofmt`, `go test ./...`, `go vet ./...`, and `make build` before committing.
