# Cover discovery and reconciliation

## Selection order

1. Extract the image explicitly declared as the EPUB cover.
2. Extract attached artwork from the saved audio/source with `ffprobe` and
   `ffmpeg`.
3. Search Open Library using a normalized title/author query. An exact
   title-and-author result may be selected automatically; uncertain matches
   are shown as candidates. YouTube uploader names are not treated as authors.
4. If no catalog cover is found and the administrator enabled generated
   covers, ask the selected managed LLM model for a small structured art
   recipe and render the SVG locally.

Catalog covers are attributed as catalog suggestions, not guaranteed
publisher-official editions. Source artwork stays unmodified; Readalong shows
its provider and verified first-publication year in the shelf/compare UI.
Generated SVGs typeset the exact book title, trusted author, and known year.
Unknown years and untrusted uploader names are omitted rather than guessed.

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

## Generated fallback and Reflection models

The admin panel discovers the attached `type=llm` integration from Reflection,
then reads its OpenAI-compatible `/v1/models` list. It includes text-output
models from OpenAI/ChatGPT, Neuralwatt, and OpenRouter; OpenRouter modality
metadata and provider pricing are normalized. Models without explicit
text-output metadata remain selectable as inferred candidates and are marked
“check required.” The six-hour cache serves stale results if discovery is
temporarily unavailable.

The API format defaults to **Auto**. The model's advertised metadata/gateway
selects the preferred Responses or Chat Completions API; it tries the alternate
after an endpoint rejection or a successful response without a valid design
recipe, but not for auth, billing, rate-limit, refusal, or truncation failures.
The health check
understands Responses SSE text deltas (including gateways whose final response
omits accumulated output) and Chat Completions string or multipart text. It
uses a bounded output budget and applies “no reasoning” only when the model
advertises that option. Failures distinguish provider limits/credits,
unsupported endpoints, truncation, refusals, and no-text responses. Checks
remain explicit and may consume quota.

The selected model returns a strict theme/color recipe based on the title,
trusted author, optional short EPUB description metadata, and verified
publication year; the server validates it and renders an SVG from fixed,
script-free templates. Vision input and native image output are separate
capabilities and neither is required for this text-to-SVG workflow. Arbitrary
model-authored SVG/HTML, URLs, scripts, and file paths are never accepted.

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
