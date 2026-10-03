# Free paired-book discovery

## Decision

Use Internet Archive Advanced Search as the primary free-LibriVox audio
catalog, restricted to `collection:librivoxaudio` and `mediatype:audio`.
Re-fetch each selected item's metadata and require a public MP3 file before
import. IA is the audio host/catalog here, not a new narration corpus.

For the Gutenberg side:

1. Prefer a numeric Gutenberg reference in the IA item's `source` or
   `description`, and validate its title, author, and language against
   Gutenberg's machine-readable catalog.
2. If IA supplies no link, suggest **all** exact title/creator/language
   matches from that catalog. The user must select an edition and explicitly
   confirm the title/author match. Never silently pick one when several
   editions exist.
3. Import the selected EPUB from the fixed Gutenberg mirror, then let
   Readalong's alignment coverage reveal mismatched or partial texts.

Direct metadata links are **source-linked, not edition-verified**.
Title/author suggestions are **unverified candidates**, not claimed pairs.
The documented LibriVox API remains a short-timeout fallback when IA search
is unavailable or yields no usable result.

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
2. Review the Internet Archive item, narrator/version, match basis, and
   Gutenberg page. If IA had no text link, choose among the displayed catalog
   candidates and confirm that you reviewed the match.
3. Confirm that using these specific recording/text editions is permitted
   where you live, then choose **Import audio + EPUB**.
4. Readalong re-fetches the IA item by its validated identifier, downloads its
   MP3 archive from Archive.org, and fetches the selected Gutenberg EPUB from
   the fixed mirror.
5. The alignment pipeline scores the EPUB/audio match. Strong matches default
   to ebook text; low-coverage matches default to the audio transcript. The
   reader allows an explicit switch to the EPUB without inventing word timing.

The user can always add their own audio and EPUB files through the normal
import form.

## Network behavior

- Primary search uses Internet Archive Advanced Search with a 20-record page,
  a 15-minute in-process cache, and a three-second minimum gap between IA
  requests, under a 15-second overall search deadline. Its sanitized Lucene
  query always requires
  `collection:librivoxaudio` and `mediatype:audio`.
- IA search requests only identifier, title, creator, language, source,
  description, runtime, and date. Paid Audible/Apple/subscription listings
  are outside this collection and are never accepted.
- If an IA description includes a LibriVox catalog-page URL, the UI may expose
  that official page as a human-review link. Readalong does not fetch/scrape
  those HTML pages; the metadata/catalog matching path works independently.
- A numeric Gutenberg ID is extracted only from IA `source` or `description`
  metadata, then checked against Gutenberg's catalog. The candidate title,
  creator, language, and catalog type (`Text`) must be compatible with the
  recording metadata.
  Description links to individual stories inside an anthology are rejected
  when they do not match the audiobook title/creator.
- If IA has no usable Gutenberg reference, exact normalized
  title/creator/language matches with catalog `Type=Text` from the official
  Gutenberg catalog are listed as candidates. Every candidate edition is
  shown; no edition is silently chosen when several exist. Import revalidates
  the selected ID and requires the user to confirm this inferred match.
- The official `pg_catalog.csv.gz` is refreshed at most weekly and cached
  under the app data directory. A stale parsed snapshot is reused if
  Gutenberg is temporarily unavailable. Readalong does not scrape Gutenberg
  search pages or fetch an EPUB for every result.
- On import, Readalong re-fetches IA metadata and requires the same identifier,
  `librivoxaudio` membership, `mediatype=audio`, and at least one non-private
  MP3. It constructs the compressed MP3 URL from that validated identifier;
  the browser never supplies an archive URL.
- For paired audiobook imports, the selected Gutenberg EPUB is downloaded and
  parsed before the large Archive.org MP3 ZIP, so a broken text edition does
  not waste an audiobook download.
- Search returns only LibriVox-collection recordings, so paid or
  subscription-based services are excluded. Public MP3 availability is
  checked again on import.
- The documented LibriVox JSON endpoint is a fallback when IA search is down
  or yields no usable results. It keeps the three-second request gap and has
  an eight-second search deadline so a stalled origin cannot dominate latency.
- If one catalog returns no matches while the other is unavailable, the search
  returns an empty result with a warning that the search may be incomplete.
  Only a failure of both catalogs is presented as a catalog outage.
- LibriVox returns HTTP 404 with `Audiobooks could not be found` for an empty
  title search; Readalong treats that response as no results, not an outage.
  Transient failures get at most one retry. HTTP 408 and 5xx can retry; 429
  retries only with `Retry-After` of at most 15 seconds. Longer requested
  delays suppress an automatic retry.
- If a full-title query has no eligible pair, Readalong makes one narrower
  trailing-phrase search for multiword queries and only keeps results whose
  title still contains every meaningful search term. This tolerates omitted
  connectors such as “of” without returning unrelated matches.
- The optional trailing-phrase query has a 45-second ceiling so a slow first
  attempt can still receive its single bounded retry. It remains one query
  phrase with at most two attempts; the production eight-second fallback
  deadline further bounds this legacy path.
- LibriVox's September 16, 2026 API notice set a 500-record maximum target
  for on/after September 26, 2026 and asks clients to leave several seconds
  between calls. The rollout status is not assumed; Readalong requests only
  20 records and keeps the throttle.
- Import accepts a validated LibriVox numeric ID or a prefixed IA identifier,
  never a user-supplied URL. The server re-fetches the provider record and
  derives or revalidates the Gutenberg ID from approved metadata/catalog data.
- Archive.org ZIP requests are HTTPS-only, limited to Archive.org hosts, and
  follow only Archive.org redirects. IA's compressed-MP3 endpoint may redirect
  through a numeric storage shard such as `/14/items/`; Readalong permits only
  one safe item `.zip` path with an MP3 format and no extra query keys. DNS
  results are pinned and checked as public before connection.
- ZIP extraction rejects traversal and symbolic links, caps the number of
  entries and aggregate uncompressed size, extracts MP3 tracks under generated
  local names, naturally sorts chapter filenames, and joins them with ffmpeg.
- Gutenberg downloads use the fixed HTTPS mirror path
  `https://gutenberg.pglaf.org/cache/epub/{id}/pg{id}.epub`; only a numeric ID
  derived from the validated LibriVox record can form this path. Redirects
  must stay on the mirror host. The EPUB is byte-limited and then subjected to
  the same ZIP/spine/path/decompression validation as an uploaded EPUB.
- Archive.org audio archives, Gutenberg EPUBs, and direct public media get at
  most one automatic retry for transient connection/stream failures, HTTP 408,
  or transient 5xx. A 429 is retried only when the server supplies a
  `Retry-After` of at most 15 seconds; longer delays, permanent 4xx responses,
  invalid content types, oversize files, and local disk errors are not retried.
  A failed second attempt leaves no partial download; the user can retry the
  import from the bookshelf.
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

## Source research follow-up (September 30, 2026)

The LibriVox API worked for record `391` on September 29, but on September
30 a fresh request to the same canonical host timed out when fetching a known
record by ID; the `www` alias returned a TLS certificate error. The app now
searches IA first and keeps the documented LibriVox API as a bounded fallback.

Live IA tests found nine **Anne of Green Gables** LibriVox versions for the
token query `collection:librivoxaudio AND mediatype:audio AND title:(anne AND
green AND gables)`. They all have public audio but no Gutenberg ID in source
metadata; the official Gutenberg catalog has four exact title/author/language
records, so the UI shows all four and requires a deliberate choice. A live
**Gift of the Magi** item (`giftofmagi`) explicitly references Gutenberg e-text
7256, which the Gutenberg catalog confirms as the same title and O. Henry.

IA item descriptions sometimes include a LibriVox catalog page URL. The
original Anne page was reachable and linked its “Online text” to Gutenberg
45, but a different version's page returned HTTP 522. This HTML fallback is
not used in automated search/import; it is less stable than the IA metadata
and Gutenberg catalog path.

Other sources evaluated:

- **Internet Archive, LibriVox collection:** now the primary audio discovery
  path. Advanced Search finds `collection:librivoxaudio` records; its metadata
  API confirms item identity, collection, media type, and MP3 files; item
  metadata may also include a license URL. For example, `giftofmagi` has
  source metadata `Librivox recording of Gutenberg e-text #7256`; Gutenberg's
  catalog confirms #7256 is **The Gift of the Magi** by O. Henry. This is an
  exact metadata crosswalk, though edition alignment still needs scoring.
- For titles without a Gutenberg reference, Gutenberg's `pg_catalog.csv.gz`
  offers machine-readable `Text#`, type, title, authors, language, and issue
  date. Only `Type=Text` records are candidates. **Anne of Green Gables** is a
  useful warning case: IA search finds nine free LibriVox versions. Four
  Gutenberg records share the exact title/author/language, but two are
  `Sound` records and are excluded. Readalong offers the two text records
  (#45 and #64365) for user review rather than guessing. A normalized text
  comparison of their EPUBs found them very similar, but that does not prove
  which edition any particular recording used.
- IA descriptions can also contain Gutenberg URLs, but not every URL is the
  text for the full audiobook. For example, the IA description for **Seven
  Men** links Gutenberg texts for two individual stories. The matcher rejects
  those when their titles do not match the audiobook. This title/author
  validation is important even when a Gutenberg ID is present in metadata.
- An IA item's description sometimes links to its official LibriVox catalog
  page; the original Anne page exposed an `Online text` link to Gutenberg
  #45. This could strengthen matching on demand, but it is an HTML-page
  fallback rather than a documented API, and another version page returned
  HTTP 522 during testing. It is not part of automatic search/import.
- **Digitalbook.io:** its search combines free LibriVox entries with paid
  Audible/Apple listings. A tested free **Anne of Green Gables** result pointed
  to chapter MP3s hosted on Archive.org; the matching IA item is in
  `librivoxaudio`, carries public-domain license metadata, and has no Gutenberg
  ID in its metadata. Digitalbook's current Terms prohibit automated scraping
  without written permission, so Readalong must not crawl its pages. Filtered
  manual browsing is possible, but an automated integration should query
  Archive.org directly and still solve the missing Gutenberg-text link. The
  page's record-specific LibriVox RSS feed responded from the VM, but that
  known-ID feed is not a title-search API and did not provide a Gutenberg link.
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
The TTS collection remains the best evaluated candidate for a genuinely
non-LibriVox narration source; IA provides the resilient catalog/file path
for LibriVox recordings.

## Primary references

- Internet Archive Advanced Search API:
  https://archive.org/developers/search.html
- Internet Archive Metadata API:
  https://archive.org/developers/metadata.html
- Internet Archive Advanced Search endpoint:
  https://archive.org/advancedsearch.php
- Internet Archive item metadata endpoint:
  https://archive.org/metadata/
- LibriVox API documentation: https://librivox.org/api/info
- LibriVox API update, published September 16, 2026:
  https://librivox.org/2026/09/16/librivox-api-update/
- LibriVox public-domain information:
  https://librivox.org/pages/public-domain/
- Project Gutenberg mirrors and harvesting:
  https://www.gutenberg.org/policy/robot_access.html
- Project Gutenberg catalog linking policy:
  https://www.gutenberg.org/policy/linking.html
- Project Gutenberg machine-readable catalog:
  https://www.gutenberg.org/cache/epub/feeds/pg_catalog.csv.gz
- Project Gutenberg terms:
  https://www.gutenberg.org/policy/terms_of_use.html
- Digitalbook.io Terms & Conditions:
  https://www.digitalbook.io/terms
- Gutenberg mirror list:
  https://www.gutenberg.org/MIRRORS.ALL
- Loyal Books About:
  https://www.loyalbooks.com/about
- Loyal Books submission terms:
  https://www.loyalbooks.com/submit-book
