# Implementation status

## Phase 1 — audio-only reader

Implemented:

- exe.dev-header identity, automatic per-user accounts, stable-ID ownership,
  and runtime-configured admin access;
- loopback-only server configuration and systemd installation;
- audio upload, YouTube acquisition, SSRF-protected direct-media downloads,
  playback normalization, chunked Groq transcription, resumable jobs, and
  absolute word timestamps;
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
- fall back to transcript mode below 75% coverage, with an explicit reader
  toggle for the EPUB text when at least one word was anchored. With zero
  matches, Readalong keeps the transcript view rather than showing an
  unbounded, untimed EPUB window.

Pair discovery searches LibriVox's documented API and imports only records
that link to Project Gutenberg text and an Archive.org audio archive. EPUBs
are fetched from the Project Gutenberg mirror after a user confirms rights.
This avoids scraping book pages and arbitrary user-supplied catalog URLs.

**Phase 2 is implemented but not yet declared complete.** Exact alignment is
covered by synthetic integration tests, and the full public-source ingestion
path has been exercised with a fake Groq endpoint. Remaining validation:

- process a real matching pair through Groq and inspect alignment quality;
- exercise a deliberately wrong translation/edition and verify transcript
  fallback and untimed ebook-word behavior;
- validate long books and chapter/foreword differences on mobile and desktop.

## Still remaining

- generic yt-dlp webpage extraction beyond YouTube and direct media URLs;
- R2 asset mirroring, Litestream credentials, and a verified restore drill;
- cover extraction, bookmarks, quotes, and offline audio downloads.

Every installation should verify its own private exe.dev share. Public source
availability does not imply a public running service.
