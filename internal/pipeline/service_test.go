package pipeline

import (
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

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
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
