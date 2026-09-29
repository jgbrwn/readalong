package transcript

import "testing"

func TestMergeChunksRemovesOverlappedWordsAndUsesAbsoluteTimes(t *testing.T) {
	doc := MergeChunks([]TimedChunk{
		{StartMS: 0, Words: []TimedWord{
			{Text: "Call", StartMS: 0, EndMS: 300, Confidence: 1},
			{Text: "me", StartMS: 350, EndMS: 500, Confidence: 1},
			{Text: "Ishmael.", StartMS: 600, EndMS: 1000, Confidence: 1},
		}},
		{StartMS: 500, Words: []TimedWord{
			{Text: "me", StartMS: 0, EndMS: 150, Confidence: 1},
			{Text: "Ishmael", StartMS: 100, EndMS: 500, Confidence: 1},
			{Text: "Call", StartMS: 900, EndMS: 1200, Confidence: 1},
		}},
	}, 3000)
	if got, want := len(doc.Sentences), 2; got != want {
		t.Fatalf("sentences = %d, want %d: %#v", got, want, doc.Sentences)
	}
	if got := len(doc.Sentences[0].Words); got != 3 {
		t.Fatalf("overlap retained duplicate words, first sentence has %d", got)
	}
	if doc.Sentences[1].Words[0].StartMS != 1400 {
		t.Fatalf("second chunk time not made absolute: %#v", doc.Sentences[1].Words[0])
	}
}

func TestWindowIncludesAdjacentSentenceAndClamps(t *testing.T) {
	doc := Document{Sentences: []Sentence{
		{ID: "a", StartMS: 0, EndMS: 100},
		{ID: "b", StartMS: 200, EndMS: 300},
		{ID: "c", StartMS: 400, EndMS: 500},
		{ID: "d", StartMS: 600, EndMS: 700},
	}}
	got := Window(doc, 250, 450)
	if len(got) != 4 || got[0].ID != "a" || got[3].ID != "d" {
		t.Fatalf("unexpected buffered window: %#v", got)
	}
	if got := Window(doc, -500, 20); len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("start clamp failed: %#v", got)
	}
}

func TestSentencePunctuation(t *testing.T) {
	if !sentenceEnd("done.”") || sentenceEnd("comma,") {
		t.Fatal("sentence punctuation detection failed")
	}
}
