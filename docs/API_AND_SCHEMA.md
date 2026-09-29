# API and schema

## SQLite tables

### users

- `id TEXT PRIMARY KEY` — `X-ExeDev-UserID`
- `email TEXT NOT NULL`
- `role TEXT NOT NULL` — user/admin
- `status TEXT NOT NULL` — active/suspended
- `created_at`, `last_seen_at`

### admin_bootstrap_claims

- `email TEXT PRIMARY KEY`
- `user_id TEXT UNIQUE NOT NULL` — stable exe.dev user ID that claimed this bootstrap email
- `claimed_at`

### books

- `id TEXT PRIMARY KEY`
- `owner_user_id TEXT NOT NULL`
- `title`, `author`
- `source_kind` — upload/youtube/url
- `source_url` nullable
- `mode` — transcript (aligned is a planned EPUB phase)
- `status` — queued/acquiring/transcribing/ready/error
- `duration_ms`
- `audio_relpath`, `epub_relpath`, `transcript_relpath`, `alignment_relpath`
- `alignment_quality` nullable
- timestamps

### chapters

- `id TEXT PRIMARY KEY`
- `book_id`
- `ordinal`
- `title`
- `start_ms`, `end_ms`
- `transcript_state`
- `alignment_quality`

### jobs

- `id TEXT PRIMARY KEY`
- `owner_user_id`
- `book_id`
- `kind`
- `status`
- `stage`
- `progress REAL`
- `attempt INTEGER`
- `error TEXT`
- `not_before_at` for rate-limit retries
- timestamps

### chunks

Per-book transcription chunks, including state, absolute start/end, local
artifact references, and `not_before_at` for resumable rate-limit backpressure.

### reading_progress

Composite primary key `(user_id, book_id)`.

- `position_ms`
- `playback_rate`
- `sync_offset_ms`
- appearance JSON
- `updated_at`

## API surface

All `/api/*` routes require exe.dev identity except `/api/health`.

```text
GET    /api/me
GET    /api/books
POST   /api/books                  multipart import
GET    /api/books/:id
DELETE /api/books/:id
GET    /api/books/:id/reader
GET    /api/books/:id/audio        Range-capable local media response
GET    /api/books/:id/events       SSE processing progress
PUT    /api/books/:id/progress
POST   /api/books/:id/retry
GET    /api/admin/users           admin only
PUT    /api/admin/users/:user_id  admin only; update active/suspended status
```

Mutating API requests require a same-origin `Origin` header. All book-specific
routes enforce stable-ID ownership, including audio ranges and event streams.
Filesystem paths and source URLs are never returned to clients.

Every authenticated identity is created or updated on its first API request.
There is no app-level email allowlist by default; the private exe.dev proxy
controls access to the VM. Admin bootstrap emails are one-time claims that
bind to a stable user ID, rather than a role that follows a mutable email.

### POST /api/books

Multipart fields:

- `source_url` optional
- `audio_file` optional
- `title` optional

Exactly one of `source_url` or `audio_file` is required. The `epub_file` field is reserved for the planned second mode.

EPUB upload is not enabled yet; including an `epub_file` currently returns
`501 Not Implemented`. EPUB alignment is the planned second reading mode.
Response `202` immediately returns the queued book; ingestion runs in a persisted
background job.

### GET /api/books/:id/reader

Returns metadata, chapters, and a compact transcript window selected by
`?start_ms=` / `?end_ms=`. The reader fetches the active window rather than
mounting an entire audiobook transcript in the DOM.

### GET /api/books/:id/audio

Use Go `http.ServeContent` or equivalent so HTTP Range requests work. The browser `<audio>` element then seeks efficiently without R2 or a separate media server.

## SSE

SSE is used for processing progress and reconnects through the browser's
`EventSource`:

```text
event: progress
data: {"status":"ready","job_status":"running","stage":"transcribing","progress":0.42}
```

The bookshelf also polls book summaries as a reconnect fallback.
