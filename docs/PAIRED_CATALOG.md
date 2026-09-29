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
- LibriVox returns HTTP 404 with `Audiobooks could not be found` for an empty
  title search; Readalong treats that no-result response as an empty result
  list, not a provider outage. Transient upstream failures get at most one
  retry, still separated by the configured request gap.
- If a full-title query has no eligible pair, Readalong makes one narrower
  trailing-phrase search for multiword queries and only keeps results whose
  title still contains every meaningful search term. This tolerates omitted
  connectors such as “of” without returning unrelated matches.
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

## Source research follow-up (September 29, 2026)

No provider was replaced. A live request for LibriVox record `391` returned
HTTP 200 during this review; the original search for **Anne Green Gables**
returned no title match, while the catalog title is **Anne of Green Gables**.
Readalong now retries one distinctive trailing phrase after an empty
multiword search and filters the results to titles containing all meaningful
query words. This is a search-quality fallback, not a new provider.

Other sources evaluated:

- **Internet Archive, LibriVox collection:** its Advanced Search API can find
  `collection:librivoxaudio` records, and its metadata API lists MP3/ZIP/M4B
  files. A test record for **The Gift of the Magi** was marked public domain
  and had downloadable audio, but its IA metadata did not carry a Gutenberg ID.
  This is a useful alternate catalog/file host for the same LibriVox
  recordings, not an independent narration corpus; matching an EPUB needs
  separate validation.
- **Project Gutenberg Open Audiobook Collection (TTS):** a promising future
  opt-in audio source, not integrated yet. Its public browse list links audio
  stored on IA, and IA metadata's `source` field contains a Gutenberg source
  URL with a numeric ebook ID. A test **Gift of the Magi** item pointed to
  Gutenberg 7256; its 8.8 MB MP3 was readable and the corresponding Gutenberg
  EPUB parsed to 2,476 words. The narration is synthetic, and alignment has
  not yet been tested through Groq. IA's item `rights` field points to
  Gutenberg's license policy rather than giving a separate `licenseurl`, so
  rights still need review before importing.
- **Open Library:** not a suitable audio/EPUB replacement for this importer.
  Its API returns ebook access states and Internet Archive identifiers, but
  the documented Listen flow is attached to borrowing and expires with the
  loan. Public DAISY is text-only for a reader's own TTS; protected DAISY is
  access-controlled. Open Library also asks third-party applications not to
  use its API as a backend. These are not direct, unrestricted audiobook-file
  links.
- **Project Gutenberg EPUB:** remains the preferred text source. A test showed
  Gutenberg 7256's no-images EPUB available from the fixed PGLAF mirror, while
  the separate audio-only Gutenberg record 22440 did not have a matching EPUB
  at the same numeric ID. Keep matching the explicit EPUB source ID rather
  than assume an audio-record number is also its ebook number.

Before adding a new source, validate rights, stable discovery/download
interfaces, Gutenberg ID extraction, and at least one matching recording.
The TTS collection is the best candidate for a genuinely non-LibriVox
alternative; the Internet Archive LibriVox collection is a resilience fallback
if the LibriVox site/API is temporarily unavailable.

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
