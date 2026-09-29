package transcript

import (
	"sort"
	"strings"
	"unicode"
)

const (
	smallTimestampJitterMS = int64(150)
	overlapToleranceMS     = int64(750)
	singleWordOverlapMS    = int64(120)
)

type TimedWord struct {
	Text       string
	StartMS    int64
	EndMS      int64
	Confidence float64
}

type TimedChunk struct {
	StartMS int64
	Words   []TimedWord
}

// MergeChunks converts chunk-local timestamps to absolute media time and removes
// repeated leading words from adjacent overlapping chunks.
func MergeChunks(chunks []TimedChunk, durationMS int64) Document {
	var merged []TimedWord
	for _, chunk := range chunks {
		words := make([]TimedWord, 0, len(chunk.Words))
		for _, word := range chunk.Words {
			if strings.TrimSpace(word.Text) == "" || word.EndMS <= word.StartMS {
				continue
			}
			word.StartMS += chunk.StartMS
			word.EndMS += chunk.StartMS
			if word.StartMS < 0 {
				word.StartMS = 0
			}
			if durationMS > 0 && word.StartMS >= durationMS {
				continue
			}
			if durationMS > 0 && word.EndMS > durationMS {
				word.EndMS = durationMS
			}
			words = append(words, word)
		}
		drop := overlapPrefix(merged, words)
		for _, word := range words[drop:] {
			if duplicateNearTail(merged, word) {
				continue
			}
			if len(merged) > 0 && word.StartMS <= merged[len(merged)-1].StartMS {
				previousStart := merged[len(merged)-1].StartMS
				if previousStart-word.StartMS > smallTimestampJitterMS {
					// Keep Groq's lexical order. A bad boundary is safer left
					// untimed than moved in front of the preceding spoken word.
					word.Confidence = 0
				}
				word.StartMS = previousStart + 1
			}
			if word.EndMS <= word.StartMS {
				word.EndMS = word.StartMS + 1
				word.Confidence = 0
			}
			if durationMS > 0 {
				if word.StartMS >= durationMS {
					continue
				}
				if word.EndMS > durationMS {
					word.EndMS = durationMS
				}
			}
			merged = append(merged, word)
		}
	}
	return Document{Version: 1, DurationMS: durationMS, Sentences: sentences(merged)}
}

func overlapPrefix(previous, next []TimedWord) int {
	max := min(25, min(len(previous), len(next)))
	for n := max; n > 0; n-- {
		match := true
		tolerance := overlapToleranceMS
		if n == 1 {
			tolerance = singleWordOverlapMS
		}
		for i := 0; i < n; i++ {
			a := previous[len(previous)-n+i]
			b := next[i]
			if tokenKey(a.Text) == "" || tokenKey(a.Text) != tokenKey(b.Text) ||
				abs64(a.StartMS-b.StartMS) > tolerance ||
				abs64(a.EndMS-b.EndMS) > tolerance {
				match = false
				break
			}
		}
		if match {
			return n
		}
	}
	return 0
}

func duplicateNearTail(previous []TimedWord, word TimedWord) bool {
	for i := len(previous) - 1; i >= 0 && i >= len(previous)-25; i-- {
		p := previous[i]
		if p.StartMS < word.StartMS-singleWordOverlapMS {
			break
		}
		if tokenKey(p.Text) == tokenKey(word.Text) && tokenKey(word.Text) != "" &&
			abs64(p.StartMS-word.StartMS) <= singleWordOverlapMS &&
			abs64(p.EndMS-word.EndMS) <= 2*singleWordOverlapMS {
			return true
		}
	}
	return false
}

func tokenKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func sentences(words []TimedWord) []Sentence {
	var out []Sentence
	var current []Word
	var start, end int64
	flush := func() {
		if len(current) == 0 {
			return
		}
		if len(out) > 0 && end < out[len(out)-1].EndMS {
			end = out[len(out)-1].EndMS
		}
		out = append(out, Sentence{
			ID: "s" + itoa(len(out)+1), ParagraphID: "s" + itoa(len(out)+1), StartMS: start, EndMS: end,
			Words: append([]Word(nil), current...),
		})
		current = nil
	}
	for _, word := range words {
		if len(current) > 0 && (word.StartMS-end > 1400 || sentenceEnd(current[len(current)-1].Text)) {
			flush()
		}
		if len(current) == 0 {
			start = word.StartMS
		}
		current = append(current, Word{Text: word.Text, StartMS: word.StartMS, EndMS: word.EndMS, Confidence: word.Confidence})
		end = max(end, word.EndMS)
		if len(current) >= 40 {
			flush()
		}
	}
	flush()
	return out
}

func sentenceEnd(s string) bool {
	runes := []rune(strings.TrimSpace(s))
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			switch r {
			case '.', '?', '!', '…':
				return true
			case '"', '\'', '”', '’', ')', ']', '}':
				continue
			}
			return false
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func Window(doc Document, startMS, endMS int64) []Sentence {
	if startMS < 0 {
		startMS = 0
	}
	if endMS <= startMS {
		endMS = startMS + 5*60*1000
	}
	first := sort.Search(len(doc.Sentences), func(i int) bool {
		return doc.Sentences[i].EndMS >= startMS
	})
	if first >= len(doc.Sentences) || doc.Sentences[first].StartMS > endMS {
		return nil
	}
	last := first
	for last+1 < len(doc.Sentences) && doc.Sentences[last+1].StartMS <= endMS {
		last++
	}
	if first > 0 {
		first--
	}
	if last+1 < len(doc.Sentences) {
		last++
	}
	return doc.Sentences[first : last+1]
}
