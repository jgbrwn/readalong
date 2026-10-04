# Cover discovery and reconciliation

## Selection order

1. Extract the image explicitly declared as the EPUB cover.
2. Attempt to extract artwork explicitly marked `attached_pic` from supported
   saved audio/source files with `ffprobe` and `ffmpeg`. Not every source
   format retains embedded artwork through conversion or chapter joining.
3. If catalog lookup is enabled, search Open Library using a normalized
   title/author query. An exact
   title-and-author result may be selected automatically; uncertain matches
   are shown as candidates. YouTube uploader names are not treated as authors.
4. If there is no catalog match (or catalog lookup is disabled), generate a
   raster cover only when an administrator has enabled AI image generation.

An uncertain catalog candidate is offered for owner review rather than
automatically invoking image generation. **Regenerate cover** can request both
a fresh catalog lookup and a new AI candidate when AI generation is enabled.

Catalog covers are attributed as catalog suggestions, not guaranteed
publisher-official editions. Source artwork stays unmodified; Readalong shows
its provider and verified first-publication year in the shelf/compare UI.
Generated images receive the exact book title, available author, verified
publication year, and optionally a sanitized EPUB description. Image models
render their own illustration and cover typography; unlike legacy SVGs, text
in generated raster art is not programmatically guaranteed to be exact.

## Persistent state and backfill

`book_cover_state` is separate from transcription `jobs`. It stores the
selected asset/URL, review candidate, provider provenance, checked/next-check
times, negative/error counters, local-scan state, lookup pause, AI-attempt block,
and an expiring worker lease.
Database migration inserts a pending row for every existing book; both manual
uploads and paired-book imports insert one atomically with the book/job
transaction.
Older databases default the new AI-attempt block to clear because previous
versions did not persist image-call outcomes; an already-pending legacy book
may therefore receive one post-upgrade automatic fallback attempt.

One in-process Go worker starts with the app and polls every two seconds.
SQLite leases make the work restart-safe and prevent overlap with admin
copy/transfer operations.

The worker does not wait for transcription to finish. It waits until the
initial ingestion job has published stable audio metadata and, for aligned
books, the parsed EPUB metadata artifact. It then scans local art and performs
catalog/image work while Groq transcription is running or rate-limited. Books
whose ingestion fails are still eligible for cover lookup once the job reaches
a terminal error state. This avoids racing partially downloaded media or
incomplete EPUB metadata while getting covers onto the shelf sooner.

Schedule:

- initial lookup after stable audio/EPUB metadata is published, normally during
  transcription rather than after the complete transcript;
- after a confirmed no-match: next-day retry, then one week, then monthly;
- Open Library provider failures: independent exponential retry (1 hour, 6 hours, 1 day,
  then 3 days);
- an automatic AI image attempt is reserved before dispatch. On failure or a
  crash after reservation, that book will not automatically dispatch another
  image request; catalog checks can continue, and an owner can explicitly
  authorize a new attempt with **Regenerate cover**;
- if generated art is selected, keep checking on the normal catalog schedule;
- if a user chooses to keep generated art after a better catalog candidate is
  found, pause future automatic lookups. A catalog selection also stops
  unnecessary polling.

Failures leave the existing cover and reader state intact. Cover tasks never
create or mutate transcription jobs, transcript state, or book readiness.

## Catalog behavior, privacy, and rights

Open Library receives only title and trusted author metadata; no user
ID/email, audio, EPUB, or transcript is sent. `OPEN_LIBRARY_CONTACT` may
identify this installation in the User-Agent for regular catalog use.
Requests are globally serialized at one per second and capped at 100
uncached/forced provider search attempts per UTC day. Normal search results
are cached/deduplicated, and negative results are not polled daily forever.
The three-model OpenRouter picker is fixed in the app; hashed Open Library
search results are stored locally. Cover image URLs are lazy-loaded directly
from the provider rather than bulk-downloaded.

The bookshelf explains that catalog lookup shares title/author and that cover
images load from Open Library. Administrators can disable catalog lookup for
the whole installation in the Cover artwork settings.

Text/recording rights do not automatically grant jacket-art rights. Readalong
records cover provenance and does not claim that public-domain text makes a
specific cover freely reusable. Catalog covers are user-reviewable; local
EPUB/audio covers are treated as part of the user's imported edition.

Open Library asks clients not to crawl its Covers API and recommends displaying
its image URLs directly; it also asks API clients to cache and identify
regular use. Readalong therefore uses cover IDs from the Search API, directly
renders lazy cover URLs, applies global request spacing, and uses the
next-day/weekly/monthly negative-cache schedule rather than downloading the
cover corpus:

- Open Library API usage: https://openlibrary.org/developers/api
- Open Library Covers API: https://openlibrary.org/dev/docs/api/covers

## Generated image covers

AI generation uses OpenRouter's dedicated Images API directly. The attached
exe.dev LLM gateway currently does not expose the requested image-model set or
the dedicated image endpoint, so `OPENROUTER_API_KEY` must be configured in the
private checkout-root `.env`. Merely configuring a key does not turn image
generation on: it is disabled by default and an administrator must enable it
in **Admin → Cover artwork settings**. The picker offers GPT Image 2 (default),
Seedream 4.5, and FLUX.2 Pro; it has no text-model search or API-format setting.
On a fresh install, Open Library lookup and sharing the short EPUB description
are enabled; AI image generation is not. These installation-wide settings are
saved by an administrator.

The prompt asks the image model to identify the actual work from its literary
knowledge, title, author, verified publication year, and optional short EPUB
description metadata. It requests a polished portrait cover with exact title
and author typography and story-specific characters/settings. For *Little
Women*, it explicitly calls for Meg, Jo, Beth, and Amy as the central subject,
not a generic flower. Only bounded bibliographic metadata is sent—never
chapters, audio, user identity, or source URLs.

OpenRouter returns base64 raster output. Readalong accepts PNG/JPEG only,
checks decoded image dimensions and portrait proportions, flattens any
transparency, normalizes to JPEG, and writes the artifact atomically. Provider
output is never treated as markup, a URL, or a file path. Readalong reserves
the per-book attempt before dispatch; failures, timeouts, and process restarts
do not automatically dispatch another image request. If a worker crashes after
reserving but before producing an image, the owner can explicitly retry with
**Regenerate cover**.

The picker warns when the private API key is missing. Each generation is
billable under the selected OpenRouter model and output settings. Existing
generated SVG covers remain available as legacy artifacts, but all newly
generated covers are raster images.
Local mutable cover previews use opaque versioned URLs and no-store responses
so a regenerated candidate cannot be mistaken for a cached older image.

## Compare and choose

If an Open Library candidate appears while a cover is selected, Readalong keeps
the selected cover and adds a discreet, reduced-motion-aware
“Cover ready · choose” shelf hint. The modal can compare the current cover with
catalog and AI suggestions and offers:

- **Use catalog cover** — select the candidate and stop automatic lookup.
- **Use AI cover** — select the new raster image and pause automatic lookup.
- **Keep current** — dismiss suggestions and pause automatic lookup.
- **Not now** — leave suggestions available for review.

No source cover replaces a selected/generated cover without the owner's
explicit choice.

Every ready book also has a **Regenerate cover** action beside Re-transcribe.
It clears stale suggestions, queues a fresh catalog lookup and—if enabled—a
new AI design, while preserving the selected cover during processing. When
results arrive, the owner can choose the current, catalog, or freshly generated
cover. Choosing “Not now” leaves the suggestions in place; regenerating again
restarts the process and returns new choices. Manual checks bypass a still-fresh
negative lookup cache, but remain subject to the same global request spacing
and daily provider budget.
