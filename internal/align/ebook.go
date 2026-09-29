package align

import (
	"sort"
	"strings"

	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/transcript"
)

const (
	anchorWidth    = 4
	maxAnchorAhead = 10000
	maxCandidates  = 32
)

type EbookChapter struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Ordinal int    `json:"ordinal"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

type EbookAlignment struct {
	Version      int                 `json:"version"`
	Quality      float64             `json:"quality"`
	MatchedWords int                 `json:"matched_words"`
	TotalWords   int                 `json:"total_words"`
	Chapters     []EbookChapter      `json:"chapters"`
	Sentences    transcript.Document `json:"document"`
}

type audioWord struct {
	token transcript.Word
	norm  string
}

// AlignEbook maps canonical EPUB words onto transcript timestamps. It first
// indexes exact four-word anchors, then runs bounded sentence-local sequence
// alignment around monotonic candidates; it never runs a whole-book quadratic
// alignment or invents word timestamps for unmatched text.
func AlignEbook(book epub.Document, acoustic transcript.Document) EbookAlignment {
	audio := flattenAudio(acoustic)
	index := make(map[uint64][]int, len(audio))
	for i := 0; i+anchorWidth <= len(audio); i++ {
		if hasEmpty(audio[i : i+anchorWidth]) {
			continue
		}
		key := phraseHash(audio[i : i+anchorWidth])
		index[key] = append(index[key], i)
	}

	out := EbookAlignment{Version: 1}
	var matchedTotal, total int
	cursor := 0
	chapterIndex := make(map[string]int)
	for _, chapter := range book.Chapters {
		chapterOut := EbookChapter{ID: chapter.ID, Title: chapter.Title, Ordinal: chapter.Ordinal}
		chapterIndex[chapter.ID] = len(out.Chapters)
		out.Chapters = append(out.Chapters, chapterOut)
		for _, block := range chapter.Blocks {
			for _, sourceSentence := range block.Sentences {
				words := sourceSentence.Words
				normalized := normalizeWords(words)
				total += len(words)
				canonical := make([]transcript.Word, len(words))
				for i, word := range words {
					canonical[i] = transcript.Word{Text: word}
				}
				mapped, start, end, next, ok := alignSentence(normalized, audio, index, cursor)
				if ok {
					matched := 0
					for i := range canonical {
						if i < len(mapped) && mapped[i].Confidence > 0 && mapped[i].EndMS > mapped[i].StartMS {
							canonical[i].StartMS = mapped[i].StartMS
							canonical[i].EndMS = mapped[i].EndMS
							canonical[i].Confidence = mapped[i].Confidence
							matched++
						}
					}
					if matched > 0 {
						matchedTotal += matched
						cursor = next
						idx := chapterIndex[chapter.ID]
						ch := &out.Chapters[idx]
						if ch.EndMS == 0 || start < ch.StartMS {
							ch.StartMS = start
						}
						if end > ch.EndMS {
							ch.EndMS = end
						}
					}
				}
				sentence := transcript.Sentence{
					ID: sourceSentence.ID, ParagraphID: block.ID,
					StartMS: start, EndMS: end, Words: canonical,
				}
				if !ok {
					sentence.StartMS, sentence.EndMS = 0, 0
				}
				out.Sentences.Sentences = append(out.Sentences.Sentences, sentence)
			}
		}
	}
	out.Sentences.Version = 1
	out.Sentences.DurationMS = acoustic.DurationMS
	out.TotalWords = total
	out.MatchedWords = matchedTotal
	if total > 0 {
		out.Quality = float64(matchedTotal) / float64(total)
	}
	interpolateSentenceWindows(out.Sentences.Sentences, acoustic.DurationMS)
	return out
}

func flattenAudio(doc transcript.Document) []audioWord {
	var out []audioWord
	for _, sentence := range doc.Sentences {
		for _, word := range sentence.Words {
			out = append(out, audioWord{token: word, norm: norm(word.Text)})
		}
	}
	return out
}

func normalizeWords(words []string) []string {
	out := make([]string, len(words))
	for i, word := range words {
		out[i] = norm(word)
	}
	return out
}

func hasEmpty(words []audioWord) bool {
	for _, word := range words {
		if word.norm == "" {
			return true
		}
	}
	return false
}

func phraseHash(words []audioWord) uint64 {
	h := uint64(14695981039346656037)
	for _, word := range words {
		for i := 0; i < len(word.norm); i++ {
			h ^= uint64(word.norm[i])
			h *= 1099511628211
		}
		h ^= 255
		h *= 1099511628211
	}
	return h
}

func wordsHash(words []string) uint64 {
	h := uint64(14695981039346656037)
	for _, word := range words {
		for i := 0; i < len(word); i++ {
			h ^= uint64(word[i])
			h *= 1099511628211
		}
		h ^= 255
		h *= 1099511628211
	}
	return h
}

func alignSentence(bookWords []string, audio []audioWord, index map[uint64][]int, cursor int) (
	[]CanonToken, int64, int64, int, bool,
) {
	if len(bookWords) == 0 || len(audio) == 0 {
		return nil, 0, 0, cursor, false
	}
	if cursor > len(audio) {
		cursor = len(audio)
	}
	type candidate struct {
		start int
		votes int
	}
	candidates := map[int]int{}
	if len(bookWords) >= anchorWidth {
		lastOffset := min(24, len(bookWords)-anchorWidth)
		for offset := 0; offset <= lastOffset; offset += 2 {
			gram := bookWords[offset : offset+anchorWidth]
			for _, pos := range index[wordsHash(gram)] {
				start := pos - offset
				if start < max(0, cursor-3) || start > cursor+maxAnchorAhead {
					continue
				}
				candidates[start]++
			}
		}
	}

	// Short headings and short sentences cannot provide a four-token anchor.
	// Search only a small region around the cursor to avoid common-word jumps.
	if len(candidates) == 0 && len(bookWords) < anchorWidth {
		lookahead := min(len(audio), cursor+600)
		for start := cursor; start < lookahead; start++ {
			if equalPrefix(bookWords, audio, start) {
				candidates[start]++
			}
		}
	}
	if len(candidates) == 0 {
		return nil, 0, 0, cursor, false
	}
	ordered := make([]candidate, 0, len(candidates))
	for start, votes := range candidates {
		ordered = append(ordered, candidate{start: start, votes: votes})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].votes != ordered[j].votes {
			return ordered[i].votes > ordered[j].votes
		}
		return ordered[i].start < ordered[j].start
	})
	if len(ordered) > maxCandidates {
		ordered = ordered[:maxCandidates]
	}

	windowExtra := max(8, len(bookWords)/4)
	windowLen := min(len(audio), len(bookWords)+windowExtra)
	bestCount, bestStart := 0, -1
	var best []CanonToken
	for _, candidate := range ordered {
		start := candidate.start
		if start < 0 || start >= len(audio) {
			continue
		}
		end := min(len(audio), start+windowLen)
		timed := make([]TimedToken, end-start)
		for i := start; i < end; i++ {
			timed[i-start] = TimedToken{
				Text: audio[i].token.Text, StartMS: audio[i].token.StartMS, EndMS: audio[i].token.EndMS,
			}
		}
		got := AlignTokens(bookWords, timed)
		count := 0
		for _, token := range got {
			if token.Confidence > 0 && token.EndMS > token.StartMS {
				count++
			}
		}
		if count > bestCount || count == bestCount && (bestStart < 0 || start < bestStart) {
			bestCount, bestStart, best = count, start, got
		}
	}
	if bestCount == 0 || float64(bestCount)/float64(len(bookWords)) < 0.30 {
		return nil, 0, 0, cursor, false
	}
	var startMS, endMS int64
	hasStart := false
	next := cursor
	for _, token := range best {
		if token.Confidence == 0 || token.EndMS <= token.StartMS {
			continue
		}
		if !hasStart || token.StartMS < startMS {
			startMS = token.StartMS
			hasStart = true
		}
		if token.EndMS > endMS {
			endMS = token.EndMS
		}
		for i := bestStart; i < len(audio) && audio[i].token.StartMS <= token.StartMS; i++ {
			if audio[i].token.StartMS == token.StartMS && audio[i].token.EndMS == token.EndMS {
				next = max(next, i+1)
				break
			}
		}
	}
	return best, startMS, endMS, next, endMS > startMS
}

func equalPrefix(bookWords []string, audio []audioWord, start int) bool {
	if start+len(bookWords) > len(audio) {
		return false
	}
	for i, word := range bookWords {
		if word == "" || word != audio[start+i].norm {
			return false
		}
	}
	return true
}

// Unmatched canonical sentences retain their display text. Their sentence
// windows are interpolated between neighboring mapped sentences solely so the
// chapter/window reader can include the text; unmatched word tokens remain
// untimed and therefore never receive a moving highlight.
func interpolateSentenceWindows(sentences []transcript.Sentence, durationMS int64) {
	hasAnyTiming := false
	for _, sentence := range sentences {
		if sentenceHasTiming(sentence) {
			hasAnyTiming = true
			break
		}
	}
	if !hasAnyTiming {
		return
	}
	for i := 0; i < len(sentences); {
		if sentenceHasTiming(sentences[i]) {
			i++
			continue
		}
		start := i
		for i < len(sentences) && !sentenceHasTiming(sentences[i]) {
			i++
		}
		end := i
		left := int64(0)
		right := durationMS
		if start > 0 {
			left = sentences[start-1].EndMS
		}
		if end < len(sentences) {
			right = sentences[end].StartMS
		}
		if right < left {
			continue
		}
		count := end - start
		span := right - left
		for n := 0; n < count; n++ {
			a := left + span*int64(n)/int64(count+1)
			b := left + span*int64(n+1)/int64(count+1)
			sentences[start+n].StartMS = a
			sentences[start+n].EndMS = max(a+1, b)
		}
	}
}

func sentenceHasTiming(sentence transcript.Sentence) bool {
	for _, word := range sentence.Words {
		if word.Confidence > 0 && word.EndMS > word.StartMS {
			return true
		}
	}
	return false
}

func (d EbookAlignment) QualityLabel() string {
	switch {
	case d.Quality >= 0.90:
		return "high"
	case d.Quality >= 0.75:
		return "medium"
	default:
		return "low"
	}
}

func (d EbookAlignment) Summary() string {
	return strings.TrimSpace(d.QualityLabel())
}
