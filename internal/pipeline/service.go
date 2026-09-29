package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/groq"
	"github.com/jgbrwn/readalong/internal/media"
	"github.com/jgbrwn/readalong/internal/transcript"
)

const groqMaxChunkBytes = 24_000_000

type Service struct {
	cfg   config.Config
	db    *db.DB
	tools media.Tools
	groq  groq.Client

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(cfg config.Config, d *db.DB) *Service {
	return &Service{
		cfg: cfg, db: d,
		tools: media.Tools{
			YTDLP: cfg.YTDLPBin, FFMPEG: cfg.FFmpegBin, FFPROBE: cfg.FFprobeBin, Deno: cfg.DenoBin,
			MaxBytes: cfg.MaxUploadBytes,
		},
		groq:   groq.Client{APIKey: cfg.GroqAPIKey, Model: cfg.GroqModel, Language: cfg.GroqLanguage},
		active: make(map[string]context.CancelFunc),
	}
}

func (s *Service) Run(ctx context.Context) {
	if err := s.db.ResetInterruptedJobs(ctx); err != nil {
		log.Printf("readalong: could not reset interrupted jobs")
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, found, err := s.db.ClaimNextJob(ctx)
		if err != nil {
			log.Printf("readalong: job queue unavailable")
		} else if found {
			jobCtx, cancel := context.WithCancel(ctx)
			s.mu.Lock()
			s.active[job.BookID] = cancel
			s.mu.Unlock()
			s.process(jobCtx, job)
			cancel()
			s.mu.Lock()
			delete(s.active, job.BookID)
			s.mu.Unlock()
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) CancelBook(bookID string) {
	s.mu.Lock()
	cancel := s.active[bookID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) process(ctx context.Context, job db.Job) {
	stage := "acquiring"
	book, err := s.db.BookByID(ctx, job.BookID)
	if err != nil {
		return
	}
	bookDir := BookDirectory(s.cfg.DataDir, book.OwnerUserID, book.ID)
	if err = os.MkdirAll(filepath.Join(bookDir, "source"), 0700); err != nil {
		s.fail(ctx, job, stage, book, "Could not prepare storage for this book.")
		return
	}
	_ = os.MkdirAll(filepath.Join(bookDir, "work"), 0700)

	sourcePath, metadata, err := s.acquire(ctx, job, book, bookDir)
	if err != nil {
		if ctx.Err() == nil {
			s.fail(ctx, job, stage, book, "Could not acquire the audio. Check the URL or uploaded file and retry.")
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	if err := media.ValidateAudioFile(sourcePath); err != nil {
		s.fail(ctx, job, stage, book, "The selected media is not a supported audio file.")
		return
	}
	sourceRel, err := filepath.Rel(s.cfg.DataDir, sourcePath)
	if err != nil || s.db.SetBookAudioPath(ctx, book.ID, sourceRel) != nil {
		s.fail(ctx, job, stage, book, "Could not save audio metadata.")
		return
	}

	probe, err := s.tools.Probe(ctx, sourcePath)
	if err != nil {
		s.fail(ctx, job, stage, book, "The selected media file could not be read.")
		return
	}
	durationMS, err := durationMillis(probe.Format.Duration)
	if err != nil || durationMS <= 0 {
		s.fail(ctx, job, stage, book, "The audio duration could not be determined.")
		return
	}
	title, author := chooseMetadata(metadata, probe)
	if book.Title != "" && book.Title != "Untitled" {
		title = book.Title
	}
	if title == "" {
		title = book.Title
		if title == "" {
			title = "Untitled"
		}
	}
	if err := s.db.SetBookMedia(ctx, book.ID, title, author, sourceRel, durationMS); err != nil {
		s.fail(ctx, job, stage, book, "Could not save audio metadata.")
		return
	}
	if err := s.saveChapters(ctx, book.ID, durationMS, probe); err != nil {
		s.fail(ctx, job, stage, book, "Could not save chapter metadata.")
		return
	}
	s.removeYTDLPInfo(bookDir)

	stage = "normalizing"
	if err := s.db.SetJobProgress(ctx, job.ID, stage, "acquiring", 0.12); err != nil {
		return
	}
	playbackPath := filepath.Join(bookDir, "playback.mp3")
	if fileMissingOrEmpty(playbackPath) {
		tmp := filepath.Join(bookDir, "playback.partial.mp3")
		_ = os.Remove(tmp)
		normalizeCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
		err = s.tools.Normalize(normalizeCtx, sourcePath, tmp)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.fail(ctx, job, stage, book, "Audio conversion failed. The source format may not be supported.")
			}
			return
		}
		if err := os.Rename(tmp, playbackPath); err != nil {
			s.fail(ctx, job, stage, book, "Could not finalize the playback audio.")
			return
		}
	}
	playbackRel, err := filepath.Rel(s.cfg.DataDir, playbackPath)
	if err != nil {
		s.fail(ctx, job, stage, book, "Could not save playback metadata.")
		return
	}
	if err := s.db.SetBookMedia(ctx, book.ID, title, author, playbackRel, durationMS); err != nil {
		s.fail(ctx, job, stage, book, "Could not save playback metadata.")
		return
	}

	if strings.TrimSpace(s.cfg.GroqAPIKey) == "" {
		s.fail(ctx, job, "transcribing", book, "GROQ_API_KEY is not configured on the server.")
		return
	}
	chunks, err := s.ensureChunks(ctx, book.ID, bookDir, durationMS)
	if err != nil || len(chunks) == 0 {
		s.fail(ctx, job, "transcribing", book, "Could not prepare audio for transcription.")
		return
	}
	stage = "transcribing"
	if err := s.db.SetJobProgress(ctx, job.ID, stage, "transcribing", 0.16); err != nil {
		return
	}
	s.transcribe(ctx, job, book, bookDir, playbackPath, chunks)
}

func (s *Service) acquire(ctx context.Context, job db.Job, book db.Book, bookDir string) (string, media.SourceMetadata, error) {
	var metadata media.SourceMetadata
	if book.AudioRelPath != "" {
		path, err := safeDataPath(s.cfg.DataDir, book.AudioRelPath)
		return path, metadata, err
	}
	sourceDir := filepath.Join(bookDir, "source")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	var path string
	switch book.SourceKind {
	case "youtube":
		if path, _ = media.FindYTDLPSource(sourceDir); path == "" {
			if err := s.tools.AcquireWithYTDLP(ctx, book.SourceURL, sourceDir); err != nil {
				return "", metadata, err
			}
			path, _ = media.FindYTDLPSource(sourceDir)
		}
		metadata, _ = media.ReadYTDLPMetadata(sourceDir)
	case "url":
		path = filepath.Join(sourceDir, "source.download")
		if fileMissingOrEmpty(path) {
			if err := media.DownloadURL(ctx, book.SourceURL, path, s.cfg.MaxUploadBytes); err != nil {
				return "", metadata, err
			}
		}
	default:
		return "", metadata, fmt.Errorf("uploaded audio file is missing")
	}
	if path == "" {
		return "", metadata, fmt.Errorf("downloaded audio is missing")
	}
	rel, err := filepath.Rel(s.cfg.DataDir, path)
	if err != nil {
		return "", metadata, err
	}
	if err := s.db.SetBookAudioPath(ctx, book.ID, rel); err != nil {
		return "", metadata, err
	}
	return path, metadata, nil
}

func (s *Service) ensureChunks(ctx context.Context, bookID, bookDir string, durationMS int64) ([]db.Chunk, error) {
	seconds := s.cfg.GroqChunkSeconds
	if seconds < 60 {
		seconds = 480
	}
	overlap := s.cfg.GroqOverlapSeconds
	if overlap < 0 || overlap >= seconds {
		overlap = 2
	}
	chunkMS := int64(seconds) * 1000
	stepMS := chunkMS - int64(overlap)*1000
	var chunks []db.Chunk
	for start, ordinal := int64(0), 0; start < durationMS; start, ordinal = start+stepMS, ordinal+1 {
		end := min(start+chunkMS, durationMS)
		base := filepath.Join(bookDir, "work", fmt.Sprintf("chunk-%05d", ordinal))
		audioRel, err := filepath.Rel(s.cfg.DataDir, base+".flac")
		if err != nil {
			return nil, err
		}
		responseRel, err := filepath.Rel(s.cfg.DataDir, base+".json.gz")
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, db.Chunk{
			ID: fmt.Sprintf("%s-%05d", bookID, ordinal), BookID: bookID, Ordinal: ordinal,
			StartMS: start, EndMS: end, Status: "queued", AudioRelPath: audioRel,
			ResponseRelPath: responseRel,
		})
		if end == durationMS {
			break
		}
	}
	if err := s.db.EnsureChunks(ctx, chunks); err != nil {
		return nil, err
	}
	return s.db.Chunks(ctx, bookID)
}

func (s *Service) transcribe(ctx context.Context, job db.Job, book db.Book, bookDir, playbackPath string, chunks []db.Chunk) {
	responses := make(map[int]groq.Response, len(chunks))
	completed := 0
	for _, chunk := range chunks {
		if chunk.Status != "completed" || chunk.ResponseRelPath == "" {
			continue
		}
		path, err := safeDataPath(s.cfg.DataDir, chunk.ResponseRelPath)
		if err != nil {
			continue
		}
		var response groq.Response
		if readGzipJSON(path, &response) == nil && len(response.Words) > 0 {
			responses[chunk.Ordinal] = response
			completed++
		} else {
			chunk.Status = "queued"
			chunk.Error = ""
			chunk.ResponseRelPath = ""
			_ = s.db.UpdateChunk(ctx, chunk)
		}
	}
	if err := s.writeTranscript(ctx, book.ID, bookDir, chunks, responses, book.DurationMS); err != nil {
		s.fail(ctx, job, "transcribing", book, "Could not save the transcription.")
		return
	}

	for _, chunk := range chunks {
		if _, ok := responses[chunk.Ordinal]; ok {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		bookStatus := "transcribing"
		if completed > 0 {
			bookStatus = "ready"
		}
		_ = s.db.SetJobProgress(ctx, job.ID, "transcribing", bookStatus, progress(completed, len(chunks)))
		chunk.Status = "running"
		chunk.Error = ""
		if err := s.db.UpdateChunk(ctx, chunk); err != nil {
			return
		}
		audioPath, err := safeDataPath(s.cfg.DataDir, chunk.AudioRelPath)
		if err != nil {
			s.fail(ctx, job, "transcribing", book, "Could not prepare audio for transcription.")
			return
		}
		if fileMissingOrEmpty(audioPath) {
			if err := os.MkdirAll(filepath.Dir(audioPath), 0700); err != nil {
				s.fail(ctx, job, "transcribing", book, "Could not prepare audio for transcription.")
				return
			}
			if err := s.tools.CreateChunk(ctx, playbackPath, audioPath, chunk.StartMS, chunk.EndMS); err != nil {
				chunk.Status, chunk.Error = "error", "Chunk preparation failed."
				_ = s.db.UpdateChunk(ctx, chunk)
				s.fail(ctx, job, "transcribing", book, "Could not prepare audio for transcription.")
				return
			}
		}
		if st, err := os.Stat(audioPath); err != nil || st.Size() > groqMaxChunkBytes {
			chunk.Status, chunk.Error = "error", "Audio chunk exceeds the transcription upload limit."
			_ = s.db.UpdateChunk(ctx, chunk)
			s.fail(ctx, job, "transcribing", book, "Audio chunk exceeds the transcription upload limit.")
			return
		}
		response, err := s.groq.TranscribeFile(ctx, audioPath, "")
		if err != nil {
			var limited *groq.RateLimitError
			if errors.As(err, &limited) {
				retryAt := time.Now().Add(limited.RetryAfter).UTC().Format(time.RFC3339)
				chunk.Status, chunk.NotBeforeAt, chunk.Error = "queued", retryAt, ""
				_ = s.db.UpdateChunk(ctx, chunk)
				_ = s.db.DeferJob(ctx, job.ID, "rate_limited", bookStatus, retryAt, progress(completed, len(chunks)))
				return
			}
			chunk.Status, chunk.Error = "error", "Transcription failed."
			_ = s.db.UpdateChunk(ctx, chunk)
			s.fail(ctx, job, "transcribing", book, "Transcription failed. Check the Groq configuration and retry.")
			return
		}
		if len(response.Words) == 0 {
			chunk.Status, chunk.Error = "error", "No word timestamps returned."
			_ = s.db.UpdateChunk(ctx, chunk)
			s.fail(ctx, job, "transcribing", book, "The transcription service returned no word timestamps.")
			return
		}
		responsePath, err := safeDataPath(s.cfg.DataDir, chunk.ResponseRelPath)
		if err != nil || writeGzipJSONAtomic(responsePath, response) != nil {
			chunk.Status, chunk.Error = "error", "Could not save transcription."
			_ = s.db.UpdateChunk(ctx, chunk)
			s.fail(ctx, job, "transcribing", book, "Could not save the transcription.")
			return
		}
		chunk.Status, chunk.Error, chunk.NotBeforeAt = "completed", "", ""
		if err := s.db.UpdateChunk(ctx, chunk); err != nil {
			return
		}
		_ = os.Remove(audioPath)
		responses[chunk.Ordinal] = response
		completed++
		if err := s.writeTranscript(ctx, book.ID, bookDir, chunks, responses, book.DurationMS); err != nil {
			s.fail(ctx, job, "transcribing", book, "Could not save the transcription.")
			return
		}
		bookStatus = "transcribing"
		if completed > 0 {
			bookStatus = "ready"
		}
		_ = s.db.SetJobProgress(ctx, job.ID, "transcribing", bookStatus, progress(completed, len(chunks)))
	}
	if completed == len(chunks) {
		_ = s.db.CompleteJob(ctx, job.ID)
	}
}

func (s *Service) writeTranscript(ctx context.Context, bookID, bookDir string, chunks []db.Chunk,
	responses map[int]groq.Response, durationMS int64) error {
	if len(responses) == 0 {
		return nil
	}
	timed := make([]transcript.TimedChunk, 0, len(responses))
	for _, c := range chunks {
		resp, ok := responses[c.Ordinal]
		if !ok {
			continue
		}
		chunk := transcript.TimedChunk{StartMS: c.StartMS}
		for _, word := range resp.Words {
			start := int64(math.Round(word.Start * 1000))
			end := int64(math.Round(word.End * 1000))
			if end <= start {
				continue
			}
			chunk.Words = append(chunk.Words, transcript.TimedWord{
				Text: word.Word, StartMS: start, EndMS: end, Confidence: 1,
			})
		}
		timed = append(timed, chunk)
	}
	sort.Slice(timed, func(i, j int) bool { return timed[i].StartMS < timed[j].StartMS })
	doc := transcript.MergeChunks(timed, durationMS)
	path := filepath.Join(bookDir, "transcript.v1.json.gz")
	if err := writeGzipJSONAtomic(path, doc); err != nil {
		return err
	}
	rel, err := filepath.Rel(s.cfg.DataDir, path)
	if err != nil {
		return err
	}
	return s.db.SetTranscriptPath(ctx, bookID, rel)
}

func (s *Service) fail(ctx context.Context, job db.Job, stage string, book db.Book, message string) {
	status := "error"
	current, err := s.db.BookByID(ctx, book.ID)
	if err == nil && current.TranscriptRelPath != "" {
		status = "ready"
	}
	_ = s.db.FailJob(ctx, job.ID, stage, status, message)
	log.Printf("readalong: job %s failed at %s", job.ID, stage)
}

func (s *Service) saveChapters(ctx context.Context, bookID string, durationMS int64, p media.ProbeResult) error {
	var chapters []db.Chapter
	for i, c := range p.Chapters {
		start, err1 := durationMillis(c.StartTime)
		end, err2 := durationMillis(c.EndTime)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		title := c.Tags["title"]
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		chapters = append(chapters, db.Chapter{
			ID: fmt.Sprintf("%s-ch-%04d", bookID, i+1), BookID: bookID, Ordinal: i,
			Title: title, StartMS: start, EndMS: min(end, durationMS),
		})
	}
	if len(chapters) == 0 {
		chapters = append(chapters, db.Chapter{
			ID: bookID + "-ch-0001", BookID: bookID, Ordinal: 0,
			Title: "Full book", StartMS: 0, EndMS: durationMS,
		})
	}
	return s.db.ReplaceChapters(ctx, bookID, chapters)
}

func chooseMetadata(m media.SourceMetadata, p media.ProbeResult) (title, author string) {
	title = strings.TrimSpace(m.Title)
	author = strings.TrimSpace(m.Uploader)
	for key, value := range p.Format.Tags {
		switch strings.ToLower(key) {
		case "title":
			if title == "" {
				title = strings.TrimSpace(value)
			}
		case "artist", "album_artist":
			if author == "" {
				author = strings.TrimSpace(value)
			}
		}
	}
	return title, author
}

func (s *Service) removeYTDLPInfo(bookDir string) {
	matches, _ := filepath.Glob(filepath.Join(bookDir, "source", "*.info.json"))
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

func BookDirectory(dataDir, ownerID, bookID string) string {
	sum := sha256.Sum256([]byte(ownerID))
	return filepath.Join(dataDir, "books", hex.EncodeToString(sum[:16]), bookID)
}

func durationMillis(value string) (int64, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, fmt.Errorf("invalid media duration")
	}
	return int64(math.Round(seconds * 1000)), nil
}

func fileMissingOrEmpty(path string) bool {
	st, err := os.Stat(path)
	return err != nil || !st.Mode().IsRegular() || st.Size() == 0
}

func progress(done, total int) float64 {
	if total <= 0 {
		return 0.16
	}
	return 0.16 + 0.84*float64(done)/float64(total)
}
