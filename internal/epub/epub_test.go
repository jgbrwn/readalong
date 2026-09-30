package epub

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEPUB(t *testing.T, entries map[string]string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func fixtureEntries() map[string]string {
	return map[string]string{
		"META-INF/container.xml":    `<?xml version="1.0"?><container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/package.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/package.opf":         `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Test Book</dc:title><dc:creator>A. Writer</dc:creator><dc:language>en</dc:language></metadata><manifest><item id="ch1" href="text/chapter1.xhtml" media-type="application/xhtml+xml"/><item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/></manifest><spine><itemref idref="nav"/><itemref idref="ch1"/></spine></package>`,
		"OEBPS/nav.xhtml":           `<html xmlns="http://www.w3.org/1999/xhtml"><body><nav><p>Navigation must not become book text.</p></nav></body></html>`,
		"OEBPS/text/chapter1.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Hidden</title></head><body><h1>Chapter One</h1><p>Hello, world! This is a test.</p><blockquote><p>“A quotation,” she said.</p></blockquote></body></html>`,
	}
}

func TestParseSpineAndExtractSemanticText(t *testing.T) {
	filename := writeEPUB(t, fixtureEntries())
	doc, err := ParseFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Test Book" || doc.Author != "A. Writer" || doc.Language != "en" {
		t.Fatalf("metadata = %#v", doc)
	}
	if len(doc.Chapters) != 1 || doc.Chapters[0].Title != "Chapter One" {
		t.Fatalf("chapters = %#v", doc.Chapters)
	}
	if doc.WordCount != 12 || len(doc.Chapters[0].Blocks) != 3 {
		t.Fatalf("word count/blocks = %d/%d", doc.WordCount, len(doc.Chapters[0].Blocks))
	}
	if got := doc.Chapters[0].Blocks[1].Sentences[0].Text; got != "Hello, world!" {
		t.Fatalf("first paragraph sentence = %q", got)
	}
	if got := doc.Chapters[0].Blocks[1].Sentences[1].Words[0]; got != "This" {
		t.Fatalf("second sentence first word = %q", got)
	}
}

func TestHintPromptUsesIdentityWithoutFutureChapterHeadings(t *testing.T) {
	doc := Document{
		Title:  "Anne of Green Gables",
		Author: "L. M. Montgomery",
		Chapters: []Chapter{
			{Title: "CHAPTER I. Mrs. Rachel Lynde Is Surprised"},
			{Title: "CHAPTER IV. Morning at Green Gables"},
		},
	}
	prompt := doc.HintPrompt()
	if !strings.Contains(prompt, doc.Title) || !strings.Contains(prompt, doc.Author) {
		t.Fatalf("prompt lost book identity: %q", prompt)
	}
	if strings.Contains(prompt, "CHAPTER I") || strings.Contains(prompt, "CHAPTER IV") {
		t.Fatalf("prompt includes chapter headings that may bias a chunk transcript: %q", prompt)
	}
	if got := (Document{}).HintPrompt(); got != "" {
		t.Fatalf("empty document hint = %q, want empty", got)
	}
}

func TestParseRejectsUnsafeArchivePaths(t *testing.T) {
	entries := fixtureEntries()
	entries["../outside.txt"] = "no"
	if _, err := ParseFile(writeEPUB(t, entries)); err == nil {
		t.Fatal("unsafe path accepted")
	}
}

func TestParseRejectsMissingSpineContent(t *testing.T) {
	entries := fixtureEntries()
	delete(entries, "OEBPS/text/chapter1.xhtml")
	if _, err := ParseFile(writeEPUB(t, entries)); err == nil {
		t.Fatal("missing spine content accepted")
	}
}

func TestTokenizePreservesUnicodeAndWordPunctuation(t *testing.T) {
	got := Tokenize("“Café,” said O’Neill — hello-world!")
	want := []string{"“Café,”", "said", "O’Neill", "hello-world!"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %#v, want %#v", got, want)
		}
	}
}
