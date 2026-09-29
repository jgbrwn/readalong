# Test plan

## Acquisition

- Upload MP3.
- Upload M4B with chapter metadata.
- Direct MP3 URL.
- YouTube normal video/audiobook URL without cookies or yt-dlp config.
- YouTube fallback client path.
- Public media URL redirecting to a private IP is rejected.
- Bad/404 URL.
- URL resolving to localhost/private IP is rejected for direct HTTP fetch.
- Playback endpoint responds correctly to byte-range requests.

## Groq

- Word + segment timestamps parse correctly.
- 8-minute FLAC chunks remain under configured upload limit.
- 2-second overlap merges without duplicate words.
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
- Apply +/- sync offset.
- Tap word seeks accurately.

## EPUB alignment

- Exact matching edition.
- Small punctuation differences.
- Narrator intro not present in ebook.
- Ebook foreword omitted from audio.
- ASR misspells a character name.
- Audiobook chapter and ebook chapter boundaries do not match.
- Deliberately wrong ebook: quality gates must prevent false word-precision.

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
- Service worker update does not strand stale API responses.

## Backup

- Litestream reports healthy sync.
- Restore DB into empty location.
- rclone restore a test book.
- Re-open and seek the restored book.
