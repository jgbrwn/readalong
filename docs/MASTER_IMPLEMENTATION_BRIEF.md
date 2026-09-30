# Master implementation brief

## Product target

Create a private web/PWA reader that combines the best ideas from HushBook/Spokt/Storyteller:

- import an audiobook file, a direct audio/web URL, or a YouTube link;
- remotely transcribe with Groq rather than making the phone do ASR;
- play audio with lyric-style synchronized words;
- pair audio with EPUB text and align the narration to the canonical ebook words;
- discover/import free LibriVox audio + Project Gutenberg pairs, preferring
  explicit source links and clearly labeling user-confirmed title/author
  candidates when catalogs omit the ebook ID;
- search a user's private bookshelf by title or author;
- maintain a private exe.dev-authenticated bookshelf.

## Phase 0 — bootstrap and verify

1. Install Go, ffmpeg/ffprobe, yt-dlp, and optionally rclone/Litestream.
2. Verify the `mst3k-anything` cookie-free YouTube pattern against at least two YouTube URLs from the exe VM.
3. Put `GROQ_API_KEY` in a local env file with restrictive permissions.
4. Start the scaffold and verify exe.dev headers arrive through the proxy.
5. Keep the site private in exe.dev; the proxy is the access gate and Readalong
   provisions a separate account for every authenticated exe.dev user. Set the
   first admin with `ADMIN_USER_IDS` or the one-time `ADMIN_BOOTSTRAP_EMAILS`
   setting in the private `.env`.

Exit criterion: authenticated bookshelf shell loads through exe.dev.

## Phase 1 — audio-only end-to-end MVP

Implement:

- upload + URL import;
- yt-dlp/direct HTTP acquisition;
- ffprobe metadata and duration;
- normalized browser playback MP3;
- chunk generation;
- Groq transcription with word timestamps;
- per-chunk persistence and retry;
- overlap merge;
- reader JSON;
- audio Range endpoint;
- word-highlighting reader;
- progress/resume/speed/sync offset.

Prioritize the first chunk so the user can start reading before the whole book has finished. It is acceptable for the rest of the book to continue processing in the background.

Exit criterion: YouTube audiobook -> ready first section -> stable word highlighting on phone and desktop.

## Phase 2 — EPUB aligned mode and paired discovery

Implemented first pass: EPUB spine extraction, quality-scored sentence-local
alignment, dual-mode reader, paired discovery/import, and owner-scoped book
search. Discovery prefers source-linked LibriVox/Gutenberg records and also
offers carefully labeled, user-confirmed Gutenberg candidates from the
Internet Archive LibriVox collection. Continue validating against real
editions before calling Phase 2 complete. Details are in
`SYNC_AND_ALIGNMENT.md` and `PAIRED_CATALOG.md`.

Start with a clean reflowed representation of publisher text. Do not get stuck preserving arbitrary EPUB styling.

Confidence metrics and fallback are implemented in the first pass; validate
them on real matching and intentionally mismatched editions before calling
this mode done.

Exit criterion: matching EPUB+audiobook displays ebook text and stays aligned across seeks/chapter boundaries; intentionally mismatched inputs fail safely.

## Phase 3 — library durability

1. Configure Litestream v0.5+ from SQLite to private R2.
2. Configure rclone R2 remote and mirror `data/books/`.
3. Add backup status to `/api/health` or admin diagnostics.
4. Document restore drill and actually test it on a copied DB/data directory.

An optional `CLOUDFLARE_API_TOKEN` in `.env` can be used for manual
Wrangler/API administration. It is not an R2 S3 credential and is stripped
from the Readalong application process. Runtime R2 replication requires
separate bucket-scoped Access Key ID and Secret Access Key credentials.

Exit criterion: delete a disposable local test library, restore DB + assets from R2, and open/read the test book.

## Phase 4 — polish

- cover extraction;
- chapter navigation;
- typography/theme controls;
- bookmarks/quotes;
- processed-source cleanup policy;
- optional offline-download mode;
- optional export of aligned EPUB/SMIL;
- optional direct-Groq-URL fast path;
- optional precision acoustic aligner.

## Groq rate-limit behavior

Provider limits vary by model, plan, and account. Treat HTTP 429 as normal backpressure:

- parse `Retry-After`;
- store `not_before_at` in the job/chunk row;
- keep completed chunks;
- show a human-readable status such as “Groq rate limit reached; queued to resume.”

Do not restart the book or discard completed transcription.

## Important implementation choices

### Use absolute timestamps everywhere

Internally store milliseconds as integers where practical. Convert Groq floats once. Avoid repeated floating-point transformations.

### Chunk artifacts are transactional

Write response to a temp file, fsync/close, then rename into final chunk path. Mark DB state complete only after the artifact exists.

### Reader data should be versioned

`transcript.v1.json.gz` and `alignment.v1.json.gz`. Never paint the app into a corner with an unversioned format.

### Prefer chapter/window loading

A 20-hour audiobook can have >150k words. Do not render them all as DOM spans at once. Load/render the active chapter plus adjacent buffer. Keep a global time-to-chapter index.

### Keep source and derived media distinct

Preserve source bytes when reasonable. Derived playback/transcription chunks can be regenerated.

## UX target

The reader should feel like a book first, not a transcription dashboard. Processing controls stay in the bookshelf/detail view. Once reading:

- large calm typography;
- current word animation;
- minimal player controls;
- chapter title/progress;
- tap word to seek;
- easy speed control;
- no AI summaries or clutter by default.
