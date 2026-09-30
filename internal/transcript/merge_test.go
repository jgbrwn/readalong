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

func TestMergeMasksDenseASRArtifactBurstWithoutChangingRawInputs(t *testing.T) {
	raw := []TimedWord{
		{Text: "in", StartMS: 900, EndMS: 1000, Confidence: 1},
		{Text: "13340108", StartMS: 1000, EndMS: 1020, Confidence: 1},
		{Text: "e2", StartMS: 1020, EndMS: 1040, Confidence: 1},
		{Text: "mcaur6", StartMS: 1040, EndMS: 1060, Confidence: 1},
		{Text: "102401013", StartMS: 1060, EndMS: 1080, Confidence: 1},
		{Text: "xe7ft", StartMS: 1080, EndMS: 1100, Confidence: 1},
		{Text: "yeltsjf", StartMS: 1100, EndMS: 1120, Confidence: 1},
		{Text: "192", StartMS: 1120, EndMS: 1140, Confidence: 1},
		{Text: "c'hovelss", StartMS: 1130, EndMS: 1250, Confidence: 0},
		{Text: "40", StartMS: 1300, EndMS: 1350, Confidence: 1},
		{Text: "41", StartMS: 1350, EndMS: 1400, Confidence: 1},
		{Text: "The", StartMS: 1400, EndMS: 1500, Confidence: 1},
		{Text: "dwellings.", StartMS: 1500, EndMS: 1800, Confidence: 1},
	}
	doc := MergeChunks([]TimedChunk{{Words: raw}}, 3000)
	var got []Word
	for _, sentence := range doc.Sentences {
		got = append(got, sentence.Words...)
	}
	if len(got) != 4 || got[0].Text != "in" || got[1].Text != "[unclear audio]" ||
		got[2].Text != "The" || got[3].Text != "dwellings." {
		t.Fatalf("artifact burst was not safely collapsed: %#v", got)
	}
	if got[1].Confidence != 0 || got[1].StartMS != 1000 || got[1].EndMS != got[2].StartMS {
		t.Fatalf("unclear-audio marker has unexpected timing/confidence: %#v", got[1])
	}
	if raw[1].Text != "13340108" || raw[8].Text != "c'hovelss" {
		t.Fatalf("source words were modified: %#v", raw)
	}
}

func TestMergeKeepsIsolatedNumbersAndNamesButDoesNotTimeImpossibleWords(t *testing.T) {
	doc := MergeChunks([]TimedChunk{{Words: []TimedWord{
		{Text: "In", StartMS: 0, EndMS: 100, Confidence: 1},
		{Text: "1999", StartMS: 120, EndMS: 250, Confidence: 1},
		{Text: "R2D2", StartMS: 260, EndMS: 400, Confidence: 1},
		{Text: "a", StartMS: 410, EndMS: 430, Confidence: 1},
		{Text: "normal", StartMS: 440, EndMS: 700, Confidence: 1},
	}}}, 1000)
	var got []Word
	for _, sentence := range doc.Sentences {
		got = append(got, sentence.Words...)
	}
	if len(got) != 5 || got[1].Text != "1999" || got[2].Text != "R2D2" {
		t.Fatalf("isolated numbers/name were incorrectly removed: %#v", got)
	}
	if got[3].Confidence != 0 {
		t.Fatalf("implausibly short word timestamp stayed highlighted: %#v", got[3])
	}
}

func TestMergeKeepsPlausiblyTimedCodes(t *testing.T) {
	doc := MergeChunks([]TimedChunk{{Words: []TimedWord{
		{Text: "The", StartMS: 0, EndMS: 180, Confidence: 1},
		{Text: "A123BC", StartMS: 200, EndMS: 500, Confidence: 1},
		{Text: "12345678", StartMS: 520, EndMS: 900, Confidence: 1},
		{Text: "ZX90QW", StartMS: 920, EndMS: 1200, Confidence: 1},
		{Text: "7654321", StartMS: 1220, EndMS: 1520, Confidence: 1},
		{Text: "followed", StartMS: 1540, EndMS: 1900, Confidence: 1},
	}}}, 3000)
	var got []Word
	for _, sentence := range doc.Sentences {
		got = append(got, sentence.Words...)
	}
	if len(got) != 6 {
		t.Fatalf("plausible code sequence was collapsed: %#v", got)
	}
	for _, word := range got {
		if word.Text == "[unclear audio]" {
			t.Fatalf("plausibly timed code sequence was marked unclear: %#v", got)
		}
	}
}
