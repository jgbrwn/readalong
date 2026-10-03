# Implementation status

## Phase 1 — audio-only reader

Implemented:

- exe.dev-header identity, automatic per-user accounts, stable-ID ownership,
  and runtime-configured admin access;
- mobile identity badge layout, persisted reader appearance, and
  feature-detected screen wake lock during visible playback;
- loopback-only server configuration and systemd installation;
- audio upload, YouTube acquisition, SSRF-protected direct-media downloads,
  playback normalization, chunked Groq transcription, resumable jobs, and
  absolute word timestamps; user-triggered re-transcription now uses an
  isolated resumable run and only replaces the active transcript after success;
- owner-scoped bookshelf, title/author search, Range-capable playback, reader
  windows, progress persistence, and the mobile-first PWA.

## Phase 2 — audio + EPUB reader

The first end-to-end implementation is present:

- upload audio with an EPUB, or import a source-linked LibriVox/Gutenberg pair;
- parse EPUB metadata and spine-ordered XHTML, reflowing semantic text blocks;
- send bounded title/chapter hints to Groq;
- use exact-token anchors and bounded sentence-local alignment, persist a
  versioned alignment artifact, and report matched-token coverage;
- render canonical EPUB text with only matched/timed words highlighted;
- preserve Groq token order when word timestamps regress; untimed words stay
  readable without a misleading animated highlight;
- fall back to transcript mode below 75% coverage, with an explicit reader
  toggle for the EPUB text when at least one word was anchored. With zero
  matches, Readalong keeps the transcript view rather than showing an
  unbounded, untimed EPUB window.

Pair discovery searches Internet Archive's LibriVox collection and falls back
to LibriVox's documented API. Explicit Gutenberg references in IA metadata
are validated against the official Project Gutenberg catalog; when IA has no
text link, exact title/creator/language candidates are shown for user
selection rather than silently treated as source-linked. Import re-fetches
and validates the IA item and its public MP3 files. The Gutenberg catalog
snapshot is cached locally, and the importer accepts no arbitrary URL.

**Phase 2 is implemented but not yet declared complete.** Exact alignment is
covered by synthetic integration tests, and the full public-source ingestion
path has been exercised with a fake Groq endpoint. Remaining validation:

- process a real matching pair through Groq and inspect alignment quality;
- exercise a deliberately wrong translation/edition and verify transcript
  fallback and untimed ebook-word behavior;
- validate long books and chapter/foreword differences on mobile and desktop.

## Shelf, covers, and admin tools

Implemented first passes:

- bookshelf sorting by recently added, title, author, recently read, or progress;
- cover state/backfill for all existing books, EPUB-declared covers, supported
  embedded audio artwork, Open Library catalog suggestions, shared lookup
  caching, persisted retry/backoff, and owner-checked local image serving;
- catalog-cover review that never silently replaces a selected generated
  cover; choosing to keep the generated cover pauses future automatic checks;
- optional AI-designed SVG fallback. Reflection discovers the VM's managed
  vision model list, caches it, and provides a model/API-style selector and
  small health check. The current model inventory advertises image input, not
  image output, so the selected LLM returns a bounded design recipe and
  Readalong renders the SVG locally instead of pretending it can emit a
  raster image;
- admin-only asynchronous clone/transfer for complete, idle books. It copies
  files independently, does not copy reading progress or runnable jobs, and
  shows progress in the admin panel.

## Phase 3 — library durability

Still incomplete: configure and verify Litestream/R2 database replication,
automated R2 asset mirroring, backup-health diagnostics, and a real restore
drill. Cover assets and their database state must be included in the eventual
asset/database backup.

## Still remaining

- generic yt-dlp webpage extraction beyond YouTube and direct media URLs;
- finish real-edition/long-book EPUB alignment validation noted above;
- chapter jump/navigation controls (chapter metadata and current chapter
  display exist, but no chapter picker/next/previous controls);
- bookmarks and quotes;
- an explicit source-retention/cleanup policy (temporary processing files are
  cleaned, but there is no general source-retention policy);
- offline audio downloads; the service worker currently caches the app shell,
  not book audio/reader data;
- optional aligned EPUB/SMIL export, direct-Groq-URL transcription, and
  precision acoustic forced alignment.

Every installation should verify its own private exe.dev share. Public source
availability does not imply a public running service.
