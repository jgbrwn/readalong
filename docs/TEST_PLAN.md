# Test plan

## Acquisition

- Upload MP3.
- Upload M4B with chapter metadata.
- Process manual audio upload with relative `APP_DATA_DIR`.
- Process manual audio + EPUB upload with relative `APP_DATA_DIR` through
  normalization, transcription, and alignment.
- Direct MP3 URL.
- YouTube normal video/audiobook URL without cookies or yt-dlp config.
- YouTube fallback client path.
- Internet Archive search is restricted to `librivoxaudio` audio items and
  excludes non-LibriVox/paid sources.
- IA Gutenberg IDs from source/description metadata are validated against the
  official Project Gutenberg catalog, require `Type=Text`, and match
  title/creator/language.
- Gutenberg links in IA descriptions that refer only to individual stories
  inside a differently titled anthology are rejected.
- IA items without an ebook ID show every exact title/creator/language
  candidate, require an explicit selection when ambiguous, and require match
  confirmation before import.
- IA import re-fetches metadata, verifies collection/media type/public MP3,
  and constructs the Archive.org download URL from the validated item ID.
- LibriVox chapter archive joins succeed when data, output, and work paths are
  relative, as they are with the default `APP_DATA_DIR=./data`.
- Archive.org `zip_dir.php` redirects accept valid numbered storage shards
  (for example `/14/items/`) while rejecting traversal and unrelated query
  parameters.
- A broken selected Gutenberg EPUB fails before the IA audiobook ZIP is
  downloaded.
- IA match metadata cannot expose a ZIP/download URL to the client.
- LibriVox catalog remains a bounded fallback when IA is unavailable or has
  no usable candidate.
- Gutenberg catalog cache survives restarts and uses stale data if its host is
  temporarily unavailable.
- LibriVox catalog retries network/HTTP 408/transient 5xx failures once, but
  retries HTTP 429 only with a short explicit `Retry-After`; long waits do not
  trigger automatic retries.
- LibriVox archive redirects remain on Archive.org hosts and are size limited.
- Gutenberg EPUB download uses the validated numeric ID and fixed HTTPS mirror
  path; off-host redirects are rejected.
- Archive.org, Gutenberg, and direct media downloads retry transient HTTP
  failures or interrupted streams once, cap attempts at two, and do not retry
  permanent HTTP 4xx or invalid media responses.
- Download retries honor short `Retry-After` values; unbounded/long wait times
  do not cause an automatic retry.
- A LibriVox chapter ZIP is path-safe, naturally ordered, and joins to one MP3.
- A LibriVox no-result 404 displays an empty result state; transient upstream
  failures retry once without violating the request gap.
- If one pair-search catalog returns no matches and the other is unavailable,
  the UI shows the empty result state with a partial-search warning rather
  than reporting the whole search as unavailable.
- “Anne Green Gables” finds “Anne of Green Gables” without broadening to titles
  that omit one of the supplied significant words.
- A slow first attempt on the trailing-phrase query does not cancel its one
  permitted retry prematurely.
- Public media URL redirecting to a private IP is rejected.
- Bad/404 URL.
- URL resolving to localhost/private IP is rejected for direct HTTP fetch.
- Playback endpoint responds correctly to byte-range requests.

## Groq

- Word + segment timestamps parse correctly.
- 8-minute FLAC chunks remain under configured upload limit.
- 2-second overlap merges without duplicate words.
- Timestamp regressions never reorder transcript tokens; severe regressions
  remain readable but are not highlighted.
- Dense bursts of impossible ASR tokens/timestamps collapse to an untimed
  `[unclear audio]` marker, while isolated numbers and alphanumeric names stay.
- Repeated words spoken close together are not discarded as chunk duplicates.
- Kill process halfway through; completed chunks are reused after restart.
- Simulated 429 stores retry time and resumes later.
- A successfully transcribed first chunk remains readable while later chunks are queued.
- For aligned books with no completed alignment, the partial ASR view is marked
  provisional while transcription is running or rate-limited.
- EPUB chapter headings are not sent as generic ASR hints to every chunk.
- A retry response with HTTP 202 and an empty body is handled as a successful
  queue operation, not parsed as JSON.
- Re-transcription preserves the existing transcript/alignment and reading
  progress when a fresh Groq pass fails or is rate-limited.
- A successful re-transcription atomically publishes a new transcript and
  alignment, while prior artifacts remain recoverable and completed fresh
  chunks resume after restart.
- Re-transcription is owner-scoped, rejects duplicate active jobs, and never
  re-downloads or mutates the book's audio/EPUB.
- Invalid API key creates actionable error without leaking the key.

## Sync engine

- Play continuously for 30 minutes: no cumulative drift.
- Seek forward/back repeatedly.
- Change speed 1.0 -> 1.75 -> 0.8.
- Pause/resume.
- Switch browser tab for a minute and return.
- A reader time-window transition loads once at the bucket boundary and does
  not repeatedly replace or scroll the same window.
- Apply +/- sync offset.
- Tap word seeks accurately.

## EPUB alignment

- Audio + local EPUB upload produces aligned mode and a reflowable reader.
- Catalog import fetches both public sources and creates an owner-scoped book.
- Exact matching text aligns; expected hints reach the Groq request.
- Exact matching edition.
- Small punctuation differences.
- Narrator intro not present in ebook.
- Ebook foreword omitted from audio.
- ASR misspells a character name.
- Audiobook chapter and ebook chapter boundaries do not match.
- Deliberately wrong ebook: quality gates must prevent false word-precision.
- Low coverage defaults to transcript mode; user can view EPUB text without
  false moving highlights on unmatched words.

## Bookshelf search

- Search matches title and author without returning another user's books.
- Empty results and clearing the query remain usable on mobile.
- Sort by recent, title, author, last-read, and progress is stable and owner-scoped.
- Sort choice persists locally and remains usable at narrow mobile widths.
- Email identity badge remains readable without overlap at 320px and 360px.

## Covers

- Migration queues existing bookshelf books; new books enter the same persisted
  reconciliation flow.
- Manual upload and paired import both enqueue cover state atomically with the
  book/job; the worker waits for stable media and aligned EPUB metadata, then
  may work during transcription/rate-limit waits, but not during acquisition
  or a retranscription.
- EPUB2 and EPUB3 declared covers extract and normalize; undeclared decorative
  images, SVG payloads, oversized images, and unsafe archive paths are rejected.
- Disabling external catalog searches does not suppress local EPUB/audio
  artwork extraction or explicitly enabled OpenRouter image-cover generation.
- Attached audio artwork is extracted only from ffprobe streams marked
  `attached_pic`; arbitrary video frames are not used as jackets.
- Open Library matches are title/author scored; only exact title and a
  trustworthy author match can auto-select. Uncertain matches need owner review.
- Equivalent normalized title/author queries share a hashed cache across users.
  No-match retries use next-day, weekly, and monthly delays; provider failures
  use separate exponential backoff.
- Catalog lookup sends only title/author; the background worker uses a global
  request gap, and cover images are lazy-loaded from the approved provider.
- Catalog candidates do not replace an AI/local cover; owner choice and
  “keep current” lookup pause are enforced.
- Cover image endpoints and cover choices are owner-scoped. Cover tasks never
  alter book readiness or transcription jobs.
- Shelf thumbnails and cover-review images contain the full source artwork
  without distortion, including title/author text near image edges; the
  existing cover frame dimensions and card layout stay unchanged.
- A ready owner can manually queue one cover regeneration at a time; current
  selection remains visible while catalog and fresh AI candidates are prepared.
- Regeneration can offer both catalog and newly generated raster image options; choosing
  either updates selection, removes stale candidates, and pauses polling until
  the owner manually regenerates again.
- Clone/transfer remaps and copies pending AI candidate assets as well as the
  selected and catalog cover files.
- The picker exposes exactly OpenAI GPT Image 2 (default), Seedream 4.5, and
  FLUX.2 Pro, rejects other model IDs, and explains missing OpenRouter setup.
- Image prompts preserve the exact book title/author, strip recording labels
  from cover typography, include verified publication year and optional
  sanitized EPUB description, and direct *Little Women* toward its four sisters.
- OpenRouter image requests use one image, portrait 2:3, and supported
  model-specific parameters; API keys never enter logs, responses, or subprocess
  environments.
- Image response size, base64, media type, format, dimensions, pixel count, and
  portrait ratio are checked; SVG/HTML, remote URLs, malformed data, and
  oversized assets are rejected before atomic storage.
- Existing SVG covers remain owner-scoped and readable; new candidates are
  normalized JPEGs and retain versioned no-store URLs. Candidate choice,
  current-cover preservation, and clone/transfer work for both asset formats.
- Provider 401/402/429, malformed output, and timeouts do not trigger fallback
  models or automatic retries that could duplicate image charges.
- Image attempts are durably reserved before provider dispatch. A failed or
  interrupted attempt cannot be automatically replayed after lease expiry or
  restart; a manual Regenerate request explicitly authorizes another attempt,
  while ordinary catalog polling can continue.
- Live OpenRouter image generation requires account credits; smoke-test each
  configured model after funding, and record returned usage/cost.

## Admin book clone/transfer

- Non-admins cannot list other users' books or queue/inspect copy operations.
- Only a completed idle book with saved audio and transcript is eligible;
  active jobs, missing files, suspended recipients, and same-owner operations
  are rejected.
- Clone gets a fresh book ID, independent files, fresh chapter IDs, no old
  runnable jobs, and no personal reading progress or Groq call.
- Transfer updates owner paths and all historical job owners, removes old
  progress, and revokes former-owner retry, reader, audio, and cover access.
- Former owners cannot recreate reading progress or retry a moved book.
- Copy rejects symlinks and trees larger than the configured hard limit;
  failures leave the source available.
- Interrupted operations recover across staging, file publication, and DB
  commit boundaries; staging markers cannot delete another operation's files.
- Admin operation results contain no filesystem paths, signed URLs, or book
  contents.

## Auth

- Every valid exe.dev identity is provisioned on first visit and gets its own bookshelf.
- Missing headers denied in production.
- No email allowlist is applied by default.
- Bootstrap admin email is claimed once and bound to its stable user ID.
- A different user ID with the same email does not inherit a claimed admin role.
- Admin identity keeps its own bookshelf and does not bypass book ownership.
- Admin can suspend/reactivate another account; suspended identities are denied.
- An admin cannot suspend their own account through the admin API.
- A user cannot request another user's book ID.
- Development override does not work when `APP_ENV=production`.

## Mobile/PWA

- 360x800 viewport.
- Android Chrome PWA install.
- iPhone/Safari responsive layout if available.
- Reader remains usable with large text settings.
- Player controls reachable one-handed.
- Reader uses absolute media time after seek and playback-rate changes.
- Reader controls collapse; light/dark, text size, highlight mode, and sync offset persist.
- Theme saves during playback and survives an immediate mobile reload.
- Screen wake lock is requested while playing where supported, then released on
  pause/backgrounding and reacquired when playback returns to the foreground.
- Service worker update does not strand stale API responses.

## Backup

- Litestream reports healthy sync.
- Restore DB into empty location.
- rclone restore a test book.
- Re-open and seek the restored book.
