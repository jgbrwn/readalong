# Sync and alignment design

This is the most important quality area in the app.

## Core rendering rule: never accumulate timing drift

The UI must **not** advance words using chained timers. At every visual update, derive the active word from the media element's absolute `currentTime`.

Recommended reader loop:

1. while audio is playing, run `requestAnimationFrame`;
2. read `audio.currentTime`;
3. binary-search the sorted word timing array;
4. activate the corresponding word span;
5. scroll only when the active sentence/line changes, not every word;
6. on `seeked`, `timeupdate`, `ratechange`, and `visibilitychange`, immediately recompute.

Playback-rate changes therefore need no timing conversion: media `currentTime` remains in source-media seconds.

Include a per-book user-adjustable sync offset (e.g. -2000..+2000 ms) to compensate for systematic transcription or output-device latency. Persist it with progress/preferences.

## Mode A — audio only

### Transcription merge

For each chunk:

```json
{
  "chunk_start": 960.0,
  "words": [
    {"word":"Call","start":0.41,"end":0.67},
    {"word":"me","start":0.68,"end":0.82}
  ]
}
```

Convert to absolute media times by adding `chunk_start`.

Chunks overlap by ~2 seconds. De-duplicate by comparing normalized trailing words of chunk N against leading words of chunk N+1. Use a bounded longest matching suffix/prefix (e.g. up to 25 words), preferring exact normalized matches but allowing one-token ASR differences. Never keep duplicate time regions.

Preserve the transcription provider's word order. Do not sort individual words
by timestamps to repair small timing regressions; that can put audible words
out of order in the transcript. Keep tokens in chunk order, clamp minor time
regressions to a searchable monotonic timeline, and leave severely regressed
words untimed instead of highlighting them with false precision.

Treat timestamps shorter than 40 ms as untrustworthy for word highlighting.
When a compact burst contains several implausible alphanumeric/long-number ASR
tokens and multiple such impossible timings, replace that derived-text span
with one untimed `[unclear audio]` marker. Preserve the original chunk response
so a later re-transcription can recover it; do not silently turn a single
unusual name or isolated number into a placeholder.

### Transcript shaping

Use Groq punctuation/segments to group words into sentences and paragraphs. The visible word token keeps punctuation, but matching uses a normalized token.

Data contract:

```json
{
  "version": 1,
  "duration_ms": 123456,
  "sentences": [
    {
      "id": "s1",
      "start_ms": 1200,
      "end_ms": 5400,
      "words": [
        {"t":"Call","s":1200,"e":1420,"c":1.0},
        {"t":"me","s":1420,"e":1590,"c":1.0}
      ]
    }
  ]
}
```

`c` is confidence in the timing/mapping, not necessarily ASR model confidence.

## Mode B — audio + EPUB

The goal is to show the ebook's real text while borrowing acoustic timing from the Groq transcript.

### Stage 1: EPUB extraction

Read the EPUB spine in order and extract semantic blocks (`h1-h6`, `p`, `li`, `blockquote`). Preserve original text and enough structure for a clean reflowed reader. Assign stable IDs to chapters, blocks, sentences, and words.

Do not require exact publisher CSS for v1. The product is a read-along reader, not an EPUB conformance suite.

### Stage 2: transcript hints

Before transcription, extract a small hint list from the ebook: chapter title and unusual/proper-name tokens. Send a short prompt to Groq to improve spelling consistency. Keep the prompt bounded.

### Stage 3: find chapter/region anchors

Follow Storyteller's proven idea: ebook and audiobook tracks are not assumed to be 1:1. Search the transcript for the beginning of each ebook chapter using normalized fuzzy matching (Levenshtein/token similarity) and monotonic ordering.

Useful anchor unit: 8-20 words from the chapter opening after removing headings/boilerplate. Search within a plausible window first, widen if needed.

### Stage 4: sentence alignment

Within the matched transcript region, align ebook sentences to transcript word windows. Use fuzzy normalized text similarity. Maintain a monotonic cursor. If a sentence does not match, do not force it; tolerate gaps and continue searching. Storyteller's strategy of allowing misses before moving the search window is a useful model.

### Stage 5: word-level alignment inside a sentence

For a matched sentence pair, run a token sequence alignment (Needleman-Wunsch / edit-distance dynamic programming) between:

- ebook tokens, which are canonical for display;
- ASR words, which own acoustic `start/end` times.

Scoring suggestion:

- exact normalized match: +4
- near spelling match / contraction equivalent: +2
- substitution: -2
- insertion/deletion: -2

Copy timestamps from matched ASR words to ebook words. For unmatched ebook words between trustworthy anchors, interpolate times proportionally using character/syllable-ish weight. Do not fabricate high confidence for interpolated words.

### Stage 6: quality gates

A wrong moving highlight is worse than a coarser correct highlight.

Store at least:

- matched ebook token ratio;
- median sentence similarity;
- maximum unanchored span;
- monotonicity violations;
- chapter lead/tail gap;
- interpolated-word ratio.

Suggested first policy:

- **high confidence:** >= 90% token coverage and no large drift -> word highlighting;
- **medium:** >= 75% coverage -> sentence highlighting, word highlight only where anchored;
- **low:** below threshold -> show the ebook for reading but use transcript-mode highlighting, or ask the user to manually choose an alignment point.

Do not silently pretend a low-confidence mapping is precise.

### Current first-pass implementation

The current aligner uses Unicode-aware normalization, exact four-word anchors,
a monotonic cursor, and bounded sentence-local dynamic programming. It does
not run a quadratic whole-book alignment or force word timestamps where no
match exists. The reported quality is the fraction of EPUB word tokens that
received a timestamped transcript match. Unmatched sentence windows may be
interpolated only to keep the text window/chapter navigation usable; their
words stay untimed and are never animated.

The reader defaults to EPUB text at >=75% coverage. Below that it defaults to
the transcript; if at least one anchor exists, the user may explicitly view
canonical EPUB text with only matched words highlighted. With zero matched
words it remains in transcript mode rather than loading an unbounded untimed
book. This is intentionally more conservative than claiming that an EPUB/audio
pair is aligned just because both files were imported. The first pass does not
yet implement fuzzy spelling substitutions, manual anchor correction, or
acoustic forced alignment.

## Optional future precision mode

A direct acoustic forced aligner (CTC/wav2vec/MMS/WhisperX-style) can improve word boundaries when canonical ebook text is available. Projects such as `tale-align` demonstrate this approach and quality-gate the result. It requires a heavier Python/model stack, so it is deliberately phase 2+, not part of the simple Groq-first MVP.

## Reader behavior

- Active word: strong emphasis.
- Already-read words: subtly de-emphasized.
- Upcoming text: normal.
- Auto-scroll by sentence/line and keep the active sentence around 40-55% of viewport height.
- Tap a word to seek to its start time.
- Long-press/select should still allow normal text selection; do not make every word an inaccessible button.
- Provide sentence/paragraph mode if word animation feels visually too busy.
- Reader should work in portrait one-handed use first, but cap line width on desktop.
