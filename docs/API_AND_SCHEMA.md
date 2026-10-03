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
- `source_kind` — upload/youtube/url/librivox
- `source_url` nullable
- `mode` — transcript/aligned
- `status` — queued/acquiring/transcribing/ready/error
- `duration_ms`
- `audio_relpath`, `epub_relpath`, `ebook_json_relpath`,
  `transcript_relpath`, `alignment_relpath`
- `gutenberg_id`, `ebook_source_url`, `alignment_quality`
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

### book_cover_state

One owner-book row tracks the selected cover, catalog suggestion, generated
cover suggestion, and any user-triggered regeneration request:

- selected/catalog/AI-candidate artifact paths or catalog URLs and provenance;
- state (`pending`, `checking`, `missing`, `selected`, or `review`);
- last/next check, negative-result count, provider-failure count, lookup pause,
  regeneration flag, and an expiring lease for the cover worker.

Existing books are backfilled on migration; new imports get a pending row.
Generated/EPUB cover files live under the book's owner-scoped directory.

### cover_lookup_cache

Shared, provider-facing lookup cache keyed by a hash of normalized title and
author. It stores the candidate/no-match result, check time, and next permitted
lookup. This deduplicates repeated searches across books and users without
storing another plaintext copy of their queries.

### app_settings and admin_book_operations

`app_settings` stores the sanitized managed-model inventory and admin-selected
cover settings. `admin_book_operations` is a separate durable queue/audit
record for admin-initiated clone/transfer operations; it is intentionally not
stored in the transcription `jobs` queue.

## API surface

All `/api/*` routes require exe.dev identity except `/api/health`.

```text
GET    /api/me
GET    /api/books?q=title-or-author&sort=recent|title|author|lastread|progress
POST   /api/books                  multipart import
GET    /api/books/:id
DELETE /api/books/:id
GET    /api/books/:id/reader       transcript/ebook window; content=transcript|ebook
GET    /api/books/:id/audio        Range-capable local media response
GET    /api/books/:id/cover/selected|candidate|ai-candidate  owner-checked image
POST   /api/books/:id/cover-choice
POST   /api/books/:id/cover-regenerate
GET    /api/books/:id/events       SSE processing progress
PUT    /api/books/:id/progress
POST   /api/books/:id/retry
POST   /api/books/:id/retranscribe
GET    /api/discovery/pairs?q=title
POST   /api/discovery/pairs/:id/import
GET    /api/admin/users           admin only
PUT    /api/admin/users/:user_id  admin only; update active/suspended status
GET    /api/admin/users/:user_id/books
POST   /api/admin/book-operations
GET    /api/admin/book-operations/:operation_id
GET    /api/admin/cover-ai
POST   /api/admin/cover-ai/refresh
PUT    /api/admin/cover-ai
POST   /api/admin/cover-ai/check
```

Mutating API requests require a same-origin `Origin` header. All book-specific
routes enforce stable-ID ownership, including audio ranges and event streams.
Filesystem paths and source URLs are never returned to clients.

### GET /api/books

`sort` is an allowlisted per-request value. `recent` is the default; `title`,
`author`, `lastread`, and `progress` are stable owner-scoped sorts.

### Cover discovery and display

EPUB-declared raster cover art and supported embedded audiobook artwork are
normalized locally first. If neither exists, the persistent cover worker
searches Open Library using only the book title and author. It shares a hashed
query cache across books/users, sends no user ID/email/audio/EPUB, and limits
requests to one per second. Catalog artwork is lazy-loaded directly from the
provider; Readalong does not crawl or bulk-download cover images.

Negative searches retry after 24 hours, then 7 days, then monthly. Provider
failures use separate exponential backoff. High-confidence catalog matches can
be selected when no cover exists; uncertain results are review candidates. A
candidate never silently replaces a generated cover.

`POST /api/books/:id/cover-choice` accepts `use_candidate` (catalog),
`use_ai_candidate`, or `keep_current`. Each explicit choice pauses automatic
replacement/lookup; “Not now” leaves suggestions available for later review.
`POST /api/books/:id/cover-regenerate` queues a fresh catalog check and, when
enabled, a new AI design. The current cover remains selected until the owner
chooses among the current cover and returned suggestions.

Admin cover settings discover/cache text-output models through Reflection,
including OpenAI/ChatGPT, Neuralwatt, and OpenRouter entries. Models without
explicit text-output metadata are shown as inferred candidates and require a
health check. Vision
means image input, not image output; neither is required for the recipe-based
local SVG renderer. The model check defaults to Auto: it tries the catalog's
preferred Responses or Chat Completions endpoint and tries the alternate after
an endpoint rejection or an accepted response without a valid design recipe.
It does not retry authentication, billing, rate-limit, refusal, or truncation
failures. Responses streaming and Chat Completions text formats are both
parsed. `GET` serves the six-hour cached inventory;
`POST /api/admin/cover-ai/refresh` refreshes it. `PUT` saves the global catalog
lookup switch, description-sharing preference, model ID, API style, and
optional generation switch. The health check is explicit and rate-limited per
model; model listing itself never runs inference. Checks and generated designs
can consume provider quota.

### Admin book operations

`POST /api/admin/book-operations` accepts a `book_id`, `source_user_id`,
`target_user_id`, and `mode` (`clone` or `transfer`). Only an idle book with a
completed initial ingestion, saved audio, and transcript is eligible. The
operation is asynchronous and reports byte progress via
`GET /api/admin/book-operations/:operation_id`.

Clones use a fresh book ID and independent file copies; transfers preserve the
book ID but move the owner and owner-scoped files. Both reset personal reading
progress, omit old runnable jobs, preserve the completed reader artifacts, and
reject symlinks or books larger than 8 GiB. A clone receives a new completed
book marker so it remains eligible for a later copy, without replaying
transcription. The target must be an active user. Transfer clears the stored
source URL and removes the old files only after the new ownership/artifact
pointers commit.

### POST /api/books/:id/retry

Re-queues the latest failed book-processing job for its owner. Returns
`202 Accepted` with an empty response body; clients must not expect JSON.
Groq rate-limited jobs are already queued with a resume time and do not need
this endpoint.

Every authenticated identity is created or updated on its first API request.
There is no app-level email allowlist by default; the private exe.dev proxy
controls access to the VM. Admin bootstrap emails are one-time claims that
bind to a stable user ID, rather than a role that follows a mutable email.

### POST /api/books

Multipart fields:

- `source_url` optional
- `audio_file` optional
- `epub_file` optional
- `title` optional

Exactly one of `source_url` or `audio_file` is required. An `epub_file` may be
provided alongside either audio source to create an aligned-mode book. The
EPUB must be a valid spine-based file smaller than 150 MiB; archives are
validated for path traversal and decompression limits.
Response `202` immediately returns the queued book; ingestion runs in a persisted
background job.

### GET /api/discovery/pairs

Searches the Internet Archive's LibriVox collection first, then uses the
documented LibriVox API as a bounded fallback. Results identify the audio
provider and match basis. A Gutenberg ID found in IA `source`/`description`
metadata is checked against the Project Gutenberg catalog; otherwise exact
title/creator/language `Type=Text` candidates are returned for explicit user
review. The response remains a JSON array. If one catalog returns no matches
while the other is unavailable, the endpoint returns `200 []` with an
`X-Readalong-Search-Warning` header explaining that the search may be
incomplete. If both catalogs are unavailable, it returns a gateway error.
Archive download URLs are never returned. Results expose `provider`,
`match_kind` (`source_linked` or `title_author`), and the validated
`text_candidates` list so the UI can distinguish evidence from suggestions.

### POST /api/discovery/pairs/:id/import

JSON body:
`{"rights_confirmed":true,"gutenberg_id":"45","match_confirmed":true}`.
`gutenberg_id` is required for IA results and must be a current source-linked
or title/creator/language candidate. `match_confirmed` is required for
inferred/ambiguous matches. The server re-fetches the LibriVox/IA item,
revalidates its approved source and audio files, downloads the Archive.org MP3
archive and selected Gutenberg EPUB, then imports both into the caller's
private shelf. No user-provided network URL is accepted. The UI asks the user
to confirm rights and distinguish a source link from a suggested match.

### GET /api/books/:id/reader

Returns metadata, chapters, and a compact transcript window selected by
`?start_ms=` / `?end_ms=`. Aligned books accept `content=transcript` or
`content=ebook`; the default uses canonical EPUB words only when alignment
coverage is at least 75%, otherwise it shows the audio transcript. Unmatched
ebook words have no timestamps and are never given a moving word highlight.
The reader fetches the active window rather than mounting an entire book in
the DOM.

### POST /api/books/:id/retranscribe

Queues a fresh Groq transcription of the book's existing normalized audio.
The book must be ready and already have a transcript. A job-specific work
directory keeps new chunk responses separate from the current version and
resumes them after restarts/rate limits. The current transcript, EPUB,
alignment, and reading progress remain active until the complete transcript
and (when applicable) its alignment are ready; one database update switches
the artifact pointers together.

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
