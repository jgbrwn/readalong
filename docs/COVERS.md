# Cover discovery and reconciliation

## Selection order

1. Extract the image explicitly declared as the EPUB cover.
2. Extract attached artwork from the saved audio/source with `ffprobe` and
   `ffmpeg`.
3. Search Open Library using a normalized title/author query. An exact
   title-and-author result may be selected automatically; uncertain matches
   are shown as candidates. YouTube uploader names are not treated as authors.
4. If no catalog cover is found and the administrator enabled generated
   covers, ask the selected OpenRouter image model to generate a raster cover.

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
times, negative/error counters, lookup pause, and an expiring worker lease.
Database migration inserts a pending row for every existing book; new imports
insert one with the book/job transaction.

One in-process Go worker claims due covers across all users. SQLite leases make
the work restart-safe and prevent overlap with admin copy/transfer operations.
It uses a shared hashed title/author lookup cache so identical books across
owners do not trigger duplicate catalog searches.

Schedule:

- initial lookup as soon as book metadata/artifacts are available;
- after a confirmed no-match: next-day retry, then one week, then monthly;
- provider failures: independent exponential retry (1 hour, 6 hours, 1 day,
  then 3 days);
- if generated art is selected, keep checking on the normal catalog schedule;
- if a user chooses to keep generated art after a better catalog candidate is
  found, pause future automatic lookups. A catalog selection also stops
  unnecessary polling.

Failures leave the existing cover and reader state intact. Cover tasks never
create or mutate transcription jobs, transcript state, or book readiness.

## Catalog behavior, privacy, and rights

Open Library receives only title and author; no user ID/email, audio, EPUB, or
transcript is sent. `OPEN_LIBRARY_CONTACT` may identify this installation in
the User-Agent for regular catalog use. Requests are globally serialized at
one per second and capped at 100 unique searches per UTC day. Search results
are cached/deduplicated, and negative results are not polled daily forever.
The model list and search results are stored locally; cover image URLs are
lazy-loaded directly from the provider rather than bulk-downloaded.

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
private `.env`. The admin picker offers GPT Image 2 (default), Seedream 4.5,
and FLUX.2 Pro; it has no text-model search or API-format setting.

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
output is never treated as markup, a URL, or a file path. Requests are not
automatically retried after ambiguous failures, preventing accidental duplicate
charges.

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
- **Use AI cover** — select the newly rendered SVG and pause automatic lookup.
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
