package align

import (
	"testing"

	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/transcript"
)

func TestAlignEbookExactTextWithIntroAndParagraphs(t *testing.T) {
	book := epub.Document{Chapters: []epub.Chapter{{
		ID: "ch1", Ordinal: 0, Title: "Chapter One",
		Blocks: []epub.Block{
			{ID: "b1", Sentences: []epub.Sentence{{
				ID: "s1", Words: []string{"Hello", "world,", "this", "is", "a", "test."},
			}}},
			{ID: "b2", Sentences: []epub.Sentence{{
				ID: "s2", Words: []string{"The", "next", "paragraph", "continues", "the", "story."},
			}}},
		},
	}}}
	acoustic := transcript.Document{
		Version: 1, DurationMS: 15000,
		Sentences: []transcript.Sentence{{
			ID: "a1", Words: []transcript.Word{
				{Text: "Narrator", StartMS: 100, EndMS: 500, Confidence: 1},
				{Text: "introduction.", StartMS: 550, EndMS: 1000, Confidence: 1},
				{Text: "Hello", StartMS: 1200, EndMS: 1500, Confidence: 1},
				{Text: "world", StartMS: 1510, EndMS: 1800, Confidence: 1},
				{Text: "this", StartMS: 1810, EndMS: 2000, Confidence: 1},
				{Text: "is", StartMS: 2010, EndMS: 2100, Confidence: 1},
				{Text: "a", StartMS: 2110, EndMS: 2200, Confidence: 1},
				{Text: "test.", StartMS: 2210, EndMS: 2500, Confidence: 1},
				{Text: "The", StartMS: 5000, EndMS: 5200, Confidence: 1},
				{Text: "next", StartMS: 5210, EndMS: 5400, Confidence: 1},
				{Text: "paragraph", StartMS: 5410, EndMS: 5800, Confidence: 1},
				{Text: "continues", StartMS: 5810, EndMS: 6200, Confidence: 1},
				{Text: "the", StartMS: 6210, EndMS: 6300, Confidence: 1},
				{Text: "story.", StartMS: 6310, EndMS: 6700, Confidence: 1},
			},
		}},
	}

	result := AlignEbook(book, acoustic)
	if result.Quality != 1 || result.MatchedWords != 12 || len(result.Sentences.Sentences) != 2 {
		t.Fatalf("alignment summary = quality %.3f, matched %d/%d, sentences %d",
			result.Quality, result.MatchedWords, result.TotalWords, len(result.Sentences.Sentences))
	}
	first := result.Sentences.Sentences[0]
	if first.StartMS != 1200 || first.EndMS != 2500 || first.ParagraphID != "b1" {
		t.Fatalf("first canonical paragraph mapping = %#v", first)
	}
	if first.Words[1].Text != "world," || first.Words[1].StartMS != 1510 || first.Words[1].Confidence == 0 {
		t.Fatalf("canonical punctuation/timing was not preserved: %#v", first.Words[1])
	}
	if result.Chapters[0].StartMS != 1200 || result.Chapters[0].EndMS != 6700 {
		t.Fatalf("chapter time range = %#v", result.Chapters[0])
	}
}

func TestAlignEbookMismatchedTextGetsNoWordTiming(t *testing.T) {
	book := epub.Document{Chapters: []epub.Chapter{{
		ID: "ch1", Blocks: []epub.Block{{ID: "b1", Sentences: []epub.Sentence{{
			ID: "s1", Words: []string{"A", "completely", "different", "opening", "sentence."},
		}}}},
	}}}
	acoustic := transcript.Document{DurationMS: 5000, Sentences: []transcript.Sentence{{
		ID: "a1", Words: []transcript.Word{
			{Text: "Nothing", StartMS: 100, EndMS: 400, Confidence: 1},
			{Text: "matches", StartMS: 500, EndMS: 900, Confidence: 1},
			{Text: "this", StartMS: 1000, EndMS: 1200, Confidence: 1},
			{Text: "audio.", StartMS: 1300, EndMS: 1700, Confidence: 1},
		},
	}}}
	result := AlignEbook(book, acoustic)
	if result.Quality != 0 || result.MatchedWords != 0 {
		t.Fatalf("mismatch should not be aligned: %#v", result)
	}
	for _, word := range result.Sentences.Sentences[0].Words {
		if word.Confidence != 0 || word.EndMS != 0 {
			t.Fatalf("unmatched word received timing: %#v", word)
		}
	}
}
