package pipeline

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jgbrwn/readalong/internal/align"
	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/transcript"
)

func TestUploadedAudioRunsThroughFirstUsableTranscript(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	groqServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("invalid Groq multipart request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text": "Hello there.",
			"words": []map[string]any{
				{"word": "Hello", "start": 0.2, "end": 0.5},
				{"word": "there.", "start": 0.6, "end": 1.0},
			},
		})
	}))
	defer groqServer.Close()

	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.UpsertUser(ctx, "owner-1", "reader@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	bookID, jobID := "test-book", "test-job"
	bookDir := BookDirectory(filepath.Join(dataDir, "data"), "owner-1", bookID)
	sourceDir := filepath.Join(bookDir, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "upload.wav")
	cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=5", "-c:a", "pcm_s16le", source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test audio: %v: %s", err, output)
	}
	dataRoot := filepath.Join(dataDir, "data")
	audioRel, err := filepath.Rel(dataRoot, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: jobID, OwnerUserID: "owner-1", Title: "Untitled",
		SourceKind: "upload", AudioRelPath: audioRel,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim job: found=%v err=%v", found, err)
	}
	cfg := config.Config{
		DataDir: dataRoot, GroqAPIKey: "test-key", GroqModel: "test-model",
		GroqChunkSeconds: 60, GroqOverlapSeconds: 2, MaxUploadBytes: 1 << 20,
		FFmpegBin: ffmpeg, FFprobeBin: ffprobe, YTDLPBin: "yt-dlp",
	}
	service := New(cfg, d)
	service.groq.Endpoint = groqServer.URL
	service.process(ctx, job)

	gotBook, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBook.Status != "ready" || gotBook.Title == "" || gotBook.DurationMS < 4000 || gotBook.TranscriptRelPath == "" {
		t.Fatalf("book did not become readable: %#v", gotBook)
	}
	gotJob, err := d.JobStatus(ctx, bookID)
	if err != nil || gotJob.Status != "completed" {
		t.Fatalf("job state = %#v, err=%v", gotJob, err)
	}
	path, err := safeDataPath(dataRoot, gotBook.TranscriptRelPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc transcript.Document
	if err := readGzipJSON(path, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sentences) != 1 || len(doc.Sentences[0].Words) != 2 {
		t.Fatalf("unexpected transcript: %#v", doc)
	}
	audioPath, err := safeDataPath(dataRoot, gotBook.AudioRelPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("normalized playback audio missing: %v", err)
	}
	chunks, err := d.Chunks(ctx, bookID)
	if err != nil || len(chunks) != 1 || chunks[0].Status != "completed" {
		t.Fatalf("chunk state = %#v, err=%v", chunks, err)
	}
}

func TestPairedBookRejectsInvalidEPUBBeforeDownloadingIAAudio(t *testing.T) {
	dataRoot := t.TempDir()
	d, err := db.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.UpsertUser(ctx, "ia-preflight-owner", "reader@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	bookID, jobID := "ia-preflight-book", "ia-preflight-job"
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: jobID, OwnerUserID: "ia-preflight-owner", Title: "Example",
		SourceKind: "librivox", SourceURL: "https://archive.org/compress/example/formats=64KBPS%20MP3",
		GutenbergID: "invalid-id",
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim pair job: found=%v err=%v", found, err)
	}
	dataDir := t.TempDir()
	service := New(config.Config{DataDir: dataDir, MaxUploadBytes: 1 << 20}, d)
	service.process(ctx, job)

	book, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if book.Status != "error" || !strings.Contains(book.Error, "EPUB") {
		t.Fatalf("invalid Gutenberg text did not fail early: %#v", book)
	}
	for _, name := range []string{"librivox.zip", "librivox.mp3"} {
		path := filepath.Join(BookDirectory(dataDir, "ia-preflight-owner", bookID), "source", name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid EPUB caused the audio archive to be downloaded: %s stat err=%v", name, err)
		}
	}
}

func TestLibriVoxAcquireDiagnosticOmitsSourceURL(t *testing.T) {
	dataDir := t.TempDir()
	service := New(config.Config{DataDir: dataDir, MaxUploadBytes: 1 << 20}, nil)
	book := db.Book{
		SourceKind: "librivox",
		SourceURL:  "https://example.invalid/audio.zip?token=must-not-be-logged",
	}

	_, _, err := service.acquire(context.Background(), db.Job{}, book, filepath.Join(dataDir, "book"))
	if err == nil {
		t.Fatal("unsupported archive URL unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "download chapter archive") ||
		strings.Contains(err.Error(), "example.invalid") ||
		strings.Contains(err.Error(), "must-not-be-logged") {
		t.Fatalf("acquisition diagnostic is missing its phase or contains the source URL: %q", err)
	}
}

func TestUploadedAudioAndEPUBAlignCanonicalText(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	groqServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("invalid Groq multipart request: %v", err)
		}
		if !strings.Contains(r.FormValue("prompt"), "Test Story") {
			t.Errorf("EPUB title hint missing from Groq prompt: %q", r.FormValue("prompt"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text": "Hello there.",
			"words": []map[string]any{
				{"word": "Hello", "start": 0.2, "end": 0.5},
				{"word": "there.", "start": 0.6, "end": 1.0},
			},
		})
	}))
	defer groqServer.Close()

	dataRoot := filepath.Join(t.TempDir(), "data")
	d, err := db.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.UpsertUser(ctx, "ebook-owner", "reader@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	bookID, jobID := "ebook-book", "ebook-job"
	bookDir := BookDirectory(dataRoot, "ebook-owner", bookID)
	sourceDir := filepath.Join(bookDir, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(sourceDir, "upload.wav")
	cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=5", "-c:a", "pcm_s16le", audioPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test audio: %v: %s", err, output)
	}
	epubPath := filepath.Join(sourceDir, "book.epub")
	writePipelineEPUB(t, epubPath)
	audioRel, _ := filepath.Rel(dataRoot, audioPath)
	epubRel, _ := filepath.Rel(dataRoot, epubPath)
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: jobID, OwnerUserID: "ebook-owner", Title: "Test Story",
		SourceKind: "upload", AudioRelPath: audioRel, EpubRelPath: epubRel,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim job: found=%v err=%v", found, err)
	}
	service := New(config.Config{
		DataDir: dataRoot, GroqAPIKey: "test-key", GroqModel: "test-model",
		GroqChunkSeconds: 60, GroqOverlapSeconds: 2, MaxUploadBytes: 1 << 20,
		FFmpegBin: ffmpeg, FFprobeBin: ffprobe,
	}, d)
	service.groq.Endpoint = groqServer.URL
	service.process(ctx, job)

	gotBook, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	gotJob, err := d.JobStatus(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBook.Mode != "aligned" || gotBook.Status != "ready" || gotBook.Author != "Test Author" ||
		gotBook.AlignmentRelPath == "" || gotBook.AlignmentQuality == nil || *gotBook.AlignmentQuality < 0.99 ||
		gotJob.Status != "completed" {
		t.Fatalf("paired book did not align: book=%#v job=%#v", gotBook, gotJob)
	}
	alignmentPath, err := safeDataPath(dataRoot, gotBook.AlignmentRelPath)
	if err != nil {
		t.Fatal(err)
	}
	var result align.EbookAlignment
	if err := readGzipJSON(alignmentPath, &result); err != nil {
		t.Fatal(err)
	}
	if result.Quality < 0.99 || len(result.Sentences.Sentences) != 1 ||
		result.Sentences.Sentences[0].Words[0].Text != "Hello" ||
		result.Sentences.Sentences[0].Words[0].Confidence == 0 {
		t.Fatalf("unexpected alignment artifact: %#v", result)
	}
}

func writePipelineEPUB(t *testing.T, filename string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entries := map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OPS/book.opf"/></rootfiles></container>`,
		"OPS/book.opf":           `<package><metadata><title>Test Story</title><creator>Test Author</creator><language>en</language></metadata><manifest><item id="c1" href="chapter.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c1"/></spine></package>`,
		"OPS/chapter.xhtml":      `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>Hello there.</p></body></html>`,
	}
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetranscriptionPreservesOldArtifactsUntilFreshPassSucceeds(t *testing.T) {
	ctx := context.Background()
	dataRoot := filepath.Join(t.TempDir(), "data")
	d, err := db.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.UpsertUser(ctx, "retranscribe-owner", "reader@example.org", false, false); err != nil {
		t.Fatal(err)
	}

	bookID, initialJobID := "retranscribe-book", "initial-book-job"
	bookDir := BookDirectory(dataRoot, "retranscribe-owner", bookID)
	if err := os.MkdirAll(filepath.Join(bookDir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	playbackPath := filepath.Join(bookDir, "playback.mp3")
	if err := os.WriteFile(playbackPath, []byte("ID3-test-audio"), 0600); err != nil {
		t.Fatal(err)
	}
	epubPath := filepath.Join(bookDir, "source", "book.epub")
	writePipelineEPUB(t, epubPath)
	audioRel, _ := filepath.Rel(dataRoot, playbackPath)
	epubRel, _ := filepath.Rel(dataRoot, epubPath)
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: initialJobID, OwnerUserID: "retranscribe-owner",
		Title: "Test Story", Author: "Test Author", SourceKind: "upload",
		AudioRelPath: audioRel, EpubRelPath: epubRel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetBookMedia(ctx, bookID, "Test Story", "Test Author", audioRel, 65000); err != nil {
		t.Fatal(err)
	}
	ebook, err := epub.ParseFile(epubPath)
	if err != nil {
		t.Fatal(err)
	}
	ebookJSONPath := filepath.Join(bookDir, "ebook.v1.json.gz")
	if err := writeGzipJSONAtomic(ebookJSONPath, ebook); err != nil {
		t.Fatal(err)
	}
	ebookJSONRel, _ := filepath.Rel(dataRoot, ebookJSONPath)
	if err := d.SetEbookJSONPath(ctx, bookID, ebookJSONRel); err != nil {
		t.Fatal(err)
	}

	oldTranscriptRel := filepath.Join("books", "old", "transcript.v1.json.gz")
	oldAlignmentRel := filepath.Join("books", "old", "alignment.v1.json.gz")
	oldTranscriptPath := filepath.Join(dataRoot, oldTranscriptRel)
	oldAlignmentPath := filepath.Join(dataRoot, oldAlignmentRel)
	oldTranscript := transcript.Document{Version: 1, DurationMS: 65000, Sentences: []transcript.Sentence{{
		ID: "old", StartMS: 100, EndMS: 500,
		Words: []transcript.Word{{Text: "Original", StartMS: 100, EndMS: 300, Confidence: 1}},
	}}}
	oldAlignment := align.EbookAlignment{Version: 1, Quality: 0.5}
	if err := writeGzipJSONAtomic(oldTranscriptPath, oldTranscript); err != nil {
		t.Fatal(err)
	}
	if err := writeGzipJSONAtomic(oldAlignmentPath, oldAlignment); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTranscriptPath(ctx, bookID, oldTranscriptRel); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAlignment(ctx, bookID, oldAlignmentRel, oldAlignment.Quality); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(ctx, initialJobID); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveProgress(ctx, "retranscribe-owner", bookID, 4321, 1.25, 100, `{"theme":"dark"}`); err != nil {
		t.Fatal(err)
	}

	fakeFFmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
	if err := os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nfor arg do last=\"$arg\"; done\nprintf 'fLaC-test' > \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	newGroqServer := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != http.StatusOK {
				http.Error(w, "temporary test failure", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"text": "Fresh words.",
				"words": []map[string]any{
					{"word": "Fresh", "start": 0.2, "end": 0.5},
					{"word": "words.", "start": 0.6, "end": 1.0},
				},
			})
		}))
	}
	cfg := config.Config{
		DataDir: dataRoot, GroqAPIKey: "test-key", GroqModel: "test-model",
		GroqChunkSeconds: 60, GroqOverlapSeconds: 2, MaxUploadBytes: 1 << 20,
		FFmpegBin: fakeFFmpeg,
	}

	if err := d.QueueRetranscription(ctx, "retranscribe-owner", bookID, "failed-fresh-job"); err != nil {
		t.Fatal(err)
	}
	failedJob, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim failed transcription job: found=%v err=%v", found, err)
	}
	failedServer := newGroqServer(http.StatusBadGateway)
	failedService := New(cfg, d)
	failedService.groq.Endpoint = failedServer.URL
	book, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	failedService.retranscribe(ctx, failedJob, book)
	failedServer.Close()

	afterFailure, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Status != "ready" || afterFailure.TranscriptRelPath != oldTranscriptRel ||
		afterFailure.AlignmentRelPath != oldAlignmentRel {
		t.Fatalf("failed fresh pass replaced prior artifacts: %#v", afterFailure)
	}
	var preserved transcript.Document
	if err := readGzipJSON(oldTranscriptPath, &preserved); err != nil ||
		len(preserved.Sentences) != 1 || preserved.Sentences[0].Words[0].Text != "Original" {
		t.Fatalf("prior transcript was damaged after failure: err=%v doc=%#v", err, preserved)
	}

	if err := d.QueueRetranscription(ctx, "retranscribe-owner", bookID, "successful-fresh-job"); err != nil {
		t.Fatal(err)
	}
	freshJob, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim fresh transcription job: found=%v err=%v", found, err)
	}
	successServer := newGroqServer(http.StatusOK)
	successService := New(cfg, d)
	successService.groq.Endpoint = successServer.URL
	book, err = d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	successService.retranscribe(ctx, freshJob, book)
	successServer.Close()

	freshBook, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	freshJobStatus, err := d.JobStatus(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if freshJobStatus.Status != "completed" || freshJobStatus.Kind != "retranscribe" ||
		freshBook.Status != "ready" || freshBook.TranscriptRelPath == oldTranscriptRel ||
		freshBook.AlignmentRelPath == oldAlignmentRel {
		t.Fatalf("fresh transcript did not publish after success: book=%#v job=%#v", freshBook, freshJobStatus)
	}
	var freshDoc transcript.Document
	freshPath, err := safeDataPath(dataRoot, freshBook.TranscriptRelPath)
	if err != nil || readGzipJSON(freshPath, &freshDoc) != nil {
		t.Fatalf("could not read published transcript: pathErr=%v", err)
	}
	if len(freshDoc.Sentences) == 0 || freshDoc.Sentences[0].Words[0].Text != "Fresh" {
		t.Fatalf("published transcript is not the fresh Groq result: %#v", freshDoc)
	}
	var progress int64
	if err := d.QueryRowContext(ctx, `SELECT position_ms FROM reading_progress WHERE user_id=? AND book_id=?`,
		"retranscribe-owner", bookID).Scan(&progress); err != nil {
		t.Fatal(err)
	}
	if progress != 4321 {
		t.Fatalf("re-transcription changed saved reading position: %d", progress)
	}
	if _, err := os.Stat(oldTranscriptPath); err != nil {
		t.Fatalf("old artifact was unexpectedly deleted: %v", err)
	}

	previousFreshTranscript := freshBook.TranscriptRelPath
	if err := d.QueueRetranscription(ctx, "retranscribe-owner", bookID, "rate-limited-fresh-job"); err != nil {
		t.Fatal(err)
	}
	rateCalls := 0
	rateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateCalls++
		if rateCalls == 2 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text": "Resumed words.",
			"words": []map[string]any{
				{"word": "Resumed", "start": 0.2, "end": 0.5},
				{"word": "words.", "start": 0.6, "end": 1.0},
			},
		})
	}))
	defer rateServer.Close()
	rateService := New(cfg, d)
	rateService.groq.Endpoint = rateServer.URL
	rateJob, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim rate-limited retranscription: found=%v err=%v", found, err)
	}
	book, err = d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	rateService.retranscribe(ctx, rateJob, book)
	deferred, err := d.JobStatus(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	book, err = d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Status != "queued" || deferred.Stage != "rate_limited" ||
		book.TranscriptRelPath != previousFreshTranscript {
		t.Fatalf("rate limit replaced current transcript or failed to defer: book=%#v job=%#v", book, deferred)
	}
	time.Sleep(1100 * time.Millisecond)
	rateJob, found, err = d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim resumed retranscription: found=%v err=%v", found, err)
	}
	rateService.retranscribe(ctx, rateJob, book)
	resumed, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	resumedJob, err := d.JobStatus(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if resumedJob.Status != "completed" || resumed.TranscriptRelPath == previousFreshTranscript || rateCalls != 3 {
		t.Fatalf("resume did not reuse the completed first chunk: book=%#v job=%#v calls=%d",
			resumed, resumedJob, rateCalls)
	}
}

func TestRateLimitQueuesResumeAndKeepsFirstSectionReadable(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	var calls atomic.Int32
	groqServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":  "First section.",
			"words": []map[string]any{{"word": "First", "start": 0.2, "end": 0.5}, {"word": "section.", "start": 0.6, "end": 1.0}},
		})
	}))
	defer groqServer.Close()

	dataRoot := filepath.Join(t.TempDir(), "data")
	d, err := db.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.UpsertUser(ctx, "owner-2", "reader@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	bookID, jobID := "rate-book", "rate-job"
	bookDir := BookDirectory(dataRoot, "owner-2", bookID)
	sourceDir := filepath.Join(bookDir, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "upload.wav")
	cmd := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=61", "-c:a", "pcm_s16le", source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test audio: %v: %s", err, output)
	}
	audioRel, err := filepath.Rel(dataRoot, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: jobID, OwnerUserID: "owner-2", Title: "Long audio",
		SourceKind: "upload", AudioRelPath: audioRel,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim job: found=%v err=%v", found, err)
	}
	service := New(config.Config{
		DataDir: dataRoot, GroqAPIKey: "test-key", GroqModel: "test-model",
		GroqChunkSeconds: 60, GroqOverlapSeconds: 2, MaxUploadBytes: 1 << 20,
		FFmpegBin: ffmpeg, FFprobeBin: ffprobe,
	}, d)
	service.groq.Endpoint = groqServer.URL
	service.process(ctx, job)

	gotBook, err := d.BookByID(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	gotJob, err := d.JobStatus(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := d.Chunks(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBook.Status != "ready" || gotBook.TranscriptRelPath == "" ||
		gotJob.Status != "queued" || gotJob.Stage != "rate_limited" ||
		!strings.Contains(gotJob.Error, "queued to resume") ||
		len(chunks) != 2 || chunks[0].Status != "completed" || chunks[1].Status != "queued" ||
		chunks[1].NotBeforeAt == "" {
		t.Fatalf("rate-limit state not preserved: book=%#v job=%#v chunks=%#v", gotBook, gotJob, chunks)
	}
}
