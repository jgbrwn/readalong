package httpapp

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jgbrwn/readalong/internal/align"
	"github.com/jgbrwn/readalong/internal/auth"
	"github.com/jgbrwn/readalong/internal/catalog"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/media"
	"github.com/jgbrwn/readalong/internal/pipeline"
	"github.com/jgbrwn/readalong/internal/transcript"
)

const (
	multipartMemoryBytes = 8 << 20
	maxEPUBBytes         = 150 << 20
	readerWindowMS       = int64(5 * 60 * 1000)
	maxReaderWindowMS    = int64(10 * 60 * 1000)
)

func (s *Server) createBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	maxUpload := s.cfg.MaxUploadBytes
	if maxUpload <= 0 {
		maxUpload = 2 << 30
	}
	if r.Header.Get("Content-Type") == "" || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		http.Error(w, "multipart form required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+maxEPUBBytes+multipartMemoryBytes)
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if r.MultipartForm == nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["audio_file"]
	epubFiles := r.MultipartForm.File["epub_file"]
	if len(files) > 1 {
		http.Error(w, "upload one audio file at a time", http.StatusBadRequest)
		return
	}
	if len(epubFiles) > 1 {
		http.Error(w, "upload one EPUB at a time", http.StatusBadRequest)
		return
	}
	sourceURL := strings.TrimSpace(r.FormValue("source_url"))
	if len(sourceURL) > 4096 {
		http.Error(w, "URL is too long", http.StatusBadRequest)
		return
	}
	if (sourceURL == "") == (len(files) == 0) {
		http.Error(w, "provide exactly one URL or audio file", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if len(title) > 255 || hasControlChars(title) {
		http.Error(w, "invalid title", http.StatusBadRequest)
		return
	}
	if title == "" {
		title = "Untitled"
	}
	sourceKind := "upload"
	normalizedURL := ""
	if sourceURL != "" {
		var err error
		sourceKind, normalizedURL, err = media.ClassifyURL(sourceURL)
		if err != nil {
			http.Error(w, "use a public HTTP(S) audio URL or a YouTube URL", http.StatusBadRequest)
			return
		}
	}
	id, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, u.ID, id)
	audioRel := ""
	if len(files) == 1 {
		header := files[0]
		if header.Size > maxUpload {
			http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
			return
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !supportedAudioExtension(ext) {
			http.Error(w, "unsupported audio file type", http.StatusBadRequest)
			return
		}
		sourceDir := filepath.Join(bookDir, "source")
		if err := os.MkdirAll(sourceDir, 0700); err != nil {
			http.Error(w, "could not prepare upload", http.StatusInternalServerError)
			return
		}
		audioPath := filepath.Join(sourceDir, "upload"+ext)
		if err := saveMultipartFile(w, header, audioPath, maxUpload); err != nil {
			_ = os.RemoveAll(bookDir)
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "could not save uploaded audio", http.StatusBadRequest)
			return
		}
		audioRel, err = filepath.Rel(s.cfg.DataDir, audioPath)
		if err != nil {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "could not save uploaded audio", http.StatusInternalServerError)
			return
		}
	}
	epubRel := ""
	var ebook epub.Document
	if len(epubFiles) == 1 {
		header := epubFiles[0]
		if header.Size > maxEPUBBytes {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "EPUB exceeds the 150 MB size limit", http.StatusRequestEntityTooLarge)
			return
		}
		if header.Size <= 0 || strings.ToLower(filepath.Ext(header.Filename)) != ".epub" {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "upload one valid EPUB file", http.StatusBadRequest)
			return
		}
		sourceDir := filepath.Join(bookDir, "source")
		if err := os.MkdirAll(sourceDir, 0700); err != nil {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "could not prepare EPUB upload", http.StatusInternalServerError)
			return
		}
		epubPath := filepath.Join(sourceDir, "book.epub")
		if err := saveMultipartFile(w, header, epubPath, maxEPUBBytes); err != nil {
			_ = os.RemoveAll(bookDir)
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "EPUB exceeds the 150 MB size limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "could not save uploaded EPUB", http.StatusBadRequest)
			return
		}
		ebook, err = epub.ParseFile(epubPath)
		if err != nil {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "the EPUB is invalid or contains no readable spine text", http.StatusBadRequest)
			return
		}
		epubRel, err = filepath.Rel(s.cfg.DataDir, epubPath)
		if err != nil {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "could not save EPUB metadata", http.StatusInternalServerError)
			return
		}
	}
	if title == "Untitled" && ebook.Title != "" {
		title = ebook.Title
	}
	author := ebook.Author
	if err := s.db.CreateBookAndJob(r.Context(), db.NewBook{
		ID: id, OwnerUserID: u.ID, Title: title, Author: author, SourceKind: sourceKind,
		SourceURL: normalizedURL, AudioRelPath: audioRel, EpubRelPath: epubRel, JobID: jobID,
	}); err != nil {
		_ = os.RemoveAll(bookDir)
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	book, err := s.db.BookForUser(r.Context(), u.ID, id)
	if err != nil {
		http.Error(w, "book was created but could not be loaded", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, book)
}

func (s *Server) searchPairs(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		http.Error(w, "paired-book catalog is unavailable", http.StatusServiceUnavailable)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	result, err := s.catalog.SearchWithStatus(r.Context(), query)
	if err != nil {
		status := http.StatusBadGateway
		if len([]rune(query)) < 2 || len([]rune(query)) > 100 || hasControlChars(query) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	if result.Warning != "" {
		w.Header().Set("X-Readalong-Search-Warning", result.Warning)
	}
	jsonOut(w, result.Pairs)
}

func (s *Server) importPair(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	if s.catalog == nil {
		http.Error(w, "paired-book catalog is unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		RightsConfirmed bool   `json:"rights_confirmed"`
		MatchConfirmed  bool   `json:"match_confirmed"`
		GutenbergID     string `json:"gutenberg_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !req.RightsConfirmed {
		http.Error(w, "confirm the source rights before importing this pair", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		http.Error(w, "confirm the source rights before importing this pair", http.StatusBadRequest)
		return
	}
	record, err := s.catalog.ByID(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "paired-book source is unavailable; search again and retry", http.StatusBadGateway)
		return
	}
	pair, ok := catalog.ToPair(record)
	if !ok {
		http.Error(w, "this record has no supported Gutenberg EPUB pair", http.StatusBadRequest)
		return
	}
	pair, ok = catalog.SelectTextCandidate(pair, req.GutenbergID)
	if !ok {
		http.Error(w, "choose a Gutenberg text candidate from the current search results", http.StatusBadRequest)
		return
	}
	if (pair.MatchKind == "title_author" || len(pair.TextCandidates) > 1) && !req.MatchConfirmed {
		http.Error(w, "confirm that you reviewed the suggested Gutenberg text for this recording", http.StatusBadRequest)
		return
	}
	id, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	book, err := s.createCatalogBook(r, u.ID, id, jobID, record, pair)
	if err != nil {
		http.Error(w, "could not add this paired book", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, book)
}

func (s *Server) createCatalogBook(r *http.Request, ownerID, id, jobID string, record catalog.Record, pair catalog.Pair) (db.Book, error) {
	author := strings.Join(pair.Authors, ", ")
	if err := s.db.CreateBookAndJob(r.Context(), db.NewBook{
		ID: id, JobID: jobID, OwnerUserID: ownerID, Title: record.Title, Author: author,
		SourceKind: "librivox", SourceURL: record.URLZipFile,
		GutenbergID: pair.GutenbergID, EbookSourceURL: pair.GutenbergURL,
	}); err != nil {
		return db.Book{}, err
	}
	return s.db.BookForUser(r.Context(), ownerID, id)
}

func saveMultipartFile(w http.ResponseWriter, header *multipart.FileHeader, dest string, maxBytes int64) error {
	in, err := header.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".upload-*.partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	n, err := io.Copy(tmp, io.LimitReader(in, maxBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n > maxBytes {
		_ = tmp.Close()
		return &http.MaxBytesError{Limit: maxBytes}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func supportedAudioExtension(ext string) bool {
	switch ext {
	case ".mp3", ".m4a", ".m4b", ".wav", ".flac", ".ogg", ".opus", ".webm", ".mp4":
		return true
	default:
		return false
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chapters, err := s.db.Chapters(r.Context(), book.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]any{"book": book, "chapters": chapters})
}

func (s *Server) deleteBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.db.BeginBookDeletion(r.Context(), u.ID, book.ID); err != nil {
		switch {
		case errors.Is(err, db.ErrBookNotFound):
			http.NotFound(w, r)
		case errors.Is(err, db.ErrBookOperationInProgress), errors.Is(err, db.ErrBookCoverInProgress),
			errors.Is(err, db.ErrBookDeleteInProgress), errors.Is(err, db.ErrBookJobInProgress):
			http.Error(w, "this book is being updated; try again shortly", http.StatusConflict)
		default:
			http.Error(w, "could not prepare book removal", http.StatusInternalServerError)
		}
		return
	}
	if err := s.db.DeleteBook(r.Context(), u.ID, book.ID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.db.CancelBookDeletion(cleanupCtx, u.ID, book.ID)
		cancel()
		if errors.Is(err, db.ErrBookOperationInProgress) || errors.Is(err, db.ErrBookCoverInProgress) ||
			errors.Is(err, db.ErrBookDeleteInProgress) {
			http.Error(w, "this book is being updated; try again shortly", http.StatusConflict)
			return
		}
		http.NotFound(w, r)
		return
	}
	_ = os.RemoveAll(pipeline.BookDirectory(s.cfg.DataDir, u.ID, book.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chapters, err := s.db.Chapters(r.Context(), book.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	requestedContent := r.URL.Query().Get("content")
	if requestedContent != "" && requestedContent != "transcript" && requestedContent != "ebook" {
		http.Error(w, "content must be transcript or ebook", http.StatusBadRequest)
		return
	}
	payload := map[string]any{
		"book": book, "chapters": chapters, "ready": false, "sentences": []transcript.Sentence{},
		"has_ebook": book.Mode == "aligned", "content_mode": "transcript",
		"alignment_pending": book.Mode == "aligned" && book.AlignmentRelPath == "",
		"alignment_ready":   book.AlignmentRelPath != "",
		"alignment_quality": book.AlignmentQuality,
	}
	if book.TranscriptRelPath == "" {
		jsonOut(w, payload)
		return
	}
	path, err := localDataPath(s.cfg.DataDir, book.TranscriptRelPath)
	if err != nil {
		http.Error(w, "reader data unavailable", http.StatusInternalServerError)
		return
	}
	var doc transcript.Document
	if err := readTranscript(path, &doc); err != nil {
		http.Error(w, "reader data unavailable", http.StatusInternalServerError)
		return
	}
	startMS, err := optionalInt64(r, "start_ms", book.PositionMS)
	if err != nil {
		http.Error(w, "invalid start_ms", http.StatusBadRequest)
		return
	}
	if startMS < 0 {
		startMS = 0
	}
	endMS, err := optionalInt64(r, "end_ms", startMS+readerWindowMS)
	if err != nil || endMS <= startMS {
		http.Error(w, "invalid reader window", http.StatusBadRequest)
		return
	}
	if endMS-startMS > maxReaderWindowMS {
		endMS = startMS + maxReaderWindowMS
	}
	if book.DurationMS > 0 && endMS > book.DurationMS {
		endMS = book.DurationMS
	}
	content := "transcript"
	chosen := doc
	if book.Mode == "aligned" && book.AlignmentRelPath != "" {
		alignmentPath, pathErr := localDataPath(s.cfg.DataDir, book.AlignmentRelPath)
		if pathErr != nil {
			http.Error(w, "alignment data unavailable", http.StatusInternalServerError)
			return
		}
		var alignmentDoc align.EbookAlignment
		if readTranscript(alignmentPath, &alignmentDoc) != nil {
			http.Error(w, "alignment data unavailable", http.StatusInternalServerError)
			return
		}
		payload["alignment_quality"] = alignmentDoc.Quality
		useEbook := alignmentDoc.Quality > 0 && (requestedContent == "ebook" ||
			requestedContent == "" && alignmentDoc.Quality >= 0.75)
		if useEbook {
			content = "ebook"
			chosen = alignmentDoc.Sentences
			if len(alignmentDoc.Chapters) > 0 {
				payload["chapters"] = alignmentDoc.Chapters
			}
		} else if requestedContent == "ebook" && alignmentDoc.Quality == 0 {
			payload["alignment_unavailable"] = true
		}
	}
	payload["ready"] = true
	payload["start_ms"] = startMS
	payload["end_ms"] = endMS
	payload["sentences"] = transcript.Window(chosen, startMS, endMS)
	payload["total_sentences"] = len(chosen.Sentences)
	payload["content_mode"] = content
	w.Header().Set("Cache-Control", "private, no-store")
	jsonOut(w, payload)
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := localDataPath(s.cfg.DataDir, book.AudioRelPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "playback"+filepath.Ext(path), info.ModTime(), file)
}

func (s *Server) updateProgress(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		PositionMS   int64           `json:"position_ms"`
		PlaybackRate float64         `json:"playback_rate"`
		SyncOffsetMS int64           `json:"sync_offset_ms"`
		Appearance   json.RawMessage `json:"appearance"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid progress payload", http.StatusBadRequest)
		return
	}
	if req.PositionMS < 0 || req.PlaybackRate < 0.5 || req.PlaybackRate > 3 ||
		math.IsNaN(req.PlaybackRate) || math.IsInf(req.PlaybackRate, 0) ||
		req.SyncOffsetMS < -10000 || req.SyncOffsetMS > 10000 {
		http.Error(w, "progress values out of range", http.StatusBadRequest)
		return
	}
	if book.DurationMS > 0 && req.PositionMS > book.DurationMS {
		req.PositionMS = book.DurationMS
	}
	appearance := "{}"
	if len(req.Appearance) > 0 {
		var value map[string]any
		if json.Unmarshal(req.Appearance, &value) != nil || value == nil || len(req.Appearance) > 8192 {
			http.Error(w, "appearance must be a small JSON object", http.StatusBadRequest)
			return
		}
		appearance = string(req.Appearance)
	}
	if err := s.db.SaveProgress(r.Context(), u.ID, book.ID, req.PositionMS, req.PlaybackRate,
		req.SyncOffsetMS, appearance); err != nil {
		if errors.Is(err, db.ErrBookNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not save progress", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retryBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	if err := s.db.RetryJob(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, db.ErrBookOperationInProgress) {
			http.Error(w, "this book is being transferred or cloned", http.StatusConflict)
			return
		}
		if errors.Is(err, db.ErrBookCoverInProgress) {
			http.Error(w, "this book cover is being updated; try again shortly", http.StatusConflict)
			return
		}
		if errors.Is(err, db.ErrBookDeleteInProgress) {
			http.Error(w, "this book is being removed; try again shortly", http.StatusConflict)
			return
		}
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) retranscribeBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	bookID := r.PathValue("id")
	if _, err := s.db.BookForUser(r.Context(), u.ID, bookID); err != nil {
		http.NotFound(w, r)
		return
	}
	if s.pipeline == nil {
		http.Error(w, "background transcription is unavailable", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(s.cfg.GroqAPIKey) == "" {
		http.Error(w, "fresh transcription is unavailable because Groq is not configured", http.StatusServiceUnavailable)
		return
	}
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "could not queue fresh transcription", http.StatusInternalServerError)
		return
	}
	if err := s.db.QueueRetranscription(r.Context(), u.ID, bookID, jobID); err != nil {
		switch {
		case errors.Is(err, db.ErrBookNotFound):
			http.NotFound(w, r)
		case errors.Is(err, db.ErrBookJobInProgress):
			http.Error(w, "this book already has a job in progress", http.StatusConflict)
		case errors.Is(err, db.ErrBookOperationInProgress):
			http.Error(w, "this book is being transferred or cloned", http.StatusConflict)
		case errors.Is(err, db.ErrBookDeleteInProgress):
			http.Error(w, "this book is being removed; try again shortly", http.StatusConflict)
		case errors.Is(err, db.ErrRetranscriptionNotReady):
			http.Error(w, "the book must be ready with audio and a transcript before re-transcribing", http.StatusConflict)
		default:
			http.Error(w, "could not queue fresh transcription", http.StatusInternalServerError)
		}
		return
	}
	book, err := s.db.BookForUser(r.Context(), u.ID, bookID)
	if err != nil {
		http.Error(w, "fresh transcription was queued but book status could not be loaded", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, book)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	bookID := r.PathValue("id")
	if _, err := s.db.BookForUser(r.Context(), u.ID, bookID); err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		book, err := s.db.BookForUser(r.Context(), u.ID, bookID)
		if err != nil {
			return
		}
		job, err := s.db.JobStatus(r.Context(), bookID)
		if err != nil {
			return
		}
		data, _ := json.Marshal(map[string]any{
			"status": book.Status, "job_status": job.Status, "stage": job.Stage, "progress": job.Progress,
			"error": job.Error, "not_before_at": job.NotBeforeAt, "job_kind": job.Kind,
		})
		_, _ = fmt.Fprintf(w, "event: progress\ndata: %s\n\n", data)
		flusher.Flush()
		if job.Status == "completed" || job.Status == "error" ||
			(job.Status == "queued" && job.NotBeforeAt != "") {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func localDataPath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid media path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootAbs, filepath.Clean(rel))
	relToRoot, err := filepath.Rel(rootAbs, path)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media path")
	}
	return path, nil
}

func readTranscript(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return json.NewDecoder(io.LimitReader(gz, 128<<20)).Decode(dst)
}

func optionalInt64(r *http.Request, key string, fallback int64) (int64, error) {
	values := r.URL.Query()[key]
	if len(values) == 0 {
		return fallback, nil
	}
	return strconv.ParseInt(values[0], 10, 64)
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
