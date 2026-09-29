# Architecture decision

## Chosen shape

```text
Browser/PWA
   |
   | HTTPS through exe.dev proxy
   | + X-ExeDev-UserID / X-ExeDev-Email
   v
Go app on dedicated exe.dev VM
   |- SQLite (users/books/jobs/progress)
   |- local persistent book files
   |- yt-dlp / ffmpeg / ffprobe
   |- Groq Whisper API
   |- LibriVox catalog API (throttled/cache)
   |- Project Gutenberg/Archive.org approved source fetch
   `- background worker

Optional future durable layer (Phase 3; not enabled by default)
   |- Litestream -> private Cloudflare R2 (SQLite)
   `- rclone -> private Cloudflare R2 (book assets)
```

## Why this over Cloudflare Workers + D1

The workload includes native media tooling, long-running ingestion, large files, resumable background work, and exe.dev-provided identity. A normal persistent VM is the natural execution environment. exe.dev explicitly supports normal persistent disks and SQLite. D1/Workers would split one simple stateful app into several services without solving a real v1 problem.

R2 is still an excellent fit for off-VM durability because it is S3-compatible, has free Internet egress, and Litestream supports R2 endpoints directly.

## Identity / bookshelf

Authentication is backend-owned. The frontend never decides what a user may access.

On every authenticated request:

1. read `X-ExeDev-UserID` and `X-ExeDev-Email`;
2. reject missing identity in production;
3. upsert the user record, creating it on first visit;
4. scope all book/job queries by stable user ID.

The private exe.dev proxy controls who can reach the app; the app does not
maintain a second email allowlist. Email is display/bootstrap metadata, never
the ownership key. Admins are configured by stable IDs with
`ADMIN_USER_IDS`. For initial setup, `ADMIN_BOOTSTRAP_EMAILS` may grant the
role to the first matching authenticated identity; that grant is then claimed
and permanently bound to its stable exe.dev user ID.

An admin remains an ordinary user for bookshelf operations. Admin-only APIs
are separate and initially support inspecting accounts and suspending or
reactivating access. Admin rights do not bypass per-book ownership checks.

## Storage model

Local disk is the hot source of truth for normal operation:

```text
data/
  app.db
  books/<hashed-user-id>/<book-id>/
    source/
    source/librivox.zip
    playback.mp3
    book.epub
    ebook.v1.json.gz
    book.json
    transcript.v1.json.gz
    alignment.v1.json.gz
    cover.jpg
    work/
```

Large word arrays do not belong in SQLite. SQLite stores metadata, state, paths, checksums, and summaries; compressed JSON stores transcript/alignment payloads.

When configured, R2 should mirror the same logical object tree. The app should
not depend on R2 to serve playback in v1; this keeps the runtime simple and
fast. R2 is for restoration/durability first and is not enabled by default.

## Background work

Use a single persisted `jobs` table and an in-process worker. Default concurrency 1 for book pipelines; Groq transcription substeps can have separately controlled concurrency.

On process startup, stale `running` jobs become `queued` again. Every stage is idempotent and writes an artifact only after successful completion. This makes restart/resume straightforward without Redis.

## Source acquisition

Accepted inputs:

- local uploaded audio (`mp3`, `m4a`, `m4b`, `wav`, `flac`, `ogg`, `opus`, `webm`, `mp4` audio track);
- YouTube URL;
- direct HTTP(S) audio URL;
- source-linked LibriVox recording + Project Gutenberg EPUB pair;
- other yt-dlp-supported webpage URLs are deferred.

Paired-catalog search uses the documented LibriVox API, caches results, and
spaces upstream requests. The client only returns records whose text-source
URL maps to a canonical numeric Gutenberg ID and whose audio archive is on
Archive.org. On import, the server re-fetches the record and derives the
Gutenberg mirror path from that ID; no arbitrary URL is accepted.
`docs/PAIRED_CATALOG.md` documents source policy, rights caveats, and network
guards.

YouTube strategy intentionally follows `jgbrwn/mst3k-anything`:

```text
yt-dlp --no-playlist \
  -f 'bestaudio[ext=m4a]/bestaudio/best' \
  --write-info-json \
  --remote-components ejs:github \
  -o 'source.%(ext)s' URL
```

If that fails for YouTube, retry with:

```text
--extractor-args 'youtube:player_client=android'
```

No cookie requirement is part of the baseline design.

## Canonical playback media

Preserve the source file, but derive a browser-safe playback file. Recommended first pass:

```bash
ffmpeg -i SOURCE -vn -map_metadata 0 \
  -c:a libmp3lame -b:a 96k -ac 1 -ar 44100 playback.mp3
```

The current implementation uses broadly compatible MP3 playback at 96 kbps
mono. The source remains available for re-encoding.

## Groq transcription

Default model: `whisper-large-v3-turbo`.

Request:

- `response_format=verbose_json`
- `timestamp_granularities[]=word`
- `timestamp_granularities[]=segment`
- `language=en` when known
- optional prompt containing chapter title + a short list of unusual ebook names/terms

Chunking is recommended even though Groq accepts URLs directly. The application needs a durable local copy for its bookshelf anyway, and chunks make progress, retries, rate-limit handling, and first-page readiness much better.

Default chunk: 480 s with 2 s overlap. Convert each temporary chunk to 16 kHz mono FLAC. Persist each chunk response independently, then merge to absolute timestamps and de-duplicate overlap.

## Why not use Groq URL passthrough as the core path?

It is useful and should remain an optional optimization, especially for a stable direct MP3 link. But the bookshelf needs local/durable media; a remote source can disappear, cannot guarantee byte-range behavior, and gives poorer resumability. Therefore the core workflow localizes media first.

## PWA/frontend

Use plain ES modules and CSS. No build system is needed for v1.

Main screens:

- Bookshelf
- Add/import sheet
- Processing/detail screen
- Reader
- Settings

PWA service worker caches the app shell and small metadata. Do not automatically cache multi-hour audio for offline use in v1.
