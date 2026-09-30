# Test plan

## Acquisition

- Upload MP3.
- Upload M4B with chapter metadata.
- Direct MP3 URL.
- YouTube normal video/audiobook URL without cookies or yt-dlp config.
- YouTube fallback client path.
- LibriVox catalog search returns only records linked to Gutenberg and
  Archive.org sources.
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
- Email identity badge remains readable without overlap at 320px and 360px.

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
