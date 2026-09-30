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

	"github.com/jgbrwn/readalong/internal/align"
	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/groq"
	"github.com/jgbrwn/readalong/internal/media"
	"github.com/jgbrwn/readalong/internal/transcript"
)

const groqMaxChunkBytes = 24_000_000
const maxStoredEPUBBytes = 150 << 20

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
	book, err := s.db.BookByID(ctx, job.BookID)
	if err != nil {
		return
	}
	if job.Kind == "retranscribe" {
		s.retranscribe(ctx, job, book)
		return
	}
	stage := "acquiring"
	bookDir := BookDirectory(s.cfg.DataDir, book.OwnerUserID, book.ID)
	if err = os.MkdirAll(filepath.Join(bookDir, "source"), 0700); err != nil {
		s.fail(ctx, job, stage, book, "Could not prepare storage for this book.")
		return
	}
	_ = os.MkdirAll(filepath.Join(bookDir, "work"), 0700)

	var ebook *epub.Document
	if book.Mode == "aligned" && book.SourceKind == "librivox" {
		stage = "validating_ebook"
		if err := s.db.SetJobProgress(ctx, job.ID, stage, "acquiring", 0.03); err != nil {
			return
		}
		ebook, err = s.prepareEbook(ctx, book, bookDir)
		if err != nil {
			s.fail(ctx, job, stage, book, "The selected Gutenberg EPUB was unavailable or invalid; no audio was downloaded. Choose another text edition.")
			return
		}
		stage = "acquiring"
		if err := s.db.SetJobProgress(ctx, job.ID, stage, "acquiring", 0.05); err != nil {
			return
		}
	}

	sourcePath, metadata, err := s.acquire(ctx, job, book, bookDir)
	if err != nil {
		if ctx.Err() == nil {
			message := "Could not acquire the audio. Check the URL or uploaded file and retry."
			if book.SourceKind == "librivox" {
				// The underlying media helpers deliberately omit URLs and local
				// paths from their errors, so this diagnostic is safe for the
				// private service journal and useful when Archive.org fails.
				log.Printf("readalong: job %s LibriVox audio acquisition failed: %v", job.ID, err)
				message = "Could not download or assemble the audiobook archive from Archive.org. Try again later or select another recording."
			}
			s.fail(ctx, job, stage, book, message)
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
	if book.Mode == "aligned" && book.Author != "" {
		author = book.Author
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
	book.DurationMS, book.Title, book.Author = durationMS, title, author

	if book.Mode == "aligned" {
		stage = "aligning"
		if err := s.db.SetJobProgress(ctx, job.ID, stage, "transcribing", 0.14); err != nil {
			return
		}
		if ebook == nil {
			ebook, err = s.prepareEbook(ctx, book, bookDir)
			if err != nil {
				s.fail(ctx, job, stage, book, "Could not load the paired EPUB. Retry, or add a valid Gutenberg EPUB.")
				return
			}
		}
		if ebook.Title != "" && book.Title == "Untitled" {
			title = ebook.Title
		}
		if ebook.Author != "" && book.Author == "" {
			author = ebook.Author
		}
		book.Title, book.Author = title, author
		if err := s.db.SetBookMedia(ctx, book.ID, title, author, playbackRel, durationMS); err != nil {
			s.fail(ctx, job, stage, book, "Could not save ebook metadata.")
			return
		}
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
	prompt := ""
	if ebook != nil {
		prompt = ebook.HintPrompt()
	}
	s.transcribe(ctx, job, book, bookDir, playbackPath, chunks, prompt, ebook)
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
	case "librivox":
		metadata = media.SourceMetadata{Title: book.Title, Uploader: book.Author}
		archivePath := filepath.Join(sourceDir, "librivox.zip")
		path = filepath.Join(sourceDir, "librivox.mp3")
		if fileMissingOrEmpty(path) {
			if fileMissingOrEmpty(archivePath) {
				if err := media.DownloadLibriVoxArchive(ctx, book.SourceURL, archivePath, s.cfg.MaxUploadBytes); err != nil {
					return "", metadata, fmt.Errorf("download chapter archive: %w", err)
				}
			}
			if err := media.JoinLibriVoxArchive(ctx, archivePath, path,
				filepath.Join(bookDir, "work", "librivox-tracks"), s.cfg.FFmpegBin, s.cfg.MaxUploadBytes); err != nil {
				return "", metadata, fmt.Errorf("validate or assemble chapter archive: %w", err)
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
	chunks, err := s.chunkPlan(bookID, bookID, filepath.Join(bookDir, "work"), durationMS)
	if err != nil {
		return nil, err
	}
	if err := s.db.EnsureChunks(ctx, chunks); err != nil {
		return nil, err
	}
	return s.db.Chunks(ctx, bookID)
}

func (s *Service) chunkPlan(idPrefix, bookID, outputDir string, durationMS int64) ([]db.Chunk, error) {
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
		base := filepath.Join(outputDir, fmt.Sprintf("chunk-%05d", ordinal))
		audioRel, err := filepath.Rel(s.cfg.DataDir, base+".flac")
		if err != nil {
			return nil, err
		}
		responseRel, err := filepath.Rel(s.cfg.DataDir, base+".json.gz")
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, db.Chunk{
			ID: fmt.Sprintf("%s-%05d", idPrefix, ordinal), BookID: bookID, Ordinal: ordinal,
			StartMS: start, EndMS: end, Status: "queued", AudioRelPath: audioRel,
			ResponseRelPath: responseRel,
		})
		if end == durationMS {
			break
		}
	}
	return chunks, nil
}

func (s *Service) retranscriptionChunks(ctx context.Context, jobID, bookID, runDir string, durationMS int64) ([]db.Chunk, error) {
	saved, err := s.db.Chunks(ctx, bookID)
	if err != nil {
		return nil, err
	}
	if len(saved) == 0 {
		return s.chunkPlan(jobID, bookID, runDir, durationMS)
	}
	chunks := make([]db.Chunk, 0, len(saved))
	for _, previous := range saved {
		if previous.EndMS <= previous.StartMS || previous.StartMS >= durationMS {
			continue
		}
		base := filepath.Join(runDir, fmt.Sprintf("chunk-%05d", previous.Ordinal))
		audioRel, err := filepath.Rel(s.cfg.DataDir, base+".flac")
		if err != nil {
			return nil, err
		}
		responseRel, err := filepath.Rel(s.cfg.DataDir, base+".json.gz")
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, db.Chunk{
			ID: fmt.Sprintf("%s-%05d", jobID, previous.Ordinal), BookID: bookID, Ordinal: previous.Ordinal,
			StartMS: previous.StartMS, EndMS: min(previous.EndMS, durationMS), Status: "queued",
			AudioRelPath: audioRel, ResponseRelPath: responseRel,
		})
	}
	return chunks, nil
}

func (s *Service) retranscribe(ctx context.Context, job db.Job, book db.Book) {
	fail := func(stage, message string) {
		if ctx.Err() == nil {
			s.fail(ctx, job, stage, book, message)
		}
	}
	if book.Status != "ready" || book.AudioRelPath == "" || book.TranscriptRelPath == "" || book.DurationMS <= 0 {
		fail("retranscribing", "This book is not ready for a fresh transcription.")
		return
	}
	if strings.TrimSpace(s.cfg.GroqAPIKey) == "" {
		fail("retranscribing", "GROQ_API_KEY is not configured on the server.")
		return
	}
	playbackPath, err := safeDataPath(s.cfg.DataDir, book.AudioRelPath)
	if err != nil || fileMissingOrEmpty(playbackPath) {
		fail("retranscribing", "The saved audio is unavailable; the current transcript was kept.")
		return
	}

	var ebook *epub.Document
	prompt := ""
	if book.Mode == "aligned" {
		var document epub.Document
		if book.EbookJSONRelPath != "" {
			ebookPath, pathErr := safeDataPath(s.cfg.DataDir, book.EbookJSONRelPath)
			if pathErr == nil {
				_ = readGzipJSON(ebookPath, &document)
			}
		}
		if len(document.Chapters) == 0 && book.EpubRelPath != "" {
			epubPath, pathErr := safeDataPath(s.cfg.DataDir, book.EpubRelPath)
			if pathErr == nil {
				document, err = epub.ParseFile(epubPath)
			} else {
				err = pathErr
			}
		}
		if err != nil || len(document.Chapters) == 0 {
			fail("retranscribing", "The paired EPUB could not be loaded; the current transcript was kept.")
			return
		}
		ebook = &document
		prompt = document.HintPrompt()
	}

	bookDir := BookDirectory(s.cfg.DataDir, book.OwnerUserID, book.ID)
	runDir := filepath.Join(bookDir, "work", "retranscriptions", job.ID)
	if err := os.MkdirAll(runDir, 0700); err != nil {
		fail("retranscribing", "Could not prepare a safe workspace; the current transcript was kept.")
		return
	}
	chunks, err := s.retranscriptionChunks(ctx, job.ID, book.ID, runDir, book.DurationMS)
	if err != nil || len(chunks) == 0 {
		fail("retranscribing", "Could not prepare the audio chunks; the current transcript was kept.")
		return
	}

	responses := make(map[int]groq.Response, len(chunks))
	for _, chunk := range chunks {
		if ctx.Err() != nil {
			return
		}
		responsePath, pathErr := safeDataPath(s.cfg.DataDir, chunk.ResponseRelPath)
		if pathErr != nil || fileMissingOrEmpty(responsePath) {
			continue
		}
		var response groq.Response
		if readGzipJSON(responsePath, &response) == nil && len(response.Words) > 0 {
			responses[chunk.Ordinal] = response
		}
	}
	if err := s.db.SetJobProgress(ctx, job.ID, "retranscribing", "ready", progress(len(responses), len(chunks))); err != nil {
		return
	}
	for _, chunk := range chunks {
		if _, ok := responses[chunk.Ordinal]; ok {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		audioPath, pathErr := safeDataPath(s.cfg.DataDir, chunk.AudioRelPath)
		if pathErr != nil {
			fail("retranscribing", "Could not prepare a fresh audio chunk; the current transcript was kept.")
			return
		}
		if fileMissingOrEmpty(audioPath) {
			if err := os.MkdirAll(filepath.Dir(audioPath), 0700); err != nil {
				fail("retranscribing", "Could not prepare a fresh audio chunk; the current transcript was kept.")
				return
			}
			if err := s.tools.CreateChunk(ctx, playbackPath, audioPath, chunk.StartMS, chunk.EndMS); err != nil {
				fail("retranscribing", "Could not prepare a fresh audio chunk; the current transcript was kept.")
				return
			}
		}
		if info, err := os.Stat(audioPath); err != nil || info.Size() > groqMaxChunkBytes {
			fail("retranscribing", "A fresh audio chunk exceeds the transcription upload limit; the current transcript was kept.")
			return
		}
		response, err := s.groq.TranscribeFile(ctx, audioPath, prompt)
		if err != nil {
			var limited *groq.RateLimitError
			if errors.As(err, &limited) {
				retryAt := time.Now().Add(limited.RetryAfter).UTC().Format(time.RFC3339)
				_ = s.db.DeferJob(ctx, job.ID, "rate_limited", "ready", retryAt, progress(len(responses), len(chunks)))
				return
			}
			fail("retranscribing", "Fresh transcription failed; the current transcript was kept. Retry when the provider is available.")
			return
		}
		if len(response.Words) == 0 {
			fail("retranscribing", "The fresh transcription returned no word timestamps; the current transcript was kept.")
			return
		}
		responsePath, pathErr := safeDataPath(s.cfg.DataDir, chunk.ResponseRelPath)
		if pathErr != nil || writeGzipJSONAtomic(responsePath, response) != nil {
			fail("retranscribing", "Could not save the fresh transcription; the current transcript was kept.")
			return
		}
		_ = os.Remove(audioPath)
		responses[chunk.Ordinal] = response
		if err := s.db.SetJobProgress(ctx, job.ID, "retranscribing", "ready", progress(len(responses), len(chunks))); err != nil {
			return
		}
	}
	if len(responses) != len(chunks) {
		fail("retranscribing", "Fresh transcription did not finish; the current transcript was kept.")
		return
	}

	transcriptDoc := mergeTranscriptionResponses(chunks, responses, book.DurationMS)
	transcriptPath := filepath.Join(runDir, "transcript.v1.json.gz")
	if err := writeGzipJSONAtomic(transcriptPath, transcriptDoc); err != nil {
		fail("retranscribing", "Could not build the fresh transcript; the current transcript was kept.")
		return
	}
	transcriptRel, err := filepath.Rel(s.cfg.DataDir, transcriptPath)
	if err != nil {
		fail("retranscribing", "Could not publish the fresh transcript; the current transcript was kept.")
		return
	}

	alignmentRel := ""
	var quality *float64
	if ebook != nil {
		if err := s.db.SetJobProgress(ctx, job.ID, "aligning_retranscription", "ready", 0.97); err != nil {
			return
		}
		alignment := align.AlignEbook(*ebook, transcriptDoc)
		alignmentPath := filepath.Join(runDir, "alignment.v1.json.gz")
		if err := writeGzipJSONAtomic(alignmentPath, alignment); err != nil {
			fail("aligning_retranscription", "Could not align the fresh transcript; the current transcript was kept.")
			return
		}
		alignmentRel, err = filepath.Rel(s.cfg.DataDir, alignmentPath)
		if err != nil {
			fail("aligning_retranscription", "Could not publish the fresh alignment; the current transcript was kept.")
			return
		}
		quality = &alignment.Quality
	}
	if err := s.db.SetTranscriptionArtifacts(ctx, book.ID, transcriptRel, alignmentRel, quality); err != nil {
		fail("retranscribing", "Could not publish the fresh transcript; the current transcript was kept.")
		return
	}
	if err := s.db.CompleteJob(ctx, job.ID); err != nil {
		return
	}
	for _, chunk := range chunks {
		if audioPath, pathErr := safeDataPath(s.cfg.DataDir, chunk.AudioRelPath); pathErr == nil {
			_ = os.Remove(audioPath)
		}
	}
}

func (s *Service) transcribe(ctx context.Context, job db.Job, book db.Book, bookDir, playbackPath string,
	chunks []db.Chunk, prompt string, ebook *epub.Document) {
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
		response, err := s.groq.TranscribeFile(ctx, audioPath, prompt)
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
		if ebook != nil {
			if err := s.db.SetJobProgress(ctx, job.ID, "aligning", "ready", 0.97); err != nil {
				return
			}
			if err := s.alignEbook(ctx, bookDir, book.ID, *ebook); err != nil {
				s.fail(ctx, job, "aligning", book, "The transcript is ready, but ebook alignment failed. Retry to try alignment again.")
				return
			}
		}
		_ = s.db.CompleteJob(ctx, job.ID)
	}
}

func (s *Service) prepareEbook(ctx context.Context, book db.Book, bookDir string) (*epub.Document, error) {
	epubPath := ""
	if book.EpubRelPath != "" {
		var err error
		epubPath, err = safeDataPath(s.cfg.DataDir, book.EpubRelPath)
		if err != nil {
			return nil, err
		}
	}
	if epubPath == "" && book.GutenbergID != "" {
		epubPath = filepath.Join(bookDir, "source", "book.epub")
		if fileMissingOrEmpty(epubPath) {
			if err := media.DownloadGutenbergEPUB(ctx, book.GutenbergID, epubPath, maxStoredEPUBBytes); err != nil {
				return nil, err
			}
		}
		rel, err := filepath.Rel(s.cfg.DataDir, epubPath)
		if err != nil {
			return nil, err
		}
		if err := s.db.SetEpubPath(ctx, book.ID, rel); err != nil {
			return nil, err
		}
	}
	if epubPath == "" || fileMissingOrEmpty(epubPath) {
		return nil, fmt.Errorf("paired EPUB is missing")
	}
	document, err := epub.ParseFile(epubPath)
	if err != nil {
		return nil, err
	}
	artifact := filepath.Join(bookDir, "ebook.v1.json.gz")
	if err := writeGzipJSONAtomic(artifact, document); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(s.cfg.DataDir, artifact)
	if err != nil {
		return nil, err
	}
	if err := s.db.SetEbookJSONPath(ctx, book.ID, rel); err != nil {
		return nil, err
	}
	return &document, nil
}

func (s *Service) alignEbook(ctx context.Context, bookDir, bookID string, ebook epub.Document) error {
	book, err := s.db.BookByID(ctx, bookID)
	if err != nil {
		return err
	}
	transcriptPath, err := safeDataPath(s.cfg.DataDir, book.TranscriptRelPath)
	if err != nil {
		return err
	}
	var acoustic transcript.Document
	if err := readGzipJSON(transcriptPath, &acoustic); err != nil {
		return err
	}
	result := align.AlignEbook(ebook, acoustic)
	alignmentPath := filepath.Join(bookDir, "alignment.v1.json.gz")
	if err := writeGzipJSONAtomic(alignmentPath, result); err != nil {
		return err
	}
	rel, err := filepath.Rel(s.cfg.DataDir, alignmentPath)
	if err != nil {
		return err
	}
	return s.db.SetAlignment(ctx, bookID, rel, result.Quality)
}

func (s *Service) writeTranscript(ctx context.Context, bookID, bookDir string, chunks []db.Chunk,
	responses map[int]groq.Response, durationMS int64) error {
	if len(responses) == 0 {
		return nil
	}
	doc := mergeTranscriptionResponses(chunks, responses, durationMS)
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

func mergeTranscriptionResponses(chunks []db.Chunk, responses map[int]groq.Response, durationMS int64) transcript.Document {
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
	return transcript.MergeChunks(timed, durationMS)
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
