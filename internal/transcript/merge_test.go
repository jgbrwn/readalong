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

func TestMergePreservesLexicalOrderWhenTimestampsRegress(t *testing.T) {
	doc := MergeChunks([]TimedChunk{{
		Words: []TimedWord{
			{Text: "first", StartMS: 1000, EndMS: 1250, Confidence: 1},
			{Text: "second", StartMS: 700, EndMS: 950, Confidence: 1},
			{Text: "third.", StartMS: 1500, EndMS: 1750, Confidence: 1},
		},
	}}, 3000)
	if len(doc.Sentences) != 1 || len(doc.Sentences[0].Words) != 3 {
		t.Fatalf("unexpected transcript: %#v", doc.Sentences)
	}
	words := doc.Sentences[0].Words
	if words[0].Text != "first" || words[1].Text != "second" || words[2].Text != "third." {
		t.Fatalf("timestamp repair reordered spoken words: %#v", words)
	}
	if words[0].StartMS >= words[1].StartMS || words[1].StartMS >= words[2].StartMS {
		t.Fatalf("word index is not chronologically searchable: %#v", words)
	}
	if words[1].Confidence != 0 {
		t.Fatalf("large timestamp regression should disable that word's highlight: %#v", words[1])
	}
}

func TestMergeKeepsRepeatedWordsWithDistinctTimings(t *testing.T) {
	doc := MergeChunks([]TimedChunk{
		{StartMS: 0, Words: []TimedWord{{Text: "very", StartMS: 0, EndMS: 140, Confidence: 1}}},
		{StartMS: 100, Words: []TimedWord{{Text: "very", StartMS: 100, EndMS: 250, Confidence: 1}}},
	}, 1000)
	if len(doc.Sentences) != 1 || len(doc.Sentences[0].Words) != 2 {
		t.Fatalf("legitimate repeated word was dropped: %#v", doc.Sentences)
	}
}

func TestMergeRepairsOrderAcrossOverlappingChunks(t *testing.T) {
	doc := MergeChunks([]TimedChunk{
		{StartMS: 0, Words: []TimedWord{
			{Text: "first", StartMS: 0, EndMS: 200, Confidence: 1},
			{Text: "second.", StartMS: 400, EndMS: 600, Confidence: 1},
		}},
		{StartMS: 300, Words: []TimedWord{
			{Text: "third", StartMS: 0, EndMS: 180, Confidence: 1},
			{Text: "fourth.", StartMS: 100, EndMS: 250, Confidence: 1},
		}},
	}, 2000)
	var got []Word
	for _, sentence := range doc.Sentences {
		got = append(got, sentence.Words...)
	}
	if len(got) != 4 || got[0].Text != "first" || got[1].Text != "second." ||
		got[2].Text != "third" || got[3].Text != "fourth." {
		t.Fatalf("chunk boundary reordered words: %#v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartMS <= got[i-1].StartMS {
			t.Fatalf("nonmonotonic repaired timestamps: %#v", got)
		}
	}
	for i := 1; i < len(doc.Sentences); i++ {
		if doc.Sentences[i].EndMS < doc.Sentences[i-1].EndMS {
			t.Fatalf("sentence windows are not monotonic: %#v", doc.Sentences)
		}
	}
}
