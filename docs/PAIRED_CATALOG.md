# Free paired-book discovery

## Decision

Use the official LibriVox audiobook API for discovery, then pair a record only
when its `url_text_source` points to a numeric Project Gutenberg `/ebooks/` or
`/etext/` record. Import the audio archive from the LibriVox API record and
fetch the no-images EPUB from the Project Gutenberg mirror. The match is
**source-linked, not edition-verified**.

Readalong does not scrape Loyal Books pages. Although it offers convenient
paired listings, the app does not need to depend on its page markup or reuse
its user-submitted catalog. It also does not search Libby, library systems,
subscription services, or paid catalogs.

Loyal Books' own About page says much of its material comes from LibriVox and
Project Gutenberg, but it also includes best sellers. Its submission terms
grant a license to Loyal Books, not automatically to third-party applications.
The reviewed Loyal Books pages did not document a catalog API for this
integration.

## User flow

1. Search by title from **Find a pair**.
2. Review the LibriVox record and canonical Project Gutenberg page.
3. Confirm that using these specific recordings/texts is permitted where the
   user lives, then choose **Import audio + EPUB**.
4. Readalong re-fetches the LibriVox record by its numeric ID and imports both
   sources into the authenticated user's private shelf.
5. The alignment pipeline scores the EPUB/audio match. Strong matches default
   to ebook text; low-coverage matches default to the audio transcript. The
   reader allows an explicit switch to the EPUB without inventing word timing.

The user can always add their own audio and EPUB files through the normal
import form.

## Network behavior

- Search uses the documented LibriVox JSON endpoint with a 20-record page,
  a 15-minute in-process cache, and a three-second minimum gap between upstream
  requests. Results are limited to HTTPS LibriVox records whose text source is
  Project Gutenberg and whose audio archive is Archive.org.
- LibriVox's September 16, 2026 API notice set a 500-record maximum target
  for on/after September 26, 2026 and asks clients to leave several seconds
  between calls. The rollout status is not assumed; Readalong requests only
  20 records and keeps the throttle.
- Import accepts a catalog record ID, never a user-supplied URL. It revalidates
  the record and derives the Project Gutenberg ID from the approved host/path.
- LibriVox ZIP requests are HTTPS-only, limited to Archive.org hosts, and
  follow only Archive.org redirects. Archive.org's `zip_dir.php` redirect is
  restricted to a `.zip` item path and MP3 format. DNS results are pinned and
  checked as public before connection.
- ZIP extraction rejects traversal and symbolic links, caps the number of
  entries and aggregate uncompressed size, extracts MP3 tracks under generated
  local names, naturally sorts chapter filenames, and joins them with ffmpeg.
- Gutenberg downloads use the fixed HTTPS mirror path
  `https://gutenberg.pglaf.org/cache/epub/{id}/pg{id}.epub`; only a numeric ID
  derived from the validated LibriVox record can form this path. Redirects
  must stay on the mirror host. The EPUB is byte-limited and then subjected to
  the same ZIP/spine/path/decompression validation as an uploaded EPUB.
- CORS is not involved in these server-to-server requests. SSRF, redirect
  validation, provider access rules, rights, and size/decompression limits are
  the relevant concerns.

## Rights and edition caveat

LibriVox describes its recordings as public domain in the United States, but
the ebook text, translation, and edition can have separate rights. Project
Gutenberg also has occasional permission-based titles. The confirmation step
is not a legal determination or a worldwide public-domain guarantee. Users
must check the particular text and recording. Neither title similarity nor a
LibriVox `url_text_source` link proves that the spoken words and EPUB edition
are identical.

## Primary references

- LibriVox API documentation: https://librivox.org/api/info
- LibriVox API update, published September 16, 2026:
  https://librivox.org/2026/09/16/librivox-api-update/
- LibriVox public-domain information:
  https://librivox.org/pages/public-domain/
- Project Gutenberg mirrors and harvesting:
  https://www.gutenberg.org/policy/robot_access.html
- Project Gutenberg catalog linking policy:
  https://www.gutenberg.org/policy/linking.html
- Project Gutenberg terms:
  https://www.gutenberg.org/policy/terms_of_use.html
- Gutenberg mirror list:
  https://www.gutenberg.org/MIRRORS.ALL
- Loyal Books About:
  https://www.loyalbooks.com/about
- Loyal Books submission terms:
  https://www.loyalbooks.com/submit-book
